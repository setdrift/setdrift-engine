// tests the platform rules of the classifier, the objects EKS, kubeadm and kind create that the export leaves out

package provenance

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/platform"
	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// check that the objects EKS creates and keeps are the platform's, by label, field manager, name or CRD group, and a user's own lookalikes arent
func TestPlatform(t *testing.T) {
	mk := func(apiVersion, kind, name string, labels map[string]string, managers ...string) *resource.Object {
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind}}
		u.SetName(name)
		u.SetLabels(labels)
		var mf []metav1.ManagedFieldsEntry
		for _, m := range managers {
			mf = append(mf, metav1.ManagedFieldsEntry{Manager: m})
		}
		u.SetManagedFields(mf)
		return resource.New(u)
	}
	gp2 := mk("storage.k8s.io/v1", "StorageClass", "gp2", nil, "kubectl-client-side-apply")
	gp2.Object["provisioner"] = "kubernetes.io/aws-ebs"
	myGP2 := mk("storage.k8s.io/v1", "StorageClass", "gp2", nil, "kubectl-client-side-apply")
	myGP2.Object["provisioner"] = "ebs.csi.aws.com"
	for _, tc := range []struct {
		name string
		o    *resource.Object
		want bool
	}{
		{"add-on label", mk("apps/v1", "Deployment", "metrics-server", map[string]string{"app.kubernetes.io/managed-by": "EKS", "app.kubernetes.io/instance": "metrics-server"}, "eks", "kube-controller-manager"), true},
		{"auto mode label", mk("karpenter.sh/v1", "NodePool", "system", map[string]string{"app.kubernetes.io/managed-by": "eks"}, "eks-auto-mode"), true},
		{"only EKS managers", mk("rbac.authorization.k8s.io/v1", "Role", "x", nil, "eks-internal", "kube-apiserver"), true},
		{"EKS manager and a person", mk("v1", "ConfigMap", "x", nil, "eks", "kubectl-edit"), false},
		{"eks: RBAC applied with kubectl", mk("rbac.authorization.k8s.io/v1", "ClusterRole", "eks:addon-manager", nil, "kubectl-client-side-apply"), true},
		{"bootstrap webhook", mk("admissionregistration.k8s.io/v1", "MutatingWebhookConfiguration", "pod-identity-webhook", nil, "kubectl-client-side-apply"), true},
		{"EKS gp2", gp2, true},
		{"own gp2", myGP2, false},
		{"VPC CNI CRD", mk("apiextensions.k8s.io/v1", "CustomResourceDefinition", "policyendpoints.networking.k8s.aws", nil, "kubectl-client-side-apply", "kube-apiserver"), true},
		{"own CRD", mk("apiextensions.k8s.io/v1", "CustomResourceDefinition", "widgets.example.com", nil, "kubectl-client-side-apply", "kube-apiserver"), false},
		{"own ClusterRole", mk("rbac.authorization.k8s.io/v1", "ClusterRole", "reader", nil, "kubectl-client-side-apply"), false},
		{"no managers", mk("v1", "ConfigMap", "x", nil), false},
	} {
		got := LabelClassifier{}.Classify(tc.o).Source == snapshotv1alpha1.Source_SOURCE_PLATFORM
		if got != tc.want {
			t.Errorf("%s: platform %v, want %v", tc.name, got, tc.want)
		}
	}
}

// check that the default instance label counts for nothing even with an Application of that name, every Helm chart sets it, and a custom one finds its Application's namespace
func TestInstanceLabelWithoutApplication(t *testing.T) {
	mk := func(apiVersion, kind, ns, name string, labels map[string]string) *resource.Object {
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind}}
		u.SetNamespace(ns)
		u.SetName(name)
		u.SetLabels(labels)
		return resource.New(u)
	}
	const custom = "argocd.argoproj.io/instance"
	app := mk("argoproj.io/v1alpha1", "Application", "argocd", "guestbook", nil)
	chart := mk("v1", "ConfigMap", "shop", "ui", map[string]string{DefaultArgoInstanceLabel: "guestbook"})
	tracked := mk("v1", "ConfigMap", "shop", "api", map[string]string{custom: "guestbook"})
	flux := mk("apps/v1", "Deployment", "flux-system", "source-controller", map[string]string{DefaultArgoInstanceLabel: "flux-system"})
	out, _, err := LabelClassifier{ArgoInstanceLabel: custom}.Process(context.Background(), []*resource.Object{app, chart, tracked, flux})
	if err != nil || len(out) != 4 {
		t.Fatal(err)
	}
	if chart.Provenance.Source != snapshotv1alpha1.Source_SOURCE_UNMANAGED {
		t.Errorf("default label: %v", chart.Provenance)
	}
	if tracked.Provenance.Source != snapshotv1alpha1.Source_SOURCE_ARGO_CD || tracked.Provenance.Manager != "argocd/guestbook" {
		t.Errorf("custom label: %v", tracked.Provenance)
	}
	if flux.Provenance.Source != snapshotv1alpha1.Source_SOURCE_UNMANAGED || len(flux.Findings) != 0 {
		t.Errorf("flux: %v %v", flux.Provenance, flux.Findings)
	}
}

// check that kubeadm's objects are the platform's by their field manager on any cluster and the user's once changed, and kind's add-ons count only on a cluster classified as kind
func TestKubeadmAndKind(t *testing.T) {
	mk := func(apiVersion, kind, ns, name string, managers ...string) *resource.Object {
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind}}
		u.SetName(name)
		u.SetNamespace(ns)
		var mf []metav1.ManagedFieldsEntry
		for _, m := range managers {
			mf = append(mf, metav1.ManagedFieldsEntry{Manager: m})
		}
		u.SetManagedFields(mf)
		return resource.New(u)
	}
	kindCluster := &platform.Stage{Result: platform.Decide(platform.FromNodes([]platform.Node{{ProviderID: "kind://docker/x/x"}}))}
	eksCluster := &platform.Stage{Result: platform.Decide(platform.FromKubeconfig(platform.Kubeconfig{Context: "arn:aws:eks:us-east-2:1:cluster/x"}))}
	localPath := mk("storage.k8s.io/v1", "StorageClass", "", "standard", "kubectl-client-side-apply")
	localPath.Object["provisioner"] = "rancher.io/local-path"
	for _, tc := range []struct {
		name    string
		o       *resource.Object
		cluster *platform.Stage
		want    string // the platform manager, or "" for not the platform's
		changed bool   // a PLATFORM_OBJECT_CHANGED finding
	}{
		{"kubeadm's CoreDNS config", mk("v1", "ConfigMap", "kube-system", "coredns", "kubeadm"), nil, "kubeadm", false},
		{"kubeadm's, with the controller manager's status", mk("apps/v1", "Deployment", "kube-system", "coredns", "kubeadm", "kube-controller-manager"), nil, "kubeadm", false},
		{"CoreDNS config edited by a person", mk("v1", "ConfigMap", "kube-system", "coredns", "kubeadm", "kubectl-edit"), nil, "", true},
		{"kubeadm's SA, no manager, on kind", mk("v1", "ServiceAccount", "kube-system", "coredns"), kindCluster, "kubeadm", false},
		{"same SA name on an unknown cluster", mk("v1", "ServiceAccount", "kube-system", "coredns"), nil, "", false},
		{"kindnet on kind", mk("apps/v1", "DaemonSet", "kube-system", "kindnet", "kubectl-create"), kindCluster, "kind", false},
		{"kindnet on EKS is not kind's", mk("apps/v1", "DaemonSet", "kube-system", "kindnet", "kubectl-create"), eksCluster, "", false},
		{"local-path storage class on kind", localPath, kindCluster, "kind", false},
		{"a user's object in local-path-storage on kind", mk("v1", "ConfigMap", "local-path-storage", "local-path-config", "kubectl-client-side-apply"), kindCluster, "kind", false},
		{"a user's ConfigMap on kind", mk("v1", "ConfigMap", "shop", "settings", "kubectl-client-side-apply"), kindCluster, "", false},
	} {
		p := LabelClassifier{Platform: tc.cluster}.Classify(tc.o)
		got := ""
		if p.Source == snapshotv1alpha1.Source_SOURCE_PLATFORM {
			got = p.Manager
		}
		changed := len(tc.o.Findings) == 1 && tc.o.Findings[0].Code == "PLATFORM_OBJECT_CHANGED"
		if got != tc.want || changed != tc.changed {
			t.Errorf("%s: platform %q, changed finding %v; want %q, %v", tc.name, got, changed, tc.want, tc.changed)
		}
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
