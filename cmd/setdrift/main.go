// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Command setdrift exports a k8s cluster's config as a git-ready kustomize repo, installed as kubectl-setdrift or kubectl setdrift
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

func main() {

	// interrupt tool using Ctrl-C, stops in flight calls and exports cleanly instead of mid-write
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)

	// restore normal signal handling when main returns
	defer stop()
	// after the first Ctrl-C, release the signal so a second Ctrl-C force-quits
	// an export stuck somewhere that ignores ctx
	go func() { <-ctx.Done(); stop() }()

	// parse arguments, pick and execute subcommands, reads cmd.Context()
	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		// a command that sets its own exit status (diff --exit-code)
		var ee *exitError
		if errors.As(err, &ee) {
			if ee.err != nil {
				fmt.Fprintln(os.Stderr, "error:", ee.err)
			}
			os.Exit(ee.code)
		}
		// print errors to stderr, exit with status 1
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// command tree built in function so tests can build a fresh tree for each run
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

	// the sub commands are defined in other files in this folder; version is small so just defined in line
	root.AddCommand(newExportCmd(), newVerifyCmd(), newDiffCmd(), newSecretsCmd(), newSnapshotCmd(), newHelmPostRenderCmd(), &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "setdrift %s (%s)\n", version.Version, version.Commit)
		},
	})
	return root
}
