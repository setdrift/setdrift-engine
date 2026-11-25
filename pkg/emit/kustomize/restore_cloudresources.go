// the cloud resources step of RESTORE.md, for objects of operators that create real cloud resources, like an ACK Bucket
// export holds them back in cloud-resources/, which no kustomization lists, since applied to a second cluster its controller would manage the same resource as the source's
// two clusters fighting over one bucket, and deleting the object from either could delete the bucket itself, so the step moves each one on purpose, one controller at a time

package kustomize

import (
	"fmt"
	"strings"

	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/capability"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// cloudResourcesDir is where export holds back cloud-resource operator objects, no kustomization lists it
const cloudResourcesDir = "cloud-resources"

// cloudResourcesReadme is cloud-resources/README.md
const cloudResourcesReadme = `# Cloud resources

These objects belong to operators that create and manage real cloud
resources, like an S3 bucket for an ACK Bucket. They aren't in any
kustomization.yaml, so applying the export doesn't apply them.

Applied as they are to a second cluster, its operator would manage the same
resource as the source cluster's: two clusters fighting over one resource,
and deleting the object from either could delete the resource itself.

RESTORE.md's "Cloud resources" step moves each one to the new cluster on
purpose, one controller at a time. For a cluster on another cloud,
setdrift translate asks what to do with each of them.
`

// heldResource is one cloud-resource operator object export held back, with its operator
type heldResource struct {
	file writtenFile
	op   *capability.CloudResourceOperator
}

// cloudResources writes the step that moves each held-back object, nothing when there are none
func (d *restoreData) cloudResources(p *playbook) {
	if len(d.held) == 0 {
		return
	}
	p.section("Cloud resources")
	p.printf("%s/ holds %s of operators that create and manage real cloud resources. "+
		"They aren't applied with the rest: applied as is here, the new cluster's operator would manage the same resource as the source's, "+
		"and deleting the object from either cluster could delete the resource itself.\n\n",
		cloudResourcesDir, text.Plural(len(d.held), "object", "objects"))
	p.printf("| Object | Operator | File |\n|---|---|---|\n")
	for _, h := range d.held {
		r := h.file.resource
		p.printf("| %s %s | %s | `%s` |\n", cell(r.Kind), cell(qualifiedRef(r)), cell(h.op.Name), cell(h.file.path))
	}
	p.printf("\n**To leave a resource with the source cluster**, do nothing: the source's operator keeps managing it. " +
		"**For a cluster on another cloud**, `setdrift translate` asks what to do with each one.\n\n")
	p.printf("**To move one to this cluster, on the same cloud**, install its operator here first, then hand the resource over one controller at a time.\n\n")
	for _, h := range d.held {
		r := h.file.resource
		res := kubectlRef(r)
		ns := ""
		if r.Namespace != "" {
			ns = " -n " + text.ShellQuote(r.Namespace)
		}
		p.printf("%s %s, with %s:\n\n", r.Kind, qualifiedRef(r), h.op.Name)
		p.printf("1. On the source cluster, set %s, so deleting its object leaves the resource in place.\n", h.op.Retain)
		p.printf("2. Delete its object from the source cluster. The resource stays, and no cluster manages it until you apply it here.\n")
		if h.op.Adopt != nil {
			p.printf("3. Apply it here with %s: %s.\n\n", h.op.Adopt, h.op.AdoptNote)
		} else {
			p.printf("3. Apply it here: %s.\n\n", h.op.AdoptNote)
		}
		lines := []string{retainCommand(h.op.Retain, ns, res),
			fmt.Sprintf(`kubectl --context "$SRC"%s delete %s`, ns, res)}
		if h.op.Adopt != nil && h.op.Adopt.Annotation != "" {
			lines = append(lines, fmt.Sprintf(`kubectl annotate --local -f %s %s -o yaml | kubectl --context "$DST" apply --server-side -f -`,
				text.ShellQuote(h.file.path), text.ShellQuote(h.op.Adopt.Annotation+"="+h.op.Adopt.Value)))
		} else {
			lines = append(lines, fmt.Sprintf(`kubectl --context "$DST" apply --server-side -f %s`, text.ShellQuote(h.file.path)))
		}
		p.code(lines...)
	}
}

// retainCommand is the command that sets an operator's retain setting on the source's object
func retainCommand(s capability.Setting, ns, res string) string {
	if s.Annotation != "" {
		return fmt.Sprintf(`kubectl --context "$SRC"%s annotate %s %s --overwrite`, ns, res, text.ShellQuote(s.Annotation+"="+s.Value))
	}
	// a spec field, as a merge patch built from its dotted path
	parts := strings.Split(s.Field, ".")
	patch := fmt.Sprintf("%q", s.Value)
	for i := len(parts) - 1; i >= 0; i-- {
		patch = fmt.Sprintf("{%q:%s}", parts[i], patch)
	}
	return fmt.Sprintf(`kubectl --context "$SRC"%s patch %s --type merge -p %s`, ns, res, text.ShellQuote(patch))
}

// kubectlRef names an object for kubectl by its kind and group, like bucket.s3.services.k8s.aws/acme-orders
func kubectlRef(r *snapshotv1alpha1.Resource) string {
	group, _, _ := strings.Cut(r.ApiVersion, "/")
	if !strings.Contains(r.ApiVersion, "/") {
		group = ""
	}
	kind := strings.ToLower(r.Kind)
	if group != "" {
		kind += "." + group
	}
	return text.ShellQuote(kind + "/" + r.Name)
}

// qualifiedRef is namespace/name, or the name alone for a cluster-scoped object
func qualifiedRef(r *snapshotv1alpha1.Resource) string {
	if r.Namespace == "" {
		return r.Name
	}
	return r.Namespace + "/" + r.Name
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
