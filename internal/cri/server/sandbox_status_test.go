/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package server

import (
	"context"
	"errors"
	"testing"
	"time"

	cni "github.com/containerd/go-cni"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	criconfig "github.com/containerd/containerd/v2/internal/cri/config"
	sandboxstore "github.com/containerd/containerd/v2/internal/cri/store/sandbox"
	servertesting "github.com/containerd/containerd/v2/internal/cri/testing"
)

func TestPodSandboxStatus(t *testing.T) {
	const (
		id = "test-id"
		ip = "10.10.10.10"
	)
	idmap := []*runtime.IDMapping{
		{
			ContainerId: 0,
			HostId:      100,
			Length:      1,
		},
	}
	additionalIPs := []string{"8.8.8.8", "2001:db8:85a3::8a2e:370:7334"}
	createdAt := time.Now()
	config := &runtime.PodSandboxConfig{
		Metadata: &runtime.PodSandboxMetadata{
			Name:      "test-name",
			Uid:       "test-uid",
			Namespace: "test-ns",
			Attempt:   1,
		},
		Linux: &runtime.LinuxPodSandboxConfig{
			SecurityContext: &runtime.LinuxSandboxSecurityContext{
				NamespaceOptions: &runtime.NamespaceOption{
					Network: runtime.NamespaceMode_NODE,
					Pid:     runtime.NamespaceMode_CONTAINER,
					Ipc:     runtime.NamespaceMode_POD,
					UsernsOptions: &runtime.UserNamespace{
						Uids: idmap,
						Gids: idmap,
						Mode: runtime.NamespaceMode_POD,
					},
				},
			},
		},
		Labels:      map[string]string{"a": "b"},
		Annotations: map[string]string{"c": "d"},
	}
	metadata := sandboxstore.Metadata{
		ID:             id,
		Name:           "test-name",
		Config:         config,
		RuntimeHandler: "test-runtime-handler",
	}

	expected := &runtime.PodSandboxStatus{
		Id:        id,
		Metadata:  config.GetMetadata(),
		CreatedAt: createdAt.UnixNano(),
		Network: &runtime.PodSandboxNetworkStatus{
			Ip: ip,
			AdditionalIps: []*runtime.PodIP{
				{
					Ip: additionalIPs[0],
				},
				{
					Ip: additionalIPs[1],
				},
			},
		},
		Linux: &runtime.LinuxPodSandboxStatus{
			Namespaces: &runtime.Namespace{
				Options: &runtime.NamespaceOption{
					Network: runtime.NamespaceMode_NODE,
					Pid:     runtime.NamespaceMode_CONTAINER,
					Ipc:     runtime.NamespaceMode_POD,
					UsernsOptions: &runtime.UserNamespace{
						Uids: idmap,
						Gids: idmap,
						Mode: runtime.NamespaceMode_POD,
					},
				},
			},
		},
		Labels:         config.GetLabels(),
		Annotations:    config.GetAnnotations(),
		RuntimeHandler: "test-runtime-handler",
	}
	for _, test := range []struct {
		desc          string
		state         string
		expectedState runtime.PodSandboxState
	}{
		{
			desc:          "sandbox state ready",
			state:         sandboxstore.StateReady.String(),
			expectedState: runtime.PodSandboxState_SANDBOX_READY,
		},
		{
			desc:          "sandbox state not ready",
			state:         sandboxstore.StateNotReady.String(),
			expectedState: runtime.PodSandboxState_SANDBOX_NOTREADY,
		},
		{
			desc:          "sandbox state unknown",
			state:         sandboxstore.StateUnknown.String(),
			expectedState: runtime.PodSandboxState_SANDBOX_NOTREADY,
		},
	} {
		t.Run(test.desc, func(t *testing.T) {
			expected.State = test.expectedState
			got := toCRISandboxStatus(metadata, test.state, createdAt, ip, additionalIPs)
			assert.Equal(t, expected, got)
		})
	}
}

func TestCheckPodSandboxNetwork(t *testing.T) {
	config := &runtime.PodSandboxConfig{
		Metadata: &runtime.PodSandboxMetadata{
			Name:      "test-name",
			Uid:       "test-uid",
			Namespace: "test-ns",
			Attempt:   1,
		},
		Linux: &runtime.LinuxPodSandboxConfig{
			SecurityContext: &runtime.LinuxSandboxSecurityContext{
				NamespaceOptions: &runtime.NamespaceOption{
					Network: runtime.NamespaceMode_POD,
				},
			},
		},
	}
	baseSandbox := sandboxstore.Sandbox{
		Metadata: sandboxstore.Metadata{
			ID:        "test-id",
			Config:    config,
			NetNSPath: "test-netns",
			CNIResult: &cni.Result{},
		},
	}

	for _, test := range []struct {
		desc        string
		checkErr    error
		disable     bool
		expectError bool
		expectCheck bool
	}{
		{
			desc:        "implemented check failure is returned",
			checkErr:    errors.New("pod IP is outside current enabled IPPools"),
			expectError: true,
			expectCheck: true,
		},
		{
			desc:        "unsupported check is ignored",
			checkErr:    errors.New(`configuration version "0.3.1" does not support the CHECK command`),
			expectCheck: true,
		},
		{
			desc:     "check can be disabled",
			checkErr: errors.New("pod IP is outside current enabled IPPools"),
			disable:  true,
		},
	} {
		t.Run(test.desc, func(t *testing.T) {
			plugin := servertesting.NewFakeCNIPlugin()
			plugin.CheckErr = test.checkErr
			c := newTestCRIService()
			c.config.CniConfig = criconfig.CniConfig{
				NetworkPluginDisableCheckPodStatus: test.disable,
			}
			c.netPlugin[defaultNetworkPlugin] = plugin

			err := c.checkPodSandboxNetwork(context.Background(), baseSandbox)
			if test.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if test.expectCheck {
				assert.Equal(t, "test-id", plugin.CheckID)
				assert.Equal(t, "test-netns", plugin.CheckPath)
			} else {
				assert.Empty(t, plugin.CheckID)
				assert.Empty(t, plugin.CheckPath)
			}
		})
	}
}
