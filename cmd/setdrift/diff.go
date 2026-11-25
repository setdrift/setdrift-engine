// compares two clusters, or a cluster with an earlier snapshot of itself, and says what differs and what it can tell about why
// the comparison itself lives in pkg/diff, this file handles the arguments, reading each side and printing the result

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
	"setdrift.io/setdrift-engine/pkg/capability"
	"setdrift.io/setdrift-engine/pkg/collect"
	"setdrift.io/setdrift-engine/pkg/diff"
	"setdrift.io/setdrift-engine/pkg/platform"
	"setdrift.io/setdrift-engine/pkg/redact"
	"setdrift.io/setdrift-engine/pkg/snapshot"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// diffOptions holds the flags diff was given, so run and the helpers below read them from one place
type diffOptions struct {
	kubeconfig string
	// limits the comparison to these namespaces, as theyre named in A
	namespaces []string
	// the --map-namespace pairs as given, A_NS=B_NS
	nsMap []string
	// the --map-value pairs as given, A_VALUE=B_VALUE
	valueMap []string
	// text or json
	format string
	// exits 1 when the sides differ, 2 on error, for ci
	exitCode bool
	// how many fields the text output shows per resource
	maxFields int
	// a translation to compare through, translations/<code> in an export
	translationDir string
}

// exitError ends the cli with a chosen exit status, main.go reads it
// err is printed before exiting when its set
type exitError struct {
	code int
	err  error
}

// Error gives back the wrapped error's message, or the exit status when theres no error to show
func (e *exitError) Error() string {
	// checking if theres no wrapped error, if so the message is the exit status
	if e.err == nil {
		return fmt.Sprintf("exit status %d", e.code)
	}
	return e.err.Error()
}

// Unwrap gives back the wrapped error, so errors.Is and errors.As see through the exit status
func (e *exitError) Unwrap() error { return e.err }

// newDiffCmd builds the diff command with its help text and flags, main.go adds it to the command tree
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

A value that differs only in each cluster's own name, region or account
is shown apart and not counted as a difference (--exit-code too): an ECR
image in each region, an ARN, a hostname with the cluster's name in it.
So 111111111111.dkr.ecr.us-east-1.amazonaws.com/app:v1.4.2 and
222222222222.dkr.ecr.us-west-2.amazonaws.com/app:v1.4.2 match, while a
different tag, app:v1.4.3, always counts. The region comes from the
cluster's nodes on any platform, the account from EKS.

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
			// invoking run with the two arguments, its defined below
			err := o.run(cmd, args[0], args[1])
			// checking if --exit-code is on and run failed without picking its own status, if so the error exits with 2
			if err != nil && o.exitCode && !errors.As(err, new(*exitError)) {
				// --exit-code keeps 1 for "they differ", so an error gets 2
				return &exitError{code: 2, err: err}
			}
			return err
		},
	}
	// registering the flags, each one fills a field of diffOptions above
	f := cmd.Flags()
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: KUBECONFIG or ~/.kube/config)")
	f.StringSliceVarP(&o.namespaces, "namespace", "n", nil, "compare only these namespaces, as named in A; skips cluster-scoped resources (repeatable)")
	f.StringSliceVar(&o.nsMap, "map-namespace", nil, "pair namespace A_NS in A with B_NS in B, as A_NS=B_NS (repeatable)")
	f.StringSliceVar(&o.valueMap, "map-value", nil, "set apart a value of A's and its counterpart in B as the clusters' own, as A_VALUE=B_VALUE, like a registry host (repeatable)")
	f.StringVar(&o.format, "format", "text", "output format: text, or json for every field")
	f.BoolVar(&o.exitCode, "exit-code", false, "exit 1 when A and B differ beyond each cluster's own values, 2 on error")
	f.IntVar(&o.maxFields, "max-fields", 5, "fields shown per resource in text output")
	f.StringVar(&o.translationDir, "translation-dir", "", "compare through a translation, its directory in the export (translations/<code>): what it changed on purpose and what its decisions cover show apart and don't count")
	return cmd
}

// run takes the two arguments, reads both sides, compares them and prints the result as text or json
// with --exit-code it gives back an exitError with status 1 when the sides differ
//
//revive:disable-next-line:cognitive-complexity,function-length the command in order, from flags to report; split by step after v0.3.0 (BACKLOG)
func (o *diffOptions) run(cmd *cobra.Command, a, b string) error {
	// checking if --format is something other than text or json, if so the flag was given wrong
	if o.format != "text" && o.format != "json" {
		return fmt.Errorf("--format %s: use text or json", o.format)
	}
	nsMap := map[string]string{}
	// going through the --map-namespace pairs and turning them into a map from A's namespace to B's
	for _, m := range o.nsMap {
		from, to, ok := strings.Cut(m, "=")
		// checking if the pair is missing its = or one of its names, if so the flag was given wrong
		if !ok || from == "" || to == "" {
			return fmt.Errorf("--map-namespace %s: use A_NS=B_NS", m)
		}
		nsMap[from] = to
	}
	valueMap := map[string]string{}
	// going through the --map-value pairs and turning them into a map from A's value to B's
	for _, m := range o.valueMap {
		from, to, ok := strings.Cut(m, "=")
		// checking if the pair is missing its = or one of its values, or a value is too short to set apart safely
		if !ok || len(from) < 3 || len(to) < 3 {
			return fmt.Errorf("--map-value %s: use A_VALUE=B_VALUE, each at least 3 characters", m)
		}
		valueMap[from] = to
	}
	var tr *translationInput
	// checking if a translation was given, if so its read before either side, so a wrong directory fails at once
	if o.translationDir != "" {
		var err error
		// invoking loadTranslation to read it, its defined in difftranslation.go
		if tr, err = loadTranslation(o.translationDir); err != nil {
			return err
		}
	}
	// invoking resolve on both arguments before either is read, so every problem shows at once, its defined below
	ra, errA := o.resolve(a)
	rb, errB := o.resolve(b)
	if err := errors.Join(errA, errB); err != nil {
		return err
	}
	// invoking same to tell if both arguments name the same cluster or file, its defined below
	same := ra.same(rb)
	namespaces := o.namespaces
	// checking if its one cluster or snapshot compared with itself, if so its two of its namespaces being compared
	if same {
		// checking if no namespaces were mapped, if so comparing it with itself would show nothing
		if len(nsMap) == 0 {
			return fmt.Errorf("comparing %s with itself: to compare two of its namespaces, pass --map-namespace A_NS=B_NS", a)
		}
		// checking if -n was left out, if so each side reads only the mapped namespaces
		// otherwise the other side's namespaces would pair with them
		if len(namespaces) == 0 {
			// adding each mapped namespace of A to the ones to read
			for from := range nsMap {
				namespaces = append(namespaces, from)
			}
			sort.Strings(namespaces)
		}
	}
	bNamespaces := make([]string, 0, len(namespaces))
	// going through A's namespaces and naming each the way B names it
	for _, ns := range namespaces {
		// checking if this namespace is mapped, if so B reads the name its mapped to
		if to, ok := nsMap[ns]; ok {
			ns = to
		}
		bNamespaces = append(bNamespaces, ns)
	}

	// invoking read to load each side into a snapshot, its defined below
	sa, fileA, err := o.read(cmd, ra, namespaces)
	if err != nil {
		return err
	}
	sb, fileB, err := o.read(cmd, rb, bNamespaces)
	if err != nil {
		return err
	}
	// checking if theres a translation, if so the target must be the second side, swapped when its the first
	if tr != nil {
		// invoking direction to tell which side is the target, its defined in difftranslation.go
		swap, why, err := tr.direction(sa, sb)
		if err != nil {
			return err
		}
		if swap {
			fmt.Fprintf(cmd.ErrOrStderr(), "Note: %s.\n", why)
			a, b, ra, rb, sa, sb, fileA, fileB = b, a, rb, ra, sb, sa, fileB, fileA
			namespaces, bNamespaces = bNamespaces, namespaces
			inverted := map[string]string{}
			for from, to := range nsMap {
				inverted[to] = from
			}
			nsMap = inverted
		}
		if tr.note != "" {
			fmt.Fprintln(cmd.ErrOrStderr(), tr.note)
		}
		// invoking olderApply to say when the target was applied from another run of the translation, its defined in difftranslation.go
		if note := tr.olderApply(sb); note != "" {
			fmt.Fprintln(cmd.ErrOrStderr(), note)
		}
	}
	// invoking checkMapping to refuse a mapped namespace that isnt on its side, its defined below
	if err := checkMapping(nsMap, sa, sb, a, b); err != nil {
		return err
	}
	la, lb := sa.GetCluster().GetName(), sb.GetCluster().GetName()
	// checking if the two sides would get the same label, if so each one is told apart
	switch {
	case same:
		// two namespaces of one cluster, each label names its namespaces
		la, lb = la+" ("+strings.Join(namespaces, ", ")+")", lb+" ("+strings.Join(bNamespaces, ", ")+")"
	case la == lb:
		// one cluster at two times, or a directory named after it
		// invoking label to say which is which, its defined below
		la, lb = label(ra, sa, la, fileA), label(rb, sb, lb, fileB)
		// checking if the labels still match, like two snapshots taken in the same second, if so each one gets its file name
		if la == lb {
			la, lb = la+" ("+filepath.Base(ra.arg)+")", lb+" ("+filepath.Base(rb.arg)+")"
		}
	}
	// invoking diff.Compare to compare the two snapshots, its defined in pkg/diff/diff.go
	// invoking capability.Load to read the capability map built into the binary, so each side is checked against its own platform, its defined in pkg/capability/capability.go
	capMap, err := capability.Load()
	if err != nil {
		return err
	}
	res, err := diff.Compare(sa, sb, diff.Options{NamespaceMap: nsMap, ValueMap: valueMap, LabelA: la, LabelB: lb, Capability: capMap})
	if err != nil {
		return err
	}
	// checking if theres a translation, if so the differences are sorted through it, ApplyTranslation is defined in pkg/diff/translation.go
	if tr != nil {
		// invoking forDiff to give the translation's objects as diff keys them, its defined in difftranslation.go
		t, err := tr.forDiff()
		if err != nil {
			return err
		}
		res.ApplyTranslation(t)
	}
	// checking if A was read live, if so its capture time is dropped, it only says something for a snapshot read earlier
	if !fileA {
		res.A.CapturedAt = ""
	}
	// checking the same for B
	if !fileB {
		res.B.CapturedAt = ""
	}
	// checking if json was asked for, if so every field is written as json, otherwise the text summary is printed
	if o.format == "json" {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return err
		}
	} else {
		// invoking printDiff to print the text summary, its defined below
		// the nodes are left out for two namespaces of one cluster, they have the same nodes
		printDiff(cmd.OutOrStdout(), res, o.maxFields, !same)
	}
	// checking if --exit-code is on and the sides differ, if so we exit with 1, Same is defined in pkg/diff/diff.go
	if o.exitCode && !res.Same() {
		return &exitError{code: 1}
	}
	return nil
}

// side is one argument of diff once resolved, a path to a snapshot file or a directory of manifests, or a kubeconfig context
type side struct {
	// the argument as it was typed
	arg string
	// the file or directory, empty for a context
	path string
	// tells if path is a directory of manifests rather than a snapshot file
	dir bool
	// the kubeconfig context, empty for a path
	context string
}

// contextPrefix marks an argument as a kubeconfig context whatever files are around, like context:prod
// only the first one is stripped, so context:arn:aws:eks:... works
const contextPrefix = "context:"

// resolve takes an argument and decides what it names, and checks it can be read, without reading it
// context:NAME is the kubeconfig context NAME
// a path (with a / or \, or ending .pb, .json, .yaml or .yml) is a file or directory, and must exist
// a bare name is a file or directory if one exists here, else a context, and both is an error since the two would compare different things
func (o *diffOptions) resolve(arg string) (side, error) {
	// checking if the argument starts with context:, if so its a kubeconfig context whatever files are around
	if name, ok := strings.CutPrefix(arg, contextPrefix); ok {
		// checking if nothing follows the prefix, if so theres no context to use
		if name == "" {
			return side{}, fmt.Errorf("%s: name the context after %s", arg, contextPrefix)
		}
		// invoking loadConfig to check the context exists and loads, its defined in kubeconfig.go
		if _, err := loadConfig(o.kubeconfig, name); err != nil {
			return side{}, fmt.Errorf("context %s: %w", name, err)
		}
		return side{arg: arg, context: name}, nil
	}
	fi, statErr := os.Stat(arg)
	exists := statErr == nil
	// invoking pathLike and checking if the argument is a bare name, if so it can be a file, a directory or a context, its defined below
	if !pathLike(arg) {
		// invoking hasContext to tell if the kubeconfig has a context by that name, its defined below
		isContext := o.hasContext(arg)
		// checking if the name is ambiguous or names no file, a file that isnt a context carries on below as a path
		switch {
		case exists && isContext:
			what := "file"
			// checking if its a directory, if so the error says directory
			if fi.IsDir() {
				what = "directory"
			}
			return side{}, fmt.Errorf("%s is both a %s here and a kubeconfig context: write ./%s for the %s, or %s%s for the cluster",
				arg, what, arg, what, contextPrefix, arg)
		case !exists:
			// invoking loadConfig to check the name is a context that loads, its defined in kubeconfig.go
			if _, err := loadConfig(o.kubeconfig, arg); err != nil {
				return side{}, fmt.Errorf("%s is neither a file or directory here nor a kubeconfig context: %w", arg, err)
			}
			return side{arg: arg, context: arg}, nil
		}
	}
	// checking if the path doesnt exist, if so the error lists what an argument can be
	if !exists {
		return side{}, fmt.Errorf("%s: no such file or directory (a snapshot file, a directory of manifests, or %sNAME for a kubeconfig context)", arg, contextPrefix)
	}
	// checking if its a directory, if so its a directory of manifests, unless its a translation or an export's platform set
	if fi.IsDir() {
		// invoking folderKind to tell them apart, its defined in folders.go
		switch f := folderKind(arg); f.kind {
		case folderTranslation:
			return side{}, fmt.Errorf("%s is %s, the changes for another platform, not a cluster's manifests: compare the export (%s), or the cluster it was applied to", arg, f.describe(), f.root)
		case folderPlatformSet:
			return side{}, fmt.Errorf("%s is the export's own platform set (%s), not a cluster's manifests: compare the export, %s", arg, f.rel, f.root)
		}
		return side{arg: arg, path: arg, dir: true}, nil
	}
	f, err := os.Open(arg) //#nosec G304 -- a snapshot file the user named
	// checking if the file cant be opened, if so readError says why, its defined below
	if err != nil {
		return side{}, readError(arg, err)
	}
	// closing it right away, it was only opened to see that it can be read
	_ = f.Close()
	return side{arg: arg, path: arg}, nil
}

// hasContext tells if the kubeconfig has a context with the given name, reading only the kubeconfig and not connecting
func (o *diffOptions) hasContext(name string) bool {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	// checking if --kubeconfig was given, if so its the file read
	if o.kubeconfig != "" {
		// invoking expandHome to turn a leading ~ into the home directory, its defined in kubeconfig.go
		rules.ExplicitPath = expandHome(o.kubeconfig)
	}
	raw, err := rules.Load()
	return err == nil && raw.Contexts[name] != nil
}

// same tells if two sides name the same cluster or the same file, so run knows its comparing one thing with itself
func (a side) same(b side) bool {
	// checking if either side is a context, if so they match only when both are the same context
	if a.context != "" || b.context != "" {
		return a.context == b.context
	}
	pa, errA := filepath.Abs(a.path)
	pb, errB := filepath.Abs(b.path)
	return errA == nil && errB == nil && pa == pb
}

// read takes a resolved side and loads it into a snapshot, limited to the given namespaces when there are any
// the bool it gives back tells if the side was a snapshot file, so run knows its capture time means something
func (o *diffOptions) read(cmd *cobra.Command, sd side, namespaces []string) (*snapshotv1alpha1.Snapshot, bool, error) {
	// checking if the side is a directory or a snapshot file, a context carries on below
	switch {
	case sd.dir:
		// setting up a source that reads the directory's manifests, the cluster named after the directory
		src := &source{collector: collect.FileSource{Dir: sd.path}, plat: &platform.Stage{},
			cluster: &snapshotv1alpha1.Cluster{Name: filepath.Base(filepath.Clean(sd.path))}}
		// invoking capture to read the manifests into a snapshot the way export does, its defined in export.go
		// now is the clock export stamps snapshots with, its defined in export.go
		s, err := capture(cmd.Context(), src, captureSettings{}, now().UTC())
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", sd.path, err)
		}
		// invoking onlyNamespaces to keep only the namespaces asked for, its defined below
		return onlyNamespaces(s, namespaces), false, nil
	case sd.path != "":
		raw, err := os.ReadFile(sd.path)
		// checking if the file cant be read, if so readError says why, its defined below
		if err != nil {
			return nil, false, readError(sd.path, err)
		}
		// invoking snapshot.Parse to decode the snapshot, protobuf or json, its defined in pkg/snapshot/snapshot.go
		s, err := snapshot.Parse(raw)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w (export saves snapshots, .pb and .json, in its snapshots/ directory)", sd.path, err)
		}
		// invoking onlyNamespaces to keep only the namespaces asked for, its defined below
		return onlyNamespaces(s, namespaces), true, nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Reading %s...\n", sd.context)
	// invoking openCluster to connect to the context, failing fast on a bad kubeconfig or an unreachable cluster, its defined in export.go
	// the namespaces go to the read itself here, so only those are read
	src, err := openCluster(cmd.Context(), clusterTarget{kubeconfig: o.kubeconfig, context: sd.context, namespaces: namespaces}, cmd.ErrOrStderr())
	if err != nil {
		return nil, false, fmt.Errorf("context %s: %w", sd.context, err)
	}
	// invoking capture to read the cluster into a snapshot the way export does, its defined in export.go
	s, err := capture(cmd.Context(), src, captureSettings{}, now().UTC())
	if err != nil {
		return nil, false, fmt.Errorf("context %s: %w", sd.context, err)
	}
	return s, false, nil
}

// pathLike tells if an argument looks like it names a file, by a slash or a file extension, so a typo in it is not tried as a context
func pathLike(arg string) bool {
	return strings.ContainsAny(arg, `/\`) || slices.Contains([]string{".pb", ".json", ".yaml", ".yml"}, filepath.Ext(arg))
}

// readError takes a path and the error reading it, and says clearly why the file cant be read
// a permission problem gets the command to check the file's owner and mode
func readError(path string, err error) error {
	// checking if permission was denied, if so the message says how to check, text.ShellQuote is defined in internal/text/text.go
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%s: can't read it (permission denied); check its owner and mode with `ls -l %s` "+
			"(snapshots are written readable by their owner only)", path, text.ShellQuote(path))
	}
	return fmt.Errorf("%s: %w", path, err)
}

// checkMapping refuses a --map-namespace pair whose namespace isnt on its side
// otherwise a typo would be ignored and every resource compared unpaired
func checkMapping(nsMap map[string]string, sa, sb *snapshotv1alpha1.Snapshot, a, b string) error {
	// checking if no namespaces were mapped, if so theres nothing to check
	if len(nsMap) == 0 {
		return nil
	}
	// invoking snapshotNamespaces to collect the namespaces each side holds, its defined below
	na, nb := snapshotNamespaces(sa), snapshotNamespaces(sb)
	var errs []error
	froms := make([]string, 0, len(nsMap))
	// collecting the mapped namespaces of A, sorted so the errors come in a stable order
	for from := range nsMap {
		froms = append(froms, from)
	}
	sort.Strings(froms)
	// going through each pair and checking both of its namespaces
	for _, from := range froms {
		to := nsMap[from]
		// checking if A has no such namespace, if so the error lists the ones it has, listOf is defined below
		if !na[from] {
			errs = append(errs, fmt.Errorf("--map-namespace %s=%s: %s has no namespace %s; its namespaces are %s", from, to, a, from, listOf(na)))
		}
		// checking the same for B
		if !nb[to] {
			errs = append(errs, fmt.Errorf("--map-namespace %s=%s: %s has no namespace %s; its namespaces are %s", from, to, b, to, listOf(nb)))
		}
	}
	return errors.Join(errs...)
}

// snapshotNamespaces gives back the namespaces a snapshot holds, those of its resources, its Namespace objects and the ones it read
func snapshotNamespaces(s *snapshotv1alpha1.Snapshot) map[string]bool {
	out := map[string]bool{}
	// going through the resources and collecting the namespaces they name
	for _, r := range s.GetResources() {
		// checking if the resource is namespaced, if so its namespace counts
		if r.GetNamespace() != "" {
			out[r.GetNamespace()] = true
		}
		// checking if the resource is a Namespace object, if so the namespace it is counts
		if r.GetKind() == "Namespace" && r.GetApiVersion() == "v1" {
			out[r.GetName()] = true
		}
	}
	// adding the namespaces the snapshot was limited to, they count even when theyre empty
	for _, ns := range s.GetScope().GetNamespaces() {
		out[ns] = true
	}
	return out
}

// listOf takes a set of names and lists up to 15 of them sorted, for an error message
func listOf(set map[string]bool) string {
	// checking if the set is empty, if so it says none
	if len(set) == 0 {
		return "none"
	}
	names := make([]string, 0, len(set))
	// collecting the names so they can be sorted
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	// checking if there are more than 15, if so the rest are only counted
	if len(names) > 15 {
		return strings.Join(names[:15], ", ") + fmt.Sprintf(" and %d more", len(names)-15)
	}
	return strings.Join(names, ", ")
}

// label names a side whose cluster name the other side shares, a directory by its path, a snapshot by when it was taken, a live cluster as now
func label(sd side, s *snapshotv1alpha1.Snapshot, name string, file bool) string {
	// checking if the side is a directory, if so its path tells it apart
	if sd.dir {
		return sd.path + " (directory)"
	}
	// invoking when to label it by time, its defined below
	return when(s, name, file)
}

// when labels a side of one cluster compared with itself by time, like "prod snapshot 2026-11-20 19:24:53Z", or "prod now"
func when(s *snapshotv1alpha1.Snapshot, name string, file bool) string {
	// checking if the side was read live, if so its now
	if !file {
		return name + " now"
	}
	// checking if the snapshot recorded when it was taken, if so the label includes it
	if t := s.GetCapturedAt(); t != nil {
		return name + " snapshot " + t.AsTime().UTC().Format("2006-01-02 15:04:05Z")
	}
	return name + " snapshot"
}

// onlyNamespaces drops the resources outside the given namespaces from a snapshot, and gives back the same snapshot
// it keeps what export -n reads, the namespaced resources and the Namespace objects
func onlyNamespaces(s *snapshotv1alpha1.Snapshot, namespaces []string) *snapshotv1alpha1.Snapshot {
	// checking if no namespaces were asked for, if so everything stays
	if len(namespaces) == 0 {
		return s
	}
	var kept []*snapshotv1alpha1.Resource
	// going through the resources and keeping the ones in the namespaces or that are one of their Namespace objects
	for _, r := range s.Resources {
		// checking if the resource is in one of the namespaces, or is the Namespace object of one
		if slices.Contains(namespaces, r.GetNamespace()) ||
			(r.GetKind() == "Namespace" && r.GetApiVersion() == "v1" && slices.Contains(namespaces, r.GetName())) {
			kept = append(kept, r)
		}
	}
	s.Resources = kept
	return s
}

// printDiff prints the comparison for the terminal, each side and the counts, then what differs
// nodes tells if the nodes' shape is shown too
func printDiff(w io.Writer, r diff.Result, maxFields int, nodes bool) {
	// invoking printSides to print what is compared and the counts, its defined below
	printSides(w, r, nodes)
	// checking if a translation sorted differences out, if so theyre listed first, shown and not counted, printSorted is defined below
	if r.Translation != "" {
		printSorted(w, r)
	}
	// checking if the sides match apart from each cluster's own values, if so theres nothing more to print, Same is defined in pkg/diff/diff.go
	if r.Same() {
		if r.Translation != "" {
			fmt.Fprintf(w, "\nNo real differences: %s is %s, translated (%s).\n", r.B.Label, r.A.Label, r.Translation)
			return
		}
		fmt.Fprintln(w, "\nNo differences.")
		return
	}
	// checking if resources name another platform's features than their side's, if so theyre listed first, a migration can look identical and not work
	if len(r.WontWork) > 0 {
		// invoking printWontWork to list them, its defined below
		printWontWork(w, r)
	}
	// invoking Counts to tell how many differ and how many differ only in each cluster's own values, its defined in pkg/diff/diff.go
	differ, envOnly := r.Counts()
	// checking if some resources really differ, if so printChanged lists them, its defined below
	if differ > 0 {
		printChanged(w, r, differ, maxFields)
	}
	// going through the resources only A has, then those only B has
	for _, only := range []struct {
		label string
		objs  []diff.Object
	}{{r.A.Label, r.OnlyA}, {r.B.Label, r.OnlyB}} {
		// checking if this side has none of its own, if so theres nothing to list
		if len(only.objs) == 0 {
			continue
		}
		fmt.Fprintf(w, "\nOnly in %s (%d):\n", only.label, len(only.objs))
		// invoking grouped to list them by namespace and manager, its defined below
		for _, line := range grouped(only.objs) {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
	// checking if some resources differ only in each cluster's own values, if so printEnvOnly names them, its defined below
	if envOnly > 0 {
		printEnvOnly(w, r, envOnly)
	}
}

// printSorted lists what a translation sorted out of the differences, translated on purpose, decided, and not moved by design, none of them counted
func printSorted(w io.Writer, r diff.Result) {
	for _, g := range []struct {
		title string
		items []diff.Sorted
	}{
		{"Translated on purpose (%d), not counted", r.Translated},
		{"Decided in translate-decisions.yaml (%d), not counted", r.Decided},
		{"Not moved by design (%d), not counted", r.NotMoved},
		{"Already true of the source (%d), not counted, they name another platform's features there", r.OnSource},
	} {
		if len(g.items) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n"+g.title+":\n", len(g.items))
		for _, s := range g.items {
			name := s.Object.Key.Kind + " " + s.Object.Key.Name
			if s.Object.Namespace != "" {
				name = s.Object.Namespace + "/" + s.Object.Key.Name + " " + s.Object.Key.Kind
			}
			var paths []string
			for _, f := range s.Fields {
				paths = append(paths, f.Path)
			}
			fmt.Fprintf(w, "  %s", name)
			if len(paths) > 0 {
				fmt.Fprintf(w, "  (%s)", strings.Join(paths, ", "))
			}
			fmt.Fprintln(w)
			for _, why := range s.Why {
				fmt.Fprintf(w, "      %s\n", why)
			}
		}
	}
}

// printWontWork lists the resources that wont work on their side's platform, with what each names
func printWontWork(w io.Writer, r diff.Result) {
	fmt.Fprintf(w, "\nWon't work on their cluster's platform (%d), they name another platform's features:\n", len(r.WontWork))
	for _, ww := range r.WontWork {
		name := ww.Object.Key.Kind + " " + ww.Object.Key.Name
		if ww.Object.Namespace != "" {
			name = ww.Object.Namespace + "/" + ww.Object.Key.Name + " " + ww.Object.Key.Kind
		}
		fmt.Fprintf(w, "  %s, in %s\n", name, ww.Side)
		for _, reason := range ww.Reasons {
			fmt.Fprintf(w, "      %s\n", reason)
		}
	}
}

// printSides prints what is compared, each side, the counts and what was left out
// nodes tells if the nodes' shape is printed too
func printSides(w io.Writer, r diff.Result, nodes bool) {
	la, lb := r.A.Label, r.B.Label
	fmt.Fprintf(w, "Comparing %s with %s\n", la, lb)
	width := max(len(la), len(lb)) + 1
	// going through both sides to print what is known about each
	for _, s := range []diff.Side{r.A, r.B} {
		var about []string
		// invoking kubeVersion and account to word those values, theyre defined below
		// going through the platform, version, region and account and keeping the ones that are known
		for _, v := range []string{s.Platform, kubeVersion(s.KubernetesVersion), s.Region, account(s.AccountKind, s.Account)} {
			// checking if the value is known, if so its shown
			if v != "" {
				about = append(about, v)
			}
		}
		// checking if this is a snapshot file whose label doesnt already say when it was taken, like one of another cluster, if so thats added
		if s.CapturedAt != "" && !strings.Contains(s.Label, s.CapturedAt) {
			about = append(about, "snapshot taken "+s.CapturedAt)
		}
		// checking if anything is known about the side, if so its printed on one line
		if len(about) > 0 {
			fmt.Fprintf(w, "  %-*s %s\n", width, s.Label+":", strings.Join(about, ", "))
		}
	}
	// checking if the comparison was limited to what both sides read, if so each limit is printed
	if len(r.Limited) > 0 {
		fmt.Fprintln(w, "\nCompared only what both sides read:")
		// printing each limit on its own line
		for _, n := range r.Limited {
			fmt.Fprintf(w, "  %s\n", n)
		}
	}
	// invoking Counts to tell how many differ and how many differ only in each cluster's own values, its defined in pkg/diff/diff.go
	differ, envOnly := r.Counts()
	fmt.Fprintf(w, "\n%d in both: %d identical", r.Identical+len(r.Changed)+r.AsTranslated, r.Identical)
	// checking if a translation sorted some out, if so the count line says how many are as it translated them
	if r.Translation != "" {
		fmt.Fprintf(w, ", %d as translated", r.AsTranslated)
	}
	fmt.Fprintf(w, ", %d differ", differ)
	// checking if some differ only in each cluster's own values, if so the count line says how many
	if envOnly > 0 {
		fmt.Fprintf(w, ", %d differ only in each cluster's own values (name, region, account)", envOnly)
	}
	fmt.Fprintf(w, "\nOnly in %s: %d   Only in %s: %d\n", la, len(r.OnlyA), lb, len(r.OnlyB))
	// checking if either side has platform objects, if so the output says they werent compared
	if r.A.PlatformObjects+r.B.PlatformObjects > 0 {
		fmt.Fprintf(w, "Not compared: each platform's own objects (%d in %s, %d in %s)\n", r.A.PlatformObjects, la, r.B.PlatformObjects, lb)
	}
	// checking if the nodes are wanted, if so printNodes prints their shape and printProfile the infrastructure, theyre defined below
	if nodes {
		printNodes(w, r)
		printProfile(w, r)
	}
}

// printChanged prints the resources that differ beyond each cluster's own values, with why and up to maxFields fields each
func printChanged(w io.Writer, r diff.Result, differ, maxFields int) {
	la, lb := r.A.Label, r.B.Label
	fmt.Fprintf(w, "\nDiffer (%d):\n", differ)
	// going through the changed resources
	for _, c := range r.Changed {
		// invoking EnvOnly and checking if this one differs only in each cluster's own values, if so printEnvOnly covers it, EnvOnly is defined in pkg/diff/diff.go
		if c.EnvOnly() {
			continue
		}
		// invoking describe to name the resource and managers to say who manages it on each side, theyre defined below
		fmt.Fprintf(w, "  %s  %s\n", describe(c.A.Key), managers(c.A.Manager, c.B.Manager, la, lb))
		// printing each reason the diff found for the difference
		for _, why := range c.Reasons {
			fmt.Fprintf(w, "      %s\n", why)
		}
		// printing the fields that differ, up to maxFields
		for i, f := range c.Fields {
			// checking if maxFields is reached, if so the rest are only counted
			if i == maxFields {
				fmt.Fprintf(w, "      ... %d more (--format json, or --max-fields)\n", len(c.Fields)-maxFields)
				break
			}
			// invoking value to render each side's value, its defined below
			fmt.Fprintf(w, "      %s: %s -> %s\n", f.Path, value(f.A), value(f.B))
		}
	}
}

// printEnvOnly lists on one line the resources that differ only in each cluster's own values
func printEnvOnly(w io.Writer, r diff.Result, envOnly int) {
	var names []string
	// going through the changed resources to collect the ones that differ only in each cluster's own values
	for _, c := range r.Changed {
		// invoking EnvOnly and checking if thats all this one differs in, if so describe names it, EnvOnly is defined in pkg/diff/diff.go, describe below
		if c.EnvOnly() {
			name := describe(c.A.Key)
			// checking if a field was set apart as bound to each cluster's region or account, if so the name says so
			if slices.ContainsFunc(c.Reasons, func(r string) bool { return strings.HasPrefix(r, "region-bound") }) {
				name += " (region-bound)"
			}
			names = append(names, name)
		}
	}
	// invoking truncate to keep the line to 300 characters, its defined in summary.go
	fmt.Fprintf(w, "\nDiffer only in each cluster's own values (%d): %s\n", envOnly, truncate(strings.Join(names, ", "), 300))
}

// printNodes shows how the clusters' nodes differ
// its not counted as a difference, since nodes come and go with autoscaling
func printNodes(w io.Writer, r diff.Result) {
	// checking if a side didnt read its nodes, if so theres nothing to compare
	if len(r.NodesUnknown) > 0 {
		// checking if only one side is missing them, if so the output names it, with both missing it says nothing
		if len(r.NodesUnknown) == 1 {
			fmt.Fprintf(w, "Nodes: not compared (not read in %s)\n", r.NodesUnknown[0])
		}
		return
	}
	// checking if the nodes have the same shape, if so thats one line
	if len(r.Nodes) == 0 {
		fmt.Fprintln(w, "Nodes: same shape (count, instance types, zones, capacity)")
		return
	}
	fmt.Fprintln(w, "\nNodes (shown, not counted as a difference: nodes change with autoscaling):")
	width := 0
	// finding the longest description, so the values line up
	for _, n := range r.Nodes {
		width = max(width, len(n.What))
	}
	// printing each difference in the nodes' shape
	for _, n := range r.Nodes {
		fmt.Fprintf(w, "  %-*s  %s -> %s\n", width, n.What, n.A, n.B)
	}
}

// printProfile shows how the clusters differ as infrastructure, not counted as a difference since each cluster has its own
func printProfile(w io.Writer, r diff.Result) {
	// checking if the profiles differ, if not theres nothing to show
	if len(r.Profile) == 0 {
		return
	}
	fmt.Fprintln(w, "\nCluster profile (shown, not counted as a difference: each cluster has its own infrastructure):")
	width := 0
	// finding the longest description, so the values line up
	for _, p := range r.Profile {
		width = max(width, len(p.What))
	}
	// printing each difference in the profiles
	for _, p := range r.Profile {
		fmt.Fprintf(w, "  %-*s  %s -> %s\n", width, p.What, p.A, p.B)
	}
}

// describe names a resource for the terminal, like "ns/name Kind.group"
func describe(k diff.Key) string {
	kind := k.Kind
	// checking if the kind has an api group, if so its added after the kind
	if k.Group != "" {
		kind += "." + k.Group
	}
	// checking if the resource is cluster-scoped, if so theres no namespace and the kind comes first
	if k.Namespace == "" {
		return kind + " " + k.Name
	}
	return k.Namespace + "/" + k.Name + " " + kind
}

// managers says who manages a resource on each side, once when its the same on both
func managers(a, b, la, lb string) string {
	// checking if both sides have the same manager, if so its named once
	if a == b {
		return "(" + a + ")"
	}
	return fmt.Sprintf("(%s in %s, %s in %s)", a, la, b, lb)
}

// grouped lists resources by namespace and manager, one line each, like "shop, applied by hand: ConfigMap a, b; Secret c"
func grouped(objs []diff.Object) []string {
	// group is the namespace and manager a line is for
	type group struct{ ns, manager string }
	kinds := map[group]map[string][]string{}
	var order []group
	// going through the resources and sorting each into its group by kind, keeping the groups in the order first seen
	for _, o := range objs {
		// the namespace as this side names it, which differs from the key with --map-namespace
		g := group{o.Namespace, o.Manager}
		// checking if this is the first resource of its group, if so the group is started
		if kinds[g] == nil {
			kinds[g] = map[string][]string{}
			order = append(order, g)
		}
		kinds[g][o.Key.Kind] = append(kinds[g][o.Key.Kind], o.Key.Name)
	}
	var out []string
	// going through the groups to write each one's line
	for _, g := range order {
		var parts []string
		ks := make([]string, 0, len(kinds[g]))
		// collecting the group's kinds so they can be sorted
		for k := range kinds[g] {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		// writing each kind with its names
		for _, k := range ks {
			parts = append(parts, k+" "+strings.Join(kinds[g][k], ", "))
		}
		ns := g.ns
		// checking if the group has no namespace, if so its cluster-scoped
		if ns == "" {
			ns = "(cluster-scoped)"
		}
		// invoking truncate to keep the line to 160 characters, its defined in summary.go
		out = append(out, truncate(fmt.Sprintf("%s, %s: %s", ns, g.manager, strings.Join(parts, "; ")), 160))
	}
	return out
}

// redactedValues holds a redacted Secret value as the snapshot holds it, base64 in data or as is in stringData, so value can show it as (redacted)
// redact.Placeholder is defined in pkg/redact/redact.go
var redactedValues = map[string]bool{redact.Placeholder: true, base64.StdEncoding.EncodeToString([]byte(redact.Placeholder)): true}

// value renders a field's value for the terminal, as (none), (redacted), or its json truncated
func value(v any) string {
	// checking if the field is missing on this side, if so its (none)
	if v == nil {
		return "(none)"
	}
	// checking if its a redacted Secret value, if so it shows as (redacted) rather than the placeholder
	if s, ok := v.(string); ok && redactedValues[s] {
		return "(redacted)"
	}
	b, err := json.Marshal(v)
	// checking if the value cant be written as json, if so its printed as go formats it
	if err != nil {
		return fmt.Sprint(v)
	}
	// invoking truncate to keep it to 70 characters, its defined in summary.go
	return truncate(string(b), 70)
}

// kubeVersion words a kubernetes version, like "Kubernetes 1.35", or gives back "" when its unknown
func kubeVersion(v string) string {
	// checking if the version is unknown, if so theres nothing to show
	if v == "" {
		return ""
	}
	return "Kubernetes " + strings.TrimPrefix(v, "v")
}

// account words a cloud account by its kind, like "account 1234" or "project acme-prod", or gives back "" when its unknown
func account(kind, a string) string {
	// checking if the account is unknown, if so theres nothing to show
	if a == "" {
		return ""
	}
	return kind + " " + a
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
