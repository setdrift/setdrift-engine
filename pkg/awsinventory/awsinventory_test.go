// tests the AWS inventory against fake EKS, IAM and EC2 clients, so no AWS account is needed

package awsinventory

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/smithy-go"
	"google.golang.org/protobuf/encoding/protojson"

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// fakeEKS answers like a small EKS cluster, with access entries denied
type fakeEKS struct{ EKS }

// DescribeCluster gives back the prod cluster with its ARN, version and OIDC issuer
func (fakeEKS) DescribeCluster(context.Context, *eks.DescribeClusterInput, ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	return &eks.DescribeClusterOutput{Cluster: &ekstypes.Cluster{
		Arn: aws.String("arn:aws:eks:us-east-2:111122223333:cluster/prod"), Version: aws.String("1.37"),
		Identity: &ekstypes.Identity{Oidc: &ekstypes.OIDC{Issuer: aws.String("https://oidc.eks.us-east-2.amazonaws.com/id/OLD")}},
	}}, nil
}

// ListPodIdentityAssociations lists one association, for external-secrets
func (fakeEKS) ListPodIdentityAssociations(context.Context, *eks.ListPodIdentityAssociationsInput, ...func(*eks.Options)) (*eks.ListPodIdentityAssociationsOutput, error) {
	return &eks.ListPodIdentityAssociationsOutput{Associations: []ekstypes.PodIdentityAssociationSummary{
		{AssociationId: aws.String("a-1"), Namespace: aws.String("external-secrets"), ServiceAccount: aws.String("external-secrets")}}}, nil
}

// DescribePodIdentityAssociation gives the association the eso role
func (fakeEKS) DescribePodIdentityAssociation(context.Context, *eks.DescribePodIdentityAssociationInput, ...func(*eks.Options)) (*eks.DescribePodIdentityAssociationOutput, error) {
	return &eks.DescribePodIdentityAssociationOutput{Association: &ekstypes.PodIdentityAssociation{RoleArn: aws.String("arn:aws:iam::111122223333:role/eso")}}, nil
}

// ListAccessEntries is denied, to check a failed call is recorded
func (fakeEKS) ListAccessEntries(context.Context, *eks.ListAccessEntriesInput, ...func(*eks.Options)) (*eks.ListAccessEntriesOutput, error) {
	return nil, &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not allowed"}
}

// ListAssociatedAccessPolicies gives back two pages, view then edit
func (fakeEKS) ListAssociatedAccessPolicies(_ context.Context, in *eks.ListAssociatedAccessPoliciesInput, _ ...func(*eks.Options)) (*eks.ListAssociatedAccessPoliciesOutput, error) {
	// two pages
	if in.NextToken == nil {
		return &eks.ListAssociatedAccessPoliciesOutput{NextToken: aws.String("2"),
			AssociatedAccessPolicies: []ekstypes.AssociatedAccessPolicy{{PolicyArn: aws.String("view")}}}, nil
	}
	return &eks.ListAssociatedAccessPoliciesOutput{AssociatedAccessPolicies: []ekstypes.AssociatedAccessPolicy{{PolicyArn: aws.String("edit")}}}, nil
}

// ListAddons lists the metrics-server add-on
func (fakeEKS) ListAddons(context.Context, *eks.ListAddonsInput, ...func(*eks.Options)) (*eks.ListAddonsOutput, error) {
	return &eks.ListAddonsOutput{Addons: []string{"metrics-server"}}, nil
}

// DescribeAddon gives the add-on configuration values that hold an API key
func (fakeEKS) DescribeAddon(context.Context, *eks.DescribeAddonInput, ...func(*eks.Options)) (*eks.DescribeAddonOutput, error) {
	return &eks.DescribeAddonOutput{Addon: &ekstypes.Addon{AddonVersion: aws.String("v0.9.0-eksbuild.14"),
		ConfigurationValues: aws.String("replicas: 2\nagent:\n  apiKey: 0123456789abcdef0123456789abcdef\n")}}, nil
}

// entriesEKS is fakeEKS listing one access entry, so its policies are read
type entriesEKS struct{ fakeEKS }

// ListAccessEntries lists the admin role's entry
func (entriesEKS) ListAccessEntries(context.Context, *eks.ListAccessEntriesInput, ...func(*eks.Options)) (*eks.ListAccessEntriesOutput, error) {
	return &eks.ListAccessEntriesOutput{AccessEntries: []string{"arn:aws:iam::111122223333:role/admin"}}, nil
}

// DescribeAccessEntry describes the entry as STANDARD
func (entriesEKS) DescribeAccessEntry(context.Context, *eks.DescribeAccessEntryInput, ...func(*eks.Options)) (*eks.DescribeAccessEntryOutput, error) {
	return &eks.DescribeAccessEntryOutput{AccessEntry: &ekstypes.AccessEntry{Type: aws.String("STANDARD")}}, nil
}

// fakeIAM answers GetRole with a trust policy for IRSA, or for Pod Identity for the eso role
type fakeIAM struct{}

// GetRole gives back the role's URL-encoded trust policy, as IAM does
func (fakeIAM) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	doc := `{"Statement":[{"Principal":{"Federated":"arn:aws:iam::111122223333:oidc-provider/oidc.eks.us-east-2.amazonaws.com/id/OLD"}}]}`
	if aws.ToString(in.RoleName) == "eso" {
		doc = `{"Statement":[{"Principal":{"Service":["pods.eks.amazonaws.com"]}}]}`
	}
	return &iam.GetRoleOutput{Role: &iamtypes.Role{AssumeRolePolicyDocument: aws.String(url.QueryEscape(doc))}}, nil
}

// fakeEC2 knows only volume vol-0123456789abcdef0
type fakeEC2 struct{}

// volumeNotFound is EC2's error for a volume that doesnt exist
var volumeNotFound = &smithy.GenericAPIError{Code: "InvalidVolume.NotFound", Message: "not found"}

// DescribeVolumes fails like EC2 for a batch or any other volume, and describes the one it knows
func (fakeEC2) DescribeVolumes(_ context.Context, in *ec2.DescribeVolumesInput, _ ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	if len(in.VolumeIds) > 1 || in.VolumeIds[0] != "vol-0123456789abcdef0" {
		return nil, volumeNotFound
	}
	return &ec2.DescribeVolumesOutput{Volumes: []ec2types.Volume{{VolumeId: aws.String("vol-0123456789abcdef0"), VolumeType: ec2types.VolumeTypeGp3,
		Size: aws.Int32(10), Iops: aws.Int32(3000), Throughput: aws.Int32(125), Encrypted: aws.Bool(true), AvailabilityZone: aws.String("us-east-2a")}}}, nil
}

// check that Collect reads the cluster, Pod Identity, IRSA and Pod Identity roles, add-ons without their values and volumes, and records the failed calls
func TestCollect(t *testing.T) {
	snap := &snapshotv1alpha1.Snapshot{
		Cluster: &snapshotv1alpha1.Cluster{Name: "prod"},
		Resources: []*snapshotv1alpha1.Resource{{ApiVersion: "v1", Kind: "ServiceAccount", Namespace: "shop", Name: "web",
			ManifestJson: `{"metadata":{"annotations":{"eks.amazonaws.com/role-arn":"arn:aws:iam::111122223333:role/web"}}}`}},
		Inventory: &snapshotv1alpha1.Inventory{Volumes: []*snapshotv1alpha1.Volume{
			{Claim: "a", VolumeHandle: "vol-0123456789abcdef0"}, {Claim: "b", VolumeHandle: "vol-0ffffffffffffffff"}}},
	}
	if err := Collect(context.Background(), Clients{EKS: fakeEKS{}, IAM: fakeIAM{}, EC2: fakeEC2{}}, "prod", "us-east-2", snap); err != nil {
		t.Fatal(err)
	}
	inv := snap.Inventory.Aws
	if inv.AccountId != "111122223333" || inv.OidcIssuer == "" || snap.Cluster.ProviderId == "" || snap.Cluster.Platform != "eks" {
		t.Errorf("cluster %v %v", inv, snap.Cluster)
	}
	if len(inv.PodIdentityAssociations) != 1 || inv.PodIdentityAssociations[0].RoleArn != "arn:aws:iam::111122223333:role/eso" {
		t.Errorf("pod identity %v", inv.PodIdentityAssociations)
	}
	if len(inv.IamRoles) != 2 || !inv.IamRoles[0].TrustsPodIdentity || inv.IamRoles[1].OidcProviders[0] != "oidc.eks.us-east-2.amazonaws.com/id/OLD" {
		t.Errorf("roles %v", inv.IamRoles)
	}
	// the add-on's configuration holds an API key, only that it has configuration is kept
	if len(inv.Addons) != 1 || inv.Addons[0].Version != "v0.9.0-eksbuild.14" || !inv.Addons[0].HasConfiguration {
		t.Errorf("addons %v", inv.Addons)
	}
	if b, _ := protojson.Marshal(snap); strings.Contains(string(b), "0123456789abcdef0123456789abcdef") || strings.Contains(string(b), "replicas") {
		t.Errorf("the add-on's configuration values reached the snapshot: %s", b)
	}
	if a := snap.Inventory.Volumes[0].Aws; a == nil || a.VolumeType != "gp3" || a.Iops != 3000 || !a.Encrypted {
		t.Errorf("volume %v", a)
	}
	// one error for the access entries, one for the missing volume
	if len(inv.Errors) != 2 {
		t.Errorf("errors %v", inv.Errors)
	}
}

// check that every page of a principal's access policies is read
func TestAccessPoliciesPaged(t *testing.T) {
	snap := &snapshotv1alpha1.Snapshot{}
	if err := Collect(context.Background(), Clients{EKS: entriesEKS{}, IAM: fakeIAM{}, EC2: fakeEC2{}}, "prod", "us-east-2", snap); err != nil {
		t.Fatal(err)
	}
	e := snap.Inventory.Aws.AccessEntries
	if len(e) != 1 || len(e[0].Policies) != 2 || e[0].Policies[1].PolicyArn != "edit" {
		t.Errorf("access entries %v", e)
	}
}

// deniedEC2 refuses every DescribeVolumes call and counts them
type deniedEC2 struct{ calls *int }

// DescribeVolumes counts the call and fails with UnauthorizedOperation
func (d deniedEC2) DescribeVolumes(context.Context, *ec2.DescribeVolumesInput, ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	*d.calls++
	return nil, &smithy.GenericAPIError{Code: "UnauthorizedOperation", Message: "denied"}
}

// check that without the permission to describe volumes theres one call and one error, not one per volume
func TestVolumesDeniedOnce(t *testing.T) {
	calls := 0
	snap := &snapshotv1alpha1.Snapshot{Inventory: &snapshotv1alpha1.Inventory{Volumes: []*snapshotv1alpha1.Volume{
		{VolumeHandle: "vol-1"}, {VolumeHandle: "vol-2"}, {VolumeHandle: "vol-3"}}}}
	if err := Collect(context.Background(), Clients{EKS: fakeEKS{}, IAM: fakeIAM{}, EC2: deniedEC2{&calls}}, "prod", "us-east-2", snap); err != nil {
		t.Fatal(err)
	}
	var volumeErrors []string
	for _, e := range snap.Inventory.Aws.Errors {
		if strings.HasPrefix(e, "EBS") {
			volumeErrors = append(volumeErrors, e)
		}
	}
	if calls != 1 || len(volumeErrors) != 1 || volumeErrors[0] != "EBS volumes (3): UnauthorizedOperation: denied" {
		t.Errorf("%d calls, errors %v", calls, volumeErrors)
	}
}

// check that a ctrl-c during the AWS read stops the export
func TestCollectCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Collect(ctx, Clients{EKS: fakeEKS{}, IAM: fakeIAM{}, EC2: fakeEC2{}}, "prod", "us-east-2", &snapshotv1alpha1.Snapshot{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
}

// recordingIAM records the roles GetRole was asked for
type recordingIAM struct{ asked *[]string }

// GetRole records the role name and answers like fakeIAM
func (r recordingIAM) GetRole(ctx context.Context, in *iam.GetRoleInput, opts ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	*r.asked = append(*r.asked, aws.ToString(in.RoleName))
	return fakeIAM{}.GetRole(ctx, in, opts...)
}

// check that a ServiceAccount annotation naming a role in another account doesnt make the export read that role, and the skipped role is recorded
func TestRolesOfOtherAccountsNotRead(t *testing.T) {
	snap := &snapshotv1alpha1.Snapshot{Cluster: &snapshotv1alpha1.Cluster{Name: "prod"},
		Resources: []*snapshotv1alpha1.Resource{
			{ApiVersion: "v1", Kind: "ServiceAccount", Namespace: "shop", Name: "web",
				ManifestJson: `{"metadata":{"annotations":{"eks.amazonaws.com/role-arn":"arn:aws:iam::111122223333:role/web"}}}`},
			{ApiVersion: "v1", Kind: "ServiceAccount", Namespace: "shop", Name: "spy",
				ManifestJson: `{"metadata":{"annotations":{"eks.amazonaws.com/role-arn":"arn:aws:iam::999988887777:role/admin"}}}`},
		}}
	var asked []string
	if err := Collect(context.Background(), Clients{EKS: fakeEKS{}, IAM: recordingIAM{&asked}, EC2: fakeEC2{}}, "prod", "us-east-2", snap); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(asked, "admin") {
		t.Errorf("read a role of another account: asked for %v", asked)
	}
	if errs := strings.Join(snap.GetInventory().GetAws().GetErrors(), "\n"); !strings.Contains(errs, "999988887777") {
		t.Errorf("the skipped role is not recorded: %s", errs)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
