// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// runs eight tests sequentially, validates tests inputs for the export command itself, output evaluated elsewhere

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// test missing, empty, and 'full' directory, confirms overwrite with checks
func TestConfirmReplace(t *testing.T) {
	empty := t.TempDir()
	full := t.TempDir()
	if err := os.MkdirAll(filepath.Join(full, "clusters", "prod"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "clusters", "prod", "kustomization.yaml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name                    string
		dir                     string
		cluster                 string
		force, yes, interactive bool
		answer                  string
		ok                      bool
		says                    string
	}{
		{"empty directory", empty, "prod", false, false, false, "", true, ""},
		{"missing directory", filepath.Join(empty, "new"), "prod", false, false, false, "", true, ""},
		{"not empty, no --force", full, "prod", false, false, true, "y\n", false, "--force"},
		{"--force, answered yes", full, "prod", true, false, true, "y\n", true, "1 file) will be deleted"},
		{"--force, answered no", full, "prod", true, false, true, "n\n", false, "cancelled"},
		{"--force, no answer", full, "prod", true, false, true, "", false, "cancelled"},
		{"--force, not interactive", full, "prod", true, false, false, "", false, "--yes"},
		{"--force --yes", full, "prod", true, true, false, "", true, "will be deleted"},
		// not empty, but holds no earlier export of this cluster: only another one
		{"--force, another cluster", full, "staging", true, true, false, "", true, "nothing is deleted"},
	} {
		var out bytes.Buffer
		err := confirmReplace(c.dir, c.cluster, c.force, c.yes, c.interactive, strings.NewReader(c.answer), &out)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v", c.name, err)
		}
		text := out.String()
		if err != nil {
			text += err.Error()
		}
		if !strings.Contains(text, c.says) {
			t.Errorf("%s: %q does not say %q", c.name, text, c.says)
		}
	}
}

// runs eksExec on the args from aws eks update-kubeconfig
func TestEKSFromKubeconfig(t *testing.T) {
	name, region, profile := eksExec("aws", []string{"--region", "us-east-2", "eks", "get-token", "--cluster-name", "e2e"},
		[]clientcmdapi.ExecEnvVar{{Name: "AWS_PROFILE", Value: "setdrift"}})
	if name != "e2e" || region != "us-east-2" || profile != "setdrift" {
		t.Errorf("exec: %s %s %s", name, region, profile)
	}
	if n, _, _ := eksExec("kubelogin", []string{"--cluster-name", "x"}, nil); n != "" {
		t.Error("non-aws exec read")
	}
	// aws.exe by path, --flag=value, and --profile winning over AWS_PROFILE
	name, region, profile = eksExec("/opt/aws/aws.exe", []string{"eks", "get-token", "--cluster-name=e2e", "--region=eu-west-1", "--profile", "flag"},
		[]clientcmdapi.ExecEnvVar{{Name: "AWS_PROFILE", Value: "env"}})
	if name != "e2e" || region != "eu-west-1" || profile != "flag" {
		t.Errorf("exec: %s %s %s", name, region, profile)
	}
	for arn, want := range map[string]string{
		"arn:aws:eks:us-east-2:111122223333:cluster/e2e":            "us-east-2/e2e",
		"arn:aws-cn:eks:cn-north-1:111122223333:cluster/e2e":        "cn-north-1/e2e",
		"arn:aws-us-gov:eks:us-gov-west-1:111122223333:cluster/e2e": "us-gov-west-1/e2e",
		"arn:aws:iam::111122223333:role/e2e":                        "",
		"kind-e2e":                                                  "",
	} {
		region, name, ok := eksARN(arn)
		if got := region + "/" + name; (ok && got != want) || (!ok && want != "") {
			t.Errorf("%s: %q %v", arn, got, ok)
		}
	}
	for host, want := range map[string]string{
		"https://ABCDEF.gr7.us-east-2.eks.amazonaws.com":     "us-east-2",
		"https://ABCDEF.yl4.eu-west-1.eks.amazonaws.com:443": "eu-west-1",
		"https://127.0.0.1:6443":                             "",
	} {
		if got := eksServerRegion(host); got != want {
			t.Errorf("%s: %q", host, got)
		}
	}
}

// loadConfig on a kubeconfig file, nothing is contacted
func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(`apiVersion: v1
kind: Config
clusters:
- {name: eks, cluster: {server: "https://ABCDEF.gr7.ap-south-1.eks.amazonaws.com"}}
users:
- name: aws
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: aws
      args: [eks, get-token, --cluster-name, other, --region, eu-west-1, --profile, team]
- {name: token, user: {token: x}}
contexts:
- {name: "arn:aws:eks:us-east-2:111122223333:cluster/prod", context: {cluster: eks, user: aws, namespace: payments}}
- {name: alias, context: {cluster: eks, user: token}}
current-context: "arn:aws:eks:us-east-2:111122223333:cluster/prod"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	// no --context: the current context
	kc, err := loadConfig(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if kc.name != "arn:aws:eks:us-east-2:111122223333:cluster/prod" || kc.namespace != "payments" {
		t.Errorf("current context: name %q namespace %q", kc.name, kc.namespace)
	}
	if kc.eksName != "prod" || kc.eksRegion != "us-east-2" || kc.awsProfile != "team" {
		t.Errorf("ARN over exec args: %s %s %s", kc.eksName, kc.eksRegion, kc.awsProfile)
	}
	if kc.config.QPS != 50 || !strings.HasPrefix(kc.config.UserAgent, "setdrift/") {
		t.Errorf("client settings: QPS %v, user agent %q", kc.config.QPS, kc.config.UserAgent)
	}

	// --context alias: no namespace set (not "default"), region from the server address
	kc, err = loadConfig(path, "alias")
	if err != nil {
		t.Fatal(err)
	}
	if kc.namespace != "" || kc.eksName != "" || kc.eksRegion != "ap-south-1" {
		t.Errorf("alias: namespace %q name %q region %q", kc.namespace, kc.eksName, kc.eksRegion)
	}
}

// --from-dir flag being set causes it to reject every other flag regarding AWS or a cluster
func TestFromDirRejectsClusterFlags(t *testing.T) {
	in := filepath.Join("..", "..", "testdata", "corpus", "basic", "input")
	cases := map[string][]string{
		"--kubeconfig":  {"--kubeconfig", "x"},
		"--context":     {"--context", "x"},
		"--namespace":   {"-n", "payments"},
		"--full-list":   {"--full-list"},
		"--aws":         {"--aws"},
		"--aws-cluster": {"--aws-cluster", "x"},
		"--aws-region":  {"--aws-region", "us-east-2"},
	}
	if len(cases) != len(clusterOnlyFlags) {
		t.Fatalf("test covers %d flags, clusterOnlyFlags has %d", len(cases), len(clusterOnlyFlags))
	}
	for flag, args := range cases {
		root := newRootCmd()
		root.SetArgs(append([]string{"export", "--from-dir", in, "-o", t.TempDir()}, args...))
		root.SetOut(&bytes.Buffer{})
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), flag) || !strings.Contains(err.Error(), "--from-dir") {
			t.Errorf("%v: got error %v, want one naming %s and --from-dir", args, err, flag)
		}
	}

	root := newRootCmd()
	root.SetArgs([]string{"export", "--from-dir", in, "--cluster-name", "test", "-o", t.TempDir()})
	root.SetOut(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatalf("--from-dir with --cluster-name: %v", err)
	}
}

// Bad flag values fail before anything is read or written
func TestBadFlagsFailBeforeReading(t *testing.T) {
	for _, c := range []struct {
		flag string
		args []string
	}{
		{"--group-by", []string{"--group-by", "apps"}},
		{"--external-secrets", []string{"--external-secrets", "vault"}},
		{"--secret-store-kind", []string{"--secret-store-kind", "Store"}},
		{"--secret-key-template", []string{"--secret-key-template", "{{.Name"}},
		// an unknown field only fails when the template runs
		{"--secret-key-template", []string{"--secret-key-template", "{{.Namespce}}/{{.Name}}"}},
		// --external-secrets none ignores these, so rejects them, even valid values
		{"--secret-store", []string{"--external-secrets", "none", "--secret-store", "vault"}},
		{"--secret-store-kind", []string{"--external-secrets", "none", "--secret-store-kind", "SecretStore"}},
		{"--secret-key-template", []string{"--external-secrets", "none", "--secret-key-template", "{{.Name}}"}},
		{"--helm-repos", []string{"--helm-repos", filepath.Join("no", "such", "helm-repos.yaml")}},
	} {
		flag, args := c.flag, c.args
		out := filepath.Join(t.TempDir(), "out")
		root := newRootCmd()
		root.SetArgs(append([]string{"export", "--from-dir", filepath.Join(t.TempDir(), "missing"), "-o", out}, args...))
		root.SetOut(&bytes.Buffer{})
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), flag) {
			t.Errorf("%v: got error %v, want one naming %s", args, err, flag)
		}
		if _, statErr := os.Stat(out); statErr == nil {
			t.Errorf("%v: output directory was created", args)
		}
	}
}

// Snapshot files are private to their owner, validates snapshot permissions
func TestSnapshotFilesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}
	dir := t.TempDir()
	pb, js := filepath.Join(dir, "snap.pb"), filepath.Join(dir, "snap.json")
	if err := os.WriteFile(pb, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := newRootCmd()
	root.SetArgs([]string{"export", "--from-dir", filepath.Join("..", "..", "testdata", "corpus", "basic", "input"),
		"-o", filepath.Join(dir, "out"), "--snapshot", pb, "--snapshot-json", js, "--force", "--yes"})
	root.SetOut(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{pb, js} {
		fi, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s: mode %o, want 600", filepath.Base(f), got)
		}
	}
}

// --aws fails on bad AWS credentials before the cluster read and before the output directory is touched
func TestAWSCheckedBeforeReading(t *testing.T) {
	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
users: [{name: u, user: {token: x}}]
contexts: [{name: k, context: {cluster: c, user: u}}]
current-context: k
`), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", empty)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", empty)
	t.Setenv("AWS_PROFILE", "setdrift-test-no-such-profile")

	out := filepath.Join(dir, "out")
	root := newRootCmd()
	root.SetArgs([]string{"export", "--kubeconfig", kubeconfig, "--aws", "--aws-cluster", "x", "--aws-region", "us-east-2", "-o", out})
	root.SetOut(&bytes.Buffer{})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--aws") {
		t.Errorf("got error %v, want one naming --aws", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("output directory was created")
	}
}

// --helm-repos fills in the repository of the release it names in
// RESTORE.md, and warns about release entries no release matches, not
// about chart entries (a shared file names charts other clusters run).
func TestHelmRepos(t *testing.T) {
	dir := t.TempDir()
	repos := filepath.Join(dir, "helm-repos.yaml")
	if err := os.WriteFile(repos, []byte("shop/web-config: https://charts.example.com\nshop/gone: https://other.example.com\nnot-in-this-cluster: https://other.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	var stderr bytes.Buffer
	root := newRootCmd()
	root.SetArgs([]string{"export", "--from-dir", filepath.Join("..", "..", "testdata", "corpus", "basic", "input"),
		"--cluster-name", "test", "-o", out, "--helm-repos", repos})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&stderr)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	restore, err := os.ReadFile(filepath.Join(out, "clusters", "test", "RESTORE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restore), "install web-config <chart> --repo https://charts.example.com ") {
		t.Errorf("RESTORE.md has no install command with the repository:\n%s", restore)
	}
	if strings.Contains(string(restore), "<repo>/") {
		t.Error("RESTORE.md still has a <repo> placeholder")
	}
	if !strings.Contains(stderr.String(), "shop/gone") || strings.Contains(stderr.String(), "not-in-this-cluster") {
		t.Errorf("want a warning about shop/gone only: %q", stderr.String())
	}
}

// The summary counts the ExternalSecrets written: Secrets with values to
// supply, not those whose only values a controller generates.
func TestSummaryCountsExternalSecrets(t *testing.T) {
	secret := func(name string, reason snapshotv1alpha1.RedactionReason) *snapshotv1alpha1.Resource {
		return &snapshotv1alpha1.Resource{ApiVersion: "v1", Kind: "Secret", Namespace: "shop", Name: name,
			Redactions: []*snapshotv1alpha1.Redaction{{Path: "data.k", Reason: reason}}}
	}
	s := &snapshotv1alpha1.Snapshot{Cluster: &snapshotv1alpha1.Cluster{Name: "prod"}, Resources: []*snapshotv1alpha1.Resource{
		secret("db", snapshotv1alpha1.RedactionReason_REDACTION_REASON_SECRET_VALUE),
		secret("webhook-cert", snapshotv1alpha1.RedactionReason_REDACTION_REASON_GENERATED_VALUE),
	}}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	printSummary(cmd, s, "out", false, true)
	if !strings.Contains(out.String(), "1 ExternalSecret for the redacted Secrets is in external-secrets/") {
		t.Errorf("summary:\n%s", out.String())
	}
}

// Snapshots go to <output>/snapshots/ by default, out of Git; the flags take
// a file or a directory, and a bad path fails before anything is read.
func TestSnapshotPaths(t *testing.T) {
	input := filepath.Join("..", "..", "testdata", "corpus", "basic", "input")
	export := func(t *testing.T, args ...string) error {
		t.Helper()
		root := newRootCmd()
		root.SetArgs(append([]string{"export", "--from-dir", input, "--cluster-name", "prod"}, args...))
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		return root.Execute()
	}
	snapshots := func(t *testing.T, dir string) []string {
		t.Helper()
		var out []string
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return out
	}

	t.Run("default", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "out")
		if err := export(t, "-o", out); err != nil {
			t.Fatal(err)
		}
		got := snapshots(t, filepath.Join(out, "snapshots"))
		if len(got) != 2 || !strings.HasSuffix(got[0], "-prod.json") || !strings.HasSuffix(got[1], "-prod.pb") {
			t.Errorf("snapshots = %v", got)
		}
		ignore, err := os.ReadFile(filepath.Join(out, ".gitignore"))
		if err != nil || !strings.Contains(string(ignore), "\n/snapshots/\n") {
			t.Errorf(".gitignore = %q, %v", ignore, err)
		}
		// exporting again keeps the earlier snapshots, and the .gitignore line once
		time.Sleep(1100 * time.Millisecond)
		if err := export(t, "-o", out, "--force", "--yes"); err != nil {
			t.Fatal(err)
		}
		if got := snapshots(t, filepath.Join(out, "snapshots")); len(got) != 4 {
			t.Errorf("after a second export: %v", got)
		}
		ignore, _ = os.ReadFile(filepath.Join(out, ".gitignore"))
		if strings.Count(string(ignore), "/snapshots/") != 1 {
			t.Errorf(".gitignore after a second export:\n%s", ignore)
		}
	})

	t.Run("an existing .gitignore gets the line appended", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "out")
		_ = os.MkdirAll(out, 0o755)
		_ = os.WriteFile(filepath.Join(out, ".gitignore"), []byte("*.tmp"), 0o644)
		if err := export(t, "-o", out, "--force", "--yes"); err != nil {
			t.Fatal(err)
		}
		ignore, _ := os.ReadFile(filepath.Join(out, ".gitignore"))
		if !strings.HasPrefix(string(ignore), "*.tmp\n") || !strings.HasSuffix(string(ignore), "/snapshots/\n") {
			t.Errorf(".gitignore = %q", ignore)
		}
	})

	t.Run("flags", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", filepath.Join(dir, "home"))
		_ = os.MkdirAll(filepath.Join(dir, "home"), 0o755)
		existing := filepath.Join(dir, "existing.pb")
		_ = os.WriteFile(existing, []byte("old"), 0o600)
		for _, c := range []struct {
			name string
			args []string
			// want is the snapshot file written, relative to dir; or wantErr
			want, wantErr string
		}{
			{"a file name", []string{"--snapshot", filepath.Join(dir, "foo")}, "foo", ""},
			{"any extension", []string{"--snapshot", filepath.Join(dir, "file.txt")}, "file.txt", ""},
			{"a directory", []string{"--snapshot", dir}, "*-prod.pb", ""},
			{"a new directory", []string{"--snapshot", filepath.Join(dir, "new") + "/"}, "new/*-prod.pb", ""},
			{"a missing parent", []string{"--snapshot", filepath.Join(dir, "a", "b", "snap.pb")}, "a/b/snap.pb", ""},
			{"~ unexpanded", []string{"--snapshot=~/"}, "home/*-prod.pb", ""},
			{"an existing file", []string{"--snapshot", existing}, "", "the file exists; pass --force"},
			{"the same file twice", []string{"--snapshot", filepath.Join(dir, "x"), "--snapshot-json", filepath.Join(dir, "x")}, "", "both name"},
			{"--no-snapshot with a path", []string{"--no-snapshot", "--snapshot", filepath.Join(dir, "y")}, "", "cannot be used with --snapshot"},
		} {
			out := filepath.Join(t.TempDir(), "out")
			err := export(t, append([]string{"-o", out}, c.args...)...)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("%s: err = %v, want %q", c.name, err, c.wantErr)
				}
				// checked before the read: nothing written
				if _, statErr := os.Stat(out); statErr == nil {
					t.Errorf("%s: the export was written before the path was refused", c.name)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
				continue
			}
			matches, _ := filepath.Glob(filepath.Join(dir, c.want))
			if len(matches) == 0 {
				t.Errorf("%s: no %s", c.name, c.want)
			}
		}
	})

	t.Run("--no-snapshot", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "out")
		if err := export(t, "-o", out, "--no-snapshot"); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"snapshots", ".gitignore"} {
			if _, err := os.Stat(filepath.Join(out, f)); err == nil {
				t.Errorf("--no-snapshot wrote %s", f)
			}
		}
	})
}
