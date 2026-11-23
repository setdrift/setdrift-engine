// entry point for the setdrift cli, builds the command tree and runs whatever command was typed
// the commands themselves live in the other files in this folder, export.go, verify.go, diff.go and so on
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

// main hands off to run and exits with whatever status it gives back, run is defined below
func main() { os.Exit(run()) }

// run takes the command line, runs it and gives back the exit status
// its split out from main so the deferred cleanup still runs before the program exits
func run() int {
	// setting up ctrl-c so an export stops cleanly instead of in the middle of writing files
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	// putting normal signal handling back once run returns
	defer stop()
	// after the first ctrl-c we let go of the signal, so a second ctrl-c force quits anything stuck
	go func() { <-ctx.Done(); stop() }()

	// invoking newRootCmd to build the commands and run the one that was typed, its defined below
	err := newRootCmd().ExecuteContext(ctx)
	// checking if the command finished without an error, if so we exit with 0
	if err == nil {
		return 0
	}
	// checking if the command picked its own exit status, like diff --exit-code does
	var ee *exitError
	if errors.As(err, &ee) {
		// checking if theres also a message to print before exiting with that status
		if ee.err != nil {
			fmt.Fprintln(os.Stderr, "error:", ee.err)
		}
		return ee.code
	}
	// anything else is a terminal error, print it and exit with 1
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}

// newRootCmd builds the command tree fresh each time, so tests can run commands one after another without state carrying over
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "setdrift",
		Short: "Export Kubernetes cluster configuration as a Git-ready repository",
		// telling cobra not to print usage and errors itself, run already prints the error once
		SilenceUsage:  true,
		SilenceErrors: true,
		// invoking checkFlags before any command runs, so a flag given without the flag it needs, or with one it cant be used with, is refused, its defined in flagrules.go
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error { return checkFlags(cmd) },
	}
	// checking if kubectl started us as kubectl setdrift, if so the help text shows that name instead
	if strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") == "kubectl-setdrift" {
		root.Annotations = map[string]string{cobra.CommandDisplayNameAnnotation: "kubectl setdrift"}
	}

	// invoking each newXCmd function to add the subcommands, theyre defined in export.go, verify.go, diff.go, snapshot.go and helm.go
	// version is small enough to define right here
	root.AddCommand(newExportCmd(), newVerifyCmd(), newDiffCmd(), newSnapshotCmd(), newHelmPostRenderCmd(), &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "setdrift %s (%s)\n", version.Version, version.Commit)
		},
	})
	return root
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
