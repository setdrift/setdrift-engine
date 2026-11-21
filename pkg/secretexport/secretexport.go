// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Package secretexport copies the values of an export's redacted Secrets
// from the source cluster straight into a secret manager, so External
// Secrets Operator can restore them in another cluster.
//
// It is the one part of SetDrift that handles secret values, and it is
// separate from the export: `setdrift export` never reads a value. This
// package reads which Secrets to copy from the export's redacted files,
// names each one's remote secret as the user says, reads the live values
// with the user's own credentials, writes them into the secret manager with
// the user's own cloud credentials, and checks what the secret manager then
// holds. Values are held in memory only, never written to disk or printed:
// plans and reports show remote keys, property names and Secret names.
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
	"maps"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
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
	// Get returns the remote secret's value, and whether it exists.
	Get(ctx context.Context, key string) (string, bool, error)
}

// SecretReader reads a Secret's data from the source cluster.
type SecretReader interface {
	// Get returns the Secret's data, decoded; an error wrapping ErrNoSecret
	// when there is no such Secret.
	Get(ctx context.Context, namespace, name string) (map[string][]byte, error)
}

// ErrNoSecret is the error a SecretReader wraps for a Secret that does not
// exist.
var ErrNoSecret = errors.New("no such Secret")

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
	Skip      Status = "skip"
	Missing   Status = "missing"
	Failed    Status = "failed"
	Done      Status = "done"
)

// Label is the status as a plan shows it: "skip: exists" for Skip.
func (s Status) Label() string {
	if s == Skip {
		return "skip: exists"
	}
	return string(s)
}

// Item is one remote secret in a plan.
type Item struct {
	Remote
	Status Status
	// Binary lists the properties whose values are not text: they are stored
	// base64-encoded, and the ExternalSecret decodes them (decodingStrategy).
	Binary []string
	// Note explains a Missing or Failed status.
	Note string
	// Merge: the live Secret has keys beyond the redacted ones, which a
	// controller wrote (Argo CD's argocd-secret); its ExternalSecret adds
	// its keys without owning the Secret (creationPolicy: Merge).
	Merge bool
	// value is the JSON to write: never printed. A pointer, so formatting
	// an Item (%v, %+v, a test failure) prints an address, not the value.
	value *secretValue
}

// secretValue holds a remote secret's value, read from the source Secret.
type secretValue struct{ json string }

// Plan is what Apply would do.
type Plan struct {
	Provider string
	Items    []*Item
}

// Remotes pairs each Secret with its remote key, keys[i] for secrets[i]
// (RemoteKeys' order): every redacted key is a property of the same name.
func Remotes(secrets []Source, keys []string) ([]Remote, error) {
	if len(keys) != len(secrets) {
		return nil, fmt.Errorf("%d remote keys for %d Secrets", len(keys), len(secrets))
	}
	out := make([]Remote, len(secrets))
	for i, src := range secrets {
		props := make(map[string]string, len(src.Keys))
		for _, k := range src.Keys {
			props[k] = k
		}
		out[i] = Remote{Key: keys[i], Namespace: src.Namespace, Secret: src.Name, Properties: props, File: src.File}
	}
	return out, nil
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
		case errors.Is(err, ErrNoSecret):
			it.Status, it.Note = Missing, "no Secret "+r.Namespace+"/"+r.Secret+" in the source cluster"
			continue
		case err != nil:
			return nil, fmt.Errorf("reading Secret %s/%s: %w", r.Namespace, r.Secret, err)
		}
		value := map[string]string{}
		var absent []string
		for k := range data {
			if _, ok := r.Properties[k]; !ok {
				it.Merge = true
			}
		}
		for _, prop := range slices.Sorted(maps.Keys(r.Properties)) {
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
		it.value = &secretValue{string(b)}
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

// Apply writes the planned values. It goes on after a failed write and
// returns the first error.
func (p *Plan) Apply(ctx context.Context, prov Provider) error {
	var first error
	for _, it := range p.Items {
		if it.Status != Create && it.Status != Overwrite {
			continue
		}
		if err := prov.Put(ctx, it.Key, it.value.json, it.Status == Overwrite); err != nil {
			it.Status, it.Note = Failed, err.Error()
			if first == nil {
				first = fmt.Errorf("%s: %w", it.Key, err)
			}
			continue
		}
		it.Status = Done
	}
	return first
}

// Check is what the secret manager holds for one remote secret, compared
// with the source Secret.
type Check struct {
	Key     string   `json:"key"`
	Secret  string   `json:"secret"` // namespace/name
	Exists  bool     `json:"exists"`
	Keys    bool     `json:"keysMatch"`
	Values  bool     `json:"valuesMatch"`
	Missing []string `json:"missingKeys,omitempty"`
	Extra   []string `json:"extraKeys,omitempty"`
	Note    string   `json:"note,omitempty"`
}

// OK reports whether the remote secret holds exactly the source's values.
func (c Check) OK() bool { return c.Exists && c.Keys && c.Values }

// Verify reads each planned remote secret back from the secret manager and
// compares it with the source Secret's values, held in memory since the
// plan: whether it exists, has the same keys, and the same values. Nothing
// is printed or kept but the result.
func (p *Plan) Verify(ctx context.Context, prov Provider) []Check {
	out := make([]Check, 0, len(p.Items))
	for _, it := range p.Items {
		c := Check{Key: it.Key, Secret: it.Namespace + "/" + it.Secret}
		if it.Status == Missing {
			c.Note = it.Note
			out = append(out, c)
			continue
		}
		got, exists, err := prov.Get(ctx, it.Key)
		switch {
		case err != nil:
			c.Note = "can't read it back: " + err.Error()
		case !exists:
			c.Note = "not in " + prov.Name()
		default:
			c.Exists = true
			var remote, want map[string]string
			if json.Unmarshal([]byte(got), &remote) != nil {
				c.Note = "not a JSON object of keys"
				break
			}
			_ = json.Unmarshal([]byte(it.value.json), &want) // our own JSON object of strings
			for k := range want {
				if _, ok := remote[k]; !ok {
					c.Missing = append(c.Missing, k)
				}
			}
			for k := range remote {
				if _, ok := want[k]; !ok {
					c.Extra = append(c.Extra, k)
				}
			}
			sort.Strings(c.Missing)
			sort.Strings(c.Extra)
			c.Keys = len(c.Missing) == 0 && len(c.Extra) == 0
			c.Values = c.Keys
			for k, v := range want {
				if remote[k] != v {
					c.Values = false
				}
			}
			switch {
			case !c.Keys:
				c.Note = "its keys differ from the Secret's"
			case !c.Values:
				c.Note = "its values differ from the Secret's (--overwrite replaces them)"
			}
		}
		out = append(out, c)
	}
	return out
}
