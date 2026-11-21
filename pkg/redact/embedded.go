// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package redact

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// Secret values outside Secrets: literal env values, keys in ConfigMaps,
// credentials in custom resources. They are replaced with Placeholder and
// recorded as SECRET_VALUE redactions, so the resource is written for
// reference only (kustomize.ReferenceOnly) like a Secret: applying it would
// overwrite the real value. A finding says where, and how to fix it.
//
// Two kinds of match:
//   - a value that is unmistakably a credential (valuePatterns), anywhere;
//   - a non-empty value under a secret-like name (secretName): a map key,
//     or the name of a name/value pair such as an env var.

// skipGroups hold objects whose content is meant to be committed (sealed
// ciphertext) or names secrets rather than holding them.
var skipGroups = map[string]bool{
	"external-secrets.io":        true, // references to remote keys
	"bitnami.com":                true, // SealedSecrets: ciphertext, safe in Git
	"secrets-store.csi.x-k8s.io": true, // SecretProviderClass: references
	"apiextensions.k8s.io":       true, // CRD schemas describe fields
}

// valuePatterns match values that are credentials whatever they are called.
var valuePatterns = []struct {
	what string
	re   *regexp.Regexp
}{
	{"a private key", regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY-----`)},
	{"an AWS access key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"a JSON web token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	{"a URL with a password", regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]+:[^/\s@$]{3,}@`)},
	{"a GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`)},
	{"a GitLab token", regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`)},
	{"a Slack token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`)},
	// A password assigned in a config file held in a ConfigMap; not one
	// read from a variable ($VAR, ${VAR}, {{ .x }}).
	{"a password in a config file", regexp.MustCompile(`(?i)\b(?:password|passwd|secret|api[_-]?key|access[_-]?key|token)\s*[:=]\s*["']?[^\s"'$<{][^\s"']{3,}`)},
}

// Name segments that make a name secret-like, and segments that mean the
// value is about a secret rather than one (secretName, tokenTTL,
// passwordFile, tokenSecretRef).
var (
	secretSegments = map[string]bool{
		"password": true, "passwd": true, "pwd": true, "secret": true, "token": true,
		"apikey": true, "credential": true, "credentials": true, "privatekey": true, "passphrase": true,
	}
	secretPairs = map[[2]string]bool{
		{"api", "key"}: true, {"access", "key"}: true, {"private", "key"}: true, {"secret", "key"}: true,
	}
	notValueSegments = map[string]bool{
		"name": true, "names": true, "ref": true, "refs": true, "selector": true, "file": true, "path": true,
		"dir": true, "mount": true, "ttl": true, "seconds": true, "duration": true, "expiry": true,
		"expiration": true, "lifetime": true, "policy": true, "type": true, "length": true, "mode": true,
		"enabled": true, "enable": true, "header": true, "issuer": true, "audience": true, "namespace": true,
		"endpoint": true, "existing": true, "rotation": true, "version": true, "kind": true, "env": true,
		"field": true, "store": true, "provider": true, "id": true, "url": true, "uri": true, "hash": true,
		"required": true, "min": true, "max": true, "format": true, "source": true, "sources": true,
	}
)

// secretName reports whether a key or env var name says its value is a
// secret.
func secretName(name string) bool {
	segs := segments(name)
	found := false
	for i, s := range segs {
		if notValueSegments[s] {
			return false
		}
		if secretSegments[s] || (i > 0 && secretPairs[[2]string{segs[i-1], s}]) {
			found = true
		}
	}
	return found
}

// segments splits a name into lower-case words: DB_PASSWORD, dbPassword and
// db-password all give [db password].
func segments(name string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	runes := []rune(name)
	for i, r := range runes {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && i > 0 && unicode.IsLower(runes[i-1]):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}

// plausibleValue reports whether a value under a secret-like name may be
// the secret itself, rather than empty, a flag, or a reference to a
// variable or template.
func plausibleValue(v string) bool {
	v = strings.TrimSpace(v)
	switch strings.ToLower(v) {
	case "", "true", "false", "yes", "no", "null", "none", "changeme":
		return false
	}
	return !strings.HasPrefix(v, "$") && !strings.HasPrefix(v, "{{") && len(v) >= 4
}

func matchValue(v string) string {
	for _, p := range valuePatterns {
		if p.re.MatchString(v) {
			return p.what
		}
	}
	return ""
}

// embeddedFinding is one redacted value, for the finding.
type embeddedFinding struct{ path, why string }

// redactEmbedded replaces secret values in o, which is not a Secret.
func redactEmbedded(o *resource.Object) {
	if o.Partial || skipGroups[o.GroupVersionKind().Group] {
		return
	}
	var found []embeddedFinding
	redact := func(path, why string) {
		found = append(found, embeddedFinding{path, why})
		o.Redactions = append(o.Redactions, &snapshotv1alpha1.Redaction{
			Path:   path,
			Reason: snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE,
		})
	}
	for _, key := range sortedKeys(o.Object) {
		v := o.Object[key]
		switch key {
		case "apiVersion", "kind", "status":
			continue
		case "metadata":
			// Names and labels are not secret; annotations rarely are,
			// so only unmistakable values count there.
			ann := o.GetAnnotations()
			keys := sortedKeys(ann)
			changed := false
			for _, k := range keys {
				if why := matchValue(ann[k]); why != "" {
					ann[k] = Placeholder
					changed = true
					redact("metadata.annotations."+k, why)
				}
			}
			if changed {
				o.SetAnnotations(ann)
			}
			continue
		}
		o.Object[key] = walk(v, key, redact)
	}
	if len(found) == 0 {
		return
	}
	paths := make([]string, 0, len(found))
	for i, f := range found {
		if i == 3 {
			paths = append(paths, fmt.Sprintf("and %d more", len(found)-3))
			break
		}
		paths = append(paths, fmt.Sprintf("%s (%s)", f.path, f.why))
	}
	o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_WARNING, "SECRET_IN_MANIFEST",
		"holds what looks like a secret value in %s; it was redacted, so this resource is kept out of the applied manifests. "+
			"Move the value into a Secret and reference it (env valueFrom.secretKeyRef, or a Secret volume)",
		strings.Join(paths, ", "))
}

// Values replaces secret values in v, a document parsed from JSON or YAML
// that is not a Kubernetes object (such as an EKS add-on's configuration),
// with Placeholder, using the same detection as manifests. It returns v and
// the paths it replaced, e.g. "agent.apiKey".
func Values(v any) (any, []string) {
	var paths []string
	v = walk(v, "", func(path, _ string) {
		paths = append(paths, strings.TrimPrefix(path, "."))
	})
	return v, paths
}

// FieldValue returns v, the value of a live object's field at path (as
// diff writes paths: spec.template.spec.containers[web].env[DB_TOKEN].value),
// with secret values replaced by Placeholder, using the same detection as
// manifests: a value that is unmistakably a credential, or a plausible value
// under a secret-like name (the field's own, or the name of the list item it
// is in, as for an env var). For showing values read from a cluster, which
// the redactor never saw, outside it. v is not changed.
func FieldValue(path string, v any) any {
	key, label := fieldNames(path)
	if s, ok := v.(string); ok {
		if matchValue(s) != "" || ((secretName(key) || secretName(label)) && plausibleValue(s)) {
			return Placeholder
		}
		return s
	}
	out, _ := Values(deepCopy(v))
	return out
}

// fieldNames returns the last key of path, and the label of the last list
// item in it: "value" and "DB_TOKEN" for ...env[DB_TOKEN].value.
func fieldNames(path string) (key, label string) {
	key = path
	if i := strings.LastIndexAny(path, ".]"); i >= 0 {
		key = path[i+1:]
	}
	if i := strings.LastIndex(path, "["); i >= 0 {
		if j := strings.Index(path[i:], "]"); j > 0 {
			label = strings.Trim(path[i+1:i+j], `"`)
		}
	}
	return key, label
}

// deepCopy copies decoded JSON, so walk's replacements don't change the
// caller's value.
func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopy(e)
		}
		return out
	}
	return v
}

// walk returns v with secret values replaced, calling redact for each.
func walk(v any, path string, redact func(path, why string)) any {
	switch n := v.(type) {
	case map[string]any:
		return walkMap(n, path, redact)
	case []any:
		for i, item := range n {
			label := fmt.Sprint(i)
			if m, ok := item.(map[string]any); ok {
				if name, ok := m["name"].(string); ok {
					label = name
				}
			}
			n[i] = walk(item, fmt.Sprintf("%s[%s]", path, label), redact)
		}
		return n
	case string:
		if why := matchValue(n); why != "" {
			redact(path, why)
			return Placeholder
		}
	}
	return v
}

// walkMap is walk for a map. A map holding a name and a value, like an env
// var, is a pair: the name says whether the value is a secret.
func walkMap(n map[string]any, path string, redact func(path, why string)) map[string]any {
	pair := false
	if name, ok := n["name"].(string); ok {
		if val, ok := n["value"].(string); ok {
			pair = true
			p := path + ".value"
			if why := matchValue(val); why != "" {
				n["value"] = Placeholder
				redact(p, why)
			} else if secretName(name) && plausibleValue(val) {
				n["value"] = Placeholder
				redact(p, "secret-like name "+name)
			}
		}
	}
	for _, k := range sortedKeys(n) {
		p := path + "." + k
		s, ok := n[k].(string)
		switch {
		case !ok:
			n[k] = walk(n[k], p, redact)
		case k == "value" && pair:
			// handled with its name above
		default:
			if why := matchValue(s); why != "" {
				n[k] = Placeholder
				redact(p, why)
			} else if secretName(k) && plausibleValue(s) {
				n[k] = Placeholder
				redact(p, "secret-like key "+k)
			}
		}
	}
	return n
}

// sortedKeys returns m's keys, sorted.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
