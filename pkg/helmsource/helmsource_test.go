package helmsource

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestAnnotate(t *testing.T) {
	in := `---
# Source: chart/templates/sa.yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: web
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  annotations:
    team: a
spec:
  template:
    metadata:
      labels: {app: web}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg
  annotations: null
`
	var out bytes.Buffer
	if err := Annotate(strings.NewReader(in), &out, "https://charts.example.com"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if n := strings.Count(got, "setdrift.io/chart-repo: https://charts.example.com"); n != 3 {
		t.Errorf("%d annotations:\n%s", n, got)
	}
	if !strings.Contains(got, "team: a") || strings.Contains(got, "template:\n      metadata:\n        annotations") {
		t.Errorf("changed more than metadata.annotations:\n%s", got)
	}
	if err := Annotate(strings.NewReader(in), &out, ""); err == nil {
		t.Error("no repo accepted")
	}
}

// A repository URL with credentials is refused, with nothing written, in
// both URL forms Helm takes.
func TestAnnotateRefusesCredentials(t *testing.T) {
	for _, repo := range []string{"https://user:token@charts.example.com", "oci://user@registry.example.com/charts"} {
		var out bytes.Buffer
		err := Annotate(strings.NewReader("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\n"), &out, repo)
		if err == nil || !strings.Contains(err.Error(), "credentials") {
			t.Errorf("%s: got error %v, want one about credentials", repo, err)
		}
		if err != nil && strings.Contains(err.Error(), "token") || out.Len() > 0 {
			t.Errorf("%s: error repeats the credentials or output was written", repo)
		}
	}
}

// Annotate stops reading past its limit.
func TestCappedReader(t *testing.T) {
	if b, err := io.ReadAll(&cappedReader{r: strings.NewReader("abcd"), n: 5}); err != nil || string(b) != "abcd" {
		t.Errorf("under the limit: %q, %v", b, err)
	}
	if _, err := io.ReadAll(&cappedReader{r: strings.NewReader("abcdef"), n: 5}); !errors.Is(err, errTooLarge) {
		t.Errorf("over the limit: %v", err)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
