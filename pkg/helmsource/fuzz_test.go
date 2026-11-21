// fuzzes the Helm post-renderer with whatever input Helm might hand it

package helmsource

import (
	"bytes"
	"testing"
)

// check that Annotate takes whatever Helm, or a post-renderer before it, writes, it may fail but never panic
func FuzzAnnotate(f *testing.F) {
	for _, s := range []string{"", "---\n", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n", "metadata: 1\n", "- a\n---\nmetadata:\n  annotations: []\n", "&a [*a]"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		var out bytes.Buffer
		_ = Annotate(bytes.NewReader(in), &out, "https://charts.example.com")
	})
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
