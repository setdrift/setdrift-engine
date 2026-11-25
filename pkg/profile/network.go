// reads what each kind of object says about the cluster's network, DNS and storage, from the settings of the components that set them up

package profile

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// read takes one object and hands it to the detector for its kind, objects no detector knows are passed over
func (b *builder) read(o *resource.Object) {
	gvk := o.GroupVersionKind()
	src := o.Ref()
	// checking the object's group and kind for the ones that say something about the cluster
	switch gvk.Group + "/" + gvk.Kind {
	case "/ConfigMap":
		// invoking configMap to read the system components settings, its defined below
		b.configMap(o, src)
	case "/Service":
		// invoking service to read the cluster's own Services and load balancer settings, its defined below
		b.service(o, src)
	case "/PersistentVolumeClaim":
		class, _, _ := unstructured.NestedString(o.Object, "spec", "storageClassName")
		b.claims[class]++
	case "networking.k8s.io/ServiceCIDR":
		// the range Services get addresses from, an object since Kubernetes 1.33
		for _, c := range stringList(o.Object, "spec", "cidrs") {
			b.add(&b.p.Network.ServiceCidrs, c, "service network", src)
		}
	case "storage.k8s.io/StorageClass":
		// invoking storageClass to list the class, its defined below
		b.storageClass(o)
	case "networking.k8s.io/Ingress":
		// invoking ingress to read its hosts and load balancer settings, its defined below
		b.ingress(o, src)
	case "gateway.networking.k8s.io/HTTPRoute", "gateway.networking.k8s.io/GRPCRoute", "gateway.networking.k8s.io/TLSRoute":
		// going through the host names the route serves
		for _, h := range stringList(o.Object, "spec", "hostnames") {
			b.add(&b.p.Dns.Hostnames, h, "host name", src)
		}
	case "gateway.networking.k8s.io/Gateway":
		// going through the gateway's listeners for the host names they serve
		for _, l := range maps(o.Object, "spec", "listeners") {
			h, _, _ := unstructured.NestedString(l, "hostname")
			b.add(&b.p.Dns.Hostnames, h, "host name", src)
		}
	case "crd.projectcalico.org/IPPool", "projectcalico.org/IPPool":
		// invoking calicoPool to read the pod network calico hands out, its defined below
		b.calicoPool(o, src)
	case "crd.k8s.amazonaws.com/ENIConfig":
		// the VPC CNI's custom networking, a subnet and security groups for pods in one zone, named after the zone
		subnet, _, _ := unstructured.NestedString(o.Object, "spec", "subnet")
		b.addSubnet(&snapshotv1alpha1.ProfileSubnet{Id: subnet, Zone: o.GetName(), Use: "pods"}, src)
		b.add(&b.p.Network.CniSettings, "custom networking", "CNI setting", src)
		// going through the security groups the pods get
		for _, sg := range stringList(o.Object, "spec", "securityGroups") {
			b.add(&b.p.Network.SecurityGroups, sg, "security group", src)
		}
	case "eks.amazonaws.com/NodeClass", "karpenter.k8s.aws/EC2NodeClass":
		// invoking nodeClass to read the subnets and security groups the nodes are put in, its defined below
		b.nodeClass(o, src)
	case "acn.azure.com/NodeNetworkConfig":
		// invoking azureNodeNetwork to read the pod network azure cni gave the node, its defined below
		b.azureNodeNetwork(o, src)
	case "elbv2.k8s.aws/TargetGroupBinding":
		vpc, _, _ := unstructured.NestedString(o.Object, "spec", "vpcID")
		b.set(&b.p.Network.VpcId, vpc, "VPC", src)
	case "apps/Deployment", "apps/DaemonSet", "apps/StatefulSet":
		// invoking workload to find the component it runs and its settings, its defined in components.go
		b.workload(o, src)
	}
}

// configMap takes a ConfigMap and reads the settings of the system components that keep theirs in one, kubeadm, kube-proxy, cilium, flannel and CoreDNS
func (b *builder) configMap(o *resource.Object, src string) {
	data, _, _ := unstructured.NestedStringMap(o.Object, "data")
	// checking which component the ConfigMap belongs to, by its namespace and name
	switch o.GetNamespace() + "/" + o.GetName() {
	case "kube-system/kubeadm-config":
		// invoking kubeadmConfig to read the networks kubeadm set up the cluster with, its defined below
		b.kubeadmConfig(data["ClusterConfiguration"], src)
	case "kube-system/kube-proxy", "kube-system/kube-proxy-config":
		// kubeadm keeps the configuration under config.conf, EKS under config
		b.kubeProxyConfig(data["config.conf"]+data["config"], src)
	case "kube-system/cilium-config":
		// invoking ciliumConfig to read cilium's pod network and modes, its defined below
		b.ciliumConfig(data, src)
	case "kube-system/kube-flannel-cfg", "kube-flannel/kube-flannel-cfg":
		// invoking flannelConfig to read flannel's pod network, its defined below
		b.flannelConfig(data["net-conf.json"], src)
	case "kube-system/coredns":
		// invoking corefile to read the cluster domain and where other names are sent, its defined below
		b.corefile(data["Corefile"], src)
	case "kube-system/cns-config":
		// invoking azureCNSConfig to read which mode azure cni runs in, its defined below
		b.azureCNSConfig(data["cns_config.json"], src)
	case "kube-system/azure-ip-masq-agent-config-reconciled":
		// the ranges azure's ip-masq-agent leaves unmasqueraded, the pod network under overlay, finish decides
		var cfg struct {
			NonMasqueradeCIDRs []string `json:"NonMasqueradeCIDRs"`
		}
		// checking if the config parses, its yaml, which a yaml decoder reads like json here
		if yaml.Unmarshal([]byte(data["ip-masq-agent-reconciled"]), &cfg) == nil {
			b.azureNoMasq, b.azureNoMasqSrc = cfg.NonMasqueradeCIDRs, src
		}
	}
}

// azureCNSConfig takes the cns-config ConfigMap's json and notes whether azure cni runs as overlay, where pods get addresses from a range outside the vnet
func (b *builder) azureCNSConfig(text, src string) {
	var cfg struct {
		CNIConflistScenario string `json:"CNIConflistScenario"`
	}
	// checking if the json parses and names an overlay scenario, like v4overlay or dualstackoverlay
	if json.Unmarshal([]byte(text), &cfg) == nil && strings.Contains(strings.ToLower(cfg.CNIConflistScenario), "overlay") {
		b.azureOverlay = true
		b.add(&b.p.Network.CniSettings, "overlay", "CNI setting", src)
	}
}

// azureNodeNetwork takes a NodeNetworkConfig, azure cni's record of the network it gave a node, and reads the pod network from its status
// subnetAddressSpace is the whole pod network, the overlay's range or the vnet subnet pods get addresses from
// the status is read here since normalize removes it later
func (b *builder) azureNodeNetwork(o *resource.Object, src string) {
	// going through the node's network containers
	for _, nc := range maps(o.Object, "status", "networkContainers") {
		space, _, _ := unstructured.NestedString(nc, "subnetAddressSpace")
		b.add(&b.p.Network.PodCidrs, space, "pod network", src)
	}
}

// kubeadmConfig takes kubeadm's ClusterConfiguration and reads its pod and service networks and the cluster domain
func (b *builder) kubeadmConfig(text, src string) {
	var cfg struct {
		Networking struct {
			PodSubnet     string `json:"podSubnet"`
			ServiceSubnet string `json:"serviceSubnet"`
			DNSDomain     string `json:"dnsDomain"`
		} `json:"networking"`
	}
	// checking if the configuration reads, if not theres nothing to take from it
	if yaml.Unmarshal([]byte(text), &cfg) != nil {
		return
	}
	// going through the pod networks, a dual-stack cluster lists two separated by a comma
	for _, c := range strings.Split(cfg.Networking.PodSubnet, ",") {
		b.add(&b.p.Network.PodCidrs, c, "pod network", src)
	}
	// going through the service networks the same way
	for _, c := range strings.Split(cfg.Networking.ServiceSubnet, ",") {
		b.add(&b.p.Network.ServiceCidrs, c, "service network", src)
	}
	b.set(&b.p.Dns.ClusterDomain, cfg.Networking.DNSDomain, "cluster domain", src)
}

// kubeProxyConfig takes kube-proxy's configuration and reads its proxy mode and the pod network it was told about
func (b *builder) kubeProxyConfig(text, src string) {
	var cfg struct {
		Mode        string `json:"mode"`
		ClusterCIDR string `json:"clusterCIDR"`
	}
	// checking if the configuration reads, if not theres nothing to take from it
	if yaml.Unmarshal([]byte(text), &cfg) != nil {
		return
	}
	// checking if the mode is left empty, if so kube-proxy uses its default, iptables on Linux
	mode := cfg.Mode
	if mode == "" {
		mode = "iptables"
	}
	b.set(&b.p.Network.KubeProxyMode, mode, "kube-proxy mode", src)
	// going through the pod networks kube-proxy was given, a dual-stack cluster lists two separated by a comma
	for _, c := range strings.Split(cfg.ClusterCIDR, ",") {
		b.add(&b.p.Network.PodCidrs, c, "pod network", src)
	}
}

// ciliumConfig takes cilium's settings and reads its pod network, how it hands out addresses and how it routes
func (b *builder) ciliumConfig(data map[string]string, src string) {
	// going through cilium's pod networks for each family, space-separated
	for _, key := range []string{"cluster-pool-ipv4-cidr", "cluster-pool-ipv6-cidr"} {
		// going through each network listed under the key
		for _, c := range strings.Fields(data[key]) {
			b.add(&b.p.Network.PodCidrs, c, "pod network", src)
		}
	}
	// checking if cilium says how big each node's pod range is
	// a mask size outside 0 to 128 isnt a range, so its left out
	if n, err := strconv.ParseInt(data["cluster-pool-ipv4-mask-size"], 10, 32); err == nil && n >= 0 && n <= 128 && b.p.Network.NodePodPrefix == 0 {
		b.p.Network.NodePodPrefix = int32(n)
		b.found("node pod range /"+data["cluster-pool-ipv4-mask-size"], src)
	}
	// checking how cilium hands out pod addresses, like cluster-pool, kubernetes, eni or azure
	if ipam := data["ipam"]; ipam != "" {
		b.add(&b.p.Network.CniSettings, "cilium IPAM "+ipam, "CNI setting", src)
	}
	// checking how cilium routes pod traffic, native routing or a tunnel
	if mode := data["routing-mode"]; mode != "" {
		b.add(&b.p.Network.CniSettings, "cilium "+mode+" routing", "CNI setting", src)
	}
	// checking if cilium replaces kube-proxy, if so Services are handled without it
	if r := data["kube-proxy-replacement"]; r == "true" || r == "strict" {
		b.add(&b.p.Network.CniSettings, "kube-proxy replacement", "CNI setting", src)
	}
}

// flannelConfig takes flannel's net-conf.json and reads its pod network and backend
func (b *builder) flannelConfig(text, src string) {
	var cfg struct {
		Network     string `json:"Network"`
		IPv6Network string `json:"IPv6Network"`
		Backend     struct {
			Type string `json:"Type"`
		} `json:"Backend"`
	}
	// checking if the configuration reads, if not theres nothing to take from it
	if json.Unmarshal([]byte(text), &cfg) != nil {
		return
	}
	b.add(&b.p.Network.PodCidrs, cfg.Network, "pod network", src)
	b.add(&b.p.Network.PodCidrs, cfg.IPv6Network, "pod network", src)
	// checking if the backend is named, like vxlan or host-gw
	if cfg.Backend.Type != "" {
		b.add(&b.p.Network.CniSettings, "flannel "+cfg.Backend.Type+" backend", "CNI setting", src)
	}
}

// corefileKubernetes matches the kubernetes plugin line of a Corefile, whose first zone is the cluster domain
var corefileKubernetes = regexp.MustCompile(`(?m)^\s*kubernetes\s+(\S+)`)

// corefileForward matches a forward line of a Corefile, the zone and where names in it are sent
var corefileForward = regexp.MustCompile(`(?m)^\s*forward\s+(\S+)\s+([^{\n]+)`)

// corefile takes CoreDNS's Corefile and reads the cluster domain and where names outside it are sent
func (b *builder) corefile(text, src string) {
	// checking for the kubernetes plugin, its first zone is the cluster domain
	if m := corefileKubernetes.FindStringSubmatch(text); m != nil {
		b.set(&b.p.Dns.ClusterDomain, m[1], "cluster domain", src)
	}
	// going through each forward line
	for _, m := range corefileForward.FindAllStringSubmatch(text, -1) {
		// going through where the line sends names, an address or a resolv.conf
		for _, to := range strings.Fields(m[2]) {
			// checking if the forward is for one zone only, if so the zone goes with it
			if m[1] != "." {
				to = m[1] + " to " + to
			}
			b.add(&b.p.Dns.Upstreams, to, "DNS upstream", src)
		}
	}
}

// service takes a Service and reads what the cluster's own Services say, the service network and cluster DNS, and load balancer settings
func (b *builder) service(o *resource.Object, src string) {
	ip, _, _ := unstructured.NestedString(o.Object, "spec", "clusterIP")
	// checking which Service it is
	switch o.GetNamespace() + "/" + o.GetName() {
	case "default/kubernetes":
		// the API's own Service takes the first address of the service network, which says where the network starts but not its size
		b.found("service network includes "+ip, src)
		// going through the address families the cluster serves
		for _, f := range stringList(o.Object, "spec", "ipFamilies") {
			b.add(&b.p.Network.IpFamilies, strings.ToLower(f), "IP family", src)
		}
	case "kube-system/kube-dns":
		b.set(&b.p.Dns.ClusterDnsIp, ip, "cluster DNS address", src)
	}
	ann := o.GetAnnotations()
	// checking if the Service names its load balancer's subnets, the AWS load balancer annotation takes ids or names, comma-separated
	for _, s := range splitList(ann["service.beta.kubernetes.io/aws-load-balancer-subnets"]) {
		b.addSubnet(&snapshotv1alpha1.ProfileSubnet{Id: s, Use: "load-balancers"}, src)
	}
	// going through the security groups the Service's load balancer gets
	for _, sg := range splitList(ann["service.beta.kubernetes.io/aws-load-balancer-security-groups"]) {
		b.add(&b.p.Network.SecurityGroups, sg, "security group", src)
	}
}

// ingress takes an Ingress and reads the host names it serves and its load balancer's subnets and security groups
func (b *builder) ingress(o *resource.Object, src string) {
	// going through the rules for their host names
	for _, r := range maps(o.Object, "spec", "rules") {
		h, _, _ := unstructured.NestedString(r, "host")
		b.add(&b.p.Dns.Hostnames, h, "host name", src)
	}
	// going through the tls sections for theirs
	for _, t := range maps(o.Object, "spec", "tls") {
		// going through each host the certificate is for
		for _, h := range stringList(t, "hosts") {
			b.add(&b.p.Dns.Hostnames, h, "host name", src)
		}
	}
	ann := o.GetAnnotations()
	// going through the subnets the AWS load balancer controller puts the ingress's load balancer in
	for _, s := range splitList(ann["alb.ingress.kubernetes.io/subnets"]) {
		b.addSubnet(&snapshotv1alpha1.ProfileSubnet{Id: s, Use: "load-balancers"}, src)
	}
	// going through the security groups it gets
	for _, sg := range splitList(ann["alb.ingress.kubernetes.io/security-groups"]) {
		b.add(&b.p.Network.SecurityGroups, sg, "security group", src)
	}
}

// storageClass takes a StorageClass and lists it, its provisioner, whether its the default and its settings
func (b *builder) storageClass(o *resource.Object) {
	sc := &snapshotv1alpha1.ProfileStorageClass{Name: o.GetName()}
	sc.Provisioner, _, _ = unstructured.NestedString(o.Object, "provisioner")
	sc.ReclaimPolicy, _, _ = unstructured.NestedString(o.Object, "reclaimPolicy")
	sc.VolumeBindingMode, _, _ = unstructured.NestedString(o.Object, "volumeBindingMode")
	sc.AllowVolumeExpansion, _, _ = unstructured.NestedBool(o.Object, "allowVolumeExpansion")
	sc.Parameters, _, _ = unstructured.NestedStringMap(o.Object, "parameters")
	ann := o.GetAnnotations()
	// checking either annotation that makes a class the default, the beta one older clusters still use
	sc.Default = ann["storageclass.kubernetes.io/is-default-class"] == "true" || ann["storageclass.beta.kubernetes.io/is-default-class"] == "true"
	b.p.StorageClasses = append(b.p.StorageClasses, sc)
}

// calicoPool takes a calico IPPool and reads the pod network it hands out and how its traffic is carried
func (b *builder) calicoPool(o *resource.Object, src string) {
	cidr, _, _ := unstructured.NestedString(o.Object, "spec", "cidr")
	b.add(&b.p.Network.PodCidrs, cidr, "pod network", src)
	// checking if the pool says how big each node's block is
	// a block size outside 0 to 128 isnt a range, so its left out
	if n, ok, _ := unstructured.NestedInt64(o.Object, "spec", "blockSize"); ok && n >= 0 && n <= 128 && b.p.Network.NodePodPrefix == 0 {
		b.p.Network.NodePodPrefix = int32(n)
		b.found("node pod range /"+strconv.FormatInt(n, 10), src)
	}
	// going through the two encapsulations calico offers, each Never, Always or CrossSubnet
	for _, enc := range []string{"ipipMode", "vxlanMode"} {
		// checking if this one is on
		if mode, _, _ := unstructured.NestedString(o.Object, "spec", enc); mode != "" && mode != "Never" {
			b.add(&b.p.Network.CniSettings, "calico "+strings.TrimSuffix(enc, "Mode")+" "+mode, "CNI setting", src)
		}
	}
}

// nodeClass takes an EKS Auto Mode NodeClass or a Karpenter EC2NodeClass and reads the subnets and security groups its nodes go in
// the status holds them as resolved, with each subnet's zone, and normalize removes status later, so its read here
func (b *builder) nodeClass(o *resource.Object, src string) {
	// going through the subnets the nodes are launched in
	for _, s := range maps(o.Object, "status", "subnets") {
		id, _, _ := unstructured.NestedString(s, "id")
		zone, _, _ := unstructured.NestedString(s, "zone")
		b.addSubnet(&snapshotv1alpha1.ProfileSubnet{Id: id, Zone: zone, Use: "nodes"}, src)
	}
	// going through the subnets pods get addresses from, when EKS Auto Mode gives pods subnets of their own
	for _, s := range maps(o.Object, "status", "podSubnets") {
		id, _, _ := unstructured.NestedString(s, "id")
		zone, _, _ := unstructured.NestedString(s, "zone")
		b.addSubnet(&snapshotv1alpha1.ProfileSubnet{Id: id, Zone: zone, Use: "pods"}, src)
	}
	// going through the security groups the nodes get
	for _, sg := range maps(o.Object, "status", "securityGroups") {
		id, _, _ := unstructured.NestedString(sg, "id")
		b.add(&b.p.Network.SecurityGroups, id, "security group", src)
	}
}

// stringList gives back the list of strings at a path, or nil when theres none or its not a list of strings
func stringList(obj map[string]any, path ...string) []string {
	l, _, _ := unstructured.NestedStringSlice(obj, path...)
	return l
}

// maps gives back the list of objects at a path, entries that arent objects left out
func maps(obj map[string]any, path ...string) []map[string]any {
	l, _, _ := unstructured.NestedSlice(obj, path...)
	out := make([]map[string]any, 0, len(l))
	// going through the entries and keeping the objects
	for _, e := range l {
		// checking if the entry is an object
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// splitList splits an annotation that lists values separated by commas, dropping the spaces and empty entries
func splitList(s string) []string {
	var out []string
	// going through each value
	for _, v := range strings.Split(s, ",") {
		// checking if its not empty once trimmed
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
