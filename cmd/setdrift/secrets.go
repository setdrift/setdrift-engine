// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// setdrift secrets export: copies the values of an export's redacted
// Secrets from the source cluster into a secret manager, and writes the
// ExternalSecrets that read them. The only command that handles secret
// values; `export` never does. Every setting is given explicitly, and every
// check runs before anything is written.

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

	"setdrift.io/setdrift-engine/pkg/collect"
	"setdrift.io/setdrift-engine/pkg/emit/kustomize"
	"setdrift.io/setdrift-engine/pkg/secretexport"
)

type secretsExportOptions struct {
	aws                 bool
	kubeconfig, context string
	secretManager       string
	keyTemplate         string
	storeKind           string
	storeName           string
	out                 string
	region, profile     string
	kmsKeyID            string
	overwrite, yes      bool
	force               bool
	check               bool
	report              string
	otherCluster        bool
}

func newSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Work with the secret values an export left out",
	}
	cmd.AddCommand(newSecretsExportCmd())
	return cmd
}

func newSecretsExportCmd() *cobra.Command {
	o := &secretsExportOptions{}
	cmd := &cobra.Command{
		Use:   "export --aws REDACTED_DIR",
		Short: "Copy redacted Secret values from the source cluster into AWS, and write the ExternalSecrets that read them",
		Long: `An export leaves every Secret's values out: it writes them under redacted/
with placeholders. This command copies the values of the Secrets you choose
from the source cluster into a secret manager, and writes the ExternalSecrets
(and their store) that make External Secrets Operator restore them.

REDACTED_DIR is the export's redacted/ folder, or a folder in it, such as
redacted/namespaces/shop: the Secrets in it are the ones copied. It works
with any export, however --external-secrets was set.

Every setting is given, not taken from the export: the secret manager, the
region, the remote key names, the store, and where to write the
ExternalSecrets. Only AWS is supported so far, so --aws is required.

Before anything is written, it checks, and reports:
  inputs        the settings, and the remote key names they make
  folder        the redacted files are an export's, and which are Secrets
  source        the context is the export's source cluster, and may get
                the Secrets
  AWS identity  whose credentials are used
  store         the secret manager answers in the region; the KMS key is
                usable
  permissions   IAM allows each action on each key (by IAM's policy
                simulator; without access to it, read access is probed and
                writes are verified by the first one)
Then it reads the Secrets' values and shows the plan. It writes only after
you confirm (--yes confirms without a prompt), then reads each remote secret
back and reports whether it exists and holds the Secret's keys and values.
--check runs every check and that report, and writes nothing.

It is the only SetDrift command that reads secret values. They are held in
memory only: never written to disk, to the export or to Git, and never
printed. Remote secrets that exist already are skipped; --overwrite
replaces them.`,
		Example: `  setdrift secrets export --aws ./prod-export/clusters/prod/redacted \
    --context prod --region us-east-2 --secret-manager aws-secrets-manager \
    --secret-key-template '{{.Cluster}}/{{.Namespace}}/{{.Name}}' \
    --store-kind ClusterSecretStore --store-name secret-store \
    --external-secrets-out ./prod-export/clusters/prod/external-secrets --force

  # one namespace only, and check what AWS holds without writing
  setdrift secrets export --aws ./prod-export/clusters/prod/redacted/namespaces/shop --check \
    --context prod --region us-east-2 --secret-manager aws-parameter-store \
    --secret-key-template '/prod/{{.Namespace}}/{{.Name}}' \
    --store-kind ClusterSecretStore --store-name params`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return o.run(cmd, args[0]) },
	}
	f := cmd.Flags()
	f.BoolVar(&o.aws, "aws", false, "write to AWS (required: the only secret manager supported so far)")
	f.StringVar(&o.context, "context", "", "kubeconfig context of the source cluster, whose Secrets are read (required)")
	f.StringVar(&o.region, "region", "", "AWS region of the secret manager (required)")
	f.StringVar(&o.secretManager, "secret-manager", "", "aws-secrets-manager or aws-parameter-store (required)")
	f.StringVar(&o.keyTemplate, "secret-key-template", "", "remote key of each Secret, a Go template with .Cluster, .Namespace and .Name, e.g. {{.Cluster}}/{{.Namespace}}/{{.Name}}, or /{{.Cluster}}/... for Parameter Store (required)")
	f.StringVar(&o.storeKind, "store-kind", "", "ClusterSecretStore or SecretStore, for the ExternalSecrets (required)")
	f.StringVar(&o.storeName, "store-name", "", "name of the store the ExternalSecrets read from (required)")
	f.StringVar(&o.out, "external-secrets-out", "", "directory to write the ExternalSecrets and their store to (required unless --check)")
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: KUBECONFIG or ~/.kube/config)")
	f.StringVar(&o.profile, "profile", "", "AWS profile (default: AWS_PROFILE, or the default credentials)")
	f.StringVar(&o.kmsKeyID, "kms-key-id", "", "KMS key to encrypt new secrets with (default: the account's AWS managed key)")
	f.BoolVar(&o.overwrite, "overwrite", false, "replace remote secrets that exist already (they are skipped otherwise)")
	f.BoolVar(&o.force, "force", false, "write into a non-empty --external-secrets-out, replacing what is there")
	f.BoolVarP(&o.yes, "yes", "y", false, "write without asking for confirmation")
	f.BoolVar(&o.check, "check", false, "run every check and report what AWS holds, writing nothing")
	f.StringVar(&o.report, "report", "", "also write the checks, the plan and the report as JSON to this file")
	f.BoolVar(&o.otherCluster, "other-cluster", false, "read the values from a cluster other than the export's source (e.g. one restored from it)")
	return cmd
}

// checkResult is one check, as printed and reported.
type checkResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, warning, failed
	Detail string `json:"detail"`
}

// secretsRun is what a run found, for printing and the JSON report.
type secretsRun struct {
	Checks []checkResult        `json:"checks"`
	Plan   []planRow            `json:"plan,omitempty"`
	Report []secretexport.Check `json:"report,omitempty"`
	Wrote  int                  `json:"wrote"`
	w      io.Writer
}

type planRow struct {
	Status string   `json:"status"`
	Key    string   `json:"key"`
	Secret string   `json:"secret"`
	Keys   []string `json:"keys"`
	Binary []string `json:"binary,omitempty"`
	Merge  bool     `json:"merge,omitempty"`
	Note   string   `json:"note,omitempty"`
}

func (r *secretsRun) add(name, status, format string, args ...any) bool {
	c := checkResult{Name: name, Status: status, Detail: fmt.Sprintf(format, args...)}
	r.Checks = append(r.Checks, c)
	fmt.Fprintf(r.w, "  %-8s %-13s %s\n", c.Status, c.Name, c.Detail)
	return status != "failed"
}

func (r *secretsRun) failed() []string {
	var out []string
	for _, c := range r.Checks {
		if c.Status == "failed" {
			out = append(out, c.Name)
		}
	}
	return out
}

var storeNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// required lists the settings the run needs and lacks: none is taken from the export.
func (o *secretsExportOptions) required() error {
	if !o.aws {
		return errors.New("secrets export writes the values to a secret manager, and supports AWS only so far: pass --aws " +
			"(with --region, --secret-manager and the other settings; see setdrift secrets export --help)")
	}
	var missing []string
	for _, f := range []struct{ name, value string }{
		{"--context", o.context}, {"--region", o.region}, {"--secret-manager", o.secretManager},
		{"--secret-key-template", o.keyTemplate}, {"--store-kind", o.storeKind}, {"--store-name", o.storeName},
	} {
		if f.value == "" {
			missing = append(missing, f.name)
		}
	}
	if o.out == "" && !o.check {
		missing = append(missing, "--external-secrets-out")
	}
	if len(missing) > 0 {
		return fmt.Errorf("secrets export takes every setting explicitly, none from the export: missing %s", strings.Join(missing, ", "))
	}
	return nil
}

func (o *secretsExportOptions) run(cmd *cobra.Command, dir string) error {
	if err := o.required(); err != nil {
		return err
	}
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	run := &secretsRun{w: w}
	defer o.writeReport(run)
	fmt.Fprintln(w, "Checks (nothing is read from the cluster's Secrets or written until every check passes):")

	// inputs
	manager := secretexport.Manager(o.secretManager)
	ok := true
	switch {
	case manager != secretexport.SecretsManagerID && manager != secretexport.ParameterStoreID:
		ok = run.add("inputs", "failed", "--secret-manager %s: use aws-secrets-manager or aws-parameter-store", o.secretManager)
	case o.storeKind != "ClusterSecretStore" && o.storeKind != "SecretStore":
		ok = run.add("inputs", "failed", "--store-kind %s: use ClusterSecretStore or SecretStore", o.storeKind)
	case !storeNameRE.MatchString(o.storeName) || len(o.storeName) > 253:
		ok = run.add("inputs", "failed", "--store-name %q: a Kubernetes name, lowercase letters, digits, - and .", o.storeName)
	}
	if ok && !o.check {
		if entries, err := os.ReadDir(o.out); err == nil && len(entries) > 0 && !o.force {
			ok = run.add("inputs", "failed", "--external-secrets-out %s is not empty: pass --force to replace what is in it, or name a new directory", o.out)
		} else if err == nil && !o.force {
			// empty: fine
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			ok = run.add("inputs", "failed", "--external-secrets-out %s: %v", o.out, err)
		}
	}

	// folder
	folder, err := secretexport.LoadRedacted(dir)
	info, root := exportOf(dir)
	var keys []string
	switch {
	case err != nil:
		run.add("folder", "failed", "%v", err)
	case info == nil:
		run.add("folder", "failed", "%s is not in a SetDrift export (no .setdrift-export above it): give the export's redacted/ folder, or a folder in it", dir)
	case len(folder.Invalid) > 0:
		run.add("folder", "failed", "%d files are not what an export writes: %s", len(folder.Invalid), strings.Join(folder.Invalid, "; "))
	case len(folder.Secrets) == 0:
		run.add("folder", "failed", "%s holds no redacted Secrets (an export writes them under redacted/namespaces/<namespace>/secret/)", dir)
	default:
		detail := fmt.Sprintf("%s in %s, from the export of %s", plural(len(folder.Secrets), "Secret", "Secrets"), dir, info.Cluster)
		status := "ok"
		if len(folder.Others) > 0 {
			status = "warning"
			detail += fmt.Sprintf("; %s hold secret values in their manifests and can't be copied as they are: "+
				"move each value into a Secret, export again, and copy that: %s",
				plural(len(folder.Others), "other resource", "other resources"), strings.Join(folder.Others, ", "))
		}
		run.add("folder", status, "%s", detail)
		keys, err = secretexport.RemoteKeys(manager, o.keyTemplate, info.Cluster, folder.Secrets)
		if err != nil {
			run.add("inputs", "failed", "%v", err)
		} else if ok {
			run.add("inputs", "ok", "%s in %s; remote keys like %s; the ExternalSecrets read %s %s%s",
				manager, o.region, keys[0], o.storeKind, o.storeName, outNote(o))
		}
	}

	// source cluster
	var dyn dynamic.Interface
	kc, err := loadConfig(o.kubeconfig, o.context)
	if err == nil {
		err = checkReachable(ctx, kc)
	}
	if err == nil {
		dyn, err = dynamic.NewForConfig(kc.config)
	}
	if err != nil {
		run.add("source", "failed", "context %s: %v", o.context, err)
	} else if info != nil {
		if msg := o.checkSource(ctx, dyn, info); msg != "" {
			run.add("source", "failed", "%s", msg)
		} else if folder != nil {
			var refused []string
			for _, ns := range secretNamespaces(folder.Secrets) {
				if allowed, known := collect.Can(ctx, dyn, "get", "", "secrets", ns); known && !allowed {
					refused = append(refused, ns)
				}
			}
			if len(refused) > 0 {
				who := collect.Identity(ctx, dyn)
				run.add("source", "failed", "%s may not get Secrets in %s: grant get on secrets there "+
					"(kubectl create rolebinding setdrift-secrets -n %s --clusterrole=view is not enough: view leaves out Secrets; "+
					"use a role with get on secrets)", orDefault(who, "these credentials"), strings.Join(refused, ", "), refused[0])
			} else {
				run.add("source", "ok", "%s is the export's source cluster%s; may get Secrets in %s",
					o.context, otherNote(o), strings.Join(secretNamespaces(folder.Secrets), ", "))
			}
		}
	}

	// AWS identity, store, permissions
	var prov secretexport.Provider
	awsOK := false
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(o.region)}
	if o.profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(o.profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	var caller secretexport.Caller
	if err == nil {
		caller, err = secretexport.Identity(ctx, sts.NewFromConfig(cfg), iam.NewFromConfig(cfg))
	}
	if err != nil {
		run.add("AWS identity", "failed", "%v (check the profile, or log in: aws sso login)", err)
	} else {
		run.add("AWS identity", "ok", "%s, account %s", caller.ARN, caller.Account)
		tags := map[string]string{"setdrift.io/source-cluster": ""}
		if info != nil {
			tags["setdrift.io/source-cluster"] = info.Cluster
		}
		var answers func(context.Context) error
		if manager == secretexport.ParameterStoreID {
			ps := secretexport.ParameterStore{Client: ssm.NewFromConfig(cfg), KMSKeyID: o.kmsKeyID, Tags: tags}
			prov, answers = ps, ps.Answers
		} else {
			sm := secretexport.SecretsManager{Client: secretsmanager.NewFromConfig(cfg), KMSKeyID: o.kmsKeyID, Tags: tags}
			prov, answers = sm, sm.Answers
		}
		kmsARN := ""
		if err := answers(ctx); err != nil {
			run.add("store", "failed", "%s does not answer in %s: %v (check --region)", prov.Name(), o.region, err)
		} else if o.kmsKeyID != "" {
			if kmsARN, err = secretexport.KMSKey(ctx, kms.NewFromConfig(cfg), o.kmsKeyID); err != nil {
				run.add("store", "failed", "%s answers in %s, but KMS key %s: %v", prov.Name(), o.region, o.kmsKeyID, err)
			} else {
				run.add("store", "ok", "%s answers in %s; KMS key %s is enabled", prov.Name(), o.region, kmsARN)
				awsOK = true
			}
		} else {
			run.add("store", "ok", "%s answers in %s; new secrets use the account's AWS managed key", prov.Name(), o.region)
			awsOK = true
		}
		if awsOK && len(keys) > 0 {
			perm := secretexport.Simulate(ctx, iam.NewFromConfig(cfg), caller, manager, o.region, keys, kmsARN)
			switch {
			case len(perm.Denied) > 0:
				run.add("permissions", "failed", "IAM does not allow %s for %s", strings.Join(perm.Denied, "; "), caller.Principal)
			case perm.Simulated:
				run.add("permissions", "ok", "IAM allows %s on all %d keys", strings.Join(secretexport.Actions(manager, kmsARN != ""), ", "), len(keys))
			default:
				if denied := secretexport.Probe(ctx, prov, keys); len(denied) > 0 {
					run.add("permissions", "failed", "may not read %d of the %d keys (e.g. %s); and %s", len(denied), len(keys), denied[0], perm.Unverified)
				} else if o.check {
					run.add("permissions", "ok", "may read all %d keys (%s; --check writes nothing)", len(keys), perm.Unverified)
				} else {
					run.add("permissions", "warning", "may read all %d keys; writes can't be verified in advance: %s. "+
						"The first refused write stops the run", len(keys), perm.Unverified)
				}
			}
		}
	}

	if failed := run.failed(); len(failed) > 0 {
		return fmt.Errorf("%s failed (%s): nothing was read from the Secrets or written", plural(len(failed), "check", "checks"), strings.Join(failed, ", "))
	}

	// plan: the values are read now, into memory
	remotes := secretexport.Remotes(folder.Secrets, keys)
	plan, err := secretexport.MakePlan(ctx, remotes, secretexport.ClusterSecrets{Client: dyn}, prov, o.overwrite)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "\nPlan (%s from %s into %s in %s; values are not shown):\n", plural(len(plan.Items), "Secret", "Secrets"), o.context, prov.Name(), o.region)
	for _, it := range plan.Items {
		row := planRow{Status: string(it.Status), Key: it.Key, Secret: it.Namespace + "/" + it.Secret, Binary: it.Binary, Merge: it.Merge, Note: it.Note}
		for k := range it.Properties {
			row.Keys = append(row.Keys, k)
		}
		sort.Strings(row.Keys)
		run.Plan = append(run.Plan, row)
		line := fmt.Sprintf("  %-14s %s  (%s, from Secret %s)", row.Status, row.Key, strings.Join(row.Keys, ", "), row.Secret)
		if row.Merge {
			line += "; merge: a controller writes its other keys"
		}
		if len(row.Binary) > 0 {
			line += "; binary, stored base64: " + strings.Join(row.Binary, ", ")
		}
		if row.Note != "" {
			line += ": " + row.Note
		}
		fmt.Fprintln(w, line)
	}
	if bad := planErrors(plan); bad != nil {
		return fmt.Errorf("%w; the redacted folder names Secrets or keys the source cluster lacks: nothing was written", bad)
	}

	if o.check {
		return o.verify(ctx, run, plan, prov, "Report (--check: nothing written)")
	}
	n := plan.Writes()
	if n > 0 {
		interactive := false
		if f, ok := cmd.InOrStdin().(*os.File); ok {
			interactive = term.IsTerminal(int(f.Fd()))
		}
		if !o.yes {
			if !interactive {
				return fmt.Errorf("%s would be written: pass --yes to write them without a prompt", plural(n, "remote secret", "remote secrets"))
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "\nWrite %s to %s in %s, and the ExternalSecrets to %s? [y/N] ", plural(n, "remote secret", "remote secrets"), prov.Name(), o.region, o.out)
			answer, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
				return errors.New("nothing written")
			}
		}
		if err := plan.Apply(ctx, prov); err != nil {
			fmt.Fprintf(w, "\nA write failed: %v\n", err)
		}
		run.Wrote = n
	}
	verifyErr := o.verify(ctx, run, plan, prov, "Report (read back from "+prov.Name()+")")

	// the ExternalSecrets, for every Secret now in AWS
	x := &kustomize.ExternalSecrets{StoreKind: o.storeKind, StoreName: o.storeName, Provider: prov.Name(),
		Service: map[secretexport.Manager]string{secretexport.SecretsManagerID: "SecretsManager", secretexport.ParameterStoreID: "ParameterStore"}[manager]}
	var sources []kustomize.SecretSource
	for i, it := range plan.Items {
		if i < len(run.Report) && !run.Report[i].Exists {
			continue
		}
		src := folder.Secrets[i]
		sources = append(sources, kustomize.SecretSource{Namespace: src.Namespace, Name: src.Name, Type: src.Type,
			Labels: src.Labels, Annotations: src.Annotations, Keys: src.Keys, RemoteKey: it.Key, Merge: it.Merge, Binary: it.Binary})
	}
	if o.force {
		if err := os.RemoveAll(o.out); err != nil {
			return err
		}
	}
	if err := kustomize.WriteExternalSecrets(o.out, x, o.region, sources); err != nil {
		return fmt.Errorf("writing the ExternalSecrets: %w", err)
	}
	fmt.Fprintf(w, "\nWrote %s and their store to %s. In the new cluster, with External Secrets Operator installed and given access to %s: "+
		"kubectl apply -k %s\n", plural(len(sources), "ExternalSecret", "ExternalSecrets"), o.out, prov.Name(), o.out)
	_ = root
	return verifyErr
}

// verify reads every remote secret back and prints the report; an error when any does not hold the Secret's values.
func (o *secretsExportOptions) verify(ctx context.Context, run *secretsRun, plan *secretexport.Plan, prov secretexport.Provider, title string) error {
	run.Report = plan.Verify(ctx, prov)
	w := run.w
	fmt.Fprintf(w, "\n%s:\n", title)
	fmt.Fprintf(w, "  %-8s %-40s %-28s %s\n", "result", "remote secret", "Secret", "detail")
	bad := 0
	for _, c := range run.Report {
		result := "ok"
		if !c.OK() {
			result, bad = "failed", bad+1
		}
		detail := c.Note
		if len(c.Missing) > 0 {
			detail += "; missing keys: " + strings.Join(c.Missing, ", ")
		}
		if len(c.Extra) > 0 {
			detail += "; extra keys: " + strings.Join(c.Extra, ", ")
		}
		if detail == "" {
			detail = "exists, same keys and values"
		}
		fmt.Fprintf(w, "  %-8s %-40s %-28s %s\n", result, c.Key, c.Secret, strings.TrimPrefix(detail, "; "))
	}
	if bad > 0 {
		return fmt.Errorf("%s of %d do not hold the Secret's values in %s", plural(bad, "remote secret", "remote secrets"), len(run.Report), prov.Name())
	}
	fmt.Fprintf(w, "\nAll %d remote secrets hold their Secret's keys and values.\n", len(run.Report))
	return nil
}

func (o *secretsExportOptions) writeReport(run *secretsRun) {
	if o.report == "" {
		return
	}
	b, err := json.MarshalIndent(run, "", "  ")
	if err == nil {
		err = os.WriteFile(o.report, append(b, '\n'), 0o600)
	}
	if err != nil {
		fmt.Fprintf(run.w, "warning: --report %s: %v\n", o.report, err)
	}
}

// exportOf finds the export a folder is in: the nearest .setdrift-export
// above it (the cluster's directory), and its info.
func exportOf(dir string) (*kustomize.ExportInfo, string) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, ""
	}
	for d := abs; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, kustomize.ExportInfoFile)); err == nil {
			info, err := kustomize.ReadExportInfo(d)
			if err != nil || info == nil {
				return nil, ""
			}
			return info, d
		}
		if filepath.Dir(d) == d {
			return nil, ""
		}
	}
}

// checkSource refuses a cluster other than the export's source, compared by
// kube-system UID: its values would restore the wrong secrets.
func (o *secretsExportOptions) checkSource(ctx context.Context, dyn dynamic.Interface, info *kustomize.ExportInfo) string {
	if o.otherCluster || info.KubeSystemUID == "" {
		return ""
	}
	ns, err := dyn.Resource(namespacesGVR).Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return "checking this is the export's source cluster: " + err.Error()
	}
	if string(ns.GetUID()) != info.KubeSystemUID {
		return fmt.Sprintf("context %s is not the cluster the export came from (%s): its Secret values would not be the ones the "+
			"export redacted. Use the source cluster's context, or pass --other-cluster to read from this one anyway", o.context, info.Cluster)
	}
	return ""
}

func secretNamespaces(secrets []secretexport.Source) []string {
	var out []string
	for _, s := range secrets {
		if len(out) == 0 || out[len(out)-1] != s.Namespace {
			out = append(out, s.Namespace)
		}
	}
	return out
}

func outNote(o *secretsExportOptions) string {
	if o.check {
		return ""
	}
	return ", written to " + o.out
}

func otherNote(o *secretsExportOptions) string {
	if o.otherCluster {
		return " (or --other-cluster)"
	}
	return ""
}

// planErrors is an error when some remote secrets could not be planned or written.
func planErrors(plan *secretexport.Plan) error {
	var bad []string
	for _, it := range plan.Items {
		if it.Status == secretexport.Missing || it.Status == secretexport.Failed {
			bad = append(bad, it.Key)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s can't be copied: %s", plural(len(bad), "remote secret", "remote secrets"), strings.Join(bad, ", "))
	}
	return nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
