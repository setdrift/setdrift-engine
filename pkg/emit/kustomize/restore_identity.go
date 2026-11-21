// RESTORE.md's identity steps: IAM roles service accounts use (IRSA and Pod
// Identity), and the AWS resources the manifests name.

package kustomize

import (
	"fmt"
	"regexp"
	"strings"

	"setdrift.io/setdrift-engine/internal/text"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// awsID matches AWS resource IDs and ARNs that tie a manifest to resources
// outside the cluster.
var awsID = regexp.MustCompile(`\b(?:arn:aws[a-z-]*:[a-z0-9-]+:[a-z0-9-]*:[0-9]*:[^\s"',]+|(?:vol|subnet|sg|vpc|fs|fsap|eni|snap|ami|igw|nat|rtb|pl|vpce)-[0-9a-f]{8,17})\b`)

// identity: AWS IAM roles bound to service accounts, and AWS IDs in the
// manifests.
// irsaRole is a service account that names an IAM role (IRSA).
type irsaRole struct{ ns, sa, role string }

// awsIDRef is an AWS ID or ARN a written manifest names, and where.
type awsIDRef struct{ id, where string }

// identity writes the steps for AWS IAM roles bound to service accounts, and
// AWS IDs in the manifests.
func (d *restoreData) identity(p *playbook) {
	roles, ids := d.awsReferences()
	aws := d.s.GetInventory().GetAws()
	pods := aws.GetPodIdentityAssociations()
	if q := d.eksp; q != nil && (len(roles) > 0 || len(pods) > 0 || (d.eks && aws == nil)) {
		q.section("IAM roles for service accounts")
		if len(roles) > 0 {
			irsaSteps(q, roles, aws.GetOidcIssuer())
		}
		d.podIdentitySteps(q, pods, aws == nil)
	} else if len(roles) > 0 {
		nonEKSRolesNote(p, roles)
	}
	if len(ids) > 0 {
		d.awsIDsStep(p, ids)
	}
}

// awsReferences returns the service accounts with an IRSA role, and the AWS
// IDs the written manifests name, by file and line.
func (d *restoreData) awsReferences() (roles []irsaRole, ids []awsIDRef) {
	seen := map[string]bool{}
	for _, w := range d.written {
		r := w.resource
		if r.Kind == "ServiceAccount" && r.ApiVersion == "v1" {
			if arn := annotation(d.manifests.of(r), irsaAnnotation); arn != "" {
				roles = append(roles, irsaRole{r.Namespace, r.Name, arn})
			}
		}
		if isCRD(r) {
			continue // schemas, whose descriptions show example ARNs
		}
		for i, line := range strings.Split(string(w.yaml), "\n") {
			if strings.Contains(line, irsaAnnotation) {
				continue // listed with the roles
			}
			for _, id := range awsID.FindAllString(line, -1) {
				where := fmt.Sprintf("%s:%d", w.path, i+1)
				if !seen[id+where] {
					seen[id+where] = true
					ids = append(ids, awsIDRef{id, where})
				}
			}
		}
	}
	return roles, ids
}

// irsaSteps writes the commands that make each IRSA role trust the new
// cluster's OIDC provider instead of the source's (oldIssuer).
func irsaSteps(q *playbook, roles []irsaRole, oldIssuer string) {
	oldHost := strings.TrimPrefix(oldIssuer, "https://")
	if oldHost == "" {
		oldHost = "<source-oidc-issuer-host>"
	}
	q.printf("**IAM roles for service accounts (IRSA).** These service accounts assume IAM\n" +
		"roles through the cluster's OIDC provider. The new cluster has a different\n" +
		"one, so each role's trust policy must trust it. The commands below replace\n" +
		"the old issuer; to keep the source working too, add a statement instead.\n\n")
	q.printf("| Service account | Role |\n|---|---|\n")
	for _, r := range roles {
		q.printf("| %s/%s | `%s` |\n", cell(r.ns), cell(r.sa), cell(r.role))
	}
	q.printf("\n")
	lines := []string{
		`NEW_OIDC=$(aws eks describe-cluster --region "$REGION" --name "$DST_CLUSTER" --query cluster.identity.oidc.issuer --output text | sed 's#^https://##')`,
		`aws iam list-open-id-connect-providers | grep -q "$NEW_OIDC" || aws iam create-open-id-connect-provider --url "https://$NEW_OIDC" --client-id-list sts.amazonaws.com`,
	}
	done := map[string]bool{}
	for _, r := range roles {
		name := r.role[strings.LastIndex(r.role, "/")+1:]
		if done[name] {
			continue
		}
		done[name] = true
		lines = append(lines,
			fmt.Sprintf(`aws iam get-role --role-name %s --query Role.AssumeRolePolicyDocument > %s`, text.ShellQuote(name), text.ShellQuote(name+"-trust.json")),
			// the hosts reach perl through its environment, so no shell expands them
			fmt.Sprintf(`OLD_OIDC=%s NEW_OIDC="$NEW_OIDC" perl -pi -e 's#\Q$ENV{OLD_OIDC}\E#$ENV{NEW_OIDC}#g' %s`, text.ShellQuote(oldHost), text.ShellQuote(name+"-trust.json")),
			fmt.Sprintf(`aws iam update-assume-role-policy --role-name %s --policy-document file://%s`, text.ShellQuote(name), text.ShellQuote(name+"-trust.json")))
	}
	q.code(lines...)
}

// podIdentitySteps writes the Pod Identity associations to create; without
// --aws (notRead), the command that lists them on the source.
func (d *restoreData) podIdentitySteps(q *playbook, pods []*snapshotv1alpha1.PodIdentityAssociation, notRead bool) {
	switch {
	case len(pods) > 0:
		q.printf("**EKS Pod Identity.** Associations are EKS settings, not Kubernetes objects;\n" +
			"create them for the new cluster (the roles already trust Pod Identity):\n\n")
		var lines []string
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

// nonEKSRolesNote lists the service accounts that name IAM roles, which do
// nothing off EKS.
func nonEKSRolesNote(p *playbook, roles []irsaRole) {
	// Not on EKS: the annotation needs EKS's webhook, so no commands here.
	p.section("Service accounts with AWS roles")
	p.printf("These service accounts name AWS IAM roles (IRSA), which only EKS acts on.\n" +
		"On EKS, the roles must trust the new cluster's OIDC provider; elsewhere,\n" +
		"give the workloads the new platform's own identity instead (Azure or GKE\n" +
		"Workload Identity, a Secret), or leave the annotation, which does nothing:\n\n")
	p.printf("| Service account | Role |\n|---|---|\n")
	for _, r := range roles {
		p.printf("| %s/%s | `%s` |\n", cell(r.ns), cell(r.sa), cell(r.role))
	}
	p.printf("\n")
}

// awsIDsStep lists the AWS resources the manifests name, in the AWS
// playbook when there is one.
func (d *restoreData) awsIDsStep(p *playbook, ids []awsIDRef) {
	q := d.awsp
	if q == nil {
		q = p
	}
	q.section("AWS resources the manifests name")
	q.printf("**AWS resources the manifests name.** Check that the new cluster can reach\n" +
		"each (same account and region, security groups, subnets); change the ones\n" +
		"that differ before applying:\n\n")
	q.printf("| ID | Where |\n|---|---|\n")
	for _, id := range ids {
		q.printf("| `%s` | %s |\n", cell(id.id), cell(id.where))
	}
	q.printf("\n")
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
