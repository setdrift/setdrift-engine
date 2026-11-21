// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package snapshot

import "testing"

// Parse and UnmarshalInspectJSON take any file diff is given: they may fail, never panic.
func FuzzParse(f *testing.F) {
	for _, s := range []string{"", "{}", "\x0a\x04test", "{\"cluster\":{\"name\":\"x\"}}", "[]", "null"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = Parse(b)
		_, _ = UnmarshalInspectJSON(b)
	})
}
