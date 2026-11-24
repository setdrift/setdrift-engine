// the translate command, which writes an export's set for another platform, like an EKS export for GKE, as an overlay beside the export's own sets

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/internal/version"
	"setdrift.io/setdrift-engine/pkg/capability"
	"setdrift.io/setdrift-engine/pkg/emit/kustomize"
	"setdrift.io/setdrift-engine/pkg/platform"
	"setdrift.io/setdrift-engine/pkg/translate"
	"setdrift.io/setdrift-engine/pkg/verify"
)

// translateOptions holds the translate command's flags
type translateOptions struct {
	to        string
	unmatched string
}

// newTranslateCmd builds the translate command, main.go adds it to the command tree
func newTranslateCmd() *cobra.Command {
	o := &translateOptions{}
	cmd := &cobra.Command{
		Use:   "translate EXPORT --to PLATFORM",
		Short: "Write an export's set for another platform",
		Long: "Writes the export's generic set for another platform, like an EKS export for GKE, as an overlay at that platform's directory (platform/gcp/gke): " +
			"storage classes, load balancers and node placement are rewritten for it, and what has no 1:1 equivalent is a decision in translate-decisions.yaml.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// invoking run to do the work, its defined below
			return o.run(cmd, args[0])
		},
	}
	cmd.Flags().StringVar(&o.to, "to", "", "the platform to translate for: eks, aks or gke")
	cmd.Flags().StringVar(&o.unmatched, "unmatched", "", "one choice for every undecided feature with no 1:1 equivalent: keep-source or leave-out")
	return cmd
}

// run translates the export at dir
func (o *translateOptions) run(cmd *cobra.Command, dir string) error {
	to := platform.Distribution(strings.ToLower(o.to))
	if to != platform.EKS && to != platform.AKS && to != platform.GKE {
		return fmt.Errorf("--to must be eks, aks or gke, not %q", o.to)
	}
	if o.unmatched != "" && o.unmatched != translate.KeepSource && o.unmatched != translate.LeaveOut {
		return fmt.Errorf("--unmatched must be keep-source or leave-out, not %q", o.unmatched)
	}
	// invoking verify.ClusterRoot to find the cluster's directory in the export, its defined in pkg/verify/dryrun.go
	root, err := verify.ClusterRoot(dir)
	if err != nil {
		return err
	}
	// invoking kustomize.ReadExportInfo to learn which platform the export came from, its defined in pkg/emit/kustomize/exportinfo.go
	info, err := kustomize.ReadExportInfo(root)
	if err != nil {
		return err
	}
	// invoking capability.Load to read the capability map built into the binary, its defined in pkg/capability/capability.go
	m, err := capability.Load()
	if err != nil {
		return err
	}
	from := platform.Unknown
	// checking if the export recorded its platform set, if so the source is the platform whose directory it is
	if info != nil && info.PlatformDir != "" {
		for _, p := range m.Platforms {
			if p.Translate.Dir == info.PlatformDir {
				from = p.Distribution
			}
		}
	}
	// invoking verify.Build to build the generic set the same way verify does, its defined in pkg/verify/dryrun.go
	objs, err := verify.Build(root, root)
	if err != nil {
		return err
	}
	tgt := m.For(to)
	// invoking translate.ReadDecisions to carry over the choices of an earlier run, its defined in pkg/translate/write.go
	earlier, err := translate.ReadDecisions(filepath.Join(root, filepath.FromSlash(tgt.Translate.Dir)))
	if err != nil {
		return err
	}
	// invoking translate.Translate to work out the patches and decisions, its defined in pkg/translate/translate.go
	r, err := translate.Translate(objs, from, to, m, earlier)
	if err != nil {
		return err
	}
	// checking if a choice was given for every undecided feature, if so its applied to them
	if o.unmatched != "" {
		for i := range r.Decisions {
			if r.Decisions[i].Choose == "" && slices.Contains(r.Decisions[i].Options, o.unmatched) {
				r.Decisions[i].Choose = o.unmatched
			}
		}
		// working the translation out again, so the choices take effect
		r, err = translate.Translate(objs, from, to, m, r.Decisions)
		if err != nil {
			return err
		}
	}
	w := cmd.OutOrStdout()
	outDir := filepath.Join(root, filepath.FromSlash(r.Dir))
	// checking if any feature still has no choice and no default, if so translate stops and writes the decisions for the user to fill in
	if und := r.Undecided(); len(und) > 0 {
		if err := translate.WriteDecisions(outDir, r.Decisions); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s no equivalent on %s and %s your decision:\n", text.Plural(len(und), "feature has", "features have"), tgt.Name, text.Word(len(und), "needs", "need"))
		for _, d := range und {
			fmt.Fprintf(w, "  %s\n      %s\n", d.Feature, d.What)
		}
		fmt.Fprintf(w, "\nWrote %s: set choose for each (%s), then run translate again, or pass --unmatched.\n", filepath.Join(outDir, translate.DecisionsFile), strings.Join(und[0].Options, ", "))
		return errors.New("translation stopped: decisions needed")
	}
	// invoking rootResources to read the generic set's directories from the cluster's kustomization, its defined below
	res, err := rootResources(root)
	if err != nil {
		return err
	}
	// invoking translate.Write to write the overlay, its defined in pkg/translate/write.go
	if err := translate.Write(root, r, res, version.Version); err != nil {
		return err
	}
	src := "a cluster on no known cloud"
	if from != platform.Unknown {
		src = m.For(from).Name
	}
	fmt.Fprintf(w, "Translated %s from %s for %s: %s, %s.\n", root, src, tgt.Name, text.Plural(len(r.Patches), "object changed", "objects changed"), text.Plural(len(r.Decisions), "decision", "decisions"))
	fmt.Fprintf(w, "Overlay: %s (README.md says what changed)\n", outDir)
	for _, n := range r.Notes {
		fmt.Fprintf(w, "  - %s\n", n)
	}
	fmt.Fprintf(w, "Apply it on %s with: kubectl apply --server-side -k %s\n", tgt.Name, outDir)
	return nil
}

// rootResources reads the resources the cluster's kustomization.yaml lists, the generic set's directories
func rootResources(root string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(root, "kustomization.yaml")) //#nosec G304 -- the export directory the user named
	if err != nil {
		return nil, err
	}
	var k struct {
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(b, &k); err != nil {
		return nil, err
	}
	return k.Resources, nil
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
