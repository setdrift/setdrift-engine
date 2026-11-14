// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestKubeconfigErrors(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	two := `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
users: [{name: u, user: {token: t}}]
contexts: [{name: a, context: {cluster: c, user: u}}, {name: b, context: {cluster: c, user: u}}]
`
	for _, c := range []struct {
		name, kubeconfig, context, want string
	}{
		{"missing --kubeconfig", filepath.Join(dir, "nope"), "", "no such file"},
		{"empty", write("empty", ""), "", "no kubeconfig found"},
		{"malformed", write("bad", "clusters: [ {"), "", "reading the kubeconfig"},
		{"no current context", write("two", two), "", "pass --context with one of a, b"},
		{"unknown context", filepath.Join(dir, "two"), "prod", `no context "prod"; its contexts are: a, b`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("KUBECONFIG", "")
			_, err := loadConfig(c.kubeconfig, c.context)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want %q", err, c.want)
			}
			if err != nil && strings.Contains(err.Error(), "KUBERNETES_MASTER") {
				t.Errorf("client-go's hint leaked: %v", err)
			}
		})
	}
}

func TestUnreachableErrors(t *testing.T) {
	gr := schema.GroupResource{}
	for _, c := range []struct {
		err  error
		want string
	}{
		{apierrors.NewUnauthorized("no"), "rejected the credentials of context prod (401"},
		{apierrors.NewForbidden(gr, "", errors.New("no")), "403 Forbidden"},
		{errors.New(`dial tcp 127.0.0.1:1: connect: connection refused`), "connection refused. Is the cluster running?"},
		{errors.New(`Get "https://x/version": getting credentials: exec: executable aws not found`), "running `aws`, which is not installed"},
		{errors.New("x509: certificate signed by unknown authority"), "certificate authority may be out of date"},
	} {
		if got := explainUnreachable(c.err, "prod", "https://x").Error(); !strings.Contains(got, c.want) {
			t.Errorf("%v: got %q, want %q", c.err, got, c.want)
		}
	}
}
