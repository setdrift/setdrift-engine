// fuzzes the field comparison with arbitrary JSON documents

package diff

import (
	"encoding/json"
	"testing"
)

// check that Fields compares any two JSON documents without panicking
func FuzzFields(f *testing.F) {
	f.Add([]byte(`{"a":1,"b":[1,2]}`), []byte(`{"a":2,"b":[{"name":"x"}]}`))
	f.Add([]byte(`[{"name":"a","v":1}]`), []byte(`[{"name":"a","v":"1"},null]`))
	f.Fuzz(func(t *testing.T, a, b []byte) {
		var x, y any
		if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
			t.Skip()
		}
		Fields(x, y)
	})
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
