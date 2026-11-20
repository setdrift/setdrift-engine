// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package collect_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"setdrift.io/setdrift-engine/pkg/collect"
	"setdrift.io/setdrift-engine/pkg/pipeline"
)

// grant answers access reviews: the credentials may list in the namespaces
// given (and across the cluster only if "" is one), and are identity.
func grant(dyn *dynamicfake.FakeDynamicClient, identity string, namespaces ...string) {
	allowed := map[string]bool{}
	for _, ns := range namespaces {
		allowed[ns] = true
	}
	dyn.PrependReactor("create", "selfsubjectaccessreviews", func(a clienttesting.Action) (bool, runtime.Object, error) {
		u := a.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured)
		ns, _, _ := unstructured.NestedString(u.Object, "spec", "resourceAttributes", "namespace")
		out := u.DeepCopy()
		_ = unstructured.SetNestedField(out.Object, allowed[ns], "status", "allowed")
		return true, out, nil
	})
	dyn.PrependReactor("create", "selfsubjectreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview",
			"status": map[string]any{"userInfo": map[string]any{"username": identity}},
		}}, nil
	})
	// lists in namespaces not allowed are refused, as RBAC refuses them
	dyn.PrependReactor("list", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetResource().Resource == "namespaces" || allowed[a.GetNamespace()] {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: a.GetResource().Resource}, "", errors.New("no"))
	})
}

func inNamespace(u *unstructured.Unstructured, ns string) *unstructured.Unstructured {
	u.SetNamespace(ns)
	return u
}

func TestCheckNamespaces(t *testing.T) {
	_, dyn := fakeCluster([]apiType{namespaceType, configMapType},
		owned("v1", "Namespace", "shop", "n1", nil), owned("v1", "Namespace", "prod", "n2", nil))
	grant(dyn, "system:serviceaccount:default:reader", "shop")
	ctx := context.Background()

	if err := collect.CheckNamespaces(ctx, dyn, []string{"shop"}, false, true); err != nil {
		t.Errorf("readable: %v", err)
	}
	err := collect.CheckNamespaces(ctx, dyn, []string{"foo"}, false, true)
	var nsErr *collect.NamespaceError
	if !errors.As(err, &nsErr) || len(nsErr.Missing) != 1 || !strings.Contains(err.Error(), "Namespace foo does not exist in this cluster; its namespaces are prod, shop") {
		t.Errorf("missing: %v", err)
	}
	err = collect.CheckNamespaces(ctx, dyn, []string{"shop", "prod"}, false, true)
	if !errors.As(err, &nsErr) || len(nsErr.Unreadable) != 1 || nsErr.Unreadable[0] != "prod" ||
		!strings.Contains(err.Error(), "(system:serviceaccount:default:reader) can't read anything in namespace prod") ||
		!strings.Contains(err.Error(), "--serviceaccount=default:reader") {
		t.Errorf("unreadable: %v", err)
	}
	// missing namespaces are only findings unless required
	if err := collect.CheckNamespaces(ctx, dyn, []string{"foo"}, false, false); err != nil && strings.Contains(err.Error(), "does not exist") {
		t.Errorf("not required: %v", err)
	}
}

// Credentials that can list namespaces but not read across them read the
// namespaces they can, and the snapshot says which they could not.
func TestReadableNamespacesOnly(t *testing.T) {
	disc, dyn := fakeCluster([]apiType{namespaceType, configMapType},
		owned("v1", "Namespace", "shop", "n1", nil), owned("v1", "Namespace", "prod", "n2", nil),
		owned("v1", "ConfigMap", "in-shop", "c1", nil), inNamespace(owned("v1", "ConfigMap", "in-prod", "c2", nil), "prod"))
	grant(dyn, "stage-reader", "shop")
	snap, err := pipeline.New(&collect.ClusterCollector{Discovery: disc, Dynamic: dyn}).Run(context.Background(), raceMeta())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(snap), " "); got != "ConfigMap/in-shop Namespace/shop" {
		t.Errorf("exported %s, want only shop's ConfigMap and Namespace", got)
	}
	sc := snap.GetScope()
	if sc.GetMode() != "readable-namespaces" || strings.Join(sc.GetNamespaces(), ",") != "shop" ||
		strings.Join(sc.GetUnreadable(), ",") != "prod" || sc.GetIdentity() != "stage-reader" {
		t.Errorf("scope = %v", sc)
	}

	// reading nothing anywhere is an error, not an empty export
	disc, dyn = fakeCluster([]apiType{namespaceType, configMapType}, owned("v1", "Namespace", "shop", "n1", nil))
	grant(dyn, "nobody")
	_, err = (&collect.ClusterCollector{Discovery: disc, Dynamic: dyn}).Collect(context.Background())
	var nothing *collect.ReadNothingError
	if !errors.As(err, &nothing) || !strings.Contains(err.Error(), "can list the cluster's 1 namespaces but read nothing") {
		t.Errorf("read nothing: %v", err)
	}
}

// A namespace that refuses a type is skipped for it, not the type everywhere.
func TestListDeniedInOneNamespace(t *testing.T) {
	disc, dyn := fakeCluster([]apiType{namespaceType, configMapType},
		owned("v1", "Namespace", "shop", "n1", nil), owned("v1", "Namespace", "stage", "n2", nil),
		owned("v1", "ConfigMap", "in-shop", "c1", nil), inNamespace(owned("v1", "ConfigMap", "in-stage", "c2", nil), "stage"))
	grant(dyn, "mixed", "shop", "stage")
	dyn.PrependReactor("list", "configmaps", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "stage" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "", errors.New("no"))
		}
		return false, nil, nil
	})
	c := &collect.ClusterCollector{Discovery: disc, Dynamic: dyn, Namespaces: []string{"shop", "stage"}, RequireNamespaces: true}
	snap, err := pipeline.New(c).Run(context.Background(), raceMeta())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(snap), " "); got != "ConfigMap/in-shop Namespace/shop Namespace/stage" {
		t.Errorf("exported %s", got)
	}
	if msg := codes(snap)["LIST_DENIED"]; !strings.Contains(msg, "in namespace stage, these credentials (mixed) may not list 1 type") {
		t.Errorf("LIST_DENIED = %q", msg)
	}
	if u := snap.GetScope().GetUnread(); len(u) != 1 || u[0].GetNamespace() != "stage" || u[0].GetKind() != "ConfigMap" {
		t.Errorf("unread = %v", u)
	}
}
