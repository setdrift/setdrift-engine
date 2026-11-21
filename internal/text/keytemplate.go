// parses and fills the remote secret key templates, like {{.Cluster}}/{{.Namespace}}/{{.Name}}, without executing a text/template

package text

import (
	"errors"
	"fmt"
	"strings"
	"text/template/parse"
)

// KeyFields are the only fields a KeyTemplate may name
var KeyFields = []string{"Cluster", "Namespace", "Name"}

// KeyTemplate is a parsed remote secret key template, Go template syntax with only text and the fields {{.Cluster}}, {{.Namespace}} and {{.Name}}
// its parsed with text/template's parser but filled in here, since executing a text/template calls methods by name through reflection
// that reflection makes the linker keep every exported method of every type in the binary, about half of its size
type KeyTemplate struct {
	// the pieces of the template in order, each one literal text or a field
	parts []keyPart
}

// keyPart is one piece of a KeyTemplate, literal text, or a field's value when field is set
type keyPart struct {
	text, field string
}

// ParseKeyTemplate takes the template text and parses it, refusing anything but text and the KeyFields so a key can never run arbitrary template logic
func ParseKeyTemplate(s string) (*KeyTemplate, error) {
	// parsing with text/template's parser only, nothing gets executed
	trees, err := parse.Parse("key", s, "", "")
	// checking if the syntax is wrong, if so the error goes back without the parser's "template: " prefix
	if err != nil {
		return nil, errors.New(strings.TrimPrefix(err.Error(), "template: "))
	}
	// checking if theres more than the one tree, that only happens with define or block, which we dont support
	if len(trees) > 1 {
		// invoking fieldList to name the fields allowed, its defined below
		return nil, errors.New("define and block are not supported: use only text and " + fieldList())
	}
	t := &KeyTemplate{}
	// checking if the template has a body, an empty template has no parts at all
	if tree := trees["key"]; tree != nil && tree.Root != nil {
		// going over each top-level node and turning it into a part
		for _, n := range tree.Root.Nodes {
			// invoking keyNode to turn the node into a part or refuse it, its defined below
			p, err := keyNode(n)
			if err != nil {
				return nil, err
			}
			t.parts = append(t.parts, p)
		}
	}
	return t, nil
}

// keyNode takes one top-level node and turns it into a part, text, or an action that is a single known field, anything else is refused
func keyNode(n parse.Node) (keyPart, error) {
	// checking what kind of node it is
	switch n := n.(type) {
	case *parse.TextNode:
		return keyPart{text: string(n.Text)}, nil
	case *parse.ActionNode:
		// checking if the action is one command with one argument and no variable declarations, like {{.Name}}
		if p := n.Pipe; p != nil && len(p.Decl) == 0 && len(p.Cmds) == 1 && len(p.Cmds[0].Args) == 1 {
			// checking if that argument is a field with a single name, not a chain like .A.B
			if f, ok := p.Cmds[0].Args[0].(*parse.FieldNode); ok && len(f.Ident) == 1 {
				// looking for the field among the KeyFields
				for _, k := range KeyFields {
					// checking if its this one, if so its a field part
					if f.Ident[0] == k {
						return keyPart{field: k}, nil
					}
				}
				// invoking fieldList to name the fields allowed, its defined below
				return keyPart{}, fmt.Errorf("unknown field .%s: use %s", f.Ident[0], fieldList())
			}
		}
		// invoking fieldList to name the fields allowed, its defined below
		return keyPart{}, fmt.Errorf("%s is not supported: use only text and %s", n, fieldList())
	}
	// any other node, like if or range, is refused, invoking fieldList to name the fields allowed, its defined below
	return keyPart{}, fmt.Errorf("%s is not supported: use only text and %s", n, fieldList())
}

// fieldList gives back the allowed fields written as template actions, like {{.Cluster}}, for the error messages
func fieldList() string {
	fs := make([]string, len(KeyFields))
	// going over each field and wrapping it as an action
	for i, f := range KeyFields {
		fs[i] = "{{." + f + "}}"
	}
	return strings.Join(fs, ", ")
}

// Uses takes a field name and tells if the template names it, so a caller can tell if the keys change with that field
func (t *KeyTemplate) Uses(field string) bool {
	// going over each part looking for that field
	for _, p := range t.parts {
		// checking if this part is the field
		if p.field == field {
			return true
		}
	}
	return false
}

// Execute takes one Secret's cluster, namespace and name and gives back its key, filling each field part with its value
func (t *KeyTemplate) Execute(cluster, namespace, name string) string {
	var b strings.Builder
	// going over each part in order and writing it out
	for _, p := range t.parts {
		// checking which field the part is, empty means literal text
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

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
