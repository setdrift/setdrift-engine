// finds the system components a cluster runs by the images of its workloads, and reads the settings some of them carry in their args and env

package profile

import (
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// known is one component the profile recognizes, by a part of its image
type known struct {
	// what the component does in the cluster, see ProfileComponent's role in the snapshot schema
	role string
	// the name the profile gives it
	name string
	// what the image path must be, without its registry and tag, a pattern with a slash is looked for anywhere in the path, one without must be the last part
	image string
}

// components are the components the profile recognizes, a few per role, the ones that shape what recreating a cluster needs
var components = []known{
	{"cni", "aws-vpc-cni", "amazon-k8s-cni"},
	{"cni", "cilium", "cilium"},
	{"cni", "calico", "calico/node"},
	{"cni", "flannel", "flannel"},
	{"cni", "kindnet", "kindnetd"},
	{"cni", "azure-cni", "azure-cns"},
	{"cni", "gke-dataplane-v2", "anetd"},
	{"cni", "weave", "weave-kube"},
	{"kube-proxy", "kube-proxy", "kube-proxy"},
	{"dns", "coredns", "coredns"},
	{"dns-records", "external-dns", "external-dns"},
	{"ingress", "ingress-nginx", "ingress-nginx/controller"},
	{"ingress", "traefik", "traefik"},
	{"ingress", "contour", "contour"},
	{"ingress", "envoy-gateway", "envoyproxy/gateway"},
	{"ingress", "kong", "kong"},
	{"ingress", "haproxy-ingress", "haproxy-ingress"},
	{"load-balancer", "aws-load-balancer-controller", "aws-load-balancer-controller"},
	{"service-mesh", "istio", "istio/pilot"},
	{"service-mesh", "linkerd", "linkerd/controller"},
	{"certificates", "cert-manager", "cert-manager-controller"},
	{"node-autoscaling", "cluster-autoscaler", "cluster-autoscaler"},
	{"node-autoscaling", "karpenter", "karpenter/controller"},
	{"autoscaling", "metrics-server", "metrics-server"},
	{"autoscaling", "keda", "kedacore/keda"},
	{"autoscaling", "vertical-pod-autoscaler", "vpa-recommender"},
	{"secrets", "external-secrets", "external-secrets"},
	{"secrets", "secrets-store-csi-driver", "secrets-store-csi-driver"},
	{"secrets", "vault", "hashicorp/vault"},
	{"storage", "aws-ebs-csi-driver", "aws-ebs-csi-driver"},
	{"storage", "aws-efs-csi-driver", "aws-efs-csi-driver"},
	{"storage", "gcp-pd-csi-driver", "gcp-compute-persistent-disk-csi-driver"},
	{"storage", "azuredisk-csi-driver", "azuredisk-csi"},
	{"storage", "longhorn", "longhorn-manager"},
	{"storage", "rook-ceph", "rook/ceph"},
	{"storage", "local-path-provisioner", "local-path-provisioner"},
	{"gitops", "argo-cd", "argoproj/argocd"},
	{"gitops", "flux", "fluxcd/source-controller"},
	{"observability", "prometheus-operator", "prometheus-operator"},
	{"observability", "datadog", "datadog/agent"},
	{"observability", "opentelemetry-collector", "opentelemetry-collector"},
	{"observability", "fluent-bit", "fluent-bit"},
	{"observability", "grafana-alloy", "grafana/alloy"},
	{"policy", "kyverno", "kyverno/kyverno"},
	{"policy", "gatekeeper", "openpolicyagent/gatekeeper"},
	{"backup", "velero", "velero/velero"},
}

// container is what the profile reads from one container of a pod template
type container struct {
	image string
	args  []string
	env   map[string]string
}

// workload takes a Deployment, DaemonSet or StatefulSet and lists the components its containers run, then reads the settings of the ones that keep them in args or env
func (b *builder) workload(o *resource.Object, src string) {
	// going through each container of the pod template, init containers too since some CNIs run their setup in one
	for _, c := range templateContainers(o) {
		// invoking recognize to find which component the image is, its defined below
		k, version, ok := recognize(c.image)
		// checking if the image is one the profile knows
		if !ok {
			continue
		}
		// invoking addComponent to list it once, its defined below
		b.addComponent(k, version, src)
		// invoking settings to read what the component says about the cluster, its defined below
		b.settings(k.name, c, src)
	}
}

// addComponent lists a component once, the first workload found running it is the one named
func (b *builder) addComponent(k known, version, src string) {
	// checking if the component is already listed
	for _, have := range b.p.Components {
		// checking if its the same one
		if have.Name == k.name {
			return
		}
	}
	b.p.Components = append(b.p.Components, &snapshotv1alpha1.ProfileComponent{Role: k.role, Name: k.name, Version: version, Workload: src})
	// checking if its the cluster's network plugin, if so the network gets its name, more than one when one chains to another
	if k.role == "cni" && !slices.Contains(strings.Split(b.p.Network.Cni, ", "), k.name) {
		b.p.Network.Cni = strings.TrimPrefix(b.p.Network.Cni+", "+k.name, ", ")
	}
	b.found(k.role+" "+k.name+" "+version, src)
}

// settings takes a recognized component's name and container and reads the settings it carries that shape the cluster's network or DNS
func (b *builder) settings(name string, c container, src string) {
	// checking which component it is, only these carry settings the profile reads
	switch name {
	case "aws-vpc-cni":
		// the VPC CNI's switches, each "true" when on
		for env, setting := range map[string]string{
			"ENABLE_PREFIX_DELEGATION":           "prefix delegation",
			"AWS_VPC_K8S_CNI_CUSTOM_NETWORK_CFG": "custom networking",
			"AWS_VPC_K8S_CNI_EXTERNALSNAT":       "external SNAT",
			"ENABLE_POD_ENI":                     "security groups for pods",
			"ENABLE_IPv6":                        "IPv6 pods",
		} {
			// checking if this switch is on
			if strings.EqualFold(c.env[env], "true") {
				b.add(&b.p.Network.CniSettings, setting, "CNI setting", src)
			}
		}
	case "kindnet":
		// kind gives kindnet the pod network in its env
		for _, cidr := range strings.Split(c.env["POD_SUBNET"], ",") {
			b.add(&b.p.Network.PodCidrs, cidr, "pod network", src)
		}
	case "aws-load-balancer-controller":
		b.set(&b.p.Network.VpcId, flag(c.args, "aws-vpc-id"), "VPC", src)
	case "external-dns":
		// going through the domains external-dns may write records in, one flag each
		for _, d := range flags(c.args, "domain-filter") {
			// going through each domain the flag lists, comma-separated in some versions
			for _, one := range splitList(d) {
				b.add(&b.p.Dns.ManagedDomains, one, "managed DNS domain", src)
			}
		}
	}
}

// recognize takes an image and gives back the component it is and its version, the image tag, and whether its one the profile knows
func recognize(image string) (known, string, bool) {
	path, version := image, ""
	// checking if the image is pinned by digest, the digest isnt part of the name
	if i := strings.Index(path, "@"); i >= 0 {
		path = path[:i]
	}
	// checking if the image has a tag after its last slash, a colon before it is a registry port
	if i := strings.LastIndex(path, ":"); i > strings.LastIndex(path, "/") {
		path, version = path[:i], path[i+1:]
	}
	// checking if the first part is a registry, it has a dot or a port, or is localhost
	if first, rest, ok := strings.Cut(path, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		path = rest
	}
	last := path[strings.LastIndex(path, "/")+1:]
	// going through the known components in order, the first that matches wins
	for _, k := range components {
		// checking if the pattern has a slash, if so it can be anywhere in the path, if not it must be the last part
		if strings.Contains(k.image, "/") && strings.Contains(path, k.image) || !strings.Contains(k.image, "/") && last == k.image {
			return k, version, true
		}
	}
	return known{}, "", false
}

// templateContainers gives back the containers and init containers of a workload's pod template, with their image, args and env values
// env values that come from a ConfigMap or Secret reference have no value here and are left out
func templateContainers(o *resource.Object) []container {
	var out []container
	// going through both lists of containers
	for _, list := range []string{"initContainers", "containers"} {
		// going through each container in the list
		for _, c := range maps(o.Object, "spec", "template", "spec", list) {
			ct := container{env: map[string]string{}}
			ct.image, _, _ = unstructured.NestedString(c, "image")
			ct.args = append(stringList(c, "command"), stringList(c, "args")...)
			// going through the container's env for the values given inline
			for _, e := range maps(c, "env") {
				name, _, _ := unstructured.NestedString(e, "name")
				value, _, _ := unstructured.NestedString(e, "value")
				ct.env[name] = value
			}
			out = append(out, ct)
		}
	}
	return out
}

// flag takes a container's args and gives back the value of the flag named, the first one, written --name=value or --name value
func flag(args []string, name string) string {
	// invoking flags for every value and keeping the first, its defined below
	if v := flags(args, name); len(v) > 0 {
		return v[0]
	}
	return ""
}

// flags takes a container's args and gives back every value of the flag named, in order
func flags(args []string, name string) []string {
	var out []string
	// going through the args for the flag, with one or two dashes
	for i, a := range args {
		a = strings.TrimLeft(a, "-")
		// checking if its the flag with its value after an equals sign
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			out = append(out, v)
			continue
		}
		// checking if its the flag with its value as the next arg
		if a == name && strings.HasPrefix(args[i], "-") && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
