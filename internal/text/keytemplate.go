// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package text

import (
	"errors"
	"fmt"
	"strings"
	"text/template/parse"
)

// KeyFields are the fields a KeyTemplate may name.
var KeyFields = []string{"Cluster", "Namespace", "Name"}

// KeyTemplate is a remote secret key template: Go template syntax with only
// text and the fields {{.Cluster}}, {{.Namespace}} and {{.Name}}.
//
// It is parsed with text/template's parser but evaluated here: executing a
// text/template calls methods by name through reflection, which makes the
// linker keep every exported method of every type in the binary (half of
// its size).
type KeyTemplate struct {
	parts []keyPart
}

// keyPart is literal text, or (with field set) a field's value.
type keyPart struct {
	text, field string
}

// ParseKeyTemplate parses s, refusing anything but text and the KeyFields.
func ParseKeyTemplate(s string) (*KeyTemplate, error) {
	trees, err := parse.Parse("key", s, "", "")
	if err != nil {
		return nil, errors.New(strings.TrimPrefix(err.Error(), "template: "))
	}
	if len(trees) > 1 {
		return nil, errors.New("define and block are not supported: use only text and " + fieldList())
	}
	t := &KeyTemplate{}
	if tree := trees["key"]; tree != nil && tree.Root != nil {
		for _, n := range tree.Root.Nodes {
			p, err := keyNode(n)
			if err != nil {
				return nil, err
			}
			t.parts = append(t.parts, p)
		}
	}
	return t, nil
}

// keyNode turns one top-level node into a part: text, or an action that is
// a single known field.
func keyNode(n parse.Node) (keyPart, error) {
	switch n := n.(type) {
	case *parse.TextNode:
		return keyPart{text: string(n.Text)}, nil
	case *parse.ActionNode:
		if p := n.Pipe; p != nil && len(p.Decl) == 0 && len(p.Cmds) == 1 && len(p.Cmds[0].Args) == 1 {
			if f, ok := p.Cmds[0].Args[0].(*parse.FieldNode); ok && len(f.Ident) == 1 {
				for _, k := range KeyFields {
					if f.Ident[0] == k {
						return keyPart{field: k}, nil
					}
				}
				return keyPart{}, fmt.Errorf("unknown field .%s: use %s", f.Ident[0], fieldList())
			}
		}
		return keyPart{}, fmt.Errorf("%s is not supported: use only text and %s", n, fieldList())
	}
	return keyPart{}, fmt.Errorf("%s is not supported: use only text and %s", n, fieldList())
}

func fieldList() string {
	fs := make([]string, len(KeyFields))
	for i, f := range KeyFields {
		fs[i] = "{{." + f + "}}"
	}
	return strings.Join(fs, ", ")
}

// Uses reports whether the template names field.
func (t *KeyTemplate) Uses(field string) bool {
	for _, p := range t.parts {
		if p.field == field {
			return true
		}
	}
	return false
}

// Execute returns the key for one Secret.
func (t *KeyTemplate) Execute(cluster, namespace, name string) string {
	var b strings.Builder
	for _, p := range t.parts {
		switch p.field {
		case "":
			b.WriteString(p.text)
		case "Cluster":
			b.WriteString(cluster)
		case "Namespace":
			b.WriteString(namespace)
		case "Name":
			b.WriteString(name)
		}
	}
	return b.String()
}
