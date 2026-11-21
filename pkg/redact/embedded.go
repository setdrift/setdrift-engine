// drops secret values found outside Secrets, like literal env values, keys in ConfigMaps and credentials in custom resources
// each is replaced with Placeholder and recorded as a SECRET_VALUE redaction, so the resource is written for reference only (kustomize.ReferenceOnly) like a Secret, applying it would overwrite the real value
// a value counts when its unmistakably a credential (valuePatterns) anywhere, or when its non-empty under a secret-like name (secretName), a map key or the name of a name/value pair such as an env var

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

// skipGroups are the API groups whose objects are left alone, their content is meant to be committed (sealed ciphertext) or names secrets rather than holding them
var skipGroups = map[string]bool{
	"external-secrets.io":        true, // references to remote keys
	"bitnami.com":                true, // SealedSecrets, ciphertext thats safe in Git
	"secrets-store.csi.x-k8s.io": true, // SecretProviderClass, references only
	"apiextensions.k8s.io":       true, // CRD schemas describe fields, they hold no values
}

// valuePatterns match values that are credentials whatever theyre called, what is the description the finding shows
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
	// a password assigned in a config file held in a ConfigMap, not one read from a variable ($VAR, ${VAR}, {{ .x }})
	{"a password in a config file", regexp.MustCompile(`(?i)\b(?:password|passwd|secret|api[_-]?key|access[_-]?key|token)\s*[:=]\s*["']?[^\s"'$<{][^\s"']{3,}`)},
}

// the name segments secretName looks for
// secretSegments make a name secret-like on their own, secretPairs do as two words in a row, like api key
// notValueSegments mean the value is about a secret rather than one, like secretName, tokenTTL, passwordFile or tokenSecretRef
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

// secretName takes a key or env var name and tells if it says its value is a secret
// one segment that means the value is about a secret, like file or ref, rules the name out
func secretName(name string) bool {
	// invoking segments to split the name into lower-case words, its defined below
	segs := segments(name)
	found := false
	// going through the words, looking for a secret word and for one that rules the name out
	for i, s := range segs {
		// checking if this word means the value is about a secret, if so the name isnt secret-like
		if notValueSegments[s] {
			return false
		}
		// checking if this word, or it and the one before, make the name secret-like
		if secretSegments[s] || (i > 0 && secretPairs[[2]string{segs[i-1], s}]) {
			found = true
		}
	}
	return found
}

// segments splits a name into lower-case words, so DB_PASSWORD, dbPassword and db-password all give [db password]
func segments(name string) []string {
	var out []string
	var cur []rune
	// flush ends the current word and adds it to out
	flush := func() {
		// checking if a word was started, so empty words are never added
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	runes := []rune(name)
	// going through the name one rune at a time
	for i, r := range runes {
		// checking where a word ends, at a separator or where a lower-case letter is followed by an upper-case one
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

// plausibleValue takes a value under a secret-like name and tells if it may be the secret itself, rather than empty, a flag, or a reference to a variable or template
func plausibleValue(v string) bool {
	v = strings.TrimSpace(v)
	// checking if its empty or a well-known non-secret word, like true or changeme
	switch strings.ToLower(v) {
	case "", "true", "false", "yes", "no", "null", "none", "changeme":
		return false
	}
	return !strings.HasPrefix(v, "$") && !strings.HasPrefix(v, "{{") && len(v) >= 4
}

// matchValue takes a value and gives back what kind of credential it unmistakably is, from valuePatterns, or empty when its none
func matchValue(v string) string {
	// trying each pattern in order, the first match names the value
	for _, p := range valuePatterns {
		// checking if this pattern matches anywhere in the value
		if p.re.MatchString(v) {
			return p.what
		}
	}
	return ""
}

// embeddedFinding is one redacted value, its path and why it was taken for a secret, for the finding redactEmbedded adds
type embeddedFinding struct{ path, why string }

// redactEmbedded takes an object thats not a Secret and drops the secret values found in its fields, replacing each with Placeholder
// it then adds a SECRET_IN_MANIFEST warning naming where, and how to move the value into a Secret
func redactEmbedded(o *resource.Object) {
	// checking if the object will be dropped anyway, or is in a group meant to hold references or ciphertext, if so its left alone
	if o.Partial || skipGroups[o.GroupVersionKind().Group] {
		return
	}
	var found []embeddedFinding
	// redact records one dropped value, for the finding and as a SECRET_VALUE redaction that keeps the resource out of the applied set
	redact := func(path, why string) {
		found = append(found, embeddedFinding{path, why})
		o.Redactions = append(o.Redactions, &snapshotv1alpha1.Redaction{
			Path:   path,
			Reason: snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE,
		})
	}
	// invoking sortedKeys to go through the top-level fields in a fixed order, its defined below
	for _, key := range sortedKeys(o.Object) {
		v := o.Object[key]
		// checking which top-level field this is, apiVersion, kind and status are skipped and metadata is only partly searched
		switch key {
		case "apiVersion", "kind", "status":
			continue
		case "metadata":
			// names and labels are not secret, annotations rarely are, so only unmistakable values count there
			ann := o.GetAnnotations()
			keys := sortedKeys(ann)
			changed := false
			// going through the annotations in sorted order
			for _, k := range keys {
				// invoking matchValue and checking if this annotation is unmistakably a credential, if so its value is dropped, its defined above
				if why := matchValue(ann[k]); why != "" {
					ann[k] = Placeholder
					changed = true
					redact("metadata.annotations."+k, why)
				}
			}
			// checking if any annotation was replaced, if so they are set back since GetAnnotations gives a copy
			if changed {
				o.SetAnnotations(ann)
			}
			continue
		}
		// invoking walk to replace secret values anywhere under this field, its defined below
		o.Object[key] = walk(v, key, redact)
	}
	// checking if nothing was found, if so theres no finding to add
	if len(found) == 0 {
		return
	}
	paths := make([]string, 0, len(found))
	// listing the first three paths with why, and how many more there are after that
	for i, f := range found {
		// checking if three are listed already, if so the rest is a count
		if i == 3 {
			paths = append(paths, fmt.Sprintf("and %d more", len(found)-3))
			break
		}
		paths = append(paths, fmt.Sprintf("%s (%s)", f.path, f.why))
	}
	// invoking AddFinding to warn where the secret values were and how to fix it, its defined in pkg/resource/resource.go
	o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_WARNING, "SECRET_IN_MANIFEST",
		"holds what looks like a secret value in %s; it was redacted, so this resource is kept out of the applied manifests. "+
			"Move the value into a Secret and reference it (env valueFrom.secretKeyRef, or a Secret volume)",
		strings.Join(paths, ", "))
}

// Values takes v, a document parsed from JSON or YAML thats not a Kubernetes object, such as an EKS add-on's configuration, and drops its secret values
// it uses the same detection as manifests, replacing each with Placeholder, and gives back v and the paths it replaced, e.g. agent.apiKey
func Values(v any) (any, []string) {
	var paths []string
	// invoking walk with a redact that only records the path, without the leading dot, its defined below
	v = walk(v, "", func(path, _ string) {
		paths = append(paths, strings.TrimPrefix(path, "."))
	})
	return v, paths
}

// FieldValue takes the path of a live object's field, as diff writes paths like spec.template.spec.containers[web].env[DB_TOKEN].value, and its value v
// it gives back v with secret values replaced by Placeholder, so values read from a cluster, which the redactor never saw, can be shown outside it
// it uses the same detection as manifests, a value thats unmistakably a credential, or a plausible value under a secret-like name, the field's own or the name of the list item its in as for an env var
// v itself is not changed
func FieldValue(path string, v any) any {
	// invoking fieldNames to get the field's own name and its list item's name, its defined below
	key, label := fieldNames(path)
	// checking if the value is a string, if so its checked here against both names
	if s, ok := v.(string); ok {
		// invoking matchValue, secretName and plausibleValue and checking if any says secret, if so only the placeholder is given back, theyre defined above
		if matchValue(s) != "" || ((secretName(key) || secretName(label)) && plausibleValue(s)) {
			return Placeholder
		}
		return s
	}
	// invoking deepCopy and Values to search a map or list without changing the caller's value, theyre defined below and above
	out, _ := Values(deepCopy(v))
	return out
}

// fieldNames takes a path and gives back its last key and the label of the last list item in it, value and DB_TOKEN for ...env[DB_TOKEN].value
func fieldNames(path string) (key, label string) {
	key = path
	// checking if the path has a dot or a closing bracket, if so the key is what comes after the last one
	if i := strings.LastIndexAny(path, ".]"); i >= 0 {
		key = path[i+1:]
	}
	// checking if the path has a list item, if so the label is what is inside its brackets, without quotes
	if i := strings.LastIndex(path, "["); i >= 0 {
		// checking if that bracket is closed
		if j := strings.Index(path[i:], "]"); j > 0 {
			label = strings.Trim(path[i+1:i+j], `"`)
		}
	}
	return key, label
}

// deepCopy copies decoded JSON, so walk's replacements dont change the caller's value
func deepCopy(v any) any {
	// checking if its a map or a list, those are copied one element at a time, anything else is a value and given back as is
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		// copying each entry, nested ones included
		for k, e := range x {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		// copying each item, nested ones included
		for i, e := range x {
			out[i] = deepCopy(e)
		}
		return out
	}
	return v
}

// walk takes a decoded value and its path and gives it back with secret values replaced, calling redact for each one it drops
func walk(v any, path string, redact func(path, why string)) any {
	// checking what kind of value it is, maps go to walkMap, lists are walked item by item, strings are matched
	switch n := v.(type) {
	case map[string]any:
		// invoking walkMap for a map, its defined below
		return walkMap(n, path, redact)
	case []any:
		// going through each list item, labelled by its name when it has one, by its index otherwise
		for i, item := range n {
			label := fmt.Sprint(i)
			// checking if the item is a map
			if m, ok := item.(map[string]any); ok {
				// checking if it has a name, if so thats its label in the path
				if name, ok := m["name"].(string); ok {
					label = name
				}
			}
			n[i] = walk(item, fmt.Sprintf("%s[%s]", path, label), redact)
		}
		return n
	case string:
		// invoking matchValue and checking if the string is unmistakably a credential, if so its dropped, its defined above
		if why := matchValue(n); why != "" {
			redact(path, why)
			return Placeholder
		}
	}
	return v
}

// walkMap is walk for a map
// a map holding a name and a value, like an env var, is a pair, the name says whether the value is a secret
func walkMap(n map[string]any, path string, redact func(path, why string)) map[string]any {
	pair := false
	// checking if the map has a string name
	if name, ok := n["name"].(string); ok {
		// checking if it also has a string value, if so its a pair and the value is judged by the name
		if val, ok := n["value"].(string); ok {
			pair = true
			p := path + ".value"
			// invoking matchValue and checking if the value is unmistakably a credential, its defined above
			// otherwise invoking secretName and plausibleValue to check if the name is secret-like and the value could be the secret, theyre defined above
			if why := matchValue(val); why != "" {
				n["value"] = Placeholder
				redact(p, why)
			} else if secretName(name) && plausibleValue(val) {
				n["value"] = Placeholder
				redact(p, "secret-like name "+name)
			}
		}
	}
	// going through the keys in sorted order, sortedKeys is defined below
	for _, k := range sortedKeys(n) {
		p := path + "." + k
		s, ok := n[k].(string)
		// checking what to do with this entry, anything but a string is walked, a pair's value is already done, any other string is judged by its key
		switch {
		case !ok:
			n[k] = walk(n[k], p, redact)
		case k == "value" && pair:
			// handled with its name above
		default:
			// invoking matchValue and checking if the string is unmistakably a credential
			// otherwise invoking secretName and plausibleValue to check if the key is secret-like and the value could be the secret, all defined above
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

// sortedKeys gives back m's keys sorted, so values are searched and redactions recorded in the same order every run
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	// collecting the keys before sorting them
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
