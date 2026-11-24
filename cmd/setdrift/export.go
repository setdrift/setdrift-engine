// exports a cluster, or a directory of manifests, as a kustomize repository without changing the cluster
// the reading and writing live in pkg/pipeline and pkg/emit/kustomize, this file turns the flags into a run of them and writes the snapshot files

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
	"setdrift.io/setdrift-engine/pkg/profile"
	"setdrift.io/setdrift-engine/pkg/provenance"
	"setdrift.io/setdrift-engine/pkg/snapshot"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// exportOptions holds the flags export was given, cobra fills them in and run and the helpers below read them
type exportOptions struct {
	// where to read from and where to write to
	kubeconfig string
	context    string
	// limits the export to these namespaces, cluster-scoped resources are skipped then
	namespaces []string
	// a directory of manifests to read instead of a cluster
	fromDir string
	// the cluster's name in the repository layout, taken from the context when its empty
	clusterName string
	output      string
	// the file or directory for the protobuf snapshot, <output>/snapshots/ when its empty
	snapshotPath string
	// the file or directory for the readable json snapshot, <output>/snapshots/ when its empty
	snapshotJSON string
	noSnapshot   bool
	// allows writing into a non-empty output directory, replacing clusters/<cluster>/ in it
	force bool
	// with force, replaces without asking
	yes bool
	// how the export reads and classifies
	// lists every type in full instead of listing high-volume types metadata-only first
	fullList bool
	// the label Argo CD tracks its resources with, application.instanceLabelKey in argocd-cm
	argoInstanceLabel string
	// writes only the resources nothing manages, the snapshot still keeps everything
	unmanagedOnly bool
	// how redacted secrets are written as ExternalSecrets
	// the secret manager the ExternalSecrets read from, auto, none or one of kustomize.ExternalSecretProviders
	externalSecrets   string
	secretStore       string
	secretStoreKind   string
	secretKeyTemplate string
	// reading the EKS settings held in AWS
	aws bool
	// the EKS cluster name and region, taken from the kubeconfig context when theyre empty
	awsCluster string
	awsRegion  string
	// what RESTORE.md and the restored resources get
	// keeps values this cluster's own defaults set that another cluster may lack, like the storage class a claim got from the default class
	keepSourceDefaults bool
	// a yaml file naming the chart repository of helm releases, which helm doesnt record
	helmRepos string
}

// newExportCmd builds the export command with its help text and flags, main.go adds it to the command tree
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
		// invoking run, its defined below
		// its RunE and not Run so the error goes back to main and gets printed once
		RunE: func(cmd *cobra.Command, _ []string) error { return o.run(cmd) },
	}
	// registering the flags, each one fills a field of exportOptions above
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
	f.StringVar(&o.argoInstanceLabel, "argocd-instance-label", provenance.DefaultArgoInstanceLabel, "custom label Argo CD's label tracking uses (application.instanceLabelKey in argocd-cm); the default app.kubernetes.io/instance is never read, since every Helm chart sets it")
	f.StringVar(&o.externalSecrets, "external-secrets", "auto", "secret manager the ExternalSecrets written for redacted Secrets read from: "+strings.Join(kustomize.ExternalSecretProviders, ", ")+
		", or none (auto: AWS Secrets Manager on a cluster on AWS, otherwise generic, with store examples for the common secret managers)")
	f.StringVar(&o.secretStore, "secret-store", "", "name of the External Secrets store the ExternalSecrets reference (default "+kustomize.DefaultStoreName+")")
	f.StringVar(&o.secretStoreKind, "secret-store-kind", "ClusterSecretStore", "kind of that store: ClusterSecretStore or SecretStore")
	f.StringVar(&o.secretKeyTemplate, "secret-key-template", "", "remote key of each Secret, a Go template with .Cluster, .Namespace and .Name (default {{.Cluster}}/{{.Namespace}}/{{.Name}}; with a leading / for aws-parameter-store)")
	f.BoolVar(&o.unmanagedOnly, "unmanaged-only", false, "write only resources no GitOps tool, Helm or CI/CD pipeline manages, to adopt into Git (the snapshot keeps everything)")
	f.BoolVar(&o.fullList, "full-list", false, "list every resource in full instead of listing high-volume types metadata-only first")
	f.BoolVar(&o.keepSourceDefaults, "keep-source-defaults", false, "keep values this cluster's own defaults set that another cluster may lack, such as the storage class a claim got from the default class, so claims ask for the same class by name in any cluster (see RESTORE.md's storage classes)")
	f.StringVar(&o.helmRepos, "helm-repos", "", "YAML file naming the chart repository of Helm releases, which Helm does not record, so RESTORE.md writes complete helm install commands: <chart>: <url> for every release of a chart, or <namespace>/<release>: <url> for one (RESTORE.md lists the charts to fill in)")
	f.BoolVar(&o.aws, "aws", false, "also read the EKS settings held in AWS (Pod Identity, IRSA role trust, access entries, add-ons, EBS volumes) for RESTORE.md, with your AWS credentials; read-only")
	f.StringVar(&o.awsCluster, "aws-cluster", "", "EKS cluster name for --aws (default: from the kubeconfig context)")
	f.StringVar(&o.awsRegion, "aws-region", "", "AWS region for --aws (default: from the kubeconfig context)")
	// invoking belongsUnder and cannotUseWith to declare how the flags relate, checkFlags enforces it before the command runs, both defined in flagrules.go
	belongsUnder(f, "aws-cluster", "aws", true)
	belongsUnder(f, "aws-region", "aws", true)
	belongsUnder(f, "yes", "force", true)
	// going through the ExternalSecrets settings, each one belongs under --external-secrets, which is on by default so they dont need it given
	for _, name := range externalSecretsFlags {
		belongsUnder(f, name, "external-secrets", false)
	}
	cannotUseWith(f, "writes no snapshot", "no-snapshot", "snapshot", "snapshot-json")
	// going through the flags about reading a cluster, none of them means anything for a directory
	for _, name := range clusterOnlyFlags {
		cannotUseWith(f, "applies to reading a cluster", name, "from-dir")
	}
	return cmd
}

// clusterOnlyFlags are the flags about reading a cluster or its cloud provider, --from-dir rejects them
var clusterOnlyFlags = []string{"kubeconfig", "context", "namespace", "full-list", "aws", "aws-cluster", "aws-region"}

// externalSecretsFlags are the flags that configure the ExternalSecrets, so --external-secrets none rejects them
var externalSecretsFlags = []string{"secret-store", "secret-store-kind", "secret-key-template"}

// now is the clock export stamps snapshots with, tests swap it out with setClock in cmd_test.go
var now = time.Now

// run checks the flags, reads the source, and writes the export and its snapshots
// everything that can be checked up front is checked before the read, so a typo doesnt fail after minutes of cluster reading
func (o *exportOptions) run(cmd *cobra.Command) error {
	// invoking emitter to check the output flags and set up the kustomize writer, its defined below
	emitter, err := o.emitter(cmd)
	if err != nil {
		return err
	}
	// invoking open to connect to the cluster or the directory the flags name, its defined below
	src, err := o.open(cmd)
	if err != nil {
		return err
	}
	kc := src.kc
	at := now().UTC()
	// invoking snapshotPaths to work out the snapshot files, before the read so a bad path fails at once, its defined below
	snapPB, snapJSON, err := o.snapshotPaths(cmd, src.cluster.Name, at)
	if err != nil {
		return err
	}

	interactive := false
	// checking if stdin is a terminal, a pipe or a test buffer means theres nobody to ask at a prompt
	if f, ok := cmd.InOrStdin().(*os.File); ok {
		interactive = term.IsTerminal(int(f.Fd()))
	}
	// invoking otherCluster to tell if the cluster's directory holds the export of a different cluster with the same name, its defined below
	other := otherCluster(cmd.Context(), o.output, src)
	// invoking confirmReplace to make sure a non-empty output directory may be written into, still before the read, its defined below
	if err := confirmReplace(o.output, src.cluster.Name, other, o.force, o.yes, interactive, cmd.InOrStdin(), cmd.ErrOrStderr()); err != nil {
		return err
	}

	// invoking capture to read the source into a snapshot with the settings from settings, both defined below
	snap, err := capture(cmd.Context(), src, o.settings(), at)
	if err != nil {
		return err
	}

	// checking if a cluster was read, if so the export records its context and region
	if kc != nil {
		emitter.SourceContext = kc.name
		emitter.Region = kc.eksRegion
	}
	// invoking Emit to write the snapshot as the kustomize repository, its defined in pkg/emit/kustomize/kustomize.go
	if err := emitter.Emit(snap, o.output); err != nil {
		return err
	}
	// invoking HelmRepos.Unmatched and checking if any release entries matched no release, if so they get a warning, its defined in pkg/helmsource/repos.go
	// those are likely typos, or releases removed since
	if unmatched := emitter.HelmRepos.Unmatched(); len(unmatched) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: --helm-repos: no Helm release in the export matches %s\n", strings.Join(unmatched, ", "))
	}
	// invoking writeSnapshots to write the snapshot files, its defined below
	if err := o.writeSnapshots(snap, snapPB, snapJSON); err != nil {
		return err
	}

	// invoking printSummary to print what was exported, its defined in summary.go
	printSummary(cmd.OutOrStdout(), snap, o.output, o.unmanagedOnly, emitter.ExternalSecrets != nil)
	// checking if any snapshot file was written, if so invoking nonEmpty to list the ones that were, its defined below
	if snapPB != "" || snapJSON != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "Snapshot: %s\n", strings.Join(nonEmpty(snapPB, snapJSON), ", "))
	}
	return nil
}

// snapshotsDir is the folder in the export where snapshots go by default, kept out of git by the export's .gitignore
// each export adds one, so they build up a history to compare with setdrift diff
const snapshotsDir = "snapshots"

// snapshotPaths takes the cluster name and the export time and gives back where the snapshot files go, empty for none
// by default thats <output>/snapshots/<time>-<cluster>.pb and .json, --snapshot and --snapshot-json name a file or a directory for either, --no-snapshot writes none
// an existing file is replaced only with --force
func (o *exportOptions) snapshotPaths(cmd *cobra.Command, cluster string, at time.Time) (pb, js string, err error) {
	// checking if --no-snapshot was given, if so no paths are needed, checkFlags in flagrules.go already refused it with a snapshot path
	if o.noSnapshot {
		return "", "", nil
	}
	// invoking fileName to make the cluster name safe for a file name, its defined in verifyreport.go
	base := at.Format("2006-01-02T150405Z") + "-" + fileName(cluster)
	def := filepath.Join(o.output, snapshotsDir)
	// invoking snapshotFile to resolve each of the two flags, its defined below
	if pb, err = snapshotFile("--snapshot", o.snapshotPath, def, base+".pb", o.force); err != nil {
		return "", "", err
	}
	// invoking snapshotFile again for the json copy of the snapshot, its defined below
	if js, err = snapshotFile("--snapshot-json", o.snapshotJSON, def, base+".json", o.force); err != nil {
		return "", "", err
	}
	// checking if both flags ended up naming the same file, one would overwrite the other
	if filepath.Clean(pb) == filepath.Clean(js) {
		return "", "", fmt.Errorf("--snapshot and --snapshot-json both name %s: give them different files", pb)
	}
	// checking if a json file was named without a .json extension, thats allowed but likely a mistake, so it gets a warning
	if o.snapshotJSON != "" && !strings.EqualFold(filepath.Ext(js), ".json") {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: --snapshot-json %s: the file holds JSON, but its name does not end in .json\n", o.snapshotJSON)
	}
	return pb, js, nil
}

// snapshotFile takes one snapshot flag's value and gives back the file it names
// empty means name in def, a directory (existing, or ending in /) gets name in it, and ~ is the home directory, for --snapshot=~/x which the shell doesnt expand
func snapshotFile(flag, arg, def, name string, force bool) (string, error) {
	// checking if the flag was left out, if so the file goes in the default directory
	if arg == "" {
		return filepath.Join(def, name), nil
	}
	// a path ending in a separator names a directory even when it doesnt exist yet, noted before ~ is expanded since filepath.Join drops the separator
	dir := strings.HasSuffix(arg, "/") || strings.HasSuffix(arg, string(filepath.Separator))
	// checking if the path starts with ~, if so we expand it to the home directory ourselves
	if arg == "~" || strings.HasPrefix(arg, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("%s %s: %w", flag, arg, err)
		}
		arg = filepath.Join(home, arg[1:])
	}
	// checking if the path named a directory, if so the file goes in it
	if dir {
		return filepath.Join(arg, name), nil
	}
	fi, err := os.Stat(arg)
	// checking what is at the path, an existing directory gets the file in it, an existing file needs --force, anything else is used as the file name
	switch {
	case err == nil && fi.IsDir():
		return filepath.Join(arg, name), nil
	case err == nil && !force:
		return "", fmt.Errorf("%s %s: the file exists; pass --force to replace it, or name another file or a directory", flag, arg)
	}
	return arg, nil
}

// writeSnapshots writes the snapshot files that are set, creating their directories, and keeps the export's snapshots/ out of git
func (o *exportOptions) writeSnapshots(snap *snapshotv1alpha1.Snapshot, pb, js string) error {
	def := filepath.Join(o.output, snapshotsDir)
	// going over the protobuf and the json file, each with the snapshot.Marshal function that encodes it, both defined in pkg/snapshot/snapshot.go
	for _, f := range []struct {
		path    string
		marshal func(*snapshotv1alpha1.Snapshot) ([]byte, error)
	}{{pb, snapshot.Marshal}, {js, snapshot.MarshalInspectJSON}} {
		// checking if this file is turned off, if so theres nothing to write
		if f.path == "" {
			continue
		}
		// checking if the file goes in the export's own snapshots/, if so invoking ignoreSnapshots to keep that out of git, its defined below
		if filepath.Dir(filepath.Clean(f.path)) == filepath.Clean(def) {
			if err := ignoreSnapshots(o.output); err != nil {
				return err
			}
		}
		// encoding the snapshot in this file's format
		b, err := f.marshal(snap)
		if err != nil {
			return err
		}
		// creating the directory private, like the snapshots in it
		if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
			return err
		}
		// invoking writePrivate to write the file only its owner can read, its defined below
		if err := writePrivate(f.path, b); err != nil {
			return err
		}
	}
	return nil
}

// gitignoreLine is the .gitignore line that keeps an export's snapshots out of git, they change with every export and the repository holds only what is applied
const gitignoreLine = "/" + snapshotsDir + "/"

// ignoreSnapshots takes the export's root and adds snapshots/ to the .gitignore there, creating the file if needed, unless the line is there already
func ignoreSnapshots(out string) error {
	path := filepath.Join(out, ".gitignore")
	// reading the current .gitignore, a missing one is fine and reads as empty
	old, err := os.ReadFile(path) //#nosec G304 -- the export's own .gitignore
	// checking if reading it failed for any reason but it not existing, if so thats a terminal error
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// looking for the snapshots line in it
	for _, l := range strings.Split(string(old), "\n") {
		// checking if this is the line, if so theres nothing to add
		if strings.TrimSpace(l) == gitignoreLine {
			return nil
		}
	}
	add := "# setdrift export: its snapshots, a local history for setdrift diff, not part of the repository\n" + gitignoreLine + "\n"
	// checking if the file doesnt end in a newline, if so one goes first so the line stays on its own
	if len(old) > 0 && !strings.HasSuffix(string(old), "\n") {
		add = "\n" + add
	}
	// creating the output directory first, so the .gitignore can be written into it
	if err := os.MkdirAll(out, 0o755); err != nil { //#nosec G301 -- the export's root, a Git repository
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //#nosec G302,G304 -- the export's .gitignore, committed with it
	if err != nil {
		return err
	}
	// checking if the write failed, if so the file still gets closed and both errors come back
	if _, err := f.WriteString(add); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

// nonEmpty takes some strings and gives back the ones that arent empty, run uses it to list the snapshot files written
func nonEmpty(s ...string) []string {
	var out []string
	// going over each string
	for _, v := range s {
		// checking if its empty, if not it goes in the result
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// source is what a snapshot is read from, a cluster (kc set) or a directory of manifests, opened and checked before the read
// export and diff both build one
type source struct {
	// reads the resources, from the cluster or the directory
	collector collect.Collector
	// the kubeconfig context, nil when reading a directory
	kc *kubeContext
	// the AWS connection, nil unless --aws was given
	aws *awsReader
	// the platform classifier's evidence gathered before the objects are read, from the kubeconfig, the API server and --aws
	plat *platform.Stage
	// the cluster as the snapshot records it, starting with its name
	cluster *snapshotv1alpha1.Cluster
}

// open connects to the cluster or the directory the flags name, failing fast on a bad kubeconfig, an unreachable cluster or bad AWS credentials
func (o *exportOptions) open(cmd *cobra.Command) (*source, error) {
	src := &source{plat: &platform.Stage{}, cluster: &snapshotv1alpha1.Cluster{Name: o.clusterName}}
	// checking if --from-dir was given, if so a directory is read and no cluster is contacted
	if o.fromDir != "" {
		// collect.FileSource reads the manifests from the directory, its defined in pkg/collect/files.go
		src.collector = collect.FileSource{Dir: o.fromDir}
		// checking if --cluster-name was left out, if so the cluster is called local
		if src.cluster.Name == "" {
			src.cluster.Name = "local"
		}
		return src, nil
	}
	// invoking openCluster with the cluster target to connect, both defined below
	return openCluster(cmd.Context(), o.target(), cmd.ErrOrStderr())
}

// target gives back the cluster the flags name and how to read it, for openCluster below
func (o *exportOptions) target() clusterTarget {
	return clusterTarget{kubeconfig: o.kubeconfig, context: o.context, clusterName: o.clusterName, namespaces: o.namespaces,
		fullList: o.fullList, aws: o.aws, awsCluster: o.awsCluster, awsRegion: o.awsRegion}
}

// settings gives back the flags that change how the capture classifies and normalizes, for capture below
func (o *exportOptions) settings() captureSettings {
	return captureSettings{argoInstanceLabel: o.argoInstanceLabel, keepSourceDefaults: o.keepSourceDefaults}
}

// clusterTarget is the cluster to read and how, the cluster flags export and diff share
type clusterTarget struct {
	kubeconfig, context string
	// clusterName names the cluster in the snapshot, when its empty the name comes from the context
	clusterName string
	// limits the read to these namespaces
	namespaces []string
	// fullList lists every type in full, none metadata-only first
	fullList bool
	// aws reads the EKS settings held in AWS too, for the cluster awsCluster in awsRegion
	aws bool
	// when these are empty they come from the context
	awsCluster, awsRegion string
}

// openCluster connects to the cluster t names and gathers the first platform evidence, failing fast on a bad kubeconfig, an unreachable cluster or bad AWS credentials
// export uses it through open above, diff uses it directly
func openCluster(ctx context.Context, t clusterTarget, stderr io.Writer) (*source, error) {
	src := &source{plat: &platform.Stage{}, cluster: &snapshotv1alpha1.Cluster{Name: t.clusterName}}
	// invoking collector to load the kubeconfig and build the clients, its defined below
	c, kc, err := t.collector()
	if err != nil {
		return nil, err
	}
	src.collector, src.kc = c, kc
	// invoking warnInsecure to warn when the context skips the certificate check, its defined in kubeconfig.go
	warnInsecure(stderr, kc)
	// invoking applyContext to fill in the cluster's name and details from the context, its defined in kubeconfig.go
	applyContext(src.cluster, kc)
	// checking if --aws was given, if so we connect to AWS now
	// that way bad credentials or a wrong cluster fail before minutes of cluster reading rather than after
	if t.aws {
		// invoking connectAWS to load the credentials and find the EKS cluster, its defined below
		if src.aws, err = t.connectAWS(ctx, kc); err != nil {
			return nil, err
		}
	}
	// invoking checkReachable to make one quick request, so an unreachable cluster or expired credentials fail right away with a clear error, its defined in kubeconfig.go
	if err := checkReachable(ctx, kc); err != nil {
		return nil, err
	}
	plat := src.plat
	// invoking platform.FromKubeconfig to read the hints the kubeconfig gives, its defined in pkg/platform/signals.go
	plat.Initial = platform.FromKubeconfig(kc.platform)
	// invoking platform.Probe to ask the cluster itself, its defined in pkg/platform/probe.go
	probed, notes := platform.Probe(ctx, c.Discovery, c.Dynamic)
	plat.Initial, plat.Notes = append(plat.Initial, probed...), notes
	// checking if AWS found the EKS cluster, if so thats strong evidence the cluster is EKS on AWS
	if src.aws != nil {
		plat.Initial = append(plat.Initial, platform.Signal{Source: "aws", Detail: "EKS cluster " + src.aws.name + " found with --aws",
			Distribution: platform.EKS, Infrastructure: platform.AWS, Region: src.aws.region, Weight: platform.Strong})
	}
	return src, nil
}

// captureSettings are the flags that change how a capture classifies and normalizes what it reads, export and diff both pass them to capture
type captureSettings struct {
	// the label Argo CD tracks its resources with
	argoInstanceLabel string
	// keeps values the source cluster's own defaults set
	keepSourceDefaults bool
}

// capture takes an opened source and reads it through the pipeline into a snapshot stamped with at, adding the node shape and the AWS settings when it can
// export and diff both read their source through it
func capture(ctx context.Context, src *source, cs captureSettings, at time.Time) (*snapshotv1alpha1.Snapshot, error) {
	// invoking newID for the snapshot's random id, its defined below
	id, err := newID()
	if err != nil {
		return nil, err
	}
	// invoking pipeline.New to set up the pipeline around the collector, its defined in pkg/pipeline/pipeline.go
	// then plugging in the stages, the lifecycle filter, the platform classifier, the provenance classifier and the normalizer
	p := pipeline.New(src.collector)
	p.Lifecycle = lifecycle.Filter{}
	p.Platform = src.plat
	p.Provenance = provenance.LabelClassifier{ArgoInstanceLabel: cs.argoInstanceLabel, Platform: src.plat}
	p.Normalizer = normalize.Normalizer{ArgoInstanceLabel: cs.argoInstanceLabel, KeepSourceDefaults: cs.keepSourceDefaults}
	// invoking Run to read and process everything into the snapshot, its defined in pkg/pipeline/pipeline.go
	snap, err := p.Run(ctx, snapshot.Meta{
		ID:         id,
		CapturedAt: at,
		Cluster:    src.cluster,
		Producer:   &snapshotv1alpha1.Producer{Name: "setdrift-cli", Version: version.Version},
	})
	// checking if the read failed, if so invoking explainAccess to turn an access error into what to do about it, its defined below
	if err != nil {
		return nil, explainAccess(err)
	}
	// checking if a cluster was read, if so the snapshot also gets the nodes' shape, for diff
	if c, ok := src.collector.(*collect.ClusterCollector); ok {
		// invoking inventory.ListNodes and checking if it worked, credentials that cant list nodes leave it out, its defined in pkg/inventory/nodes.go
		if nodes, err := inventory.ListNodes(ctx, c.Dynamic); err == nil {
			// checking if the snapshot has no inventory yet, if so it gets one
			if snap.Inventory == nil {
				snap.Inventory = &snapshotv1alpha1.Inventory{}
			}
			snap.Inventory.Nodes = inventory.SummarizeNodes(nodes)
			// invoking profile.AddNodes to add their zones, address ranges, pod ranges and cloud account to the profile, its defined in pkg/profile/nodes.go
			snap.Inventory.Profile = profile.AddNodes(snap.Inventory.Profile, nodes)
		}
	}
	// checking if --aws was given, if so the rest of the AWS read happens now since it needs the snapshot, its IRSA roles and volumes
	if src.aws != nil {
		// invoking awsinventory.Collect to read the EKS settings held in AWS into the snapshot, its defined in pkg/awsinventory/awsinventory.go
		if err := awsinventory.Collect(ctx, src.aws.clients, src.aws.name, src.aws.region, snap); err != nil {
			return nil, fmt.Errorf("--aws: %w", err)
		}
	}
	return snap, nil
}

// explainAccess takes an error from the read and turns the collector's access errors into what to do about them, other errors pass through unchanged
func explainAccess(err error) error {
	var scopeErr *collect.ScopeError
	var nsErr *collect.NamespaceError
	var noAccess *collect.NoAccessError
	var nothing *collect.ReadNothingError
	// checking which access error it is, the error types are defined in pkg/collect/cluster.go and pkg/collect/access.go
	// the scope error gets the full explanation here, the others already explain themselves and are given back unwrapped
	switch {
	case errors.As(err, &scopeErr):
		who := "These credentials"
		// checking if we know who the credentials belong to, if so the error names them
		if scopeErr.Identity != "" {
			who += " (" + scopeErr.Identity + ")"
		}
		// invoking collect.Subject to name the credentials in the suggested rolebinding command, its defined in pkg/collect/access.go
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

// awsReader is the AWS connection for --aws, checked before the cluster read so bad credentials fail early
type awsReader struct {
	// the EKS, IAM and EC2 clients awsinventory reads with
	clients awsinventory.Clients
	// the EKS cluster's name and region
	name, region string
}

// connectAWS takes the kubeconfig context, loads AWS credentials and reads the EKS cluster once, so bad credentials or a wrong cluster fail before the cluster read
func (t clusterTarget) connectAWS(ctx context.Context, kc *kubeContext) (*awsReader, error) {
	// invoking awsTarget for the EKS name and region, from --aws-cluster and --aws-region or the kubeconfig, its defined below
	name, region, err := t.awsTarget(kc)
	if err != nil {
		return nil, err
	}
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	// checking if the kubeconfig names an AWS profile and AWS_PROFILE isnt set, if so AWS is read as the identity the kubeconfig reads the cluster with
	// AWS_PROFILE always wins
	if kc.awsProfile != "" && os.Getenv("AWS_PROFILE") == "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(kc.awsProfile))
	}
	// loading the credentials the standard AWS way, from the environment, ~/.aws config and SSO, or instance roles
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
	// reading the EKS cluster once, so bad credentials or a wrong name or region fail right here
	if _, err := r.clients.EKS.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: &name}); err != nil {
		return nil, fmt.Errorf("--aws: reading EKS cluster %s in %s: %w", name, region, err)
	}
	return r, nil
}

// writePrivate writes a snapshot file only its owner can read (0o600), replacing an existing one
// it writes a new file, mode 0600 from the start, and renames it over path, so an existing file's looser mode never applies to the new contents and a failed write leaves the old file whole
func writePrivate(path string, data []byte) (err error) {
	// creating the temporary file next to path, CreateTemp makes it 0600
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	// removing the temporary file again when anything below fails
	defer func() {
		// checking if writePrivate is returning an error, if so the temporary file goes
		// its removal is best effort, the error that matters is err
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	// checking if the write failed, if so the file still gets closed and both errors come back
	if _, err := f.Write(data); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return err
	}
	// moving the finished file over path in one step
	return os.Rename(f.Name(), path)
}

// awsTarget takes the kubeconfig context and gives back the EKS cluster name and region --aws reads, from the flags if set, otherwise what the context shows
func (t clusterTarget) awsTarget(kc *kubeContext) (name, region string, err error) {
	name, region = t.awsCluster, t.awsRegion
	// checking if --aws-cluster was left out, if so the name comes from the context
	if name == "" {
		name = kc.eksName
	}
	// checking if --aws-region was left out, if so the region comes from the context
	if region == "" {
		region = kc.eksRegion
	}
	// checking if either is still unknown, if so thats a terminal error asking for the flags
	if name == "" || region == "" {
		return "", "", errors.New("--aws needs the EKS cluster's name and region, which the kubeconfig context does not show: pass --aws-cluster and --aws-region")
	}
	return name, region, nil
}

// otherCluster takes the output directory and the source and gives back the cluster an earlier export in the cluster's directory came from, when its a different cluster with the same name, else ""
// two EKS clusters named prod in two regions, or two kubeconfigs with a prod context, would otherwise replace each other's export
// it compares the EKS ARNs when both are known, else the kube-system uid, which is fixed for a cluster's life, and says nothing when it cant tell
func otherCluster(ctx context.Context, dir string, src *source) string {
	// invoking kustomize.ReadExportInfo to read which cluster the earlier export came from, its defined in pkg/emit/kustomize/exportinfo.go
	info, err := kustomize.ReadExportInfo(kustomize.ClusterDir(dir, src.cluster.Name))
	// checking if theres no earlier export to compare, or the source is a directory with no cluster to tell apart
	c, isCluster := src.collector.(*collect.ClusterCollector)
	if err != nil || info == nil || !isCluster {
		return ""
	}
	// the other cluster has the same name, so its told apart by its provider id, or else by when its export was taken
	from := "the one exported " + info.ExportedAt
	// checking if the earlier export recorded its provider id, like an EKS ARN, if so that names the cluster
	if info.ProviderID != "" {
		from = info.ProviderID
	}
	// checking if both sides have a provider id, like an EKS ARN, if so they tell the clusters apart on their own
	if info.ProviderID != "" && src.cluster.ProviderId != "" {
		// checking if they differ, if so its another cluster
		if info.ProviderID != src.cluster.ProviderId {
			return from
		}
		return ""
	}
	// invoking sameCluster to compare the kube-system uids, its defined in verify.go
	if same := sameCluster(ctx, c.Dynamic, info); same != nil && !*same {
		return from
	}
	return ""
}

// confirmReplace takes the output directory, the cluster name and the other cluster an earlier export there came from ("" when its this one), and checks the directory may be written into
// one that exists and isnt empty needs --force, and then a yes at the prompt, or --yes when theres no terminal to ask at
// replacing another cluster's export needs a person to say yes, --yes alone refuses it, so a script never overwrites another cluster
func confirmReplace(dir, cluster, other string, force, yes, interactive bool, in io.Reader, out io.Writer) error {
	entries, err := os.ReadDir(dir)
	// checking if the directory is missing or empty, if so theres nothing to replace
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(entries) == 0) {
		return nil
	}
	if err != nil {
		return err
	}
	// invoking kustomize.ClusterDir for the cluster's directory in the export, the part that gets replaced, its defined in pkg/emit/kustomize/kustomize.go
	root := kustomize.ClusterDir(dir, cluster)
	// checking if --force was left out, if so a non-empty directory is refused
	if !force {
		return fmt.Errorf("output directory %s is not empty; use --force to replace %s in it", dir, root)
	}
	files := 0
	// counting the files that will be deleted, for the message only, so an unreadable entry is skipped and not counted
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		// checking if this entry is a readable file
		if err == nil && !d.IsDir() {
			files++
		}
		return nil
	})
	var msg string
	// checking if there are files of an earlier export of this cluster, if so the message says theyll be deleted, otherwise that nothing is
	if files > 0 {
		msg = fmt.Sprintf("Output directory %s is not empty. %s (%s) will be deleted and written again; everything else in %s is kept.",
			dir, root, text.Plural(files, "file", "files"), dir)
	} else {
		msg = fmt.Sprintf("Output directory %s is not empty. The export will be written to %s; nothing is deleted.", dir, root)
	}
	// checking if the directory holds another cluster's export under this name, if so the message says so first
	if other != "" {
		msg = fmt.Sprintf("%s holds the export of a different cluster, %s, under the same name. Replacing it loses that export; to keep both, export this one with --cluster-name.\n%s", root, other, msg)
		// checking if --yes was given, it answers for a person, but not to replacing another cluster's export
		if yes {
			return fmt.Errorf("%s\n--yes does not replace another cluster's export: pass --cluster-name, or run without --yes to confirm", msg)
		}
	}
	// checking if --yes was given, if so we say what happens and carry on without asking
	if yes {
		fmt.Fprintln(out, msg)
		return nil
	}
	// checking if theres no terminal to ask at, if so thats a terminal error pointing to --yes
	if !interactive {
		return fmt.Errorf("%s\nPass --yes to confirm without a prompt", msg)
	}
	fmt.Fprintf(out, "%s\nContinue? [y/N] ", msg)
	// reading the answer, no answer at all (EOF) counts as a no
	answer, _ := bufio.NewReader(in).ReadString('\n')
	// checking if the answer is a yes, anything else cancels the export
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	}
	return errors.New("export cancelled; nothing was written")
}

// emitter checks the output flags and turns them into the settings of the kustomize writer, kustomize.Emitter, its defined in pkg/emit/kustomize/kustomize.go
func (o *exportOptions) emitter(cmd *cobra.Command) (kustomize.Emitter, error) {
	e := kustomize.Emitter{Force: o.force, UnmanagedOnly: o.unmanagedOnly}
	// checking if --helm-repos was given, if so the file is read and checked now, so a bad entry fails before the read
	if o.helmRepos != "" {
		// invoking helmsource.LoadRepos to read the file, its defined in pkg/helmsource/repos.go
		repos, err := helmsource.LoadRepos(o.helmRepos)
		if err != nil {
			return e, fmt.Errorf("--helm-repos: %w", err)
		}
		e.HelmRepos = repos
	}
	// checking if ExternalSecrets were turned off, if so none of their settings apply
	if o.externalSecrets == "none" {
		// going over the store flags, they only describe ExternalSecrets, so they get rejected rather than ignored
		for _, name := range externalSecretsFlags {
			// checking if this flag was given
			if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
				return e, fmt.Errorf("--%s configures the ExternalSecrets, so it cannot be used with --external-secrets none", name)
			}
		}
		return e, nil
	}
	// invoking kustomize.DefaultExternalSecrets for the settings of the chosen secret manager, its defined in pkg/emit/kustomize/externalsecrets.go
	es, err := kustomize.DefaultExternalSecrets(o.externalSecrets)
	if err != nil {
		return e, fmt.Errorf("--external-secrets: %w", err)
	}
	// checking if the store kind is one of the two External Secrets knows
	if o.secretStoreKind != "ClusterSecretStore" && o.secretStoreKind != "SecretStore" {
		return e, fmt.Errorf("--secret-store-kind must be ClusterSecretStore or SecretStore, not %q", o.secretStoreKind)
	}
	es.StoreKind = o.secretStoreKind
	// checking if a store name was given, if so it replaces the default
	if o.secretStore != "" {
		es.StoreName = o.secretStore
	}
	// checking if a key template was given, if so it replaces the default
	if o.secretKeyTemplate != "" {
		es.KeyTemplate = o.secretKeyTemplate
	}
	// invoking kustomize.CheckStore to check the store's kind and name, its defined in pkg/emit/kustomize/externalsecrets.go
	if err := kustomize.CheckStore(es.StoreKind, es.StoreName); err != nil {
		return e, fmt.Errorf("--secret-store: %w", err)
	}
	// invoking Validate to check the key template, its defined in pkg/emit/kustomize/externalsecrets.go
	if err := es.Validate(); err != nil {
		return e, fmt.Errorf("--secret-key-template: %w", err)
	}
	e.ExternalSecrets = es
	return e, nil
}

// collector loads the kubeconfig and builds the cluster collector with its clients, discovery, dynamic, and metadata unless fullList is set
func (t clusterTarget) collector() (*collect.ClusterCollector, *kubeContext, error) {
	// invoking loadConfig to read the kubeconfig for the chosen context, its defined in kubeconfig.go
	kc, err := loadConfig(t.kubeconfig, t.context)
	if err != nil {
		return nil, nil, err
	}
	cfg := kc.config

	// invoking discover.New for the client that lists the cluster's API types, its defined in internal/discover/discover.go
	dc, err := discover.New(cfg)
	if err != nil {
		return nil, nil, err
	}

	// building the dynamic client that lists objects of any type without generated go types
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	// collect.ClusterCollector reads the cluster with those clients, its defined in pkg/collect/cluster.go
	collector := &collect.ClusterCollector{Discovery: dc, Dynamic: dyn, Namespaces: t.namespaces, FallbackNamespace: kc.namespace, RequireNamespaces: true}
	// checking if --full-list was left out, if so high-volume managed types like pods and events are listed metadata-only first
	if !t.fullList {
		// building the metadata client for those metadata-only lists
		if collector.Metadata, err = metadata.NewForConfig(cfg); err != nil {
			return nil, nil, err
		}
	}
	return collector, kc, nil
}

// newID gives back a random 128-bit snapshot id, hex encoded, so each snapshot can be told apart
func newID() (string, error) {
	b := make([]byte, 16)
	// filling the id with random bytes, a failure means the system has no randomness to give
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
