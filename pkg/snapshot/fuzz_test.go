// fuzzes the snapshot readers with whatever file diff might be given

package snapshot

import "testing"

// check that Parse and UnmarshalInspectJSON take any file diff is given, they may fail but never panic
func FuzzParse(f *testing.F) {
	for _, s := range []string{"", "{}", "\x0a\x04test", "{\"cluster\":{\"name\":\"x\"}}", "[]", "null"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = Parse(b)
		_, _ = UnmarshalInspectJSON(b)
	})
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
