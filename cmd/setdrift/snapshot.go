// works with snapshot files, for now that means printing a protobuf snapshot as readable json

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"setdrift.io/setdrift-engine/pkg/snapshot"
)

// newSnapshotCmd builds the snapshot command and its json subcommand, which prints a protobuf snapshot as readable json, main.go adds it to the command tree
func newSnapshotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Work with snapshot files",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "json FILE",
		Short: "Print a protobuf snapshot as readable JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// reading the snapshot file that was given
			b, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			// invoking snapshot.Unmarshal to decode the protobuf, its defined in pkg/snapshot/snapshot.go
			s, err := snapshot.Unmarshal(b)
			if err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			// invoking snapshot.MarshalInspectJSON to turn it into the readable json, its defined in pkg/snapshot/snapshot.go
			out, err := snapshot.MarshalInspectJSON(s)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(out)
			return err
		},
	})
	return cmd
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
