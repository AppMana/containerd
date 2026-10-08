// Copyright The containerd Authors.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	goruntime "runtime"
	"testing"
	"time"

	sandboxstore "github.com/containerd/containerd/v2/internal/cri/store/sandbox"
	servertesting "github.com/containerd/containerd/v2/internal/cri/testing"
	cni "github.com/containerd/go-cni"
	"github.com/stretchr/testify/require"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestCheckSandboxSkipBoundaries(t *testing.T) {
	for _, tc := range []string{"healthy", "disabled", "host network", "no result", "no plugin"} {
		t.Run(tc, func(t *testing.T) {
			c := newTestCRIService()
			plugin := servertesting.NewFakeCNIPlugin()
			c.netPlugin[defaultNetworkPlugin] = plugin
			config := &runtime.PodSandboxConfig{Metadata: &runtime.PodSandboxMetadata{Name: "pod", Namespace: "test", Uid: "uid"}, Linux: &runtime.LinuxPodSandboxConfig{SecurityContext: &runtime.LinuxSandboxSecurityContext{NamespaceOptions: &runtime.NamespaceOption{Network: runtime.NamespaceMode_POD}}}}
			sb := sandboxstore.Sandbox{Metadata: sandboxstore.Metadata{ID: "sandbox", Config: config, NetNSPath: "netns", CNIResult: &cni.Result{}}}
			switch tc {
			case "disabled":
				c.config.CniConfig.NetworkPluginDisableCheckPodStatus = true
			case "host network":
				if goruntime.GOOS == "windows" {
					config.Linux = nil
					config.Windows = &runtime.WindowsPodSandboxConfig{SecurityContext: &runtime.WindowsSandboxSecurityContext{HostProcess: true}}
				} else {
					config.Linux.SecurityContext.NamespaceOptions.Network = runtime.NamespaceMode_NODE
				}
			case "no result":
				sb.CNIResult = nil
			case "no plugin":
				delete(c.netPlugin, defaultNetworkPlugin)
			}
			require.NoError(t, c.checkPodSandboxNetwork(context.Background(), sb))
			if tc == "healthy" {
				require.Equal(t, "sandbox", plugin.CheckID)
			} else {
				require.Empty(t, plugin.CheckID)
			}
		})
	}
}

func TestCheckUnsupportedBoundaries(t *testing.T) {
	for _, message := range []string{`configuration version "0.3.1" does not support the CHECK command`, "unknown CNI_COMMAND", "unknown CNI command", "unsupported CNI command"} {
		require.True(t, isCNIPluginCheckUnsupported(errors.New(message)), message)
	}
	for _, err := range []error{nil, context.DeadlineExceeded, context.Canceled, errors.New("pod IP outside current enabled IPPools"), errors.New("permission denied"), errors.New("plugin executable missing")} {
		require.False(t, isCNIPluginCheckUnsupported(err), "%v", err)
	}
}

func TestCheckListReadinessRecoveryAndFilters(t *testing.T) {
	c := newTestCRIService()
	clock := &fakeCheckClock{t: time.Now()}
	c.sandboxNetworkChecks = newNetworkCheckCache(podSandboxNetworkCheckTTL, clock.now)
	plugin := servertesting.NewFakeCNIPlugin()
	c.netPlugin[defaultNetworkPlugin] = plugin
	sb := sandboxstore.NewSandbox(sandboxstore.Metadata{ID: "sandbox", Name: "test", Config: &runtime.PodSandboxConfig{Metadata: &runtime.PodSandboxMetadata{Name: "pod", Namespace: "test", Uid: "uid"}}, NetNSPath: "netns", CNIResult: &cni.Result{}}, sandboxstore.Status{State: sandboxstore.StateReady, CreatedAt: time.Now()})
	require.NoError(t, c.sandboxStore.Add(sb))
	for _, failure := range []error{errors.New("stale IPv6"), nil} {
		plugin.CheckErr = failure
		// A new verdict is observed once the previous one expires.
		clock.advance(podSandboxNetworkCheckTTL)
		for _, state := range []runtime.PodSandboxState{runtime.PodSandboxState_SANDBOX_READY, runtime.PodSandboxState_SANDBOX_NOTREADY} {
			response, err := c.ListPodSandbox(context.Background(), &runtime.ListPodSandboxRequest{Filter: &runtime.PodSandboxFilter{State: &runtime.PodSandboxStateValue{State: state}}})
			require.NoError(t, err)
			if (failure == nil) == (state == runtime.PodSandboxState_SANDBOX_READY) {
				require.Len(t, response.Items, 1)
			} else {
				require.Empty(t, response.Items)
			}
		}
		// A status observation must not overwrite the persisted lifecycle state.
		stored, err := c.sandboxStore.Get("sandbox")
		require.NoError(t, err)
		require.Equal(t, sandboxstore.StateReady, stored.Status.Get().State)
	}
}

type waitingCheckPlugin struct{ *servertesting.FakeCNIPlugin }

func (p waitingCheckPlugin) Check(ctx context.Context, _ string, _ string, _ ...cni.NamespaceOpts) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestCheckPropagatesCallerDeadline(t *testing.T) {
	c := newTestCRIService()
	c.netPlugin[defaultNetworkPlugin] = waitingCheckPlugin{servertesting.NewFakeCNIPlugin()}
	sb := sandboxstore.Sandbox{Metadata: sandboxstore.Metadata{ID: "sandbox", Config: &runtime.PodSandboxConfig{Metadata: &runtime.PodSandboxMetadata{Name: "pod", Namespace: "test", Uid: "uid"}}, NetNSPath: "netns", CNIResult: &cni.Result{}}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- c.checkPodSandboxNetwork(ctx, sb) }()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("CHECK lost caller deadline")
	}
}
