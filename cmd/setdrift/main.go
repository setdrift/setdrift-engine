// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Command setdrift exports a Kubernetes cluster's configuration as a
// Git-ready Kustomize repository, compares clusters, and verifies and
// restores exports. Installed as kubectl-setdrift, it runs as kubectl
// setdrift.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"setdrift.io/setdrift-engine/internal/version"
)

func main() { os.Exit(run()) }

// run runs the command line and returns the exit status; main exits with
// it once run's deferred calls have run.
func run() int {
	// Ctrl-C stops in-flight calls, so an export ends cleanly instead of mid-write
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	// restore normal signal handling when run returns
	defer stop()
	// after the first Ctrl-C, release the signal so a second Ctrl-C force-quits
	// an export stuck somewhere that ignores ctx; ends when ctx does, at the
	// latest when run returns
	go func() { <-ctx.Done(); stop() }()

	err := newRootCmd().ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	// a command that sets its own exit status (diff --exit-code)
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.err != nil {
			fmt.Fprintln(os.Stderr, "error:", ee.err)
		}
		return ee.code
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}

// newRootCmd builds the command tree, fresh for each run, so tests can run
// commands one after another.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "setdrift",
		Short: "Export Kubernetes cluster configuration as a Git-ready repository",
		// cobra settings to avoid stderr duplication
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// run by kubectl as `kubectl setdrift`: show that name in help and usage text
	if strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") == "kubectl-setdrift" {
		root.Annotations = map[string]string{cobra.CommandDisplayNameAnnotation: "kubectl setdrift"}
	}

	// the subcommands are in this folder's other files; version is small enough to define here
	root.AddCommand(newExportCmd(), newVerifyCmd(), newDiffCmd(), newSecretsCmd(), newSnapshotCmd(), newHelmPostRenderCmd(), &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "setdrift %s (%s)\n", version.Version, version.Commit)
		},
	})
	return root
}
