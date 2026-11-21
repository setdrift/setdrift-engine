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
	"k8s.io/client-go/tools/clientcmd"

	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/collect"
	"setdrift.io/setdrift-engine/pkg/diff"
	"setdrift.io/setdrift-engine/pkg/platform"
	"setdrift.io/setdrift-engine/pkg/redact"
	"setdrift.io/setdrift-engine/pkg/snapshot"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// diffOptions holds diff's flags.
type diffOptions struct {
	kubeconfig string
	namespaces []string
	nsMap      []string
	format     string
	exitCode   bool
	maxFields  int
	// CI/CD pipelines' field managers, for reading a live side
	ciFieldManagers []string
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

// newDiffCmd builds the diff command.
func newDiffCmd() *cobra.Command {
	o := &diffOptions{}
	cmd := &cobra.Command{
		Use:   "diff A B",
		Short: "Compare two clusters, or a cluster with an earlier snapshot (read-only)",
		Long: `Diff compares two clusters, or one cluster with an earlier snapshot of
itself, and says what differs and what it can tell about why: who manages
each resource on each side, whether a Helm chart version differs, and which
differences are only each cluster's own values (its name, region, account).

A and B are each a kubeconfig context, a snapshot file (.pb or .json: each
export saves both in its snapshots/ directory), or a directory of manifests:
  - context:NAME is always the kubeconfig context NAME (context:arn:aws:... too);
  - a path (with a / or \, or ending .pb, .json, .yaml or .yml) is always a
    file or directory, and must exist;
  - a bare name is a file or directory if one exists here, else a context.
    A bare name that is both is an error, as the two compare different
    things: write ./prod for the directory, or context:prod for the cluster.
Both arguments are checked before either is read. They are read and
normalized as export reads them, so server defaults, status and secret values never show up as
differences: Secrets compare by their keys. Each platform's own objects
(EKS's, kind's) are not compared.

Volumes compare as bound, so a claim that names no storage class shows
each cluster's default class. Replicas a HorizontalPodAutoscaler sets are
left out. The nodes' shape (count, instance types, zones, capacity) is
shown, but not counted as a difference.

To compare two namespaces of one cluster, give the same context twice
with --map-namespace: setdrift diff prod prod --map-namespace shop=shop-prod.`,
		Example: `  setdrift diff staging prod
  setdrift diff setdrift-export/snapshots/2026-11-16T090000Z-prod.pb prod   # what changed since
  setdrift diff staging prod -n shop --map-namespace shop-staging=shop
  setdrift diff prod prod --map-namespace shop=shop-canary   # two namespaces of one cluster
  setdrift diff staging prod --format json > diff.json
  setdrift diff staging prod --exit-code            # in CI: exit 1 when they differ
  setdrift diff ./prod context:prod                 # an export directory named prod, and the cluster`,
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
	f.StringVar(&o.format, "format", "text", "output format: text, or json for every field")
	f.BoolVar(&o.exitCode, "exit-code", false, "exit 1 when A and B differ beyond each cluster's own values, 2 on error")
	f.IntVar(&o.maxFields, "max-fields", 5, "fields shown per resource in text output")
	f.StringSliceVar(&o.ciFieldManagers, "ci-field-manager", nil, "kubectl field manager of a CI/CD pipeline, as for export (repeatable)")
	return cmd
}

// run compares A and B, and prints or writes the result.
func (o *diffOptions) run(cmd *cobra.Command, a, b string) error {
	if o.format != "text" && o.format != "json" {
		return fmt.Errorf("--format %s: use text or json", o.format)
	}
	nsMap := map[string]string{}
	for _, m := range o.nsMap {
		from, to, ok := strings.Cut(m, "=")
		if !ok || from == "" || to == "" {
			return fmt.Errorf("--map-namespace %s: use A_NS=B_NS", m)
		}
		nsMap[from] = to
	}
	// both arguments resolved and checked before either is read: every problem at once
	ra, errA := o.resolve(a)
	rb, errB := o.resolve(b)
	if err := errors.Join(errA, errB); err != nil {
		return err
	}
	same := ra.same(rb)
	namespaces := o.namespaces
	if same {
		// one cluster (or snapshot) with itself: two of its namespaces
		if len(nsMap) == 0 {
			return fmt.Errorf("comparing %s with itself: to compare two of its namespaces, pass --map-namespace A_NS=B_NS", a)
		}
		if len(namespaces) == 0 {
			// each side is the mapped namespaces only, or the other side's
			// namespaces would pair with them
			for from := range nsMap {
				namespaces = append(namespaces, from)
			}
			sort.Strings(namespaces)
		}
	}
	bNamespaces := make([]string, 0, len(namespaces))
	for _, ns := range namespaces {
		if to, ok := nsMap[ns]; ok {
			ns = to
		}
		bNamespaces = append(bNamespaces, ns)
	}

	sa, fileA, err := o.read(cmd, ra, namespaces)
	if err != nil {
		return err
	}
	sb, fileB, err := o.read(cmd, rb, bNamespaces)
	if err != nil {
		return err
	}
	if err := checkMapping(nsMap, sa, sb, a, b); err != nil {
		return err
	}
	la, lb := sa.GetCluster().GetName(), sb.GetCluster().GetName()
	switch {
	case same:
		// two namespaces of one cluster
		la, lb = la+" ("+strings.Join(namespaces, ", ")+")", lb+" ("+strings.Join(bNamespaces, ", ")+")"
	case la == lb:
		// one cluster at two times, or a directory named after it: say which is which
		la, lb = label(ra, sa, la, fileA), label(rb, sb, lb, fileB)
		if la == lb {
			// taken in the same second: add each one's file
			la, lb = la+" ("+filepath.Base(ra.arg)+")", lb+" ("+filepath.Base(rb.arg)+")"
		}
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
	if o.format == "json" {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return err
		}
	} else {
		// two namespaces of one cluster have the same nodes
		printDiff(cmd.OutOrStdout(), res, o.maxFields, !same)
	}
	if o.exitCode && !res.Same() {
		return &exitError{code: 1}
	}
	return nil
}

// side is one argument of diff, resolved: a path (a snapshot file or a
// directory of manifests) or a kubeconfig context.
type side struct {
	arg     string
	path    string
	dir     bool
	context string
}

// contextPrefix marks an argument as a kubeconfig context, whatever files
// are around: context:prod. Only the first is stripped, so context:arn:aws:eks:... works.
const contextPrefix = "context:"

// resolve decides what arg names, and checks it can be read, without reading it:
//   - context:NAME is the kubeconfig context NAME;
//   - a path (with a / or \, or ending .pb, .json, .yaml or .yml) is a file or directory, and must exist;
//   - a bare name is a file or directory if one exists here, else a context; both is an error, as the
//     two would compare different things.
func (o *diffOptions) resolve(arg string) (side, error) {
	if name, ok := strings.CutPrefix(arg, contextPrefix); ok {
		if name == "" {
			return side{}, fmt.Errorf("%s: name the context after %s", arg, contextPrefix)
		}
		if _, err := loadConfig(o.kubeconfig, name); err != nil {
			return side{}, fmt.Errorf("context %s: %w", name, err)
		}
		return side{arg: arg, context: name}, nil
	}
	fi, statErr := os.Stat(arg)
	exists := statErr == nil
	if !pathLike(arg) {
		isContext := o.hasContext(arg)
		switch {
		case exists && isContext:
			what := "file"
			if fi.IsDir() {
				what = "directory"
			}
			return side{}, fmt.Errorf("%s is both a %s here and a kubeconfig context: write ./%s for the %s, or %s%s for the cluster",
				arg, what, arg, what, contextPrefix, arg)
		case !exists:
			if _, err := loadConfig(o.kubeconfig, arg); err != nil {
				return side{}, fmt.Errorf("%s is neither a file or directory here nor a kubeconfig context: %w", arg, err)
			}
			return side{arg: arg, context: arg}, nil
		}
	}
	if !exists {
		return side{}, fmt.Errorf("%s: no such file or directory (a snapshot file, a directory of manifests, or %sNAME for a kubeconfig context)", arg, contextPrefix)
	}
	if fi.IsDir() {
		return side{arg: arg, path: arg, dir: true}, nil
	}
	f, err := os.Open(arg)
	if err != nil {
		return side{}, readError(arg, err)
	}
	_ = f.Close() // opened only to see that it can be read
	return side{arg: arg, path: arg}, nil
}

// hasContext reports whether the kubeconfig has a context named name, reading only the kubeconfig.
func (o *diffOptions) hasContext(name string) bool {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if o.kubeconfig != "" {
		rules.ExplicitPath = expandHome(o.kubeconfig)
	}
	raw, err := rules.Load()
	return err == nil && raw.Contexts[name] != nil
}

// same reports whether two sides name the same cluster or the same file.
func (a side) same(b side) bool {
	if a.context != "" || b.context != "" {
		return a.context == b.context
	}
	pa, errA := filepath.Abs(a.path)
	pb, errB := filepath.Abs(b.path)
	return errA == nil && errB == nil && pa == pb
}

// read loads one side, limited to namespaces when set. file reports a
// snapshot file.
func (o *diffOptions) read(cmd *cobra.Command, sd side, namespaces []string) (*snapshotv1alpha1.Snapshot, bool, error) {
	switch {
	case sd.dir:
		src := &source{collector: collect.FileSource{Dir: sd.path}, plat: &platform.Stage{},
			cluster: &snapshotv1alpha1.Cluster{Name: filepath.Base(filepath.Clean(sd.path))}}
		s, err := capture(cmd.Context(), src, captureSettings{}, now().UTC())
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", sd.path, err)
		}
		return onlyNamespaces(s, namespaces), false, nil
	case sd.path != "":
		raw, err := os.ReadFile(sd.path)
		if err != nil {
			return nil, false, readError(sd.path, err)
		}
		s, err := snapshot.Parse(raw)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w (export saves snapshots, .pb and .json, in its snapshots/ directory)", sd.path, err)
		}
		return onlyNamespaces(s, namespaces), true, nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Reading %s...\n", sd.context)
	src, err := openCluster(cmd.Context(), clusterTarget{kubeconfig: o.kubeconfig, context: sd.context, namespaces: namespaces})
	if err != nil {
		return nil, false, fmt.Errorf("context %s: %w", sd.context, err)
	}
	s, err := capture(cmd.Context(), src, captureSettings{ciFieldManagers: o.ciFieldManagers}, now().UTC())
	if err != nil {
		return nil, false, fmt.Errorf("context %s: %w", sd.context, err)
	}
	return s, false, nil
}

// pathLike: an argument that names a file, so a typo in it is not tried as a context.
func pathLike(arg string) bool {
	return strings.ContainsAny(arg, `/\`) || slices.Contains([]string{".pb", ".json", ".yaml", ".yml"}, filepath.Ext(arg))
}

// readError says plainly why a file can't be read.
func readError(path string, err error) error {
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%s: can't read it (permission denied); check its owner and mode with `ls -l %s` "+
			"(snapshots are written readable by their owner only)", path, text.ShellQuote(path))
	}
	return fmt.Errorf("%s: %w", path, err)
}

// checkMapping refuses a --map-namespace pair whose namespace is not on its
// side: a typo would otherwise be ignored, and every resource compared unpaired.
func checkMapping(nsMap map[string]string, sa, sb *snapshotv1alpha1.Snapshot, a, b string) error {
	if len(nsMap) == 0 {
		return nil
	}
	na, nb := snapshotNamespaces(sa), snapshotNamespaces(sb)
	var errs []error
	froms := make([]string, 0, len(nsMap))
	for from := range nsMap {
		froms = append(froms, from)
	}
	sort.Strings(froms)
	for _, from := range froms {
		to := nsMap[from]
		if !na[from] {
			errs = append(errs, fmt.Errorf("--map-namespace %s=%s: %s has no namespace %s; its namespaces are %s", from, to, a, from, listOf(na)))
		}
		if !nb[to] {
			errs = append(errs, fmt.Errorf("--map-namespace %s=%s: %s has no namespace %s; its namespaces are %s", from, to, b, to, listOf(nb)))
		}
	}
	return errors.Join(errs...)
}

// snapshotNamespaces are the namespaces a snapshot holds: of its resources,
// its Namespace objects, and the ones it read.
func snapshotNamespaces(s *snapshotv1alpha1.Snapshot) map[string]bool {
	out := map[string]bool{}
	for _, r := range s.GetResources() {
		if r.GetNamespace() != "" {
			out[r.GetNamespace()] = true
		}
		if r.GetKind() == "Namespace" && r.GetApiVersion() == "v1" {
			out[r.GetName()] = true
		}
	}
	for _, ns := range s.GetScope().GetNamespaces() {
		out[ns] = true
	}
	return out
}

// listOf lists up to 15 of the set's names, sorted.
func listOf(set map[string]bool) string {
	if len(set) == 0 {
		return "none"
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > 15 {
		return strings.Join(names[:15], ", ") + fmt.Sprintf(" and %d more", len(names)-15)
	}
	return strings.Join(names, ", ")
}

// label names a side whose cluster name the other side shares: a directory
// by its path, a snapshot by when it was taken, a live cluster as now.
func label(sd side, s *snapshotv1alpha1.Snapshot, name string, file bool) string {
	if sd.dir {
		return sd.path + " (directory)"
	}
	return when(s, name, file)
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

// onlyNamespaces drops from s the resources outside namespaces, as export
// -n reads them (the namespaced resources and the Namespace objects), and
// returns s.
func onlyNamespaces(s *snapshotv1alpha1.Snapshot, namespaces []string) *snapshotv1alpha1.Snapshot {
	if len(namespaces) == 0 {
		return s
	}
	var kept []*snapshotv1alpha1.Resource
	for _, r := range s.Resources {
		if slices.Contains(namespaces, r.GetNamespace()) ||
			(r.GetKind() == "Namespace" && r.GetApiVersion() == "v1" && slices.Contains(namespaces, r.GetName())) {
			kept = append(kept, r)
		}
	}
	s.Resources = kept
	return s
}

// printDiff prints the comparison for the terminal: each side, the counts,
// then what differs.
func printDiff(w io.Writer, r diff.Result, maxFields int, nodes bool) {
	printSides(w, r, nodes)
	if r.Same() {
		fmt.Fprintln(w, "\nNo differences.")
		return
	}
	differ, envOnly := r.Counts()
	if differ > 0 {
		printChanged(w, r, differ, maxFields)
	}
	for _, only := range []struct {
		label string
		objs  []diff.Object
	}{{r.A.Label, r.OnlyA}, {r.B.Label, r.OnlyB}} {
		if len(only.objs) == 0 {
			continue
		}
		fmt.Fprintf(w, "\nOnly in %s (%d):\n", only.label, len(only.objs))
		for _, line := range grouped(only.objs) {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
	if envOnly > 0 {
		printEnvOnly(w, r, envOnly)
	}
}

// printSides prints what is compared: each side, the counts, what was left
// out, and, when nodes, the nodes' shape.
func printSides(w io.Writer, r diff.Result, nodes bool) {
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
	if len(r.Limited) > 0 {
		fmt.Fprintln(w, "\nCompared only what both sides read:")
		for _, n := range r.Limited {
			fmt.Fprintf(w, "  %s\n", n)
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
	if nodes {
		printNodes(w, r)
	}
}

// printChanged prints the resources that differ beyond the clusters' own
// values, with why and up to maxFields fields each.
func printChanged(w io.Writer, r diff.Result, differ, maxFields int) {
	la, lb := r.A.Label, r.B.Label
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
				fmt.Fprintf(w, "      ... %d more (--format json, or --max-fields)\n", len(c.Fields)-maxFields)
				break
			}
			fmt.Fprintf(w, "      %s: %s -> %s\n", f.Path, value(f.A), value(f.B))
		}
	}
}

// printEnvOnly lists the resources that differ only in the clusters' own
// values.
func printEnvOnly(w io.Writer, r diff.Result, envOnly int) {
	var names []string
	for _, c := range r.Changed {
		if c.EnvOnly() {
			names = append(names, describe(c.A.Key))
		}
	}
	fmt.Fprintf(w, "\nDiffer only in each cluster's own values (%d): %s\n", envOnly, truncate(strings.Join(names, ", "), 300))
}

// printNodes shows how the clusters' nodes differ: not counted as a
// difference, since nodes come and go with autoscaling.
func printNodes(w io.Writer, r diff.Result) {
	if len(r.NodesUnknown) > 0 {
		if len(r.NodesUnknown) == 1 {
			fmt.Fprintf(w, "Nodes: not compared (not read in %s)\n", r.NodesUnknown[0])
		}
		return
	}
	if len(r.Nodes) == 0 {
		fmt.Fprintln(w, "Nodes: same shape (count, instance types, zones, capacity)")
		return
	}
	fmt.Fprintln(w, "\nNodes (shown, not counted as a difference: nodes change with autoscaling):")
	width := 0
	for _, n := range r.Nodes {
		width = max(width, len(n.What))
	}
	for _, n := range r.Nodes {
		fmt.Fprintf(w, "  %-*s  %s -> %s\n", width, n.What, n.A, n.B)
	}
}

// describe names a resource: "ns/name Kind.group".
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

// managers says who manages a resource on each side.
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

// redactedValues are a redacted Secret value as the snapshot holds it, in
// data (base64) or stringData.
var redactedValues = map[string]bool{redact.Placeholder: true, base64.StdEncoding.EncodeToString([]byte(redact.Placeholder)): true}

// value renders a field's value for the terminal: (none), (redacted), or
// its JSON, truncated.
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

// kubeVersion is "Kubernetes 1.35", or "" when unknown.
func kubeVersion(v string) string {
	if v == "" {
		return ""
	}
	return "Kubernetes " + strings.TrimPrefix(v, "v")
}

// account is "account 1234", or "" when unknown.
func account(a string) string {
	if a == "" {
		return ""
	}
	return "account " + a
}
