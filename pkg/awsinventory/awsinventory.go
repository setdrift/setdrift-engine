// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Package awsinventory reads, read-only, the EKS configuration that lives in
// AWS rather than in the cluster, for the restore playbook: IRSA roles, Pod
// Identity, access entries, add-ons and EBS volumes. A failed call is
// recorded in the inventory's errors and the other calls still run.
package awsinventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/smithy-go"
	"sigs.k8s.io/yaml"

	"setdrift.io/setdrift-engine/pkg/redact"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// EKS is the part of the EKS client Collect uses. The CLI passes the AWS
// SDK's clients; tests pass fakes, so they run without AWS.
type EKS interface {
	DescribeCluster(context.Context, *eks.DescribeClusterInput, ...func(*eks.Options)) (*eks.DescribeClusterOutput, error)
	ListPodIdentityAssociations(context.Context, *eks.ListPodIdentityAssociationsInput, ...func(*eks.Options)) (*eks.ListPodIdentityAssociationsOutput, error)
	DescribePodIdentityAssociation(context.Context, *eks.DescribePodIdentityAssociationInput, ...func(*eks.Options)) (*eks.DescribePodIdentityAssociationOutput, error)
	ListAccessEntries(context.Context, *eks.ListAccessEntriesInput, ...func(*eks.Options)) (*eks.ListAccessEntriesOutput, error)
	DescribeAccessEntry(context.Context, *eks.DescribeAccessEntryInput, ...func(*eks.Options)) (*eks.DescribeAccessEntryOutput, error)
	ListAssociatedAccessPolicies(context.Context, *eks.ListAssociatedAccessPoliciesInput, ...func(*eks.Options)) (*eks.ListAssociatedAccessPoliciesOutput, error)
	ListAddons(context.Context, *eks.ListAddonsInput, ...func(*eks.Options)) (*eks.ListAddonsOutput, error)
	DescribeAddon(context.Context, *eks.DescribeAddonInput, ...func(*eks.Options)) (*eks.DescribeAddonOutput, error)
}

// IAM is the part of the IAM client Collect uses.
type IAM interface {
	GetRole(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error)
}

// EC2 is the part of the EC2 client Collect uses.
type EC2 interface {
	DescribeVolumes(context.Context, *ec2.DescribeVolumesInput, ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
}

// Clients are the AWS clients Collect calls.
type Clients struct {
	EKS EKS
	IAM IAM
	EC2 EC2
}

// irsaAnnotation names the IAM role a ServiceAccount assumes with IRSA.
const irsaAnnotation = "eks.amazonaws.com/role-arn"

// Collect reads the AWS side of cluster, records it in snap.Inventory.Aws, and the EBS details of snap's volumes.
// It returns an error only when the cluster can't be described or ctx is done.
func Collect(ctx context.Context, c Clients, cluster, region string, snap *snapshotv1alpha1.Snapshot) error {
	if snap.Inventory == nil {
		snap.Inventory = &snapshotv1alpha1.Inventory{}
	}
	inv := &snapshotv1alpha1.AwsInventory{ClusterName: cluster, Region: region}
	snap.Inventory.Aws = inv
	fail := func(what string, err error) {
		inv.Errors = append(inv.Errors, fmt.Sprintf("%s: %s", what, short(err)))
	}
	// only fatal call, nothing makes sense without the cluster
	dc, err := c.EKS.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(cluster)})
	if err != nil {
		return fmt.Errorf("reading EKS cluster %s in %s: %w", cluster, region, err)
	}
	// pulls the k8s version, oidc issuer, irsa roles trust, accountid (from ARN), cluster ARN, and kubeconfig if not provided
	ec := dc.Cluster
	inv.KubernetesVersion = aws.ToString(ec.Version)
	if ec.Identity != nil && ec.Identity.Oidc != nil {
		inv.OidcIssuer = aws.ToString(ec.Identity.Oidc.Issuer)
	}
	if arn := aws.ToString(ec.Arn); arn != "" {
		if parts := strings.Split(arn, ":"); len(parts) > 4 {
			inv.AccountId = parts[4]
		}
		if snap.Cluster != nil && snap.Cluster.ProviderId == "" {
			snap.Cluster.ProviderId = arn
		}
	}
	if snap.Cluster != nil {
		snap.Cluster.Platform = "eks"
	}

	// gather every role before iamRoles reads them: those Pod Identity associations use, and those ServiceAccounts name for IRSA
	roles := map[string]bool{}
	podIdentity(ctx, c.EKS, cluster, inv, roles, fail)
	for _, r := range snap.Resources {
		if r.Kind == "ServiceAccount" && r.ApiVersion == "v1" {
			if arn := annotation(r.ManifestJson, irsaAnnotation); arn != "" {
				roles[arn] = true
			}
		}
	}
	iamRoles(ctx, c.IAM, roles, inv, fail)
	accessEntries(ctx, c.EKS, cluster, inv, fail)
	addons(ctx, c.EKS, cluster, inv, fail)
	volumes(ctx, c.EC2, snap.Inventory.Volumes, fail)
	// after a Ctrl-C every call fails at once; stop the export rather than write an inventory of "context canceled"
	return ctx.Err()
}

// podIdentity lists and describes every Pod Identity association, sorted by
// namespace and ServiceAccount, and adds their roles to roles.
func podIdentity(ctx context.Context, c EKS, cluster string, inv *snapshotv1alpha1.AwsInventory, roles map[string]bool, fail func(string, error)) {
	var token *string
	for {
		out, err := c.ListPodIdentityAssociations(ctx, &eks.ListPodIdentityAssociationsInput{ClusterName: aws.String(cluster), NextToken: token})
		if err != nil {
			fail("Pod Identity associations", err)
			return
		}
		for _, a := range out.Associations {
			d, err := c.DescribePodIdentityAssociation(ctx, &eks.DescribePodIdentityAssociationInput{
				ClusterName: aws.String(cluster), AssociationId: a.AssociationId,
			})
			if err != nil {
				fail("Pod Identity association "+aws.ToString(a.AssociationId), err)
				continue
			}
			role := aws.ToString(d.Association.RoleArn)
			inv.PodIdentityAssociations = append(inv.PodIdentityAssociations, &snapshotv1alpha1.PodIdentityAssociation{
				Namespace: aws.ToString(a.Namespace), ServiceAccount: aws.ToString(a.ServiceAccount), RoleArn: role,
			})
			roles[role] = true
		}
		if token = out.NextToken; token == nil {
			break
		}
	}
	sort.Slice(inv.PodIdentityAssociations, func(i, j int) bool {
		a, b := inv.PodIdentityAssociations[i], inv.PodIdentityAssociations[j]
		return a.Namespace+"/"+a.ServiceAccount < b.Namespace+"/"+b.ServiceAccount
	})
}

// trustPolicy is the part of an IAM trust policy iamRoles reads.
type trustPolicy struct {
	Statement []struct {
		Principal struct {
			Federated any `json:"Federated"`
			Service   any `json:"Service"`
		} `json:"Principal"`
	} `json:"Statement"`
}

// iamRoles reads each role's trust policy, in ARN order: which OIDC
// providers it trusts (IRSA), and whether it trusts Pod Identity.
func iamRoles(ctx context.Context, c IAM, roles map[string]bool, inv *snapshotv1alpha1.AwsInventory, fail func(string, error)) {
	arns := make([]string, 0, len(roles))
	for arn := range roles {
		if arn != "" {
			arns = append(arns, arn)
		}
	}
	sort.Strings(arns)
	for _, arn := range arns {
		name := arn[strings.LastIndex(arn, "/")+1:]
		out, err := c.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
		if err != nil {
			fail("IAM role "+name, err)
			continue
		}
		role := &snapshotv1alpha1.IamRole{Arn: arn}
		if role.OidcProviders, role.TrustsPodIdentity, err = parseTrust(aws.ToString(out.Role.AssumeRolePolicyDocument)); err != nil {
			fail("trust policy of IAM role "+name, err)
		}
		inv.IamRoles = append(inv.IamRoles, role)
	}
}

// parseTrust reads a role's trust policy (URL-encoded JSON, as IAM returns
// it): the OIDC providers it trusts, and whether it trusts Pod Identity.
func parseTrust(doc string) (oidcProviders []string, podIdentity bool, err error) {
	raw, err := url.QueryUnescape(doc)
	if err != nil {
		return nil, false, err
	}
	var tp trustPolicy
	if err := json.Unmarshal([]byte(raw), &tp); err != nil {
		return nil, false, err
	}
	for _, st := range tp.Statement {
		for _, f := range strs(st.Principal.Federated) {
			if _, provider, ok := strings.Cut(f, ":oidc-provider/"); ok {
				oidcProviders = append(oidcProviders, provider)
			}
		}
		if slices.Contains(strs(st.Principal.Service), "pods.eks.amazonaws.com") {
			podIdentity = true
		}
	}
	return oidcProviders, podIdentity, nil
}

// accessEntries lists and describes the access entries (type, Kubernetes
// groups) and their access policies, with each policy's scope (the cluster,
// or namespaces).
func accessEntries(ctx context.Context, c EKS, cluster string, inv *snapshotv1alpha1.AwsInventory, fail func(string, error)) {
	var token *string
	var principals []string
	for {
		out, err := c.ListAccessEntries(ctx, &eks.ListAccessEntriesInput{ClusterName: aws.String(cluster), NextToken: token})
		if err != nil {
			fail("access entries", err)
			return
		}
		principals = append(principals, out.AccessEntries...)
		if token = out.NextToken; token == nil {
			break
		}
	}
	sort.Strings(principals)
	for _, p := range principals {
		d, err := c.DescribeAccessEntry(ctx, &eks.DescribeAccessEntryInput{ClusterName: aws.String(cluster), PrincipalArn: aws.String(p)})
		if err != nil {
			fail("access entry "+p, err)
			continue
		}
		e := &snapshotv1alpha1.AccessEntry{
			PrincipalArn:     p,
			Type:             aws.ToString(d.AccessEntry.Type),
			KubernetesGroups: d.AccessEntry.KubernetesGroups,
		}
		var polToken *string
		for {
			pols, err := c.ListAssociatedAccessPolicies(ctx, &eks.ListAssociatedAccessPoliciesInput{
				ClusterName: aws.String(cluster), PrincipalArn: aws.String(p), NextToken: polToken,
			})
			if err != nil {
				fail("access policies of "+p, err)
				break
			}
			for _, ap := range pols.AssociatedAccessPolicies {
				pol := &snapshotv1alpha1.AccessPolicy{PolicyArn: aws.ToString(ap.PolicyArn)}
				if ap.AccessScope != nil {
					pol.Scope = string(ap.AccessScope.Type)
					pol.Namespaces = ap.AccessScope.Namespaces
				}
				e.Policies = append(e.Policies, pol)
			}
			if polToken = pols.NextToken; polToken == nil {
				break
			}
		}
		inv.AccessEntries = append(inv.AccessEntries, e)
	}
}

// addons lists and describes the add-ons: version, ServiceAccount role and
// configuration values (redacted).
func addons(ctx context.Context, c EKS, cluster string, inv *snapshotv1alpha1.AwsInventory, fail func(string, error)) {
	var token *string
	var names []string
	for {
		out, err := c.ListAddons(ctx, &eks.ListAddonsInput{ClusterName: aws.String(cluster), NextToken: token})
		if err != nil {
			fail("add-ons", err)
			return
		}
		names = append(names, out.Addons...)
		if token = out.NextToken; token == nil {
			break
		}
	}
	sort.Strings(names)
	for _, n := range names {
		d, err := c.DescribeAddon(ctx, &eks.DescribeAddonInput{ClusterName: aws.String(cluster), AddonName: aws.String(n)})
		if err != nil {
			fail("add-on "+n, err)
			continue
		}
		config, redacted := addonConfig(aws.ToString(d.Addon.ConfigurationValues))
		inv.Addons = append(inv.Addons, &snapshotv1alpha1.Addon{
			Name:                    n,
			Version:                 aws.ToString(d.Addon.AddonVersion),
			ServiceAccountRoleArn:   aws.ToString(d.Addon.ServiceAccountRoleArn),
			ConfigurationValues:     config,
			ConfigurationRedactions: redacted,
		})
	}
}

// addonConfig redacts secret values (e.g. a vendor add-on's API key) from an add-on's configuration, since it is
// written into RESTORE.md. It is returned unchanged when nothing was redacted, as JSON when something was, and
// as SETDRIFT_REDACTED when it does not parse, since then nothing can be checked.
func addonConfig(config string) (string, []string) {
	if strings.TrimSpace(config) == "" {
		return config, nil
	}
	var v any
	if err := yaml.Unmarshal([]byte(config), &v); err != nil {
		return redact.Placeholder, nil
	}
	v, paths := redact.Values(v)
	if len(paths) == 0 {
		return config, nil
	}
	out, err := json.Marshal(v)
	if err != nil {
		return redact.Placeholder, nil
	}
	return string(out), paths
}

// describeVolumesBatch is how many volume IDs one DescribeVolumes call
// asks for: under the API's 500, so one call's response stays small.
const describeVolumesBatch = 200

// volumes fills in the EBS details of the volumes with an EBS volume ID.
func volumes(ctx context.Context, c EC2, vols []*snapshotv1alpha1.Volume, fail func(string, error)) {
	byID := map[string][]*snapshotv1alpha1.Volume{}
	var ids []string
	for _, v := range vols {
		if strings.HasPrefix(v.VolumeHandle, "vol-") {
			if _, ok := byID[v.VolumeHandle]; !ok {
				ids = append(ids, v.VolumeHandle)
			}
			byID[v.VolumeHandle] = append(byID[v.VolumeHandle], v)
		}
	}
	// DescribeVolumes takes up to 500 IDs, one missing ID fails the whole call, so fall back to one call per ID.
	// Only then: any other failure (e.g. access denied) would fail every ID too, so it is recorded once.
	for start := 0; start < len(ids); start += describeVolumesBatch {
		batch := ids[start:min(start+describeVolumesBatch, len(ids))]
		out, err := c.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: batch})
		if err != nil {
			if !notFound(err) {
				fail(fmt.Sprintf("EBS volumes (%d)", len(batch)), err)
				continue
			}
			for _, id := range batch {
				one, err := c.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{id}})
				if err != nil {
					fail("EBS volume "+id, err)
					continue
				}
				record(one, byID)
			}
			continue
		}
		record(out, byID)
	}
}

// notFound reports whether err is EC2's error for a volume that doesn't exist.
func notFound(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == "InvalidVolume.NotFound"
}

// record copies each described volume's attributes onto the snapshot
// volumes with its ID.
func record(out *ec2.DescribeVolumesOutput, byID map[string][]*snapshotv1alpha1.Volume) {
	for _, ev := range out.Volumes {
		a := &snapshotv1alpha1.AwsVolume{
			VolumeType:       string(ev.VolumeType),
			SizeGib:          aws.ToInt32(ev.Size),
			Iops:             aws.ToInt32(ev.Iops),
			Throughput:       aws.ToInt32(ev.Throughput),
			Encrypted:        aws.ToBool(ev.Encrypted),
			KmsKeyId:         aws.ToString(ev.KmsKeyId),
			AvailabilityZone: aws.ToString(ev.AvailabilityZone),
			State:            string(ev.State),
		}
		for _, v := range byID[aws.ToString(ev.VolumeId)] {
			v.Aws = a
		}
	}
}

// annotation reads one annotation from a manifest's JSON; "" if the
// manifest has none or does not parse.
func annotation(manifest, key string) string {
	var m struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal([]byte(manifest), &m) // a snapshot's manifest: JSON the export wrote
	return m.Metadata.Annotations[key]
}

// strs returns a policy principal, a string or a list of them, as a list.
func strs(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, s := range t {
			if str, ok := s.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// short returns an AWS error's code and message, without request IDs.
func short(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode() + ": " + api.ErrorMessage()
	}
	return err.Error()
}
