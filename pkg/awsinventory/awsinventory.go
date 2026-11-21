// Package awsinventory reads, read-only, the EKS configuration that lives in AWS rather than in the cluster, for the restore playbook, IRSA roles, Pod Identity, access entries, add-ons and EBS volumes
// a failed call is recorded in the inventory's errors and the other calls still run
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

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// EKS is the part of the EKS client Collect uses
// the cli passes the AWS SDK's clients and tests pass fakes, so they run without AWS
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

// IAM is the part of the IAM client Collect uses, only GetRole for the trust policies
type IAM interface {
	GetRole(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error)
}

// EC2 is the part of the EC2 client Collect uses, only DescribeVolumes for the EBS details
type EC2 interface {
	DescribeVolumes(context.Context, *ec2.DescribeVolumesInput, ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
}

// Clients are the AWS clients Collect calls, bundled so the cli can build them once and pass them in
type Clients struct {
	EKS EKS
	IAM IAM
	EC2 EC2
}

// irsaAnnotation is the ServiceAccount annotation naming the IAM role it assumes with IRSA
const irsaAnnotation = "eks.amazonaws.com/role-arn"

// Collect takes the AWS clients, the EKS cluster's name and region and the snapshot, reads the AWS side of the cluster into snap.Inventory.Aws, and fills in the EBS details of snap's volumes
// it gives back an error only when the cluster cant be described or ctx is done, every other failure is recorded in the inventory's errors
func Collect(ctx context.Context, c Clients, cluster, region string, snap *snapshotv1alpha1.Snapshot) error {
	// checking if the snapshot has no inventory yet, if so one is started for the AWS part to go in
	if snap.Inventory == nil {
		snap.Inventory = &snapshotv1alpha1.Inventory{}
	}
	inv := &snapshotv1alpha1.AwsInventory{ClusterName: cluster, Region: region}
	snap.Inventory.Aws = inv
	// fail records a failed call in the inventory's errors, so the other calls still run, invoking short to keep the error brief, its defined below
	fail := func(what string, err error) {
		inv.Errors = append(inv.Errors, fmt.Sprintf("%s: %s", what, short(err)))
	}
	// the only call whose failure is a terminal error, nothing makes sense without the cluster
	dc, err := c.EKS.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(cluster)})
	if err != nil {
		return fmt.Errorf("reading EKS cluster %s in %s: %w", cluster, region, err)
	}
	// taking the Kubernetes version, the OIDC issuer, the account id from the cluster ARN, and the ARN as the provider id when the snapshot has none
	ec := dc.Cluster
	inv.KubernetesVersion = aws.ToString(ec.Version)
	// checking if the cluster has an OIDC identity, IRSA roles trust its issuer
	if ec.Identity != nil && ec.Identity.Oidc != nil {
		inv.OidcIssuer = aws.ToString(ec.Identity.Oidc.Issuer)
	}
	// checking if the cluster has an ARN
	if arn := aws.ToString(ec.Arn); arn != "" {
		// checking if the ARN has an account part, the fifth one, if so thats the cluster's account
		if parts := strings.Split(arn, ":"); len(parts) > 4 {
			inv.AccountId = parts[4]
		}
		// checking if the snapshot's cluster has no provider id yet, if so the ARN becomes it
		if snap.Cluster != nil && snap.Cluster.ProviderId == "" {
			snap.Cluster.ProviderId = arn
		}
	}
	// checking if the snapshot has a cluster, if so its marked as EKS
	if snap.Cluster != nil {
		snap.Cluster.Platform = "eks"
	}

	// gathering every role before iamRoles reads them, those Pod Identity associations use and those ServiceAccounts name for IRSA
	roles := map[string]bool{}
	// invoking podIdentity to read the associations and add their roles, its defined below
	podIdentity(ctx, c.EKS, cluster, inv, roles, fail)
	// going over the snapshot's resources looking for ServiceAccounts
	for _, r := range snap.Resources {
		// checking if its a core ServiceAccount
		if r.Kind == "ServiceAccount" && r.ApiVersion == "v1" {
			// invoking annotation and checking if it names an IRSA role, its defined below
			if arn := annotation(r.ManifestJson, irsaAnnotation); arn != "" {
				roles[arn] = true
			}
		}
	}
	// invoking iamRoles, accessEntries, addons and volumes to read the rest, theyre defined below
	iamRoles(ctx, c.IAM, roles, inv, fail)
	accessEntries(ctx, c.EKS, cluster, inv, fail)
	addons(ctx, c.EKS, cluster, inv, fail)
	volumes(ctx, c.EC2, snap.Inventory.Volumes, fail)
	// after a ctrl-c every call fails at once, so the export stops rather than write an inventory of "context canceled"
	return ctx.Err()
}

// podIdentity lists and describes every Pod Identity association into the inventory, sorted by namespace and ServiceAccount, and adds their roles to roles
// a failed call goes to fail, a failed list stops it, a failed describe only skips that association
func podIdentity(ctx context.Context, c EKS, cluster string, inv *snapshotv1alpha1.AwsInventory, roles map[string]bool, fail func(string, error)) {
	var token *string
	// going over each page of associations
	for {
		out, err := c.ListPodIdentityAssociations(ctx, &eks.ListPodIdentityAssociationsInput{ClusterName: aws.String(cluster), NextToken: token})
		// checking if the list failed, if so its recorded and theres nothing more to read
		if err != nil {
			fail("Pod Identity associations", err)
			return
		}
		// going over each association on the page, describing it for its role
		for _, a := range out.Associations {
			d, err := c.DescribePodIdentityAssociation(ctx, &eks.DescribePodIdentityAssociationInput{
				ClusterName: aws.String(cluster), AssociationId: a.AssociationId,
			})
			// checking if the describe failed, if so its recorded and this association skipped
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
		// checking if theres another page
		if token = out.NextToken; token == nil {
			break
		}
	}
	// sorting by namespace and ServiceAccount so the inventory comes out the same every run
	sort.Slice(inv.PodIdentityAssociations, func(i, j int) bool {
		a, b := inv.PodIdentityAssociations[i], inv.PodIdentityAssociations[j]
		return a.Namespace+"/"+a.ServiceAccount < b.Namespace+"/"+b.ServiceAccount
	})
}

// arnAccount gives back the account of an ARN, arn:<partition>:<service>:<region>:<account>:..., or empty when it isnt one
func arnAccount(arn string) string {
	// checking if it has all six parts and starts with arn
	if parts := strings.SplitN(arn, ":", 6); len(parts) == 6 && parts[0] == "arn" {
		return parts[4]
	}
	return ""
}

// trustPolicy is the part of an IAM trust policy iamRoles reads, each statement's federated and service principals, which can be a string or a list
type trustPolicy struct {
	Statement []struct {
		Principal struct {
			Federated any `json:"Federated"`
			Service   any `json:"Service"`
		} `json:"Principal"`
	} `json:"Statement"`
}

// iamRoles takes the role ARNs and reads each role's trust policy into the inventory, in ARN order, which OIDC providers it trusts for IRSA, and whether it trusts Pod Identity
func iamRoles(ctx context.Context, c IAM, roles map[string]bool, inv *snapshotv1alpha1.AwsInventory, fail func(string, error)) {
	arns := make([]string, 0, len(roles))
	// collecting the ARNs so theyre read in a stable order
	for arn := range roles {
		// checking if the ARN is empty, like a Pod Identity association without a role, those are left out
		if arn != "" {
			arns = append(arns, arn)
		}
	}
	sort.Strings(arns)
	// going over each role
	for _, arn := range arns {
		// checking if the role is outside the cluster's own account, invoking arnAccount for that, its defined above, only roles in that account are read
		// a ServiceAccount annotation can name any role ARN, and anyone who can write one in a namespace could otherwise have the export read and record the trust policy of a role they choose
		if account := arnAccount(arn); inv.AccountId != "" && account != inv.AccountId {
			fail("IAM role "+arn, fmt.Errorf("not read: account %s is not the cluster's (%s)", account, inv.AccountId))
			continue
		}
		// the role's name is the last part of the ARN, after any path
		name := arn[strings.LastIndex(arn, "/")+1:]
		out, err := c.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
		// checking if the role couldnt be read, if so its recorded and skipped
		if err != nil {
			fail("IAM role "+name, err)
			continue
		}
		role := &snapshotv1alpha1.IamRole{Arn: arn}
		// invoking parseTrust to read the trust policy, its defined below, a policy that doesnt parse is recorded but the role is still listed
		if role.OidcProviders, role.TrustsPodIdentity, err = parseTrust(aws.ToString(out.Role.AssumeRolePolicyDocument)); err != nil {
			fail("trust policy of IAM role "+name, err)
		}
		inv.IamRoles = append(inv.IamRoles, role)
	}
}

// parseTrust takes a role's trust policy, URL-encoded JSON as IAM gives it back, and reads the OIDC providers it trusts, and whether it trusts Pod Identity
func parseTrust(doc string) (oidcProviders []string, podIdentity bool, err error) {
	raw, err := url.QueryUnescape(doc)
	if err != nil {
		return nil, false, err
	}
	var tp trustPolicy
	if err := json.Unmarshal([]byte(raw), &tp); err != nil {
		return nil, false, err
	}
	// going over each statement
	for _, st := range tp.Statement {
		// going over each federated principal, invoking strs since it can be a string or a list, its defined below
		for _, f := range strs(st.Principal.Federated) {
			// checking if its an OIDC provider ARN, if so the provider after oidc-provider/ is kept
			if _, provider, ok := strings.Cut(f, ":oidc-provider/"); ok {
				oidcProviders = append(oidcProviders, provider)
			}
		}
		// checking if the EKS Pod Identity service is a trusted principal, invoking strs, its defined below
		if slices.Contains(strs(st.Principal.Service), "pods.eks.amazonaws.com") {
			podIdentity = true
		}
	}
	return oidcProviders, podIdentity, nil
}

// accessEntries lists and describes the access entries into the inventory, sorted by principal, with their type, Kubernetes groups and access policies
// each policy keeps its scope, the cluster or namespaces
func accessEntries(ctx context.Context, c EKS, cluster string, inv *snapshotv1alpha1.AwsInventory, fail func(string, error)) {
	var token *string
	var principals []string
	// going over each page of access entries
	for {
		out, err := c.ListAccessEntries(ctx, &eks.ListAccessEntriesInput{ClusterName: aws.String(cluster), NextToken: token})
		// checking if the list failed, if so its recorded and theres nothing more to read
		if err != nil {
			fail("access entries", err)
			return
		}
		principals = append(principals, out.AccessEntries...)
		// checking if theres another page
		if token = out.NextToken; token == nil {
			break
		}
	}
	sort.Strings(principals)
	// going over each principal to describe its entry
	for _, p := range principals {
		d, err := c.DescribeAccessEntry(ctx, &eks.DescribeAccessEntryInput{ClusterName: aws.String(cluster), PrincipalArn: aws.String(p)})
		// checking if the describe failed, if so its recorded and this entry skipped
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
		// going over each page of the entry's access policies
		for {
			pols, err := c.ListAssociatedAccessPolicies(ctx, &eks.ListAssociatedAccessPoliciesInput{
				ClusterName: aws.String(cluster), PrincipalArn: aws.String(p), NextToken: polToken,
			})
			// checking if the list failed, if so its recorded and the entry is kept with the policies read so far
			if err != nil {
				fail("access policies of "+p, err)
				break
			}
			// going over each policy on the page
			for _, ap := range pols.AssociatedAccessPolicies {
				pol := &snapshotv1alpha1.AccessPolicy{PolicyArn: aws.ToString(ap.PolicyArn)}
				// checking if the policy has a scope, if so its type and namespaces are kept
				if ap.AccessScope != nil {
					pol.Scope = string(ap.AccessScope.Type)
					pol.Namespaces = ap.AccessScope.Namespaces
				}
				e.Policies = append(e.Policies, pol)
			}
			// checking if theres another page
			if polToken = pols.NextToken; polToken == nil {
				break
			}
		}
		inv.AccessEntries = append(inv.AccessEntries, e)
	}
}

// addons lists and describes the add-ons into the inventory, sorted by name, with their version, ServiceAccount role and whether they have configuration values
func addons(ctx context.Context, c EKS, cluster string, inv *snapshotv1alpha1.AwsInventory, fail func(string, error)) {
	var token *string
	var names []string
	// going over each page of add-ons
	for {
		out, err := c.ListAddons(ctx, &eks.ListAddonsInput{ClusterName: aws.String(cluster), NextToken: token})
		// checking if the list failed, if so its recorded and theres nothing more to read
		if err != nil {
			fail("add-ons", err)
			return
		}
		names = append(names, out.Addons...)
		// checking if theres another page
		if token = out.NextToken; token == nil {
			break
		}
	}
	sort.Strings(names)
	// going over each add-on to describe it
	for _, n := range names {
		d, err := c.DescribeAddon(ctx, &eks.DescribeAddonInput{ClusterName: aws.String(cluster), AddonName: aws.String(n)})
		// checking if the describe failed, if so its recorded and this add-on skipped
		if err != nil {
			fail("add-on "+n, err)
			continue
		}
		// only whether it has configuration is kept, the values can hold secrets and are never kept
		inv.Addons = append(inv.Addons, &snapshotv1alpha1.Addon{
			Name:                  n,
			Version:               aws.ToString(d.Addon.AddonVersion),
			ServiceAccountRoleArn: aws.ToString(d.Addon.ServiceAccountRoleArn),
			HasConfiguration:      strings.TrimSpace(aws.ToString(d.Addon.ConfigurationValues)) != "",
		})
	}
}

// describeVolumesBatch is how many volume ids one DescribeVolumes call asks for, under the API's 500, so one call's response stays small
const describeVolumesBatch = 200

// volumes takes the snapshot's volumes and fills in the EBS details of those with an EBS volume id
func volumes(ctx context.Context, c EC2, vols []*snapshotv1alpha1.Volume, fail func(string, error)) {
	byID := map[string][]*snapshotv1alpha1.Volume{}
	var ids []string
	// going over each volume, grouping them by EBS volume id, since several claims can name the same one
	for _, v := range vols {
		// checking if the handle is an EBS volume id
		if strings.HasPrefix(v.VolumeHandle, "vol-") {
			// checking if this id is new, if so its asked for once
			if _, ok := byID[v.VolumeHandle]; !ok {
				ids = append(ids, v.VolumeHandle)
			}
			byID[v.VolumeHandle] = append(byID[v.VolumeHandle], v)
		}
	}
	// DescribeVolumes takes up to 500 ids, but one missing id fails the call for all of them, so then it falls back to one call per id
	// only then, any other failure, like access denied, would fail every id too, so its recorded once
	// going over the ids in batches
	for start := 0; start < len(ids); start += describeVolumesBatch {
		batch := ids[start:min(start+describeVolumesBatch, len(ids))]
		out, err := c.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: batch})
		// checking if the batch call failed
		if err != nil {
			// invoking notFound and checking if the failure is anything but a missing volume, if so its recorded once for the batch, its defined below
			if !notFound(err) {
				fail(fmt.Sprintf("EBS volumes (%d)", len(batch)), err)
				continue
			}
			// going over each id in the batch, asking for it alone
			for _, id := range batch {
				one, err := c.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{id}})
				// checking if this one failed, if so its recorded and skipped
				if err != nil {
					fail("EBS volume "+id, err)
					continue
				}
				// invoking record to copy the details onto the snapshot's volumes, its defined below
				record(one, byID)
			}
			continue
		}
		// invoking record to copy the details onto the snapshot's volumes, its defined below
		record(out, byID)
	}
}

// notFound tells if err is EC2's error for a volume that doesnt exist
func notFound(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == "InvalidVolume.NotFound"
}

// record takes a DescribeVolumes answer and the snapshot's volumes by id, and copies each described volume's attributes onto the snapshot volumes with its id
func record(out *ec2.DescribeVolumesOutput, byID map[string][]*snapshotv1alpha1.Volume) {
	// going over each described volume
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
		// going over each snapshot volume with this id, they all share the same details
		for _, v := range byID[aws.ToString(ev.VolumeId)] {
			v.Aws = a
		}
	}
}

// annotation takes a manifest's JSON and a key and reads that one annotation, empty when the manifest has none or doesnt parse
func annotation(manifest, key string) string {
	var m struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	// the error is ignored since its a snapshot's manifest, JSON the export wrote
	_ = json.Unmarshal([]byte(manifest), &m)
	return m.Metadata.Annotations[key]
}

// strs takes a policy principal, a string or a list of them, and gives it back as a list, anything else gives nil
func strs(v any) []string {
	// checking if its a single string or a list
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		// going over the list, keeping only its strings
		for _, s := range t {
			// checking if the item is a string
			if str, ok := s.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// short gives back an AWS error's code and message, without the request ids that make every error look different
func short(err error) string {
	var api smithy.APIError
	// checking if its an AWS API error, if not the error is used as is
	if errors.As(err, &api) {
		return api.ErrorCode() + ": " + api.ErrorMessage()
	}
	return err.Error()
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
