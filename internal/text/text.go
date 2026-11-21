// Package text holds the wording helpers SetDrift's messages, READMEs and
// playbooks share: counts with the right noun, and shell words quoted for
// the commands they show.
package text

import (
	"fmt"
	"strings"
)

// Plural formats n with the singular or plural noun: Plural(1, "Pod",
// "Pods") is "1 Pod", Plural(3, "Pod", "Pods") is "3 Pods".
func Plural(n int, singular, plural string) string {
	return fmt.Sprintf("%d %s", n, Word(n, singular, plural))
}

// Word returns singular for n == 1, else plural: Word(n, "was", "were").
func Word(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// Choose returns a if cond, else b.
func Choose[T any](cond bool, a, b T) T {
	if cond {
		return a
	}
	return b
}

// ShellQuote quotes s as one word for a POSIX shell when it needs it:
// letters, digits and -_./:=@,+ stay as they are.
func ShellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@,+", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellQuoteAll quotes each of ss with ShellQuote.
func ShellQuoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = ShellQuote(s)
	}
	return out
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
