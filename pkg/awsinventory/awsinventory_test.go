// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

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

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

type fakeEKS struct{ EKS }

func (fakeEKS) DescribeCluster(context.Context, *eks.DescribeClusterInput, ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	return &eks.DescribeClusterOutput{Cluster: &ekstypes.Cluster{
		Arn: aws.String("arn:aws:eks:us-east-2:111122223333:cluster/prod"), Version: aws.String("1.37"),
		Identity: &ekstypes.Identity{Oidc: &ekstypes.OIDC{Issuer: aws.String("https://oidc.eks.us-east-2.amazonaws.com/id/OLD")}},
	}}, nil
}
func (fakeEKS) ListPodIdentityAssociations(context.Context, *eks.ListPodIdentityAssociationsInput, ...func(*eks.Options)) (*eks.ListPodIdentityAssociationsOutput, error) {
	return &eks.ListPodIdentityAssociationsOutput{Associations: []ekstypes.PodIdentityAssociationSummary{
		{AssociationId: aws.String("a-1"), Namespace: aws.String("external-secrets"), ServiceAccount: aws.String("external-secrets")}}}, nil
}
func (fakeEKS) DescribePodIdentityAssociation(context.Context, *eks.DescribePodIdentityAssociationInput, ...func(*eks.Options)) (*eks.DescribePodIdentityAssociationOutput, error) {
	return &eks.DescribePodIdentityAssociationOutput{Association: &ekstypes.PodIdentityAssociation{RoleArn: aws.String("arn:aws:iam::111122223333:role/eso")}}, nil
}
func (fakeEKS) ListAccessEntries(context.Context, *eks.ListAccessEntriesInput, ...func(*eks.Options)) (*eks.ListAccessEntriesOutput, error) {
	return nil, &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not allowed"}
}
func (fakeEKS) ListAssociatedAccessPolicies(_ context.Context, in *eks.ListAssociatedAccessPoliciesInput, _ ...func(*eks.Options)) (*eks.ListAssociatedAccessPoliciesOutput, error) {
	// two pages
	if in.NextToken == nil {
		return &eks.ListAssociatedAccessPoliciesOutput{NextToken: aws.String("2"),
			AssociatedAccessPolicies: []ekstypes.AssociatedAccessPolicy{{PolicyArn: aws.String("view")}}}, nil
	}
	return &eks.ListAssociatedAccessPoliciesOutput{AssociatedAccessPolicies: []ekstypes.AssociatedAccessPolicy{{PolicyArn: aws.String("edit")}}}, nil
}
func (fakeEKS) ListAddons(context.Context, *eks.ListAddonsInput, ...func(*eks.Options)) (*eks.ListAddonsOutput, error) {
	return &eks.ListAddonsOutput{Addons: []string{"metrics-server"}}, nil
}
func (fakeEKS) DescribeAddon(context.Context, *eks.DescribeAddonInput, ...func(*eks.Options)) (*eks.DescribeAddonOutput, error) {
	return &eks.DescribeAddonOutput{Addon: &ekstypes.Addon{AddonVersion: aws.String("v0.9.0-eksbuild.14"),
		ConfigurationValues: aws.String("replicas: 2\nagent:\n  apiKey: 0123456789abcdef0123456789abcdef\n")}}, nil
}

// entriesEKS lists one access entry, so its policies are read.
type entriesEKS struct{ fakeEKS }

func (entriesEKS) ListAccessEntries(context.Context, *eks.ListAccessEntriesInput, ...func(*eks.Options)) (*eks.ListAccessEntriesOutput, error) {
	return &eks.ListAccessEntriesOutput{AccessEntries: []string{"arn:aws:iam::111122223333:role/admin"}}, nil
}
func (entriesEKS) DescribeAccessEntry(context.Context, *eks.DescribeAccessEntryInput, ...func(*eks.Options)) (*eks.DescribeAccessEntryOutput, error) {
	return &eks.DescribeAccessEntryOutput{AccessEntry: &ekstypes.AccessEntry{Type: aws.String("STANDARD")}}, nil
}

type fakeIAM struct{}

func (fakeIAM) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	doc := `{"Statement":[{"Principal":{"Federated":"arn:aws:iam::111122223333:oidc-provider/oidc.eks.us-east-2.amazonaws.com/id/OLD"}}]}`
	if aws.ToString(in.RoleName) == "eso" {
		doc = `{"Statement":[{"Principal":{"Service":["pods.eks.amazonaws.com"]}}]}`
	}
	return &iam.GetRoleOutput{Role: &iamtypes.Role{AssumeRolePolicyDocument: aws.String(url.QueryEscape(doc))}}, nil
}

type fakeEC2 struct{}

var volumeNotFound = &smithy.GenericAPIError{Code: "InvalidVolume.NotFound", Message: "not found"}

func (fakeEC2) DescribeVolumes(_ context.Context, in *ec2.DescribeVolumesInput, _ ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	if len(in.VolumeIds) > 1 || in.VolumeIds[0] != "vol-0123456789abcdef0" {
		return nil, volumeNotFound
	}
	return &ec2.DescribeVolumesOutput{Volumes: []ec2types.Volume{{VolumeId: aws.String("vol-0123456789abcdef0"), VolumeType: ec2types.VolumeTypeGp3,
		Size: aws.Int32(10), Iops: aws.Int32(3000), Throughput: aws.Int32(125), Encrypted: aws.Bool(true), AvailabilityZone: aws.String("us-east-2a")}}}, nil
}

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
	if len(inv.Addons) != 1 || inv.Addons[0].Version != "v0.9.0-eksbuild.14" ||
		inv.Addons[0].ConfigurationValues != `{"agent":{"apiKey":"SETDRIFT_REDACTED"},"replicas":2}` ||
		!slices.Equal(inv.Addons[0].ConfigurationRedactions, []string{"agent.apiKey"}) {
		t.Errorf("addons %v", inv.Addons)
	}
	if a := snap.Inventory.Volumes[0].Aws; a == nil || a.VolumeType != "gp3" || a.Iops != 3000 || !a.Encrypted {
		t.Errorf("volume %v", a)
	}
	// One error for the access entries, one for the missing volume.
	if len(inv.Errors) != 2 {
		t.Errorf("errors %v", inv.Errors)
	}
}

// Every page of a principal's access policies is read.
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

// Configuration is kept as written unless something is redacted, and replaced
// whole when it can't be parsed.
func TestAddonConfig(t *testing.T) {
	for _, c := range []struct{ in, want, paths string }{
		{"", "", ""},
		{`{"replicas":2}`, `{"replicas":2}`, ""},
		{"replicas: 2\n", "replicas: 2\n", ""},
		{`{"env":[{"name":"API_TOKEN","value":"s3cr3t-v4lue-abcdef"}]}`, `{"env":[{"name":"API_TOKEN","value":"SETDRIFT_REDACTED"}]}`, "env[API_TOKEN].value"},
		{"{not: [valid", "SETDRIFT_REDACTED", ""},
	} {
		got, paths := addonConfig(c.in)
		if got != c.want || strings.Join(paths, ",") != c.paths {
			t.Errorf("%q: %q %v, want %q %q", c.in, got, paths, c.want, c.paths)
		}
	}
}

// Without the permission to describe volumes, one error, not one per volume.
type deniedEC2 struct{ calls *int }

func (d deniedEC2) DescribeVolumes(context.Context, *ec2.DescribeVolumesInput, ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	*d.calls++
	return nil, &smithy.GenericAPIError{Code: "UnauthorizedOperation", Message: "denied"}
}

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

// A Ctrl-C during the AWS read stops the export.
func TestCollectCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Collect(ctx, Clients{EKS: fakeEKS{}, IAM: fakeIAM{}, EC2: fakeEC2{}}, "prod", "us-east-2", &snapshotv1alpha1.Snapshot{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
}
