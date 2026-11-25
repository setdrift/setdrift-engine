package translate

import (
	"fmt"
	"slices"
	"strings"

	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/netpolicy"
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
		fmt.Fprintf(&b, "| The data in claim %s | Its disk is %s, and a %s node can't attach it. The claim comes over and gets a new, empty disk | Copy the data with Velero (file system backup with Kopia) or the app's own tools, like a database's dump or replication |\n", v, diskOf(r.From), to)
	}
	for _, p := range r.Patches {
		// checking if its a deleted object other than a cloud resource, those have rows of their own below
		if p.Delete && !slices.ContainsFunc(r.CloudResources, func(c CloudResource) bool { return c.Ref == p.Kind+" "+qualifiedName(&p) }) {
			fmt.Fprintf(&b, "| %s %s | Left out, as decided in %s | See the decision there |\n", p.Kind, qualifiedName(&p), DecisionsFile)
		}
	}
	for _, c := range r.CloudResources {
		// invoking cloudResourceRow for what happens to it and what to do, its defined below
		why, todo := cloudResourceRow(c, to)
		fmt.Fprintf(&b, "| %s (a real %s resource, through %s) | %s | %s |\n", c.Ref, c.Cloud, c.Operator, why, todo)
	}
	b.WriteString("| Secret values | SetDrift never copies them | Supply them, below |\n")
	b.WriteString("| Load balancer addresses | A new cluster gets new addresses | Point DNS and clients at the new ones once they're assigned |\n\n")

	step := 0
	next := func(title string) { step++; fmt.Fprintf(&b, "## %d. %s\n\n", step, title) }

	next("Check the Kubernetes version")
	fmt.Fprintf(&b, "The source ran Kubernetes %s.\n\n```sh\nkubectl --context \"$DST\" version\n```\n\n", ri.SourceVersion)
	b.WriteString("If the target's version is older (clouds don't all offer the newest), the apply still goes ahead, and the API server refuses any object that uses a field or version it doesn't have yet. `setdrift verify` lists those.\n\n")

	if r.NetworkPolicyEngine != "" {
		next("Turn on network policy enforcement")
		fmt.Fprintf(&b, "The source enforced its %s with %s. On a %s cluster that doesn't enforce them, they apply and do nothing, so turn enforcement on before applying: %s.\n\n",
			text.Plural(r.NetworkPolicies, "NetworkPolicy", "NetworkPolicies"), r.NetworkPolicyEngine, to, netpolicy.HowToEnforce(string(r.To)))
		if NetworkPolicyChoice(r.Decisions) == AcceptUnenforced {
			fmt.Fprintf(&b, "**%s accepts them unenforced** (%s is accept-unenforced), so `setdrift verify` passes on a cluster that doesn't enforce them, and says so every run. Set it back to require once enforcement is on.\n\n", DecisionsFile, NetworkPolicyFeature)
		} else {
			b.WriteString("`setdrift verify` checks it on the new cluster and fails until enforcement is on.\n\n")
		}
	}

	next("Create the CRDs and install the controllers")
	b.WriteString("The \"CustomResourceDefinitions\" and \"Controllers to install first\" sections of RESTORE.md are the same on any cluster. Follow them now.\n\n")

	if len(r.Identities) > 0 {
		next("Trust the new cluster for the identities kept")
		fmt.Fprintf(&b, "These ServiceAccounts keep their cloud identity, as decided in %s, and their Pods already carry the token and settings to use it (README.md). What's left is the cloud's side, trusting this %s cluster's OIDC issuer for each. First get the issuer.\n\n```sh\n%s\n```\n\n", DecisionsFile, to, issuerCommand(r.To))
		for _, id := range r.Identities {
			// invoking trustSteps to write the commands for the identity's cloud, its defined below
			b.WriteString(trustSteps(id))
		}
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

// diskOf names the disks of a platform's block storage, for the volumes that stay behind
func diskOf(d platform.Distribution) string {
	switch d {
	case platform.EKS:
		return "an AWS EBS volume"
	case platform.AKS:
		return "an Azure managed disk"
	case platform.GKE:
		return "a Google Cloud persistent disk"
	}
	return "on the source cluster's storage"
}

// issuerCommand gives back the command that prints the new cluster's OIDC issuer on the target platform
func issuerCommand(d platform.Distribution) string {
	switch d {
	case platform.GKE:
		return "ISSUER=$(gcloud container clusters describe <new cluster> --location <location> --format='value(selfLink)')"
	case platform.AKS:
		return "ISSUER=$(az aks show --name <new cluster> --resource-group <resource group> --query oidcIssuerProfile.issuerUrl -o tsv)   # needs --enable-oidc-issuer on the cluster"
	case platform.EKS:
		return "ISSUER=$(aws eks describe-cluster --name <new cluster> --query cluster.identity.oidc.issuer --output text)"
	}
	return "ISSUER=<the cluster's OIDC issuer URL>"
}

// trustSteps writes the commands that make an identity's cloud trust the new cluster for one ServiceAccount
func trustSteps(id Identity) string {
	subject := "system:serviceaccount:" + id.Namespace + ":" + id.ServiceAccount
	var b strings.Builder
	fmt.Fprintf(&b, "**%s/%s**", id.Namespace, id.ServiceAccount)
	switch id.Cloud {
	case "aws":
		account := ""
		if parts := strings.Split(id.ID, ":"); len(parts) >= 5 {
			account = parts[4]
		}
		role := id.ID[strings.LastIndex(id.ID, "/")+1:]
		fmt.Fprintf(&b, " keeps AWS IAM role `%s`. Register the issuer with AWS IAM once, then add a statement to the role's trust policy (`aws iam get-role --role-name %s` shows it; keep its other statements).\n\n", id.ID, role)
		fmt.Fprintf(&b, "```sh\naws iam create-open-id-connect-provider --url \"$ISSUER\" --client-id-list sts.amazonaws.com\n```\n\n")
		fmt.Fprintf(&b, "```json\n{\n  \"Effect\": \"Allow\",\n  \"Principal\": {\"Federated\": \"arn:aws:iam::%s:oidc-provider/<ISSUER without https://>\"},\n  \"Action\": \"sts:AssumeRoleWithWebIdentity\",\n  \"Condition\": {\"StringEquals\": {\n    \"<ISSUER without https://>:sub\": \"%s\",\n    \"<ISSUER without https://>:aud\": \"sts.amazonaws.com\"\n  }}\n}\n```\n\n", account, subject)
	case "azure":
		fmt.Fprintf(&b, " keeps Azure managed identity `%s`. Add a federated credential for the new cluster (`az identity list --query \"[?clientId=='%s']\"` finds the identity's name and resource group).\n\n", id.ID, id.ID)
		fmt.Fprintf(&b, "```sh\naz identity federated-credential create --name %s-%s --identity-name <identity name> --resource-group <resource group> \\\n  --issuer \"$ISSUER\" --subject %s --audiences api://AzureADTokenExchange\n```\n\n", id.Namespace, id.ServiceAccount, subject)
	case "gcp":
		fmt.Fprintf(&b, " keeps Google service account `%s`. Google Cloud federates another cluster through a workload identity pool and a credential configuration file, which isn't added to the Pods for you. Create the pool's provider for the issuer and let the ServiceAccount act as the Google service account, then follow Google's guide to mount the configuration (create-cred-config) and a projected token, with GOOGLE_APPLICATION_CREDENTIALS pointing at it.\n\n", id.ID)
		fmt.Fprintf(&b, "```sh\ngcloud iam workload-identity-pools providers create-oidc <provider> --location global --workload-identity-pool <pool> \\\n  --issuer-uri \"$ISSUER\" --attribute-mapping google.subject=assertion.sub\ngcloud iam service-accounts add-iam-policy-binding %s --role roles/iam.workloadIdentityUser \\\n  --member \"principal://iam.googleapis.com/projects/<project number>/locations/global/workloadIdentityPools/<pool>/subject/%s\"\n```\n\n", id.ID, subject)
	}
	return b.String()
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
