// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// setdrift secrets export: copies the values of an export's redacted
// Secrets from the source cluster into a secret manager. The only command
// that handles secret values; `export` never does.

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

	"setdrift.io/setdrift-engine/pkg/emit/kustomize"
	"setdrift.io/setdrift-engine/pkg/secretexport"
	"setdrift.io/setdrift-engine/pkg/verify"
)

type secretsExportOptions struct {
	kubeconfig, context string
	secretManager       string
	region, profile     string
	kmsKeyID            string
	overwrite, yes      bool
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
		Use:   "export DIR",
		Short: "Copy an export's redacted Secret values from the source cluster into a secret manager",
		Long: `Export writes an ExternalSecret for each Secret whose values it left out,
naming the remote secret and the properties to read from a secret manager.
This command fills those remote secrets: it reads each Secret's values from
the source cluster and writes them straight into the secret manager, so
External Secrets Operator can restore them in another cluster.

It is the only SetDrift command that reads secret values. They are held in
memory only: never written to disk, to the export or to Git, and never
printed. It shows a plan first and writes only after you confirm (--yes
confirms without a prompt). Remote secrets that exist already are skipped;
--overwrite replaces them.

DIR is the export (or its cluster directory). The context must be the
cluster the export came from: values from another cluster would restore the
wrong secrets. Supported: AWS Secrets Manager and Parameter Store, with your
AWS credentials; the remote secrets are tagged with the source cluster.

Needs get on the exported Secrets in the cluster, and on AWS
secretsmanager:DescribeSecret, CreateSecret and PutSecretValue (or
ssm:GetParameter and PutParameter), plus kms:Encrypt on a --kms-key-id.`,
		Example: `  setdrift secrets export ./prod-export                 # plan, then confirm
  setdrift secrets export ./prod-export --yes --region us-east-2`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return o.run(cmd, args[0]) },
	}
	f := cmd.Flags()
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: KUBECONFIG or ~/.kube/config)")
	f.StringVar(&o.context, "context", "", "kubeconfig context of the source cluster (default: current context)")
	f.StringVar(&o.secretManager, "secret-manager", "", "aws-secrets-manager or aws-parameter-store (default: what the export's ExternalSecrets read from)")
	f.StringVar(&o.region, "region", "", "AWS region (default: the export's, else your AWS configuration's)")
	f.StringVar(&o.profile, "profile", "", "AWS profile (default: AWS_PROFILE or the default profile)")
	f.StringVar(&o.kmsKeyID, "kms-key-id", "", "KMS key to encrypt new secrets with (default: the account's AWS managed key)")
	f.BoolVar(&o.overwrite, "overwrite", false, "replace remote secrets that exist already (they are skipped otherwise)")
	f.BoolVar(&o.yes, "yes", false, "write without asking for confirmation")
	f.BoolVar(&o.otherCluster, "other-cluster", false, "read the values from a cluster other than the export's source (e.g. one restored from it)")
	return cmd
}

func (o *secretsExportOptions) run(cmd *cobra.Command, dir string) error {
	root, err := verify.ClusterRoot(dir)
	if err != nil {
		return err
	}
	info, err := kustomize.ReadExportInfo(root)
	if err != nil {
		return err
	}
	if info == nil {
		info = &kustomize.ExportInfo{}
	}
	remotes, err := secretexport.Load(root)
	if err != nil {
		return err
	}
	if len(remotes) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "The export has no ExternalSecrets: nothing to copy.")
		return nil
	}
	manager := o.secretManager
	if manager == "" {
		manager = info.SecretManager
	}
	if manager != "aws-secrets-manager" && manager != "aws-parameter-store" {
		return fmt.Errorf("the export's ExternalSecrets read from a %s store: pass --secret-manager aws-secrets-manager or "+
			"aws-parameter-store (the secret managers supported so far), matching the store you create for them", orDefault(manager, "generic"))
	}

	kc, err := loadConfig(o.kubeconfig, o.context)
	if err != nil {
		return err
	}
	if err := checkReachable(cmd.Context(), kc); err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(kc.config)
	if err != nil {
		return err
	}
	if err := o.checkSource(cmd.Context(), dyn, info); err != nil {
		return err
	}

	region := orDefault(o.region, info.Region)
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	if o.profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(o.profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(cmd.Context(), opts...)
	if err != nil {
		return fmt.Errorf("AWS configuration: %w", err)
	}
	if cfg.Region == "" {
		return errors.New("no AWS region: pass --region")
	}
	tags := map[string]string{"setdrift.io/source-cluster": orDefault(info.Cluster, kc.name)}
	var prov secretexport.Provider = secretexport.SecretsManager{Client: secretsmanager.NewFromConfig(cfg), KMSKeyID: o.kmsKeyID, Tags: tags}
	if manager == "aws-parameter-store" {
		prov = secretexport.ParameterStore{Client: ssm.NewFromConfig(cfg), KMSKeyID: o.kmsKeyID, Tags: tags}
	}

	plan, err := secretexport.MakePlan(cmd.Context(), remotes, secretexport.ClusterSecrets{Client: dyn}, prov, o.overwrite)
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "Copying Secret values from %s into %s (%s). Values are not shown.\n\n", kc.name, prov.Name(), cfg.Region)
	printPlan(w, plan)
	n := plan.Writes()
	if n == 0 {
		fmt.Fprintln(w, "\nNothing to write.")
		return planErrors(plan)
	}

	interactive := false
	if f, ok := cmd.InOrStdin().(*os.File); ok {
		interactive = term.IsTerminal(int(f.Fd()))
	}
	if !o.yes {
		if !interactive {
			return fmt.Errorf("%s would be written: pass --yes to write them without a prompt", plural(n, "remote secret", "remote secrets"))
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "\nWrite %s to %s in %s? [y/N] ", plural(n, "remote secret", "remote secrets"), prov.Name(), cfg.Region)
		answer, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			return errors.New("nothing written")
		}
	}
	applyErr := plan.Apply(cmd.Context(), root, prov)
	fmt.Fprintln(w)
	printPlan(w, plan)
	if applyErr != nil {
		return fmt.Errorf("some writes failed: %w", applyErr)
	}
	if err := planErrors(plan); err != nil {
		return err
	}
	fmt.Fprintf(w, "\nWrote %s. Create the store (see external-secrets/README.md), then apply external-secrets/ in the new cluster.\n",
		plural(n, "remote secret", "remote secrets"))
	return nil
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
		return fmt.Errorf("this context is not the cluster the export came from (%s): its Secret values would not be the ones the "+
			"export redacted. Use the source cluster's context, or pass --other-cluster to read from this one anyway", info.Cluster)
	}
	return nil
}

func printPlan(w io.Writer, plan *secretexport.Plan) {
	for _, it := range plan.Items {
		props := make([]string, 0, len(it.Properties))
		for p := range it.Properties {
			props = append(props, p)
		}
		sort.Strings(props)
		line := fmt.Sprintf("  %-12s %s  (%s, from Secret %s/%s)", it.Status, it.Key, strings.Join(props, ", "), it.Namespace, it.Secret)
		if len(it.Binary) > 0 {
			line += "; binary, stored base64: " + strings.Join(it.Binary, ", ")
		}
		if it.Note != "" {
			line += ": " + it.Note
		}
		fmt.Fprintln(w, line)
	}
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
		return fmt.Errorf("%s not copied: %s", plural(len(bad), "remote secret", "remote secrets"), strings.Join(bad, ", "))
	}
	return nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
