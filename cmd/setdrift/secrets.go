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
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/collect"
	"setdrift.io/setdrift-engine/pkg/emit/kustomize"
	"setdrift.io/setdrift-engine/pkg/secretexport"
)

// secretsExportOptions holds secrets export's flags.
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

// newSecretsCmd builds the secrets command.
func newSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Work with the secret values an export left out",
	}
	cmd.AddCommand(newSecretsExportCmd())
	return cmd
}

// newSecretsExportCmd builds secrets export.
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

// The statuses of a check.
const (
	statusOK      = "ok"
	statusWarning = "warning"
	statusFailed  = "failed"
)

// checkResult is one check, as printed and reported.
type checkResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // statusOK, statusWarning, statusFailed
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

// planRow is one remote secret of the plan, as printed and reported.
type planRow struct {
	Status string   `json:"status"`
	Key    string   `json:"key"`
	Secret string   `json:"secret"`
	Keys   []string `json:"keys"`
	Binary []string `json:"binary,omitempty"`
	Merge  bool     `json:"merge,omitempty"`
	Note   string   `json:"note,omitempty"`
}

// add records and prints a check; it reports whether the check passed.
func (r *secretsRun) add(name, status, format string, args ...any) bool {
	c := checkResult{Name: name, Status: status, Detail: fmt.Sprintf(format, args...)}
	r.Checks = append(r.Checks, c)
	fmt.Fprintf(r.w, "  %-8s %-13s %s\n", c.Status, c.Name, c.Detail)
	return status != statusFailed
}

// failed returns the names of the checks that failed.
func (r *secretsRun) failed() []string {
	var out []string
	for _, c := range r.Checks {
		if c.Status == statusFailed {
			out = append(out, c.Name)
		}
	}
	return out
}

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

// run checks everything, plans, writes after confirmation, reads each
// remote secret back, and writes the ExternalSecrets.
func (o *secretsExportOptions) run(cmd *cobra.Command, dir string) error {
	if err := o.required(); err != nil {
		return err
	}
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	run := &secretsRun{w: w}
	defer o.writeReport(run)
	fmt.Fprintln(w, "Checks (nothing is read from the cluster's Secrets or written until every check passes):")

	manager := secretexport.Manager(o.secretManager)
	inputsOK := o.checkInputs(run, manager)
	folder, info, keys := o.checkFolder(run, dir, manager, inputsOK)
	dyn := o.checkSourceCluster(ctx, run, info, folder)
	prov := o.checkAWS(ctx, run, info, manager, keys)
	if failed := run.failed(); len(failed) > 0 {
		return fmt.Errorf("%s failed (%s): nothing was read from the Secrets or written", text.Plural(len(failed), "check", "checks"), strings.Join(failed, ", "))
	}

	// plan: the values are read now, into memory
	plan, err := o.plan(ctx, run, folder, keys, dyn, prov)
	if err != nil {
		return err
	}
	if o.check {
		return o.verify(ctx, run, plan, prov, "Report (--check: nothing written)")
	}
	applyErr, err := o.write(cmd, run, plan, prov)
	if err != nil {
		return err
	}
	verifyErr := o.verify(ctx, run, plan, prov, "Report (read back from "+prov.Name()+")")
	if err := o.writeExternalSecrets(w, run, folder, plan, prov, manager); err != nil {
		return err
	}
	return errors.Join(applyErr, verifyErr)
}

// checkInputs checks the settings; it reports whether they passed.
func (o *secretsExportOptions) checkInputs(run *secretsRun, manager secretexport.Manager) bool {
	ok := true
	switch {
	case manager != secretexport.SecretsManagerID && manager != secretexport.ParameterStoreID:
		ok = run.add("inputs", statusFailed, "--secret-manager %s: use aws-secrets-manager or aws-parameter-store", o.secretManager)
	case o.storeKind != "ClusterSecretStore" && o.storeKind != "SecretStore":
		ok = run.add("inputs", statusFailed, "--store-kind %s: use ClusterSecretStore or SecretStore", o.storeKind)
	case kustomize.CheckStore(o.storeKind, o.storeName) != nil:
		ok = run.add("inputs", statusFailed, "--store-name %q: a Kubernetes name, lowercase letters, digits, - and .", o.storeName)
	}
	if !ok || o.check {
		return ok
	}
	entries, err := os.ReadDir(o.out)
	switch {
	case err == nil && len(entries) > 0 && !o.force:
		ok = run.add("inputs", statusFailed, "--external-secrets-out %s is not empty: pass --force to replace what is in it, or name a new directory", o.out)
	case err == nil && len(entries) > 0:
		if err := replaceableOutput(o.out); err != nil {
			ok = run.add("inputs", statusFailed, "--external-secrets-out %s with --force: %v", o.out, err)
		}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		ok = run.add("inputs", statusFailed, "--external-secrets-out %s: %v", o.out, err)
	}
	return ok
}

// checkFolder reads the redacted folder and the export it is in, and names
// each Secret's remote key; with inputsOK, it reports the inputs as ok.
func (o *secretsExportOptions) checkFolder(run *secretsRun, dir string, manager secretexport.Manager, inputsOK bool) (*secretexport.Folder, *kustomize.ExportInfo, []string) {
	folder, err := secretexport.LoadRedacted(dir)
	info := exportOf(dir)
	switch {
	case err != nil:
		run.add("folder", statusFailed, "%v", err)
	case info == nil:
		run.add("folder", statusFailed, "%s is not in a SetDrift export (no .setdrift-export above it): give the export's redacted/ folder, or a folder in it", dir)
	case len(folder.Invalid) > 0:
		run.add("folder", statusFailed, "%d files are not what an export writes: %s", len(folder.Invalid), strings.Join(folder.Invalid, "; "))
	case len(folder.Secrets) == 0:
		run.add("folder", statusFailed, "%s holds no redacted Secrets (an export writes them under redacted/namespaces/<namespace>/secret/)", dir)
	default:
		detail := fmt.Sprintf("%s in %s, from the export of %s", text.Plural(len(folder.Secrets), "Secret", "Secrets"), dir, info.Cluster)
		status := statusOK
		if len(folder.Others) > 0 {
			status = statusWarning
			detail += fmt.Sprintf("; %s hold secret values in their manifests and can't be copied as they are: "+
				"move each value into a Secret, export again, and copy that: %s",
				text.Plural(len(folder.Others), "other resource", "other resources"), strings.Join(folder.Others, ", "))
		}
		run.add("folder", status, "%s", detail)
		keys, err := secretexport.RemoteKeys(manager, o.keyTemplate, info.Cluster, folder.Secrets)
		if err != nil {
			run.add("inputs", statusFailed, "%v", err)
			return folder, info, nil
		}
		if inputsOK {
			run.add("inputs", statusOK, "%s in %s; remote keys like %s; the ExternalSecrets read %s %s%s",
				manager, o.region, keys[0], o.storeKind, o.storeName, outNote(o))
		}
		return folder, info, keys
	}
	return folder, info, nil
}

// checkSourceCluster checks the context reaches the export's source
// cluster, and may get the Secrets; it returns the client to read them with.
func (o *secretsExportOptions) checkSourceCluster(ctx context.Context, run *secretsRun, info *kustomize.ExportInfo, folder *secretexport.Folder) dynamic.Interface {
	var dyn dynamic.Interface
	kc, err := loadConfig(o.kubeconfig, o.context)
	if err == nil {
		warnInsecure(run.w, kc)
		err = checkReachable(ctx, kc)
	}
	if err == nil {
		dyn, err = dynamic.NewForConfig(kc.config)
	}
	if err != nil {
		run.add("source", statusFailed, "context %s: %v", o.context, err)
		return nil
	}
	if info == nil {
		return dyn
	}
	if err := o.checkSource(ctx, dyn, info); err != nil {
		run.add("source", statusFailed, "%v", err)
		return dyn
	}
	if folder == nil {
		return dyn
	}
	var refused []string
	for _, ns := range secretNamespaces(folder.Secrets) {
		if allowed, known := collect.Can(ctx, dyn, "get", "", "secrets", ns); known && !allowed {
			refused = append(refused, ns)
		}
	}
	if len(refused) > 0 {
		who := collect.Identity(ctx, dyn)
		run.add("source", statusFailed, "%s may not get Secrets in %s: grant get on secrets there "+
			"(kubectl create rolebinding setdrift-secrets -n %s --clusterrole=view is not enough: view leaves out Secrets; "+
			"use a role with get on secrets)", cmp.Or(who, "these credentials"), strings.Join(refused, ", "), refused[0])
		return dyn
	}
	run.add("source", statusOK, "%s is the export's source cluster%s; may get Secrets in %s",
		o.context, otherNote(o), strings.Join(secretNamespaces(folder.Secrets), ", "))
	return dyn
}

// checkAWS checks whose AWS credentials are used, that the secret manager
// answers in the region (and the KMS key is usable), and that IAM allows
// the run; it returns the secret manager.
func (o *secretsExportOptions) checkAWS(ctx context.Context, run *secretsRun, info *kustomize.ExportInfo, manager secretexport.Manager, keys []string) secretexport.Provider {
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
		run.add("AWS identity", statusFailed, "%v (check the profile, or log in: aws sso login)", err)
		return nil
	}
	run.add("AWS identity", statusOK, "%s, account %s", caller.ARN, caller.Account)
	tags := map[string]string{"setdrift.io/source-cluster": ""}
	if info != nil {
		tags["setdrift.io/source-cluster"] = info.Cluster
	}
	var prov secretexport.Provider
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
		run.add("store", statusFailed, "%s does not answer in %s: %v (check --region)", prov.Name(), o.region, err)
		return prov
	}
	if o.kmsKeyID != "" {
		if kmsARN, err = secretexport.KMSKey(ctx, kms.NewFromConfig(cfg), o.kmsKeyID); err != nil {
			run.add("store", statusFailed, "%s answers in %s, but KMS key %s: %v", prov.Name(), o.region, o.kmsKeyID, err)
			return prov
		}
		run.add("store", statusOK, "%s answers in %s; KMS key %s is enabled", prov.Name(), o.region, kmsARN)
	} else {
		run.add("store", statusOK, "%s answers in %s; new secrets use the account's AWS managed key", prov.Name(), o.region)
	}
	if len(keys) > 0 {
		o.checkPermissions(ctx, run, iam.NewFromConfig(cfg), caller, manager, prov, keys, kmsARN)
	}
	return prov
}

// checkPermissions checks IAM allows each action on each key: by IAM's
// policy simulator, or, without access to it, by probing reads.
func (o *secretsExportOptions) checkPermissions(ctx context.Context, run *secretsRun, i secretexport.IAMAPI, caller secretexport.Caller,
	manager secretexport.Manager, prov secretexport.Provider, keys []string, kmsARN string) {
	perm := secretexport.Simulate(ctx, i, caller, manager, o.region, keys, kmsARN)
	switch {
	case len(perm.Denied) > 0:
		run.add("permissions", statusFailed, "IAM does not allow %s for %s", strings.Join(perm.Denied, "; "), caller.Principal)
	case perm.Simulated:
		run.add("permissions", statusOK, "IAM allows %s on all %d keys", strings.Join(secretexport.Actions(manager, kmsARN != ""), ", "), len(keys))
	default:
		if denied := secretexport.Probe(ctx, prov, keys); len(denied) > 0 {
			run.add("permissions", statusFailed, "may not read %d of the %d keys (e.g. %s); and %s", len(denied), len(keys), denied[0], perm.Unverified)
		} else if o.check {
			run.add("permissions", statusOK, "may read all %d keys (%s; --check writes nothing)", len(keys), perm.Unverified)
		} else {
			run.add("permissions", statusWarning, "may read all %d keys; writes can't be verified in advance: %s. "+
				"The first refused write stops the run", len(keys), perm.Unverified)
		}
	}
}

// plan reads the Secrets' values into memory and prints the plan; an error
// when a Secret or key the folder names is not in the source cluster.
func (o *secretsExportOptions) plan(ctx context.Context, run *secretsRun, folder *secretexport.Folder, keys []string,
	dyn dynamic.Interface, prov secretexport.Provider) (*secretexport.Plan, error) {
	remotes, err := secretexport.Remotes(folder.Secrets, keys)
	if err != nil {
		return nil, err
	}
	plan, err := secretexport.MakePlan(ctx, remotes, secretexport.ClusterSecrets{Client: dyn}, prov, o.overwrite)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(run.w, "\nPlan (%s from %s into %s in %s; values are not shown):\n", text.Plural(len(plan.Items), "Secret", "Secrets"), o.context, prov.Name(), o.region)
	for _, it := range plan.Items {
		row := planRow{Status: string(it.Status), Key: it.Key, Secret: it.Namespace + "/" + it.Secret, Binary: it.Binary, Merge: it.Merge, Note: it.Note}
		row.Keys = slices.Sorted(maps.Keys(it.Properties))
		run.Plan = append(run.Plan, row)
		line := fmt.Sprintf("  %-14s %s  (%s, from Secret %s)", it.Status.Label(), row.Key, strings.Join(row.Keys, ", "), row.Secret)
		if row.Merge {
			line += "; merge: a controller writes its other keys"
		}
		if len(row.Binary) > 0 {
			line += "; binary, stored base64: " + strings.Join(row.Binary, ", ")
		}
		if row.Note != "" {
			line += ": " + row.Note
		}
		fmt.Fprintln(run.w, line)
	}
	if bad := planErrors(plan); bad != nil {
		return nil, fmt.Errorf("%w; the redacted folder names Secrets or keys the source cluster lacks: nothing was written", bad)
	}
	return plan, nil
}

// write asks for confirmation (unless --yes) and writes the planned remote
// secrets. err is the run stopping (declined, or no terminal to ask at);
// applyErr a failed write, after which the run goes on to the report.
func (o *secretsExportOptions) write(cmd *cobra.Command, run *secretsRun, plan *secretexport.Plan, prov secretexport.Provider) (applyErr, err error) {
	n := plan.Writes()
	if n == 0 {
		return nil, nil
	}
	if !o.yes {
		interactive := false
		if f, ok := cmd.InOrStdin().(*os.File); ok {
			interactive = term.IsTerminal(int(f.Fd()))
		}
		if !interactive {
			return nil, fmt.Errorf("%s would be written: pass --yes to write them without a prompt", text.Plural(n, "remote secret", "remote secrets"))
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "\nWrite %s to %s in %s, and the ExternalSecrets to %s? [y/N] ", text.Plural(n, "remote secret", "remote secrets"), prov.Name(), o.region, o.out)
		// no answer (EOF) is a no
		answer, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			return nil, errors.New("nothing written")
		}
	}
	if applyErr = plan.Apply(cmd.Context(), prov); applyErr != nil {
		fmt.Fprintf(run.w, "\nA write failed: %v\n", applyErr)
	}
	run.Wrote = n
	return applyErr, nil
}

// writeExternalSecrets writes, for every Secret now in the secret manager,
// the ExternalSecret that reads it, and their store.
func (o *secretsExportOptions) writeExternalSecrets(w io.Writer, run *secretsRun, folder *secretexport.Folder, plan *secretexport.Plan,
	prov secretexport.Provider, manager secretexport.Manager) error {
	x := &kustomize.ExternalSecrets{StoreKind: o.storeKind, StoreName: o.storeName, Provider: prov.Name(), Service: esoService(manager)}
	sources := map[string]secretexport.Source{}
	for _, src := range folder.Secrets {
		sources[src.Namespace+"/"+src.Name] = src
	}
	exists := map[string]bool{}
	for _, c := range run.Report {
		exists[c.Key] = c.Exists
	}
	var out []kustomize.SecretSource
	for _, it := range plan.Items {
		if !exists[it.Key] {
			continue
		}
		src := sources[it.Namespace+"/"+it.Secret]
		out = append(out, kustomize.SecretSource{Namespace: src.Namespace, Name: src.Name, Type: src.Type,
			Labels: src.Labels, Annotations: src.Annotations, Keys: src.Keys, RemoteKey: it.Key, Merge: it.Merge, Binary: it.Binary})
	}
	if o.force {
		// checked before anything was read; checked again, as the directory may have changed since
		if err := replaceableOutput(o.out); err != nil {
			return fmt.Errorf("--external-secrets-out %s: %w", o.out, err)
		}
		if err := os.RemoveAll(o.out); err != nil {
			return err
		}
	}
	if err := kustomize.WriteExternalSecrets(o.out, x, o.region, out); err != nil {
		return fmt.Errorf("writing the ExternalSecrets: %w", err)
	}
	fmt.Fprintf(w, "\nWrote %s and their store to %s. In the new cluster, with External Secrets Operator installed and given access to %s: "+
		"kubectl apply -k %s\n", text.Plural(len(out), "ExternalSecret", "ExternalSecrets"), o.out, prov.Name(), o.out)
	return nil
}

// replaceableOutput checks that --force may replace dir: it is absent,
// empty, or holds only what this command writes (an earlier run's
// ExternalSecrets), and is not the home, root or current directory. --force
// replaces an earlier run's output, never a directory of other files.
func replaceableOutput(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir() // "" when unknown: then nothing matches it
	wd, _ := os.Getwd()
	if filepath.Dir(abs) == abs || abs == filepath.Clean(home) || abs == filepath.Clean(wd) {
		return fmt.Errorf("refusing to replace %s, the root, home or current directory: name a directory of its own", abs)
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var other []string
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == "namespaces" && e.IsDir(),
			!e.IsDir() && (name == "kustomization.yaml" || name == "README.md" ||
				(strings.HasPrefix(name, "secret-store") && strings.HasSuffix(name, ".yaml"))):
		default:
			other = append(other, name)
		}
	}
	if len(other) > 0 {
		return fmt.Errorf("it holds files this command did not write (%s): --force replaces only an earlier run's output; name a new directory", strings.Join(other, ", "))
	}
	return nil
}

// esoService is External Secrets Operator's spec.provider.aws.service for a
// secret manager.
func esoService(m secretexport.Manager) string {
	if m == secretexport.ParameterStoreID {
		return "ParameterStore"
	}
	return "SecretsManager"
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
		return fmt.Errorf("%s of %d do not hold the Secret's values in %s", text.Plural(bad, "remote secret", "remote secrets"), len(run.Report), prov.Name())
	}
	fmt.Fprintf(w, "\nAll %d remote secrets hold their Secret's keys and values.\n", len(run.Report))
	return nil
}

// writeReport writes the run as JSON to --report, if given.
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

// exportOf finds the export a folder is in, by the nearest
// .setdrift-export above it (the cluster's directory), and returns its info.
func exportOf(dir string) *kustomize.ExportInfo {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil
	}
	for d := abs; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, kustomize.ExportInfoFile)); err == nil {
			info, err := kustomize.ReadExportInfo(d)
			if err != nil {
				return nil
			}
			return info
		}
		if filepath.Dir(d) == d {
			return nil
		}
	}
}

// checkSource refuses a cluster other than the export's source, compared by
// kube-system UID: its values would restore the wrong secrets.
func (o *secretsExportOptions) checkSource(ctx context.Context, dyn dynamic.Interface, info *kustomize.ExportInfo) error {
	if o.otherCluster || info.KubeSystemUID == "" {
		return nil
	}
	ns, err := dyn.Resource(namespacesGVR).Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("checking this is the export's source cluster: %w", err)
	}
	if string(ns.GetUID()) != info.KubeSystemUID {
		return fmt.Errorf("context %s is not the cluster the export came from (%s): its Secret values would not be the ones the "+
			"export redacted. Use the source cluster's context, or pass --other-cluster to read from this one anyway", o.context, info.Cluster)
	}
	return nil
}

// secretNamespaces returns the Secrets' namespaces, in order (secrets are
// sorted by namespace).
func secretNamespaces(secrets []secretexport.Source) []string {
	var out []string
	for _, s := range secrets {
		if len(out) == 0 || out[len(out)-1] != s.Namespace {
			out = append(out, s.Namespace)
		}
	}
	return out
}

// outNote says where the ExternalSecrets go, unless --check.
func outNote(o *secretsExportOptions) string {
	if o.check {
		return ""
	}
	return ", written to " + o.out
}

// otherNote marks a run with --other-cluster.
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
		return fmt.Errorf("%s can't be copied: %s", text.Plural(len(bad), "remote secret", "remote secrets"), strings.Join(bad, ", "))
	}
	return nil
}
