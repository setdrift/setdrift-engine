// tests that the profile reads the networks, DNS, storage, components and cloud account from the objects and nodes a kind cluster and an EKS cluster have

package profile

import (
	"net/netip"
	"reflect"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

func objects(t *testing.T, docs ...string) []*resource.Object {
	t.Helper()
	var out []*resource.Object
	for _, d := range docs {
		m := map[string]any{}
		if err := yaml.Unmarshal([]byte(d), &m); err != nil {
			t.Fatalf("%v\n%s", err, d)
		}
		out = append(out, resource.New(&unstructured.Unstructured{Object: m}))
	}
	return out
}

func evidence(p *snapshotv1alpha1.Profile) []string {
	var out []string
	for _, e := range p.Evidence {
		out = append(out, e.Value+" <- "+e.Source)
	}
	return out
}

var kindObjects = []string{`
apiVersion: v1
kind: ConfigMap
metadata: {name: kubeadm-config, namespace: kube-system}
data:
  ClusterConfiguration: |
    apiVersion: kubeadm.k8s.io/v1beta4
    kind: ClusterConfiguration
    networking:
      dnsDomain: cluster.local
      podSubnet: 10.244.0.0/16
      serviceSubnet: 10.96.0.0/16
`, `
apiVersion: v1
kind: ConfigMap
metadata: {name: kube-proxy, namespace: kube-system}
data:
  config.conf: |
    apiVersion: kubeproxy.config.k8s.io/v1alpha1
    kind: KubeProxyConfiguration
    clusterCIDR: 10.244.0.0/16
    mode: iptables
`, `
apiVersion: v1
kind: ConfigMap
metadata: {name: coredns, namespace: kube-system}
data:
  Corefile: |
    .:53 {
        errors
        kubernetes cluster.local in-addr.arpa ip6.arpa {
           pods insecure
        }
        forward . /etc/resolv.conf {
           max_concurrent 1000
        }
        cache 30
    }
`, `
apiVersion: v1
kind: Service
metadata: {name: kube-dns, namespace: kube-system}
spec: {clusterIP: 10.96.0.10}
`, `
apiVersion: v1
kind: Service
metadata: {name: kubernetes, namespace: default}
spec: {clusterIP: 10.96.0.1, ipFamilies: [IPv4]}
`, `
apiVersion: networking.k8s.io/v1
kind: ServiceCIDR
metadata: {name: kubernetes}
spec: {cidrs: [10.96.0.0/16]}
`, `
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: kindnet, namespace: kube-system}
spec:
  template:
    spec:
      containers:
      - name: kindnet-cni
        image: docker.io/kindest/kindnetd:v20250512-df8de77b
        env:
        - {name: POD_SUBNET, value: 10.244.0.0/16}
        - {name: HOST_IP, valueFrom: {fieldRef: {fieldPath: status.hostIP}}}
`, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: coredns, namespace: kube-system}
spec:
  template:
    spec:
      containers:
      - {name: coredns, image: registry.k8s.io/coredns/coredns:v1.12.1}
`, `
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: standard
  annotations: {storageclass.kubernetes.io/is-default-class: "true"}
provisioner: rancher.io/local-path
reclaimPolicy: Delete
volumeBindingMode: WaitForFirstConsumer
`, `
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: data, namespace: shop}
spec: {storageClassName: standard}
`, `
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: cache, namespace: shop}
spec: {}
`}

// check that a kind cluster's networks, DNS, plugin, proxy mode and storage come from kubeadm's, kube-proxy's and CoreDNS's settings
func TestBuildKind(t *testing.T) {
	p := Build(objects(t, kindObjects...))
	n, d := p.Network, p.Dns
	if !reflect.DeepEqual(n.PodCidrs, []string{"10.244.0.0/16"}) || !reflect.DeepEqual(n.ServiceCidrs, []string{"10.96.0.0/16"}) {
		t.Errorf("pod %v, service %v", n.PodCidrs, n.ServiceCidrs)
	}
	if n.Cni != "kindnet" || n.KubeProxyMode != "iptables" || !reflect.DeepEqual(n.IpFamilies, []string{"ipv4"}) {
		t.Errorf("cni %q, proxy %q, families %v", n.Cni, n.KubeProxyMode, n.IpFamilies)
	}
	if d.ClusterDomain != "cluster.local" || d.ClusterDnsIp != "10.96.0.10" || !reflect.DeepEqual(d.Upstreams, []string{"/etc/resolv.conf"}) {
		t.Errorf("dns %+v", d)
	}
	if len(p.StorageClasses) != 1 || !p.StorageClasses[0].Default || p.StorageClasses[0].Claims != 2 {
		t.Errorf("storage classes %+v, want standard as default with both claims", p.StorageClasses)
	}
	var names []string
	for _, c := range p.Components {
		names = append(names, c.Name+" "+c.Version)
	}
	if want := []string{"kindnet v20250512-df8de77b", "coredns v1.12.1"}; !reflect.DeepEqual(names, want) {
		t.Errorf("components %v, want %v", names, want)
	}
	ev := evidence(p)
	for _, want := range []string{
		"pod network 10.244.0.0/16 <- ConfigMap kube-system/kubeadm-config",
		"pod network 10.244.0.0/16 <- ConfigMap kube-system/kube-proxy",
		"pod network 10.244.0.0/16 <- apps/DaemonSet kube-system/kindnet",
		"service network 10.96.0.0/16 <- networking.k8s.io/ServiceCIDR kubernetes",
		"service network includes 10.96.0.1 <- Service default/kubernetes",
	} {
		if !slices.Contains(ev, want) {
			t.Errorf("evidence lacks %q:\n%v", want, ev)
		}
	}
}

// check that the profile is the same whatever order the objects come in
func TestBuildOrder(t *testing.T) {
	objs := objects(t, kindObjects...)
	a := Build(objs)
	slices.Reverse(objs)
	if b := Build(objs); !reflect.DeepEqual(evidence(a), evidence(b)) || !reflect.DeepEqual(a.Network.PodCidrs, b.Network.PodCidrs) {
		t.Errorf("order changed the profile:\n%v\n%v", evidence(a), evidence(b))
	}
}

// check that an EKS cluster's VPC CNI settings, node subnets, security groups, VPC, host names and managed domains come from its components and custom resources
func TestBuildEKS(t *testing.T) {
	p := Build(objects(t, `
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: aws-node, namespace: kube-system}
spec:
  template:
    spec:
      initContainers:
      - {name: aws-vpc-cni-init, image: 602401143452.dkr.ecr.us-east-1.amazonaws.com/amazon-k8s-cni-init:v1.20.4}
      containers:
      - name: aws-node
        image: 602401143452.dkr.ecr.us-east-1.amazonaws.com/amazon-k8s-cni:v1.20.4-eksbuild.1
        env:
        - {name: ENABLE_PREFIX_DELEGATION, value: "true"}
        - {name: AWS_VPC_K8S_CNI_EXTERNALSNAT, value: "false"}
`, `
apiVersion: eks.amazonaws.com/v1
kind: NodeClass
metadata: {name: default}
status:
  subnets:
  - {id: subnet-0aaa, zone: us-east-1a, zoneID: use1-az1}
  - {id: subnet-0bbb, zone: us-east-1b, zoneID: use1-az2}
  securityGroups:
  - {id: sg-0node, name: eks-cluster-sg}
`, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: aws-load-balancer-controller, namespace: kube-system}
spec:
  template:
    spec:
      containers:
      - name: controller
        image: public.ecr.aws/eks/aws-load-balancer-controller:v2.14.1
        args: [--cluster-name=prod, --aws-vpc-id=vpc-0abc, --aws-region=us-east-1]
`, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: external-dns, namespace: external-dns}
spec:
  template:
    spec:
      containers:
      - name: external-dns
        image: registry.k8s.io/external-dns/external-dns:v0.19.0
        args: [--provider=aws, --domain-filter=example.com, --domain-filter, internal.example.com]
`, `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: web
  namespace: shop
  annotations: {alb.ingress.kubernetes.io/subnets: "subnet-0pub1, subnet-0pub2"}
spec:
  rules: [{host: shop.example.com}]
  tls: [{hosts: [shop.example.com, www.example.com]}]
`))
	n := p.Network
	if n.Cni != "aws-vpc-cni" || !reflect.DeepEqual(n.CniSettings, []string{"prefix delegation"}) {
		t.Errorf("cni %q settings %v", n.Cni, n.CniSettings)
	}
	if n.VpcId != "vpc-0abc" || !reflect.DeepEqual(n.SecurityGroups, []string{"sg-0node"}) {
		t.Errorf("vpc %q sgs %v", n.VpcId, n.SecurityGroups)
	}
	var subnets []string
	for _, s := range n.Subnets {
		subnets = append(subnets, s.Use+" "+s.Id+" "+s.Zone)
	}
	if want := []string{"load-balancers subnet-0pub1 ", "load-balancers subnet-0pub2 ", "nodes subnet-0aaa us-east-1a", "nodes subnet-0bbb us-east-1b"}; !reflect.DeepEqual(subnets, want) {
		t.Errorf("subnets %v, want %v", subnets, want)
	}
	if !reflect.DeepEqual(p.Dns.Hostnames, []string{"shop.example.com", "www.example.com"}) {
		t.Errorf("host names %v", p.Dns.Hostnames)
	}
	if !reflect.DeepEqual(p.Dns.ManagedDomains, []string{"example.com", "internal.example.com"}) {
		t.Errorf("managed domains %v", p.Dns.ManagedDomains)
	}
}

// check that the nodes give the zones, the range their addresses are in, the pod network when nothing named it, the most pods and the cloud account
func TestAddNodes(t *testing.T) {
	node := func(zone, ip, podCIDR, providerID, pods string) unstructured.Unstructured {
		return unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "n", "labels": map[string]any{"topology.kubernetes.io/zone": zone}},
			"spec":     map[string]any{"podCIDR": podCIDR, "providerID": providerID},
			"status": map[string]any{
				"addresses":   []any{map[string]any{"type": "InternalIP", "address": ip}, map[string]any{"type": "Hostname", "address": "n"}},
				"allocatable": map[string]any{"pods": pods},
			},
		}}
	}
	p := AddNodes(nil, []unstructured.Unstructured{
		node("us-central1-a", "10.128.0.5", "10.4.0.0/24", "gce://my-project/us-central1-a/gke-a", "110"),
		node("us-central1-a", "10.128.0.9", "10.4.1.0/24", "gce://my-project/us-central1-a/gke-b", "110"),
		node("us-central1-b", "10.128.1.20", "10.4.2.0/24", "gce://my-project/us-central1-b/gke-c", "64"),
	})
	if p.Cloud.Account != "my-project" || !reflect.DeepEqual(p.Cloud.Zones, []string{"us-central1-a", "us-central1-b"}) {
		t.Errorf("cloud %+v", p.Cloud)
	}
	if !reflect.DeepEqual(p.Network.PodCidrs, []string{"10.4.0.0/22"}) || !p.Network.PodCidrsFromNodes || p.Network.NodePodPrefix != 24 || p.Network.MaxPodsPerNode != 110 {
		t.Errorf("pods %v /%d max %d", p.Network.PodCidrs, p.Network.NodePodPrefix, p.Network.MaxPodsPerNode)
	}
	var nodes []string
	for _, r := range p.Network.NodeRanges {
		nodes = append(nodes, r.Zone+" "+r.Cidr)
	}
	if want := []string{"us-central1-a 10.128.0.0/28", "us-central1-b 10.128.1.20/32"}; !reflect.DeepEqual(nodes, want) {
		t.Errorf("node ranges %v, want %v", nodes, want)
	}

	named := &snapshotv1alpha1.Profile{Network: &snapshotv1alpha1.ProfileNetwork{PodCidrs: []string{"10.4.0.0/14"}}}
	if p := AddNodes(named, []unstructured.Unstructured{node("a", "10.0.0.1", "10.4.0.0/24", "", "")}); !reflect.DeepEqual(p.Network.PodCidrs, []string{"10.4.0.0/14"}) {
		t.Errorf("named pod network replaced: %v", p.Network.PodCidrs)
	}

	az := AddNodes(nil, []unstructured.Unstructured{node("eastus-1", "10.224.0.4", "", "azure:///subscriptions/0000-1111/resourceGroups/MC_rg_aks_eastus/providers/Microsoft.Compute/virtualMachineScaleSets/aks-pool/virtualMachines/0", "250")})
	if az.Cloud.Account != "0000-1111" || az.Cloud.ResourceGroup != "MC_rg_aks_eastus" {
		t.Errorf("azure cloud %+v", az.Cloud)
	}

	// aks labels a node outside any zone with its fault domain, "0", which isnt a zone
	fd := AddNodes(nil, []unstructured.Unstructured{node("0", "10.224.0.4", "", "", "250")})
	if len(fd.Cloud.Zones) != 0 || fd.Network.NodeRanges[0].Zone != "" {
		t.Errorf("fault domain taken for a zone: zones %v, node ranges %+v", fd.Cloud.Zones, fd.Network.NodeRanges)
	}
}

// check that AKS's pod network is read from azure cni, the NodeNetworkConfig first, and under overlay the ip-masq-agent's ranges when theres no NodeNetworkConfig, but not under its other modes, where theyre the vnet
func TestBuildAKS(t *testing.T) {
	cns := func(scenario string) string {
		return `
apiVersion: v1
kind: ConfigMap
metadata: {name: cns-config, namespace: kube-system}
data:
  cns_config.json: '{"CNIConflistScenario": "` + scenario + `", "ChannelMode": "CRD"}'
`
	}
	const masq = `
apiVersion: v1
kind: ConfigMap
metadata: {name: azure-ip-masq-agent-config-reconciled, namespace: kube-system}
data:
  ip-masq-agent-reconciled: |
    MasqLinkLocal: true
    NonMasqueradeCIDRs:
    - 10.244.0.0/16
`
	const nnc = `
apiVersion: acn.azure.com/v1alpha
kind: NodeNetworkConfig
metadata: {name: aks-nodepool1-0, namespace: kube-system}
status:
  networkContainers:
  - {type: Overlay, subnetAddressSpace: 10.248.0.0/16, primaryIP: 10.248.0.0/24}
`
	if p := Build(objects(t, cns("v4overlay"), masq)); !reflect.DeepEqual(p.Network.PodCidrs, []string{"10.244.0.0/16"}) || !slices.Contains(p.Network.CniSettings, "overlay") {
		t.Errorf("overlay from ip-masq-agent: pods %v settings %v", p.Network.PodCidrs, p.Network.CniSettings)
	}
	if p := Build(objects(t, cns("v4overlay"), masq, nnc)); !reflect.DeepEqual(p.Network.PodCidrs, []string{"10.248.0.0/16"}) {
		t.Errorf("overlay with a NodeNetworkConfig: pods %v", p.Network.PodCidrs)
	}
	if p := Build(objects(t, cns("v4swift"), masq)); len(p.Network.PodCidrs) != 0 {
		t.Errorf("not overlay, the vnet taken for the pod network: %v", p.Network.PodCidrs)
	}
}

// check that GKE's own networking and kube-dns are recognized, and the cluster domain is read from kube-dns's flag
func TestBuildGKE(t *testing.T) {
	p := Build(objects(t, `
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: netd, namespace: kube-system}
spec:
  template:
    spec:
      containers:
      - {name: netd, image: gke.gcr.io/netd:v0.8.4-gke.7}
`, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: kube-dns, namespace: kube-system}
spec:
  template:
    spec:
      containers:
      - name: kubedns
        image: gke.gcr.io/k8s-dns-kube-dns:1.26.4-gke.1
        args: [--domain=cluster.local., --dns-port=10053]
`))
	if p.Network.Cni != "gke-netd" || p.Dns.ClusterDomain != "cluster.local" {
		t.Errorf("cni %q, cluster domain %q", p.Network.Cni, p.Dns.ClusterDomain)
	}
}

// check that the EKS API's VPC, subnets, security groups, service network and endpoint access go into the profile
func TestAddEKS(t *testing.T) {
	p := AddEKS(nil, EKS{
		Account: "111111111111", Region: "us-east-1", VpcID: "vpc-0abc",
		SubnetIDs: []string{"subnet-0b", "subnet-0a"}, SecurityGroupIDs: []string{"sg-0extra"}, ClusterSecurityGroupID: "sg-0cluster",
		ServiceIPv4CIDR: "172.20.0.0/16", IPFamily: "ipv4",
		EndpointPublicAccess: true, EndpointPrivateAccess: true, PublicAccessCIDRs: []string{"0.0.0.0/0"},
	})
	n := p.Network
	if p.Cloud.Provider != "aws" || p.Cloud.Account != "111111111111" || n.VpcId != "vpc-0abc" {
		t.Errorf("cloud %+v vpc %q", p.Cloud, n.VpcId)
	}
	if len(n.Subnets) != 2 || n.Subnets[0].Id != "subnet-0a" || n.Subnets[0].Use != "control-plane" {
		t.Errorf("subnets %+v", n.Subnets)
	}
	if !reflect.DeepEqual(n.SecurityGroups, []string{"sg-0cluster", "sg-0extra"}) || !reflect.DeepEqual(n.ServiceCidrs, []string{"172.20.0.0/16"}) {
		t.Errorf("sgs %v service %v", n.SecurityGroups, n.ServiceCidrs)
	}
	if n.ApiEndpointAccess != "public and private" || !reflect.DeepEqual(n.ApiPublicAccessCidrs, []string{"0.0.0.0/0"}) {
		t.Errorf("endpoint %q from %v", n.ApiEndpointAccess, n.ApiPublicAccessCidrs)
	}
}

// check that the classification gives the cloud and region and an EKS ARN gives the account, without replacing what was already found
func TestSetPlatform(t *testing.T) {
	p := SetPlatform(nil, &snapshotv1alpha1.Cluster{
		ProviderId:     "arn:aws:eks:us-west-2:222222222222:cluster/west",
		Classification: &snapshotv1alpha1.Platform{Infrastructure: "aws", Region: "us-west-2"},
	})
	if p.Cloud.Provider != "aws" || p.Cloud.Region != "us-west-2" || p.Cloud.Account != "222222222222" {
		t.Errorf("cloud %+v", p.Cloud)
	}
	kept := &snapshotv1alpha1.Profile{Cloud: &snapshotv1alpha1.ProfileCloud{Account: "my-project"}}
	if p := SetPlatform(kept, &snapshotv1alpha1.Cluster{Classification: &snapshotv1alpha1.Platform{Infrastructure: "gcp"}}); p.Cloud.Account != "my-project" || p.Cloud.Provider != "gcp" {
		t.Errorf("cloud %+v", p.Cloud)
	}
}

// check that the covering range is the smallest holding every address or range, one per family
func TestCovering(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want []string
	}{
		{[]string{"10.0.0.0/24", "10.0.1.0/24"}, []string{"10.0.0.0/23"}},
		{[]string{"10.0.0.0/24", "10.0.3.0/24"}, []string{"10.0.0.0/22"}},
		{[]string{"192.168.1.7/32"}, []string{"192.168.1.7/32"}},
		{[]string{"10.0.0.1/32", "10.255.0.1/32"}, []string{"10.0.0.0/8"}},
		{[]string{"10.0.0.0/16", "10.0.4.0/24"}, []string{"10.0.0.0/16"}},
		{[]string{"10.0.0.1/32", "fd00::1/128", "fd00::ff/128"}, []string{"10.0.0.1/32", "fd00::/120"}},
	} {
		var in []netip.Prefix
		for _, s := range c.in {
			in = append(in, netip.MustParsePrefix(s))
		}
		var got []string
		for _, p := range covering(in) {
			got = append(got, p.String())
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("covering(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// check that images are recognized whatever their registry, tag or digest, and that a lookalike name is not
func TestRecognize(t *testing.T) {
	for _, c := range []struct{ image, name, version string }{
		{"quay.io/cilium/cilium:v1.18.2@sha256:abc", "cilium", "v1.18.2"},
		{"localhost:5000/coredns/coredns:v1.12.1", "coredns", "v1.12.1"},
		{"quay.io/jetstack/cert-manager-controller:v1.19.1", "cert-manager", "v1.19.1"},
		{"docker.io/calico/node:v3.30.0", "calico", "v3.30.0"},
		{"public.ecr.aws/karpenter/controller:1.8.0", "karpenter", "1.8.0"},
		{"registry.example.com/team/my-coredns-fork:1", "", ""},
		{"nginx:1.29", "", ""},
	} {
		k, v, ok := recognize(c.image)
		if k.name != c.name || v != c.version || ok != (c.name != "") {
			t.Errorf("recognize(%q) = %q %q %v, want %q %q", c.image, k.name, v, ok, c.name, c.version)
		}
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
