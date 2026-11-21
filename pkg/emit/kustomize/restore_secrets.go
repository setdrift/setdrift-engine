// RESTORE.md's secrets step: the values to supply, how, and what a
// controller writes again.

package kustomize

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	yamlv3 "go.yaml.in/yaml/v3"

	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/redact"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// secrets: the values to supply, by file and line.
// secrets writes the step for the values to supply, by file and line.
func (d *restoreData) secrets(p *playbook) {
	var redacted, generated []writtenFile
	var tokens []string
	for _, w := range d.written {
		switch {
		case w.referenceOnly && NeedsValues(w.resource):
			redacted = append(redacted, w)
		case w.referenceOnly && len(Generated(w.resource)) > 0:
			generated = append(generated, w)
		}
		if w.resource.Kind == "Secret" && w.resource.ApiVersion == "v1" && str(d.manifests.of(w.resource), "type") == "kubernetes.io/service-account-token" {
			tokens = append(tokens, w.resource.Namespace+"/"+w.resource.Name)
		}
	}
	if len(redacted)+len(generated)+len(tokens) == 0 {
		return
	}
	p.section("Secrets")
	// A tool created some of them empty and the controller only fills them in: they must exist first.
	var fill, nothing []writtenFile
	for _, w := range generated {
		if hasFinding(w.resource, redact.SecretFilledByController) {
			fill = append(fill, w)
		} else {
			nothing = append(nothing, w)
		}
	}
	if len(fill) > 0 {
		createEmptySteps(p, fill)
	}
	if len(nothing) > 0 {
		nothingToDo(p, nothing)
	}
	if len(redacted) > 0 {
		d.redactedSteps(p, redacted)
	}
	if len(tokens) > 0 {
		p.printf("Service account tokens (%s) are created again with new values, and\n"+
			"tokens copied out of the source cluster stop working in the new one.\n",
			strings.Join(tokens, ", "))
	}
}

// createEmptySteps writes the commands that create, empty, the Secrets a
// tool creates and only a controller fills in, then restart their namespaces.
func createEmptySteps(p *playbook, fill []writtenFile) {
	p.printf("**Create these empty.** Only their controller wrote their values, and it\n" +
		"writes them again in the new cluster, but it does not create the Secret:\n" +
		"the installation did, empty. Until it exists the controller fails to\n" +
		"start. The files have no values; apply them, then restart the namespace:\n\n")
	var lines, nss []string
	for _, w := range fill {
		lines = append(lines, fmt.Sprintf(`kubectl --context "$DST" apply --server-side --field-manager=setdrift-restore -f %s   # filled in by %s`,
			text.ShellQuote(w.path), comment(controllersOf(w.resource))))
		if !slices.Contains(nss, w.resource.Namespace) {
			nss = append(nss, w.resource.Namespace)
		}
	}
	sort.Strings(nss)
	for _, ns := range nss {
		lines = append(lines, fmt.Sprintf(`kubectl --context "$DST" -n %s rollout restart deployment,statefulset`, text.ShellQuote(ns)))
	}
	p.code(lines...)
}

// nothingToDo lists the Secrets whose controller writes them again.
func nothingToDo(p *playbook, nothing []writtenFile) {
	p.printf("**Nothing to do** for these: only their controller wrote their values (a\n" +
		"generated password or key, a certificate it issues), and it creates and\n" +
		"writes them again in the new cluster. Restoring the old values would fight it.\n\n")
	for _, w := range nothing {
		p.printf("- %s %s/%s, by %s\n", w.resource.Kind, w.resource.Namespace, w.resource.Name, controllersOf(w.resource))
	}
	p.printf("\n")
}

// redactedSteps writes the ways to supply the redacted values, then the
// restarts that make workloads read them.
func (d *restoreData) redactedSteps(p *playbook, redacted []writtenFile) {
	p.printf("These files hold `%s` instead of secret values and were not applied with\n"+
		"the rest; Pods that use them wait until they exist. Supply the values one\n"+
		"of these ways. In a Secret marked *merge*, a controller writes the other\n"+
		"keys: only the keys listed are restored, and the controller's are left to\n"+
		"it.\n\n", "SETDRIFT_REDACTED")
	copyFromSource(p, redacted)
	fillInFiles(p, redacted)
	if len(d.remotes) > 0 {
		d.externalSecretsSteps(p)
	}
	restartSteps(p, redacted)
	// About the registry, wherever the cluster runs: ECR tokens expire.
	pullSecretNote(p, redacted)
	if n := countNonSecrets(redacted); n > 0 {
		p.printf("%s other than Secrets hold secret values (see the files above). Moving\n"+
			"those values into Secrets and referencing them makes the next export apply\n"+
			"without this step.\n\n", text.Plural(n, "object", "objects"))
	}
}

// copyFromSource writes way A: copying the values from the source cluster,
// cluster to cluster.
func copyFromSource(p *playbook, redacted []writtenFile) {
	p.printf("**A. Copy them from the source cluster**, while it still exists. The values\n" +
		"go straight from one cluster to the other and are never written to disk:\n\n")
	var lines []string
	for _, w := range redacted {
		r := w.resource
		if len(Generated(r)) > 0 {
			// Only the keys a person wrote, under their own field manager.
			var keys []string
			for _, red := range r.Redactions {
				if red.Reason == snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE {
					field, key, _ := strings.Cut(red.Path, ".")
					if field == "data" || field == "stringData" {
						k := jqString(key)
						keys = append(keys, k+": .data["+k+"]")
					}
				}
			}
			program := `{apiVersion: "v1", kind: "Secret", metadata: {name: .metadata.name, namespace: .metadata.namespace}, data: {` + strings.Join(keys, ", ") + `}}`
			lines = append(lines, fmt.Sprintf(`kubectl --context "$SRC" -n %s get secret %s -o json | jq %s | kubectl --context "$DST" apply --server-side --field-manager=setdrift-restore -f -   # merge`,
				text.ShellQuote(r.Namespace), text.ShellQuote(r.Name), text.ShellQuote(program)))
			continue
		}
		lines = append(lines, fmt.Sprintf(`kubectl --context "$SRC" -n %s get %s %s -o json | jq 'del(.metadata.uid, .metadata.resourceVersion, .metadata.creationTimestamp, .metadata.managedFields, .metadata.ownerReferences, .metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"], .status)' | kubectl --context "$DST" apply --server-side --field-manager=setdrift-restore -f -`,
			text.ShellQuote(r.Namespace), text.ShellQuote(strings.ToLower(KindDir(r.ApiVersion, r.Kind))), text.ShellQuote(r.Name)))
	}
	p.code(lines...)
}

// fillInFiles writes way B: the files and lines to fill in, and the applies.
func fillInFiles(p *playbook, redacted []writtenFile) {
	p.printf("**B. Fill in the files** from wherever the values are kept, then apply each\n" +
		"file. Values under `data:` are base64 (`printf %%s 'value' | base64`). Keys\n" +
		"a controller writes are not in the files.\n\n")
	for _, w := range redacted {
		r := w.resource
		merge := ""
		if len(Generated(r)) > 0 {
			merge = ", merge: " + controllersOf(r) + " writes the other keys"
		}
		p.printf("- `%s` (%s %s/%s%s)\n", w.path, r.Kind, r.Namespace, r.Name, merge)
		for _, red := range r.Redactions {
			if red.Reason != snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE {
				continue
			}
			if line := lineOf(w.yaml, red.Path); line > 0 {
				p.printf("  - line %d: `%s`\n", line, red.Path)
			} else {
				p.printf("  - `%s`\n", red.Path)
			}
		}
	}
	p.printf("\n")
	var apply []string
	for _, w := range redacted {
		apply = append(apply, fmt.Sprintf(`kubectl --context "$DST" apply --server-side --field-manager=setdrift-restore -f %s`, text.ShellQuote(w.path)))
	}
	p.code(apply...)
}

// externalSecretsSteps writes way C: External Secrets Operator, and, for a
// new cluster on AWS, the remote secrets to create there.
func (d *restoreData) externalSecretsSteps(p *playbook) {
	p.printf("**C. External Secrets Operator.** external-secrets/ has an ExternalSecret\n" +
		"for each Secret (see its README); those marked *merge* add their keys to\n" +
		"the Secret the controller creates (`creationPolicy: Merge`). Put the values\n" +
		"in your secret manager (the remote secrets the README lists, with their\n" +
		"keys; SetDrift never copies them for you), install the operator and its\n" +
		"store, then apply the directory:\n\n")
	p.code(`kubectl --context "$DST" apply -k external-secrets/`)
	// The AWS commands that create the remote secrets, for a new cluster on AWS.
	if q := d.awsp; q != nil && (d.secretService == serviceSecretsManager || d.secretService == serviceParameterStore) {
		d.awsRemoteSecrets(q)
	}
}

// awsRemoteSecrets writes the AWS playbook's step that creates the remote
// secrets in Secrets Manager or Parameter Store.
func (d *restoreData) awsRemoteSecrets(q *playbook) {
	q.section("Secrets in " + text.Choose(d.secretService == serviceParameterStore, "Parameter Store", "Secrets Manager"))
	q.printf("Before applying external-secrets/ (RESTORE.md's secrets step), create each\n" +
		"remote secret with its values, as JSON with one property per key. Fill in\n" +
		"each `<value>` yourself: SetDrift never copies secret values.\n\n")
	var lines []string
	for _, r := range d.remotes {
		props := make([]string, len(r.properties))
		for i, k := range r.properties {
			props[i] = fmt.Sprintf(`%q:"<value>"`, k)
		}
		value := text.ShellQuote("{" + strings.Join(props, ",") + "}")
		if d.secretService == serviceParameterStore {
			lines = append(lines, fmt.Sprintf(`aws ssm put-parameter --region "$REGION" --type SecureString --name %s --value %s   # replaces %s`,
				text.ShellQuote(r.key), value, comment(r.secret)))
		} else {
			lines = append(lines, fmt.Sprintf(`aws secretsmanager create-secret --region "$REGION" --name %s --secret-string %s   # replaces %s`,
				text.ShellQuote(r.key), value, comment(r.secret)))
		}
	}
	q.code(lines...)
}

// restartSteps writes the restarts of the namespaces whose values were
// restored.
func restartSteps(p *playbook, redacted []writtenFile) {
	var nss []string
	for _, w := range redacted {
		if !slices.Contains(nss, w.resource.Namespace) {
			nss = append(nss, w.resource.Namespace)
		}
	}
	sort.Strings(nss)
	p.printf("Workloads read these values when they start. Restart the workloads in\n" +
		"these namespaces so every component uses the restored values:\n\n")
	var restart []string
	for _, ns := range nss {
		restart = append(restart, fmt.Sprintf(`kubectl --context "$DST" -n %s rollout restart deployment,statefulset,daemonset`, text.ShellQuote(ns)))
	}
	p.code(restart...)
}

// lineOf returns the line in doc of the field at path, a redaction path
// (data.password, spec.template.spec.containers[web].env[TOKEN].value), or
// 0 if it is not found.
func lineOf(doc []byte, path string) int {
	var root yamlv3.Node
	if err := yamlv3.Unmarshal(doc, &root); err != nil || len(root.Content) == 0 {
		return 0
	}
	return find(root.Content[0], strings.TrimPrefix(path, "."))
}

// find resolves path from node. Map keys may contain dots (tls.crt,
// .dockerconfigjson), so each key is matched against the start of path
// rather than splitting it.
func find(node *yamlv3.Node, path string) int {
	switch node.Kind {
	case yamlv3.MappingNode:
		best, line := -1, 0
		for i := 0; i+1 < len(node.Content); i += 2 {
			k, v := node.Content[i], node.Content[i+1]
			switch {
			case path == k.Value:
				return k.Line
			case strings.HasPrefix(path, k.Value+"."), strings.HasPrefix(path, k.Value+"["):
				// Prefer the longest key: "a.b" over "a" for "a.b.c".
				if len(k.Value) > best {
					rest := strings.TrimPrefix(path[len(k.Value):], ".")
					if l := find(v, rest); l > 0 {
						best, line = len(k.Value), l
					}
				}
			}
		}
		return line
	case yamlv3.SequenceNode:
		if !strings.HasPrefix(path, "[") {
			return 0
		}
		end := strings.Index(path, "]")
		if end < 0 {
			return 0
		}
		label, rest := path[1:end], strings.TrimPrefix(path[end+1:], ".")
		for i, item := range node.Content {
			if itemName(item) == label || strconv.Itoa(i) == label {
				if rest == "" {
					return item.Line
				}
				return find(item, rest)
			}
		}
	}
	return 0
}

// itemName returns a list item's name field, or "".
func itemName(n *yamlv3.Node) string {
	if n.Kind != yamlv3.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == "name" {
			return n.Content[i+1].Value
		}
	}
	return ""
}

// controllersOf names the field managers that wrote r's generated values.
func controllersOf(r *snapshotv1alpha1.Resource) string {
	var names []string
	for _, red := range Generated(r) {
		for _, c := range strings.Split(red.Controller, ",") {
			if c != "" && !slices.Contains(names, c) {
				names = append(names, c)
			}
		}
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// pullSecret reports whether r is an image pull Secret, by its type: the
// placeholder keeps nothing of the value, so which registry it is for is not known.
func pullSecret(r *snapshotv1alpha1.Resource) bool {
	if r.Kind != "Secret" || r.ApiVersion != "v1" {
		return false
	}
	var m struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(r.ManifestJson), &m) != nil {
		return false
	}
	return m.Type == "kubernetes.io/dockerconfigjson" || m.Type == "kubernetes.io/dockercfg"
}

// pullSecretNote warns that a pull Secret's registry token may expire: an
// Amazon ECR token lasts 12 hours, so a copied or stored value stops working
// soon after the restore unless whatever refreshed it in the source cluster
// comes along.
func pullSecretNote(p *playbook, redacted []writtenFile) {
	var names []string
	for _, w := range redacted {
		if pullSecret(w.resource) {
			names = append(names, w.resource.Namespace+"/"+w.resource.Name)
		}
	}
	if len(names) == 0 {
		return
	}
	p.printf("**Registry tokens can expire.** %s %s registry credentials. If they are\n"+
		"for Amazon ECR, the token lasts 12 hours: a copied, filled-in or stored value\n"+
		"stops working within a day. Restore what refreshes it in the source cluster\n"+
		"(often a CronJob, or a controller that rewrites the Secret), or drop the\n"+
		"Secret and let nodes pull with their IAM role: on EKS, a node role with ECR\n"+
		"read access (AmazonEC2ContainerRegistryReadOnly) pulls from its own account,\n"+
		"and from other accounts whose repository policy allows it.\n\n",
		strings.Join(names, ", "), text.Choose(len(names) == 1, "holds", "hold"))
}

// jqString writes s as a jq (JSON) string literal.
func jqString(s string) string {
	b, _ := json.Marshal(s) // a string always marshals
	return string(b)
}

// countNonSecrets counts the files that are not Secrets.
func countNonSecrets(ws []writtenFile) int {
	n := 0
	for _, w := range ws {
		if !(w.resource.Kind == "Secret" && w.resource.ApiVersion == "v1") {
			n++
		}
	}
	return n
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
