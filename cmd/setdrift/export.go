// setdrift export command - reads a cluster or directory, writes it as kustomize repo, converts flags into a pipeline run

package main

import (
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
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"

	"setdrift.io/setdrift-engine/internal/discover"
	"setdrift.io/setdrift-engine/internal/text"
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

// exportOptions holds export's flags: cobra writes them, run reads them.
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
	noSnapshot   bool
	force        bool
	yes          bool
	// export settings
	fullList          bool
	argoInstanceLabel string
	unmanagedOnly     bool
	ciFieldManagers   []string
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

// newExportCmd builds the export command: its help, flags and run.
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
  setdrift export --context prod -n payments -n checkout -o ./out
  setdrift export -o ./out --snapshot ~/snapshots/   # the snapshot elsewhere (default: ./out/snapshots/)
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
	f.StringVar(&o.snapshotPath, "snapshot", "", "file or directory for the protobuf snapshot (default: <output>/snapshots/<time>-<cluster>.pb, kept out of Git)")
	f.StringVar(&o.snapshotJSON, "snapshot-json", "", "file or directory for the snapshot as readable JSON (default: <output>/snapshots/<time>-<cluster>.json)")
	f.BoolVar(&o.noSnapshot, "no-snapshot", false, "write no snapshot files")
	f.BoolVar(&o.force, "force", false, "write into a non-empty output directory, replacing clusters/<cluster>/ in it (asks first)")
	f.BoolVarP(&o.yes, "yes", "y", false, "with --force, replace without asking")
	f.StringVar(&o.argoInstanceLabel, "argocd-instance-label", provenance.DefaultArgoInstanceLabel, "label Argo CD's label tracking uses (application.instanceLabelKey in argocd-cm)")
	f.StringVar(&o.externalSecrets, "external-secrets", "auto", "secret manager the ExternalSecrets written for redacted Secrets read from: "+strings.Join(kustomize.ExternalSecretProviders, ", ")+
		", or none (auto: AWS Secrets Manager on a cluster on AWS, otherwise generic, with store examples for the common secret managers)")
	f.StringVar(&o.secretStore, "secret-store", "", "name of the External Secrets store the ExternalSecrets reference (default "+kustomize.DefaultStoreName+")")
	f.StringVar(&o.secretStoreKind, "secret-store-kind", "ClusterSecretStore", "kind of that store: ClusterSecretStore or SecretStore")
	f.StringVar(&o.secretKeyTemplate, "secret-key-template", "", "remote key of each Secret, a Go template with .Cluster, .Namespace and .Name (default {{.Cluster}}/{{.Namespace}}/{{.Name}}; with a leading / for aws-parameter-store)")
	f.BoolVar(&o.unmanagedOnly, "unmanaged-only", false, "write only resources no GitOps tool, Helm or CI/CD pipeline manages, to adopt into Git (the snapshot keeps everything)")
	f.StringSliceVar(&o.ciFieldManagers, "ci-field-manager", nil, "kubectl field manager of a CI/CD pipeline (kubectl apply --field-manager=NAME): what it wrote counts as deployed from Git, not applied by hand (repeatable)")
	f.BoolVar(&o.fullList, "full-list", false, "list every resource in full instead of listing high-volume types metadata-only first")
	f.BoolVar(&o.keepSourceDefaults, "keep-source-defaults", false, "keep values this cluster's own defaults set that another cluster may lack, such as the storage class a claim got from the default class, so claims ask for the same class by name in any cluster (see RESTORE.md's storage classes)")
	f.StringVar(&o.helmRepos, "helm-repos", "", "YAML file naming the chart repository of Helm releases, which Helm does not record, so RESTORE.md writes complete helm install commands: <chart>: <url> for every release of a chart, or <namespace>/<release>: <url> for one (RESTORE.md lists the charts to fill in)")
	f.BoolVar(&o.aws, "aws", false, "also read the EKS settings held in AWS (Pod Identity, IRSA role trust, access entries, add-ons, EBS volumes) for RESTORE.md, with your AWS credentials; read-only")
	f.StringVar(&o.awsCluster, "aws-cluster", "", "EKS cluster name for --aws (default: from the kubeconfig context)")
	f.StringVar(&o.awsRegion, "aws-region", "", "AWS region for --aws (default: from the kubeconfig context)")
	return cmd
}

// clusterOnlyFlags are the flags about reading a cluster or its cloud
// provider, which --from-dir rejects.
var clusterOnlyFlags = []string{"kubeconfig", "context", "namespace", "full-list", "aws", "aws-cluster", "aws-region"}

// externalSecretsFlags configure the ExternalSecrets, so --external-secrets
// none rejects them.
var externalSecretsFlags = []string{"secret-store", "secret-store-kind", "secret-key-template"}

// now is the clock export stamps snapshots with; tests set it.
var now = time.Now

// run checks the flags, reads the source, and writes the export and its
// snapshots.
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
	// the snapshot files, checked before the read so a bad path fails at once
	at := now().UTC()
	snapPB, snapJSON, err := o.snapshotPaths(cmd, src.cluster.Name, at)
	if err != nil {
		return err
	}

	// Before cluster read, a pipe or test buffer means no prompt
	interactive := false
	if f, ok := cmd.InOrStdin().(*os.File); ok {
		interactive = term.IsTerminal(int(f.Fd()))
	}
	if err := confirmReplace(o.output, src.cluster.Name, o.force, o.yes, interactive, cmd.InOrStdin(), cmd.ErrOrStderr()); err != nil {
		return err
	}

	snap, err := capture(cmd.Context(), src, o.settings(), at)
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
	if err := o.writeSnapshots(snap, snapPB, snapJSON); err != nil {
		return err
	}

	printSummary(cmd.OutOrStdout(), snap, o.output, o.unmanagedOnly, emitter.ExternalSecrets != nil)
	if snapPB != "" || snapJSON != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "Snapshot: %s\n", strings.Join(nonEmpty(snapPB, snapJSON), ", "))
	}
	return nil
}

// snapshotsDir is where export writes its snapshots by default, in the
// export, kept out of Git by the export's .gitignore: each export adds one,
// a history to compare with setdrift diff.
const snapshotsDir = "snapshots"

// snapshotPaths returns where the snapshot files go: by default
// <output>/snapshots/<time>-<cluster>.pb and .json; --snapshot and
// --snapshot-json name a file or a directory for either; --no-snapshot
// writes none. An existing file is replaced only with --force.
func (o *exportOptions) snapshotPaths(cmd *cobra.Command, cluster string, at time.Time) (pb, js string, err error) {
	if o.noSnapshot {
		if o.snapshotPath != "" || o.snapshotJSON != "" {
			return "", "", errors.New("--no-snapshot writes no snapshot, so it cannot be used with --snapshot or --snapshot-json")
		}
		return "", "", nil
	}
	base := at.Format("2006-01-02T150405Z") + "-" + fileName(cluster)
	def := filepath.Join(o.output, snapshotsDir)
	if pb, err = snapshotFile("--snapshot", o.snapshotPath, def, base+".pb", o.force); err != nil {
		return "", "", err
	}
	if js, err = snapshotFile("--snapshot-json", o.snapshotJSON, def, base+".json", o.force); err != nil {
		return "", "", err
	}
	if filepath.Clean(pb) == filepath.Clean(js) {
		return "", "", fmt.Errorf("--snapshot and --snapshot-json both name %s: give them different files", pb)
	}
	if o.snapshotJSON != "" && !strings.EqualFold(filepath.Ext(js), ".json") {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: --snapshot-json %s: the file holds JSON, but its name does not end in .json\n", o.snapshotJSON)
	}
	return pb, js, nil
}

// snapshotFile resolves one snapshot flag: empty means name in def; a
// directory (existing, or ending in /) gets name in it; ~ is the home
// directory, for --snapshot=~/x, which the shell does not expand.
func snapshotFile(flag, arg, def, name string, force bool) (string, error) {
	if arg == "" {
		return filepath.Join(def, name), nil
	}
	if arg == "~" || strings.HasPrefix(arg, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("%s %s: %w", flag, arg, err)
		}
		arg = filepath.Join(home, arg[1:])
	}
	if strings.HasSuffix(arg, "/") || strings.HasSuffix(arg, string(filepath.Separator)) {
		return filepath.Join(arg, name), nil
	}
	fi, err := os.Stat(arg)
	switch {
	case err == nil && fi.IsDir():
		return filepath.Join(arg, name), nil
	case err == nil && !force:
		return "", fmt.Errorf("%s %s: the file exists; pass --force to replace it, or name another file or a directory", flag, arg)
	}
	return arg, nil
}

// writeSnapshots writes the snapshot files that are set, creating their
// directories, and keeps the export's snapshots/ out of Git.
func (o *exportOptions) writeSnapshots(snap *snapshotv1alpha1.Snapshot, pb, js string) error {
	def := filepath.Join(o.output, snapshotsDir)
	for _, f := range []struct {
		path    string
		marshal func(*snapshotv1alpha1.Snapshot) ([]byte, error)
	}{{pb, snapshot.Marshal}, {js, snapshot.MarshalInspectJSON}} {
		if f.path == "" {
			continue
		}
		if filepath.Dir(filepath.Clean(f.path)) == filepath.Clean(def) {
			if err := ignoreSnapshots(o.output); err != nil {
				return err
			}
		}
		b, err := f.marshal(snap)
		if err != nil {
			return err
		}
		// private, like the snapshots in it
		if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
			return err
		}
		if err := writePrivate(f.path, b); err != nil {
			return err
		}
	}
	return nil
}

// gitignoreLine keeps an export's snapshots out of Git: they change with
// every export, and the repository holds what is applied.
const gitignoreLine = "/" + snapshotsDir + "/"

// ignoreSnapshots adds snapshots/ to the .gitignore at the export's root,
// creating it, unless it is there already.
func ignoreSnapshots(out string) error {
	path := filepath.Join(out, ".gitignore")
	old, err := os.ReadFile(path) //#nosec G304 -- the export's own .gitignore
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, l := range strings.Split(string(old), "\n") {
		if strings.TrimSpace(l) == gitignoreLine {
			return nil
		}
	}
	add := "# setdrift export: its snapshots, a local history for setdrift diff, not part of the repository\n" + gitignoreLine + "\n"
	if len(old) > 0 && !strings.HasSuffix(string(old), "\n") {
		add = "\n" + add
	}
	if err := os.MkdirAll(out, 0o755); err != nil { //#nosec G301 -- the export's root, a Git repository
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //#nosec G302,G304 -- the export's .gitignore, committed with it
	if err != nil {
		return err
	}
	if _, err := f.WriteString(add); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

// nonEmpty returns the strings that are not empty.
func nonEmpty(s ...string) []string {
	var out []string
	for _, v := range s {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
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
	return openCluster(cmd.Context(), o.target(), cmd.ErrOrStderr())
}

// target is the cluster the flags name.
func (o *exportOptions) target() clusterTarget {
	return clusterTarget{kubeconfig: o.kubeconfig, context: o.context, clusterName: o.clusterName, namespaces: o.namespaces,
		fullList: o.fullList, aws: o.aws, awsCluster: o.awsCluster, awsRegion: o.awsRegion}
}

// settings are the flags that change how the capture classifies and
// normalizes.
func (o *exportOptions) settings() captureSettings {
	return captureSettings{argoInstanceLabel: o.argoInstanceLabel, ciFieldManagers: o.ciFieldManagers, keepSourceDefaults: o.keepSourceDefaults}
}

// clusterTarget is the cluster to read and how: the cluster flags export
// and diff share.
type clusterTarget struct {
	kubeconfig, context string
	// clusterName names the cluster in the snapshot; empty takes it from the
	// context
	clusterName string
	namespaces  []string
	// fullList lists every type in full, none metadata-only first
	fullList bool
	// aws reads the EKS settings held in AWS too, for the cluster awsCluster
	// in awsRegion (empty: from the context)
	aws                   bool
	awsCluster, awsRegion string
}

// openCluster connects to the cluster t names, failing fast on a bad
// kubeconfig, an unreachable cluster or bad AWS credentials.
func openCluster(ctx context.Context, t clusterTarget, stderr io.Writer) (*source, error) {
	src := &source{plat: &platform.Stage{}, cluster: &snapshotv1alpha1.Cluster{Name: t.clusterName}}
	c, kc, err := t.collector()
	if err != nil {
		return nil, err
	}
	src.collector, src.kc = c, kc
	warnInsecure(stderr, kc)
	applyContext(src.cluster, kc)
	if t.aws {
		// connect to AWS now, so bad credentials or a wrong cluster fail
		// before minutes of cluster reading rather than after
		if src.aws, err = t.connectAWS(ctx, kc); err != nil {
			return nil, err
		}
	}
	// a quick first request, so an unreachable cluster or expired credentials fail now and plainly
	if err := checkReachable(ctx, kc); err != nil {
		return nil, err
	}
	plat := src.plat
	plat.Initial = platform.FromKubeconfig(kc.platform)
	probed, notes := platform.Probe(ctx, c.Discovery, c.Dynamic)
	plat.Initial, plat.Notes = append(plat.Initial, probed...), notes
	if src.aws != nil {
		plat.Initial = append(plat.Initial, platform.Signal{Source: "aws", Detail: "EKS cluster " + src.aws.name + " found with --aws",
			Distribution: platform.EKS, Infrastructure: platform.AWS, Region: src.aws.region, Weight: platform.Strong})
	}
	return src, nil
}

// captureSettings are the flags that change how a capture classifies and
// normalizes what it reads.
type captureSettings struct {
	argoInstanceLabel  string
	ciFieldManagers    []string
	keepSourceDefaults bool
}

// capture reads the source through the pipeline into a snapshot.
func capture(ctx context.Context, src *source, cs captureSettings, at time.Time) (*snapshotv1alpha1.Snapshot, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	p := pipeline.New(src.collector)
	p.Lifecycle = lifecycle.Filter{}
	p.Platform = src.plat
	p.Provenance = provenance.LabelClassifier{ArgoInstanceLabel: cs.argoInstanceLabel, Platform: src.plat, CIFieldManagers: cs.ciFieldManagers}
	p.Normalizer = normalize.Normalizer{ArgoInstanceLabel: cs.argoInstanceLabel, KeepSourceDefaults: cs.keepSourceDefaults}
	snap, err := p.Run(ctx, snapshot.Meta{
		ID:         id,
		CapturedAt: at,
		Cluster:    src.cluster,
		Producer:   &snapshotv1alpha1.Producer{Name: "setdrift-cli", Version: version.Version},
	})
	if err != nil {
		return nil, explainAccess(err)
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

// explainAccess turns the collector's access errors into what to do,
// plainly; other errors pass through.
func explainAccess(err error) error {
	var scopeErr *collect.ScopeError
	var nsErr *collect.NamespaceError
	var noAccess *collect.NoAccessError
	var nothing *collect.ReadNothingError
	switch {
	case errors.As(err, &scopeErr):
		who := "These credentials"
		if scopeErr.Identity != "" {
			who += " (" + scopeErr.Identity + ")"
		}
		return fmt.Errorf("%s may not list namespaces, so SetDrift can't read the cluster as a whole, and the kubeconfig context "+
			"names no namespace to read instead.\n"+
			"Pass the namespaces you can read with -n, e.g. -n <your-team-namespace>; `kubectl auth can-i --list -n <namespace>` shows "+
			"what you can read in one. Or set the context's namespace: kubectl config set-context --current --namespace=<namespace>.\n"+
			"To read the whole cluster, the credentials need get and list on everything: the built-in view ClusterRole bound cluster-wide "+
			"is enough (kubectl create clusterrolebinding setdrift-read --clusterrole=view %s)", who, collect.Subject(scopeErr.Identity))
	case errors.As(err, &nsErr):
		return nsErr
	case errors.As(err, &noAccess):
		return noAccess
	case errors.As(err, &nothing):
		return nothing
	}
	return err
}

// awsReader is an AWS connection checked before the cluster read, for --aws.
type awsReader struct {
	clients      awsinventory.Clients
	name, region string
}

// connectAWS loads AWS credentials and reads the EKS cluster once.
func (t clusterTarget) connectAWS(ctx context.Context, kc *kubeContext) (*awsReader, error) {
	// the EKS name and region, from --aws-cluster and --aws-region or the kubeconfig
	name, region, err := t.awsTarget(kc)
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

// writePrivate writes a snapshot file only its owner can read (0o600),
// replacing an existing one. It writes a new file, mode 0600 from the start, and renames it over path:
// an existing file's looser mode never applies to the new contents, and a
// failed write leaves the old file whole.
func writePrivate(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*") // created 0600
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name()) // best effort: the error that matters is err
		}
	}()
	if _, err := f.Write(data); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// awsTarget is the EKS cluster name and region --aws reads: from the flags
// if set, otherwise what the kubeconfig context shows.
func (t clusterTarget) awsTarget(kc *kubeContext) (name, region string, err error) {
	name, region = t.awsCluster, t.awsRegion
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

// confirmReplace checks the output directory: one that exists and is not
// empty needs --force, and then a yes at the prompt, or --yes when there is
// no terminal to ask at.
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
	// counted for the message only: an unreadable entry is not counted
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files++
		}
		return nil
	})
	var msg string
	if files > 0 {
		msg = fmt.Sprintf("Output directory %s is not empty. %s (%s) will be deleted and written again; everything else in %s is kept.",
			dir, root, text.Plural(files, "file", "files"), dir)
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
	// no answer (EOF) is a no
	answer, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	}
	return errors.New("export cancelled; nothing was written")
}

// emitter converts the flags into the Kustomize writer's settings, checking
// them first.
func (o *exportOptions) emitter(cmd *cobra.Command) (kustomize.Emitter, error) {
	e := kustomize.Emitter{Force: o.force, UnmanagedOnly: o.unmanagedOnly}
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
	if err := kustomize.CheckStore(es.StoreKind, es.StoreName); err != nil {
		return e, fmt.Errorf("--secret-store: %w", err)
	}
	if err := es.Validate(); err != nil {
		return e, fmt.Errorf("--secret-key-template: %w", err)
	}
	e.ExternalSecrets = es
	return e, nil
}

// collector loads the kubeconfig and builds the collector's clients:
// discovery, dynamic, and metadata (unless fullList).
func (t clusterTarget) collector() (*collect.ClusterCollector, *kubeContext, error) {
	kc, err := loadConfig(t.kubeconfig, t.context)
	if err != nil {
		return nil, nil, err
	}
	cfg := kc.config

	// types of resources on cluster
	dc, err := discover.New(cfg)
	if err != nil {
		return nil, nil, err
	}

	// list any type without generated go types
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	collector := &collect.ClusterCollector{Discovery: dc, Dynamic: dyn, Namespaces: t.namespaces, FallbackNamespace: kc.namespace, RequireNamespaces: true}
	if !t.fullList {
		// metadata only for high volume, managed resource types like pods, events
		if collector.Metadata, err = metadata.NewForConfig(cfg); err != nil {
			return nil, nil, err
		}
	}
	return collector, kc, nil
}

// newID returns a random 128-bit snapshot ID.
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
