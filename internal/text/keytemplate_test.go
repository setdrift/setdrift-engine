// tests the remote secret key templates, what they fill in, what they refuse and which fields they use

package text

import (
	"strings"
	"testing"
)

// check that key templates fill in the cluster, namespace and name, with trim markers and comments handled like text/template
func TestKeyTemplate(t *testing.T) {
	for in, want := range map[string]string{
		"{{.Cluster}}/{{.Namespace}}/{{.Name}}": "prod/shop/db",
		"/prod/{{.Namespace}}/{{.Name}}":        "/prod/shop/db",
		"{{ .Name }}-{{- .Namespace -}} x":      "db-shopx",
		"plain":                                 "plain",
		"":                                      "",
		"{{/* a comment */}}{{.Name}}":          "db",
	} {
		k, err := ParseKeyTemplate(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got := k.Execute("prod", "shop", "db"); got != want {
			t.Errorf("%q gives %q, want %q", in, got, want)
		}
	}
}

// check that anything but text and the known fields is refused with a message saying why
func TestKeyTemplateRefuses(t *testing.T) {
	for in, want := range map[string]string{
		"{{.Name":                           "unclosed action",
		"{{.Namespce}}/{{.Name}}":           "unknown field .Namespce",
		"{{.Name | printf \"%s\"}}":         "not defined",
		"{{printf \"%s\" .Name}}":           "function \"printf\" not defined",
		"{{if .Name}}x{{end}}":              "not supported",
		"{{$x := .Name}}{{$x}}":             "not supported",
		"{{.Name.Inner}}":                   "not supported",
		"{{define \"a\"}}x{{end}}{{.Name}}": "define and block",
		"{{.}}":                             "not supported",
	} {
		_, err := ParseKeyTemplate(in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want one containing %q", in, err, want)
		}
	}
}

// check that Uses tells the fields a template names from the ones it doesnt
func TestKeyTemplateUses(t *testing.T) {
	k, err := ParseKeyTemplate("{{.Namespace}}/x")
	if err != nil {
		t.Fatal(err)
	}
	if !k.Uses("Namespace") || k.Uses("Name") {
		t.Error("Uses is wrong")
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
