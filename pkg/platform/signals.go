// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"net"
	"net/url"
	"slices"
	"strings"

	"setdrift.io/setdrift-engine/pkg/resource"
)

// Kubeconfig is what the first pass reads: the context's name, its API
// server address and its credential plugin, if any.
type Kubeconfig struct {
	Context  string
	Server   string
	ExecCmd  string
	ExecArgs []string
}

// FromKubeconfig returns the signals in a kubeconfig context. Nothing is
// read from the cluster.
func FromKubeconfig(k Kubeconfig) []Signal {
	var out []Signal
	add := func(d Distribution, i Infrastructure, region string, w int, detail string) {
		out = append(out, Signal{Source: "kubeconfig", Detail: detail, Distribution: d, Infrastructure: i, Region: region, Weight: w})
	}

	host := k.Server
	if u, err := url.Parse(k.Server); err == nil && u.Host != "" {
		host = u.Hostname()
	}
	labels := strings.Split(host, ".")
	switch {
	case strings.HasSuffix(host, ".eks.amazonaws.com") || strings.HasSuffix(host, ".eks.amazonaws.com.cn"):
		// <id>.<shard>.<region>.eks.amazonaws.com
		region := ""
		if i := slices.Index(labels, "eks"); i > 0 {
			region = labels[i-1]
		}
		add(EKS, AWS, region, Strong, "API server "+host)
	case strings.HasSuffix(host, ".azmk8s.io"):
		// <name>-<hash>.hcp.<region>.azmk8s.io, or <name>.privatelink.<region>.azmk8s.io
		region := ""
		for i, l := range labels {
			if (l == "hcp" || l == "privatelink") && i+1 < len(labels) {
				region = labels[i+1]
			}
		}
		add(AKS, Azure, region, Strong, "API server "+host)
	case strings.HasSuffix(host, ".aroapp.io"):
		add(OpenShift, Azure, "", Strong, "API server "+host+" (Azure Red Hat OpenShift)")
	case strings.HasSuffix(host, ".openshiftapps.com"):
		add(OpenShift, UnknownInfra, "", Strong, "API server "+host+" (Red Hat OpenShift service)")
	case host == "localhost" || host == "host.docker.internal" || isLoopback(host):
		add(Unknown, Local, "", Weak, "API server on this machine ("+host+")")
	}

	// The credential plugin: each managed service has its own.
	// the program's name, from a Unix or a Windows path
	cmd := k.ExecCmd
	if i := strings.LastIndexAny(cmd, `/\`); i >= 0 {
		cmd = cmd[i+1:]
	}
	cmd = strings.TrimSuffix(cmd, ".exe")
	args := strings.Join(k.ExecArgs, " ")
	switch {
	case cmd == "aws" && strings.Contains(args, "eks") && strings.Contains(args, "get-token"),
		cmd == "aws-iam-authenticator":
		add(EKS, AWS, "", Strong, "credentials from "+cmd+" (EKS)")
	case cmd == "gke-gcloud-auth-plugin":
		add(GKE, GCP, "", Strong, "credentials from gke-gcloud-auth-plugin")
	case cmd == "kubelogin" && strings.Contains(args, "get-token") && strings.Contains(args, "--server-id"):
		add(AKS, Azure, "", Strong, "credentials from kubelogin (Microsoft Entra ID)")
	}

	// Context names the tools write; anyone can rename a context, so medium.
	ctx := k.Context
	switch {
	case strings.HasPrefix(ctx, "arn:aws") && strings.Contains(ctx, ":eks:"):
		region := ""
		if parts := strings.Split(ctx, ":"); len(parts) > 3 {
			region = parts[3]
		}
		add(EKS, AWS, region, Strong, "context is an EKS cluster ARN")
	case strings.HasPrefix(ctx, "gke_"):
		// gke_<project>_<location>_<name>
		region := ""
		if parts := strings.Split(ctx, "_"); len(parts) >= 4 {
			region = parts[2]
		}
		add(GKE, GCP, region, Medium, "context name "+ctx+" (gcloud's format)")
	case strings.HasPrefix(ctx, "kind-"):
		add(Kind, Local, "", Medium, "context name "+ctx+" (kind's format)")
	case strings.HasPrefix(ctx, "k3d-"):
		add(K3s, Local, "", Medium, "context name "+ctx+" (k3d's format)")
	case ctx == "minikube":
		add(Minikube, Local, "", Medium, "context name minikube")
	case ctx == "docker-desktop":
		add(DockerDesktop, Local, "", Medium, "context name docker-desktop")
	case ctx == "rancher-desktop":
		add(K3s, Local, "", Medium, "context name rancher-desktop")
	}
	return out
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// FromServer returns the signals in the API server's version and API groups.
func FromServer(gitVersion string, groups []string) []Signal {
	var out []Signal
	add := func(d Distribution, i Infrastructure, w int, detail string) {
		out = append(out, Signal{Source: "server", Detail: detail, Distribution: d, Infrastructure: i, Weight: w})
	}
	switch v := gitVersion; {
	case strings.Contains(v, "-eks-"):
		add(EKS, AWS, Strong, "server version "+v)
	case strings.Contains(v, "-gke."):
		add(GKE, GCP, Strong, "server version "+v)
	case strings.Contains(v, "+k3s"):
		add(K3s, UnknownInfra, Strong, "server version "+v)
	case strings.Contains(v, "+rke2"):
		add(RKE2, UnknownInfra, Strong, "server version "+v)
	}
	for _, g := range groups {
		switch g {
		case "eks.amazonaws.com", "vpcresources.k8s.aws":
			add(EKS, AWS, Medium, "API group "+g)
		case "config.openshift.io":
			add(OpenShift, UnknownInfra, Strong, "API group "+g)
		case "networking.gke.io":
			add(GKE, GCP, Medium, "API group "+g)
		case "k3s.cattle.io":
			add(K3s, UnknownInfra, Medium, "API group "+g)
		}
	}
	return out
}

// Node is what the second pass reads from a node: its provider ID and labels.
type Node struct {
	Name       string
	ProviderID string
	Labels     map[string]string
}

// FromNodes returns the signals in a few nodes. The provider ID names the
// infrastructure (aws://, azure://, gce://, kind://); managed services add
// their own node labels, which tell a managed cluster from a self-managed
// one on the same cloud.
func FromNodes(nodes []Node) []Signal {
	var out []Signal
	seen := map[string]bool{}
	add := func(d Distribution, i Infrastructure, region string, w int, detail string) {
		if seen[detail] {
			return // one signal per kind of evidence, however many nodes show it
		}
		seen[detail] = true
		out = append(out, Signal{Source: "node", Detail: detail, Distribution: d, Infrastructure: i, Region: region, Weight: w})
	}
	for _, n := range nodes {
		region := n.Labels["topology.kubernetes.io/region"]
		scheme, _, _ := strings.Cut(n.ProviderID, "://")
		switch scheme {
		case "aws":
			add(Unknown, AWS, region, Strong, "node provider IDs aws://")
		case "azure":
			add(Unknown, Azure, region, Strong, "node provider IDs azure://")
		case "gce":
			add(Unknown, GCP, region, Strong, "node provider IDs gce://")
		case "kind":
			add(Kind, Local, "", Strong, "node provider IDs kind://")
		case "k3s":
			add(K3s, UnknownInfra, "", Strong, "node provider IDs k3s://")
		case "vsphere", "openstack":
			add(Unknown, Other, region, Medium, "node provider IDs "+scheme+"://")
		}
		for _, l := range sortedKeys(n.Labels) {
			switch {
			case strings.HasPrefix(l, "eks.amazonaws.com/"):
				add(EKS, AWS, region, Strong, "node label "+l)
			case strings.HasPrefix(l, "kubernetes.azure.com/"):
				add(AKS, Azure, region, Strong, "node label "+l)
			case strings.HasPrefix(l, "cloud.google.com/gke-"):
				add(GKE, GCP, region, Strong, "node label "+l)
			case strings.HasPrefix(l, "node.openshift.io/"):
				add(OpenShift, UnknownInfra, "", Strong, "node label "+l)
			case strings.HasPrefix(l, "minikube.k8s.io/"):
				add(Minikube, Local, "", Strong, "node label "+l)
			}
		}
	}
	return out
}

// FromObjects returns the signals in collected objects: the network plugin
// and cloud agents in kube-system, the CSI drivers (where the cluster's
// storage lives), and kubeadm's own configuration.
func FromObjects(objs []*resource.Object) []Signal {
	var out []Signal
	seen := map[string]bool{}
	add := func(d Distribution, i Infrastructure, w int, detail string) {
		if !seen[detail] {
			seen[detail] = true
			out = append(out, Signal{Source: "object", Detail: detail, Distribution: d, Infrastructure: i, Weight: w})
		}
	}
	for _, o := range objs {
		gk := o.GroupVersionKind().GroupKind()
		ns, name := o.GetNamespace(), o.GetName()
		switch {
		case gk.Kind == "DaemonSet" && gk.Group == "apps" && ns == "kube-system":
			switch name {
			case "aws-node":
				add(EKS, AWS, Medium, "DaemonSet kube-system/aws-node (Amazon VPC CNI)")
			case "azure-cns", "cloud-node-manager", "azure-ip-masq-agent":
				add(AKS, Azure, Medium, "DaemonSet kube-system/"+name)
			case "kindnet":
				add(Kind, Local, Medium, "DaemonSet kube-system/kindnet")
			}
		case gk.Kind == "CSIDriver" && gk.Group == "storage.k8s.io":
			switch name {
			case "ebs.csi.aws.com", "ebs.csi.eks.amazonaws.com", "efs.csi.aws.com":
				add(Unknown, AWS, Medium, "CSI driver "+name)
			case "disk.csi.azure.com", "file.csi.azure.com":
				add(Unknown, Azure, Medium, "CSI driver "+name)
			case "pd.csi.storage.gke.io", "filestore.csi.storage.gke.io":
				add(Unknown, GCP, Medium, "CSI driver "+name)
			}
		case gk.Kind == "ConfigMap" && gk.Group == "" && ns == "kube-system" && name == "kubeadm-config":
			add(Kubeadm, UnknownInfra, Medium, "ConfigMap kube-system/kubeadm-config (built with kubeadm)")
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
