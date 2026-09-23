// Copyright The containerd Authors.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/sandbox"
	sandboxstore "github.com/containerd/containerd/v2/internal/cri/store/sandbox"
	servertesting "github.com/containerd/containerd/v2/internal/cri/testing"
	"github.com/containerd/containerd/v2/pkg/netns"
	cni "github.com/containerd/go-cni"
	"github.com/stretchr/testify/require"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type checkStatusService struct {
	*fakeSandboxService
	state string
}

func (s checkStatusService) SandboxStatus(context.Context, string, string, bool) (sandbox.ControllerStatus, error) {
	return sandbox.ControllerStatus{State: s.state, CreatedAt: time.Now()}, nil
}

func TestCheckPodSandboxStatusBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		checkErr error
		state    string
		want     runtime.PodSandboxState
		called   bool
	}{
		{"healthy", nil, "SANDBOX_READY", runtime.PodSandboxState_SANDBOX_READY, true},
		{"stale IPv6", errors.New("outside current enabled IPPools"), "SANDBOX_READY", runtime.PodSandboxState_SANDBOX_NOTREADY, true},
		{"unsupported", errors.New("unknown CNI_COMMAND"), "SANDBOX_READY", runtime.PodSandboxState_SANDBOX_READY, true},
		{"already not ready", nil, "SANDBOX_NOTREADY", runtime.PodSandboxState_SANDBOX_NOTREADY, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCRIService()
			plugin := servertesting.NewFakeCNIPlugin()
			plugin.CheckErr = tc.checkErr
			c.netPlugin[defaultNetworkPlugin] = plugin
			c.sandboxService = checkStatusService{&fakeSandboxService{}, tc.state}
			sb := sandboxstore.NewSandbox(sandboxstore.Metadata{ID: "sandbox", Name: "test", Config: &runtime.PodSandboxConfig{Metadata: &runtime.PodSandboxMetadata{Name: "pod", Namespace: "test", Uid: "uid"}}, IP: "10.244.0.2", AdditionalIPs: []string{"2001:db8:1::2"}, NetNSPath: "/proc/self/ns/net", CNIResult: &cni.Result{}}, sandboxstore.Status{State: sandboxstore.StateReady, CreatedAt: time.Now()})
			// Read-only reference to this process's namespace; no privileged setup.
			sb.NetNS = netns.LoadNetNS("/proc/self/ns/net")
			require.NoError(t, c.sandboxStore.Add(sb))
			response, err := c.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: "sandbox"})
			require.NoError(t, err)
			require.Equal(t, tc.want, response.Status.State)
			require.Equal(t, "10.244.0.2", response.Status.Network.Ip)
			require.Equal(t, "2001:db8:1::2", response.Status.Network.AdditionalIps[0].Ip)
			require.Equal(t, tc.called, plugin.CheckID != "")
		})
	}
}
