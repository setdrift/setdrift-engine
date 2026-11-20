// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// setdrift export command - reads a cluster or directory, writes it as kustomize repo, converts flags into a pipeline run

package main

import (
	// standard library packages
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	// cli, k8s packages
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	// aws packages
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	// packages from this repo
	"setdrift.io/setdrift-engine/internal/version"
	"setdrift.io/setdrift-engine/pkg/awsinventory"
	"setdrift.io/setdrift-engine/pkg/collect"
	"setdrift.io/setdrift-engine/pkg/emit/kustomize"
	"setdrift.io/setdrift-engine/pkg/helmsource"
	"setdrift.io/setdrift-engine/pkg/inventory"
	"setdrift.io/setdrift-engine/pkg/lifecycle"
	"setdrift.io/setdrift-engine/pkg/normalize"
	"setdrift.io/setdrift-engine/pkg/pipeline"
	"setdrift.io/setdrift-engine/pkg/platform"
	"setdrift.io/setdrift-engine/pkg/provenance"
	"setdrift.io/setdrift-engine/pkg/snapshot"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// holds every flag, cobra writes it, run reads it
type exportOptions struct {
	// source and output settings
	kubeconfig   string
	context      string
	namespaces   []string
	fromDir      string
	clusterName  string
	output       string
	snapshotPath string
	snapshotJSON string
	force        bool
	yes          bool
	// export settings
	includeFinishedJobs bool
	fullList            bool
	argoInstanceLabel   string
	unmanagedOnly       bool
	groupBy             string
	// secret settings
	externalSecrets   string
	secretStore       string
	secretStoreKind   string
	secretKeyTemplate string
	// aws settings
	aws        bool
	awsCluster string
	awsRegion  string
	// restore settings
	keepSourceDefaults bool
	helmRepos          string
}

// builds export command including help, flags, and what runs when its called, added via main.go
func newExportCmd() *cobra.Command {
	o := &exportOptions{}
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export cluster configuration as a Kustomize repository (read-only)",
		Long: `Export reads a cluster (or a directory of manifests) and writes its
configuration as a Kustomize repository, without changing the cluster.

Use it to evaluate SetDrift and to adopt resources nothing manages into your
existing repositories (see --unmanaged-only). SetDrift does not need you to
keep this repository: it compares the cluster with the repositories you
already have.

Secret values never leave the cluster: Secrets are written with placeholders
under redacted/, outside every kustomization.yaml.`,
		Example: `  setdrift export                      # current context, into ./setdrift-export
  setdrift export -o ./prod-export
  setdrift export --context prod -n payments -n checkout -o ./out --snapshot snap.pb
  setdrift export --from-dir ./manifests --cluster-name test -o ./out
  setdrift export -o ./prod-export --helm-repos ./prod-export/helm-repos.yaml`,
		// RunE not Run so error goes back to main and printed once
		RunE: func(cmd *cobra.Command, _ []string) error { return o.run(cmd) },
	}
	f := cmd.Flags()
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: KUBECONFIG or ~/.kube/config)")
	f.StringVar(&o.context, "context", "", "kubeconfig context (default: current context)")
	f.StringSliceVarP(&o.namespaces, "namespace", "n", nil, "limit to these namespaces; skips cluster-scoped resources (repeatable)")
	f.StringVar(&o.fromDir, "from-dir", "", "read manifests from a directory instead of a cluster (not with the cluster and AWS flags, e.g. -n)")
	f.StringVar(&o.clusterName, "cluster-name", "", "cluster name used in the repo layout (default: derived from the context)")
	f.StringVarP(&o.output, "output", "o", "setdrift-export", "directory to write the repository to")
	f.StringVar(&o.snapshotPath, "snapshot", "", "also write the protobuf snapshot to this file")
	f.StringVar(&o.snapshotJSON, "snapshot-json", "", "also write the snapshot as readable JSON to this file")
	f.BoolVar(&o.force, "force", false, "write into a non-empty output directory, replacing clusters/<cluster>/ in it (asks first)")
	f.BoolVarP(&o.yes, "yes", "y", false, "with --force, replace without asking")
	f.BoolVar(&o.includeFinishedJobs, "include-finished-jobs", false, "export Jobs that have completed or failed")
	f.StringVar(&o.argoInstanceLabel, "argocd-instance-label", provenance.DefaultArgoInstanceLabel, "label Argo CD's label tracking uses (application.instanceLabelKey in argocd-cm)")
	f.StringVar(&o.groupBy, "group-by", string(kustomize.GroupByKind), "directories within each namespace: kind, or app (one directory and kustomization.yaml per Argo CD app, Flux Kustomization, Helm release or app label)")
	f.StringVar(&o.externalSecrets, "external-secrets", "auto", "secret manager the ExternalSecrets written for redacted Secrets read from: "+strings.Join(kustomize.ExternalSecretProviders, ", ")+
		", or none (auto: AWS Secrets Manager on a cluster on AWS, otherwise generic, with store examples for the common secret managers)")
	f.StringVar(&o.secretStore, "secret-store", "", "name of the External Secrets store the ExternalSecrets reference (default "+kustomize.DefaultStoreName+")")
	f.StringVar(&o.secretStoreKind, "secret-store-kind", "ClusterSecretStore", "kind of that store: ClusterSecretStore or SecretStore")
	f.StringVar(&o.secretKeyTemplate, "secret-key-template", "", "remote key of each Secret, a Go template with .Cluster, .Namespace and .Name (default {{.Cluster}}/{{.Namespace}}/{{.Name}}; with a leading / for aws-parameter-store)")
	f.BoolVar(&o.unmanagedOnly, "unmanaged-only", false, "write only resources no GitOps tool or Helm manages, to adopt into Git (the snapshot keeps everything)")
	f.BoolVar(&o.fullList, "full-list", false, "list every resource in full instead of listing high-volume types metadata-only first")
	f.BoolVar(&o.keepSourceDefaults, "keep-source-defaults", false, "keep values this cluster's own defaults set that another cluster may lack, such as the storage class a claim got from the default class, so claims ask for the same class by name in any cluster (see RESTORE.md's storage classes)")
	f.StringVar(&o.helmRepos, "helm-repos", "", "YAML file naming the chart repository of Helm releases, which Helm does not record, so RESTORE.md writes complete helm install commands: <chart>: <url> for every release of a chart, or <namespace>/<release>: <url> for one (RESTORE.md lists the charts to fill in)")
	f.BoolVar(&o.aws, "aws", false, "also read the EKS settings held in AWS (Pod Identity, IRSA role trust, access entries, add-ons, EBS volumes) for RESTORE.md, with your AWS credentials; read-only")
	f.StringVar(&o.awsCluster, "aws-cluster", "", "EKS cluster name for --aws (default: from the kubeconfig context)")
	f.StringVar(&o.awsRegion, "aws-region", "", "AWS region for --aws (default: from the kubeconfig context)")
	return cmd
}

// clusterOnlyFlags are the flags about reading a cluster or its cloud provider, which --from-dir rejects
var clusterOnlyFlags = []string{"kubeconfig", "context", "namespace", "full-list", "aws", "aws-cluster", "aws-region"}

// externalSecretsFlags configure the ExternalSecrets, so --external-secrets none rejects them
var externalSecretsFlags = []string{"secret-store", "secret-store-kind", "secret-key-template"}

// executes export step from input and flags
func (o *exportOptions) run(cmd *cobra.Command) error {
	// flags checked first, so a typo fails before minutes of cluster reading
	emitter, err := o.emitter(cmd)
	if err != nil {
		return err
	}
	src, err := o.open(cmd)
	if err != nil {
		return err
	}
	kc := src.kc

	// Before cluster read, a pipe or test buffer means no prompt
	interactive := false
	if f, ok := cmd.InOrStdin().(*os.File); ok {
		interactive = term.IsTerminal(int(f.Fd()))
	}
	if err := confirmReplace(o.output, src.cluster.Name, o.force, o.yes, interactive, cmd.InOrStdin(), cmd.ErrOrStderr()); err != nil {
		return err
	}

	snap, err := o.capture(cmd.Context(), src)
	if err != nil {
		return err
	}

	if kc != nil {
		emitter.SourceContext = kc.name
		emitter.Region = kc.eksRegion
	}
	if err := emitter.Emit(snap, o.output); err != nil {
		return err
	}
	// release entries that matched no release: likely typos, or releases since removed
	if unmatched := emitter.HelmRepos.Unmatched(); len(unmatched) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: --helm-repos: no Helm release in the export matches %s\n", strings.Join(unmatched, ", "))
	}
	if o.snapshotPath != "" {
		b, err := snapshot.Marshal(snap)
		if err != nil {
			return err
		}
		if err := writePrivate(o.snapshotPath, b); err != nil {
			return err
		}
	}
	if o.snapshotJSON != "" {
		b, err := snapshot.MarshalInspectJSON(snap)
		if err != nil {
			return err
		}
		if err := writePrivate(o.snapshotJSON, b); err != nil {
			return err
		}
	}

	printSummary(cmd, snap, o.output, o.unmanagedOnly, emitter.ExternalSecrets != nil)
	return nil
}

// source is what a snapshot is read from, opened and checked before the
// read: a cluster (kc set) or a directory of manifests.
type source struct {
	collector collect.Collector
	kc        *kubeContext
	aws       *awsReader
	// the platform classifier's evidence before the objects: the kubeconfig, the API server, --aws
	plat    *platform.Stage
	cluster *snapshotv1alpha1.Cluster
}

// open connects to the cluster (or the directory) the flags name, failing
// fast on a bad kubeconfig, an unreachable cluster or bad AWS credentials.
func (o *exportOptions) open(cmd *cobra.Command) (*source, error) {
	src := &source{plat: &platform.Stage{}, cluster: &snapshotv1alpha1.Cluster{Name: o.clusterName}}
	// from directory flag pre-empts other cluster specific flags
	if o.fromDir != "" {
		// these flags only mean something when reading a cluster; reject them (e.g. -n would otherwise export every namespace)
		for _, name := range clusterOnlyFlags {
			if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
				flag := "--" + name
				if f.Shorthand != "" {
					flag += " (-" + f.Shorthand + ")"
				}
				return nil, fmt.Errorf("%s applies to reading a cluster, so it cannot be used with --from-dir", flag)
			}
		}
		src.collector = collect.FileSource{Dir: o.fromDir}
		if src.cluster.Name == "" {
			src.cluster.Name = "local"
		}
		return src, nil
	}
	c, kc, err := o.clusterCollector()
	if err != nil {
		return nil, err
	}
	src.collector, src.kc = c, kc
	applyContext(src.cluster, kc)
	if o.aws {
		// connect to AWS now, so bad credentials or a wrong cluster fail
		// before minutes of cluster reading rather than after
		if src.aws, err = o.connectAWS(cmd.Context(), kc); err != nil {
			return nil, err
		}
	}
	// a quick first request, so an unreachable cluster or expired credentials fail now and plainly
	if err := checkReachable(cmd.Context(), kc); err != nil {
		return nil, err
	}
	plat := src.plat
	plat.Initial = platform.FromKubeconfig(kc.platform)
	probed, notes := platform.Probe(cmd.Context(), c.Discovery, c.Dynamic)
	plat.Initial, plat.Notes = append(plat.Initial, probed...), notes
	if src.aws != nil {
		plat.Initial = append(plat.Initial, platform.Signal{Source: "aws", Detail: "EKS cluster " + src.aws.name + " found with --aws",
			Distribution: platform.EKS, Infrastructure: platform.AWS, Region: src.aws.region, Weight: platform.Strong})
	}
	return src, nil
}

// capture reads the source through the pipeline into a snapshot.
func (o *exportOptions) capture(ctx context.Context, src *source) (*snapshotv1alpha1.Snapshot, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	p := pipeline.New(src.collector)
	p.Lifecycle = lifecycle.Filter{IncludeFinishedJobs: o.includeFinishedJobs}
	p.Platform = src.plat
	p.Provenance = provenance.LabelClassifier{ArgoInstanceLabel: o.argoInstanceLabel, Platform: src.plat}
	p.Normalizer = normalize.Normalizer{ArgoInstanceLabel: o.argoInstanceLabel, KeepSourceDefaults: o.keepSourceDefaults}
	snap, err := p.Run(ctx, snapshot.Meta{
		ID:         id,
		CapturedAt: time.Now().UTC(),
		Cluster:    src.cluster,
		Producer:   &snapshotv1alpha1.Producer{Name: "setdrift-cli", Version: version.Version},
	})
	// only error with its own message, adhoc error messaging
	var scopeErr *collect.ScopeError
	if errors.As(err, &scopeErr) {
		return nil, fmt.Errorf("your access is limited to some namespaces: your credentials can't list resources across the cluster " +
			"(listing namespaces is forbidden), and the kubeconfig context sets no namespace to fall back to.\n" +
			"Pass the namespaces you can read with -n, e.g. -n <your-team-namespace>; `kubectl auth can-i --list -n <namespace>` shows what you can read in one")
	}
	if err != nil {
		return nil, err
	}
	if c, ok := src.collector.(*collect.ClusterCollector); ok {
		// the nodes' shape, for diff; credentials that can't list nodes leave it out
		if nodes, err := inventory.Nodes(ctx, c.Dynamic); err == nil {
			if snap.Inventory == nil {
				snap.Inventory = &snapshotv1alpha1.Inventory{}
			}
			snap.Inventory.Nodes = nodes
		}
	}
	if src.aws != nil {
		// the rest of the AWS read needs the snapshot: its IRSA roles and volumes
		if err := awsinventory.Collect(ctx, src.aws.clients, src.aws.name, src.aws.region, snap); err != nil {
			return nil, fmt.Errorf("--aws: %w", err)
		}
	}
	return snap, nil
}

// awsReader is an AWS connection checked before the cluster read, for --aws
type awsReader struct {
	clients      awsinventory.Clients
	name, region string
}

// connectAWS loads AWS credentials and reads the EKS cluster once
func (o *exportOptions) connectAWS(ctx context.Context, kc *kubeContext) (*awsReader, error) {
	// the EKS name and region, from --aws-cluster and --aws-region or the kubeconfig
	name, region, err := o.awsTarget(kc)
	if err != nil {
		return nil, err
	}
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	// AWS_PROFILE wins, otherwise read AWS as the identity the kubeconfig reads the cluster with
	if kc.awsProfile != "" && os.Getenv("AWS_PROFILE") == "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(kc.awsProfile))
	}
	// the standard AWS credential lookup: environment, ~/.aws config and SSO, instance roles
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("--aws: %w", err)
	}
	// one client each for EKS, IAM and EC2, every call made is a read
	r := &awsReader{
		clients: awsinventory.Clients{EKS: eks.NewFromConfig(cfg), IAM: iam.NewFromConfig(cfg), EC2: ec2.NewFromConfig(cfg)},
		name:    name,
		region:  region,
	}
	if _, err := r.clients.EKS.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: &name}); err != nil {
		return nil, fmt.Errorf("--aws: reading EKS cluster %s in %s: %w", name, region, err)
	}
	return r, nil
}

// writePrivate writes a snapshot file only its owner can read locally (0o600)
func writePrivate(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// awsTarget is the EKS cluster name and region --aws reads from the flags if set, otherwise what the kubeconfig context shows.
func (o *exportOptions) awsTarget(kc *kubeContext) (name, region string, err error) {
	name, region = o.awsCluster, o.awsRegion
	if name == "" {
		name = kc.eksName
	}
	if region == "" {
		region = kc.eksRegion
	}
	if name == "" || region == "" {
		return "", "", errors.New("--aws needs the EKS cluster's name and region, which the kubeconfig context does not show: pass --aws-cluster and --aws-region")
	}
	return name, region, nil
}

// checks output dir, if it already exists and is not empty, requires --force flag and a confirmation if interactive otherwise --yes for pipeline
func confirmReplace(dir, cluster string, force, yes, interactive bool, in io.Reader, out io.Writer) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(entries) == 0) {
		return nil
	}
	if err != nil {
		return err
	}
	root := kustomize.ClusterDir(dir, cluster)
	if !force {
		return fmt.Errorf("output directory %s is not empty; use --force to replace %s in it", dir, root)
	}
	files := 0
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files++
		}
		return nil
	})
	var msg string
	if files > 0 {
		msg = fmt.Sprintf("Output directory %s is not empty. %s (%s) will be deleted and written again; everything else in %s is kept.",
			dir, root, plural(files, "file", "files"), dir)
	} else {
		msg = fmt.Sprintf("Output directory %s is not empty. The export will be written to %s; nothing is deleted.", dir, root)
	}
	if yes {
		fmt.Fprintln(out, msg)
		return nil
	}
	if !interactive {
		return fmt.Errorf("%s\nPass --yes to confirm without a prompt", msg)
	}
	fmt.Fprintf(out, "%s\nContinue? [y/N] ", msg)
	answer, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	}
	return errors.New("export cancelled; nothing was written")
}

// kubeContext is what the CLI takes from the kubeconfig.
type kubeContext struct {
	config *rest.Config
	name   string
	// namespace is the context's namespace; empty if it sets none.
	namespace string
	// EKS cluster name and region and the AWS profile, when the context
	// shows them (an EKS ARN, `aws eks get-token` credentials, or an EKS
	// API server address).
	eksName, eksRegion, awsProfile string
	// what the platform classifier reads from the context (its first pass)
	platform platform.Kubeconfig
}

// loadConfig reads the kubeconfig
func loadConfig(kubeconfig, context string) (*kubeContext, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: context})
	raw, err := cc.RawConfig()
	if err != nil {
		// unparseable YAML, or a file that can't be read
		return nil, explainKubeconfig(err, rules, raw, context)
	}
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, explainKubeconfig(err, rules, raw, context)
	}
	cfg.QPS, cfg.Burst = 50, 100
	cfg.UserAgent = "setdrift/" + version.Version
	// API deprecation warnings (e.g. v1 Endpoints, listed by discovery) are not the user's to act on
	cfg.WarningHandler = rest.NoWarnings{}
	contextName := context
	if contextName == "" {
		contextName = raw.CurrentContext
	}
	kc := &kubeContext{config: cfg, name: contextName, platform: platform.Kubeconfig{Context: contextName, Server: cfg.Host}}
	// Read from the context itself, ClientConfig().Namespace() gives "default" when the context sets none
	if ctx := raw.Contexts[contextName]; ctx != nil {
		kc.namespace = ctx.Namespace
		if user := raw.AuthInfos[ctx.AuthInfo]; user != nil && user.Exec != nil {
			kc.eksName, kc.eksRegion, kc.awsProfile = eksExec(user.Exec.Command, user.Exec.Args, user.Exec.Env)
			kc.platform.ExecCmd, kc.platform.ExecArgs = user.Exec.Command, user.Exec.Args
		}
	}
	if region, name, ok := eksARN(contextName); ok {
		kc.eksRegion, kc.eksName = region, name
	}
	if kc.eksRegion == "" {
		kc.eksRegion = eksServerRegion(cfg.Host)
	}
	return kc, nil
}

// eksExec reads the cluster name, region and profile from aws
func eksExec(command string, args []string, env []clientcmdapi.ExecEnvVar) (name, region, profile string) {
	// aws, or aws.exe on Windows, possibly as a full path
	if strings.TrimSuffix(filepath.Base(command), ".exe") != "aws" {
		return "", "", ""
	}
	for i := 0; i < len(args); i++ {
		// both --region us-east-2 and --region=us-east-2
		flag, value, hasValue := strings.Cut(args[i], "=")
		if !hasValue {
			if i+1 >= len(args) {
				break
			}
			value = args[i+1]
		}
		switch flag {
		case "--cluster-name", "--cluster-id":
			name = value
		case "--region":
			region = value
		case "--profile":
			profile = value
		}
	}
	// AWS_PROFILE only when there's no --profile, which wins in the AWS CLI
	if profile == "" {
		for _, e := range env {
			if e.Name == "AWS_PROFILE" {
				profile = e.Value
			}
		}
	}
	return name, region, profile
}

// eksARN returns the region and name of an EKS cluster ARN
func eksARN(s string) (region, name string, ok bool) {
	parts := strings.SplitN(s, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "eks" || !strings.HasPrefix(parts[5], "cluster/") {
		return "", "", false
	}
	return parts[3], strings.TrimPrefix(parts[5], "cluster/"), true
}

// eksServerRegion returns the region of an EKS API server address
func eksServerRegion(host string) string {
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host, _, _ = strings.Cut(host, ":")
	host, _, _ = strings.Cut(host, "/")
	parts := strings.Split(host, ".")
	if n := len(parts); n >= 4 && strings.HasSuffix(host, ".eks.amazonaws.com") {
		return parts[n-4]
	}
	return ""
}

// emitter converts flags to Kustomize writer's settings
func (o *exportOptions) emitter(cmd *cobra.Command) (kustomize.Emitter, error) {
	e := kustomize.Emitter{Force: o.force, UnmanagedOnly: o.unmanagedOnly, GroupBy: kustomize.GroupBy(o.groupBy)}
	// groupby is picky, only accepts kind or app
	if e.GroupBy != kustomize.GroupByKind && e.GroupBy != kustomize.GroupByApp {
		return e, fmt.Errorf("--group-by must be kind or app, not %q", o.groupBy)
	}
	// read and check the --helm-repos file now, so a bad entry fails before the read
	if o.helmRepos != "" {
		repos, err := helmsource.LoadRepos(o.helmRepos)
		if err != nil {
			return e, fmt.Errorf("--helm-repos: %w", err)
		}
		e.HelmRepos = repos
	}
	if o.externalSecrets == "none" {
		// the store flags only describe ExternalSecrets; reject rather than ignore them
		for _, name := range externalSecretsFlags {
			if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
				return e, fmt.Errorf("--%s configures the ExternalSecrets, so it cannot be used with --external-secrets none", name)
			}
		}
		return e, nil
	}
	es, err := kustomize.DefaultExternalSecrets(o.externalSecrets)
	// --external-secrets none stops the processing, otherwise it checks --secret-store-kind, applies values and validates template
	if err != nil {
		return e, fmt.Errorf("--external-secrets: %w", err)
	}
	if o.secretStoreKind != "ClusterSecretStore" && o.secretStoreKind != "SecretStore" {
		return e, fmt.Errorf("--secret-store-kind must be ClusterSecretStore or SecretStore, not %q", o.secretStoreKind)
	}
	es.StoreKind = o.secretStoreKind
	if o.secretStore != "" {
		es.StoreName = o.secretStore
	}
	if o.secretKeyTemplate != "" {
		es.KeyTemplate = o.secretKeyTemplate
	}
	if err := es.Validate(); err != nil {
		return e, fmt.Errorf("--secret-key-template: %w", err)
	}
	e.ExternalSecrets = es
	return e, nil
}

// builds 3 api clients - discovery, dynamic, and metadata
func (o *exportOptions) clusterCollector() (*collect.ClusterCollector, *kubeContext, error) {
	kc, err := loadConfig(o.kubeconfig, o.context)
	if err != nil {
		return nil, nil, err
	}
	cfg := kc.config

	// types of resources on cluster
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}

	// list any type without generated go types
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	collector := &collect.ClusterCollector{Discovery: dc, Dynamic: dyn, Namespaces: o.namespaces, FallbackNamespace: kc.namespace}
	if !o.fullList {
		// metadata only for high volume, managed resource types like pods, events
		if collector.Metadata, err = metadata.NewForConfig(cfg); err != nil {
			return nil, nil, err
		}
	}
	return collector, kc, nil
}

// applyContext fills in cluster details from the kubeconfig context
func applyContext(c *snapshotv1alpha1.Cluster, kc *kubeContext) {
	contextName := kc.name
	if kc.eksName != "" || kc.eksRegion != "" {
		c.Platform = "eks"
	}
	if _, name, ok := eksARN(contextName); ok {
		c.Platform = "eks"
		c.ProviderId = contextName
		if c.Name == "" {
			c.Name = name
		}
	}
	if c.Name == "" {
		// An aliased EKS context, the credentials name the cluster
		c.Name = kc.eksName
	}
	if c.Name == "" {
		c.Name = contextName
	}
}

// generates random 128 bit snapshot id
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// truncate shortens s to n runes with an ellipsis.
func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// maxSuggestions caps the cleanup suggestions printed, the snapshot has all
const maxSuggestions = 10

// platformCreators names who of the platform created the objects left out, e.g. "kind and kubeadm"
func platformCreators(s *snapshotv1alpha1.Snapshot) string {
	var by []string
	for _, r := range s.Resources {
		if kustomize.Platform(r) {
			if n := kustomize.PlatformName(r.GetProvenance().GetManager()); !slices.Contains(by, n) {
				by = append(by, n)
			}
		}
	}
	sort.Strings(by)
	return strings.Join(by, " and ")
}

// counts resources per source, findings and redactions, what was written, and managed ones if --unmanaged-only
func printSummary(cmd *cobra.Command, s *snapshotv1alpha1.Snapshot, out string, unmanagedOnly, externalSecrets bool) {
	w := cmd.OutOrStdout()
	bySource := map[snapshotv1alpha1.Source]int{}
	findings, redactions, referenceOnly, written, redactedSecrets := len(s.Findings), 0, 0, 0, 0
	needsValues, regenerated := 0, 0
	suggestions := map[string][]string{}
	for _, f := range s.Findings {
		suggestions[f.Code] = append(suggestions[f.Code], f.Message)
	}
	for _, r := range s.Resources {
		bySource[r.GetProvenance().GetSource()]++
		findings += len(r.Findings)
		redactions += len(r.Redactions)
		for _, f := range r.Findings {
			suggestions[f.Code] = append(suggestions[f.Code], fmt.Sprintf("%s %s/%s %s", r.Kind, r.Namespace, r.Name, f.Message))
		}
		if kustomize.Platform(r) || (unmanagedOnly && !kustomize.Unmanaged(r)) {
			continue
		}
		written++
		if kustomize.NeedsValues(r) {
			needsValues++
		} else if kustomize.ReferenceOnly(r) {
			regenerated++
		}
		if kustomize.ReferenceOnly(r) {
			referenceOnly++
			// only Secrets with values to supply get an ExternalSecret: a controller recreates the others
			if r.Kind == "Secret" && r.ApiVersion == "v1" && kustomize.NeedsValues(r) {
				redactedSecrets++
			}
		}
	}

	for _, f := range s.Findings {
		if f.Code == "NAMESPACE_SCOPED_ACCESS" {
			fmt.Fprintf(w, "Your access is limited to some namespaces: %s.\nPass -n to export other namespaces you can read.\n\n", f.Message)
		}
	}
	fmt.Fprintf(w, "Exported %s from %s to %s\n", plural(len(s.Resources), "resource", "resources"), s.Cluster.GetName(), out)
	if c := s.Cluster.GetClassification(); c != nil {
		cls := platform.FromProto(c)
		if dir := cls.Dir(); dir != "" {
			fmt.Fprintf(w, "Platform: %s. The export is generic (any cluster); %s/ is the set for the same platform.\n", cls.Summary(), dir)
		} else if cls.Distribution != platform.Unknown || cls.Infrastructure != platform.UnknownInfra {
			fmt.Fprintf(w, "Platform: %s, not certain enough for platform-specific output; the export is generic.\n", cls.Summary())
		}
	}
	for src := snapshotv1alpha1.Source_SOURCE_UNMANAGED; src <= snapshotv1alpha1.Source_SOURCE_PLATFORM; src++ {
		if n := bySource[src]; n > 0 {
			note := ""
			if src == snapshotv1alpha1.Source_SOURCE_PLATFORM {
				note = "  (created by " + platformCreators(s) + "; in the snapshot, not written)"
			}
			fmt.Fprintf(w, "  %-10s %d%s\n", strings.ToLower(strings.TrimPrefix(src.String(), "SOURCE_")), n, note)
		}
	}
	if unmanagedOnly {
		fmt.Fprintf(w, "Wrote the %s nothing manages (--unmanaged-only); the snapshot has all of them.\n", plural(written, "resource", "resources"))
	}
	ov := kustomize.NewOverview(s, unmanagedOnly)
	if n := ov.ByHandCount(); n > 0 {
		fmt.Fprintf(w, "\nApplied by hand, so this export is their only record (no GitOps tool, Helm or operator manages them): %d\n", n)
		for i, ns := range ov.Namespaces() {
			if i == maxSuggestions {
				fmt.Fprintf(w, "  ... and %d more namespaces (see README.md)\n", len(ov.ByHand)-maxSuggestions)
				break
			}
			fmt.Fprintf(w, "  %-20s %3d  %s\n", orDefault(ns, "(cluster-scoped)"), len(ov.ByHand[ns]), truncate(kustomize.Grouped(ov.ByHand[ns]), 90))
		}
	}
	if len(ov.HeldBack) > 0 {
		fmt.Fprintf(w, "\nHeld back from the kustomization, secret values in the manifest (move them into Secrets):\n")
		for _, h := range ov.HeldBack {
			fmt.Fprintf(w, "  %s: %s\n", h.Resource, strings.Join(h.Fields, ", "))
		}
	}
	fmt.Fprintln(w)
	if referenceOnly > 0 {
		fmt.Fprintf(w, "%s with redacted values %s written to redacted/ for reference only.\n"+
			"They are in no kustomization.yaml: applying them would overwrite the real values. See redacted/README.md.\n",
			plural(referenceOnly, "resource", "resources"), verb(referenceOnly))
		if externalSecrets && redactedSecrets > 0 {
			fmt.Fprintf(w, "%s for the redacted Secrets %s in external-secrets/, not applied until the values are in the secret manager (see its README).\n",
				plural(redactedSecrets, "ExternalSecret", "ExternalSecrets"), map[bool]string{true: "is", false: "are"}[redactedSecrets == 1])
		}
	}

	fmt.Fprintf(w, "\nTo recreate this cluster elsewhere, follow %s", filepath.Join(kustomize.ClusterDir(out, s.Cluster.GetName()), "RESTORE.md"))
	if todo := restoreTodo(s, needsValues, regenerated); len(todo) > 0 {
		fmt.Fprintf(w, ": %s", strings.Join(todo, ", "))
	}
	fmt.Fprintln(w, ".")
	fmt.Fprintf(w, "Overview: %s (%s, %s; every finding is in --snapshot-json).\n",
		filepath.Join(kustomize.ClusterDir(out, s.Cluster.GetName()), "README.md"), plural(redactions, "redaction", "redactions"), plural(findings, "finding", "findings"))

	if lines := ov.Cleanup; len(lines) > 0 {
		fmt.Fprintf(w, "\nCleanup suggestions (SetDrift changes nothing in the cluster):\n")
		for i, l := range lines {
			if i == maxSuggestions {
				fmt.Fprintf(w, "  ... and %d more in the snapshot\n", len(lines)-maxSuggestions)
				break
			}
			fmt.Fprintf(w, "  - %s\n", l)
		}
	}
}

// restoreTodo summarizes what RESTORE.md asks of the user
func restoreTodo(s *snapshotv1alpha1.Snapshot, needsValues, regenerated int) []string {
	var todo []string
	if needsValues > 0 {
		todo = append(todo, plural(needsValues, "resource", "resources")+" to supply secret values for")
	}
	if regenerated > 0 {
		todo = append(todo, plural(regenerated, "Secret", "Secrets")+" their controllers create again (nothing to do)")
	}
	if n := len(s.GetInventory().GetVolumes()); n > 0 {
		todo = append(todo, plural(n, "volume", "volumes")+" to bring over")
	}
	crds, irsa := 0, 0
	releases := map[string]bool{}
	for _, r := range s.Resources {
		if kustomize.Platform(r) {
			continue
		}
		switch {
		case r.Kind == "CustomResourceDefinition":
			crds++
		case r.Kind == "ServiceAccount" && strings.Contains(r.ManifestJson, `"eks.amazonaws.com/role-arn"`):
			irsa++
		}
		if p := r.GetProvenance(); p.GetSource() == snapshotv1alpha1.Source_SOURCE_HELM && p.GetManager() != "" && p.GetEvidence()["confidence"] != "low" {
			releases[p.GetManager()] = true
		}
	}
	if crds > 0 {
		todo = append(todo, plural(crds, "CRD", "CRDs")+" to create first")
	}
	if len(releases) > 0 {
		todo = append(todo, plural(len(releases), "Helm release", "Helm releases")+" to install")
	}
	if irsa > 0 {
		todo = append(todo, plural(irsa, "IRSA service account", "IRSA service accounts")+" whose roles must trust the new cluster")
	}
	if n := len(s.GetInventory().GetAws().GetPodIdentityAssociations()); n > 0 {
		todo = append(todo, plural(n, "Pod Identity association", "Pod Identity associations")+" to create")
	}
	if n := len(s.GetInventory().GetLoadBalancers()); n > 0 {
		todo = append(todo, plural(n, "load balancer address", "load balancer addresses")+" that will change")
	}
	return todo
}

// helper
func plural(n int, singular, pl string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, pl)
}

// helper
func verb(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}
