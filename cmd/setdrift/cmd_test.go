// Helpers the command tests share.

package main

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"
)

// corpusInput is the basic corpus case's input: manifests to export with
// --from-dir.
var corpusInput = filepath.Join("..", "..", "testdata", "corpus", "basic", "input")

// runCmd runs setdrift with args, and returns what it printed to stdout and
// stderr.
func runCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errOut)
	err = root.Execute()
	return out.String(), errOut.String(), err
}

// setClock makes export stamp snapshots with at, until the test ends.
func setClock(t *testing.T, at time.Time) {
	t.Helper()
	old := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = old })
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
