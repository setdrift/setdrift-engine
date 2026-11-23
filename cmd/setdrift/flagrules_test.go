// tests that a flag given without the flag it needs, or with one it cant be used with, is refused before the command reads anything

package main

import (
	"strings"
	"testing"
)

// check that each relation is refused with a message naming both flags, and that a flag given with its parent is not
func TestCheckFlags(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"export", "--yes"}, "--yes (-y) is for --force, so it cannot be used without it"},
		{[]string{"export", "--aws-region", "us-east-1"}, "--aws-region is for --aws, so it cannot be used without it"},
		{[]string{"export", "--aws-cluster", "prod"}, "--aws-cluster is for --aws, so it cannot be used without it"},
		{[]string{"export", "--from-dir", corpusInput, "-n", "shop"}, "--namespace (-n) applies to reading a cluster, so it cannot be used with --from-dir"},
		{[]string{"export", "--from-dir", corpusInput, "--no-snapshot", "--snapshot-json", "x.json"}, "--no-snapshot writes no snapshot, so it cannot be used with --snapshot-json"},
		{[]string{"verify", "--no-report", "-o", "reports", "."}, "--output (-o) names where to write the report, so it cannot be used with --no-report"},
	} {
		_, _, err := runCmd(t, c.args...)
		if err == nil || err.Error() != c.want {
			t.Errorf("%v: got %v, want %q", c.args, err, c.want)
		}
	}
	_, _, err := runCmd(t, "export", "--force", "--yes", "--kubeconfig", "/nonexistent/kubeconfig")
	if err != nil && strings.Contains(err.Error(), "cannot be used") {
		t.Errorf("--yes with --force refused: %v", err)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
