// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// setdrift helm-postrender: a Helm post-renderer that records the chart
// repository on what Helm installs.

package main

import (
	"github.com/spf13/cobra"

	"setdrift.io/setdrift-engine/pkg/helmsource"
)

// newHelmPostRenderCmd builds the helm-postrender command.
func newHelmPostRenderCmd() *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "helm-postrender --chart-repo <url>",
		Short: "Helm post-renderer that records a release's chart repository",
		Long: `helm-postrender reads the manifests Helm rendered on stdin and writes them
to stdout with the annotation ` + helmsource.ChartRepo + `=<url> on every
object. Helm does not record which repository a chart came from; with this
annotation, setdrift export can write the exact helm install command in
RESTORE.md.

Most users don't need it: setdrift export --helm-repos <file> names the
repositories at export time, with no change to how Helm runs. This is for
teams that run Helm from scripts or CI and want each export to find the
repository in the cluster.

With Helm 4, install the plugin from contrib/helm and pass
--post-renderer setdrift-chart-repo --post-renderer-args --chart-repo=<url>.
With Helm 3, pass contrib/helm's postrender.sh as --post-renderer. A URL with
credentials is refused.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return helmsource.Annotate(cmd.InOrStdin(), cmd.OutOrStdout(), repo)
		},
	}
	cmd.Flags().StringVar(&repo, "chart-repo", "", "the chart repository URL, e.g. https://charts.example.com or oci://registry/charts (required)")
	_ = cmd.MarkFlagRequired("chart-repo") // fails only for a flag that does not exist
	return cmd
}
