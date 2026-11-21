// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Reading snapshot resources' manifests, each decoded once.

package kustomize

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// manifests decodes snapshot resources' manifests, each once, for the
// playbook's checks; callers must not change what of returns. A manifest
// that does not parse means the snapshot was not one an export wrote
// (snapshot.Build marshals every manifest): the first such error is kept,
// for writeRestore to return.
type manifests struct {
	decoded map[*snapshotv1alpha1.Resource]map[string]any
	err     error
}

// of returns r's manifest, decoded; nil if it does not parse.
func (m *manifests) of(r *snapshotv1alpha1.Resource) map[string]any {
	if v, ok := m.decoded[r]; ok {
		return v
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(r.ManifestJson), &v); err != nil && m.err == nil {
		m.err = fmt.Errorf("%s %s/%s: the snapshot's manifest does not parse: %w", r.Kind, r.Namespace, r.Name, err)
	}
	if m.decoded == nil {
		m.decoded = map[*snapshotv1alpha1.Resource]map[string]any{}
	}
	m.decoded[r] = v
	return v
}

// str returns the string at path in a decoded manifest, or "".
func str(m map[string]any, path ...string) string {
	v, _, _ := unstructured.NestedString(m, path...)
	return v
}

// strSlice returns the strings at path in a decoded manifest, or nil.
func strSlice(m map[string]any, path ...string) []string {
	v, _, _ := unstructured.NestedStringSlice(m, path...)
	return v
}

// labels returns a decoded manifest's labels.
func labels(m map[string]any) map[string]string {
	v, _, _ := unstructured.NestedStringMap(m, "metadata", "labels")
	return v
}

// annotation returns one annotation of a decoded manifest.
func annotation(m map[string]any, key string) string {
	return str(m, "metadata", "annotations", key)
}

// comment makes s safe after "#" on a command line: control characters
// (a newline would end the comment and start a command) become spaces.
func comment(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// cell makes s safe inside a Markdown table cell, with or without
// backticks: no "|" ends the cell, no backtick ends the code span, no line
// break ends the row.
func cell(s string) string {
	return strings.NewReplacer("|", `\|`, "`", "'", "\n", " ", "\r", " ").Replace(s)
}
