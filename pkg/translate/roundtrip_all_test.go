package translate

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/diff"
	"setdrift.io/setdrift-engine/pkg/platform"
)

// cluster builds a representative cluster of a platform as its export holds it: a storage class of its general SSD, an internal load balancer,
// a workload pinned to a node pool on spot, a workload using the platform's cloud identity, a NetworkPolicy and a ConfigMap
func cluster(t *testing.T, d platform.Distribution) []*unstructured.Unstructured {
	t.Helper()
	var class, lb, pool, spot, saAnn string
	// a platform that taints its spot nodes has the toleration on a workload pinned to them, as it must to run there
	tol := ""
	switch d {
	case platform.EKS:
		class = "provisioner: ebs.csi.eks.amazonaws.com\nparameters: {type: gp3}"
		lb = "annotations: {service.beta.kubernetes.io/aws-load-balancer-scheme: internal}}\nspec: {type: LoadBalancer, loadBalancerClass: eks.amazonaws.com/nlb, ports: [{port: 80}]}"
		pool, spot = "eks.amazonaws.com/nodegroup", "{eks.amazonaws.com/capacityType: SPOT}"
		saAnn = "eks.amazonaws.com/role-arn: arn:aws:iam::111122223333:role/orders"
	case platform.GKE:
		class = "provisioner: pd.csi.storage.gke.io\nparameters: {type: pd-balanced}"
		lb = "annotations: {networking.gke.io/load-balancer-type: Internal}}\nspec: {type: LoadBalancer, ports: [{port: 80}]}"
		pool, spot = "cloud.google.com/gke-nodepool", "{cloud.google.com/gke-spot: \"true\"}"
		tol = `
      tolerations: [{key: cloud.google.com/gke-spot, operator: Equal, value: "true", effect: NoSchedule}]`
		saAnn = "iam.gke.io/gcp-service-account: orders@iam.gserviceaccount.com"
	case platform.AKS:
		class = "provisioner: disk.csi.azure.com\nparameters: {skuName: PremiumV2_LRS}"
		lb = "annotations: {service.beta.kubernetes.io/azure-load-balancer-internal: \"true\"}}\nspec: {type: LoadBalancer, ports: [{port: 80}]}"
		pool, spot = "kubernetes.azure.com/agentpool", "{kubernetes.azure.com/scalesetpriority: spot}"
		tol = `
      tolerations: [{key: kubernetes.azure.com/scalesetpriority, operator: Equal, value: spot, effect: NoSchedule}]`
		saAnn = "azure.workload.identity/client-id: 00000000-0000-0000-0000-000000000000"
	}
	docs := []string{
		"apiVersion: storage.k8s.io/v1\nkind: StorageClass\nmetadata: {name: fast}\n" + class,
		"apiVersion: v1\nkind: Service\nmetadata: {name: web-lb, namespace: shop, " + lb,
		"apiVersion: v1\nkind: ServiceAccount\nmetadata: {name: orders, namespace: shop, annotations: {" + saAnn + "}}",
		`apiVersion: apps/v1
kind: Deployment
metadata: {name: orders, namespace: shop}
spec:
  selector: {matchLabels: {app: orders}}
  template:
    metadata: {labels: {app: orders}}
    spec:
      serviceAccountName: orders
      nodeSelector: {` + pool + `: batch}
      containers: [{name: orders, image: busybox, ports: [{containerPort: 8080}]}]`,
		`apiVersion: apps/v1
kind: Deployment
metadata: {name: batch, namespace: shop}
spec:
  selector: {matchLabels: {app: batch}}
  template:
    metadata: {labels: {app: batch}}
    spec:
      nodeSelector: ` + spot + tol + `
      containers: [{name: batch, image: busybox}]`,
		"apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata: {name: deny-all, namespace: shop}\nspec: {podSelector: {}}",
		"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: settings, namespace: shop}\ndata: {mode: prod}",
	}
	var out []*unstructured.Unstructured
	for _, d := range docs {
		out = append(out, u(t, d))
	}
	return out
}

// roundTrip translates a platform's cluster to another and back with every default, and gives back each object's differences from where it started, as kind/name: paths
func roundTrip(t *testing.T, home, away platform.Distribution) map[string][]string {
	t.Helper()
	start := cluster(t, home)
	back := translated(t, translated(t, start, home, away), away, home)
	out := map[string][]string{}
	for _, o := range start {
		got := find(back, o.GetKind(), o.GetName())
		key := o.GetKind() + "/" + o.GetName()
		if got == nil {
			out[key] = []string{"(missing)"}
			continue
		}
		for _, f := range diff.Fields(diff.WithoutTranslationMark(jsonOf(o.Object)), diff.WithoutTranslationMark(jsonOf(got.Object))) {
			out[key] = append(out[key], fmt.Sprintf("%s: %v -> %v", f.Path, f.A, f.B))
		}
	}
	return out
}

// jsonOf gives back an object as JSON decodes it, so number types match on both sides
func jsonOf(m map[string]any) any {
	o := &unstructured.Unstructured{Object: m}
	b, _ := o.MarshalJSON()
	back := &unstructured.Unstructured{}
	_ = back.UnmarshalJSON(b)
	return back.Object
}

// every direction, there and back with every default, comes back as it was, apart from the known case below, ROUNDTRIP=1 prints every difference
func TestRoundTripAllDirections(t *testing.T) {
	all := []platform.Distribution{platform.EKS, platform.AKS, platform.GKE}
	var report []string
	for _, home := range all {
		for _, away := range all {
			if home == away {
				continue
			}
			got := roundTrip(t, home, away)
			keys := make([]string, 0, len(got))
			for k := range got {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				report = append(report, fmt.Sprintf("%s -> %s -> %s  %s\n      %s", home, away, home, k, strings.Join(got[k], "\n      ")))
				// the one known case: a storage class comes back as the platform's own class of its tier, see the M3 notes in the backlog
				for _, line := range got[k] {
					if k != "StorageClass/fast" || !strings.HasPrefix(line, "parameters") {
						t.Errorf("%s -> %s -> %s: %s doesn't come back as it was: %s", home, away, home, k, line)
					}
				}
			}
		}
	}
	if os.Getenv("ROUNDTRIP") != "" {
		t.Log("\n" + strings.Join(report, "\n"))
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
