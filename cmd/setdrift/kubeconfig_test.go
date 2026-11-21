// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
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

// A kubeconfig that is a directory or can't be read says so plainly, and a ~
// the shell left alone is the home directory.
func TestKubeconfigFileErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if _, err := loadConfig(dir, ""); err == nil || !strings.Contains(err.Error(), "is a directory, not a kubeconfig file") {
		t.Errorf("directory: %v", err)
	}
	if runtime.GOOS != "windows" && os.Getuid() != 0 {
		p := filepath.Join(dir, "config")
		if err := os.WriteFile(p, []byte("apiVersion: v1\nkind: Config\n"), 0o000); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(p, ""); err == nil || !strings.Contains(err.Error(), "can't read it (permission denied)") {
			t.Errorf("unreadable: %v", err)
		}
	}
	if _, err := loadConfig("~/missing", ""); err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "missing")+": no such file") {
		t.Errorf("~: %v", err)
	}
}

// A context that skips the certificate check is named in a warning.
func TestWarnInsecure(t *testing.T) {
	var b strings.Builder
	warnInsecure(&b, &kubeContext{name: "dev", config: &rest.Config{Host: "https://k.example.com"}})
	if b.Len() != 0 {
		t.Errorf("warned for a verified context: %s", b.String())
	}
	warnInsecure(&b, &kubeContext{name: "dev", config: &rest.Config{Host: "https://k.example.com", TLSClientConfig: rest.TLSClientConfig{Insecure: true}}})
	if !strings.Contains(b.String(), "context dev skips the API server's certificate check") {
		t.Errorf("no warning: %q", b.String())
	}
}
