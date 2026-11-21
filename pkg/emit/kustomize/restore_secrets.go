// writes RESTORE.md's secrets step, the values to supply, how to supply them, and what a controller writes again on its own

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

// secrets writes the step for the secret values to supply, by file and line, and the ones a controller writes again
func (d *restoreData) secrets(p *playbook) {
	var redacted, generated []writtenFile
	var tokens []string
	// sorting the written files into redacted ones, controller-generated ones and service account tokens
	for _, w := range d.written {
		// checking if a reference-only file needs values from the user, or only holds what a controller generated
		// NeedsValues and Generated are defined in kustomize.go
		switch {
		case w.referenceOnly && NeedsValues(w.resource):
			redacted = append(redacted, w)
		case w.referenceOnly && len(Generated(w.resource)) > 0:
			generated = append(generated, w)
		}
		// checking if its a service account token Secret, str and of are defined in manifest.go
		if w.resource.Kind == "Secret" && w.resource.ApiVersion == "v1" && str(d.manifests.of(w.resource), "type") == "kubernetes.io/service-account-token" {
			tokens = append(tokens, w.resource.Namespace+"/"+w.resource.Name)
		}
	}
	// checking if there are none of the three, if so theres no step
	if len(redacted)+len(generated)+len(tokens) == 0 {
		return
	}
	p.section("Secrets")
	// a tool created some of them empty and the controller only fills them in, so they must exist first
	var fill, nothing []writtenFile
	// splitting the generated ones into those to create empty and those that need nothing
	for _, w := range generated {
		// invoking hasFinding and checking if the controller only fills the Secret in, its defined in restore.go, redact.SecretFilledByController in pkg/redact/redact.go
		if hasFinding(w.resource, redact.SecretFilledByController) {
			fill = append(fill, w)
		} else {
			nothing = append(nothing, w)
		}
	}
	// checking if some must be created empty, if so createEmptySteps writes how, its defined below
	if len(fill) > 0 {
		createEmptySteps(p, fill)
	}
	// checking if some need nothing, if so nothingToDo lists them, its defined below
	if len(nothing) > 0 {
		nothingToDo(p, nothing)
	}
	// checking if some values must be supplied, if so redactedSteps writes the ways to do it, its defined below
	if len(redacted) > 0 {
		d.redactedSteps(p, redacted)
	}
	// checking if there are service account tokens, if so the step says they get new values
	if len(tokens) > 0 {
		p.printf("Service account tokens (%s) are created again with new values, and\n"+
			"tokens copied out of the source cluster stop working in the new one.\n",
			strings.Join(tokens, ", "))
	}
}

// createEmptySteps writes the commands that create, empty, the Secrets a tool creates and only a controller fills in, then restart their namespaces
func createEmptySteps(p *playbook, fill []writtenFile) {
	p.printf("**Create these empty.** Only their controller wrote their values, and it\n" +
		"writes them again in the new cluster, but it does not create the Secret:\n" +
		"the installation did, empty. Until it exists the controller fails to\n" +
		"start. The files have no values; apply them, then restart the namespace:\n\n")
	var lines, nss []string
	// building an apply command per file, controllersOf is defined below, comment in manifest.go
	for _, w := range fill {
		lines = append(lines, fmt.Sprintf(`kubectl --context "$DST" apply --server-side --field-manager=setdrift-restore -f %s   # filled in by %s`,
			text.ShellQuote(w.path), comment(controllersOf(w.resource))))
		// checking if the namespace isnt listed yet
		if !slices.Contains(nss, w.resource.Namespace) {
			nss = append(nss, w.resource.Namespace)
		}
	}
	sort.Strings(nss)
	// building a restart command per namespace, text.ShellQuote is defined in internal/text/text.go
	for _, ns := range nss {
		lines = append(lines, fmt.Sprintf(`kubectl --context "$DST" -n %s rollout restart deployment,statefulset`, text.ShellQuote(ns)))
	}
	p.code(lines...)
}

// nothingToDo lists the Secrets whose controller writes them again, so restoring them would only fight it
func nothingToDo(p *playbook, nothing []writtenFile) {
	p.printf("**Nothing to do** for these: only their controller wrote their values (a\n" +
		"generated password or key, a certificate it issues), and it creates and\n" +
		"writes them again in the new cluster. Restoring the old values would fight it.\n\n")
	// listing each one with the controller that writes it
	for _, w := range nothing {
		p.printf("- %s %s/%s, by %s\n", w.resource.Kind, w.resource.Namespace, w.resource.Name, controllersOf(w.resource))
	}
	p.printf("\n")
}

// redactedSteps writes the ways to supply the redacted values, then the restarts that make workloads read them
func (d *restoreData) redactedSteps(p *playbook, redacted []writtenFile) {
	p.printf("These files hold `%s` instead of secret values and were not applied with\n"+
		"the rest; Pods that use them wait until they exist. Supply the values one\n"+
		"of these ways. In a Secret marked *merge*, a controller writes the other\n"+
		"keys: only the keys listed are restored, and the controller's are left to\n"+
		"it.\n\n", "SETDRIFT_REDACTED")
	// invoking copyFromSource and fillInFiles for ways A and B, theyre defined below
	copyFromSource(p, redacted)
	fillInFiles(p, redacted)
	// checking if secrets went to an external store, if so externalSecretsSteps writes way C, its defined below
	if len(d.remotes) > 0 {
		d.externalSecretsSteps(p)
	}
	// invoking restartSteps so workloads read the restored values, its defined below
	restartSteps(p, redacted)
	// invoking pullSecretNote about the registry, wherever the cluster runs, since ECR tokens expire, its defined below
	pullSecretNote(p, redacted)
	// invoking countNonSecrets and checking if objects other than Secrets hold secret values, if so the step suggests moving them into Secrets, its defined below
	if n := countNonSecrets(redacted); n > 0 {
		p.printf("%s other than Secrets hold secret values (see the files above). Moving\n"+
			"those values into Secrets and referencing them makes the next export apply\n"+
			"without this step.\n\n", text.Plural(n, "object", "objects"))
	}
}

// copyFromSource writes way A, copying the values from the source cluster straight to the new one, never to disk
func copyFromSource(p *playbook, redacted []writtenFile) {
	p.printf("**A. Copy them from the source cluster**, while it still exists. The values\n" +
		"go straight from one cluster to the other and are never written to disk:\n\n")
	var lines []string
	// building a copy command per file
	for _, w := range redacted {
		r := w.resource
		// checking if a controller writes some of its keys, if so only the keys a person wrote are copied, under their own field manager
		if len(Generated(r)) > 0 {
			var keys []string
			// collecting the keys that were redacted as secret values
			for _, red := range r.Redactions {
				// checking if the redaction is a secret value
				if red.Reason == snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE {
					field, key, _ := strings.Cut(red.Path, ".")
					// checking if its a data or stringData key, the live object holds both under data, jqString is defined below
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
		// all of the object is copied, without the fields the source cluster set, KindDir is defined in kustomize.go
		lines = append(lines, fmt.Sprintf(`kubectl --context "$SRC" -n %s get %s %s -o json | jq 'del(.metadata.uid, .metadata.resourceVersion, .metadata.creationTimestamp, .metadata.managedFields, .metadata.ownerReferences, .metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"], .status)' | kubectl --context "$DST" apply --server-side --field-manager=setdrift-restore -f -`,
			text.ShellQuote(r.Namespace), text.ShellQuote(strings.ToLower(KindDir(r.ApiVersion, r.Kind))), text.ShellQuote(r.Name)))
	}
	p.code(lines...)
}

// fillInFiles writes way B, the files and lines to fill in, and the applies
func fillInFiles(p *playbook, redacted []writtenFile) {
	p.printf("**B. Fill in the files** from wherever the values are kept, then apply each\n" +
		"file. Values under `data:` are base64 (`printf %%s 'value' | base64`). Keys\n" +
		"a controller writes are not in the files.\n\n")
	// listing each file with the lines to fill in
	for _, w := range redacted {
		r := w.resource
		merge := ""
		// checking if a controller writes the other keys, if so the file is marked merge
		if len(Generated(r)) > 0 {
			merge = ", merge: " + controllersOf(r) + " writes the other keys"
		}
		p.printf("- `%s` (%s %s/%s%s)\n", w.path, r.Kind, r.Namespace, r.Name, merge)
		// going through the redactions for the secret values
		for _, red := range r.Redactions {
			// checking if the redaction isnt a secret value, if so theres nothing to fill in
			if red.Reason != snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE {
				continue
			}
			// invoking lineOf and checking if the field's line was found, if not only its path is listed, its defined below
			if line := lineOf(w.yaml, red.Path); line > 0 {
				p.printf("  - line %d: `%s`\n", line, red.Path)
			} else {
				p.printf("  - `%s`\n", red.Path)
			}
		}
	}
	p.printf("\n")
	var apply []string
	// building an apply command per file
	for _, w := range redacted {
		apply = append(apply, fmt.Sprintf(`kubectl --context "$DST" apply --server-side --field-manager=setdrift-restore -f %s`, text.ShellQuote(w.path)))
	}
	p.code(apply...)
}

// externalSecretsSteps writes way C, External Secrets Operator, and for a new cluster on aws the remote secrets to create there
func (d *restoreData) externalSecretsSteps(p *playbook) {
	p.printf("**C. External Secrets Operator.** external-secrets/ has an ExternalSecret\n" +
		"for each Secret (see its README); those marked *merge* add their keys to\n" +
		"the Secret the controller creates (`creationPolicy: Merge`). Put the values\n" +
		"in your secret manager (the remote secrets the README lists, with their\n" +
		"keys; SetDrift never copies them for you), install the operator and its\n" +
		"store, then apply the directory:\n\n")
	p.code(`kubectl --context "$DST" apply -k external-secrets/`)
	// checking if theres an aws playbook and the store is an aws service, if so awsRemoteSecrets writes the commands that create the remote secrets, its defined below
	if q := d.awsp; q != nil && (d.secretService == serviceSecretsManager || d.secretService == serviceParameterStore) {
		d.awsRemoteSecrets(q)
	}
}

// awsRemoteSecrets writes the aws playbook's step that creates the remote secrets in Secrets Manager or Parameter Store, with placeholders for the values
func (d *restoreData) awsRemoteSecrets(q *playbook) {
	// text.Choose is defined in internal/text/text.go
	q.section("Secrets in " + text.Choose(d.secretService == serviceParameterStore, "Parameter Store", "Secrets Manager"))
	q.printf("Before applying external-secrets/ (RESTORE.md's secrets step), create each\n" +
		"remote secret with its values, as JSON with one property per key. Fill in\n" +
		"each `<value>` yourself: SetDrift never copies secret values.\n\n")
	var lines []string
	// building a create command per remote secret
	for _, r := range d.remotes {
		props := make([]string, len(r.properties))
		// a placeholder for each property
		for i, k := range r.properties {
			props[i] = fmt.Sprintf(`%q:"<value>"`, k)
		}
		value := text.ShellQuote("{" + strings.Join(props, ",") + "}")
		// checking which service holds it, Parameter Store or Secrets Manager
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

// restartSteps writes the restarts of the namespaces whose values were restored, since workloads read them when they start
func restartSteps(p *playbook, redacted []writtenFile) {
	var nss []string
	// collecting each namespace once
	for _, w := range redacted {
		// checking if the namespace isnt listed yet
		if !slices.Contains(nss, w.resource.Namespace) {
			nss = append(nss, w.resource.Namespace)
		}
	}
	sort.Strings(nss)
	p.printf("Workloads read these values when they start. Restart the workloads in\n" +
		"these namespaces so every component uses the restored values:\n\n")
	var restart []string
	// building a restart command per namespace
	for _, ns := range nss {
		restart = append(restart, fmt.Sprintf(`kubectl --context "$DST" -n %s rollout restart deployment,statefulset,daemonset`, text.ShellQuote(ns)))
	}
	p.code(restart...)
}

// lineOf gives back the line in doc of the field at path, a redaction path like data.password or spec.template.spec.containers[web].env[TOKEN].value, or 0 if its not found
func lineOf(doc []byte, path string) int {
	var root yamlv3.Node
	// checking if the yaml doesnt parse or is empty, if so theres no line to give
	if err := yamlv3.Unmarshal(doc, &root); err != nil || len(root.Content) == 0 {
		return 0
	}
	// invoking find from the document's top node, its defined below
	return find(root.Content[0], strings.TrimPrefix(path, "."))
}

// find resolves path from node and gives back the line it ends on, or 0
// map keys may contain dots (tls.crt, .dockerconfigjson), so each key is matched against the start of path rather than splitting it
func find(node *yamlv3.Node, path string) int {
	// checking if the node is a map or a list
	switch node.Kind {
	case yamlv3.MappingNode:
		best, line := -1, 0
		// going through the map's key and value pairs
		for i := 0; i+1 < len(node.Content); i += 2 {
			k, v := node.Content[i], node.Content[i+1]
			// checking if the key is all of the path, or the start of it
			switch {
			case path == k.Value:
				return k.Line
			case strings.HasPrefix(path, k.Value+"."), strings.HasPrefix(path, k.Value+"["):
				// checking if this key is longer than the best so far, the longest key wins, "a.b" over "a" for "a.b.c"
				if len(k.Value) > best {
					rest := strings.TrimPrefix(path[len(k.Value):], ".")
					// invoking find on the value for the rest of the path and checking if it was found there
					if l := find(v, rest); l > 0 {
						best, line = len(k.Value), l
					}
				}
			}
		}
		return line
	case yamlv3.SequenceNode:
		// checking if the path doesnt start with a list label, if so it cant go on into a list
		if !strings.HasPrefix(path, "[") {
			return 0
		}
		end := strings.Index(path, "]")
		// checking if the label isnt closed
		if end < 0 {
			return 0
		}
		label, rest := path[1:end], strings.TrimPrefix(path[end+1:], ".")
		// looking for the item the label names, by its name field or its index
		for i, item := range node.Content {
			// invoking itemName and checking if the item matches the label, its defined below
			if itemName(item) == label || strconv.Itoa(i) == label {
				// checking if the path ends at the item, if so its line is the answer
				if rest == "" {
					return item.Line
				}
				return find(item, rest)
			}
		}
	}
	return 0
}

// itemName gives back a list item's name field, or "", the label redaction paths use for list items
func itemName(n *yamlv3.Node) string {
	// checking if the item isnt a map, if so it has no name field
	if n.Kind != yamlv3.MappingNode {
		return ""
	}
	// looking through the keys for name
	for i := 0; i+1 < len(n.Content); i += 2 {
		// checking if this key is name, if so its value is the item's name
		if n.Content[i].Value == "name" {
			return n.Content[i+1].Value
		}
	}
	return ""
}

// controllersOf names the field managers that wrote r's generated values, sorted and each once
func controllersOf(r *snapshotv1alpha1.Resource) string {
	var names []string
	// going through each generated value, Generated is defined in kustomize.go
	for _, red := range Generated(r) {
		// a redaction can name several controllers, comma separated
		for _, c := range strings.Split(red.Controller, ",") {
			// checking if the name isnt empty or listed yet
			if c != "" && !slices.Contains(names, c) {
				names = append(names, c)
			}
		}
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// pullSecret tells if r is an image pull Secret, by its type
// the placeholder keeps nothing of the value, so which registry its for isnt known
func pullSecret(r *snapshotv1alpha1.Resource) bool {
	// checking if its not a core Secret
	if r.Kind != "Secret" || r.ApiVersion != "v1" {
		return false
	}
	var m struct {
		Type string `json:"type"`
	}
	// checking if the manifest doesnt parse, if so its not counted as a pull Secret
	if json.Unmarshal([]byte(r.ManifestJson), &m) != nil {
		return false
	}
	return m.Type == "kubernetes.io/dockerconfigjson" || m.Type == "kubernetes.io/dockercfg"
}

// pullSecretNote warns that a pull Secret's registry token may expire
// an Amazon ECR token lasts 12 hours, so a copied or stored value stops working soon after the restore unless whatever refreshed it in the source cluster comes along
func pullSecretNote(p *playbook, redacted []writtenFile) {
	var names []string
	// collecting the pull Secrets, pullSecret is defined above
	for _, w := range redacted {
		// checking if its an image pull Secret
		if pullSecret(w.resource) {
			names = append(names, w.resource.Namespace+"/"+w.resource.Name)
		}
	}
	// checking if there are none, if so theres no note
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

// jqString writes s as a jq (json) string literal, so any key is safe in the jq program
func jqString(s string) string {
	b, _ := json.Marshal(s) // a string always marshals
	return string(b)
}

// countNonSecrets counts the files that arent core Secrets
func countNonSecrets(ws []writtenFile) int {
	n := 0
	// going through the files
	for _, w := range ws {
		// checking if its anything but a v1 Secret
		if !(w.resource.Kind == "Secret" && w.resource.ApiVersion == "v1") {
			n++
		}
	}
	return n
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
