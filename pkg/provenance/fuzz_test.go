// fuzzes the repository url canonicalization and the Argo CD tracking id parser

package provenance

import (
	"strings"
	"testing"
)

// check that a token in a repository url's userinfo never reaches CanonicalRepo's result
func FuzzCanonicalRepo(f *testing.F) {
	schemes := []string{"", "https://", "http://", "ssh://", "git+https://", "oci://"}
	f.Add(uint8(1), "user", "ghp0123456789abcdef", "github.com", "foo/bar.git")
	f.Add(uint8(3), "git", "secretsecret", "gitlab.example.com:22", "a/b")
	f.Add(uint8(0), "x", "tokentoken", "bitbucket.org", "c/d/")
	f.Add(uint8(1), "u", "tokentoken", "localhost", "")
	f.Fuzz(func(t *testing.T, n uint8, user, token, host, path string) {
		scheme := schemes[int(n)%len(schemes)]
		if len(token) < 8 || strings.ContainsAny(token, "/@:") || strings.Contains(host+"/"+path, token) ||
			strings.ContainsAny(user+host, "/@") || user == "" && scheme == "" || strings.TrimSpace(user) != user || strings.TrimSpace(token) != token {
			t.Skip()
		}
		in := scheme + user + ":" + token + "@" + host + "/" + path
		if got := CanonicalRepo(in); strings.Contains(got, token) {
			t.Fatalf("CanonicalRepo(%q) = %q: keeps the token", in, got)
		}
	})
}

// check that parsing any tracking id doesnt panic
func FuzzParseTrackingID(f *testing.F) {
	for _, s := range []string{"app:apps/Deployment:ns/web", "app:/ConfigMap:ns/x", ":", "a:b", "a:b:c/d/e"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) { parseTrackingID(s) })
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
