// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// What setdrift verify prints and writes: its differences grouped by cause,
// with values and who changed them, the likely cause overall, and the
// report files.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"setdrift.io/setdrift-engine/pkg/verify"
)

// verifyDocument is a verify run, as the report files hold it.
type verifyDocument struct {
	VerifiedAt string `json:"verifiedAt"`
	Context    string `json:"context"`
	Cluster    string `json:"cluster"`
	Directory  string `json:"directory"`
	// Set says which set was checked, and why
	Set           string `json:"set"`
	ExportCluster string `json:"exportCluster,omitempty"`
	ExportedAt    string `json:"exportedAt,omitempty"`
	// SameCluster: whether this is the cluster the export came from; absent when unknown
	SameCluster *bool           `json:"sameCluster,omitempty"`
	Passed      bool            `json:"passed"`
	FailReason  string          `json:"failReason,omitempty"`
	LikelyCause string          `json:"likelyCause,omitempty"`
	Report      verify.Report   `json:"report"`
	Analysis    verify.Analysis `json:"analysis"`
}

// causes are the groups differences print in, by Diff.Kind, in order.
var causes = []struct {
	kind, title string
}{
	{verify.KindChanged, "Fields differ"},
	{verify.KindNotInCluster, "Not in the cluster (applying would create them)"},
	{verify.KindNotServed, "Type not served by the cluster (its CRD is missing)"},
	{verify.KindLost, "Spec or data missing from the export"},
	{verify.KindRejected, "Rejected by the cluster"},
}

// likelyCause says what most likely explains a run's differences, or "" for a clean run.
func likelyCause(d verifyDocument) string {
	a, r := d.Analysis, d.Report
	n := len(r.Diffs)
	switch {
	case n == 0:
		return ""
	case d.SameCluster != nil && !*d.SameCluster:
		return fmt.Sprintf("This is a different cluster than the export came from (%s), so differences are expected: "+
			"to see how the two clusters differ, run setdrift diff on them.", d.ExportCluster)
	case a.ChangedAfterExport*2 >= n:
		return fmt.Sprintf("The cluster changed after the export (%s): %d of the %d resources were written since, by %s. "+
			"The export is not wrong: export again to include the changes, or review them as drift.",
			d.ExportedAt, a.ChangedAfterExport, n, writers(a.Writers))
	case a.CommonField != "":
		return fmt.Sprintf("One field differs on %d of the changed resources: %s. Something in the cluster sets it on each "+
			"(a mutating webhook, a service mesh, a controller), or the export should leave it out; if the export should, "+
			"please report it: https://github.com/setdrift/setdrift-engine/issues", a.CommonCount, a.CommonField)
	case a.ByKind[verify.KindNotInCluster]*2 >= n:
		return fmt.Sprintf("%d of the %d are not in the cluster: deleted after the export, or the export comes from another cluster "+
			"(check --context).", a.ByKind[verify.KindNotInCluster], n)
	case a.ByKind[verify.KindNotServed]*2 >= n:
		return "Most are custom resources whose CustomResourceDefinitions the cluster lacks: create the CRDs first (see above), then verify again."
	}
	return ""
}

// writers renders "kubectl-edit (4), helm (2)".
func writers(m map[string]int) string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		if m[names[i]] != m[names[j]] {
			return m[names[i]] > m[names[j]]
		}
		return names[i] < names[j]
	})
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s (%d)", n, m[n]))
	}
	if len(parts) == 0 {
		return "unknown field managers"
	}
	return strings.Join(parts, ", ")
}

// printVerify prints a run for the terminal: the result, then the differences by cause.
func printVerify(w io.Writer, d verifyDocument, root, context string) {
	r := d.Report
	result := "PASSED"
	if !d.Passed {
		result = "FAILED"
	}
	fmt.Fprintf(w, "\n%s: %d of %s would not change (%.1f%%; %d%% required)", result, r.Clean, plural(r.Total, "resource", "resources"),
		r.CleanPercent(), r.RequiredPercent())
	if u := r.Unexplained(); u > 0 {
		fmt.Fprintf(w, "; %s", plural(u, "difference unexplained", "differences unexplained"))
	}
	fmt.Fprintln(w)
	reasons := make([]string, 0, len(r.Skipped))
	for k := range r.Skipped {
		reasons = append(reasons, k)
	}
	sort.Strings(reasons)
	for _, k := range reasons {
		fmt.Fprintf(w, "Skipped %d: %s\n", r.Skipped[k], k)
	}
	if n := len(r.MissingCRDs); n > 0 {
		crds := filepath.Join(root, "crds")
		fmt.Fprintf(w, "%s in the export %s not in this cluster: %s.\n"+
			"Their custom resources show below as types the cluster does not serve. Create them first:\n"+
			"    kubectl --context %s apply --server-side -k %s\n"+
			"    kubectl --context %s wait --for condition=established --timeout=60s -k %s\n",
			plural(n, "CustomResourceDefinition", "CustomResourceDefinitions"), choose(n == 1, "is", "are"),
			strings.Join(r.MissingCRDs, ", "), context, crds, context, crds)
	}
	if len(r.Diffs) == 0 {
		return
	}
	fmt.Fprintf(w, "\nWould change (%d):\n", len(r.Diffs))
	for _, c := range causes {
		var group []verify.Diff
		for _, diff := range r.Diffs {
			if diff.Kind == c.kind {
				group = append(group, diff)
			}
		}
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(w, "  %s (%d):\n", c.title, len(group))
		// resources with nothing more to say than their name fit on one line
		if c.kind == verify.KindNotInCluster {
			var refs []string
			for _, diff := range group {
				refs = append(refs, diff.Ref)
			}
			fmt.Fprintf(w, "    %s\n", truncate(strings.Join(refs, ", "), 400))
			continue
		}
		for _, diff := range group {
			fmt.Fprintf(w, "    %s%s\n", diff.Ref, changedBy(diff))
			if diff.Explanation != "" {
				fmt.Fprintf(w, "        explained: %s\n", diff.Explanation)
			}
			if len(diff.Fields) > 0 {
				for i, f := range diff.Fields {
					if i == 5 {
						fmt.Fprintf(w, "        ... %d more (in the report)\n", len(diff.Fields)-5)
						break
					}
					fmt.Fprintf(w, "        %s: cluster %s, export %s%s\n", f.Path, value(f.Cluster), value(f.Export), setBy(f.SetBy, "  (set by ", ")"))
				}
				continue
			}
			for _, p := range diff.Paths {
				fmt.Fprintf(w, "        %s\n", p)
			}
		}
	}
	if d.LikelyCause != "" {
		fmt.Fprintf(w, "\nLikely cause: %s\n", d.LikelyCause)
	}
}

// setBy renders a field's owners: "kubectl via scale, helm".
func setBy(owners []verify.Owner, before, after string) string {
	if len(owners) == 0 {
		return ""
	}
	parts := make([]string, 0, len(owners))
	for _, o := range owners {
		p := o.Manager
		if o.Subresource != "" {
			p += " via " + o.Subresource
		}
		parts = append(parts, p)
	}
	return before + strings.Join(parts, ", ") + after
}

// changedBy renders "  (written by kubectl-edit at 2026-11-20 10:02Z)" for a diff written after the export.
func changedBy(d verify.Diff) string {
	if len(d.ChangedAfterExport) == 0 {
		return ""
	}
	parts := make([]string, 0, len(d.ChangedAfterExport))
	for _, wr := range d.ChangedAfterExport {
		parts = append(parts, wr.Manager+" at "+wr.Time.Format("2006-01-02 15:04Z"))
	}
	return "  (written after the export by " + strings.Join(parts, ", ") + ")"
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// writeVerifyReport writes the run as <dir>/<time>-<cluster>-report.md and
// .json, and returns the Markdown file's path.
func writeVerifyReport(dir string, d verifyDocument, at time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// an EKS ARN or a context with slashes still makes one file name
	cluster := strings.Trim(unsafeName.ReplaceAllString(d.Cluster, "-"), "-")
	if cluster == "" {
		cluster = "cluster"
	}
	base := filepath.Join(dir, at.UTC().Format("2006-01-02T150405Z")+"-"+cluster+"-report")
	js, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(base+".json", append(js, '\n'), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(base+".md", verifyMarkdown(d), 0o644); err != nil {
		return "", err
	}
	return base + ".md", nil
}

// verifyMarkdown renders the run as a Markdown report.
func verifyMarkdown(d verifyDocument) []byte {
	var b bytes.Buffer
	r := d.Report
	result := "Passed"
	if !d.Passed {
		result = "Failed: " + d.FailReason
	}
	fmt.Fprintf(&b, "# SetDrift verify: %s\n\n", d.Cluster)
	fmt.Fprintf(&b, "| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Result | **%s** |\n", result)
	fmt.Fprintf(&b, "| Would not change | %d of %d (%.1f%%; %d%% required) |\n", r.Clean, r.Total, r.CleanPercent(), r.RequiredPercent())
	fmt.Fprintf(&b, "| Unexplained differences | %d |\n", r.Unexplained())
	fmt.Fprintf(&b, "| Verified | %s, against context `%s` |\n", d.VerifiedAt, d.Context)
	fmt.Fprintf(&b, "| Checked | `%s`: %s |\n", d.Directory, d.Set)
	if d.ExportCluster != "" {
		fmt.Fprintf(&b, "| Export | from %s, taken %s |\n", d.ExportCluster, d.ExportedAt)
	}
	if d.SameCluster != nil {
		fmt.Fprintf(&b, "| Same cluster as the export | %s |\n", choose(*d.SameCluster, "yes", "no"))
	}
	for k, n := range r.Skipped {
		fmt.Fprintf(&b, "| Skipped | %d: %s |\n", n, k)
	}
	if len(r.MissingCRDs) > 0 {
		fmt.Fprintf(&b, "| CRDs missing from the cluster | %s |\n", strings.Join(r.MissingCRDs, ", "))
	}
	if d.LikelyCause != "" {
		fmt.Fprintf(&b, "\n**Likely cause:** %s\n", d.LikelyCause)
	}
	for _, c := range causes {
		var group []verify.Diff
		for _, diff := range r.Diffs {
			if diff.Kind == c.kind {
				group = append(group, diff)
			}
		}
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s (%d)\n", c.title, len(group))
		for _, diff := range group {
			fmt.Fprintf(&b, "\n### %s\n\n", diff.Ref)
			if diff.Explanation != "" {
				fmt.Fprintf(&b, "Explained: %s\n\n", diff.Explanation)
			}
			for _, wr := range diff.ChangedAfterExport {
				fmt.Fprintf(&b, "- Written after the export by `%s` (%s) at %s\n", wr.Manager, wr.Operation, wr.Time.Format(time.RFC3339))
			}
			if len(diff.ChangedAfterExport) > 0 {
				b.WriteString("\n")
			}
			if len(diff.Fields) > 0 {
				b.WriteString("| Field | Cluster | Export | Set in the cluster by |\n|---|---|---|---|\n")
				for _, f := range diff.Fields {
					fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", f.Path, mdCell(value(f.Cluster)), mdCell(value(f.Export)), setBy(f.SetBy, "", ""))
				}
				continue
			}
			for _, p := range diff.Paths {
				fmt.Fprintf(&b, "- %s\n", p)
			}
		}
	}
	return b.Bytes()
}

// mdCell keeps a value inside one Markdown table cell.
func mdCell(s string) string {
	return "`" + strings.NewReplacer("|", `\|`, "`", "'", "\n", " ").Replace(s) + "`"
}

func choose(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
