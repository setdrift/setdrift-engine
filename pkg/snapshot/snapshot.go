// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Package snapshot builds and serializes snapshots. Protobuf is the canonical
// format; JSON export is for people to read and is not meant to be parsed back.
package snapshot

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// SchemaVersion identifies the snapshot schema written by this package.
const SchemaVersion = "setdrift.snapshot.v1alpha1"

// Meta describes a capture.
type Meta struct {
	ID         string
	CapturedAt time.Time
	Cluster    *snapshotv1alpha1.Cluster
	Producer   *snapshotv1alpha1.Producer
	Findings   []*snapshotv1alpha1.Finding
	// Inventory is copied into the snapshot as is.
	Inventory *snapshotv1alpha1.Inventory
}

// Build assembles a snapshot from pipeline output. Resources are sorted so the
// same cluster state always produces the same bytes.
func Build(meta Meta, objs []*resource.Object) (*snapshotv1alpha1.Snapshot, error) {
	s := &snapshotv1alpha1.Snapshot{
		SchemaVersion: SchemaVersion,
		Id:            meta.ID,
		CapturedAt:    timestamppb.New(meta.CapturedAt),
		Cluster:       meta.Cluster,
		Producer:      meta.Producer,
		// A copy: duplicates are appended below.
		Findings:  slices.Clone(meta.Findings),
		Inventory: meta.Inventory,
	}
	// A resource's identity is group, kind, namespace and name. The version is
	// left out because one object can be read at several versions.
	seen := map[string]bool{}
	for _, o := range objs {
		if o.Partial {
			// Only metadata was collected; emitting it would produce an
			// incomplete manifest.
			return nil, fmt.Errorf("%s: metadata-only object reached the snapshot", o.Ref())
		}
		// Ref is exactly that identity.
		ref := o.Ref()
		if seen[ref] {
			// Possible with --from-dir input. Keep the first copy so the
			// output stays applicable.
			s.Findings = append(s.Findings, resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_WARNING,
				"DUPLICATE_RESOURCE", "%s appears more than once in the input; kept the first copy", ref))
			continue
		}
		seen[ref] = true
		if err := checkRedacted(o); err != nil {
			return nil, err
		}
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
	slices.SortFunc(s.Resources, func(a, b *snapshotv1alpha1.Resource) int {
		return cmp.Or(
			cmp.Compare(group(a.ApiVersion), group(b.ApiVersion)),
			cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(a.Namespace, b.Namespace),
			cmp.Compare(a.Name, b.Name),
		)
	})
	return s, nil
}

// checkRedacted refuses a core Secret with a value the redactor did not
// replace: no secret value may reach a snapshot, whoever calls Build.
func checkRedacted(o *resource.Object) error {
	if o.GroupVersionKind().Group != "" || o.GetKind() != "Secret" {
		return nil
	}
	redacted := map[string]bool{}
	for _, r := range o.Redactions {
		redacted[r.GetPath()] = true
	}
	for _, field := range []string{"data", "stringData"} {
		m, _ := o.Object[field].(map[string]any)
		for k := range m {
			if !redacted[field+"."+k] {
				return fmt.Errorf("%s: %s.%s was not redacted; Secret values must not reach a snapshot (run the Redactor stage)", o.Ref(), field, k)
			}
		}
	}
	return nil
}

// marshalJSON is json.Marshal without HTML escaping, so "a && b" stays
// readable instead of becoming "a \u0026\u0026 b".
func marshalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// Marshal encodes a snapshot as deterministic protobuf.
func Marshal(s *snapshotv1alpha1.Snapshot) ([]byte, error) {
	return proto.MarshalOptions{Deterministic: true}.Marshal(s)
}

// Unmarshal decodes a protobuf snapshot. It fails on a snapshot of another
// schema version, whose fields this version would misread, and on input
// that is not a snapshot: an empty file decodes as valid, empty protobuf.
func Unmarshal(b []byte) (*snapshotv1alpha1.Snapshot, error) {
	if len(b) == 0 {
		return nil, errors.New("not a snapshot: the input is empty")
	}
	s := &snapshotv1alpha1.Snapshot{}
	if err := proto.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("not a snapshot: %w", err)
	}
	switch v := s.GetSchemaVersion(); v {
	case SchemaVersion:
		return s, nil
	case "":
		return nil, errors.New("not a snapshot: no schema version")
	default:
		return nil, fmt.Errorf("snapshot schema %s is not supported by this version of setdrift (supported: %s)", v, SchemaVersion)
	}
}

// MarshalInspectJSON renders a snapshot as indented, stable JSON, with each
// resource's manifest inlined as an object instead of an escaped string.
func MarshalInspectJSON(s *snapshotv1alpha1.Snapshot) ([]byte, error) {
	b, err := protojson.Marshal(s)
	if err != nil {
		return nil, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if raw, ok := doc["resources"]; ok {
		var resources []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &resources); err != nil {
			return nil, err
		}
		for _, r := range resources {
			var manifest string
			if err := json.Unmarshal(r["manifestJson"], &manifest); err != nil {
				return nil, err
			}
			r["manifest"] = json.RawMessage(manifest)
			delete(r, "manifestJson")
		}
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

func group(apiVersion string) string {
	g, _, found := strings.Cut(apiVersion, "/")
	if !found {
		return ""
	}
	return g
}
