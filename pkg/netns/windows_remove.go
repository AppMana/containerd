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

import "fmt"

type windowsNamespaceRemovalAPI interface {
	namespaceExists(namespaceID string) (bool, error)
	namespaceEndpointIDs(namespaceID string) ([]string, error)
	endpointExists(endpointID string) (bool, error)
	detachEndpoint(namespaceID, endpointID string) error
	deleteEndpoint(endpointID string) error
	deleteNamespace(namespaceID string) error
}

// removeWindowsNamespace removes a sandbox HCN namespace cleanly when a live
// HCN endpoint was left attached (for example, after a partial CNI ADD with
// CNIResult == nil followed by a failed CNI DEL), while remaining strictly
// fail-closed if an already-deleted orphan endpoint reference keeps Windows
// HNS from deleting the namespace (0x803b0015).
func removeWindowsNamespace(namespaceID string, api windowsNamespaceRemovalAPI) error {
	if namespaceID == "" {
		return nil
	}
	exists, err := api.namespaceExists(namespaceID)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	endpointIDs, err := api.namespaceEndpointIDs(namespaceID)
	if err != nil {
		return err
	}

	var missingOrphans []string
	for _, epID := range endpointIDs {
		if epID == "" {
			continue
		}
		live, err := api.endpointExists(epID)
		if err != nil {
			return err
		}
		if !live {
			missingOrphans = append(missingOrphans, epID)
			continue
		}
		// Detach while the endpoint still exists; deleting an attached HCN
		// endpoint without detaching first wedges its GUID in the namespace.
		if err := api.detachEndpoint(namespaceID, epID); err != nil {
			return fmt.Errorf("detach live HCN endpoint %s from namespace %s: %w", epID, namespaceID, err)
		}
		if err := api.deleteEndpoint(epID); err != nil {
			return fmt.Errorf("delete detached HCN endpoint %s from namespace %s: %w", epID, namespaceID, err)
		}
	}

	if err := api.deleteNamespace(namespaceID); err != nil {
		if len(missingOrphans) > 0 {
			return fmt.Errorf("orphaned missing HCN endpoint(s) %v remain referenced by namespace %s: %w", missingOrphans, namespaceID, err)
		}
		return err
	}
	return nil
}
