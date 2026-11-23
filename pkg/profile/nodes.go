// adds what the nodes, the platform classification and the EKS API say to a profile, the parts read outside the pipeline's objects

package profile

import (
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// nodesSource is how evidence from the nodes is named, the nodes themselves arent kept so no one node is named
const nodesSource = "the nodes"

// AddNodes takes a profile and the nodes as read from the API and adds what they say, their zones, address ranges, pod ranges, the most pods one takes and the cloud account in their provider ids
// it gives back the profile, a new one when p is nil
func AddNodes(p *snapshotv1alpha1.Profile, nodes []unstructured.Unstructured) *snapshotv1alpha1.Profile {
	b := newBuilder(p)
	var podRanges []netip.Prefix
	// the nodes internal addresses by zone, "" when a node has no zone label
	addrs := map[string][]netip.Prefix{}
	// going through each node for what it says about the cluster
	for _, n := range nodes {
		zone := n.GetLabels()["topology.kubernetes.io/zone"]
		// checking if the node has a zone, if so the cloud lists it
		if zone != "" && !slices.Contains(b.p.Cloud.Zones, zone) {
			b.p.Cloud.Zones = append(b.p.Cloud.Zones, zone)
		}
		// going through the node's pod ranges, podCIDRs on dual-stack clusters and podCIDR on older ones
		for _, c := range append(stringList(n.Object, "spec", "podCIDRs"), stringOf(n.Object, "spec", "podCIDR")) {
			// checking if its a range, a node without one leaves it empty
			if pr, err := netip.ParsePrefix(c); err == nil && !slices.Contains(podRanges, pr) {
				podRanges = append(podRanges, pr)
			}
		}
		// going through the node's addresses for its internal ones
		for _, a := range maps(n.Object, "status", "addresses") {
			// checking if its an internal address that parses
			if t, _, _ := unstructured.NestedString(a, "type"); t == "InternalIP" {
				// checking if the address parses, if so its kept as a one-address range
				if ip, err := netip.ParseAddr(stringOf(a, "address")); err == nil {
					addrs[zone] = append(addrs[zone], netip.PrefixFrom(ip, ip.BitLen()))
				}
			}
		}
		// checking if the node takes more pods than any before it
		if pods, err := strconv.Atoi(stringOf(n.Object, "status", "allocatable", "pods")); err == nil && int32(pods) > b.p.Network.MaxPodsPerNode {
			b.p.Network.MaxPodsPerNode = int32(pods)
		}
		// invoking providerAccount to read the cloud account from the node's provider id, its defined below
		b.providerAccount(stringOf(n.Object, "spec", "providerID"))
	}
	// checking if any node took pods, if so the most is evidence too
	if b.p.Network.MaxPodsPerNode > 0 {
		b.found("most pods per node "+strconv.Itoa(int(b.p.Network.MaxPodsPerNode)), nodesSource+" (allocatable pods)")
	}
	// invoking nodePodRanges to read the pod network from the nodes pod ranges, its defined below
	b.nodePodRanges(podRanges)
	// going through the zones, giving each the smallest range holding its nodes addresses
	for zone, list := range addrs {
		// invoking covering for each address family, its defined in cidr.go
		for _, c := range covering(list) {
			b.p.Network.NodeRanges = append(b.p.Network.NodeRanges, &snapshotv1alpha1.ProfileSubnet{Cidr: c.String(), Zone: zone, Use: "nodes"})
			b.found("node addresses within "+c.String()+zoneSuffix(zone), nodesSource+" (internal addresses)")
		}
	}
	b.sort()
	return b.p
}

// nodePodRanges takes the nodes pod ranges and, when no component named the pod network, gives the smallest range holding them all
// that range is the part of the pod network the nodes use now, the network itself is that size or larger
func (b *builder) nodePodRanges(ranges []netip.Prefix) {
	// checking if any node had a pod range
	if len(ranges) == 0 {
		return
	}
	// checking if no component said how big a node's range is, if so the nodes say it
	if b.p.Network.NodePodPrefix == 0 {
		b.p.Network.NodePodPrefix = int32(ranges[0].Bits())
		b.found("node pod range /"+strconv.Itoa(ranges[0].Bits()), nodesSource+" (spec.podCIDR)")
	}
	// invoking covering to find the range holding them, per address family, its defined in cidr.go
	for _, c := range covering(ranges) {
		b.found("node pod ranges within "+c.String(), nodesSource+" (spec.podCIDR)")
		// checking if no component named the pod network, if so this range stands in for it
		if len(b.p.Network.PodCidrs) == 0 {
			b.p.Network.PodCidrs = append(b.p.Network.PodCidrs, c.String())
		}
	}
}

// providerAccount takes a node's provider id and reads the cloud account it names, the GCP project or the Azure subscription and resource group
// an AWS provider id names only the zone and instance, the account comes from the EKS ARN or --aws
func (b *builder) providerAccount(id string) {
	const src = nodesSource + " (spec.providerID)"
	// checking which cloud the provider id is from
	switch {
	case strings.HasPrefix(id, "gce://"):
		// gce://<project>/<zone>/<instance>
		project, _, _ := strings.Cut(strings.TrimPrefix(id, "gce://"), "/")
		b.set(&b.p.Cloud.Account, project, "GCP project", src)
	case strings.HasPrefix(id, "azure://"):
		// azure:///subscriptions/<subscription>/resourceGroups/<group>/providers/...
		parts := strings.Split(strings.TrimPrefix(id, "azure://"), "/")
		// going through the parts for the subscription and the resource group, each followed by its value
		for i := 0; i+1 < len(parts); i++ {
			// checking which part this is
			switch strings.ToLower(parts[i]) {
			case "subscriptions":
				b.set(&b.p.Cloud.Account, parts[i+1], "Azure subscription", src)
			case "resourcegroups":
				b.set(&b.p.Cloud.ResourceGroup, parts[i+1], "Azure resource group", src)
			}
		}
	}
}

// SetPlatform takes a profile and the snapshot's cluster and fills in the cloud and region its platform classification found, and on EKS the account and region in the cluster's ARN
// a value already set is kept, it gives back the profile, a new one when p is nil
func SetPlatform(p *snapshotv1alpha1.Profile, c *snapshotv1alpha1.Cluster) *snapshotv1alpha1.Profile {
	b := newBuilder(p)
	// checking if theres a classification
	if cls := c.GetClassification(); cls != nil {
		const src = "the platform classification"
		b.set(&b.p.Cloud.Provider, cls.GetInfrastructure(), "cloud", src)
		b.set(&b.p.Cloud.Region, cls.GetRegion(), "region", src)
	}
	// checking if the cluster's provider id is an EKS ARN, arn:aws:eks:<region>:<account>:cluster/<name>
	if parts := strings.Split(c.GetProviderId(), ":"); len(parts) >= 6 && parts[0] == "arn" && parts[2] == "eks" {
		const src = "the EKS cluster ARN"
		b.set(&b.p.Cloud.Provider, "aws", "cloud", src)
		b.set(&b.p.Cloud.Region, parts[3], "region", src)
		b.set(&b.p.Cloud.Account, parts[4], "AWS account", src)
	}
	b.sort()
	return b.p
}

// EKS is what the EKS API says about a cluster's network, from DescribeCluster, which export --aws already reads
type EKS struct {
	// the cluster's account, from its ARN
	Account   string
	Region    string
	VpcID     string
	SubnetIDs []string
	// the security groups for the control plane's network interfaces, and the one EKS creates for the cluster
	SecurityGroupIDs       []string
	ClusterSecurityGroupID string
	ServiceIPv4CIDR        string
	ServiceIPv6CIDR        string
	// ipv4 or ipv6
	IPFamily              string
	EndpointPublicAccess  bool
	EndpointPrivateAccess bool
	PublicAccessCIDRs     []string
}

// AddEKS takes a profile and what the EKS API says about the cluster and adds it, the cloud account, the VPC and its subnets and the API endpoint
// it gives back the profile, a new one when p is nil
func AddEKS(p *snapshotv1alpha1.Profile, e EKS) *snapshotv1alpha1.Profile {
	b := newBuilder(p)
	const src = "the EKS API (DescribeCluster)"
	b.set(&b.p.Cloud.Provider, "aws", "cloud", src)
	b.set(&b.p.Cloud.Account, e.Account, "AWS account", src)
	b.set(&b.p.Cloud.Region, e.Region, "region", src)
	b.set(&b.p.Network.VpcId, e.VpcID, "VPC", src)
	// going through the subnets the cluster was created with, where EKS puts the control plane's network interfaces
	for _, s := range e.SubnetIDs {
		b.addSubnet(&snapshotv1alpha1.ProfileSubnet{Id: s, Use: "control-plane"}, src)
	}
	// going through the security groups, the cluster's own one too
	for _, sg := range append(slices.Clone(e.SecurityGroupIDs), e.ClusterSecurityGroupID) {
		b.add(&b.p.Network.SecurityGroups, sg, "security group", src)
	}
	b.add(&b.p.Network.ServiceCidrs, e.ServiceIPv4CIDR, "service network", src)
	b.add(&b.p.Network.ServiceCidrs, e.ServiceIPv6CIDR, "service network", src)
	b.add(&b.p.Network.IpFamilies, strings.ToLower(e.IPFamily), "IP family", src)
	// checking which endpoints the API server has, public, private or both
	var access []string
	if e.EndpointPublicAccess {
		access = append(access, "public")
	}
	if e.EndpointPrivateAccess {
		access = append(access, "private")
	}
	b.set(&b.p.Network.ApiEndpointAccess, strings.Join(access, " and "), "API endpoint", src)
	// checking if the public endpoint is open, if so the ranges allowed to reach it matter
	if e.EndpointPublicAccess {
		// going through the ranges allowed to reach the public endpoint
		for _, c := range e.PublicAccessCIDRs {
			b.add(&b.p.Network.ApiPublicAccessCidrs, c, "API public access from", src)
		}
	}
	b.sort()
	return b.p
}

// stringOf gives back the string at a path, or "" when theres none
func stringOf(obj map[string]any, path ...string) string {
	s, _, _ := unstructured.NestedString(obj, path...)
	return s
}

// zoneSuffix gives back " (zone)" for a zone, or "" when theres none
func zoneSuffix(zone string) string {
	// checking if theres a zone to name
	if zone == "" {
		return ""
	}
	return " (" + zone + ")"
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
