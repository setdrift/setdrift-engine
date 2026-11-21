// fuzzes ParseKeyTemplate and Execute against text/template itself

package text

import (
	"strings"
	"testing"
	"text/template"
)

// check that any template ParseKeyTemplate accepts gives the same key text/template gives
func FuzzKeyTemplate(f *testing.F) {
	for _, s := range []string{"{{.Cluster}}/{{.Namespace}}/{{.Name}}", "/p/{{ .Name -}} x", "{{/* c */}}{{.Name}}", "a{{.Name}}{{.Name}}", "{{.Nope}}", "{{"} {
		f.Add(s, "prod", "shop", "db")
	}
	f.Fuzz(func(t *testing.T, s, cluster, namespace, name string) {
		k, err := ParseKeyTemplate(s)
		if err != nil {
			return
		}
		tmpl, err := template.New("key").Option("missingkey=error").Parse(s)
		if err != nil {
			t.Fatalf("%q: accepted here, refused by text/template: %v", s, err)
		}
		var b strings.Builder
		if err := tmpl.Execute(&b, map[string]string{"Cluster": cluster, "Namespace": namespace, "Name": name}); err != nil {
			t.Fatalf("%q: accepted here, text/template fails: %v", s, err)
		}
		if got := k.Execute(cluster, namespace, name); got != b.String() {
			t.Fatalf("%q gives %q, text/template %q", s, got, b.String())
		}
	})
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
