// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// just modifies the helm flow to inject a source-repo annotation into the rendered yaml

package main

import (
	"github.com/spf13/cobra"

	"setdrift.io/setdrift-engine/pkg/helmsource"
)

func newHelmPostRenderCmd() *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "helm-postrender --source-repo <url>",
		Short: "Helm post-renderer that records a release's chart repository",
		Long: `helm-postrender reads the manifests Helm rendered on stdin and writes them
to stdout with the annotation ` + helmsource.SourceRepo + `=<url> on every
object. Helm does not record which repository a chart came from; with this
annotation, setdrift export can write the exact helm install command in
RESTORE.md.

Most users don't need it: setdrift export --helm-repos <file> names the
repositories at export time, with no change to how Helm runs. This is for
teams that run Helm from scripts or CI and want each export to find the
repository in the cluster.

With Helm 4, install the plugin from contrib/helm and pass
--post-renderer setdrift-source-repo --post-renderer-args --source-repo=<url>.
With Helm 3, pass contrib/helm's postrender.sh as --post-renderer. A URL with
credentials is refused.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return helmsource.Annotate(cmd.InOrStdin(), cmd.OutOrStdout(), repo)
		},
	}
	cmd.Flags().StringVar(&repo, "source-repo", "", "the chart repository URL, e.g. https://charts.example.com or oci://registry/charts (required)")
	_ = cmd.MarkFlagRequired("source-repo")
	return cmd
}
