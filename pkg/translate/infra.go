// the target cluster's create command, written from the source cluster's shape, for RESTORE.md's first step
// SetDrift writes the command, the user runs it with their own credentials, SetDrift creates nothing

package translate

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"regexp"
	"strings"

	"setdrift.io/setdrift-engine/pkg/platform"
)

// Shape is what the source cluster was built as, read from its profile and nodes, what the target's create command starts from
type Shape struct {
	// PodCIDRs and ServiceCIDRs are the source's pod and Service ranges, pod ranges empty when pods took addresses from the node network (the AWS VPC CNI)
	PodCIDRs, ServiceCIDRs []string
	// Nodes is how many nodes ran, Zones how many zones they ran in
	Nodes, Zones int
	// CPU and Memory are the nodes' allocatable totals, as Kubernetes quantities read into millicores and bytes
	MilliCPU, MemoryBytes int64
	// Arm tells if the nodes were arm64
	Arm bool
}

// minorVersion gives back a Kubernetes version's major.minor, like 1.37 from v1.37.1-eks-91f89a9
func minorVersion(v string) string {
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	// checking if theres a major and a minor, if not theres no version to give
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "." + parts[1]
}

// perNode gives back each node's size as whole vCPUs and GiB, rounded up from the allocatable totals to the size a machine is sold in
// allocatable is a little less than the machine, so rounding up finds it again: 1780m is a 2 vCPU machine, 3.0 GiB a 4 GiB one
func (s Shape) perNode() (vcpus, gib int) {
	// checking if the nodes werent read, if so theres no size to give
	if s.Nodes == 0 || s.MilliCPU == 0 || s.MemoryBytes == 0 {
		return 0, 0
	}
	vcpus = int(math.Ceil(float64(s.MilliCPU) / float64(s.Nodes) / 1000))
	g := float64(s.MemoryBytes) / float64(s.Nodes) / (1 << 30)
	gib = 1
	// doubling to the next size machines come in
	for float64(gib) < g {
		gib *= 2
	}
	return vcpus, gib
}

// minorDigits matches a major.minor version, digits only
var minorDigits = regexp.MustCompile(`^\d+\.\d+$`)

// firstPrefix gives back the first range as netip parses and writes it, an error when theres none or it isnt a range
func firstPrefix(cidrs []string) (string, error) {
	// checking if theres a range at all
	if len(cidrs) == 0 {
		return "", errors.New("no range")
	}
	p, err := netip.ParsePrefix(cidrs[0])
	if err != nil {
		return "", err
	}
	return p.String(), nil
}

// dnsServiceIP gives back the cluster DNS address AKS takes in a Service range, the tenth address, as EKS's and kubeadm's DNS use
func dnsServiceIP(cidr string) string {
	p, err := netip.ParsePrefix(cidr)
	// checking if its a range, if not theres no address to give
	if err != nil {
		return ""
	}
	a := p.Addr()
	// stepping ten addresses on
	for range 10 {
		a = a.Next()
	}
	return a.String()
}

// createStep writes RESTORE.md's step that creates the target cluster, for AKS, gives back false for a target it doesnt write one for yet
func createStep(b *strings.Builder, r *Result, ri RestoreInfo) bool {
	// checking if the target is AKS, the only one written so far, the others create their cluster as their docs say
	if r.To != platform.AKS {
		return false
	}
	s := ri.Shape
	lines := []string{"az aks create --resource-group <resource group> --name <new cluster> --location <location> --tier standard"}
	// checking if the source's version is known, and only digits, if so the same minor version is asked for
	if v := minorVersion(ri.SourceVersion); minorDigits.MatchString(v) {
		lines = append(lines, "--kubernetes-version "+v)
	}
	count := max(s.Nodes, 1)
	size := "<a VM size>"
	// invoking perNode for each node's size, its defined above
	if vcpus, gib := s.perNode(); vcpus > 0 {
		size = fmt.Sprintf("<a size with %d vCPUs and %d GiB>", vcpus, gib)
	}
	lines = append(lines, fmt.Sprintf("--node-count %d --node-vm-size %s", count, size))
	// checking if the nodes ran in more than one zone, if so the new ones are spread over three
	if s.Zones > 1 {
		lines = append(lines, "--zones 1 2 3")
	}
	net := "--network-plugin azure --network-plugin-mode overlay"
	// checking if the pods had their own range, if so its kept, written as parsed so nothing else from the cluster reaches the command
	if p, err := firstPrefix(s.PodCIDRs); err == nil {
		net += " --pod-cidr " + p
	}
	// checking if the Service range is known, if so its kept, with DNS at the same address, so nothing that names a Service address changes
	if p, err := firstPrefix(s.ServiceCIDRs); err == nil {
		net += " --service-cidr " + p + " --dns-service-ip " + dnsServiceIP(p)
	}
	lines = append(lines, net)
	// checking if the source enforced its NetworkPolicies, if so the new cluster does too, with Cilium
	if r.NetworkPolicyEngine != "" {
		lines = append(lines, "--network-dataplane cilium --network-policy cilium")
	}
	// checking if identities are kept on their cloud, if so the new cluster needs an OIDC issuer for them to trust
	if len(r.Identities) > 0 {
		lines = append(lines, "--enable-oidc-issuer --enable-workload-identity")
	}
	b.WriteString("Written from the source cluster's shape: the same Kubernetes minor version, as many nodes of the same size, the same Service range and DNS address")
	if r.NetworkPolicyEngine != "" {
		b.WriteString(", network policy enforced as the source did")
	}
	if len(r.Identities) > 0 {
		b.WriteString(", and the OIDC issuer the kept identities trust")
	}
	b.WriteString(". Fill in the names and the VM size, and change what you want different. SetDrift creates nothing, the command is yours to run.\n\n")
	fmt.Fprintf(b, "```sh\n%s\n```\n\n", strings.Join(lines, " \\\n  "))
	// checking if each node's size is known, if so the command that lists Azure's sizes of it follows
	if vcpus, gib := s.perNode(); vcpus > 0 {
		arch := ""
		if s.Arm {
			arch = " The nodes were arm64, so choose an Arm size, a name with a p after the number, like Standard_D2ps_v6."
		}
		fmt.Fprintf(b, "Each node had about %d vCPUs and %d GiB. These sizes have that:%s\n\n```sh\naz vm list-sizes --location <location> --query \"[?numberOfCores==\\`%d\\` && memoryInMB==\\`%d\\`].name\" -o tsv\n```\n\n", vcpus, gib, arch, vcpus, gib*1024)
	}
	b.WriteString("If AKS doesn't offer the version yet, take the newest it does (`az aks get-versions --location <location>`). Then get its context, the `DST` the steps below use.\n\n")
	b.WriteString("```sh\naz aks get-credentials --resource-group <resource group> --name <new cluster> --context <new cluster>\nDST=<new cluster>\n```\n\n")
	return true
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
