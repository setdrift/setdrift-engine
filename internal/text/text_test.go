// tests the wording helpers, plural nouns and shell quoting

package text

import "testing"

// check that Plural picks the singular noun only for one
func TestPlural(t *testing.T) {
	for _, c := range []struct {
		n    int
		want string
	}{{0, "0 Pods"}, {1, "1 Pod"}, {2, "2 Pods"}} {
		if got := Plural(c.n, "Pod", "Pods"); got != c.want {
			t.Errorf("Plural(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// check that ShellQuote leaves safe words alone and quotes empty words, spaces, quotes and shell syntax
func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"prod":                        "prod",
		"arn:aws:eks:us-east-2:1:x/y": "arn:aws:eks:us-east-2:1:x/y",
		"":                            "''",
		"two words":                   "'two words'",
		"it's":                        `'it'\''s'`,
		"$(rm -rf /)":                 "'$(rm -rf /)'",
	} {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
