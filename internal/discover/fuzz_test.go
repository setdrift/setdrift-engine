// fuzzes reading aggregated discovery documents

package discover

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// check that reading whatever an API server, or something answering in its place, sends as aggregated discovery doesnt panic
func FuzzAggregated(f *testing.F) {
	f.Add([]byte(`{"items":[{"metadata":{"name":"apps"},"versions":[{"version":"v1","resources":[{"resource":"deployments","responseKind":{"kind":"Deployment"},"scope":"Namespaced","subresources":[{"subresource":"status"}]}]}]}]}`))
	f.Add([]byte(`{"items":[{"versions":[{"version":"v1","freshness":"Stale"}]},{}]}`))
	f.Add([]byte(`{"items":null}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		var l aggregatedList
		if json.Unmarshal(b, &l) != nil {
			return
		}
		l.groups(map[string]*metav1.APIResourceList{}, map[string]error{})
	})
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
