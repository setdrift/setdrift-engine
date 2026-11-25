// adds what the nodes, the platform classification and the EKS API say to a profile, the parts read outside the pipeline's objects

package profile

import (
	"cmp"
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
		// invoking nodeZone to read the node's zone, its defined below
		zone := nodeZone(n.GetLabels())
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
		// a count outside what int32 holds isnt a real node's, so its left out
		if pods, err := strconv.ParseInt(stringOf(n.Object, "status", "allocatable", "pods"), 10, 32); err == nil && int32(pods) > b.p.Network.MaxPodsPerNode {
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
	// checking if theyre GKE nodes and nothing named a network plugin, if so its GKE's built-in networking, which runs no component of its own to find
	if b.p.Network.Cni == "" && gkeNodes(nodes) {
		b.p.Network.Cni = "gke-default"
		b.found("network plugin gke-default (GKE's built-in networking, no other network plugin found)", nodesSource+" (cloud.google.com/gke-nodepool label)")
	}
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
		b.p.Network.NodePodPrefix = int32(ranges[0].Bits()) //#nosec G115 -- a prefix's bits are 0 to 128
		b.found("node pod range /"+strconv.Itoa(ranges[0].Bits()), nodesSource+" (spec.podCIDR)")
	}
	// invoking covering to find the range holding them, per address family, its defined in cidr.go
	for _, c := range covering(ranges) {
		b.found("node pod ranges within "+c.String(), nodesSource+" (spec.podCIDR)")
		// checking if no component named the pod network, if so this range stands in for it
		if len(b.p.Network.PodCidrs) == 0 || b.p.Network.PodCidrsFromNodes {
			b.p.Network.PodCidrs = append(b.p.Network.PodCidrs, c.String())
			b.p.Network.PodCidrsFromNodes = true
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

// SubnetIDs gives back the ids of the cloud subnets the profile names, AWS ones (subnet-...) once each, for reading their ranges from EC2
// a load balancer annotation can name a subnet by its Name tag instead, those arent ids and are left out
func SubnetIDs(p *snapshotv1alpha1.Profile) []string {
	var ids []string
	// going through the subnets the profile lists
	for _, s := range p.GetNetwork().GetSubnets() {
		// checking if its an AWS subnet id not yet listed
		if strings.HasPrefix(s.Id, "subnet-") && !slices.Contains(ids, s.Id) {
			ids = append(ids, s.Id)
		}
	}
	slices.Sort(ids)
	return ids
}

// AddSubnetDetails takes a profile and what EC2 says about one subnet and fills in its range, zone and free addresses everywhere the profile lists it
// it gives back the profile, a new one when p is nil
func AddSubnetDetails(p *snapshotv1alpha1.Profile, id, cidr, zone string, availableIPs int32) *snapshotv1alpha1.Profile {
	b := newBuilder(p)
	const src = "EC2 (DescribeSubnets)"
	// going through the subnets the profile lists, one subnet can be listed for more than one use
	for _, s := range b.p.Network.Subnets {
		// checking if its this subnet
		if s.Id != id {
			continue
		}
		s.Cidr = cmp.Or(cidr, s.Cidr)
		s.Zone = cmp.Or(zone, s.Zone)
		s.AvailableIps = availableIPs
	}
	b.found("subnet "+id+" "+cidr+zoneSuffix(zone)+", "+strconv.Itoa(int(availableIPs))+" addresses free", src)
	b.sort()
	return b.p
}

// AddVpcCidrs takes a profile, a VPC id and its address ranges as EC2 gives them, and adds the ranges when its the profile's VPC
// it gives back the profile, a new one when p is nil
func AddVpcCidrs(p *snapshotv1alpha1.Profile, vpcID string, cidrs []string) *snapshotv1alpha1.Profile {
	b := newBuilder(p)
	// checking if its the VPC the profile names, another one is left out
	if vpcID == "" || vpcID != b.p.Network.VpcId {
		return b.p
	}
	// going through the VPC's ranges, IPv4 and IPv6
	for _, c := range cidrs {
		b.add(&b.p.Network.VpcCidrs, c, "VPC "+vpcID+" range", "EC2 (DescribeVpcs)")
	}
	b.sort()
	return b.p
}

// stringOf gives back the string at a path, or "" when theres none
func stringOf(obj map[string]any, path ...string) string {
	s, _, _ := unstructured.NestedString(obj, path...)
	return s
}

// gkeNodes tells if every node is a GKE node, labelled with its node pool
func gkeNodes(nodes []unstructured.Unstructured) bool {
	// going through the nodes, any without the label means its not all GKE
	for _, n := range nodes {
		if n.GetLabels()["cloud.google.com/gke-nodepool"] == "" {
			return false
		}
	}
	return len(nodes) > 0
}

// nodeZone takes a node's labels and gives back its zone, "" when it has none
// aks labels a node outside any zone with its fault domain, a bare number like "0", which isnt a zone, real zones are named like eastus-1
func nodeZone(labels map[string]string) string {
	zone := labels["topology.kubernetes.io/zone"]
	// checking if the zone is only digits, if so its a fault domain and the node has no zone
	if zone != "" && strings.Trim(zone, "0123456789") == "" {
		return ""
	}
	return zone
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
