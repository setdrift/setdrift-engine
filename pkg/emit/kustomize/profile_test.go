// tests the cluster profile README and profile.json, what each shows and that a platform default stays out of profile.json

package kustomize

import (
	"strings"
	"testing"

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// check that a value the cluster didnt give shows its platform's default labelled as not read, a found one shows as is, and profile.json holds only what was found
func TestProfileReadmeDefaults(t *testing.T) {
	s := &snapshotv1alpha1.Snapshot{
		Cluster: &snapshotv1alpha1.Cluster{Name: "prod", Classification: &snapshotv1alpha1.Platform{Distribution: "eks", Confidence: "high"}},
		Inventory: &snapshotv1alpha1.Inventory{Profile: &snapshotv1alpha1.Profile{
			Network: &snapshotv1alpha1.ProfileNetwork{
				VpcId: "vpc-0abc", VpcCidrs: []string{"10.0.0.0/16"},
				Subnets: []*snapshotv1alpha1.ProfileSubnet{
					{Id: "subnet-0a", Cidr: "10.0.0.0/19", Zone: "us-east-1a", Use: "nodes", AvailableIps: 8000},
					{Id: "subnet-0b", Use: "nodes"},
				},
			},
			Dns:      &snapshotv1alpha1.ProfileDns{ClusterDomain: "cluster.local"},
			Evidence: []*snapshotv1alpha1.ProfileEvidence{{Value: "VPC vpc-0abc", Source: "the EKS API (DescribeCluster)"}},
		}},
	}
	readme := profileReadme(s)
	for _, want := range []string{
		"| Service network | not found (the eks default is 10.100.0.0/16 or 172.20.0.0/16, whichever does not overlap the VPC; not read from this cluster) |",
		"| Cluster domain | cluster.local |",
		"| VPC ranges | 10.0.0.0/16 |",
		"| subnet-0a | 10.0.0.0/19 | us-east-1a | nodes | 8000 |",
		"| subnet-0b | - | - | nodes | - |",
		"export\n`--aws` reads it from EC2",
		"| VPC vpc-0abc | the EKS API (DescribeCluster) |",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README lacks %q:\n%s", want, readme)
		}
	}
	j, err := profileJSON(s.Inventory.Profile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(j), "10.100.0.0") || strings.Contains(string(j), "default") {
		t.Errorf("a platform default reached profile.json:\n%s", j)
	}
	if again, _ := profileJSON(s.Inventory.Profile); string(again) != string(j) {
		t.Error("profile.json differs between two runs")
	}

	s.Cluster.Classification.Confidence = "low"
	if readme := profileReadme(s); strings.Contains(readme, "eks default") {
		t.Error("a low-confidence platform's defaults are shown")
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
