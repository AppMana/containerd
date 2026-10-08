// Copyright The containerd Authors.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"sync"
	"time"
)

// podSandboxNetworkCheckTTL bounds how often CNI CHECK runs per sandbox.
// kubelet relists sandboxes every second; the verdict is reused for this long
// by both ListPodSandbox and PodSandboxStatus.
const podSandboxNetworkCheckTTL = 30 * time.Second

// networkCheckCache keeps the latest CNI CHECK verdict per sandbox and lets
// concurrent callers for the same sandbox share one in-flight CHECK.
type networkCheckCache struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[string]*networkCheckEntry
}

type networkCheckEntry struct {
	// done is closed once the CHECK finishes.
	done chan struct{}
	// valid is false when the CHECK was abandoned because its caller's
	// context ended; such an outcome says nothing about the network.
	valid     bool
	err       error
	checkedAt time.Time
}

func newNetworkCheckCache(ttl time.Duration, now func() time.Time) *networkCheckCache {
	return &networkCheckCache{ttl: ttl, now: now, entries: map[string]*networkCheckEntry{}}
}

// check returns the cached verdict for id if it is younger than the TTL,
// waits for an in-flight CHECK for id, or runs run itself.
func (n *networkCheckCache) check(ctx context.Context, id string, run func(context.Context) error) error {
	for {
		n.mu.Lock()
		entry, ok := n.entries[id]
		if ok {
			select {
			case <-entry.done:
				if entry.valid && n.now().Sub(entry.checkedAt) < n.ttl {
					n.mu.Unlock()
					return entry.err
				}
			default:
				n.mu.Unlock()
				select {
				case <-entry.done:
					if entry.valid {
						return entry.err
					}
					// The CHECK owner's context ended; retry with ours.
					continue
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		entry = &networkCheckEntry{done: make(chan struct{})}
		n.entries[id] = entry
		n.mu.Unlock()

		err := run(ctx)

		n.mu.Lock()
		entry.err = err
		entry.valid = ctx.Err() == nil || !(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
		entry.checkedAt = n.now()
		if !entry.valid && n.entries[id] == entry {
			delete(n.entries, id)
		}
		close(entry.done)
		n.mu.Unlock()
		return err
	}
}

// forget drops the verdict of a removed sandbox.
func (n *networkCheckCache) forget(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.entries, id)
}
