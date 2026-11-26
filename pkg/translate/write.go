package translate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"setdrift.io/setdrift-engine/pkg/capability"
)

// the files a translation writes in its platform directory
const (
	// DecisionsFile holds the decisions for features with no 1:1 equivalent, the user edits it and runs translate again
	DecisionsFile = "translate-decisions.yaml"
	// StampFile records that the directory is a translation, from which platform to which, so verify can say so
	StampFile = "translation.yaml"
	// equivalentsDir holds the target's equivalents chosen for cloud resources, inside the overlay
	equivalentsDir = "equivalents"
)

// Stamp is what StampFile holds
type Stamp struct {
	// Code names the translation, like te2g for EKS to GKE, the same as its directory's name
	Code string `json:"code,omitempty"`
	// ID is a short hash of what the overlay applies, the annotation on the target's objects is code-id, so an older apply is told apart
	ID           string `json:"id,omitempty"`
	From         string `json:"from"`
	To           string `json:"to"`
	TranslatedAt string `json:"translatedAt"`
	SetDrift     string `json:"setdriftVersion,omitempty"`
}

// ReadDecisions reads the decisions file in dir, none when it doesnt exist yet
func ReadDecisions(dir string) ([]Decision, error) {
	b, err := os.ReadFile(filepath.Join(dir, DecisionsFile)) //#nosec G304 -- the export directory the user named
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ds []Decision
	// invoking yaml.UnmarshalStrict so a misspelled key fails instead of passing as undecided
	if err := yaml.UnmarshalStrict(b, &ds); err != nil {
		// invoking plainYAMLError to say what's wrong without the library's wording, its defined below
		return nil, fmt.Errorf("%s isn't valid: %s. Fix it, or delete it to start over", filepath.Join(dir, DecisionsFile), plainYAMLError(err))
	}
	for _, d := range ds {
		if d.Feature == "" {
			return nil, fmt.Errorf("%s: an entry has no feature. Fix it, or delete the file to start over", filepath.Join(dir, DecisionsFile))
		}
	}
	for _, d := range ds {
		// checking each choice is one of the three, a typo must not pass as undecided
		if d.Choose != "" && !slices.Contains(choices, d.Choose) {
			opts := d.Options
			// checking if the entry lost its options, if so every choice is named
			if len(opts) == 0 {
				opts = choices
			}
			return nil, fmt.Errorf("%s: %s: choose must be %s, not %q", DecisionsFile, d.Feature, strings.Join(opts, ", "), d.Choose)
		}
	}
	return ds, nil
}

// Explainer gives back a function telling verify why an object of the translation names the source platform on purpose, the user's or the default choice for it
func Explainer(ds []Decision) func(*unstructured.Unstructured) string {
	byFeature := map[string]Decision{}
	for _, d := range ds {
		byFeature[d.Feature] = d
	}
	return func(o *unstructured.Unstructured) string {
		q := qualified(o)
		for _, f := range []string{"identity/" + q, "secret-store/" + q, "object/" + o.GetKind() + "/" + q, "cloud-resource/" + o.GetKind() + "/" + q, "storage/" + q} {
			// a decision waiting for the user to choose again, the source changed under it, explains nothing
			if d, ok := byFeature[f]; ok && d.choice() != "" && !d.Waiting() {
				how := "as chosen"
				if d.Choose == "" {
					how = "the default"
				}
				return fmt.Sprintf("decided in %s: %s is %s (%s)", DecisionsFile, f, d.choice(), how)
			}
		}
		return ""
	}
}

// TrustStep is RESTORE.md's step that trusts the new cluster for the identities kept, verify and diff point at it
const TrustStep = "Trust the new cluster for the identities kept"

// IdentityKept gives back a function telling verify and diff that a ServiceAccount keeps its cloud identity by the translation's decision, empty when it doesnt
// its Pods already carry the token and settings, what's left is the cloud trusting the new cluster, a step outside the cluster
func IdentityKept(ds []Decision) func(*unstructured.Unstructured) string {
	byFeature := map[string]Decision{}
	for _, d := range ds {
		byFeature[d.Feature] = d
	}
	return func(o *unstructured.Unstructured) string {
		// checking if its a ServiceAccount, only one carries an identity
		if o.GetAPIVersion() != "v1" || o.GetKind() != "ServiceAccount" {
			return ""
		}
		f := "identity/" + qualified(o)
		d, ok := byFeature[f]
		// checking if its decided keep-source, a decision waiting for the user to choose again explains nothing
		if !ok || d.choice() != KeepSource || d.Waiting() {
			return ""
		}
		return fmt.Sprintf("keeps its cloud identity (%s is %s in %s), and its Pods carry the token and settings to use it: it works once the cloud trusts this cluster, RESTORE.md's step %q", f, KeepSource, DecisionsFile, TrustStep)
	}
}

// plainYAMLError rewords the yaml library's errors, which speak of JSON since it converts YAML to JSON first
func plainYAMLError(err error) string {
	msg := err.Error()
	// checking if its a key the file shouldnt have, if so it names the key and the ones it may have
	if i := strings.Index(msg, `unknown field "`); i >= 0 {
		key := strings.SplitN(msg[i+len(`unknown field "`):], `"`, 2)[0]
		return fmt.Sprintf("it has an unknown key %q (an entry has feature, what, options, default, previous, choose and basis)", key)
	}
	for _, p := range []string{"error converting YAML to JSON: ", "error unmarshaling JSON: while decoding JSON: ", "yaml: "} {
		msg = strings.ReplaceAll(msg, p, "")
	}
	return "it isn't readable YAML (" + msg + ")"
}

// WriteDecisions writes the decisions file in dir, with a header saying how to use it
func WriteDecisions(dir string, ds []Decision) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	// written by hand so each entry reads in the order a person uses it, what it is, the options, then the choice
	var b []byte
	for _, d := range ds {
		entry := []struct {
			k string
			v any
		}{{"feature", d.Feature}, {"what", d.What}, {"options", d.Options}, {"default", d.Default}, {"previous", d.Previous}, {"choose", d.Choose}, {"basis", d.Basis}}
		for i, kv := range entry {
			if (kv.k == "default" && d.Default == "") || (kv.k == "previous" && d.Previous == "") || (kv.k == "basis" && d.Basis == "") {
				continue
			}
			vb, err := yaml.Marshal(map[string]any{kv.k: kv.v})
			if err != nil {
				return err
			}
			lines := strings.Split(strings.TrimRight(string(vb), "\n"), "\n")
			for j, l := range lines {
				prefix := "  "
				if i == 0 && j == 0 {
					prefix = "- "
				}
				b = append(b, []byte(prefix+l+"\n")...)
			}
		}
	}
	head := "# Written by setdrift translate: what has no 1:1 equivalent on the target.\n" +
		"# For each, set choose to one of its options (keep-source, equivalent, leave-out,\n" +
		"# or for NetworkPolicies require or accept-unenforced),\n" +
		"# then run setdrift translate again. An empty choose takes the default where there\n" +
		"# is one; with no default, translate stops until you choose. basis records what\n" +
		"# each was decided on: when the source changes under your choice, translate asks\n" +
		"# again, with your earlier choice in previous.\n"
	return os.WriteFile(filepath.Join(dir, DecisionsFile), append([]byte(head), b...), 0o600)
}

// Write writes the translation's overlay into the cluster's directory root, at r.Dir
// resources are the generic set's directories, as the cluster's kustomization.yaml lists them
// it refuses a directory that isnt a translation, like the export's own platform set
func Write(root string, r *Result, resources []string, version string, ri RestoreInfo) error {
	dir := filepath.Join(root, filepath.FromSlash(r.Dir))
	// checking if the directory exists without a stamp, if so translate didnt write it and leaves it as it is
	// unless it holds only the decisions file, which a translation that stopped for decisions wrote
	if entries, err := os.ReadDir(dir); err == nil {
		onlyDecisions := len(entries) == 1 && entries[0].Name() == DecisionsFile
		if _, err := os.Stat(filepath.Join(dir, StampFile)); err != nil && !onlyDecisions {
			return fmt.Errorf("%s exists and wasn't written by setdrift translate (it has no %s): move it aside and run translate again", dir, StampFile)
		}
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("translate writes into the export and couldn't: %w", err)
	}
	up := strings.Repeat("../", strings.Count(r.Dir, "/")+1)
	k := map[string]any{"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization"}
	var res []string
	for _, rsc := range resources {
		res = append(res, up+rsc)
	}
	// the equivalents chosen for cloud resources, each in its own file under equivalents/, written fresh each run
	eqDir := filepath.Join(dir, equivalentsDir)
	if err := os.RemoveAll(eqDir); err != nil {
		return err
	}
	for _, a := range r.Added {
		name := strings.ToLower(a.GetKind()) + "-" + a.GetName() + ".yaml"
		if a.GetNamespace() != "" {
			name = a.GetNamespace() + "-" + name
		}
		// checking if the name is a plain file name, an export read from disk isnt held to Kubernetes' naming rules, so a name like ../x must not write outside the overlay
		if name != filepath.Base(name) || strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
			return fmt.Errorf("%s %s/%s: its name isn't a valid Kubernetes name, so translate won't write a file for it", a.GetKind(), a.GetNamespace(), a.GetName())
		}
		b, err := yaml.Marshal(a.Object)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(eqDir, 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(eqDir, name), b, 0o600); err != nil {
			return err
		}
		res = append(res, equivalentsDir+"/"+name)
	}
	k["resources"] = res
	// the translation's id, a short hash of what the overlay applies, the same for the same translation and new for a changed one
	var added []any
	for _, a := range r.Added {
		added = append(added, a.Object)
	}
	r.ID = Fingerprint([]any{res, r.Patches, added})[:8]
	// every object the overlay applies, and its Pod template, carries the translation's code and id, so the target cluster's objects say which translation they came from
	k["commonAnnotations"] = map[string]string{capability.TranslationAnnotation: r.Code + "-" + r.ID}
	var patches []map[string]any
	for _, p := range r.Patches {
		// invoking patchEntry to write one patch as kustomize reads it, its defined below
		e, err := patchEntry(p)
		if err != nil {
			return err
		}
		patches = append(patches, e)
	}
	if len(patches) > 0 {
		k["patches"] = patches
	}
	kb, err := yaml.Marshal(k)
	if err != nil {
		return err
	}
	kHead := fmt.Sprintf("# Written by setdrift translate: this export's generic set, translated from %s for %s.\n# Apply it with kubectl apply --server-side -k %s; README.md says what changed.\n", r.From.Name(), r.To.Name(), r.Dir)
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), append([]byte(kHead), kb...), 0o600); err != nil {
		return err
	}
	sb, err := yaml.Marshal(Stamp{Code: r.Code, ID: r.ID, From: string(r.From), To: string(r.To), TranslatedAt: time.Now().UTC().Format(time.RFC3339), SetDrift: version})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, StampFile), sb, 0o600); err != nil {
		return err
	}
	// invoking readme to write what the translation changed and what to do, its defined below
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme(r)), 0o600); err != nil {
		return err
	}
	// invoking restore to write the cross-cloud RESTORE.md, its defined in restore.go
	if err := os.WriteFile(filepath.Join(dir, "RESTORE.md"), []byte(restore(r, ri)), 0o600); err != nil {
		return err
	}
	return WriteDecisions(dir, r.Decisions)
}

// patchEntry writes a Patch as a kustomize patches entry, a JSON patch on its target, or a strategic merge patch deleting it
func patchEntry(p Patch) (map[string]any, error) {
	target := map[string]any{"version": p.Version, "kind": p.Kind, "name": p.Name}
	if p.Group != "" {
		target["group"] = p.Group
	}
	if p.Namespace != "" {
		target["namespace"] = p.Namespace
	}
	if p.Delete {
		apiVersion := p.Version
		if p.Group != "" {
			apiVersion = p.Group + "/" + p.Version
		}
		md := map[string]any{"name": p.Name}
		if p.Namespace != "" {
			md["namespace"] = p.Namespace
		}
		b, err := yaml.Marshal(map[string]any{"$patch": "delete", "apiVersion": apiVersion, "kind": p.Kind, "metadata": md})
		if err != nil {
			return nil, err
		}
		return map[string]any{"patch": string(b)}, nil
	}
	b, err := yaml.Marshal(p.Ops)
	if err != nil {
		return nil, err
	}
	return map[string]any{"target": target, "patch": string(b)}, nil
}

// readme writes the overlay's README, what changed for each object, what to know, and the decisions
func readme(r *Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Translated for %s\n\n", r.To.Name())
	from := r.From.Name()
	if from == "" {
		from = "a cluster on no known cloud"
	}
	fmt.Fprintf(&b, "Written by `setdrift translate`: this export's generic set, from %s, with what %s reads differently rewritten. The generic set itself is unchanged. Apply this directory on %s with `kubectl apply --server-side -k .`, and check it with `setdrift verify`.\n\n", from, r.To.Name(), r.To.Name())
	if len(r.Patches) > 0 {
		b.WriteString("## What changed\n\n")
		for _, p := range r.Patches {
			fmt.Fprintf(&b, "- **%s %s**\n", p.Kind, qualifiedName(&p))
			for _, w := range p.Why {
				fmt.Fprintf(&b, "  - %s\n", w)
			}
		}
		b.WriteString("\n")
	}
	if len(r.Added) > 0 {
		fmt.Fprintf(&b, "## Equivalents written\n\nChosen with equivalent in %s, in %s/. Each is new and empty: the source's resource and its data stay where they are.\n\n", DecisionsFile, equivalentsDir)
		for _, a := range r.Added {
			fmt.Fprintf(&b, "- %s %s (%s)\n", a.GetKind(), qualified(a), a.GetAPIVersion())
		}
		b.WriteString("\n")
	}
	if len(r.Notes) > 0 {
		b.WriteString("## Before you apply\n\n")
		for _, n := range r.Notes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
		b.WriteString("\n")
	}
	if len(r.Decisions) > 0 {
		fmt.Fprintf(&b, "## Decisions\n\n%s has the features with no 1:1 equivalent on %s and the choice for each.\n\n", DecisionsFile, r.To.Name())
		for _, d := range r.Decisions {
			c := d.choice()
			if c == "" {
				c = "not decided yet"
			} else if d.Choose == "" {
				c += " (the default)"
			}
			fmt.Fprintf(&b, "- `%s`: %s\n", d.Feature, c)
		}
	}
	return b.String()
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
