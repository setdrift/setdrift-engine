package translate

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/capability"
	"setdrift.io/setdrift-engine/pkg/platform"
)

// RestoreInfo is what the translation's RESTORE.md needs from the export besides the translation itself
type RestoreInfo struct {
	// Cluster is the export's cluster name
	Cluster string
	// SourceVersion is the source cluster's Kubernetes version
	SourceVersion string
	// Volumes are the claims with a volume RESTORE.md pre-binds, namespace/claim, whose disks stay on the source cloud
	Volumes []string
	// Shape is what the source cluster was built as, for the target's create command
	Shape Shape
}

// restore writes the translation's RESTORE.md, the cross-cloud version of the export's, what comes over and what doesnt, then the steps in order
// steps that are the same on any cluster point to the export's own RESTORE.md
func restore(r *Result, ri RestoreInfo) string {
	var b strings.Builder
	to, from := r.To.Name(), r.From.Name()
	if from == "" {
		from = "the source cluster"
	}
	fmt.Fprintf(&b, "# Restoring %s on %s\n\n", ri.Cluster, to)
	fmt.Fprintf(&b, "The cross-cloud version of this export's RESTORE.md, for a %s cluster. Follow this one instead of that one. Steps that are the same on any cluster point to its sections by name. Commands run from the cluster's directory (where that RESTORE.md is), with `SRC` and `DST` set as it says.\n\n", to)

	b.WriteString("## What comes over, and what doesn't\n\n")
	fmt.Fprintf(&b, "Everything in the export comes over, translated for %s (README.md says what changed), except these, and why.\n\n", to)
	b.WriteString("| What | Why | What to do |\n|---|---|---|\n")
	for _, v := range ri.Volumes {
		fmt.Fprintf(&b, "| The data in claim %s | Its disk is %s, and a %s node can't attach it. The claim comes over and gets a new, empty disk | Copy the data with Velero (file system backup with Kopia) or the app's own tools, like a database's dump or replication |\n", v, diskOf(r.capMap(), r.From), to)
	}
	for _, p := range r.Patches {
		// checking if its a deleted object other than a cloud resource, those have rows of their own below
		if p.Delete && !slices.ContainsFunc(r.CloudResources, func(c CloudResource) bool { return c.Ref == p.Kind+" "+qualifiedName(&p) }) {
			why, todo := "Left out, as decided in "+DecisionsFile, "See the decision there"
			// checking if it was left out by a rule rather than a decision, like EKS's VPC CNI settings, if so its own reason is given
			if len(p.Why) > 0 && !strings.Contains(p.Why[0], "as decided") {
				why, todo = strings.TrimPrefix(p.Why[0], "left out: "), "Nothing"
			}
			fmt.Fprintf(&b, "| %s %s | %s | %s |\n", p.Kind, qualifiedName(&p), why, todo)
		}
	}
	for _, c := range r.CloudResources {
		// invoking cloudResourceRow for what happens to it and what to do, its defined below
		why, todo := cloudResourceRow(c, to)
		fmt.Fprintf(&b, "| %s (a real %s resource, through %s) | %s | %s |\n", c.Ref, c.Cloud, c.Operator, why, todo)
	}
	for _, l := range r.Lost {
		fmt.Fprintf(&b, "| **%s** | %s | %s |\n", l.What, l.Why, l.ToDo)
	}
	// checking if any image is in another cloud's private registry, if so its a row, with the step below
	if len(r.Images) > 0 {
		fmt.Fprintf(&b, "| %s in another cloud's registry | The new cluster can't pull from it without that cloud's credentials | Copy them to %s's registry, below |\n", text.Plural(len(r.Images), "image", "images"), to)
	}
	b.WriteString("| Secret values | SetDrift never copies them | Supply them, below |\n")
	b.WriteString("| Load balancer addresses | A new cluster gets new addresses | Point DNS and clients at the new ones once they're assigned |\n\n")

	step := 0
	next := func(title string) { step++; fmt.Fprintf(&b, "## %d. %s\n\n", step, title) }

	var create strings.Builder
	// invoking createStep for the target cluster's create command, its defined in infra.go
	if createStep(&create, r, ri) {
		next("Create the " + to + " cluster")
		b.WriteString(create.String())
	}

	next("Check the Kubernetes version")
	fmt.Fprintf(&b, "The source ran Kubernetes %s.\n\n```sh\nkubectl --context \"$DST\" version\n```\n\n", ri.SourceVersion)
	b.WriteString("If the target's version is older (clouds don't all offer the newest), the apply still goes ahead, and the API server refuses any object that uses a field or version it doesn't have yet. `setdrift verify` lists those.\n\n")

	if r.NetworkPolicyEngine != "" {
		next("Turn on network policy enforcement")
		fmt.Fprintf(&b, "The source enforced its %s with %s. On a %s cluster that doesn't enforce them, they apply and do nothing, so turn enforcement on before applying: %s.\n\n",
			text.Plural(r.NetworkPolicies, "NetworkPolicy", "NetworkPolicies"), r.NetworkPolicyEngine, to, r.capMap().For(r.To).Translate.NetworkPolicy.HowToEnforce)
		if NetworkPolicyChoice(r.Decisions) == AcceptUnenforced {
			fmt.Fprintf(&b, "**%s accepts them unenforced** (%s is accept-unenforced), so `setdrift verify` passes on a cluster that doesn't enforce them, and says so every run. Set it back to require once enforcement is on.\n\n", DecisionsFile, NetworkPolicyFeature)
		} else {
			b.WriteString("`setdrift verify` checks it on the new cluster and fails until enforcement is on.\n\n")
		}
	}

	next("Create the CRDs and install the controllers")
	b.WriteString("The \"CustomResourceDefinitions\" and \"Controllers to install first\" sections of RESTORE.md are the same on any cluster. Follow them now.\n\n")

	if len(r.Identities) > 0 {
		next(TrustStep)
		fmt.Fprintf(&b, "These ServiceAccounts keep their cloud identity, as decided in %s, and their Pods already carry the token and settings to use it (README.md). What's left is the cloud's side, trusting this %s cluster's OIDC issuer for each. First get the issuer.\n\n```sh\n%s\n```\n\n", DecisionsFile, to, r.capMap().For(r.To).Translate.OIDCIssuerCommand)
		for _, id := range r.Identities {
			// invoking trustSteps to write the commands for the identity's cloud, its defined below
			b.WriteString(trustSteps(r.capMap(), id))
		}
	}

	if len(r.Images) > 0 {
		next("Copy the images")
		// invoking imagesStep for the commands, its defined in images.go
		imagesStep(r.capMap(), &b, r.Images, r.To, r.Dir)
	}

	next("Apply the translation")
	fmt.Fprintf(&b, "Apply this directory, not the generic set.\n\n```sh\nkubectl --context \"$DST\" apply --server-side -k %s\n```\n\n", r.Dir)

	next("Supply the secret values")
	b.WriteString("The \"Secrets\" section of RESTORE.md, the same on any cluster.\n\n")

	if len(ri.Volumes) > 0 {
		next("Bring the data over")
		fmt.Fprintf(&b, "The claims above have new, empty disks on %s. Copy their data with Velero (a file system backup with Kopia on %s, restored on %s), or the app's own tools. Do it before the apps that use the data take traffic.\n\n", to, from, to)
	}

	next("Check the result")
	b.WriteString("```sh\nsetdrift verify --context \"$DST\" .\n```\n\n")
	fmt.Fprintf(&b, "Verify picks this translation for a %s cluster and counts what's decided in %s as explained.\n", to, DecisionsFile)
	return b.String()
}

// azureGUID matches an Azure client ID, a GUID
var azureGUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// cloudResourceRow says what happens to a cloud-resource object in the translation, and what to do about it
func cloudResourceRow(c CloudResource, to string) (why, todo string) {
	switch c.Choice {
	case "":
		return fmt.Sprintf("Its operator runs on %s, so it isn't translated", to), "Move it with the \"Cloud resources\" section of RESTORE.md, one controller at a time"
	case KeepSource:
		return "Stays with the source cluster, whose operator keeps managing it, as decided in " + DecisionsFile,
			"Keep the source cluster's object. Before you ever delete that cluster or the object, set " + c.Retain + " on it, or the resource is deleted with it"
	case Equivalent:
		return fmt.Sprintf("Written as %s's own (%s), a new, empty resource, as decided in %s. The source's stays where it is", to, strings.Join(c.Equivalent, ", "), DecisionsFile),
			"Fill in what README.md lists before applying, and install the target's operator. Copy the data across with the cloud's tools"
	}
	return "Left out, as decided in " + DecisionsFile, "Nothing on the new cluster manages it. Before you ever delete the source cluster or the object, set " + c.Retain + " on it if the resource must stay"
}

// diskOf names the disks of a platform's block storage, for the volumes that stay behind, generic for a source the map doesnt know, like a local cluster
func diskOf(m *capability.Map, d platform.Distribution) string {
	if p := m.For(d); p != nil {
		return p.Translate.DiskName
	}
	return "on the source cluster's storage"
}

// trustSteps writes the commands that make an identity's cloud trust the new cluster for one ServiceAccount, by the cloud's trust strategy
func trustSteps(m *capability.Map, id Identity) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s/%s**", id.Namespace, id.ServiceAccount)
	if c := m.Cloud(id.Cloud); c != nil {
		if write := trusts[c.Identity.Trust]; write != nil {
			write(&b, id, "system:serviceaccount:"+id.Namespace+":"+id.ServiceAccount)
		}
	}
	return b.String()
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
