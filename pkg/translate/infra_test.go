package translate

import (
	"strings"
	"testing"

	"setdrift.io/setdrift-engine/pkg/platform"
)

// check that each node's size rounds up from allocatable to the machine it came from, and DNS takes the range's tenth address
func TestShape(t *testing.T) {
	// four c7i-flex.large nodes: 2 vCPUs and 4 GiB each, 7120m and 12.1Gi allocatable in all
	s := Shape{Nodes: 4, MilliCPU: 7120, MemoryBytes: 13_000_000_000}
	if v, g := s.perNode(); v != 2 || g != 4 {
		t.Errorf("perNode: got %d vCPUs %d GiB, want 2 and 4", v, g)
	}
	if v, g := (Shape{}).perNode(); v != 0 || g != 0 {
		t.Errorf("no nodes read: got %d %d, want 0 0", v, g)
	}
	if ip := dnsServiceIP("172.20.0.0/16"); ip != "172.20.0.10" {
		t.Errorf("dnsServiceIP: got %s", ip)
	}
	if v := minorVersion("v1.37.1-eks-91f89a9"); v != "1.37" {
		t.Errorf("minorVersion: got %s", v)
	}
}

// check the AKS create command keeps the source's version, size, ranges, enforcement and the issuer for kept identities, and other targets get none yet
func TestCreateStep(t *testing.T) {
	r := &Result{From: platform.EKS, To: platform.AKS, NetworkPolicyEngine: "the EKS VPC CNI", Identities: []Identity{{Namespace: "shop", ServiceAccount: "reports"}}}
	ri := RestoreInfo{SourceVersion: "v1.37.1-eks-91f89a9", Shape: Shape{Nodes: 4, Zones: 2, MilliCPU: 7120, MemoryBytes: 13_000_000_000, ServiceCIDRs: []string{"172.20.0.0/16"}}}
	var b strings.Builder
	if !createStep(&b, r, ri) {
		t.Fatal("want a step for AKS")
	}
	for _, want := range []string{"--kubernetes-version 1.37", "--node-count 4 --node-vm-size <a size with 2 vCPUs and 4 GiB>", "--zones 1 2 3",
		"--service-cidr 172.20.0.0/16 --dns-service-ip 172.20.0.10", "--network-policy cilium", "--enable-oidc-issuer --enable-workload-identity", "memoryInMB==\\`4096\\`"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("lacks %q:\n%s", want, b.String())
		}
	}
	if strings.Contains(b.String(), "--pod-cidr") {
		t.Error("the VPC CNI's pods have no range of their own, want no --pod-cidr")
	}
	r.To = platform.GKE
	if createStep(&b, r, ri) {
		t.Error("GKE: want no step yet")
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
