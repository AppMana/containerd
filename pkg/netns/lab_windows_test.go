//go:build windows

package netns

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/hcsshim/hcn"
)

// Explicitly opt-in, read-only investigation of one namespace in a disposable
// VM. Keep native HCN evidence separate from legacy Get-HnsNamespace output.
func TestLabInspectWindowsNamespace(t *testing.T) {
	id := os.Getenv("LABCONTAINERS_INSPECT_NAMESPACE")
	if id == "" {
		t.Skip("requires an explicit isolated-lab namespace ID")
	}
	ns, err := hcn.GetNamespaceByID(id)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(ns)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("HCN_NAMESPACE=%s", body)
	endpoints, err := hcn.GetNamespaceEndpointIds(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, endpointID := range endpoints {
		ep, err := hcn.GetEndpointByID(endpointID)
		body, _ := json.Marshal(ep)
		t.Logf("HCN_ENDPOINT id=%s notFound=%v error=%v value=%s", endpointID, hcn.IsNotFoundError(err), err, body)
	}
	containers, err := hcn.GetNamespaceContainerIds(id)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("HCN_CONTAINERS=%v", containers)
}

// Only an explicitly named resource already proven absent may be detached.
// This diagnoses the failed lab; it never deletes a namespace or live endpoint
// and is not a substitute for automatic recovery qualification.
func TestLabDetachMissingWindowsEndpoint(t *testing.T) {
	namespaceID := os.Getenv("LABCONTAINERS_INSPECT_NAMESPACE")
	endpointID := os.Getenv("LABCONTAINERS_DETACH_MISSING_ENDPOINT")
	if namespaceID == "" || endpointID == "" {
		t.Skip("requires exact isolated-lab namespace and missing endpoint IDs")
	}
	ns, err := hcn.GetNamespaceByID(namespaceID)
	if err != nil {
		t.Fatal(err)
	}
	if ns.Type == hcn.NamespaceTypeHostDefault || ns.Type == hcn.NamespaceTypeGuestDefault {
		t.Fatal("refusing default namespace")
	}
	ids, err := hcn.GetNamespaceEndpointIds(namespaceID)
	if err != nil || len(ids) != 1 || !strings.EqualFold(ids[0], endpointID) {
		t.Fatalf("namespace does not have exactly the expected orphan: %v %v", ids, err)
	}
	_, err = hcn.GetEndpointByID(endpointID)
	if !hcn.IsNotFoundError(err) {
		t.Fatalf("refusing detach: endpoint absence not proven: %v", err)
	}
	if err := hcn.RemoveNamespaceEndpoint(namespaceID, endpointID); err != nil {
		t.Fatal(err)
	}
	ids, err = hcn.GetNamespaceEndpointIds(namespaceID)
	if err != nil && !hcn.IsNotFoundError(err) {
		t.Fatal(err)
	}
	for _, id := range ids {
		if strings.EqualFold(id, endpointID) {
			t.Fatal("orphan reference remains after detach")
		}
	}
	t.Log("detached only the explicitly named absent endpoint; namespace removal left to the runtime")
}

// Reproduces a live leaked HCN endpoint remaining attached to a sandbox NetNS
// (e.g. partial CNI ADD with CNIResult == nil followed by a failed CNI DEL) on
// an isolated Private HCN network, and verifies LoadNetNS(ns.Id).Remove()
// detaches & deletes the live endpoint so HcnDeleteNamespace succeeds instead
// of failing with 0x803b0015.
func TestLabNetNSRemoveDetachesLiveEndpoint(t *testing.T) {
	if os.Getenv("LABCONTAINERS_PRIVATE_HCN_REPRO") != "1" {
		t.Skip("requires an explicitly isolated Windows lab")
	}
	name := fmt.Sprintf("lc-netns-remove-%d", time.Now().UnixNano())
	network, err := (&hcn.HostComputeNetwork{
		Name: name, Type: hcn.Private, SchemaVersion: hcn.SchemaVersion{Major: 2},
		Ipams: []hcn.Ipam{{Type: "Static", Subnets: []hcn.Subnet{{
			IpAddressPrefix: "198.18.233.0/24",
			Routes:          []hcn.Route{{DestinationPrefix: "0.0.0.0/0", NextHop: "198.18.233.1"}},
		}}}},
	}).Create()
	if err != nil {
		t.Fatal("create isolated private network:", err)
	}
	t.Cleanup(func() {
		if _, err := hcn.GetNetworkByID(network.Id); !hcn.IsNotFoundError(err) {
			_ = network.Delete()
		}
	})
	ns, err := hcn.NewNamespace(hcn.NamespaceTypeHost).Create()
	if err != nil {
		t.Fatal("create isolated HCN namespace:", err)
	}
	t.Cleanup(func() {
		if _, err := hcn.GetNamespaceByID(ns.Id); !hcn.IsNotFoundError(err) {
			_ = ns.Delete()
		}
	})
	ep, err := (&hcn.HostComputeEndpoint{
		Name:               name + "_ep",
		HostComputeNetwork: network.Id,
		SchemaVersion:      hcn.SchemaVersion{Major: 2},
		IpConfigurations:   []hcn.IpConfig{{IpAddress: "198.18.233.4", PrefixLength: 24}},
	}).Create()
	if err != nil {
		t.Fatal("create isolated HCN endpoint:", err)
	}
	if err := ep.NamespaceAttach(ns.Id); err != nil {
		t.Fatal("attach endpoint to HCN namespace:", err)
	}
	if err := LoadNetNS(ns.Id).Remove(); err != nil {
		t.Fatalf("NetNS.Remove() failed with live attached endpoint: %v", err)
	}
	if _, err := hcn.GetEndpointByID(ep.Id); !hcn.IsNotFoundError(err) {
		t.Fatalf("expected live leaked endpoint %s to be deleted, got err=%v", ep.Id, err)
	}
	if _, err := hcn.GetNamespaceByID(ns.Id); !hcn.IsNotFoundError(err) {
		t.Fatalf("expected HCN namespace %s to be deleted, got err=%v", ns.Id, err)
	}
}
