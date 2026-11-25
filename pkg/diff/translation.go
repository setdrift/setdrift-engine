// a comparison through a translation, sorting each difference between the source cluster and the target into translated on purpose, decided, not moved by design or real
// it lines up four versions of each object, the source as exported (E), the export translated (T, the overlay built), the source now (A) and the target now (B)
// so a field translate changed is known from the overlay itself, and a change made since on either side still shows as real

package diff

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Translation is a translation to compare through, what ApplyTranslation sorts the differences by
type Translation struct {
	// Code names it, like te2g
	Code string
	// Exported are the source's objects as the export took them (E), Translated the export's generic set through the translation's overlay (T), both by key, as decoded JSON
	Exported   map[Key]any
	Translated map[Key]any
	// NotMoved says why an exported object isnt in the translation by design, like a Secret whose values never move, empty when it isnt one of those
	NotMoved func(Key) string
	// Explain says why an object of the target differs on purpose, from the translation's decisions, empty when nothing does
	Explain func(manifest any) string
}

// Sorted is a difference sorted out of the real ones, with why
type Sorted struct {
	Object Object   `json:"object"`
	Why    []string `json:"why"`
	// Fields are the fields it covers, for a resource in both
	Fields []Field `json:"fields,omitempty"`
}

// ApplyTranslation sorts the result's differences through the translation, A is the source and B the target
// what the translation changed on purpose goes to Translated, what its decisions explain to Decided, what the export never moves to NotMoved, and only the rest stays a difference
func (r *Result) ApplyTranslation(t Translation) {
	r.Translation = t.Code
	var changed []Change
	for _, c := range r.Changed {
		// both read without the translation's own annotations, as A and B were compared
		e, t2 := WithoutTranslationMark(t.Exported[c.A.Key]), WithoutTranslationMark(t.Translated[c.A.Key])
		var real, translated []Field
		for _, f := range c.Fields {
			// invoking sortField to tell what this field is, its defined below
			switch kind, reason := t.sortField(c, f, e, t2); kind {
			case sortedTranslated:
				translated = append(translated, f)
			default:
				real = append(real, f)
				if reason != "" {
					c.Reasons = appendOnce(c.Reasons, reason)
				}
			}
		}
		if len(translated) > 0 {
			r.Translated = append(r.Translated, Sorted{Object: c.B, Why: []string{"changed by the translation"}, Fields: translated})
		}
		// a resource stays changed when a real field differs, or it only differs in the clusters' own values, which shows on its own
		if len(real) > 0 || (len(c.Fields) == 0 && len(c.EnvFields) > 0) {
			c.Fields = real
			changed = append(changed, c)
		} else {
			r.AsTranslated++
		}
	}
	r.Changed = changed

	var onlyA []Object
	for _, o := range r.OnlyA {
		_, exported := t.Exported[o.Key]
		_, translated := t.Translated[o.Key]
		switch {
		case exported && !translated && t.NotMoved != nil && t.NotMoved(o.Key) != "":
			r.NotMoved = append(r.NotMoved, Sorted{Object: o, Why: []string{t.NotMoved(o.Key)}})
		case exported && !translated:
			r.Translated = append(r.Translated, Sorted{Object: o, Why: []string{"left out by the translation"}})
		default:
			// not in the export, so it was made on the source after it, a real difference
			onlyA = append(onlyA, o)
		}
	}
	r.OnlyA = onlyA

	var onlyB []Object
	for _, o := range r.OnlyB {
		_, exported := t.Exported[o.Key]
		if _, translated := t.Translated[o.Key]; translated && !exported {
			r.Translated = append(r.Translated, Sorted{Object: o, Why: []string{"added by the translation, the target's equivalent"}})
			continue
		}
		onlyB = append(onlyB, o)
	}
	r.OnlyB = onlyB

	var wont []WontWork
	for _, ww := range r.WontWork {
		// checking if the translation's decisions explain it, and the target still has it as translated, a decision holds only while thats so
		if ww.Side == r.B.Label && t.Explain != nil && !changedOnTarget(r.Changed, ww.Object.Key) {
			if m, ok := t.Translated[ww.Object.Key]; ok {
				if why := t.Explain(m); why != "" {
					r.Decided = append(r.Decided, Sorted{Object: ww.Object, Why: append([]string{why}, ww.Reasons...)})
					continue
				}
			}
		}
		wont = append(wont, ww)
	}
	r.WontWork = wont
}

// changedOnTarget tells if a resource still has a real difference after sorting, so a decision about it no longer holds
func changedOnTarget(changed []Change, k Key) bool {
	for _, c := range changed {
		if c.A.Key == k && len(c.Fields) > 0 {
			return true
		}
	}
	return false
}

// what sortField finds a field is
const (
	sortedReal = iota
	sortedTranslated
)

// sortField tells if a differing field was changed by the translation or is real, and why when its real
// decisions explain no field, a decided object the target still has as translated doesnt differ, and one it changed differs for real
func (t Translation) sortField(c Change, f Field, e, tr any) (int, string) {
	ev, eok := at(e, f.Keys)
	tv, tok := at(tr, f.Keys)
	have := e != nil && tr != nil && len(f.Keys) > 0
	switch {
	// the source is as exported, the target as translated, and the translation changed it
	case have && same(f.A, ev, eok) && same(f.B, tv, tok) && (eok != tok || !reflect.DeepEqual(ev, tv)):
		return sortedTranslated, ""
	// the source changed since the export, the translation never saw it
	case have && !same(f.A, ev, eok):
		return sortedReal, "changed on the source after the export it was translated from: export and translate again, or make the same change on the target"
	// a claim follows its class, a translated class gives it a new provisioner, and the target's controller writes what follows from that
	case c.A.Key.Kind == "PersistentVolumeClaim" && c.A.Key.Group == "" && t.classTranslated(e):
		return sortedTranslated, ""
	}
	// a decision never explains a field the target changed: it holds only while the target is as the translation made it, so this is real
	if have {
		return sortedReal, "differs on the target from the translation: changed there since, or never applied"
	}
	return sortedReal, ""
}

// classTranslated tells if a claim's StorageClass is one the translation changed
func (t Translation) classTranslated(claim any) bool {
	cls, ok := mapKey(claim, "spec", "storageClassName")
	name, _ := cls.(string)
	if !ok || name == "" {
		return false
	}
	k := Key{Group: "storage.k8s.io", Kind: "StorageClass", Name: name}
	e, eok := t.Exported[k]
	tr, tok := t.Translated[k]
	return eok && tok && !reflect.DeepEqual(WithoutTranslationMark(e), WithoutTranslationMark(tr))
}

// same tells if a field's value matches another's, a missing value matching only an absent one
func same(a, b any, present bool) bool {
	if !present {
		return a == nil
	}
	return reflect.DeepEqual(a, b)
}

// at reads the value a field's managedFields-style keys lead to, "f:spec" a map key, "n:web" the list item named web, "i:0" an index
func at(v any, keys []string) (any, bool) {
	for _, k := range keys {
		switch {
		case strings.HasPrefix(k, "f:"):
			m, ok := v.(map[string]any)
			if !ok {
				return nil, false
			}
			if v, ok = m[k[2:]]; !ok {
				return nil, false
			}
		case strings.HasPrefix(k, "n:"):
			l, ok := v.([]any)
			if !ok {
				return nil, false
			}
			item, ok := byName(l)[k[2:]]
			if !ok {
				return nil, false
			}
			v = item
		case strings.HasPrefix(k, "i:"):
			l, ok := v.([]any)
			i, err := strconv.Atoi(k[2:])
			if !ok || err != nil || i < 0 || i >= len(l) {
				return nil, false
			}
			v = l[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// appendOnce adds a reason once
func appendOnce(l []string, s string) []string {
	for _, x := range l {
		if x == s {
			return l
		}
	}
	return append(l, s)
}

// Plain turns an object into JSON as encoding/json decodes it, numbers as float64, the way diff reads every manifest
func Plain(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("reading the translation's object: %w", err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
