// writes RESTORE.md's identity steps, the IAM roles service accounts use (IRSA and Pod Identity), and the aws resources the manifests name

package kustomize

import (
	"fmt"
	"regexp"
	"strings"

	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/portability"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// awsID matches aws resource ids and arns that tie a manifest to resources outside the cluster
var awsID = regexp.MustCompile(`\b(?:arn:aws[a-z-]*:[a-z0-9-]+:[a-z0-9-]*:[0-9]*:[^\s"',]+|(?:vol|subnet|sg|vpc|fs|fsap|eni|snap|ami|igw|nat|rtb|pl|vpce)-[0-9a-f]{8,17})\b`)

// irsaRole is a service account that names an IAM role (IRSA), by namespace, name and role arn
type irsaRole struct{ ns, sa, role string }

// awsIDRef is an aws id or arn a written manifest names, and where, as the file and line
type awsIDRef struct{ id, where string }

// identity writes the steps for aws IAM roles bound to service accounts, and the aws ids in the manifests
func (d *restoreData) identity(p *playbook) {
	// invoking awsReferences for the roles and ids the manifests name, its defined below
	roles, ids := d.awsReferences()
	aws := d.s.GetInventory().GetAws()
	pods := aws.GetPodIdentityAssociations()
	// checking if theres an EKS playbook and something for it, roles, pod identity associations, or an EKS export taken without --aws
	// otherwise, if service accounts still name roles, a note in RESTORE.md says they do nothing off EKS
	if q := d.eksp; q != nil && (len(roles) > 0 || len(pods) > 0 || (d.eks && aws == nil)) {
		q.section("IAM roles for service accounts")
		// checking if there are IRSA roles, if so irsaSteps writes the trust policy changes, its defined below
		if len(roles) > 0 {
			irsaSteps(q, roles, aws.GetOidcIssuer())
		}
		// invoking podIdentitySteps for the pod identity associations, its defined below
		d.podIdentitySteps(q, pods, aws == nil)
	} else if len(roles) > 0 {
		// invoking nonEKSRolesNote to list the roles, its defined below
		nonEKSRolesNote(p, roles)
	}
	// checking if the manifests name aws ids, if so awsIDsStep lists them, its defined below
	if len(ids) > 0 {
		d.awsIDsStep(p, ids)
	}
	// invoking regionBound for the values bound to this cluster's region or account, and regionBoundStep to list them, theyre defined below
	if bound := d.regionBound(); len(bound) > 0 {
		regionBoundStep(p, bound)
	}
}

// boundRef is a value bound to one region or account in a written manifest, and where, as the file and line
type boundRef struct {
	portability.Match
	where string
}

// regionBound gives back the values in the written manifests bound to one region or account, by file and line, CRDs and Secrets left out
func (d *restoreData) regionBound() []boundRef {
	var out []boundRef
	// going through what was written
	for _, w := range d.written {
		r := w.resource
		// invoking isCRD and checking if its a CRD or a Secret, if so its skipped, CRD schemas show example values and Secrets hold placeholders
		if isCRD(r) || (r.Kind == "Secret" && r.ApiVersion == "v1") {
			continue
		}
		// going through the file line by line, invoking portability.Find on each, its defined in pkg/portability/portability.go
		for i, line := range strings.Split(string(w.yaml), "\n") {
			// checking if its the IRSA annotation line, if so its skipped, the IRSA step lists the role with what to change
			if strings.Contains(line, irsaAnnotation) {
				continue
			}
			for _, m := range portability.Find(line) {
				out = append(out, boundRef{m, fmt.Sprintf("%s:%d", w.path, i+1)})
			}
		}
	}
	return out
}

// regionBoundStep lists the values bound to this cluster's region or account, what each is bound to and where it is
func regionBoundStep(p *playbook, refs []boundRef) {
	p.section("Values bound to a region or account")
	p.printf("**Values bound to a region or account.** They work in this cluster's\n" +
		"region and account. Restoring in another region, account or cloud, give\n" +
		"each its new value before applying (an image copied to the new registry,\n" +
		"a certificate, key or queue made there, the new network's subnets):\n\n")
	p.printf("| Value | Kind | Bound to | Where |\n|---|---|---|---|\n")
	// writing a table row per value and place
	for _, r := range refs {
		p.printf("| `%s` | %s | %s | %s |\n", cell(r.Value), cell(r.Kind), cell(r.Bound), cell(r.where))
	}
	p.printf("\n")
}

// awsReferences gives back the service accounts with an IRSA role, and the aws ids the written manifests name, by file and line
func (d *restoreData) awsReferences() (roles []irsaRole, ids []awsIDRef) {
	seen := map[string]bool{}
	// going through what was written
	for _, w := range d.written {
		r := w.resource
		// checking if its a service account
		if r.Kind == "ServiceAccount" && r.ApiVersion == "v1" {
			// invoking annotation and checking if it names an IRSA role, its defined in manifest.go
			if arn := annotation(d.manifests.of(r), irsaAnnotation); arn != "" {
				roles = append(roles, irsaRole{r.Namespace, r.Name, arn})
			}
		}
		// invoking isCRD and checking if its a CRD, if so its skipped, its defined in kustomize.go
		if isCRD(r) {
			continue // schemas, whose descriptions show example arns
		}
		// going through the file line by line for ids
		for i, line := range strings.Split(string(w.yaml), "\n") {
			// checking if its the IRSA annotation line, if so its skipped
			if strings.Contains(line, irsaAnnotation) {
				continue // listed with the roles
			}
			// going through each id on the line
			for _, id := range awsID.FindAllString(line, -1) {
				// checking if its bound to a region or account, if so the region-bound step lists it with what its bound to, invoking portability.Find, its defined in pkg/portability/portability.go
				if len(portability.Find(id)) > 0 {
					continue
				}
				where := fmt.Sprintf("%s:%d", w.path, i+1)
				// checking if this id at this place isnt listed yet
				if !seen[id+where] {
					seen[id+where] = true
					ids = append(ids, awsIDRef{id, where})
				}
			}
		}
	}
	return roles, ids
}

// irsaSteps writes the commands that make each IRSA role trust the new cluster's oidc provider instead of the source's (oldIssuer)
func irsaSteps(q *playbook, roles []irsaRole, oldIssuer string) {
	oldHost := strings.TrimPrefix(oldIssuer, "https://")
	// checking if the source's issuer isnt known, if so the command gets a placeholder
	if oldHost == "" {
		oldHost = "<source-oidc-issuer-host>"
	}
	q.printf("**IAM roles for service accounts (IRSA).** These service accounts assume IAM\n" +
		"roles through the cluster's OIDC provider. The new cluster has a different\n" +
		"one, so each role's trust policy must trust it. The commands below replace\n" +
		"the old issuer; to keep the source working too, add a statement instead.\n\n")
	q.printf("| Service account | Role |\n|---|---|\n")
	// writing a table row per service account, cell is defined in manifest.go
	for _, r := range roles {
		q.printf("| %s/%s | `%s` |\n", cell(r.ns), cell(r.sa), cell(r.role))
	}
	q.printf("\n")
	// reading the new cluster's issuer and creating its oidc provider when its missing
	lines := []string{
		`NEW_OIDC=$(aws eks describe-cluster --region "$REGION" --name "$DST_CLUSTER" --query cluster.identity.oidc.issuer --output text | sed 's#^https://##')`,
		`aws iam list-open-id-connect-providers | grep -q "$NEW_OIDC" || aws iam create-open-id-connect-provider --url "https://$NEW_OIDC" --client-id-list sts.amazonaws.com`,
	}
	done := map[string]bool{}
	// building the trust policy update for each role once
	for _, r := range roles {
		name := r.role[strings.LastIndex(r.role, "/")+1:]
		// checking if the role was handled already, several service accounts can share one
		if done[name] {
			continue
		}
		done[name] = true
		// text.ShellQuote is defined in internal/text/text.go
		lines = append(lines,
			fmt.Sprintf(`aws iam get-role --role-name %s --query Role.AssumeRolePolicyDocument > %s`, text.ShellQuote(name), text.ShellQuote(name+"-trust.json")),
			// the hosts reach perl through its environment, so no shell expands them
			fmt.Sprintf(`OLD_OIDC=%s NEW_OIDC="$NEW_OIDC" perl -pi -e 's#\Q$ENV{OLD_OIDC}\E#$ENV{NEW_OIDC}#g' %s`, text.ShellQuote(oldHost), text.ShellQuote(name+"-trust.json")),
			fmt.Sprintf(`aws iam update-assume-role-policy --role-name %s --policy-document file://%s`, text.ShellQuote(name), text.ShellQuote(name+"-trust.json")))
	}
	q.code(lines...)
}

// podIdentitySteps writes the pod identity associations to create, or without --aws (notRead) the command that lists them on the source
func (d *restoreData) podIdentitySteps(q *playbook, pods []*snapshotv1alpha1.PodIdentityAssociation, notRead bool) {
	// checking if associations were read, otherwise if its EKS and aws wasnt read
	switch {
	case len(pods) > 0:
		q.printf("**EKS Pod Identity.** Associations are EKS settings, not Kubernetes objects;\n" +
			"create them for the new cluster (the roles already trust Pod Identity):\n\n")
		var lines []string
		// building a create command per association
		for _, a := range pods {
			lines = append(lines, fmt.Sprintf(`aws eks create-pod-identity-association --region "$REGION" --cluster-name "$DST_CLUSTER" --namespace %s --service-account %s --role-arn %s`,
				text.ShellQuote(a.Namespace), text.ShellQuote(a.ServiceAccount), text.ShellQuote(a.RoleArn)))
		}
		q.code(lines...)
	case d.eks && notRead:
		q.printf("**EKS Pod Identity.** Associations are EKS settings, not Kubernetes objects.\n" +
			"Export with `--aws` to have them listed here, or list them on the source\n" +
			"and create each for the new cluster:\n\n")
		q.code(fmt.Sprintf(`aws eks list-pod-identity-associations --region "$REGION" --cluster-name %s`, text.ShellQuote(d.source)))
	}
}

// nonEKSRolesNote lists the service accounts that name IAM roles, which do nothing off EKS
// the annotation needs EKS's webhook, so theres no command here
func nonEKSRolesNote(p *playbook, roles []irsaRole) {
	p.section("Service accounts with AWS roles")
	p.printf("These service accounts name AWS IAM roles (IRSA), which only EKS acts on.\n" +
		"On EKS, the roles must trust the new cluster's OIDC provider; elsewhere,\n" +
		"give the workloads the new platform's own identity instead (Azure or GKE\n" +
		"Workload Identity, a Secret), or leave the annotation, which does nothing:\n\n")
	p.printf("| Service account | Role |\n|---|---|\n")
	// writing a table row per service account
	for _, r := range roles {
		p.printf("| %s/%s | `%s` |\n", cell(r.ns), cell(r.sa), cell(r.role))
	}
	p.printf("\n")
}

// awsIDsStep lists the aws resources the manifests name, in the aws playbook when there is one
func (d *restoreData) awsIDsStep(p *playbook, ids []awsIDRef) {
	q := d.awsp
	// checking if theres no aws playbook, if so the step goes in RESTORE.md itself
	if q == nil {
		q = p
	}
	q.section("AWS resources the manifests name")
	q.printf("**AWS resources the manifests name.** Check that the new cluster can reach\n" +
		"each (same account and region, security groups, subnets); change the ones\n" +
		"that differ before applying:\n\n")
	q.printf("| ID | Where |\n|---|---|\n")
	// writing a table row per id and place
	for _, id := range ids {
		q.printf("| `%s` | %s |\n", cell(id.id), cell(id.where))
	}
	q.printf("\n")
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
