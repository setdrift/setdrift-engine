package translate

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"setdrift.io/setdrift-engine/pkg/platform"
	"sigs.k8s.io/yaml"
)

// ingress carries an Ingress over to the target's controller, keeping its exposure, internal stays internal, written explicitly
// another platform's class and annotations go, those that configured its load balancer (certificates, health checks, WAF) are listed for the README
func (t *translator) ingress(p *Patch, ref string, o *unstructured.Unstructured) {
	ann := o.GetAnnotations()
	clsName, _, _ := unstructured.NestedString(o.Object, "spec", "ingressClassName")
	clsAnn := ann["kubernetes.io/ingress.class"]
	cls := clsName
	if cls == "" {
		cls = clsAnn
	}
	// checking if the class belongs to another platform's controller, a self-managed one like nginx or traefik comes over as it is
	if cls == "" || !t.foreign(t.m.IngressClassOwners(cls)) {
		return
	}
	// invoking ingressExposure to read whether the source Ingress is internal or external, its defined below
	exp, source := t.ingressExposure(cls, ann)
	tgt := t.tgt.Translate.Ingress
	if exp == "internal" && tgt.InternalUnsupported {
		what := fmt.Sprintf("%s is an internal Ingress (%s), and %s's Ingress controller (%s) offers only public ones. equivalent makes it public on %s, so restrict who can reach it yourself; leave-out leaves it for you to put behind an internal load balancer of your own", ref, source, t.tgt.Name, tgt.ClassName, t.tgt.Name)
		switch t.decide(Decision{Feature: "ingress/" + qualified(o), What: what, Options: []string{Equivalent, LeaveOut}, Basis: Fingerprint(o.Object)}) {
		case LeaveOut:
			p.Delete = true
			p.Why = append(p.Why, "left out, as decided: "+what)
			return
		case Equivalent:
			exp = "external"
		default:
			return
		}
	}
	var dropped []string
	for _, k := range slices.Sorted(maps.Keys(ann)) {
		if k == "kubernetes.io/ingress.class" || t.foreign(t.m.IngressAnnotationOwners(k)) {
			p.Ops = append(p.Ops, Op{Op: "remove", Path: "/metadata/annotations/" + escape(k)})
			if k != "kubernetes.io/ingress.class" && !t.exposureKey(k) {
				dropped = append(dropped, k)
			}
		}
	}
	if clsName != "" {
		p.Ops = append(p.Ops, Op{Op: "remove", Path: "/spec/ingressClassName"})
	}
	want := map[string]string{}
	if tgt.ClassAnnotation {
		want["kubernetes.io/ingress.class"] = tgt.ExternalClass
		if exp == "internal" {
			want["kubernetes.io/ingress.class"] = tgt.InternalClass
		}
	} else {
		p.Ops = append(p.Ops, Op{Op: "add", Path: "/spec/ingressClassName", Value: tgt.ClassName})
		want = tgt.External
		if exp == "internal" {
			want = tgt.Internal
		}
	}
	if len(want) > 0 && len(ann) == 0 {
		p.Ops = append(p.Ops, Op{Op: "add", Path: "/metadata/annotations", Value: map[string]any{}})
	}
	for _, k := range slices.Sorted(maps.Keys(want)) {
		p.Ops = append(p.Ops, Op{Op: "add", Path: "/metadata/annotations/" + escape(k), Value: want[k]})
	}
	p.Why = append(p.Why, fmt.Sprintf("%s Ingress (%s) moves from class %s to %s's controller, written explicitly", exp, source, cls, t.tgt.Name))
	if len(dropped) > 0 {
		t.r.Notes = append(t.r.Notes, fmt.Sprintf("%s: its load balancer settings (%s) were dropped, set %s's equivalents for them (certificates, health checks, WAF) by hand", ref, strings.Join(dropped, ", "), t.tgt.Name))
	}
}

// exposureKey tells if an annotation is a platform's Ingress exposure annotation, which is rewritten rather than dropped
func (t *translator) exposureKey(k string) bool {
	for _, pl := range t.m.Platforms {
		if pl.Translate.Ingress.ExposureAnnotation == k {
			return true
		}
	}
	return false
}

// ingressExposure reads an Ingress's exposure on the platform its class belongs to, and says what it went by
func (t *translator) ingressExposure(cls string, ann map[string]string) (exposure, source string) {
	for _, pl := range t.m.Platforms {
		ing := pl.Translate.Ingress
		if v, ok := ann[ing.ExposureAnnotation]; ok && ing.ExposureAnnotation != "" {
			switch {
			case slices.Contains(ing.InternalValues, v):
				return "internal", ing.ExposureAnnotation + ": " + v
			case slices.Contains(ing.ExternalValues, v):
				return "external", ing.ExposureAnnotation + ": " + v
			}
		}
		if ing.InternalClass != "" && cls == ing.InternalClass {
			return "internal", "class " + cls
		}
		if ing.ExternalClass != "" && cls == ing.ExternalClass {
			return "external", "class " + cls
		}
	}
	for _, d := range t.m.IngressClassOwners(cls) {
		if p := t.m.For(d); p != nil && p.Translate.Ingress.Default != "" {
			return p.Translate.Ingress.Default, "the default of " + p.Name + "'s class " + cls
		}
	}
	return "internal", "no platform to go by, so the safe reading"
}

// ingressClass leaves out another platform's IngressClass, the target's controller installs its own, and the Ingresses are moved to it
func (t *translator) ingressClass(p *Patch, ref string, o *unstructured.Unstructured) {
	ctl, _, _ := unstructured.NestedString(o.Object, "spec", "controller")
	if !t.foreign(t.m.IngressControllerOwners(ctl)) {
		return
	}
	p.Delete = true
	p.Why = append(p.Why, fmt.Sprintf("controller %s isn't on %s: its own Ingress controller brings its class, and the Ingresses move to it", ctl, t.tgt.Name))
}

// snapshotClass points a VolumeSnapshotClass of another platform's driver at the target's, its parameters are the driver's own and go
func (t *translator) snapshotClass(p *Patch, ref string, o *unstructured.Unstructured) {
	drv, _, _ := unstructured.NestedString(o.Object, "driver")
	if !t.foreign(t.m.ProvisionerOwners(drv)) || t.tgt.Translate.SnapshotDriver == "" {
		return
	}
	p.Ops = append(p.Ops, Op{Op: "replace", Path: "/driver", Value: t.tgt.Translate.SnapshotDriver})
	if _, ok := o.Object["parameters"]; ok {
		p.Ops = append(p.Ops, Op{Op: "remove", Path: "/parameters"})
	}
	p.Why = append(p.Why, fmt.Sprintf("driver %s becomes %s, %s's block storage driver; its parameters were %s's own and go", drv, t.tgt.Translate.SnapshotDriver, t.tgt.Name, drv))
}

// secretProviderClass converts a Secrets Store CSI class to the target's provider, keeping its secret names, with the target's own settings left to fill in
// secret stores are a commodity, so the class stays a SecretProviderClass, SetDrift never copies the values, you put them in the target's secret manager
func (t *translator) secretProviderClass(p *Patch, ref string, o *unstructured.Unstructured) {
	prov, _, _ := unstructured.NestedString(o.Object, "spec", "provider")
	owners := t.m.SecretStoreProviderOwners(prov)
	owners = append(owners, t.m.SecretProviderClassOwners(prov)...)
	if !t.foreign(owners) {
		return
	}
	params, _, _ := unstructured.NestedStringMap(o.Object, "spec", "parameters")
	// invoking secretNames to read the secret names the class names, whichever provider wrote them, its defined below
	names := secretNames(params)
	// renaming them for the target's rules, Azure Key Vault allows letters, digits and dashes, Google Secret Manager underscores too, neither a slash
	var renamed []string
	for i, n := range names {
		// invoking secretNameFor to apply the target's rules, its defined below
		if nn := secretNameFor(t.to.Name(), n); nn != n {
			renamed = append(renamed, n+" as "+nn)
			names[i] = nn
		}
	}
	if len(renamed) > 0 {
		t.r.Notes = append(t.r.Notes, fmt.Sprintf("%s: %s's secret manager doesn't allow these names, so they're renamed: %s", ref, clouds(t.m, []platform.Distribution{t.to}), strings.Join(renamed, ", ")))
	}
	var newProv string
	newParams := map[string]any{}
	switch t.to.Name() {
	case "EKS":
		newProv = "aws"
		newParams["objects"] = listYAML(names, func(n string) map[string]any { return map[string]any{"objectName": n, "objectType": "secretsmanager"} })
	case "AKS":
		newProv = "azure"
		newParams["keyvaultName"] = "<Key Vault name>"
		newParams["tenantId"] = "<Azure tenant ID>"
		var arr []string
		for _, n := range names {
			b, _ := yaml.Marshal(map[string]any{"objectName": n, "objectType": "secret"})
			arr = append(arr, string(b))
		}
		b, _ := yaml.Marshal(map[string]any{"array": arr})
		newParams["objects"] = string(b)
	case "GKE":
		newProv = "gcp"
		newParams["secrets"] = listYAML(names, func(n string) map[string]any {
			return map[string]any{"resourceName": "projects/<project>/secrets/" + n + "/versions/latest", "path": n}
		})
	default:
		return
	}
	p.Ops = append(p.Ops, Op{Op: "replace", Path: "/spec/provider", Value: newProv}, Op{Op: "replace", Path: "/spec/parameters", Value: newParams})
	p.Why = append(p.Why, fmt.Sprintf("provider %s becomes %s, keeping its secret names (%s)", prov, newProv, strings.Join(names, ", ")))
	t.r.Notes = append(t.r.Notes, fmt.Sprintf("%s: put the secrets %s in %s's secret manager yourself (SetDrift never copies secret values), and fill in the values in angle brackets in the overlay's kustomization.yaml", ref, strings.Join(names, ", "), clouds(t.m, []platform.Distribution{t.to})))
}

// secretNames reads the secret names a SecretProviderClass's parameters name, from AWS's objects, Azure's objects array or Google Cloud's secrets
func secretNames(params map[string]string) []string {
	var names []string
	add := func(n string) {
		// keeping the last path segment of a resource name or ARN, the secret's own name
		if i := strings.Index(n, "/secrets/"); i >= 0 {
			n = strings.SplitN(n[i+len("/secrets/"):], "/", 2)[0]
		}
		if i := strings.LastIndex(n, ":secret:"); i >= 0 {
			n = n[i+len(":secret:"):]
		}
		if n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	var list []map[string]any
	if yaml.Unmarshal([]byte(params["objects"]), &list) == nil {
		for _, it := range list {
			add(fmt.Sprint(it["objectName"]))
		}
	}
	var az struct {
		Array []string `json:"array"`
	}
	if yaml.Unmarshal([]byte(params["objects"]), &az) == nil {
		for _, it := range az.Array {
			var m map[string]any
			if yaml.Unmarshal([]byte(it), &m) == nil {
				add(fmt.Sprint(m["objectName"]))
			}
		}
	}
	var gcp []map[string]any
	if yaml.Unmarshal([]byte(params["secrets"]), &gcp) == nil {
		for _, it := range gcp {
			add(fmt.Sprint(it["resourceName"]))
		}
	}
	if len(names) == 0 {
		names = []string{"<secret name>"}
	}
	return names
}

// secretNameFor renames a secret for the target's secret manager, every character it doesnt allow becomes a dash
func secretNameFor(target, n string) string {
	if n == "<secret name>" {
		return n
	}
	allowed := func(r rune) bool {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-'
		if target == "GKE" {
			ok = ok || r == '_'
		}
		return ok || target == "EKS"
	}
	return strings.Map(func(r rune) rune {
		if allowed(r) {
			return r
		}
		return '-'
	}, n)
}

// listYAML writes a list of items as the YAML string a SecretProviderClass parameter holds
func listYAML(names []string, item func(string) map[string]any) string {
	var l []any
	for _, n := range names {
		l = append(l, item(n))
	}
	b, _ := yaml.Marshal(l)
	return string(b)
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
