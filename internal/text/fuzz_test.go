// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package text

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// A POSIX shell reads ShellQuote's word back as exactly the input, and as one word.
func FuzzShellQuote(f *testing.F) {
	if runtime.GOOS == "windows" {
		f.Skip("needs a POSIX shell")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		f.Skip("no sh")
	}
	for _, s := range []string{"", "plain", "a b", "it's", "$(id)", "`id`", "a\nb", "'\"\\", "-n", "*"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if strings.ContainsRune(s, 0) {
			t.Skip("a shell word can't hold NUL")
		}
		out, err := exec.Command(sh, "-c", "printf %s "+ShellQuote(s)+"; printf '|%d' $#", "sh").Output()
		if err != nil {
			t.Fatal(err)
		}
		if want := s + "|0"; string(out) != want {
			t.Fatalf("ShellQuote(%q) = %s, read back as %q", s, ShellQuote(s), out)
		}
	})
}
