package snapshot

import (
	"encoding/base64"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/redact"
	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

func obj(apiVersion, kind, ns, name string) *resource.Object {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(ns)
	u.SetName(name)
	return resource.New(u)
}

func TestBuild(t *testing.T) {
	findings := make([]*snapshotv1alpha1.Finding, 0, 4)
	objs := []*resource.Object{
		obj("v1", "Service", "b", "web"),
		obj("apps/v1", "Deployment", "a", "web"),
		obj("v1", "ConfigMap", "a", "x"),
		obj("v1", "ConfigMap", "a", "x"), // duplicate
		obj("v1beta1", "ConfigMap", "a", "y"),
	}
	s, err := Build(Meta{Findings: findings}, objs)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range s.Resources {
		got = append(got, r.Kind+"/"+r.Namespace+"/"+r.Name)
	}
	want := []string{"ConfigMap/a/x", "ConfigMap/a/y", "Service/b/web", "Deployment/a/web"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (core group first, then group, kind, namespace, name)", got, want)
		}
	}
	if len(s.Findings) != 1 || s.Findings[0].Code != "DUPLICATE_RESOURCE" {
		t.Errorf("findings %v", s.Findings)
	}
	if findings[:1][0] != nil {
		t.Error("Build wrote into the caller's Findings backing array")
	}
}

func TestBuildRejectsPartial(t *testing.T) {
	o := obj("v1", "Pod", "a", "p")
	o.Partial = true
	if _, err := Build(Meta{}, []*resource.Object{o}); err == nil {
		t.Error("metadata-only object accepted")
	}
}

func TestBuildRejectsUnredactedSecret(t *testing.T) {
	o := obj("v1", "Secret", "a", "db")
	o.Object["data"] = map[string]any{"password": "aHVudGVyMg==", "user": "YWRtaW4="}
	o.Redactions = []*snapshotv1alpha1.Redaction{{Path: "data.user"}}
	if _, err := Build(Meta{}, []*resource.Object{o}); err == nil || !strings.Contains(err.Error(), "data.password") {
		t.Errorf("unredacted value accepted: %v", err)
	}
	// a redaction record for every key is not enough: the values are still real
	o.Redactions = append(o.Redactions, &snapshotv1alpha1.Redaction{Path: "data.password"})
	if _, err := Build(Meta{}, []*resource.Object{o}); err == nil {
		t.Error("Secret with redaction records but its real values accepted")
	}
	// stringData is checked too, unencoded
	p := obj("v1", "Secret", "a", "plain")
	p.Object["stringData"] = map[string]any{"token": "s3cr3t"}
	p.Redactions = []*snapshotv1alpha1.Redaction{{Path: "stringData.token"}}
	if _, err := Build(Meta{}, []*resource.Object{p}); err == nil {
		t.Error("stringData with its real value accepted")
	}
	// the placeholders themselves pass
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	o.Object["data"] = map[string]any{"password": enc(redact.Placeholder), "user": enc(redact.Placeholder)}
	p.Object["stringData"] = map[string]any{"token": redact.Placeholder}
	pull := obj("v1", "Secret", "a", "pull")
	pull.Object["data"] = map[string]any{".dockerconfigjson": enc(redact.DockerConfigJSONPlaceholder), ".dockercfg": enc(redact.DockerCfgPlaceholder)}
	pull.Redactions = []*snapshotv1alpha1.Redaction{{Path: "data..dockerconfigjson"}, {Path: "data..dockercfg"}}
	if _, err := Build(Meta{}, []*resource.Object{o, p, pull}); err != nil {
		t.Errorf("redacted Secrets refused: %v", err)
	}
	// Not a core Secret: the embedded-secret redactor's business, not a guard here.
	cr := obj("example.com/v1", "Secret", "a", "x")
	cr.Object["data"] = map[string]any{"k": "v"}
	if _, err := Build(Meta{}, []*resource.Object{cr}); err != nil {
		t.Errorf("custom kind refused: %v", err)
	}
}

func TestUnmarshalChecksSchema(t *testing.T) {
	for name, in := range map[string]*snapshotv1alpha1.Snapshot{
		"other version": {SchemaVersion: "setdrift.snapshot.v2"},
		"no version":    {Id: "x"},
	} {
		b, err := Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Unmarshal(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Unmarshal(nil); err == nil {
		t.Error("empty input accepted")
	}
	if _, err := Unmarshal([]byte("apiVersion: v1\n")); err == nil {
		t.Error("YAML accepted")
	}
	b, _ := Marshal(&snapshotv1alpha1.Snapshot{SchemaVersion: SchemaVersion})
	if _, err := Unmarshal(b); err != nil {
		t.Errorf("current version refused: %v", err)
	}
}

func TestNoHTMLEscaping(t *testing.T) {
	o := obj("v1", "ConfigMap", "a", "x")
	o.Object["data"] = map[string]any{"cmd": "a && b <c>"}
	s, err := Build(Meta{}, []*resource.Object{o})
	if err != nil {
		t.Fatal(err)
	}
	if m := s.Resources[0].ManifestJson; !strings.Contains(m, "a && b <c>") {
		t.Errorf("manifest escaped: %s", m)
	}
	js, err := MarshalInspectJSON(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "a && b <c>") {
		t.Errorf("inspect JSON escaped: %s", js)
	}
}

// The JSON export writes beside each .pb reads back as the same snapshot.
func TestParseJSONRoundTrip(t *testing.T) {
	s := &snapshotv1alpha1.Snapshot{SchemaVersion: SchemaVersion, Id: "x",
		Cluster: &snapshotv1alpha1.Cluster{Name: "prod"},
		Resources: []*snapshotv1alpha1.Resource{{ApiVersion: "v1", Kind: "ConfigMap", Namespace: "shop", Name: "a",
			ManifestJson: `{"apiVersion":"v1","data":{"k":"v <&>"},"kind":"ConfigMap"}`}}}
	js, err := MarshalInspectJSON(s)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(js)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(s, back) {
		t.Errorf("round trip:\n got %v\nwant %v", back, s)
	}
	pb, _ := Marshal(s)
	if fromPB, err := Parse(pb); err != nil || !proto.Equal(s, fromPB) {
		t.Errorf("protobuf through Parse: %v", err)
	}
	if _, err := Parse([]byte(`{"hello": 1}`)); err == nil {
		t.Error("JSON that is no snapshot parsed")
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
