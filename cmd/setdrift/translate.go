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
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/internal/version"
	"setdrift.io/setdrift-engine/pkg/capability"
	"setdrift.io/setdrift-engine/pkg/emit/kustomize"
	"setdrift.io/setdrift-engine/pkg/platform"
	"setdrift.io/setdrift-engine/pkg/snapshot"
	"setdrift.io/setdrift-engine/pkg/translate"
	"setdrift.io/setdrift-engine/pkg/verify"
)

// translateOptions holds the translate command's flags
type translateOptions struct {
	to        string
	unmatched string
	// acceptUnenforced chooses accept-unenforced for the export's NetworkPolicies, for an emergency
	acceptUnenforced bool
}

// newTranslateCmd builds the translate command, main.go adds it to the command tree
func newTranslateCmd() *cobra.Command {
	o := &translateOptions{}
	cmd := &cobra.Command{
		Use:   "translate <path-to-export> --to <platform>",
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
	cmd.Flags().BoolVar(&o.acceptUnenforced, "accept-unenforced-network-policies", false,
		"choose accept-unenforced for the export's NetworkPolicies, so setdrift verify passes on a target that doesn't enforce them (for an emergency; recorded in translate-decisions.yaml)")
	// invoking acceptSingular so the singular is the same flag, like --to gcp for gke, its defined in verifynetpol.go
	cmd.Flags().SetNormalizeFunc(acceptSingular)
	return cmd
}

// run translates the export at dir
func (o *translateOptions) run(cmd *cobra.Command, dir string) error {
	// invoking targetPlatform to read --to, a platform or its cloud's name, its defined below
	to, err := targetPlatform(o.to)
	if err != nil {
		return err
	}
	// invoking checkExportPath to say plainly when dir isnt an export, its defined below
	if err := checkExportPath(dir); err != nil {
		return err
	}
	// invoking folderKind to tell a translation or a platform set from an export, its defined in folders.go
	switch f := folderKind(dir); f.kind {
	case folderTranslation:
		return fmt.Errorf("%s is %s of the export at %s, not an export: translate the export, setdrift translate %s --to %s", dir, f.describe(), f.root, f.root, to)
	case folderPlatformSet:
		return fmt.Errorf("%s is the export's own platform set (%s), not an export: translate the export, setdrift translate %s --to %s", dir, f.rel, f.root, to)
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
	earlier, err := translate.ReadDecisions(filepath.Join(root, filepath.FromSlash(m.TranslationDir(from, to))))
	if err != nil {
		return err
	}
	known := translate.Source{}
	// checking if the export recorded the source's region and network policy engine, if so the translation uses them
	if info != nil {
		known.Region, known.NetworkPolicy = info.Region, info.NetworkPolicy
	}
	// invoking heldObjects to read the cloud-resource objects export held back, its defined below
	if known.Held, err = heldObjects(root); err != nil {
		return err
	}
	// invoking translate.Translate to work out the patches and decisions, its defined in pkg/translate/translate.go
	r, err := translate.Translate(objs, from, to, m, earlier, known)
	if err != nil {
		return err
	}
	// checking if the NetworkPolicies were accepted unenforced, if so the decision records it, and a flag with nothing to accept says so
	if o.acceptUnenforced {
		i := slices.IndexFunc(r.Decisions, func(d translate.Decision) bool { return d.Feature == translate.NetworkPolicyFeature })
		switch {
		case i >= 0:
			r.Decisions[i].Choose = translate.AcceptUnenforced
		case r.NetworkPolicies == 0:
			fmt.Fprintln(cmd.ErrOrStderr(), "--accept-unenforced-network-policies: the export has no NetworkPolicies, so there's nothing to accept")
		default:
			fmt.Fprintln(cmd.ErrOrStderr(), "--accept-unenforced-network-policies: the export doesn't say the source enforced its NetworkPolicies, so there's no decision to record")
		}
	}
	// checking if a choice was given for every undecided feature, or the NetworkPolicies accepted, if so the translation is worked out again with them
	if o.unmatched != "" || o.acceptUnenforced {
		for i := range r.Decisions {
			if o.unmatched != "" && r.Decisions[i].Choose == "" && slices.Contains(r.Decisions[i].Options, o.unmatched) {
				r.Decisions[i].Choose = o.unmatched
			}
		}
		// working the translation out again, so the choices take effect
		r, err = translate.Translate(objs, from, to, m, r.Decisions, known)
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
		var fresh, again []translate.Decision
		// sorting the new questions from the choices to make again, the source changed under them, each group gets its own heading
		for _, d := range und {
			if d.Waiting() {
				again = append(again, d)
			} else {
				fresh = append(fresh, d)
			}
		}
		if len(fresh) > 0 {
			fmt.Fprintf(w, "%s no equivalent on %s and %s your decision:\n", text.Plural(len(fresh), "feature has", "features have"), tgt.Name, text.Word(len(fresh), "needs", "need"))
			for _, d := range fresh {
				fmt.Fprintf(w, "  %s\n      %s\n", d.Feature, d.What)
			}
		}
		if len(again) > 0 {
			// a blank line between the two groups
			if len(fresh) > 0 {
				fmt.Fprintln(w)
			}
			fmt.Fprintf(w, "%s on the source since you chose, and %s choosing again:\n", text.Plural(len(again), "decision changed", "decisions changed"), text.Word(len(again), "needs", "need"))
			for _, d := range again {
				fmt.Fprintf(w, "  %s\n      Earlier you chose %s. It has changed on the source since then, so translate needs you to choose again. Your earlier choice is kept in the file as previous.\n      %s\n", d.Feature, d.Describe(d.Previous), d.What)
			}
		}
		fmt.Fprintf(w, "\nWrote %s: set choose for each (%s), then run translate again, or pass --unmatched.\n", filepath.Join(outDir, translate.DecisionsFile), strings.Join(und[0].Options, ", "))
		return errors.New("translation stopped: decisions needed")
	}
	// invoking rootResources to read the generic set's directories from the cluster's kustomization, its defined below
	res, err := rootResources(root)
	if err != nil {
		return err
	}
	// checking if earlier decisions name what the export no longer has, if so theyre dropped and said
	if len(r.Stale) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "Dropped %s from %s, for what the export no longer has: %s\n", text.Plural(len(r.Stale), "decision", "decisions"), translate.DecisionsFile, strings.Join(r.Stale, ", "))
	}
	// invoking translate.Write to write the overlay, its defined in pkg/translate/write.go
	ri := translate.RestoreInfo{Cluster: filepath.Base(root)}
	// checking if the export recorded the source, if so RESTORE.md names its cluster and version
	if info != nil {
		ri.Cluster, ri.SourceVersion = info.Cluster, info.KubernetesVersion
	}
	// invoking volumeClaims to list the claims whose disks stay on the source cloud, its defined below
	ri.Volumes = volumeClaims(root)
	// invoking sourceShape to read the source cluster's shape from the export's snapshot, for the target's create command, its defined below
	ri.Shape = sourceShape(root, info)
	if err := translate.Write(root, r, res, version.Version, ri); err != nil {
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
	// checking if the NetworkPolicies were accepted unenforced, if so its said loudly, every run
	if r.NetworkPolicyEngine != "" && translate.NetworkPolicyChoice(r.Decisions) == translate.AcceptUnenforced {
		fmt.Fprintf(w, "WARNING: %s accepted unenforced (%s): on a %s cluster that doesn't enforce them, they apply and do nothing.\n",
			text.Plural(r.NetworkPolicies, "NetworkPolicy is", "NetworkPolicies are"), translate.DecisionsFile, tgt.Name)
	}
	fmt.Fprintf(w, "Apply it on %s with: kubectl apply --server-side -k %s\n", tgt.Name, outDir)
	return nil
}

// targetPlatform reads --to, eks, aks or gke, or the cloud's name, aws, azure or gcp, without regard to case
func targetPlatform(s string) (platform.Distribution, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return platform.Unknown, errors.New("--to is required: the platform to translate for, eks, aks or gke")
	case "eks", "aws":
		return platform.EKS, nil
	case "aks", "azure":
		return platform.AKS, nil
	case "gke", "gcp", "google", "google-cloud":
		return platform.GKE, nil
	}
	return platform.Unknown, fmt.Errorf("--to must be eks, aks or gke (or aws, azure, gcp), not %q", s)
}

// checkExportPath says plainly what dir is when its no export, missing, a snapshot file or another file
func checkExportPath(dir string) error {
	fi, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s doesn't exist: give the directory setdrift export wrote", dir)
	}
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return nil
	}
	// checking if its a snapshot, by its name, if so it says what to use instead
	if strings.HasSuffix(dir, ".pb") || strings.HasSuffix(dir, ".json") {
		return fmt.Errorf("%s is a snapshot, and translate works on an export, the directory setdrift export writes (setdrift export -o <directory>). A snapshot can be compared with setdrift diff", dir)
	}
	return fmt.Errorf("%s is a file: give the directory setdrift export wrote", dir)
}

// heldObjects reads the cloud-resource objects export held back in cloud-resources/, none for an export without it
func heldObjects(root string) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	dir := filepath.Join(root, "cloud-resources")
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) && path == dir {
			return filepath.SkipDir
		}
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		b, err := os.ReadFile(path) //#nosec G304 -- inside the export directory the user named
		if err != nil {
			return err
		}
		var m map[string]any
		// checking if the file is a readable object, if not its named in the error
		if err := yaml.Unmarshal(b, &m); err != nil || m == nil {
			return fmt.Errorf("%s isn't a readable Kubernetes object: fix it, or export again", path)
		}
		out = append(out, &unstructured.Unstructured{Object: m})
		return nil
	})
	return out, err
}

// volumeClaims lists the claims the export pre-binds a volume for, restore/volumes/<namespace>/<claim>.yaml, as namespace/claim
func volumeClaims(root string) []string {
	var out []string
	nss, _ := os.ReadDir(filepath.Join(root, "restore", "volumes"))
	for _, ns := range nss {
		if !ns.IsDir() {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(root, "restore", "volumes", ns.Name()))
		for _, f := range files {
			if name, ok := strings.CutSuffix(f.Name(), ".yaml"); ok {
				out = append(out, ns.Name()+"/"+name)
			}
		}
	}
	return out
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

// sourceShape reads the source cluster's shape from the snapshot the export took, its ranges and nodes, an empty shape when theres none
func sourceShape(root string, info *kustomize.ExportInfo) translate.Shape {
	var sh translate.Shape
	// invoking exportSnapshot to find the snapshot taken with the export, its defined in difftranslation.go
	path, err := exportSnapshot(root, info)
	if err != nil {
		return sh
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return sh
	}
	// invoking snapshot.Parse to decode it, its defined in pkg/snapshot/snapshot.go
	s, err := snapshot.Parse(raw)
	if err != nil {
		return sh
	}
	net := s.GetInventory().GetProfile().GetNetwork()
	sh.PodCIDRs, sh.ServiceCIDRs = net.GetPodCidrs(), net.GetServiceCidrs()
	n := s.GetInventory().GetNodes()
	sh.Nodes, sh.Zones = int(n.GetCount()), len(n.GetZones())
	// checking if the allocatable totals read as quantities, if so theyre kept as millicores and bytes
	if q, err := apiresource.ParseQuantity(n.GetCpu()); err == nil {
		sh.MilliCPU = q.MilliValue()
	}
	if q, err := apiresource.ParseQuantity(n.GetMemory()); err == nil {
		sh.MemoryBytes = q.Value()
	}
	// going through the nodes' platforms for arm64
	for p := range n.GetPlatforms() {
		if strings.HasSuffix(p, "/arm64") {
			sh.Arm = true
		}
	}
	return sh
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
