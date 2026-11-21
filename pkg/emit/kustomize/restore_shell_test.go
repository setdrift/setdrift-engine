// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package kustomize

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// hostile is a value a cluster object, a Helm release record or a manifest
// file could carry: every way a shell would run part of it, if a playbook
// command left it unquoted, creates the file PWNED.
const hostile = "h$(touch PWNED)`touch PWNED`;touch PWNED&&touch PWNED|touch PWNED\ntouch PWNED #'\""

// RESTORE.md and the platform playbooks are commands users paste with their
// cluster and AWS credentials. Rendered from a snapshot whose values are
// hostile, no command may run anything but what it names: every line of
// every shell block runs with stub kubectl, aws, helm, jq, perl and
// setdrift, and no PWNED file may appear.
func TestRestoreCommandsQuoteValues(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	x := mustExternalSecrets(t, "aws-secrets-manager")
	if err := (Emitter{SourceContext: hostile, ExternalSecrets: x}).Emit(hostileSnapshot(t), dir); err != nil {
		t.Fatal(err)
	}
	root := ClusterDir(dir, hostile)
	var blocks []string
	for _, f := range []string{"RESTORE.md", "platform/aws/RESTORE.md", "platform/aws/eks/RESTORE.md"} {
		blocks = append(blocks, shellBlocks(readFile(t, root, f))...)
	}
	if len(blocks) < 10 {
		t.Fatalf("only %d shell blocks rendered: the fixture no longer reaches the playbook's steps", len(blocks))
	}

	// stubs on PATH: each logs that it ran, and does nothing
	bin, work := t.TempDir(), t.TempDir()
	log := filepath.Join(work, "ran.log")
	for _, name := range []string{"kubectl", "aws", "helm", "jq", "perl", "setdrift"} {
		stub := "#!/bin/sh\necho " + name + " >> " + quote(log) + "\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
	for _, block := range blocks {
		lines := commandLines(block)
		for i, line := range lines {
			cmd := exec.Command(sh, "-c", line)
			cmd.Dir, cmd.Env = work, env
			_ = cmd.Run() // placeholders (<target-context>) make some lines fail; only what they run matters
			if _, err := os.Stat(filepath.Join(work, "PWNED")); err == nil {
				before := ""
				if i > 0 {
					before = lines[i-1]
				}
				t.Fatalf("this line ran an injected command:\n%s\n(after: %s)", line, before)
			}
		}
	}
	ran, err := os.ReadFile(log)
	if err != nil || len(ran) == 0 {
		t.Fatal("no command ran at all: the test checks nothing")
	}
}

// quote quotes a path for the stub scripts.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// shellBlocks returns the contents of the ```sh blocks in md.
func shellBlocks(md string) []string {
	var out []string
	for _, part := range strings.Split(md, "```sh\n")[1:] {
		if block, _, ok := strings.Cut(part, "\n```"); ok {
			out = append(out, block)
		}
	}
	return out
}

// commandLines splits a block into the commands a shell reads from it: at
// each newline outside quotes, joining a line that ends in a backslash with
// the next. (A newline inside quotes is part of the quoted word.)
func commandLines(block string) []string {
	var out []string
	var cur strings.Builder
	var quote rune // the open quote, or 0
	rs := []rune(block)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			}
		case r == '\\' && i+1 < len(rs):
			if rs[i+1] == '\n' && quote == 0 {
				i++ // a line continuation
				cur.WriteRune(' ')
				continue
			}
			cur.WriteRune(r)
			i++
			r = rs[i]
		case quote == '"':
			if r == '"' {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '#' && (i == 0 || rs[i-1] == ' ' || rs[i-1] == '\t' || rs[i-1] == '\n'):
			// a comment, to the end of the line: quotes in it are text
			for i+1 < len(rs) && rs[i+1] != '\n' {
				i++
			}
			continue
		case r == '\n':
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	return append(out, cur.String())
}

// hostileSnapshot is an EKS snapshot that reaches every step of the
// playbooks, with hostile values wherever the cluster or AWS supplies one.
func hostileSnapshot(t *testing.T) *snapshotv1alpha1.Snapshot {
	t.Helper()
	manifest := func(m map[string]any) string {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	h := hostile
	meta := func(name, ns string, extra map[string]any) map[string]any {
		m := map[string]any{"name": name}
		if ns != "" {
			m["namespace"] = ns
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	secretValue := snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE
	generated := snapshotv1alpha1.RedactionReason_REDACTION_REASON_GENERATED_VALUE
	return &snapshotv1alpha1.Snapshot{
		Cluster:  &snapshotv1alpha1.Cluster{Name: h, Platform: "eks", ProviderId: "arn:aws:eks:" + h + ":111122223333:cluster/" + h},
		Producer: &snapshotv1alpha1.Producer{Version: "test"},
		Resources: []*snapshotv1alpha1.Resource{
			{ApiVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition", Name: h,
				ManifestJson: manifest(map[string]any{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": meta(h, "", nil),
					"spec": map[string]any{"group": "example.com", "names": map[string]any{"kind": "Thing"}}})},
			{ApiVersion: "v1", Kind: "ServiceAccount", Namespace: h, Name: h,
				ManifestJson: manifest(map[string]any{"apiVersion": "v1", "kind": "ServiceAccount",
					"metadata": meta(h, h, map[string]any{"annotations": map[string]any{irsaAnnotation: "arn:aws:iam::111122223333:role/" + h}})})},
			{ApiVersion: "admissionregistration.k8s.io/v1", Kind: "ValidatingWebhookConfiguration", Name: "hook",
				ManifestJson: manifest(map[string]any{"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingWebhookConfiguration", "metadata": meta("hook", "", nil),
					"webhooks": []any{map[string]any{"name": "w", "clientConfig": map[string]any{"service": map[string]any{"namespace": h, "name": h}}}}})},
			{ApiVersion: "v1", Kind: "ConfigMap", Namespace: h, Name: "chart-cfg",
				ManifestJson: manifest(map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta("chart-cfg", h, nil)}),
				Provenance:   &snapshotv1alpha1.Provenance{Source: snapshotv1alpha1.Source_SOURCE_HELM, Manager: h + "/" + h}},
			{ApiVersion: "storage.k8s.io/v1", Kind: "StorageClass", Name: h,
				ManifestJson: manifest(map[string]any{"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass", "metadata": meta(h, "", nil), "provisioner": h})},
			{ApiVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: h, Name: h,
				ManifestJson: manifest(map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": meta(h, h, nil), "spec": map[string]any{"storageClassName": h}})},
			// a Secret whose values a person wrote: copied, filled in, or read from AWS
			{ApiVersion: "v1", Kind: "Secret", Namespace: h, Name: h,
				ManifestJson: manifest(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": meta(h, h, nil), "data": map[string]any{h: "U0VURFJJRlRfUkVEQUNURUQ="}}),
				Redactions:   []*snapshotv1alpha1.Redaction{{Path: "data." + h, Reason: secretValue}}},
			// a Secret a controller fills in, with one key a person wrote: merged
			{ApiVersion: "v1", Kind: "Secret", Namespace: h, Name: "merged",
				ManifestJson: manifest(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": meta("merged", h, nil), "data": map[string]any{h: "U0VURFJJRlRfUkVEQUNURUQ="}}),
				Redactions:   []*snapshotv1alpha1.Redaction{{Path: "data." + h, Reason: secretValue}, {Path: "data.gen", Reason: generated, Controller: h}}},
			// one a tool created empty and a controller fills in
			{ApiVersion: "v1", Kind: "Secret", Namespace: h, Name: "filled",
				ManifestJson: manifest(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": meta("filled", h, nil)}),
				Redactions:   []*snapshotv1alpha1.Redaction{{Path: "data.gen", Reason: generated, Controller: h}},
				Findings:     []*snapshotv1alpha1.Finding{{Code: "SECRET_FILLED_BY_CONTROLLER"}}},
			// a manifest holding a secret value outside a Secret
			{ApiVersion: "v1", Kind: "ConfigMap", Namespace: h, Name: "app",
				ManifestJson: manifest(map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta("app", h, nil), "data": map[string]any{"password": "SETDRIFT_REDACTED"}}),
				Redactions:   []*snapshotv1alpha1.Redaction{{Path: "data.password", Reason: secretValue}}},
		},
		Inventory: &snapshotv1alpha1.Inventory{
			Volumes: []*snapshotv1alpha1.Volume{{Namespace: h, Claim: h, PersistentVolume: h, StorageClass: h, Capacity: "1Gi",
				Driver: autoModeEBSDriver, VolumeHandle: "vol-" + h, Zone: h, ReclaimPolicy: "Delete",
				Aws: &snapshotv1alpha1.AwsVolume{VolumeType: h, KmsKeyId: h, Encrypted: true, AvailabilityZone: h}}},
			HelmReleases:  []*snapshotv1alpha1.HelmRelease{{Namespace: h, Name: h, Chart: h, ChartVersion: h, Repository: "oci://registry.example.com/" + h}},
			LoadBalancers: []*snapshotv1alpha1.LoadBalancer{{Kind: "Service", Namespace: h, Name: h, Hostnames: []string{h}}},
			Aws: &snapshotv1alpha1.AwsInventory{ClusterName: h, Region: h, OidcIssuer: "https://" + h,
				Addons: []*snapshotv1alpha1.Addon{{Name: h, Version: h, ServiceAccountRoleArn: h, ConfigurationValues: `{"a":1}`}},
				AccessEntries: []*snapshotv1alpha1.AccessEntry{{PrincipalArn: h, Type: "STANDARD", KubernetesGroups: []string{h},
					Policies: []*snapshotv1alpha1.AccessPolicy{{PolicyArn: h, Scope: "namespace", Namespaces: []string{h}}}}},
				PodIdentityAssociations: []*snapshotv1alpha1.PodIdentityAssociation{{Namespace: h, ServiceAccount: h, RoleArn: h}},
			},
		},
	}
}
