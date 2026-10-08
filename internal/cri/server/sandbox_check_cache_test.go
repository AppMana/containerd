// Copyright The containerd Authors.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sandboxstore "github.com/containerd/containerd/v2/internal/cri/store/sandbox"
	servertesting "github.com/containerd/containerd/v2/internal/cri/testing"
	cni "github.com/containerd/go-cni"
	"github.com/stretchr/testify/require"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// countingCheckPlugin counts CHECK invocations per sandbox and can hold every
// CHECK until released, so concurrent callers overlap deterministically.
type countingCheckPlugin struct {
	*servertesting.FakeCNIPlugin
	mu      sync.Mutex
	calls   map[string]int
	err     error
	started chan struct{}
	release chan struct{}
}

func newCountingCheckPlugin() *countingCheckPlugin {
	return &countingCheckPlugin{FakeCNIPlugin: servertesting.NewFakeCNIPlugin(), calls: map[string]int{}}
}

func (p *countingCheckPlugin) Check(ctx context.Context, id, _ string, _ ...cni.NamespaceOpts) error {
	p.mu.Lock()
	p.calls[id]++
	err, started, release := p.err, p.started, p.release
	p.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (p *countingCheckPlugin) setErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

func (p *countingCheckPlugin) count(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[id]
}

func addReadyCheckSandbox(t *testing.T, c *criService, id string) {
	t.Helper()
	sb := sandboxstore.NewSandbox(
		sandboxstore.Metadata{
			ID:        id,
			Name:      "name-" + id,
			Config:    &runtime.PodSandboxConfig{Metadata: &runtime.PodSandboxMetadata{Name: "pod-" + id, Namespace: "test", Uid: "uid-" + id}},
			NetNSPath: "netns-" + id,
			CNIResult: &cni.Result{},
		},
		sandboxstore.Status{State: sandboxstore.StateReady, CreatedAt: time.Now()},
	)
	require.NoError(t, c.sandboxStore.Add(sb))
}

func listStates(t *testing.T, c *criService) map[string]runtime.PodSandboxState {
	t.Helper()
	response, err := c.ListPodSandbox(context.Background(), &runtime.ListPodSandboxRequest{})
	require.NoError(t, err)
	states := map[string]runtime.PodSandboxState{}
	for _, item := range response.Items {
		states[item.Id] = item.State
	}
	return states
}

// kubelet's PLEG relists every second; each relist must not spawn a CNI CHECK
// per sandbox.
func TestCheckRelistStormRunsOneCheckPerSandboxPerTTL(t *testing.T) {
	c := newTestCRIService()
	plugin := newCountingCheckPlugin()
	c.netPlugin[defaultNetworkPlugin] = plugin
	for _, id := range []string{"a", "b", "c"} {
		addReadyCheckSandbox(t, c, id)
	}
	for range 120 {
		states := listStates(t, c)
		for _, id := range []string{"a", "b", "c"} {
			require.Equal(t, runtime.PodSandboxState_SANDBOX_READY, states[id])
		}
	}
	for _, id := range []string{"a", "b", "c"} {
		require.Equal(t, 1, plugin.count(id), id)
	}
}

// A failed CHECK still marks the sandbox NOTREADY on every relist without
// re-running CHECK, and the observation does not rewrite lifecycle state.
func TestCheckFailureStillSurfacesOnEveryRelist(t *testing.T) {
	c := newTestCRIService()
	plugin := newCountingCheckPlugin()
	plugin.setErr(errors.New("pod IP is outside current enabled IPPools"))
	c.netPlugin[defaultNetworkPlugin] = plugin
	addReadyCheckSandbox(t, c, "broken")

	for range 10 {
		require.Equal(t, runtime.PodSandboxState_SANDBOX_NOTREADY, listStates(t, c)["broken"])
	}
	require.Equal(t, 1, plugin.count("broken"))

	stored, err := c.sandboxStore.Get("broken")
	require.NoError(t, err)
	require.Equal(t, sandboxstore.StateReady, stored.Status.Get().State)
}

// Concurrent List/Status callers for the same sandbox share one CHECK.
func TestCheckConcurrentCallersShareOneCheck(t *testing.T) {
	c := newTestCRIService()
	plugin := newCountingCheckPlugin()
	plugin.err = errors.New("stale IPv6 address")
	plugin.started = make(chan struct{}, 64)
	plugin.release = make(chan struct{})
	c.netPlugin[defaultNetworkPlugin] = plugin
	addReadyCheckSandbox(t, c, "shared")
	sb, err := c.sandboxStore.Get("shared")
	require.NoError(t, err)

	const callers = 16
	var wg sync.WaitGroup
	var failures atomic.Int32
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c.checkPodSandboxNetwork(context.Background(), sb) != nil {
				failures.Add(1)
			}
		}()
	}
	<-plugin.started
	// Give the remaining callers time to arrive while the first CHECK is held.
	time.Sleep(100 * time.Millisecond)
	close(plugin.release)
	wg.Wait()

	require.Equal(t, 1, plugin.count("shared"))
	require.Equal(t, int32(callers), failures.Load())
}

type fakeCheckClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeCheckClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeCheckClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// Once the verdict expires the next relist runs CHECK again, so a network
// that breaks after a healthy CHECK is reported within one TTL.
func TestCheckVerdictExpiresAfterTTL(t *testing.T) {
	c := newTestCRIService()
	clock := &fakeCheckClock{t: time.Now()}
	c.sandboxNetworkChecks = newNetworkCheckCache(podSandboxNetworkCheckTTL, clock.now)
	plugin := newCountingCheckPlugin()
	c.netPlugin[defaultNetworkPlugin] = plugin
	addReadyCheckSandbox(t, c, "sb")

	require.Equal(t, runtime.PodSandboxState_SANDBOX_READY, listStates(t, c)["sb"])
	plugin.setErr(errors.New("pod IP is outside current enabled IPPools"))
	clock.advance(podSandboxNetworkCheckTTL - time.Second)
	require.Equal(t, runtime.PodSandboxState_SANDBOX_READY, listStates(t, c)["sb"])
	require.Equal(t, 1, plugin.count("sb"))

	clock.advance(time.Second)
	require.Equal(t, runtime.PodSandboxState_SANDBOX_NOTREADY, listStates(t, c)["sb"])
	require.Equal(t, runtime.PodSandboxState_SANDBOX_NOTREADY, listStates(t, c)["sb"])
	require.Equal(t, 2, plugin.count("sb"))
}

// A CHECK abandoned because its caller's deadline passed is not a verdict:
// the caller sees its own error and the next caller runs CHECK again.
func TestCheckAbandonedByCallerIsNotCached(t *testing.T) {
	c := newTestCRIService()
	plugin := newCountingCheckPlugin()
	plugin.release = make(chan struct{})
	c.netPlugin[defaultNetworkPlugin] = plugin
	addReadyCheckSandbox(t, c, "sb")
	sb, err := c.sandboxStore.Get("sb")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, c.checkPodSandboxNetwork(ctx, sb), context.DeadlineExceeded)

	close(plugin.release)
	require.NoError(t, c.checkPodSandboxNetwork(context.Background(), sb))
	require.Equal(t, 2, plugin.count("sb"))
}

// A waiter whose own context ends stops waiting without disturbing the CHECK
// it was sharing.
func TestCheckWaiterHonoursItsOwnDeadline(t *testing.T) {
	c := newTestCRIService()
	plugin := newCountingCheckPlugin()
	plugin.started = make(chan struct{}, 1)
	plugin.release = make(chan struct{})
	c.netPlugin[defaultNetworkPlugin] = plugin
	addReadyCheckSandbox(t, c, "sb")
	sb, err := c.sandboxStore.Get("sb")
	require.NoError(t, err)

	owner := make(chan error, 1)
	go func() { owner <- c.checkPodSandboxNetwork(context.Background(), sb) }()
	<-plugin.started

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, c.checkPodSandboxNetwork(ctx, sb), context.DeadlineExceeded)

	close(plugin.release)
	require.NoError(t, <-owner)
	require.NoError(t, c.checkPodSandboxNetwork(context.Background(), sb))
	require.Equal(t, 1, plugin.count("sb"))
}

// Removing a sandbox drops its verdict.
func TestCheckVerdictForgottenOnRemove(t *testing.T) {
	cache := newNetworkCheckCache(podSandboxNetworkCheckTTL, time.Now)
	calls := 0
	run := func(context.Context) error { calls++; return nil }
	require.NoError(t, cache.check(context.Background(), "sb", run))
	require.NoError(t, cache.check(context.Background(), "sb", run))
	cache.forget("sb")
	require.NoError(t, cache.check(context.Background(), "sb", run))
	require.Equal(t, 2, calls)
}
