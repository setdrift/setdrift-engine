// writes cluster_profile/, the cluster described as infrastructure, its README for people and profile.json for tools

package kustomize

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// ProfileDir is the directory of the cluster profile in each cluster's directory, the directory read skips it as its not manifests
const ProfileDir = "cluster_profile"

// maxHostnames is how many host names the README lists before saying how many more there are, profile.json has every one
const maxHostnames = 50

// notFound is what the README shows for a value the profile has none for, so a gap reads as one
const notFound = "not found"

// writeProfile takes the snapshot and writes cluster_profile/README.md and cluster_profile/profile.json when the snapshot has a profile
func writeProfile(out *outDir, s *snapshotv1alpha1.Snapshot) error {
	p := s.GetInventory().GetProfile()
	// checking if the snapshot has a profile, an older snapshot or one read from files may not
	if p == nil {
		return nil
	}
	// invoking profileJSON to encode it, its defined below
	j, err := profileJSON(p)
	if err != nil {
		return err
	}
	if err := out.write(filepath.Join(ProfileDir, "profile.json"), j); err != nil {
		return err
	}
	// invoking profileReadme to write it out for people, its defined below
	return out.write(filepath.Join(ProfileDir, "README.md"), []byte(profileReadme(s)))
}

// profileJSON encodes a profile as indented JSON with the schema's field names, the same bytes for the same profile
func profileJSON(p *snapshotv1alpha1.Profile) ([]byte, error) {
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(p)
	if err != nil {
		return nil, err
	}
	// protojson varies its spacing on purpose, compacting then indenting gives the same bytes every time
	var compact, out bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, err
	}
	if err := json.Indent(&out, compact.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	// a bytes.Buffer's WriteByte only fails when it cant grow, which panics instead, so its error is always nil
	_ = out.WriteByte('\n')
	return out.Bytes(), nil
}

// profileReadme takes the snapshot and writes its profile out for people, a section per part and then where each value was found
func profileReadme(s *snapshotv1alpha1.Snapshot) string {
	p := s.GetInventory().GetProfile()
	c := s.GetCluster()
	var b strings.Builder
	fmt.Fprintf(&b, "# Cluster profile: %s\n\n", c.GetName())
	b.WriteString("What the cluster is built on and with, as SetDrift found it in the cluster's\n" +
		"system components, their settings, its nodes and, with `--aws`, the EKS API.\n" +
		"It is where recreating the cluster on another platform or cloud starts.\n" +
		"profile.json holds the same for tools, and the last section says where each\n" +
		"value was found. A range found only from what is in use (\"within\") is the\n" +
		"part the cluster uses now: the network it is in is that size or larger.\n")

	cloud := p.GetCloud()
	section(&b, "Cloud")
	row(&b, "Cloud", cloud.GetProvider())
	row(&b, "Account", cloud.GetAccount())
	row(&b, "Region", cloud.GetRegion())
	row(&b, "Zones", strings.Join(cloud.GetZones(), ", "))
	// checking if the cloud has a resource group, only Azure does
	if cloud.GetResourceGroup() != "" {
		row(&b, "Resource group", cloud.GetResourceGroup())
	}
	row(&b, "Platform", platformText(c.GetClassification()))
	row(&b, "Kubernetes", c.GetKubernetesVersion())

	// checking if the nodes were read, credentials that cant list nodes leave the summary out
	if nodes := s.GetInventory().GetNodes(); nodes != nil {
		section(&b, "Compute")
		row(&b, "Nodes", strconv.Itoa(int(nodes.GetCount())))
		row(&b, "Instance types", counts(nodes.GetInstanceTypes()))
		row(&b, "Pools", counts(nodes.GetPools()))
		row(&b, "OS and CPU", counts(nodes.GetPlatforms()))
		row(&b, "Kubelet versions", counts(nodes.GetKubeletVersions()))
		row(&b, "Allocatable", fmt.Sprintf("%s CPU, %s memory, %d pods", nodes.GetCpu(), nodes.GetMemory(), nodes.GetPods()))
	}

	n := p.GetNetwork()
	section(&b, "Network")
	dist := knownDistribution(c.GetClassification())
	pods := strings.Join(n.GetPodCidrs(), ", ")
	// checking if the pod network is only what the nodes use now, if so it reads as a lower bound
	if n.GetPodCidrsFromNodes() && pods != "" {
		pods = "within " + pods + " (the nodes' pod ranges; the pod network is that size or larger)"
	}
	row(&b, "Pod network", orDefault(pods, dist, "pod network"))
	// checking if the node pod range is known
	if n.GetNodePodPrefix() > 0 {
		row(&b, "Each node's pod range", "/"+strconv.Itoa(int(n.GetNodePodPrefix())))
	}
	row(&b, "Most pods per node", positive(n.GetMaxPodsPerNode()))
	row(&b, "Service network", orDefault(strings.Join(n.GetServiceCidrs(), ", "), dist, "service network"))
	row(&b, "IP families", strings.Join(n.GetIpFamilies(), ", "))
	row(&b, "Node addresses", ranges(n.GetNodeRanges()))
	row(&b, "Network plugin (CNI)", n.GetCni())
	row(&b, "CNI settings", strings.Join(n.GetCniSettings(), ", "))
	row(&b, "kube-proxy mode", n.GetKubeProxyMode())
	row(&b, "VPC / network", n.GetVpcId())
	// checking if the VPC's ranges were read, only with --aws
	if len(n.GetVpcCidrs()) > 0 {
		row(&b, "VPC ranges", strings.Join(n.GetVpcCidrs(), ", "))
	}
	row(&b, "Security groups", strings.Join(n.GetSecurityGroups(), ", "))
	row(&b, "API endpoint", n.GetApiEndpointAccess())
	// checking if the public endpoint has ranges allowed to reach it
	if len(n.GetApiPublicAccessCidrs()) > 0 {
		row(&b, "API public access from", strings.Join(n.GetApiPublicAccessCidrs(), ", "))
	}
	// checking if any subnets were found, if so they get a table of their own
	if len(n.GetSubnets()) > 0 {
		b.WriteString("\n| Subnet | Range | Zone | Holds | Free addresses |\n|---|---|---|---|---|\n")
		// going through the subnets
		for _, sn := range n.GetSubnets() {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", cmp.Or(sn.GetId(), "-"), cmp.Or(sn.GetCidr(), "-"), cmp.Or(sn.GetZone(), "-"), sn.GetUse(), cmp.Or(positive(sn.GetAvailableIps()), "-"))
		}
		// checking if any subnet still lacks its range, if so the README says how to get it
		if slices.ContainsFunc(n.GetSubnets(), func(sn *snapshotv1alpha1.ProfileSubnet) bool { return sn.GetCidr() == "" }) {
			b.WriteString("\nA subnet's range is held by the cloud, not the cluster. On EKS, export\n`--aws` reads it from EC2.\n")
		}
	}

	d := p.GetDns()
	section(&b, "DNS")
	row(&b, "Cluster domain", orDefault(d.GetClusterDomain(), dist, "cluster domain"))
	row(&b, "Cluster DNS address", orDefault(d.GetClusterDnsIp(), dist, "cluster DNS address"))
	row(&b, "Upstreams", strings.Join(d.GetUpstreams(), ", "))
	row(&b, "Managed domains (external-dns)", strings.Join(d.GetManagedDomains(), ", "))
	row(&b, "Host names served", hostnames(d.GetHostnames()))

	section(&b, "Storage")
	// checking if any storage classes were found
	if len(p.GetStorageClasses()) == 0 {
		b.WriteString("No StorageClasses found.\n")
	} else {
		b.WriteString("| Class | Provisioner | Default | Claims | Settings |\n|---|---|---|---|---|\n")
		// going through the classes
		for _, sc := range p.GetStorageClasses() {
			def := ""
			// checking if its the default class
			if sc.GetDefault() {
				def = "yes"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %d | %s |\n", sc.GetName(), sc.GetProvisioner(), def, sc.GetClaims(), classSettings(sc))
		}
	}

	section(&b, "Components")
	// checking if any components were recognized
	if len(p.GetComponents()) == 0 {
		b.WriteString("None of the components SetDrift recognizes were found.\n")
	} else {
		b.WriteString("| Role | Component | Version | Runs as |\n|---|---|---|---|\n")
		// going through the components
		for _, comp := range p.GetComponents() {
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", comp.GetRole(), comp.GetName(), cmp.Or(comp.GetVersion(), "-"), comp.GetWorkload())
		}
	}

	section(&b, "Where each value was found")
	b.WriteString("| Found | Where |\n|---|---|\n")
	// going through the evidence
	for _, e := range p.GetEvidence() {
		fmt.Fprintf(&b, "| %s | %s |\n", escapeCell(e.GetValue()), escapeCell(e.GetSource()))
	}
	return b.String()
}

// platformDefaults are each platform's defaults for the values the cluster doesnt always say, shown only for a value not found and only as that platform's default
// theyre never written to profile.json, which holds only what was read
var platformDefaults = map[string]map[string]string{
	"eks": {
		"pod network":         "the VPC subnets: with the VPC CNI pods take node subnet addresses",
		"service network":     "10.100.0.0/16 or 172.20.0.0/16, whichever does not overlap the VPC",
		"cluster DNS address": "10.100.0.10 or 172.20.0.10, the tenth address of the service network",
		"cluster domain":      "cluster.local",
	},
	"aks": {
		"pod network":         "10.244.0.0/16 (Azure CNI Overlay)",
		"service network":     "10.0.0.0/16",
		"cluster DNS address": "10.0.0.10",
		"cluster domain":      "cluster.local",
	},
	"gke": {
		"pod network":     "a secondary range of the node subnet, a /24 per node",
		"service network": "a secondary range GKE picks",
		"cluster domain":  "cluster.local",
	},
	"kind": {
		"pod network":         "10.244.0.0/16",
		"service network":     "10.96.0.0/16",
		"cluster DNS address": "10.96.0.10",
		"cluster domain":      "cluster.local",
	},
	"kubeadm": {
		"pod network":         "none, the network plugin sets it",
		"service network":     "10.96.0.0/12",
		"cluster DNS address": "10.96.0.10",
		"cluster domain":      "cluster.local",
	},
	"minikube": {
		"pod network":         "10.244.0.0/16",
		"service network":     "10.96.0.0/12",
		"cluster DNS address": "10.96.0.10",
		"cluster domain":      "cluster.local",
	},
	"k3s": {
		"pod network":         "10.42.0.0/16",
		"service network":     "10.43.0.0/16",
		"cluster DNS address": "10.43.0.10",
		"cluster domain":      "cluster.local",
	},
	"rke2": {
		"pod network":         "10.42.0.0/16",
		"service network":     "10.43.0.0/16",
		"cluster DNS address": "10.43.0.10",
		"cluster domain":      "cluster.local",
	},
}

// knownDistribution gives back the platform the classification found when its sure enough to act on, medium or high confidence, else ""
func knownDistribution(c *snapshotv1alpha1.Platform) string {
	// checking the confidence, a low one is a guess and its defaults would be too
	if c.GetConfidence() != "high" && c.GetConfidence() != "medium" {
		return ""
	}
	return c.GetDistribution()
}

// orDefault gives back a value, or when theres none and the platform has a default for it, notFound with that default named as the platform's, not read from the cluster
func orDefault(value, dist, what string) string {
	// checking if theres a value, or no default to name
	def := platformDefaults[dist][what]
	if value != "" || def == "" {
		return value
	}
	return notFound + " (the " + dist + " default is " + def + "; not read from this cluster)"
}

// section starts a section of the README, rows of a two-column table follow it unless it writes its own
func section(b *strings.Builder, title string) {
	fmt.Fprintf(b, "\n## %s\n\n", title)
}

// row writes one row of a section's table, the header first when its the section's first row, and notFound for an empty value
func row(b *strings.Builder, name, value string) {
	// checking if this is the section's first row, the section heading ends in a blank line, if so the table header goes first
	if strings.HasSuffix(b.String(), "\n\n") {
		b.WriteString("| | |\n|---|---|\n")
	}
	fmt.Fprintf(b, "| %s | %s |\n", name, escapeCell(cmp.Or(value, notFound)))
}

// platformText takes the platform classification and gives back how it reads, like "eks, managed (high confidence)"
func platformText(c *snapshotv1alpha1.Platform) string {
	// checking if the classification found a distribution
	if c.GetDistribution() == "" {
		return ""
	}
	parts := []string{c.GetDistribution()}
	// checking if its known whether the platform is managed
	if c.GetManaged() != "" {
		parts = append(parts, c.GetManaged())
	}
	out := strings.Join(parts, ", ")
	// checking if the confidence is known
	if c.GetConfidence() != "" {
		out += " (" + c.GetConfidence() + " confidence)"
	}
	return out
}

// counts takes counts by name and gives back how they read, like "m6g.large 3, m6g.xlarge 1", in name order
func counts(m map[string]int32) string {
	var parts []string
	// going through the names in order
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

// ranges takes the node address ranges and gives back how they read, like "within 10.0.32.0/20 (us-east-1a)"
func ranges(rs []*snapshotv1alpha1.ProfileSubnet) string {
	var parts []string
	// going through the ranges
	for _, r := range rs {
		part := "within " + r.GetCidr()
		// checking if the range is for one zone
		if r.GetZone() != "" {
			part += " (" + r.GetZone() + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// hostnames takes the host names served and gives back how they read, the first maxHostnames and how many more there are
func hostnames(hs []string) string {
	// checking if there are more than the README lists
	if len(hs) > maxHostnames {
		return strings.Join(hs[:maxHostnames], ", ") + fmt.Sprintf(" and %d more (profile.json lists them all)", len(hs)-maxHostnames)
	}
	return strings.Join(hs, ", ")
}

// classSettings takes a storage class and gives back its settings, the parameters, reclaim policy, binding mode and expansion, as one line
func classSettings(sc *snapshotv1alpha1.ProfileStorageClass) string {
	var parts []string
	// going through the parameters in name order
	for _, k := range slices.Sorted(maps.Keys(sc.GetParameters())) {
		parts = append(parts, k+"="+sc.GetParameters()[k])
	}
	// checking each of the other settings, an unset one is left out
	if sc.GetReclaimPolicy() != "" {
		parts = append(parts, "reclaim "+sc.GetReclaimPolicy())
	}
	if sc.GetVolumeBindingMode() != "" {
		parts = append(parts, "binding "+sc.GetVolumeBindingMode())
	}
	if sc.GetAllowVolumeExpansion() {
		parts = append(parts, "expandable")
	}
	return escapeCell(strings.Join(parts, ", "))
}

// positive gives back a number as text, or "" for zero so the README shows notFound
func positive(n int32) string {
	// checking if theres a number to show
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(int(n))
}

// escapeCell keeps a value from breaking a Markdown table, a pipe is escaped and a line break becomes a space
func escapeCell(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ").Replace(s)
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
