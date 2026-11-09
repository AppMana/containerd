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

package netns

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fakeWindowsHCN struct {
	nsExists      bool
	attached      []string
	liveEndpoints map[string]bool
	detachErr     error
	deleteNsErr   error
	calls         []string
}

func (f *fakeWindowsHCN) namespaceExists(id string) (bool, error) {
	f.calls = append(f.calls, "ns-exists:"+id)
	return f.nsExists, nil
}

func (f *fakeWindowsHCN) namespaceEndpointIDs(id string) ([]string, error) {
	f.calls = append(f.calls, "ns-endpoints:"+id)
	out := make([]string, len(f.attached))
	copy(out, f.attached)
	return out, nil
}

func (f *fakeWindowsHCN) endpointExists(id string) (bool, error) {
	f.calls = append(f.calls, "ep-exists:"+id)
	return f.liveEndpoints[id], nil
}

func (f *fakeWindowsHCN) detachEndpoint(nsID, epID string) error {
	f.calls = append(f.calls, "detach:"+nsID+":"+epID)
	if f.detachErr != nil {
		return f.detachErr
	}
	var kept []string
	for _, id := range f.attached {
		if id != epID {
			kept = append(kept, id)
		}
	}
	f.attached = kept
	return nil
}

func (f *fakeWindowsHCN) deleteEndpoint(id string) error {
	f.calls = append(f.calls, "delete-ep:"+id)
	delete(f.liveEndpoints, id)
	return nil
}

func (f *fakeWindowsHCN) deleteNamespace(id string) error {
	f.calls = append(f.calls, "delete-ns:"+id)
	if len(f.attached) > 0 {
		if f.deleteNsErr != nil {
			return f.deleteNsErr
		}
		return errors.New("hcnDeleteNamespace failed in Win32: The specified request is unsupported. (0x803b0015)")
	}
	f.nsExists = false
	return f.deleteNsErr
}

func TestRemoveWindowsNamespaceDetachesLiveLeakedEndpointBeforeDelete(t *testing.T) {
	// Reproduces a sandbox whose CNI ADD attached a live HCN endpoint before
	// failing (CNIResult == nil) and whose CNI DEL did not detach it: direct
	// HcnDeleteNamespace would fail with 0x803b0015, whereas detaching and
	// deleting the live endpoint first allows namespace deletion to succeed.
	hcnSim := &fakeWindowsHCN{
		nsExists:      true,
		attached:      []string{"live-ep-1"},
		liveEndpoints: map[string]bool{"live-ep-1": true},
	}
	if err := removeWindowsNamespace("ns-1", hcnSim); err != nil {
		t.Fatalf("expected live leaked endpoint to be detached and namespace removed: %v", err)
	}
	want := []string{
		"ns-exists:ns-1",
		"ns-endpoints:ns-1",
		"ep-exists:live-ep-1",
		"detach:ns-1:live-ep-1",
		"delete-ep:live-ep-1",
		"delete-ns:ns-1",
	}
	if !reflect.DeepEqual(hcnSim.calls, want) {
		t.Fatalf("unexpected call sequence: %v", hcnSim.calls)
	}
}

func TestRemoveWindowsNamespaceFailsClosedOnCascadeDeletedOrphanEndpoint(t *testing.T) {
	// Preserves commit 30d274134's invariant: when an external caller deleted an
	// HNS network/endpoint without detaching it first, the missing endpoint GUID
	// remains wedged in the namespace and HcnDeleteNamespace returns 0x803b0015.
	// removeWindowsNamespace must NOT suppress the error and must surface the
	// orphaned endpoint ID alongside the underlying 0x803b0015 error.
	errUnsupported := errors.New("hcnDeleteNamespace failed in Win32: The specified request is unsupported. (0x803b0015)")
	hcnSim := &fakeWindowsHCN{
		nsExists:      true,
		attached:      []string{"orphan-ep-02dc7983"},
		liveEndpoints: map[string]bool{},
		deleteNsErr:   errUnsupported,
	}
	err := removeWindowsNamespace("ns-wedged", hcnSim)
	if !errors.Is(err, errUnsupported) {
		t.Fatalf("expected wrapped 0x803b0015 error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "orphan-ep-02dc7983") {
		t.Fatalf("expected error to identify orphaned endpoint ID, got: %v", err)
	}
}

func TestRemoveWindowsNamespaceAbortsBeforeDeleteEndpointWhenDetachFails(t *testing.T) {
	// Never delete a still-attached HCN endpoint if detachment fails; doing so
	// would convert a recoverable live endpoint into a permanent 0x803b0015 orphan.
	hcnSim := &fakeWindowsHCN{
		nsExists:      true,
		attached:      []string{"live-ep-1"},
		liveEndpoints: map[string]bool{"live-ep-1": true},
		detachErr:     errors.New("HcnModifyNamespace failed"),
	}
	if err := removeWindowsNamespace("ns-1", hcnSim); err == nil {
		t.Fatal("expected detach failure")
	}
	want := []string{
		"ns-exists:ns-1",
		"ns-endpoints:ns-1",
		"ep-exists:live-ep-1",
		"detach:ns-1:live-ep-1",
	}
	if !reflect.DeepEqual(hcnSim.calls, want) {
		t.Fatalf("must not delete endpoint or namespace after detach failure: %v", hcnSim.calls)
	}
}

func TestRemoveWindowsNamespaceIdempotentWhenAbsent(t *testing.T) {
	hcnSim := &fakeWindowsHCN{nsExists: false}
	if err := removeWindowsNamespace("ns-gone", hcnSim); err != nil {
		t.Fatalf("expected nil for absent namespace, got: %v", err)
	}
	if !reflect.DeepEqual(hcnSim.calls, []string{"ns-exists:ns-gone"}) {
		t.Fatalf("unexpected calls for absent namespace: %v", hcnSim.calls)
	}
}
