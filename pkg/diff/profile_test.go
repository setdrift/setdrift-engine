package diff

import (
	"reflect"
	"testing"

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// check that a GCP project and an Azure subscription are each side's account, worded by their cloud, like the AWS account
func TestProfileAccount(t *testing.T) {
	for _, tc := range []struct {
		provider, account, kind string
	}{
		{"gcp", "acme-prod", "project"},
		{"azure", "00000000-0000-0000-0000-000000000000", "subscription"},
		{"aws", "111122223333", "account"},
	} {
		s := &snapshotv1alpha1.Snapshot{Inventory: &snapshotv1alpha1.Inventory{Profile: &snapshotv1alpha1.Profile{
			Cloud: &snapshotv1alpha1.ProfileCloud{Provider: tc.provider, Account: tc.account, Region: "west"}}}}
		sd := side(s)
		if sd.Account != tc.account || sd.AccountKind != tc.kind || sd.Region != "west" {
			t.Errorf("%s: got %q %q %q, want %q %q west", tc.provider, sd.AccountKind, sd.Account, sd.Region, tc.kind, tc.account)
		}
	}
}

// check that two profiles list only the properties that differ, components by role, and nothing when a side has no profile
func TestCompareProfiles(t *testing.T) {
	eks := &snapshotv1alpha1.Profile{
		Cloud:      &snapshotv1alpha1.ProfileCloud{Provider: "aws", Region: "us-east-2"},
		Network:    &snapshotv1alpha1.ProfileNetwork{Cni: "aws-vpc-cni", ServiceCidrs: []string{"172.20.0.0/16"}},
		Dns:        &snapshotv1alpha1.ProfileDns{ClusterDomain: "cluster.local"},
		Components: []*snapshotv1alpha1.ProfileComponent{{Role: "dns", Name: "coredns"}, {Role: "cni", Name: "aws-vpc-cni"}},
	}
	aks := &snapshotv1alpha1.Profile{
		Cloud:      &snapshotv1alpha1.ProfileCloud{Provider: "azure", Region: "eastus"},
		Network:    &snapshotv1alpha1.ProfileNetwork{Cni: "cilium", PodCidrs: []string{"10.244.0.0/16"}, ServiceCidrs: []string{"10.0.0.0/16"}},
		Dns:        &snapshotv1alpha1.ProfileDns{ClusterDomain: "cluster.local"},
		Components: []*snapshotv1alpha1.ProfileComponent{{Role: "cni", Name: "cilium"}, {Role: "dns", Name: "coredns"}},
	}
	want := []NodeChange{
		{What: "cloud", A: "aws", B: "azure"},
		{What: "region", A: "us-east-2", B: "eastus"},
		{What: "network plugin", A: "aws-vpc-cni", B: "cilium"},
		{What: "pod ranges", A: "-", B: "10.244.0.0/16"},
		{What: "service ranges", A: "172.20.0.0/16", B: "10.0.0.0/16"},
		{What: "components", A: "cni: aws-vpc-cni, dns: coredns", B: "cni: cilium, dns: coredns"},
	}
	if got := compareProfiles(eks, aks); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	if got := compareProfiles(eks, eks); got != nil {
		t.Errorf("the same profile: got %+v, want nothing", got)
	}
	if got := compareProfiles(eks, nil); got != nil {
		t.Errorf("a side without a profile: got %+v, want nothing", got)
	}
}

// check that values bound to each cluster's own region, like subnet IDs, differ by design between clusters in two regions, and count within one region
func TestRegionBound(t *testing.T) {
	cm := func(subnet string) *snapshotv1alpha1.Resource {
		return res("v1", "ConfigMap", "shop", "net", `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"net","namespace":"shop"},"data":{"SUBNETS":"`+subnet+`"}}`, unmanaged, "")
	}
	east, west := cm("subnet-0a1b2c3d4e5f6a7b8"), cm("subnet-0b2c3d4e5f6a7b8c9")
	r := mustCompare(t, snap("east", "", "us-east-2", east), snap("west", "", "us-west-2", west), Options{})
	if len(r.Changed) != 1 || !r.Changed[0].EnvOnly() || len(r.Changed[0].Reasons) == 0 {
		t.Errorf("two regions: got %+v, want one difference by design", r.Changed)
	}
	r = mustCompare(t, snap("a", "", "us-east-2", east), snap("b", "", "us-east-2", west), Options{})
	if len(r.Changed) != 1 || r.Changed[0].EnvOnly() {
		t.Errorf("one region: got %+v, want a real difference", r.Changed)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
