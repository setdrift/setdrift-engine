// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Package secretexport copies the values of an export's redacted Secrets
// from the source cluster straight into a secret manager, so the
// ExternalSecrets the export wrote can restore them in another cluster.
//
// It is the one part of SetDrift that handles secret values, and it is
// separate from the export: `setdrift export` never reads a value, and
// writes ExternalSecrets that name, for each Secret, a remote key and its
// properties. This package reads those ExternalSecrets, reads each Secret's
// live values with the user's own credentials, and writes them into the
// secret manager with the user's own cloud credentials. Values are held in
// memory only, never written to disk or printed: plans and reports show
// remote keys, property names and Secret names.
//
// Remote secrets that already exist are skipped unless overwriting is asked
// for; the plan says which.
package secretexport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"sigs.k8s.io/yaml"
)

// Provider is a secret manager.
type Provider interface {
	// Name is shown in plans, e.g. "AWS Secrets Manager".
	Name() string
	// Exists reports whether the remote secret key exists.
	Exists(ctx context.Context, key string) (bool, error)
	// Put writes value (a JSON object, one property per Secret key) under
	// key, creating it, or replacing its value when overwrite is set.
	Put(ctx context.Context, key, value string, overwrite bool) error
}

// SecretReader reads a Secret's data from the source cluster.
type SecretReader interface {
	// Get returns the Secret's data, decoded; an error wrapping
	// fs.ErrNotExist when there is no such Secret.
	Get(ctx context.Context, namespace, name string) (map[string][]byte, error)
}

// Remote is one remote secret an ExternalSecret reads: a key, and which
// Secret key each property becomes.
type Remote struct {
	Key       string
	Namespace string
	Secret    string // the Secret the ExternalSecret creates
	// Properties maps each property in the remote secret to the Secret key it fills.
	Properties map[string]string
	// File is the ExternalSecret's file, relative to the cluster's directory.
	File string
}

// Status of an item in a plan.
type Status string

// Statuses.
const (
	Create    Status = "create"
	Overwrite Status = "overwrite"
	Skip      Status = "skip: exists"
	Missing   Status = "missing"
	Failed    Status = "failed"
	Done      Status = "done"
)

// Item is one remote secret in a plan.
type Item struct {
	Remote
	Status Status
	// Binary lists the properties whose values are not text: they are stored
	// base64-encoded, and the ExternalSecret decodes them (decodingStrategy).
	Binary []string
	// Note explains a Missing or Failed status.
	Note  string
	value string // the JSON to write; never printed
}

// Plan is what Apply would do.
type Plan struct {
	Provider string
	Items    []*Item
}

// Load reads the remote secrets named by the ExternalSecrets in an export's
// external-secrets/ directory (root is the cluster's directory).
func Load(root string) ([]Remote, error) {
	dir := filepath.Join(root, "external-secrets")
	var out []Remote
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".yaml" || d.Name() == "kustomization.yaml" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var es struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				Target struct {
					Name string `json:"name"`
				} `json:"target"`
				Data []struct {
					SecretKey string `json:"secretKey"`
					RemoteRef struct {
						Key      string `json:"key"`
						Property string `json:"property"`
					} `json:"remoteRef"`
				} `json:"data"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(b, &es); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if es.Kind != "ExternalSecret" {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		byKey := map[string]*Remote{}
		var keys []string
		for _, d := range es.Spec.Data {
			r := byKey[d.RemoteRef.Key]
			if r == nil {
				target := es.Spec.Target.Name
				if target == "" {
					target = es.Metadata.Name
				}
				r = &Remote{Key: d.RemoteRef.Key, Namespace: es.Metadata.Namespace, Secret: target, Properties: map[string]string{}, File: filepath.ToSlash(rel)}
				byKey[d.RemoteRef.Key] = r
				keys = append(keys, d.RemoteRef.Key)
			}
			r.Properties[d.RemoteRef.Property] = d.SecretKey
		}
		for _, k := range keys {
			out = append(out, *byKey[k])
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s has no external-secrets/ directory: export again with --external-secrets (not none)", root)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, err
}

// MakePlan reads each remote secret's values from the source cluster and
// checks whether it exists in the secret manager. It writes nothing.
func MakePlan(ctx context.Context, remotes []Remote, src SecretReader, p Provider, overwrite bool) (*Plan, error) {
	plan := &Plan{Provider: p.Name()}
	for _, r := range remotes {
		it := &Item{Remote: r}
		plan.Items = append(plan.Items, it)
		data, err := src.Get(ctx, r.Namespace, r.Secret)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			it.Status, it.Note = Missing, "no Secret "+r.Namespace+"/"+r.Secret+" in the source cluster"
			continue
		case err != nil:
			return nil, fmt.Errorf("reading Secret %s/%s: %w", r.Namespace, r.Secret, err)
		}
		value := map[string]string{}
		var absent []string
		for _, prop := range sortedKeys(r.Properties) {
			v, ok := data[r.Properties[prop]]
			if !ok {
				absent = append(absent, r.Properties[prop])
				continue
			}
			if utf8.Valid(v) {
				value[prop] = string(v)
			} else {
				value[prop] = base64.StdEncoding.EncodeToString(v)
				it.Binary = append(it.Binary, prop)
			}
		}
		if len(absent) > 0 {
			it.Status, it.Note = Missing, "the Secret has no key "+strings.Join(absent, ", ")
			continue
		}
		b, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		it.value = string(b)
		exists, err := p.Exists(ctx, r.Key)
		switch {
		case err != nil:
			it.Status, it.Note = Failed, err.Error()
		case exists && overwrite:
			it.Status = Overwrite
		case exists:
			it.Status = Skip
		default:
			it.Status = Create
		}
	}
	return plan, nil
}

// Writes reports how many items Apply would write.
func (p *Plan) Writes() int {
	n := 0
	for _, it := range p.Items {
		if it.Status == Create || it.Status == Overwrite {
			n++
		}
	}
	return n
}

// Apply writes the planned values. Binary values are stored base64-encoded,
// so it also adds decodingStrategy: Base64 to their entries in the
// ExternalSecret files (under root, the cluster's directory). It goes on
// after a failed write and returns the first error.
func (p *Plan) Apply(ctx context.Context, root string, prov Provider) error {
	var first error
	for _, it := range p.Items {
		if it.Status != Create && it.Status != Overwrite {
			continue
		}
		if err := prov.Put(ctx, it.Key, it.value, it.Status == Overwrite); err != nil {
			it.Status, it.Note = Failed, err.Error()
			if first == nil {
				first = fmt.Errorf("%s: %w", it.Key, err)
			}
			continue
		}
		it.value = ""
		it.Status = Done
		if len(it.Binary) > 0 {
			if err := markBase64(filepath.Join(root, filepath.FromSlash(it.File)), it.Key, it.Binary); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

// markBase64 adds decodingStrategy: Base64 to the remoteRefs of key's
// binary properties in an ExternalSecret file.
func markBase64(path, key string, props []string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var es map[string]any
	if err := yaml.Unmarshal(b, &es); err != nil {
		return err
	}
	spec, _ := es["spec"].(map[string]any)
	data, _ := spec["data"].([]any)
	for _, d := range data {
		m, _ := d.(map[string]any)
		ref, _ := m["remoteRef"].(map[string]any)
		if ref["key"] == key {
			for _, p := range props {
				if ref["property"] == p {
					ref["decodingStrategy"] = "Base64"
				}
			}
		}
	}
	out, err := yaml.Marshal(es)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
