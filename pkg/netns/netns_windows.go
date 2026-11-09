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
	"fmt"
	"strings"

	"github.com/Microsoft/hcsshim/hcn"
)

var errNotImplementedOnWindows = errors.New("not implemented on windows")

// NetNS holds network namespace for sandbox
type NetNS struct {
	path string
}

// NewNetNS creates a network namespace for the sandbox.
func NewNetNS(baseDir string) (*NetNS, error) {
	temp := hcn.HostComputeNamespace{}
	hcnNamespace, err := temp.Create()
	if err != nil {
		return nil, err
	}

	return &NetNS{path: hcnNamespace.Id}, nil
}

// NewNetNSFromPID returns the netns from pid or a new netns if pid is 0.
func NewNetNSFromPID(baseDir string, pid uint32) (*NetNS, error) {
	return nil, errNotImplementedOnWindows
}

// LoadNetNS loads existing network namespace.
func LoadNetNS(path string) *NetNS {
	return &NetNS{path: path}
}

// Remove removes network namespace if it exists and not closed. Remove is idempotent,
// meaning it might be invoked multiple times and provides consistent result.
func (n *NetNS) Remove() error {
	return removeWindowsNamespace(n.path, realWindowsNamespaceRemoval{})
}

type realWindowsNamespaceRemoval struct{}

func (realWindowsNamespaceRemoval) namespaceExists(namespaceID string) (bool, error) {
	ns, err := hcn.GetNamespaceByID(namespaceID)
	if err != nil {
		if hcn.IsNotFoundError(err) {
			return false, nil
		}
		return false, err
	}
	if ns.Type == hcn.NamespaceTypeHostDefault || ns.Type == hcn.NamespaceTypeGuestDefault {
		return false, fmt.Errorf("refusing to remove default HCN namespace %s (type %s)", namespaceID, ns.Type)
	}
	return true, nil
}

func (realWindowsNamespaceRemoval) namespaceEndpointIDs(namespaceID string) ([]string, error) {
	ids, err := hcn.GetNamespaceEndpointIds(namespaceID)
	if hcn.IsNotFoundError(err) {
		return nil, nil
	}
	return ids, err
}

func (realWindowsNamespaceRemoval) endpointExists(endpointID string) (bool, error) {
	_, err := hcn.GetEndpointByID(endpointID)
	if err == nil {
		return true, nil
	}
	if hcn.IsNotFoundError(err) {
		return false, nil
	}
	return false, err
}

func (realWindowsNamespaceRemoval) detachEndpoint(namespaceID, endpointID string) error {
	if err := hcn.RemoveNamespaceEndpoint(namespaceID, endpointID); err != nil && !hcn.IsNotFoundError(err) {
		return err
	}
	ids, err := hcn.GetNamespaceEndpointIds(namespaceID)
	if hcn.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, id := range ids {
		if strings.EqualFold(id, endpointID) {
			return fmt.Errorf("endpoint %s remains attached to HCN namespace %s after detach", endpointID, namespaceID)
		}
	}
	return nil
}

func (realWindowsNamespaceRemoval) deleteEndpoint(endpointID string) error {
	ep, err := hcn.GetEndpointByID(endpointID)
	if err != nil {
		if hcn.IsNotFoundError(err) {
			return nil
		}
		return err
	}
	if err := ep.Delete(); err != nil && !hcn.IsNotFoundError(err) {
		return err
	}
	return nil
}

func (realWindowsNamespaceRemoval) deleteNamespace(namespaceID string) error {
	ns, err := hcn.GetNamespaceByID(namespaceID)
	if err != nil {
		if hcn.IsNotFoundError(err) {
			return nil
		}
		return err
	}
	err = ns.Delete()
	if err == nil || hcn.IsNotFoundError(err) {
		return nil
	}
	return err
}

// Closed checks whether the network namespace has been closed.
func (n *NetNS) Closed() (bool, error) {
	_, err := hcn.GetNamespaceByID(n.path)
	if err == nil {
		return false, nil
	}
	if hcn.IsNotFoundError(err) {
		return true, nil
	}
	return false, err
}

// GetPath returns network namespace path for sandbox container
func (n *NetNS) GetPath() string {
	return n.path
}

// NOTE: Do function is not supported.
