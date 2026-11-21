// reads the manifests of snapshot resources, decoding each one only once, plus small helpers that make text safe for RESTORE.md

package kustomize

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// manifests decodes snapshot resources' manifests, each once, for the playbook's checks, callers must not change what of gives back
// a manifest that doesnt parse means the snapshot wasnt one an export wrote (snapshot.Build marshals every manifest)
type manifests struct {
	// each resource's decoded manifest, nil for one that doesnt parse
	decoded map[*snapshotv1alpha1.Resource]map[string]any
	// the first parse error, kept for writeRestore to give back
	err error
}

// of gives back r's manifest decoded, or nil if it doesnt parse
func (m *manifests) of(r *snapshotv1alpha1.Resource) map[string]any {
	// checking if r was decoded before, if so thats the answer
	if v, ok := m.decoded[r]; ok {
		return v
	}
	var v map[string]any
	// decoding the manifest, checking if it doesnt parse and its the first one, if so the error is kept
	if err := json.Unmarshal([]byte(r.ManifestJson), &v); err != nil && m.err == nil {
		m.err = fmt.Errorf("%s %s/%s: the snapshot's manifest does not parse: %w", r.Kind, r.Namespace, r.Name, err)
	}
	// checking if the cache isnt made yet, the zero manifests has none
	if m.decoded == nil {
		m.decoded = map[*snapshotv1alpha1.Resource]map[string]any{}
	}
	m.decoded[r] = v
	return v
}

// str gives back the string at path in a decoded manifest, or ""
func str(m map[string]any, path ...string) string {
	v, _, _ := unstructured.NestedString(m, path...)
	return v
}

// strSlice gives back the strings at path in a decoded manifest, or nil
func strSlice(m map[string]any, path ...string) []string {
	v, _, _ := unstructured.NestedStringSlice(m, path...)
	return v
}

// labels gives back a decoded manifest's labels
func labels(m map[string]any) map[string]string {
	v, _, _ := unstructured.NestedStringMap(m, "metadata", "labels")
	return v
}

// annotation gives back one annotation of a decoded manifest
func annotation(m map[string]any, key string) string {
	// invoking str to read it, its defined above
	return str(m, "metadata", "annotations", key)
}

// comment makes s safe after "#" on a command line, control characters become spaces
// a newline would end the comment and start a command
func comment(s string) string {
	// mapping every rune of s, control characters to a space
	return strings.Map(func(r rune) rune {
		// checking if r is a control character
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// cell makes s safe inside a markdown table cell, with or without backticks
// no "|" ends the cell, no backtick ends the code span, no line break ends the row
func cell(s string) string {
	return strings.NewReplacer("|", `\|`, "`", "'", "\n", " ", "\r", " ").Replace(s)
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
