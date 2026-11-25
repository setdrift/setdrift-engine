// prints and writes what setdrift verify found, the differences grouped by cause with their values and who changed them, the likely cause overall, and the report files

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/verify"
)

// verifyDocument is one verify run as the report files hold it, the printed result reads from it too
type verifyDocument struct {
	// when the run started, in RFC3339
	VerifiedAt string `json:"verifiedAt"`
	// the kubeconfig context verify ran against
	Context string `json:"context"`
	// the cluster's name, from that context
	Cluster string `json:"cluster"`
	// the directory that was checked, the cluster's or its platform set's
	Directory string `json:"directory"`
	// Set says which set was checked, and why
	Set string `json:"set"`
	// the cluster the export came from, as the export recorded it
	ExportCluster string `json:"exportCluster,omitempty"`
	// when the export was taken, as the export recorded it
	ExportedAt string `json:"exportedAt,omitempty"`
	// SameCluster tells if this is the cluster the export came from, its absent when unknown
	SameCluster *bool `json:"sameCluster,omitempty"`
	// Identity is who the cluster says the credentials are, when a resource could not be checked
	Identity string `json:"identity,omitempty"`
	Passed   bool   `json:"passed"`
	// which part of the test a failed run missed, from failReason in verify.go
	FailReason string `json:"failReason,omitempty"`
	// what most likely explains the differences, from likelyCause below
	LikelyCause string `json:"likelyCause,omitempty"`
	// the dry-run results themselves
	Report verify.Report `json:"report"`
	// the summary of the differences that likelyCause reads
	Analysis verify.Analysis `json:"analysis"`
	// NetworkPolicy is the network policy check, absent when it had nothing to say
	NetworkPolicy *networkPolicyCheck `json:"networkPolicy,omitempty"`
}

// causes are the groups the differences print in, by Diff.Kind, in this order, with the heading each group gets
var causes = []struct {
	kind  verify.Kind
	title string
}{
	{verify.KindWontWork, "Won't work on this platform (they name another platform's features)"},
	{verify.KindChanged, "Fields differ"},
	{verify.KindNotInCluster, "Not in the cluster (applying would create them)"},
	{verify.KindNotServed, "Type not served by the cluster (its CRD is missing)"},
	{verify.KindLost, "Spec or data missing from the export"},
	{verify.KindRejected, "Rejected by the cluster"},
	{verify.KindNotChecked, "Not checked: no permission (not counted)"},
}

// likelyCause takes a run and says what most likely explains its differences, or "" for a clean run
// the first cause that fits wins, missing permissions, then a different cluster, changes since the export, one common field, missing resources, missing CRDs
func likelyCause(d verifyDocument) string {
	a, r := d.Analysis, d.Report
	n := len(r.Diffs)
	// checking if resources name another platform's features, if so thats the cause to fix first, since the rest may follow from it
	// one a translation's decision explains is no cause, its decided, and when every difference is explained theres nothing to say
	wont, explained := 0, 0
	for _, df := range r.Diffs {
		switch {
		case df.Explanation != "":
			explained++
		case df.Kind == verify.KindWontWork:
			wont++
		}
	}
	if n > 0 && explained == n && len(r.NotChecked) == 0 {
		return ""
	}
	if wont > 0 {
		return fmt.Sprintf("%s would apply here but not work: %s another platform's features, like its load balancer class, storage provisioner or identity annotation. Each one below says what it names.", text.Plural(wont, "resource", "resources"), text.Word(wont, "it names", "they name"))
	}
	// checking if at least as many resources couldnt be checked as differ, if so missing permissions are the likely cause
	if nc := len(r.NotChecked); nc > 0 && nc >= n {
		who := "These credentials"
		// checking if we know who the credentials belong to, if so the cause names them
		if d.Identity != "" {
			who += " (" + d.Identity + ")"
		}
		// invoking refusals to say what was refused and how to fix it, its defined in verify.go
		return fmt.Sprintf("%s could not check %s, which are not counted: %s.", who, text.Plural(nc, "resource", "resources"), refusals(r.NotChecked, d.Identity))
	}
	// checking each cause in turn, the first that fits is the answer
	// most of them need at least half of the differences to share the cause
	switch {
	// nothing differs, so theres nothing to explain
	case n == 0:
		return ""
	// this isnt the export's cluster, so differences are expected
	case d.SameCluster != nil && !*d.SameCluster:
		return fmt.Sprintf("This is a different cluster than the export came from (%s), so differences are expected: "+
			"to see how the two clusters differ, run setdrift diff on them.", d.ExportCluster)
	// at least half were written after the export
	case a.ChangedAfterExport*2 >= n:
		when := ""
		// checking if the export recorded when it was taken, if so the cause says when, an export without the time leaves it out
		if d.ExportedAt != "" {
			when = " (" + d.ExportedAt + ")"
		}
		// invoking writers to list who wrote them, its defined below
		return fmt.Sprintf("The cluster changed after the export%s: %d of the %d resources were written since, by %s. "+
			"The export is not wrong: export again to include the changes, or review them as drift.",
			when, a.ChangedAfterExport, n, writers(a.Writers))
	// one field differs on most changed resources, the Analysis found it
	case a.CommonField != "":
		return fmt.Sprintf("One field differs on %d of the changed resources: %s. Something in the cluster sets it on each "+
			"(a mutating webhook, a service mesh, a controller), or the export should leave it out; if the export should, "+
			"please report it: https://github.com/setdrift/setdrift-engine/issues", a.CommonCount, a.CommonField)
	// at least half are missing from the cluster
	case a.ByKind[verify.KindNotInCluster]*2 >= n:
		return fmt.Sprintf("%d of the %d are not in the cluster: deleted after the export, or the export comes from another cluster "+
			"(check --context).", a.ByKind[verify.KindNotInCluster], n)
	// at least half are types the cluster doesnt serve, their CRDs are missing
	case a.ByKind[verify.KindNotServed]*2 >= n:
		return "Most are custom resources whose CustomResourceDefinitions the cluster lacks: create the CRDs first (see above), then verify again."
	}
	return ""
}

// writers takes the count of resources each field manager wrote and renders it like "kubectl-edit (4), helm (2)", the busiest first
func writers(m map[string]int) string {
	names := make([]string, 0, len(m))
	// collecting the manager names
	for k := range m {
		names = append(names, k)
	}
	// sorting by count, most first, and by name when counts tie, so the output is stable
	sort.Slice(names, func(i, j int) bool {
		// checking if the counts differ, if so the bigger count goes first
		if m[names[i]] != m[names[j]] {
			return m[names[i]] > m[names[j]]
		}
		return names[i] < names[j]
	})
	parts := make([]string, 0, len(names))
	// putting each manager next to its count
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s (%d)", n, m[n]))
	}
	// checking if no manager was recorded, if so the text says so instead of being empty
	if len(parts) == 0 {
		return "unknown field managers"
	}
	return strings.Join(parts, ", ")
}

// printVerify takes a run and prints it for the terminal, the result line first, then the skips, the missing CRDs, the differences by cause and the likely cause
// root and contextName go into the kubectl commands that create the missing CRDs
func printVerify(w io.Writer, d verifyDocument, root, contextName string) {
	r := d.Report
	result := "PASSED"
	// checking if the run failed, if so the result line says so
	if !d.Passed {
		result = "FAILED"
	}
	// invoking text.Plural for the resource count, its defined in internal/text/text.go
	// invoking CleanPercent and RequiredPercent for the figures, theyre defined in pkg/verify/verify.go
	fmt.Fprintf(w, "\n%s: %d of %s would not change (%.1f%%; %d%% required)", result, r.Clean, text.Plural(r.Total, "resource", "resources"),
		r.CleanPercent(), r.RequiredPercent())
	// invoking Unexplained and checking if any difference has no known explanation, if so the result line counts them, its defined in pkg/verify/verify.go
	if u := r.Unexplained(); u > 0 {
		fmt.Fprintf(w, "; %s", text.Plural(u, "difference unexplained", "differences unexplained"))
	}
	// checking if some resources couldnt be checked, if so the result line counts them too
	if n := len(r.NotChecked); n > 0 {
		fmt.Fprintf(w, "; %d not checked (no permission)", n)
	}
	fmt.Fprintln(w)
	// printing each kind of skip, sorted so the output is stable
	for _, k := range slices.Sorted(maps.Keys(r.Skipped)) {
		fmt.Fprintf(w, "Skipped %d: %s\n", r.Skipped[k], k)
	}
	// checking if the export has CRDs the cluster lacks, if so we print the commands that create them from the export's crds directory
	if n := len(r.MissingCRDs); n > 0 {
		crds := filepath.Join(root, "crds")
		// invoking text.Choose to pick is or are, its defined in internal/text/text.go
		fmt.Fprintf(w, "%s in the export %s not in this cluster: %s.\n"+
			"Their custom resources show below as types the cluster does not serve. Create them first:\n"+
			"    kubectl --context %s apply --server-side -k %s\n"+
			"    kubectl --context %s wait --for condition=established --timeout=60s -k %s\n",
			text.Plural(n, "CustomResourceDefinition", "CustomResourceDefinitions"), text.Choose(n == 1, "is", "are"),
			strings.Join(r.MissingCRDs, ", "), contextName, crds, contextName, crds)
	}
	// checking if nothing differs and nothing went unchecked, if so theres nothing more to print
	if len(r.Diffs)+len(r.NotChecked) == 0 {
		return
	}
	// checking if some resources couldnt be checked, if so the heading counts them next to the differences
	if len(r.NotChecked) > 0 {
		fmt.Fprintf(w, "\nWould change (%d), and not checked (%d):\n", len(r.Diffs), len(r.NotChecked))
	} else {
		fmt.Fprintf(w, "\nWould change (%d):\n", len(r.Diffs))
	}
	// invoking byCause to group the differences and the unchecked resources by cause, its defined below
	groups := byCause(r)
	// printing the groups in the order causes lists them
	for _, c := range causes {
		group := groups[c.kind]
		// checking if this cause has nothing in it, if so it gets no heading
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(w, "  %s (%d):\n", c.title, len(group))
		// checking if these are the unchecked resources, if so they print by namespace and what was refused, each resource is in the report
		if c.kind == verify.KindNotChecked {
			// invoking notCheckedLines for those lines, its defined below
			for _, line := range notCheckedLines(group) {
				fmt.Fprintf(w, "    %s\n", line)
			}
			continue
		}
		// checking if these are resources missing from the cluster, if so they fit on one line since theres nothing more to say than their names
		if c.kind == verify.KindNotInCluster {
			var refs []string
			// collecting their names
			for _, diff := range group {
				refs = append(refs, diff.Ref)
			}
			// invoking truncate to keep the line from running too long, its defined in summary.go
			fmt.Fprintf(w, "    %s\n", truncate(strings.Join(refs, ", "), 400))
			continue
		}
		// printing every other difference in full
		for _, diff := range group {
			// invoking printVerifyDiff to print one, its defined below
			printVerifyDiff(w, diff)
		}
	}
	// checking if a likely cause was found, if so it goes last
	if d.LikelyCause != "" {
		fmt.Fprintf(w, "\nLikely cause: %s\n", d.LikelyCause)
	}
}

// printVerifyDiff takes one difference and prints who wrote it since the export, its explanation, and its fields, up to fieldsShown, or what went wrong
func printVerifyDiff(w io.Writer, diff verify.Diff) {
	// invoking changedBy to add who wrote it after the export, its defined below
	fmt.Fprintf(w, "    %s%s\n", diff.Ref, changedBy(diff))
	// checking if the difference has a known explanation, if so it gets printed
	if diff.Explanation != "" {
		fmt.Fprintf(w, "        explained: %s\n", diff.Explanation)
	}
	// checking if there are no field values, if so the paths say what differs or what went wrong
	if len(diff.Fields) == 0 {
		// printing each path on its own line
		for _, p := range diff.Paths {
			fmt.Fprintf(w, "        %s\n", p)
		}
		return
	}
	// printing each field with both values and who set it in the cluster
	for i, f := range diff.Fields {
		// checking if fieldsShown are already printed, if so the rest are only counted, the report has them all
		if i == fieldsShown {
			fmt.Fprintf(w, "        ... %d more (in the report)\n", len(diff.Fields)-fieldsShown)
			break
		}
		// invoking value to render each side, its defined in diff.go, and setBy for the managers, its defined below
		fmt.Fprintf(w, "        %s: cluster %s, export %s%s\n", f.Path, value(f.Cluster), value(f.Export), setBy(f.SetBy, "  (set by ", ")"))
	}
}

// notCheckedLines takes the resources that couldnt be checked and groups them by namespace and by what was refused, one line each like "prod1: 2 resources (may not get)"
func notCheckedLines(group []verify.Diff) []string {
	// key is one namespace and refusal pair, the unit a line counts
	type key struct{ ns, refused string }
	counts := map[key]int{}
	var order []key
	// counting the resources for each pair, remembering the order pairs first show up in
	for _, d := range group {
		ns := "(cluster-scoped)"
		// checking if the ref has a space, the part after it starts with the namespace when theres one
		if _, rest, ok := strings.Cut(d.Ref, " "); ok {
			// checking if that part has a slash, if so whats before it is the namespace, otherwise the resource is cluster-scoped
			if n, _, ok := strings.Cut(rest, "/"); ok {
				ns = n
			}
		}
		refused := "may not get"
		// checking if it was patch that was refused, otherwise it was get
		if d.Refused == verify.RefusedPatch {
			refused = "may not patch, for the dry-run"
		}
		k := key{ns, refused}
		// checking if this pair is new, if so it takes its place in the order
		if counts[k] == 0 {
			order = append(order, k)
		}
		counts[k]++
	}
	// sorting by namespace, stable so get and patch keep the order they first showed up in
	sort.SliceStable(order, func(i, j int) bool { return order[i].ns < order[j].ns })
	out := make([]string, 0, len(order))
	// writing one line per pair
	for _, k := range order {
		out = append(out, fmt.Sprintf("%s: %s (%s)", k.ns, text.Plural(counts[k], "resource", "resources"), k.refused))
	}
	return out
}

// byCause takes a report and groups its differences, then the resources not checked, by their cause, so the printers can go through causes in order
// the appends start from a nil slice so the report's own Diffs backing array is never written into
func byCause(r verify.Report) map[verify.Kind][]verify.Diff {
	out := map[verify.Kind][]verify.Diff{}
	// putting each resource in the group for its kind
	for _, d := range append(append([]verify.Diff(nil), r.Diffs...), r.NotChecked...) {
		out[d.Kind] = append(out[d.Kind], d)
	}
	return out
}

// fieldsShown caps the fields printed per difference, the report has them all
const fieldsShown = 5

// setBy takes a field's owners and renders them like "kubectl via scale, helm", wrapped in before and after, or "" when theres none
// the wrapping lets the terminal add "  (set by ...)" while the markdown table cell gets the names alone
func setBy(owners []verify.Owner, before, after string) string {
	// checking if the field has no owners, if so theres nothing to render, not even the wrapping
	if len(owners) == 0 {
		return ""
	}
	parts := make([]string, 0, len(owners))
	// naming each owner by its field manager
	for _, o := range owners {
		p := o.Manager
		// checking if it wrote through a subresource like scale, if so thats named too
		if o.Subresource != "" {
			p += " via " + o.Subresource
		}
		parts = append(parts, p)
	}
	return before + strings.Join(parts, ", ") + after
}

// changedBy takes a difference and renders who wrote it after the export, like "  (written after the export by kubectl-edit at 2026-11-20 10:02Z)", or "" when nobody did
func changedBy(d verify.Diff) string {
	// checking if nobody wrote it after the export, if so theres nothing to add
	if len(d.ChangedAfterExport) == 0 {
		return ""
	}
	parts := make([]string, 0, len(d.ChangedAfterExport))
	// naming each writer with when it wrote
	for _, wr := range d.ChangedAfterExport {
		parts = append(parts, wr.Manager+" at "+wr.Time.Format("2006-01-02 15:04Z"))
	}
	return "  (written after the export by " + strings.Join(parts, ", ") + ")"
}

// unsafeName matches the runs of characters that dont belong in a file name
var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// fileName takes a cluster name and makes it usable in a file name, so an EKS ARN or a context with slashes still makes one name
func fileName(cluster string) string {
	// replacing every unsafe run with a dash and trimming the ends, checking if anything is left, if not the name falls back to "cluster"
	if s := strings.Trim(unsafeName.ReplaceAllString(cluster, "-"), "-"); s != "" {
		return s
	}
	return "cluster"
}

// writeVerifyReport takes a run and writes it as <dir>/<time>-<cluster>-report.md and .json, and gives back the markdown file's path
// reports show live field values, redacted as the export would be but read from the cluster, so theyre private to their owner
// with ignore, when dir is the export's own verify/, a .gitignore keeps them out of the export's git repository
func writeVerifyReport(dir string, d verifyDocument, at time.Time, ignore bool) (string, error) {
	// creating the directory readable by its owner only, since the reports hold live values
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// checking if the reports go inside the export, if so git is told to ignore them
	if ignore {
		gi := filepath.Join(dir, ".gitignore")
		// checking if theres no .gitignore yet, if so one is written, an existing one is left as the owner made it
		if _, err := os.Stat(gi); errors.Is(err, fs.ErrNotExist) {
			if err := os.WriteFile(gi, []byte("# setdrift verify's reports: they show live cluster values, kept out of Git\n*\n"), 0o600); err != nil {
				return "", err
			}
		}
	}
	// invoking fileName to fit the cluster's name into the file name, its defined above
	base := filepath.Join(dir, at.UTC().Format("2006-01-02T150405Z")+"-"+fileName(d.Cluster)+"-report")
	js, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(base+".json", append(js, '\n'), 0o600); err != nil {
		return "", err
	}
	// invoking verifyMarkdown to render the markdown report, its defined below
	if err := os.WriteFile(base+".md", verifyMarkdown(d), 0o600); err != nil {
		return "", err
	}
	return base + ".md", nil
}

// verifyMarkdown takes a run and renders it as the markdown report, a summary table first, then every difference by cause with all its fields
func verifyMarkdown(d verifyDocument) []byte {
	var b bytes.Buffer
	r := d.Report
	result := "Passed"
	// checking if the run failed, if so the result names the reason
	if !d.Passed {
		result = "Failed: " + d.FailReason
	}
	fmt.Fprintf(&b, "# SetDrift verify: %s\n\n", d.Cluster)
	fmt.Fprintf(&b, "| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Result | **%s** |\n", result)
	// invoking CleanPercent, RequiredPercent and Unexplained for the figures, theyre defined in pkg/verify/verify.go
	fmt.Fprintf(&b, "| Would not change | %d of %d (%.1f%%; %d%% required) |\n", r.Clean, r.Total, r.CleanPercent(), r.RequiredPercent())
	fmt.Fprintf(&b, "| Unexplained differences | %d |\n", r.Unexplained())
	// checking if some resources couldnt be checked, if so the table counts them and names the credentials when known
	if len(r.NotChecked) > 0 {
		// invoking text.Choose to add the identity only when its known, its defined in internal/text/text.go
		fmt.Fprintf(&b, "| Not checked (no permission) | %d%s |\n", len(r.NotChecked), text.Choose(d.Identity != "", ", as "+d.Identity, ""))
	}
	// checking if the run was limited to some namespaces, if so the table lists them
	if len(r.Namespaces) > 0 {
		fmt.Fprintf(&b, "| Namespaces | %s (-n) |\n", strings.Join(r.Namespaces, ", "))
	}
	fmt.Fprintf(&b, "| Verified | %s, against context `%s` |\n", d.VerifiedAt, d.Context)
	fmt.Fprintf(&b, "| Checked | `%s`: %s |\n", d.Directory, d.Set)
	// checking if the export recorded its source, if so the table names it
	if d.ExportCluster != "" {
		fmt.Fprintf(&b, "| Export | from %s, taken %s |\n", d.ExportCluster, d.ExportedAt)
	}
	// checking if its known whether this is the export's cluster, if so the table says yes or no
	if d.SameCluster != nil {
		fmt.Fprintf(&b, "| Same cluster as the export | %s |\n", text.Choose(*d.SameCluster, "yes", "no"))
	}
	// adding a row for each kind of skip, sorted so the report is stable
	for _, k := range slices.Sorted(maps.Keys(r.Skipped)) {
		fmt.Fprintf(&b, "| Skipped | %d: %s |\n", r.Skipped[k], k)
	}
	// checking if the export has CRDs the cluster lacks, if so the table lists them
	if len(r.MissingCRDs) > 0 {
		fmt.Fprintf(&b, "| CRDs missing from the cluster | %s |\n", strings.Join(r.MissingCRDs, ", "))
	}
	// checking if the network policy check had something to say, if so the table has it
	if c := d.NetworkPolicy; c != nil {
		label := "Network policies"
		switch {
		case c.Failed:
			label = "**Network policies not enforced**"
		case c.Warning:
			label = "**Network policies (warning)**"
		}
		fmt.Fprintf(&b, "| %s | %s |\n", label, c.Message)
	}
	// checking if a likely cause was found, if so it goes right under the table
	if d.LikelyCause != "" {
		fmt.Fprintf(&b, "\n**Likely cause:** %s\n", d.LikelyCause)
	}
	// invoking byCause to group the differences and the unchecked resources by cause, its defined above
	groups := byCause(r)
	// writing a section per cause, in the order causes lists them
	for _, c := range causes {
		group := groups[c.kind]
		// checking if this cause has nothing in it, if so it gets no section
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s (%d)\n", c.title, len(group))
		// writing each resource under its own heading
		for _, diff := range group {
			fmt.Fprintf(&b, "\n### %s\n\n", diff.Ref)
			// checking if the difference has a known explanation, if so it gets written
			if diff.Explanation != "" {
				fmt.Fprintf(&b, "Explained: %s\n\n", diff.Explanation)
			}
			// listing who wrote the resource after the export, with the operation and the time
			for _, wr := range diff.ChangedAfterExport {
				fmt.Fprintf(&b, "- Written after the export by `%s` (%s) at %s\n", wr.Manager, wr.Operation, wr.Time.Format(time.RFC3339))
			}
			// checking if that list was written, if so a blank line ends it before the table
			if len(diff.ChangedAfterExport) > 0 {
				b.WriteString("\n")
			}
			// checking if there are field values, if so they go in a table, all of them unlike the terminal
			if len(diff.Fields) > 0 {
				b.WriteString("| Field | Cluster | Export | Set in the cluster by |\n|---|---|---|---|\n")
				// writing one row per field
				for _, f := range diff.Fields {
					// invoking mdCell to keep each value inside its cell, its defined below, value is defined in diff.go and setBy above
					fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", f.Path, mdCell(value(f.Cluster)), mdCell(value(f.Export)), setBy(f.SetBy, "", ""))
				}
				continue
			}
			// listing the paths instead, they say what differs or what went wrong
			for _, p := range diff.Paths {
				fmt.Fprintf(&b, "- %s\n", p)
			}
		}
	}
	return b.Bytes()
}

// mdCell takes a value and keeps it inside one markdown table cell, escaping pipes, swapping backticks for quotes and newlines for spaces, then wrapping it as code
func mdCell(s string) string {
	return "`" + strings.NewReplacer("|", `\|`, "`", "'", "\n", " ").Replace(s) + "`"
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
