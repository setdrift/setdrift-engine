// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// setdrift diff: compares two clusters, or a cluster with an earlier
// snapshot of itself, and says what differs and what it can tell about why.

package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"setdrift.io/setdrift-engine/pkg/collect"
	"setdrift.io/setdrift-engine/pkg/diff"
	"setdrift.io/setdrift-engine/pkg/platform"
	"setdrift.io/setdrift-engine/pkg/redact"
	"setdrift.io/setdrift-engine/pkg/snapshot"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

type diffOptions struct {
	kubeconfig string
	namespaces []string
	nsMap      []string
	output     string
	exitCode   bool
	maxFields  int
}

// exitError ends the CLI with a given status; err, when set, is printed.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit status %d", e.code)
	}
	return e.err.Error()
}

func (e *exitError) Unwrap() error { return e.err }

func newDiffCmd() *cobra.Command {
	o := &diffOptions{}
	cmd := &cobra.Command{
		Use:   "diff A B",
		Short: "Compare two clusters, or a cluster with an earlier snapshot (read-only)",
		Long: `Diff compares two clusters, or one cluster with an earlier snapshot of
itself, and says what differs and what it can tell about why: who manages
each resource on each side, whether a Helm chart version differs, and which
differences are only each cluster's own values (its name, region, account).

A and B are each a kubeconfig context, a snapshot file (export --snapshot),
or a directory of manifests. Both are read and normalized as export reads
them, so server defaults, status and secret values never show up as
differences: Secrets compare by their keys. Each platform's own objects
(EKS's, kind's) are not compared.`,
		Example: `  setdrift diff staging prod
  setdrift diff prod-monday.pb prod                 # what changed in prod since the snapshot
  setdrift diff staging prod -n shop --map-namespace shop-staging=shop
  setdrift diff staging prod -o json > diff.json
  setdrift diff staging prod --exit-code            # in CI: exit 1 when they differ`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			err := o.run(cmd, args[0], args[1])
			if err != nil && o.exitCode && !errors.As(err, new(*exitError)) {
				// --exit-code keeps 1 for "they differ"
				return &exitError{code: 2, err: err}
			}
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: KUBECONFIG or ~/.kube/config)")
	f.StringSliceVarP(&o.namespaces, "namespace", "n", nil, "compare only these namespaces, as named in A; skips cluster-scoped resources (repeatable)")
	f.StringSliceVar(&o.nsMap, "map-namespace", nil, "pair namespace A_NS in A with B_NS in B, as A_NS=B_NS (repeatable)")
	f.StringVarP(&o.output, "output", "o", "text", "text, or json for every field")
	f.BoolVar(&o.exitCode, "exit-code", false, "exit 1 when A and B differ beyond each cluster's own values, 2 on error")
	f.IntVar(&o.maxFields, "max-fields", 5, "fields shown per resource in text output")
	return cmd
}

func (o *diffOptions) run(cmd *cobra.Command, a, b string) error {
	if o.output != "text" && o.output != "json" {
		return fmt.Errorf("-o %s: use text or json", o.output)
	}
	nsMap := map[string]string{}
	for _, m := range o.nsMap {
		from, to, ok := strings.Cut(m, "=")
		if !ok || from == "" || to == "" {
			return fmt.Errorf("--map-namespace %s: use A_NS=B_NS", m)
		}
		nsMap[from] = to
	}
	bNamespaces := make([]string, 0, len(o.namespaces))
	for _, ns := range o.namespaces {
		if to, ok := nsMap[ns]; ok {
			ns = to
		}
		bNamespaces = append(bNamespaces, ns)
	}

	sa, fileA, err := o.read(cmd, a, o.namespaces)
	if err != nil {
		return err
	}
	sb, fileB, err := o.read(cmd, b, bNamespaces)
	if err != nil {
		return err
	}
	la, lb := sa.GetCluster().GetName(), sb.GetCluster().GetName()
	if la == lb {
		// one cluster at two times: say which is which
		la, lb = when(sa, la, fileA), when(sb, lb, fileB)
	}
	res, err := diff.Compare(sa, sb, diff.Options{NamespaceMap: nsMap, LabelA: la, LabelB: lb})
	if err != nil {
		return err
	}
	// a capture time says something only for a snapshot read earlier
	if !fileA {
		res.A.CapturedAt = ""
	}
	if !fileB {
		res.B.CapturedAt = ""
	}
	if o.output == "json" {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return err
		}
	} else {
		printDiff(cmd.OutOrStdout(), res, o.maxFields)
	}
	if o.exitCode && !res.Same() {
		return &exitError{code: 1}
	}
	return nil
}

// read loads one side: a snapshot file, a directory of manifests, or a
// kubeconfig context, limited to namespaces when set. file reports a
// snapshot file.
func (o *diffOptions) read(cmd *cobra.Command, arg string, namespaces []string) (s *snapshotv1alpha1.Snapshot, file bool, err error) {
	if fi, err := os.Stat(arg); err == nil {
		if fi.IsDir() {
			eo := &exportOptions{}
			src := &source{collector: collect.FileSource{Dir: arg}, plat: &platform.Stage{},
				cluster: &snapshotv1alpha1.Cluster{Name: filepath.Base(filepath.Clean(arg))}}
			s, err := eo.capture(cmd.Context(), src)
			if err != nil {
				return nil, false, fmt.Errorf("%s: %w", arg, err)
			}
			return onlyNamespaces(s, namespaces), false, nil
		}
		raw, err := os.ReadFile(arg)
		if err != nil {
			return nil, false, err
		}
		s, err := snapshot.Unmarshal(raw)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w (a snapshot is written with export --snapshot)", arg, err)
		}
		return onlyNamespaces(s, namespaces), true, nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Reading %s...\n", arg)
	eo := &exportOptions{kubeconfig: o.kubeconfig, context: arg, namespaces: namespaces}
	src, err := eo.open(cmd)
	if err != nil {
		return nil, false, fmt.Errorf("%s is not a file or directory, so it was read as a kubeconfig context: %w", arg, err)
	}
	s, err = eo.capture(cmd.Context(), src)
	return s, false, err
}

// when labels a side of one cluster compared with itself: "prod snapshot 2026-11-20 19:24:53Z", or "prod now".
func when(s *snapshotv1alpha1.Snapshot, name string, file bool) string {
	if !file {
		return name + " now"
	}
	if t := s.GetCapturedAt(); t != nil {
		return name + " snapshot " + t.AsTime().UTC().Format("2006-01-02 15:04:05Z")
	}
	return name + " snapshot"
}

// onlyNamespaces keeps a snapshot's resources in namespaces, as export -n
// reads them: the namespaced resources and the Namespace objects.
func onlyNamespaces(s *snapshotv1alpha1.Snapshot, namespaces []string) *snapshotv1alpha1.Snapshot {
	if len(namespaces) == 0 {
		return s
	}
	kept := s.Resources[:0:0]
	for _, r := range s.Resources {
		if slices.Contains(namespaces, r.GetNamespace()) ||
			(r.GetKind() == "Namespace" && r.GetApiVersion() == "v1" && slices.Contains(namespaces, r.GetName())) {
			kept = append(kept, r)
		}
	}
	s.Resources = kept
	return s
}

func printDiff(w io.Writer, r diff.Result, maxFields int) {
	la, lb := r.A.Label, r.B.Label
	fmt.Fprintf(w, "Comparing %s with %s\n", la, lb)
	width := max(len(la), len(lb)) + 1
	for _, s := range []diff.Side{r.A, r.B} {
		var about []string
		for _, v := range []string{s.Platform, kubeVersion(s.KubernetesVersion), s.Region, account(s.Account)} {
			if v != "" {
				about = append(about, v)
			}
		}
		if s.CapturedAt != "" && !strings.Contains(s.Label, s.CapturedAt) {
			// a snapshot file of another cluster: when it was taken
			about = append(about, "snapshot taken "+s.CapturedAt)
		}
		if len(about) > 0 {
			fmt.Fprintf(w, "  %-*s %s\n", width, s.Label+":", strings.Join(about, ", "))
		}
	}
	differ, envOnly := r.Counts()
	fmt.Fprintf(w, "\n%d in both: %d identical, %d differ", r.Identical+len(r.Changed), r.Identical, differ)
	if envOnly > 0 {
		fmt.Fprintf(w, ", %d differ only in each cluster's own values (name, region, account)", envOnly)
	}
	fmt.Fprintf(w, "\nOnly in %s: %d   Only in %s: %d\n", la, len(r.OnlyA), lb, len(r.OnlyB))
	if r.A.PlatformObjects+r.B.PlatformObjects > 0 {
		fmt.Fprintf(w, "Not compared: each platform's own objects (%d in %s, %d in %s)\n", r.A.PlatformObjects, la, r.B.PlatformObjects, lb)
	}
	if r.Same() {
		fmt.Fprintln(w, "\nNo differences.")
		return
	}

	if differ > 0 {
		fmt.Fprintf(w, "\nDiffer (%d):\n", differ)
		for _, c := range r.Changed {
			if c.EnvOnly() {
				continue
			}
			fmt.Fprintf(w, "  %s  %s\n", describe(c.A.Key), managers(c.A.Manager, c.B.Manager, la, lb))
			for _, why := range c.Reasons {
				fmt.Fprintf(w, "      %s\n", why)
			}
			for i, f := range c.Fields {
				if i == maxFields {
					fmt.Fprintf(w, "      ... %d more (-o json, or --max-fields)\n", len(c.Fields)-maxFields)
					break
				}
				fmt.Fprintf(w, "      %s: %s -> %s\n", f.Path, value(f.A), value(f.B))
			}
		}
	}
	for _, only := range []struct {
		label string
		objs  []diff.Object
	}{{la, r.OnlyA}, {lb, r.OnlyB}} {
		if len(only.objs) == 0 {
			continue
		}
		fmt.Fprintf(w, "\nOnly in %s (%d):\n", only.label, len(only.objs))
		for _, line := range grouped(only.objs) {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
	if envOnly > 0 {
		var names []string
		for _, c := range r.Changed {
			if c.EnvOnly() {
				names = append(names, describe(c.A.Key))
			}
		}
		fmt.Fprintf(w, "\nDiffer only in each cluster's own values (%d): %s\n", envOnly, truncate(strings.Join(names, ", "), 300))
	}
}

func describe(k diff.Key) string {
	kind := k.Kind
	if k.Group != "" {
		kind += "." + k.Group
	}
	if k.Namespace == "" {
		return kind + " " + k.Name
	}
	return k.Namespace + "/" + k.Name + " " + kind
}

func managers(a, b, la, lb string) string {
	if a == b {
		return "(" + a + ")"
	}
	return fmt.Sprintf("(%s in %s, %s in %s)", a, la, b, lb)
}

// grouped lists resources by namespace and manager: "shop, applied by hand: ConfigMap a, b; Secret c".
func grouped(objs []diff.Object) []string {
	type group struct{ ns, manager string }
	kinds := map[group]map[string][]string{}
	var order []group
	for _, o := range objs {
		// the namespace as this side names it (--map-namespace)
		g := group{o.Namespace, o.Manager}
		if kinds[g] == nil {
			kinds[g] = map[string][]string{}
			order = append(order, g)
		}
		kinds[g][o.Key.Kind] = append(kinds[g][o.Key.Kind], o.Key.Name)
	}
	var out []string
	for _, g := range order {
		var parts []string
		ks := make([]string, 0, len(kinds[g]))
		for k := range kinds[g] {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			parts = append(parts, k+" "+strings.Join(kinds[g][k], ", "))
		}
		ns := g.ns
		if ns == "" {
			ns = "(cluster-scoped)"
		}
		out = append(out, truncate(fmt.Sprintf("%s, %s: %s", ns, g.manager, strings.Join(parts, "; ")), 160))
	}
	return out
}

// redactedValue is a redacted Secret value as the snapshot holds it, in data (base64) or stringData.
var redactedValues = map[string]bool{redact.Placeholder: true, base64.StdEncoding.EncodeToString([]byte(redact.Placeholder)): true}

func value(v any) string {
	if v == nil {
		return "(none)"
	}
	if s, ok := v.(string); ok && redactedValues[s] {
		return "(redacted)"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return truncate(string(b), 70)
}

func kubeVersion(v string) string {
	if v == "" {
		return ""
	}
	return "Kubernetes " + strings.TrimPrefix(v, "v")
}

func account(a string) string {
	if a == "" {
		return ""
	}
	return "account " + a
}
