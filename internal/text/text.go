// Package text holds the wording helpers that SetDrift's messages, READMEs and playbooks share
// that means counts with the right noun, and shell words quoted for the commands they show
package text

import (
	"fmt"
	"strings"
)

// Plural takes a count and both forms of a noun and gives back the count with the right one, Plural(1, "Pod", "Pods") is "1 Pod", Plural(3, "Pod", "Pods") is "3 Pods"
func Plural(n int, singular, plural string) string {
	// invoking Word to pick the noun for n, its defined below
	return fmt.Sprintf("%d %s", n, Word(n, singular, plural))
}

// Word gives back singular when n is 1 and plural otherwise, for wording like Word(n, "was", "were")
func Word(n int, singular, plural string) string {
	// checking if theres exactly one, if so the singular fits
	if n == 1 {
		return singular
	}
	return plural
}

// Choose gives back a when cond holds and b otherwise, a one-line stand-in for an if in the middle of an expression
func Choose[T any](cond bool, a, b T) T {
	// checking the condition to pick between the two
	if cond {
		return a
	}
	return b
}

// ShellQuote quotes s as one word for a POSIX shell, only when it needs it, so the commands we print can be pasted as is
// letters, digits and -_./:=@,+ stay as they are
func ShellQuote(s string) string {
	// checking if s is non-empty and has only safe characters, if so it goes out unquoted
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@,+", r))
	}) < 0 {
		return s
	}
	// wrapping it in single quotes, each single quote inside is closed, escaped and reopened
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellQuoteAll takes a list of words and quotes each one with ShellQuote, for printing a full command line
func ShellQuoteAll(ss []string) []string {
	out := make([]string, len(ss))
	// going over each word, invoking ShellQuote on it, its defined above
	for i, s := range ss {
		out[i] = ShellQuote(s)
	}
	return out
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
