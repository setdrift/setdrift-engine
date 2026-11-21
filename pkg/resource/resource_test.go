// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func object(apiVersion, kind, ns, name string) *Object {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(ns)
	u.SetName(name)
	return New(u)
}

func TestRef(t *testing.T) {
	for _, c := range []struct {
		o    *Object
		want string
	}{
		{object("apps/v1", "Deployment", "shop", "web"), "apps/Deployment shop/web"},
		{object("rbac.authorization.k8s.io/v1", "ClusterRole", "", "view"), "rbac.authorization.k8s.io/ClusterRole view"},
		{object("v1", "ConfigMap", "shop", "a"), "ConfigMap shop/a"},
	} {
		if got := c.o.Ref(); got != c.want {
			t.Errorf("Ref() = %q, want %q", got, c.want)
		}
	}
}

func TestCaptureTime(t *testing.T) {
	if _, ok := CaptureTime(context.Background()); ok {
		t.Error("CaptureTime on a context without one: ok")
	}
	if _, ok := CaptureTime(WithCaptureTime(context.Background(), time.Time{})); ok {
		t.Error("a zero capture time counts as set")
	}
	at := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	if got, ok := CaptureTime(WithCaptureTime(context.Background(), at)); !ok || !got.Equal(at) {
		t.Errorf("CaptureTime = %v, %v; want %v, true", got, ok, at)
	}
}

func TestClaimTemplatePrefixes(t *testing.T) {
	sts := object("apps/v1", "StatefulSet", "shop", "db")
	if err := unstructured.SetNestedSlice(sts.Object, []any{map[string]any{"metadata": map[string]any{"name": "data"}}},
		"spec", "volumeClaimTemplates"); err != nil {
		t.Fatal(err)
	}
	got := ClaimTemplatePrefixes([]*Object{sts, object("v1", "ConfigMap", "shop", "x")})
	if len(got) != 1 || got["shop/data-db-"] != "db" {
		t.Errorf("ClaimTemplatePrefixes = %v, want shop/data-db- -> db", got)
	}
}
