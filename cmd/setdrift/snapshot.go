// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// setdrift snapshot: work with snapshot files.

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"setdrift.io/setdrift-engine/pkg/snapshot"
)

// newSnapshotCmd builds the snapshot command and snapshot json, which
// prints a protobuf snapshot as readable JSON.
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
			b, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			s, err := snapshot.Unmarshal(b)
			if err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
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
