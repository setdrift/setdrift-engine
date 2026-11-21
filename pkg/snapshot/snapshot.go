// Package snapshot builds and serializes snapshots, protobuf is the canonical format
// the JSON export is for people to read and isnt meant as a format to build on
package snapshot

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"setdrift.io/setdrift-engine/internal/kube"
	"setdrift.io/setdrift-engine/pkg/redact"
	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// SchemaVersion identifies the snapshot schema this package writes, and the only one it reads
const SchemaVersion = "setdrift.snapshot.v1alpha1"

// the modes of a snapshot's ReadScope, what the credentials let a capture read
const (
	// ScopeCluster is all of the cluster
	ScopeCluster = "cluster"
	// ScopeNamespaces is the namespaces asked for, with export -n
	ScopeNamespaces = "namespaces"
	// ScopeContextNamespace is the kubeconfig context's namespace only, the credentials may not list namespaces
	ScopeContextNamespace = "context-namespace"
	// ScopeReadableNamespaces is the namespaces the credentials can read, when they can list namespaces but not read across them
	ScopeReadableNamespaces = "readable-namespaces"
)

// Meta describes a capture, everything Build puts in a snapshot besides the resources
type Meta struct {
	ID         string
	CapturedAt time.Time
	Cluster    *snapshotv1alpha1.Cluster
	Producer   *snapshotv1alpha1.Producer
	// findings about the run itself rather than one resource
	Findings []*snapshotv1alpha1.Finding
	// copied into the snapshot as is
	Inventory *snapshotv1alpha1.Inventory
	// what was read, the collector's, copied as is
	Scope *snapshotv1alpha1.ReadScope
}

// Build takes the capture's meta and the pipeline's output and assembles a snapshot, refusing partial objects and unredacted Secrets
// resources are sorted so the same cluster state always produces the same bytes
func Build(meta Meta, objs []*resource.Object) (*snapshotv1alpha1.Snapshot, error) {
	s := &snapshotv1alpha1.Snapshot{
		SchemaVersion: SchemaVersion,
		Id:            meta.ID,
		CapturedAt:    timestamppb.New(meta.CapturedAt),
		Cluster:       meta.Cluster,
		Producer:      meta.Producer,
		// a copy, since duplicate findings are appended below
		Findings:  slices.Clone(meta.Findings),
		Inventory: meta.Inventory,
		Scope:     meta.Scope,
	}
	// a resource's identity is group, kind, namespace and name
	// the version is left out because one object can be read at several versions
	seen := map[string]bool{}
	// going over each object to check it and add it as a resource
	for _, o := range objs {
		// checking if only metadata was collected, emitting it would produce an incomplete manifest, so thats a terminal error
		if o.Partial {
			return nil, fmt.Errorf("%s: metadata-only object reached the snapshot", o.Ref())
		}
		// invoking Ref, which is exactly that identity, its defined in pkg/resource/resource.go
		ref := o.Ref()
		// checking if the same resource came already, possible with --from-dir input, the first copy is kept so the output stays applicable
		if seen[ref] {
			// invoking resource.NewFinding to warn about the duplicate, its defined in pkg/resource/resource.go
			s.Findings = append(s.Findings, resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_WARNING,
				"DUPLICATE_RESOURCE", "%s appears more than once in the input; kept the first copy", ref))
			continue
		}
		seen[ref] = true
		// invoking checkRedacted to refuse a Secret with a value left in, its defined below
		if err := checkRedacted(o); err != nil {
			return nil, err
		}
		// invoking marshalJSON to write the manifest without HTML escaping, its defined below
		manifest, err := marshalJSON(o.Object)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", o.Ref(), err)
		}
		s.Resources = append(s.Resources, &snapshotv1alpha1.Resource{
			ApiVersion:   o.GetAPIVersion(),
			Kind:         o.GetKind(),
			Namespace:    o.GetNamespace(),
			Name:         o.GetName(),
			SourceUid:    cmp.Or(o.SourceUID, string(o.GetUID())),
			ManifestJson: string(manifest),
			Provenance:   o.Provenance,
			Redactions:   o.Redactions,
			Findings:     o.Findings,
		})
	}
	// sorting the resources by group, kind, namespace and name, invoking kube.Group for the group, its defined in internal/kube/kube.go
	slices.SortFunc(s.Resources, func(a, b *snapshotv1alpha1.Resource) int {
		return cmp.Or(
			cmp.Compare(kube.Group(a.ApiVersion), kube.Group(b.ApiVersion)),
			cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(a.Namespace, b.Namespace),
			cmp.Compare(a.Name, b.Name),
		)
	})
	return s, nil
}

// checkRedacted takes an object and refuses a core Secret with a value the redactor didnt replace, so no secret value can reach a snapshot, whoever calls Build
func checkRedacted(o *resource.Object) error {
	// checking if its a core Secret, anything else has nothing to check
	if o.GroupVersionKind().Group != "" || o.GetKind() != "Secret" {
		return nil
	}
	redacted := map[string]bool{}
	// going over the redaction records to note each path that was redacted
	for _, r := range o.Redactions {
		redacted[r.GetPath()] = true
	}
	// going over both places a Secret holds values
	for _, field := range []string{"data", "stringData"} {
		m, _ := o.Object[field].(map[string]any)
		// going over each key in sorted order, so the error always names the same key
		for _, k := range slices.Sorted(maps.Keys(m)) {
			v := m[k]
			// checking if the key has a redaction record and its value is a placeholder, invoking placeholder for that, its defined below
			// both are needed, if either is missing thats a terminal error
			if !redacted[field+"."+k] || !placeholder(k, v, field == "data") {
				return fmt.Errorf("%s: %s.%s was not redacted; Secret values must not reach a snapshot (run the Redactor stage)", o.Ref(), field, k)
			}
		}
	}
	return nil
}

// placeholder tells if v, a Secret's value for key, is one of the redactor's placeholders, base64-encoded when encoded, like under data
// a redaction record alone isnt proof the value was replaced
func placeholder(key string, v any, encoded bool) bool {
	s, ok := v.(string)
	// checking if the value is a string at all, anything else cant be a placeholder
	if !ok {
		return false
	}
	// checking if the value is base64-encoded, if so its decoded first
	if encoded {
		b, err := base64.StdEncoding.DecodeString(s)
		// checking if it isnt valid base64, if so it cant be a placeholder
		if err != nil {
			return false
		}
		s = string(b)
	}
	// checking if its one of the redactor's placeholders, theyre defined in pkg/redact/redact.go
	switch s {
	case redact.Placeholder, redact.DockerConfigJSONPlaceholder, redact.DockerCfgPlaceholder:
		return true
	}
	return false
}

// marshalJSON is json.Marshal without HTML escaping, so "a && b" stays readable instead of becoming "a \u0026\u0026 b"
func marshalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// dropping the newline Encode adds, json.Marshal adds none
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// Marshal encodes a snapshot as deterministic protobuf, so the same snapshot always gives the same bytes
func Marshal(s *snapshotv1alpha1.Snapshot) ([]byte, error) {
	return proto.MarshalOptions{Deterministic: true}.Marshal(s)
}

// Unmarshal decodes a protobuf snapshot
// it fails on a snapshot of another schema version, whose fields this version would misread, and on input that isnt a snapshot
func Unmarshal(b []byte) (*snapshotv1alpha1.Snapshot, error) {
	// checking if the input is empty, an empty file decodes as valid, empty protobuf, so its refused here
	if len(b) == 0 {
		return nil, errors.New("not a snapshot: the input is empty")
	}
	s := &snapshotv1alpha1.Snapshot{}
	if err := proto.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("not a snapshot: %w", err)
	}
	// invoking checkSchema to refuse another schema version, its defined below
	return checkSchema(s)
}

// checkSchema gives back s when this version reads its schema, and an error saying why otherwise
func checkSchema(s *snapshotv1alpha1.Snapshot) (*snapshotv1alpha1.Snapshot, error) {
	// checking the schema version, ours, none at all, or another one
	switch v := s.GetSchemaVersion(); v {
	case SchemaVersion:
		return s, nil
	case "":
		return nil, errors.New("not a snapshot: no schema version")
	default:
		return nil, fmt.Errorf("snapshot schema %s is not supported by this version of setdrift (supported: %s)", v, SchemaVersion)
	}
}

// Parse decodes a snapshot in either format export writes, protobuf, or the JSON of MarshalInspectJSON, the .json beside each .pb
func Parse(b []byte) (*snapshotv1alpha1.Snapshot, error) {
	// checking if the input starts with a brace, if so its the JSON, invoking UnmarshalInspectJSON, its defined below
	if t := bytes.TrimSpace(b); len(t) > 0 && t[0] == '{' {
		return UnmarshalInspectJSON(b)
	}
	// invoking Unmarshal for protobuf, its defined above
	return Unmarshal(b)
}

// UnmarshalInspectJSON decodes the JSON of MarshalInspectJSON, each resource's inlined manifest goes back to its manifestJson string
// like Unmarshal, it refuses another schema version
func UnmarshalInspectJSON(b []byte) (*snapshotv1alpha1.Snapshot, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("not a snapshot: %w", err)
	}
	// checking if the snapshot has resources, if so their manifests are turned back into strings
	if raw, ok := doc["resources"]; ok {
		var resources []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &resources); err != nil {
			return nil, fmt.Errorf("not a snapshot: resources: %w", err)
		}
		// going over each resource
		for _, r := range resources {
			// checking if it has an inlined manifest, if so its compacted and stored back as the manifestJson string
			if m, ok := r["manifest"]; ok {
				compact := &bytes.Buffer{}
				if err := json.Compact(compact, m); err != nil {
					return nil, fmt.Errorf("not a snapshot: manifest: %w", err)
				}
				str, err := json.Marshal(compact.String())
				if err != nil {
					return nil, err
				}
				r["manifestJson"] = str
				delete(r, "manifest")
			}
		}
		var err error
		// encoding the resources back into the document, a failure here is a terminal error
		if doc["resources"], err = json.Marshal(resources); err != nil {
			return nil, err
		}
	}
	back, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	s := &snapshotv1alpha1.Snapshot{}
	if err := protojson.Unmarshal(back, s); err != nil {
		return nil, fmt.Errorf("not a snapshot: %w", err)
	}
	// invoking checkSchema to refuse another schema version, its defined above
	return checkSchema(s)
}

// MarshalInspectJSON renders a snapshot as indented, stable JSON for people to read, with each resource's manifest inlined as an object instead of an escaped string
func MarshalInspectJSON(s *snapshotv1alpha1.Snapshot) ([]byte, error) {
	b, err := protojson.Marshal(s)
	if err != nil {
		return nil, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	// checking if the snapshot has resources, if so their manifests are inlined
	if raw, ok := doc["resources"]; ok {
		var resources []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &resources); err != nil {
			return nil, err
		}
		// going over each resource, swapping its manifestJson string for the manifest itself
		for _, r := range resources {
			var manifest string
			if err := json.Unmarshal(r["manifestJson"], &manifest); err != nil {
				return nil, err
			}
			r["manifest"] = json.RawMessage(manifest)
			delete(r, "manifestJson")
		}
		// invoking marshalJSON to keep the manifests free of HTML escaping, its defined above
		if doc["resources"], err = marshalJSON(resources); err != nil {
			return nil, err
		}
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
