// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// snapshot command turns a binary snapshot into readable JSON

package main

import (
	"os"

	"github.com/spf13/cobra"

	"setdrift.io/setdrift-engine/pkg/snapshot"
)

// takes snap.pb provided, returns it as JSON format
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
				return err
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
