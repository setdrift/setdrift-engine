// Package provenance classifies which system manages each object, Argo CD, Flux, Helm, an operator, a CI/CD pipeline, the platform, or nothing, meaning a person's kubectl apply
package provenance

import (
	"context"
	"maps"
	"slices"
	"strings"

	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/platform"
	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// the labels and annotations that tell an external system manages an object
const (
	argoTrackingID = "argocd.argoproj.io/tracking-id"
	// DefaultArgoInstanceLabel is the label Argo CD's label tracking uses unless argocd-cm sets instanceLabelKey
	DefaultArgoInstanceLabel = "app.kubernetes.io/instance"
	fluxKustomizeName        = "kustomize.toolkit.fluxcd.io/name"
	fluxKustomizeNS          = "kustomize.toolkit.fluxcd.io/namespace"
	fluxHelmName             = "helm.toolkit.fluxcd.io/name"
	fluxHelmNS               = "helm.toolkit.fluxcd.io/namespace"
	managedByLabel           = "app.kubernetes.io/managed-by"
	helmReleaseName          = "meta.helm.sh/release-name"
	helmReleaseNS            = "meta.helm.sh/release-namespace"
)

// the API groups of the external systems' own objects, for finding the managers among the collected objects
const (
	argoGroup          = "argoproj.io"
	fluxKustomizeGroup = "kustomize.toolkit.fluxcd.io"
	fluxHelmGroup      = "helm.toolkit.fluxcd.io"
)

// LabelClassifier classifies objects from the labels and annotations that GitOps tools stamp on what they apply
// the checks run from the strongest evidence to the weakest, and tool-specific markers before Helm's, since Flux and Argo CD also render Helm charts so their objects can carry Helm markers too
// first the Kubernetes platform, see the platform package, objects EKS creates and keeps up to date, which a new cluster gets from EKS anyway
// second the Flux labels, the Kustomization one then the HelmRelease one
// third the Argo CD tracking-id annotation, if it names this object, a tracking id naming another object was copied with kubectl get | kubectl apply and Argo CD doesnt manage the copy
// fourth the meta.helm.sh/release-* annotations, which only `helm install` and `helm upgrade` write
// fifth a custom Argo CD instance label, from label tracking with application.instanceLabelKey set in argocd-cm, which no chart sets
// only markers a tool writes itself count, the labels every Helm chart's templates set dont, app.kubernetes.io/instance (also Argo CD's default tracking label before 3.0) and app.kubernetes.io/managed-by=Helm
// theyre on whatever renders the chart, helm install, helm template | kubectl apply, Argo CD or Flux, so they cant say who applied it, and an object with only those is unmanaged
// a pipeline makes its objects clear with the setdrift.io/source-* annotations, see pipeline below
// the stage then checks that each manager exists in the collected objects, see managerIndex.check below, and drops Helm's release records, which are Helm's state rather than configuration
// last an owner reference to an object outside the export, meaning an operator created it
// TODO detect operator-managed objects from managedFields managers, that needs real cluster dumps to learn the manager names, and resolve Argo Applications to their Git source
type LabelClassifier struct {
	// ArgoInstanceLabel is the label Argo CD's label tracking uses, when its not DefaultArgoInstanceLabel, from application.instanceLabelKey in argocd-cm
	// the default label is never read, every Helm chart sets it too
	ArgoInstanceLabel string
	// Platform is the stage that classified the platform, which runs before this one, its Result decides which name-based platform rules apply, like kind's add-ons
	// with nil or an unsure result only the rules whose evidence names its platform apply, like EKS's and kubeadm's field managers
	Platform *platform.Stage
}

// ManagerNotFound is the code of the finding on an object whose markers name a manager that isnt in the cluster, like an Argo CD Application, a Flux object or a Helm release
const ManagerNotFound = "MANAGER_NOT_FOUND"

// Name gives back the stage's name, it implements resource.Stage, which is defined in pkg/resource/resource.go
func (LabelClassifier) Name() string { return "provenance" }

// Process implements resource.Stage, it sets each object's provenance and drops Helm's release records, with a finding per namespace saying how many
// the most specific evidence wins, an Argo CD Application that deploys a Helm chart makes its objects Argo CD's
func (c LabelClassifier) Process(_ context.Context, objs []*resource.Object) ([]*resource.Object, []*snapshotv1alpha1.Finding, error) {
	// invoking newManagerIndex to note which managers exist among the objects, its defined below
	idx := newManagerIndex(objs)
	out := make([]*resource.Object, 0, len(objs))
	// releases counts the dropped Helm release records per namespace
	releases := map[string]int{}
	// running each object through the classifier checks
	for _, o := range objs {
		// invoking helmReleaseRecord and checking if the object is one of Helm's release records, if so its counted and left out, its defined below
		if _, ok := helmReleaseRecord(o); ok {
			releases[o.GetNamespace()]++
			continue
		}
		// invoking fromMarkers to classify the object from its markers, with platformResult giving the platform, theyre defined below
		o.Provenance = c.fromMarkers(o, c.platformResult())
		// invoking check to fill in the manager's namespace or flag a missing manager, its defined below
		idx.check(o)
		// invoking pipeline to apply a CI/CD pipeline's evidence on top, its defined below
		o.Provenance = c.pipeline(o, o.Provenance)
		out = append(out, o)
	}
	var findings []*snapshotv1alpha1.Finding
	// reporting the dropped release records, one finding per namespace in sorted order
	for _, ns := range slices.Sorted(maps.Keys(releases)) {
		// invoking resource.NewFinding to build the finding, its defined in pkg/resource/resource.go
		// text.Plural and text.Word pick the wording for the count, theyre defined in internal/text/text.go
		findings = append(findings, resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "HELM_RELEASE_RECORDS_SKIPPED",
			"%s in namespace %q %s not exported (Helm's release history; applying it would corrupt Helm's state)",
			text.Plural(releases[ns], "Helm release record", "Helm release records"), ns, text.Word(releases[ns], "was", "were")))
	}
	return out, findings, nil
}

// platformResult gives back the platform classification from the Platform stage, or nil when theres no stage
func (c LabelClassifier) platformResult() *platform.Classification {
	// checking if no platform stage was given, if so theres no classification
	if c.Platform == nil {
		return nil
	}
	return c.Platform.Result
}

// Classify gives back o's provenance from its labels, annotations and field managers, as Process does, without the checks against the other objects
func (c LabelClassifier) Classify(o *resource.Object) *snapshotv1alpha1.Provenance {
	// invoking fromMarkers and then pipeline, the same as Process, theyre defined below
	return c.pipeline(o, c.fromMarkers(o, c.platformResult()))
}

// the annotations a CI/CD pipeline stamps on what it deploys, so its objects arent taken for ones applied by hand, only the repository is required
// in GitHub Actions setdrift.io/source-repo takes $GITHUB_SERVER_URL/$GITHUB_REPOSITORY
// setdrift.io/source-path takes the path in the repository, e.g. deploy/prod
// setdrift.io/source-branch takes $GITHUB_REF_NAME, or $GITHUB_HEAD_REF on a pull request
// setdrift.io/source-tag takes $GITHUB_REF_NAME when a tag is deployed
// setdrift.io/source-revision takes $GITHUB_SHA
// setdrift.io/deployed-by takes $GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID
const (
	SourceRepoAnnotation     = "setdrift.io/source-repo"
	SourcePathAnnotation     = "setdrift.io/source-path"
	SourceBranchAnnotation   = "setdrift.io/source-branch"
	SourceTagAnnotation      = "setdrift.io/source-tag"
	SourceRevisionAnnotation = "setdrift.io/source-revision"
	DeployedByAnnotation     = "setdrift.io/deployed-by"
)

// repoHosts maps the shorthands for the common Git hosts, like github/foo/bar, to their full host names
var repoHosts = map[string]string{"github": "github.com", "gitlab": "gitlab.com", "bitbucket": "bitbucket.org"}

// CanonicalRepo takes a repository however a pipeline names it and writes it as one url, so the same repository always groups the same
// e.g. https://github.com/foo/bar, http://github.com/foo/bar/, github.com/foo/bar, github/foo/bar, https://github.com/foo/bar.git, git@github.com:foo/bar.git and ssh://git@github.com/foo/bar all become https://github.com/foo/bar
// credentials in it, like https://user:token@host/..., are dropped so they never reach a snapshot
// a value that names no host is kept as written
func CanonicalRepo(repo string) string {
	r := strings.TrimSpace(repo)
	r = strings.TrimSuffix(strings.TrimSuffix(r, "/"), ".git")
	// checking if its the scp-like form git@host:owner/repo, the part before the @ has no slash or colon in that form
	if at, rest, ok := strings.Cut(r, "@"); ok && !strings.Contains(at, "/") && !strings.Contains(at, ":") {
		// checking if theres a colon after the host that doesnt start a //, if so it becomes a slash
		if host, path, ok := strings.Cut(rest, ":"); ok && !strings.HasPrefix(path, "//") {
			r = host + "/" + path
		}
	}
	// checking if theres a scheme, as in scheme://[user[:token]@]host/path, if so its dropped
	if _, rest, ok := strings.Cut(r, "://"); ok {
		r = rest
	}
	// checking for an @ in the host part, the last one there ends the userinfo, an @ in the path isnt userinfo
	if at := strings.LastIndex(r[:strings.Index(r+"/", "/")], "@"); at >= 0 {
		r = r[at+1:]
	}
	host, path, ok := strings.Cut(r, "/")
	// checking if theres no path after the host, if so it names no repository and is kept as written, minus credentials
	if !ok || path == "" {
		// invoking withoutUserinfo to drop any credentials, its defined below
		return withoutUserinfo(repo)
	}
	host = strings.ToLower(host)
	// checking if the host is one of the shorthands, if so its full name is used
	if full, ok := repoHosts[host]; ok {
		host = full
	}
	// checking if the host has no dot or port, if so no host is named and its kept as written, minus credentials
	if !strings.Contains(host, ".") && !strings.Contains(host, ":") {
		// invoking withoutUserinfo to drop any credentials, its defined below
		return withoutUserinfo(repo)
	}
	return "https://" + host + "/" + strings.TrimSuffix(path, "/")
}

// withoutUserinfo gives back repo trimmed, without what comes before an @ in its first path element, as in [scheme://]user:token@host/...
func withoutUserinfo(repo string) string {
	r := strings.TrimSpace(repo)
	start := 0
	// checking if theres a scheme, if so the host part starts after it
	if i := strings.Index(r, "://"); i >= 0 {
		start = i + len("://")
	}
	end := len(r)
	// checking if theres a slash after the host part, if so the host part ends there
	if i := strings.Index(r[start:], "/"); i >= 0 {
		end = start + i
	}
	// checking if the host part has an @, if so everything up to the last one is dropped
	if at := strings.LastIndex(r[start:end], "@"); at >= 0 {
		r = r[:start] + r[start+at+1:]
	}
	return r
}

// SourceAnnotations lists the CI/CD source annotations, in the order theyre read
var SourceAnnotations = []string{SourceRepoAnnotation, SourcePathAnnotation, SourceBranchAnnotation, SourceTagAnnotation,
	SourceRevisionAnnotation, DeployedByAnnotation}

// pipeline takes o and the provenance its markers gave, and applies a CI/CD pipeline's evidence on top
// the source annotations make an object CI/CD's, managed from its repository, when no tool claims it, like what helm template | kubectl apply leaves
// on an object Argo CD, Flux, Helm, an operator or the platform manages, theyre kept as extra evidence since those markers are stronger
// only the annotations make an object a pipeline's, they name the repository and path its deployed from, which is where a change to it goes back to
func (c LabelClassifier) pipeline(o *resource.Object, p *snapshotv1alpha1.Provenance) *snapshotv1alpha1.Provenance {
	ann := o.GetAnnotations()
	src := map[string]string{}
	// collecting the source annotations the object carries
	for _, k := range SourceAnnotations {
		// checking if the annotation is set, empty ones are skipped
		if v := ann[k]; v != "" {
			src[k] = v
		}
	}
	// weak means nothing claims the object
	weak := p.GetSource() == snapshotv1alpha1.Source_SOURCE_UNMANAGED || p.GetSource() == snapshotv1alpha1.Source_SOURCE_UNSPECIFIED
	// checking if theres a repository annotation, if so its written in its canonical form
	if repo, ok := src[SourceRepoAnnotation]; ok {
		// invoking CanonicalRepo to write it as one url, its defined above
		src[SourceRepoAnnotation] = CanonicalRepo(repo)
	}
	// checking if theres a repository and the markers are weak, if so the object is CI/CD's, managed from that repository
	if repo := src[SourceRepoAnnotation]; repo != "" && weak {
		manager := repo
		// checking if theres a path too, if so the manager names it after a //
		if path := src[SourcePathAnnotation]; path != "" {
			manager += "//" + strings.TrimPrefix(path, "/")
		}
		return &snapshotv1alpha1.Provenance{Source: snapshotv1alpha1.Source_SOURCE_CI_CD, Manager: manager, Evidence: src}
	}
	// checking if theres source evidence on an object something stronger manages, if so its added to that evidence
	if len(src) > 0 && !weak {
		ev := make(map[string]string, len(p.GetEvidence())+len(src))
		// copying the markers' evidence first
		for k, v := range p.GetEvidence() {
			ev[k] = v
		}
		// then adding the source annotations
		for k, v := range src {
			ev[k] = v
		}
		p.Evidence = ev
		return p
	}
	return p
}

// fromMarkers takes o and the platform classification and classifies o from the markers GitOps tools, Helm, operators and the platform leave, in the order LabelClassifier describes
func (c LabelClassifier) fromMarkers(o *resource.Object, cls *platform.Classification) *snapshotv1alpha1.Provenance {
	labels, ann := o.GetLabels(), o.GetAnnotations()
	instanceLabel := c.ArgoInstanceLabel
	// checking if the instance label is the default one, if so its never read, every Helm chart sets it too
	if instanceLabel == DefaultArgoInstanceLabel {
		instanceLabel = ""
	}
	// p builds a provenance from its source, manager and evidence
	p := func(src snapshotv1alpha1.Source, manager string, evidence map[string]string) *snapshotv1alpha1.Provenance {
		return &snapshotv1alpha1.Provenance{Source: src, Manager: manager, Evidence: evidence}
	}
	// invoking platformObject and checking if the platform manages the object, its defined below
	if manager, ev := platformObject(o, cls); ev != nil {
		return p(snapshotv1alpha1.Source_SOURCE_PLATFORM, manager, ev)
	}
	// checking if theres a Flux Kustomization name label, if so Flux manages it, the manager as namespace/name
	if labels[fluxKustomizeName] != "" {
		// invoking join to write the manager as namespace/name, its defined below
		return p(snapshotv1alpha1.Source_SOURCE_FLUX, join(labels[fluxKustomizeNS], labels[fluxKustomizeName]),
			map[string]string{fluxKustomizeName: labels[fluxKustomizeName]})
	}
	// checking if theres a Flux HelmRelease name label, if so Flux manages it, the manager as namespace/name
	if labels[fluxHelmName] != "" {
		return p(snapshotv1alpha1.Source_SOURCE_FLUX, join(labels[fluxHelmNS], labels[fluxHelmName]),
			map[string]string{fluxHelmName: labels[fluxHelmName]})
	}
	// checking if theres an Argo CD tracking id annotation
	if id := ann[argoTrackingID]; id != "" {
		// invoking parseTrackingID and matches and checking if the tracking id names this object, if so Argo CD manages it, theyre defined below
		if t, ok := parseTrackingID(id); ok && t.matches(o) {
			return p(snapshotv1alpha1.Source_SOURCE_ARGO_CD, t.app, map[string]string{argoTrackingID: id})
		}
		// the tracking id names another object, so it was probably copied, invoking AddFinding to say so, its defined in pkg/resource/resource.go
		o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "ARGOCD_TRACKING_ID_MISMATCH",
			"the %s annotation %q names another object, so Argo CD does not manage this one; it was probably copied from that object",
			argoTrackingID, id)
	}
	// checking if theres a Helm release name annotation, if so Helm manages it, the manager as namespace/release
	if name := ann[helmReleaseName]; name != "" {
		ev := map[string]string{helmReleaseName: name}
		// checking if theres a managed-by label too, if so its kept as evidence
		if v := labels[managedByLabel]; v != "" {
			ev[managedByLabel] = v
		}
		ns := ann[helmReleaseNS]
		// checking if the release namespace annotation is missing, if so the object's own namespace stands in
		if ns == "" {
			ns = o.GetNamespace()
		}
		return p(snapshotv1alpha1.Source_SOURCE_HELM, join(ns, name), ev)
	}
	// checking if a custom Argo CD instance label is set and the object has it, if so Argo CD manages it from label tracking
	if app := labels[instanceLabel]; instanceLabel != "" && app != "" {
		// invoking argoApp to write the instance name as namespace/app, its defined below
		return p(snapshotv1alpha1.Source_SOURCE_ARGO_CD, argoApp(app), map[string]string{instanceLabel: app})
	}

	// looking for an owner in a custom resource group, an object that wasnt collected and that an operator runs
	for _, r := range o.GetOwnerReferences() {
		// invoking customGroup and checking if the owner's group is a custom resource group, if so an operator manages it, its defined below
		if group, _, _ := strings.Cut(r.APIVersion, "/"); customGroup(group) {
			return p(snapshotv1alpha1.Source_SOURCE_OPERATOR, r.Kind+"/"+r.Name,
				map[string]string{"ownerReference": r.APIVersion + " " + r.Kind + " " + r.Name})
		}
	}
	return p(snapshotv1alpha1.Source_SOURCE_UNMANAGED, "", nil)
}

// eksManagers are the field managers of the components EKS runs, its add-ons (eks), the control plane's own objects (eks-internal) and Auto Mode (eks-auto-mode)
// eks-resource-optimizer is Auto Mode's resource optimizer, which writes its MutatingAdmissionPolicy resource-optimization.eks.amazonaws.com (seen on 1.37)
var eksManagers = map[string]bool{"eks": true, "eks-internal": true, "eks-auto-mode": true, "eks-resource-optimizer": true}

// coreManagers are the field managers of Kubernetes' own components, which write status and defaults into objects whoever created them, so theyre ignored
var coreManagers = map[string]bool{
	"kube-apiserver": true, "kube-controller-manager": true, "api-priority-and-fairness-config-consumer-v1": true,
}

// aksManagers are the field managers of the components AKS runs in the cluster, eno and its reconciler (AKS's own deployer of its add-ons), the live patching controller, azure cni's dnc-rc, and the reconciler of AKS's webhooks
var aksManagers = map[string]bool{"eno": true, "eno-reconciler": true, "live-patching-controller": true, "dnc-rc": true, "aks-webhook-reconciler": true,
	// CoreDNS's autoscaler, seen writing coredns-autoscaler on AKS 1.36 (v0.3.0's Azure run)
	"cluster-proportional-autoscaler": true,
}

// aksUnmarked are AKS's own objects that carry no label and no field manager of AKS's, keyed by namespace/kind/name, the known exceptions, named and checked only on AKS like gkeUnmarked
// azure-cns and bootstrap-signer are ServiceAccounts AKS creates with the cluster with no field manager at all (AKS 1.36)
// overlay-upgrade-data is a ConfigMap AKS's own Flux writes from its control plane, its Flux labels name a namespace that isnt in the cluster
var aksUnmarked = map[string]bool{
	"kube-system/ServiceAccount/azure-cns":        true,
	"kube-system/ServiceAccount/bootstrap-signer": true,
	"kube-system/ConfigMap/overlay-upgrade-data":  true,
}

// gkeManagers are the field managers of the components GKE runs in the cluster, its add-on manager, its common webhooks, the load balancer controller (glbc), the GCP controller manager, the volume populator and its managed cluster autoscaler
var gkeManagers = map[string]bool{
	"kube-addon-manager": true, "gke-common-webhooks": true, "glbc": true, "gcp-controller-manager": true, "gke-volume-populator": true, "cluster-autoscaler": true,
	// kube-dns's autoscaler and the metrics-server's APIService minder, seen writing kube-dns-autoscaler and v1beta1.metrics.k8s.io (M1 of v0.3.0)
	"cluster-proportional-autoscaler": true, "api-service-nanny": true,
}

// gkeNamespaceManagers are field managers that are GKE's only in their namespace, Managed Prometheus's operator is named just operator, too common a name to trust anywhere else
// an object there written only by these and gkeManagers is GKE's, so OperatorConfig gmp-public/config stays the user's once anyone else edits it
var gkeNamespaceManagers = map[string]map[string]bool{
	"gmp-system": {"operator": true},
	"gmp-public": {"operator": true},
}

// gkeUnmarked are GKE's own objects that carry no label and no field manager, so nothing on them says GKE wrote them, keyed by namespace/kind/name
// the rule is to recognize a platform's objects by the markers it writes, these are the known exceptions, named and checked only on GKE
// gke-common-webhook-heartbeat is a ConfigMap GKE rewrites constantly, so left in it shows as a difference in every verify
// system:gke-kcm-ccm-leader-election is a Role and RoleBinding GKE creates with kubectl, the same field manager a person's commands have
// cloud-provider is a ServiceAccount GKE creates with the cluster (seen on 1.36), shown as applied by hand without this
var gkeUnmarked = map[string]bool{
	"kube-system/ServiceAccount/cloud-provider":                  true,
	"kube-system/ConfigMap/gke-common-webhook-heartbeat":         true,
	"kube-system/Role/system:gke-kcm-ccm-leader-election":        true,
	"kube-system/RoleBinding/system:gke-kcm-ccm-leader-election": true,
}

// the labels a platform's add-on manager and AKS put on the objects they own
const (
	// addonModeLabel is the add-on manager's, on GKE, AKS and other clusters built with it, Reconcile means it keeps the object as it set it, EnsureExists that it creates it once and leaves it to you
	addonModeLabel = "addonmanager.kubernetes.io/mode"
	// aksManagedByLabel is AKS's, with the value aks
	aksManagedByLabel = "kubernetes.azure.com/managedby"
)

// eksBootstrap names the objects EKS creates with kubectl when it creates a cluster, keyed by group/kind
// their only field manager is kubectl-client-side-apply, as if a person had applied them
// rbac names starting with "eks:" are EKS's too, eksEvidence checks those separately
var eksBootstrap = map[string]map[string]bool{
	"rbac.authorization.k8s.io/ClusterRole":        {"vpc-resource-controller-role": true},
	"rbac.authorization.k8s.io/ClusterRoleBinding": {"vpc-resource-controller-rolebinding": true},
	"rbac.authorization.k8s.io/Role":               {"eks-vpc-resource-controller-role": true},
	"rbac.authorization.k8s.io/RoleBinding":        {"eks-vpc-resource-controller-rolebinding": true},
	"admissionregistration.k8s.io/MutatingWebhookConfiguration": {
		"pod-identity-webhook": true, "vpc-resource-mutating-webhook": true,
	},
	"admissionregistration.k8s.io/ValidatingWebhookConfiguration": {"vpc-resource-validating-webhook": true},
	"storage.k8s.io/CSIDriver":                                    {"efs.csi.aws.com": true},
}

// builtinNamespaces are the namespaces Kubernetes creates in every cluster
var builtinNamespaces = map[string]bool{"default": true, "kube-system": true, "kube-public": true, "kube-node-lease": true}

// eksCRDGroups are the CRD groups of the controllers EKS runs in its control plane, the VPC resource controller, the network policy controller and Auto Mode
var eksCRDGroups = []string{"eks.amazonaws.com", "vpcresources.k8s.aws", "networking.k8s.aws"}

// platformObject takes o and the platform classification and gives back which part of the platform manages o, eks, kubeadm or kind, with the evidence, or nil evidence
// a new cluster gets its own copy of these objects from its platform, exported they would carry the old cluster's values like webhook CA bundles, endpoints and kubelet settings, and applied to another cluster overwrite its own
// EKS's and kubeadm's own field managers name their platform, so those rules apply on any cluster
// kind's add-ons were applied with kubectl and are known only by name, so those rules apply only on a cluster classified as kind
func platformObject(o *resource.Object, cls *platform.Classification) (string, map[string]string) {
	// checking if its one of the namespaces Kubernetes itself creates in every cluster, if so a new cluster has its own, and applied elsewhere it would only take over its metadata
	if gvk := o.GroupVersionKind(); gvk.Group == "" && gvk.Kind == "Namespace" && builtinNamespaces[o.GetName()] {
		return "kubernetes", map[string]string{"namespace": o.GetName()}
	}
	// invoking eksEvidence and checking if EKS manages the object, its defined below
	if ev := eksEvidence(o); ev != nil {
		return "eks", ev
	}
	// invoking addonEvidence and checking if the platform's add-on manager or AKS owns the object, its defined below
	if manager, ev := addonEvidence(o, cls); ev != nil {
		return manager, ev
	}
	// invoking Is and checking if the cluster is confidently AKS or GKE, their managers names are only theirs on their own platform
	if cls.Is(platform.AKS) {
		// invoking onlyManagers to check every writer is one of AKS's, its defined below
		if ev := onlyManagers(o, aksManagers); ev != nil {
			return "aks", ev
		}
		// checking if its a CRD of Cilium's, the network AKS runs (Azure CNI powered by Cilium), whose operator and Helm release create them with no marker of AKS's
		// a Cilium installed by hand on AKS is the cluster's network too, which a new cluster gets from its own setup, not from the export
		if o.GroupVersionKind().Kind == "CustomResourceDefinition" && strings.HasSuffix(o.GetName(), ".cilium.io") {
			return "aks", map[string]string{"crd": o.GetName()}
		}
		// checking if its one of AKS's known objects that carry no marker of AKS's, if so its AKS's by name
		if aksUnmarked[o.GetNamespace()+"/"+o.GroupVersionKind().Kind+"/"+o.GetName()] {
			return "aks", map[string]string{"name": o.GetName()}
		}
	}
	if cls.Is(platform.GKE) {
		// invoking onlyManagers to check every writer is one of GKE's, its defined below
		if ev := onlyManagers(o, gkeManagers); ev != nil {
			return "gke", ev
		}
		// checking if its in a namespace with GKE managers of its own, like Managed Prometheus's operator, if so those count too
		if extra, ok := gkeNamespaceManagers[o.GetNamespace()]; ok {
			both := maps.Clone(gkeManagers)
			maps.Copy(both, extra)
			// invoking onlyManagers with the namespace's managers added, its defined below
			if ev := onlyManagers(o, both); ev != nil {
				return "gke", ev
			}
		}
		// checking if its one of GKE's known objects that carry no marker at all, if so its GKE's by name
		if gkeUnmarked[o.GetNamespace()+"/"+o.GroupVersionKind().Kind+"/"+o.GetName()] {
			return "gke", map[string]string{"name": o.GetName()}
		}
	}
	// invoking kubeadmEvidence and checking if kubeadm created it, its defined below
	if ev := kubeadmEvidence(o, cls); ev != nil {
		return "kubeadm", ev
	}
	// invoking Is and checking if the cluster is confidently kind, its defined in pkg/platform/platform.go
	if cls.Is(platform.Kind) {
		// invoking kindEvidence and checking if the object is one of kind's add-ons, its defined below
		if ev := kindEvidence(o); ev != nil {
			return "kind", ev
		}
	}
	return "", nil
}

// addonEvidence takes o and the platform classification and gives back which platform owns o and the evidence, from the labels the platform itself puts on it, or nil evidence
// AKS labels its objects kubernetes.azure.com/managedby=aks, and the add-on manager (GKE, AKS and others) labels them addonmanager.kubernetes.io/mode
// Reconcile objects are the platform's, the add-on manager puts back any change, EnsureExists ones it creates once for you to change, like AKS's coredns-custom
// so an EnsureExists object is the platform's only while one manager has written it, once someone else has its the user's configuration and is exported
func addonEvidence(o *resource.Object, cls *platform.Classification) (string, map[string]string) {
	labels := o.GetLabels()
	// name gives the platform the object belongs to, by the cluster's classification
	name := "platform"
	switch {
	case cls.Is(platform.AKS):
		name = "aks"
	case cls.Is(platform.GKE):
		name = "gke"
	}
	// checking if AKS's own label is on it, if so AKS owns it
	if v := labels[aksManagedByLabel]; strings.EqualFold(v, "aks") {
		return "aks", map[string]string{aksManagedByLabel: v}
	}
	switch mode := labels[addonModeLabel]; mode {
	case "Reconcile":
		return name, map[string]string{addonModeLabel: mode}
	case "EnsureExists":
		// invoking writers to list who wrote the object besides Kubernetes' own components, its defined below
		if w := writers(o); len(w) > 1 {
			// invoking AddFinding to say the object was changed after the platform created it, its defined in pkg/resource/resource.go
			o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "PLATFORM_OBJECT_CHANGED",
				"was created by the platform for you to change, and changed since by %s, so it is exported to keep the change", strings.Join(w[1:], ", "))
			return "", nil
		}
		return name, map[string]string{addonModeLabel: mode}
	}
	return "", nil
}

// onlyManagers takes o and a platform's field managers and gives back the evidence when every manager that wrote o, besides Kubernetes' own components, is one of them, or nil
func onlyManagers(o *resource.Object, platformManagers map[string]bool) map[string]string {
	// invoking writers to list who wrote the object, its defined below
	w := writers(o)
	// checking if anyone wrote it and each one is the platform's
	if len(w) == 0 {
		return nil
	}
	for _, m := range w {
		// checking if this writer isnt the platform's, if so the object isnt only the platform's
		if !platformManagers[m] {
			return nil
		}
	}
	return map[string]string{"managedFields": strings.Join(w, ",")}
}

// writers takes o and gives back the field managers that wrote it, in the order managedFields lists them, once each, leaving out Kubernetes' own components and status-only writes
func writers(o *resource.Object) []string {
	var out []string
	// going through the managedFields entries
	for _, m := range o.GetManagedFields() {
		// checking if its Kubernetes' own component, a status-only write, or already listed, if so its skipped
		if coreManagers[m.Manager] || m.Subresource == "status" || slices.Contains(out, m.Manager) {
			continue
		}
		out = append(out, m.Manager)
	}
	return out
}

// kubeadmEvidence gives back the evidence that kubeadm created o and nothing changed it since, meaning its only field manager is kubeadm, besides Kubernetes' own
// an object kubeadm created that someone changed, like a CoreDNS Corefile edit, is the user's configuration, its exported with a finding saying so
// on a cluster known to be built with kubeadm, so kubeadm, kind or minikube, the service accounts kubeadm creates without a field manager count too
func kubeadmEvidence(o *resource.Object, cls *platform.Classification) map[string]string {
	var managers, others []string
	// collecting the field managers, and the ones that arent kubeadm
	for _, m := range o.GetManagedFields() {
		// checking if its one of Kubernetes' own managers, if so its ignored
		if coreManagers[m.Manager] {
			continue
		}
		managers = append(managers, m.Manager)
		// checking if the manager isnt kubeadm, if so its someone else who wrote to the object
		if m.Manager != "kubeadm" {
			others = append(others, m.Manager)
		}
	}
	// checking if kubeadm is the only manager, or kubeadm and others wrote to it
	switch {
	case len(managers) > 0 && len(others) == 0:
		return map[string]string{"managedFields": "kubeadm"}
	case len(others) < len(managers):
		// invoking AddFinding to say the object was changed after kubeadm created it, its defined in pkg/resource/resource.go
		o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "PLATFORM_OBJECT_CHANGED",
			"was created by kubeadm and changed since by %s, so it is exported to keep the change; a cluster built with kubeadm creates its own",
			strings.Join(others, ", "))
		return nil
	}
	// invoking Is to tell if the cluster is confidently built with kubeadm, its defined in pkg/platform/platform.go
	builtWithKubeadm := cls.Is(platform.Kubeadm) || cls.Is(platform.Kind) || cls.Is(platform.Minikube)
	// checking if its one of the kube-system service accounts kubeadm creates without a field manager, on a cluster built with kubeadm
	if builtWithKubeadm && len(managers) == 0 && o.GetNamespace() == "kube-system" && o.GetKind() == "ServiceAccount" &&
		kubeadmServiceAccounts[o.GetName()] {
		return map[string]string{"name": o.GetName()}
	}
	return nil
}

// kubeadmServiceAccounts are the kube-system service accounts kubeadm creates without a field manager
var kubeadmServiceAccounts = map[string]bool{"coredns": true, "kube-proxy": true, "bootstrap-signer": true}

// kindEvidence gives back the evidence that o is one of kind's own add-ons, or nil
// those are the kindnet network plugin and the local-path storage provisioner, with its namespace and its "standard" StorageClass
func kindEvidence(o *resource.Object) map[string]string {
	gk := o.GroupVersionKind().GroupKind()
	name, ns := o.GetName(), o.GetNamespace()
	// checking if the object is in or is the local-path-storage namespace, is kindnet, is the provisioner's rbac, or is its standard StorageClass
	switch {
	case ns == "local-path-storage":
		return map[string]string{"namespace": ns}
	case gk.Group == "" && gk.Kind == "Namespace" && name == "local-path-storage":
		return map[string]string{"name": name}
	case name == "kindnet" && (ns == "kube-system" || ns == ""):
		return map[string]string{"name": name}
	case gk.Group == "rbac.authorization.k8s.io" && strings.HasPrefix(name, "local-path-provisioner-"):
		return map[string]string{"name": name}
	case gk.Group == "storage.k8s.io" && gk.Kind == "StorageClass" && name == "standard" && o.Object["provisioner"] == "rancher.io/local-path":
		return map[string]string{"name": name}
	}
	return nil
}

// eksEvidence gives back the evidence that EKS manages o, or nil
// EKS creates these objects in every cluster, or with an add-on, and keeps them up to date
// exported they would carry the old cluster's values, like webhook CA bundles and endpoints, and some are rejected on apply
func eksEvidence(o *resource.Object) map[string]string {
	// checking if the managed-by label says eks, in any case, if so thats the evidence
	if v := o.GetLabels()[managedByLabel]; strings.EqualFold(v, "eks") {
		return map[string]string{managedByLabel: v}
	}
	var managers []string
	onlyEKS := true
	// collecting the field managers, and whether all of them are EKS's
	for _, m := range o.GetManagedFields() {
		// checking if its one of Kubernetes' own managers, if so its ignored
		if coreManagers[m.Manager] {
			continue
		}
		managers = append(managers, m.Manager)
		onlyEKS = onlyEKS && eksManagers[m.Manager]
	}
	// checking if every manager is EKS's, if so EKS manages it
	if len(managers) > 0 && onlyEKS {
		return map[string]string{"managedFields": strings.Join(managers, ",")}
	}

	gvk := o.GroupVersionKind()
	gk := gvk.Group + "/" + gvk.Kind
	name := o.GetName()
	// checking if the object is one EKS is known to create by name
	switch {
	case strings.HasPrefix(gk, "rbac.authorization.k8s.io/") && strings.HasPrefix(name, "eks:"):
		return map[string]string{"name": name}
	case eksBootstrap[gk][name]:
		return map[string]string{"name": name}
	case gk == "storage.k8s.io/StorageClass" && name == "gp2" && o.Object["provisioner"] == "kubernetes.io/aws-ebs":
		// EKS creates gp2 in every cluster, with the in-tree provisioner
		return map[string]string{"name": name}
	case gk == "apiextensions.k8s.io/CustomResourceDefinition":
		_, group, _ := strings.Cut(name, ".")
		// checking if the CRD's group is one of the groups of EKS's controllers
		for _, g := range eksCRDGroups {
			// checking if its this group, if so the CRD belongs to EKS
			if group == g {
				return map[string]string{"group": group}
			}
		}
	}
	return nil
}

// trackingID is a parsed Argo CD tracking id, in the format app:group/kind:namespace/name
type trackingID struct {
	// the application, as namespace/app when the id names its namespace
	app                   string
	group, kind, ns, name string
}

// parseTrackingID takes an Argo CD tracking id and splits it into its parts, telling if it had the expected format
func parseTrackingID(s string) (trackingID, bool) {
	app, rest, ok := strings.Cut(s, ":")
	// checking if theres an app before the first colon, if not its not a tracking id
	if !ok || app == "" {
		return trackingID{}, false
	}
	gk, nsName, ok := strings.Cut(rest, ":")
	// checking if theres a second colon between group/kind and namespace/name
	if !ok {
		return trackingID{}, false
	}
	group, kind, ok := strings.Cut(gk, "/")
	// checking if theres a kind after the group, the group itself may be empty for core objects
	if !ok || kind == "" {
		return trackingID{}, false
	}
	ns, name, ok := strings.Cut(nsName, "/")
	// checking if theres a name after the namespace, the namespace may be empty for cluster-scoped objects
	if !ok || name == "" {
		return trackingID{}, false
	}
	// invoking argoApp to write the app as namespace/app, its defined below
	return trackingID{app: argoApp(app), group: group, kind: kind, ns: ns, name: name}, true
}

// matches tells if the tracking id names o, the namespace isnt compared when either side is cluster-scoped
func (t trackingID) matches(o *resource.Object) bool {
	gvk := o.GroupVersionKind()
	// checking if the group, kind or name differ, if so its another object
	if t.group != gvk.Group || t.kind != gvk.Kind || t.name != o.GetName() {
		return false
	}
	return t.ns == "" || o.GetNamespace() == "" || t.ns == o.GetNamespace()
}

// argoApp takes an Argo CD app instance name, which writes namespace_app for an app outside argocd's namespace, and gives back namespace/app
func argoApp(s string) string {
	// checking if the name has both a namespace and an app around an underscore, if not its kept as is
	if ns, app, ok := strings.Cut(s, "_"); ok && ns != "" && app != "" {
		return ns + "/" + app
	}
	return s
}

// helmReleaseRecord tells if o is one of the records Helm keeps per release revision, and gives back the release name
// thats a Secret of type helm.sh/release.v1 with the default driver, or a ConfigMap labelled owner=helm with HELM_DRIVER=configmap
func helmReleaseRecord(o *resource.Object) (string, bool) {
	gvk := o.GroupVersionKind()
	// checking if the object is in the core group and named like a release revision, if not its no record
	if gvk.Group != "" || !strings.HasPrefix(o.GetName(), "sh.helm.release.v1.") {
		return "", false
	}
	// checking if its a release Secret or a release ConfigMap
	switch gvk.Kind {
	case "Secret":
		// checking if the Secret has the release type
		if t, _ := o.Object["type"].(string); t != "helm.sh/release.v1" {
			return "", false
		}
	case "ConfigMap":
		// checking if the ConfigMap carries Helm's owner label
		if o.GetLabels()["owner"] != "helm" {
			return "", false
		}
	default:
		return "", false
	}
	return o.GetLabels()["name"], true
}

// managerIndex holds the managers found in the collected objects, so an object whose manager was deleted can be flagged
type managerIndex struct {
	// argoApps maps each Application name to the namespaces its found in
	argoApps map[string][]string
	// flux holds group/namespace/name of each Kustomization and HelmRelease
	flux map[string]bool
	// helm holds namespace/release of each Helm release record
	helm map[string]bool

	// whether the manager's own objects can be seen at all, when they cant theres nothing to check against, e.g. Argo CD running in a remote cluster
	argoSeen, fluxSeen bool
	// the namespaces any Secret was collected from, Helm's records are Secrets so a namespace without them cant be checked
	secretNamespaces map[string]bool
}

// newManagerIndex takes the collected objects and indexes the Argo CD Applications, Flux objects and Helm releases among them
func newManagerIndex(objs []*resource.Object) *managerIndex {
	idx := &managerIndex{
		argoApps:         map[string][]string{},
		flux:             map[string]bool{},
		helm:             map[string]bool{},
		secretNamespaces: map[string]bool{},
	}
	// noting each manager object and each namespace with Secrets
	for _, o := range objs {
		gvk := o.GroupVersionKind()
		// checking if the object is an Argo CD Application, a Flux Kustomization or HelmRelease, or a Secret
		switch {
		case gvk.Group == argoGroup && gvk.Kind == "Application":
			idx.argoSeen = true
			idx.argoApps[o.GetName()] = append(idx.argoApps[o.GetName()], o.GetNamespace())
		case (gvk.Group == fluxKustomizeGroup && gvk.Kind == "Kustomization") || (gvk.Group == fluxHelmGroup && gvk.Kind == "HelmRelease"):
			idx.fluxSeen = true
			idx.flux[gvk.Group+"/"+o.GetNamespace()+"/"+o.GetName()] = true
		case gvk.Group == "" && gvk.Kind == "Secret":
			idx.secretNamespaces[o.GetNamespace()] = true
		}
		// invoking helmReleaseRecord and checking if the object is a release record with a name, if so the release is noted, its defined above
		if name, ok := helmReleaseRecord(o); ok && name != "" {
			idx.helm[o.GetNamespace()+"/"+name] = true
		}
	}
	return idx
}

// check takes a classified object and fills in its Argo CD app's namespace when it can, and flags it when its manager isnt in the cluster
func (idx *managerIndex) check(o *resource.Object) {
	p := o.Provenance
	missing := ""
	// checking which kind of manager claims the object, the others have nothing to check
	switch p.Source {
	case snapshotv1alpha1.Source_SOURCE_ARGO_CD:
		// checking if Argo CD's Applications cant be seen or no app is named, if so theres nothing to check against
		if !idx.argoSeen || p.Manager == "" {
			return
		}
		// checking if the manager names its namespace, if so that Application must be in it
		// otherwise a single Application of that name gives its namespace, and several leave it out as ambiguous
		if ns, app, ok := strings.Cut(p.Manager, "/"); ok {
			// looking for the Application in the named namespace, if its there all is well
			for _, n := range idx.argoApps[app] {
				// checking if the Application is in that namespace, if so the tracking id is fine
				if n == ns {
					return
				}
			}
		} else if nss := idx.argoApps[p.Manager]; len(nss) == 1 {
			p.Manager = nss[0] + "/" + p.Manager
			return
		} else if len(nss) > 1 {
			return
		}
		missing = "Argo CD Application " + p.Manager
	case snapshotv1alpha1.Source_SOURCE_FLUX:
		// checking if Flux's objects cant be seen, if so theres nothing to check against
		if !idx.fluxSeen {
			return
		}
		group, kind := fluxKustomizeGroup, "Kustomization"
		// checking if the evidence is the HelmRelease label, if so its a HelmRelease to look for
		if _, ok := p.Evidence[fluxHelmName]; ok {
			group, kind = fluxHelmGroup, "HelmRelease"
		}
		// checking if the Flux object was collected, if so all is well
		if idx.flux[group+"/"+p.Manager] {
			return
		}
		missing = "Flux " + kind + " " + p.Manager
	case snapshotv1alpha1.Source_SOURCE_HELM:
		ns, _, ok := strings.Cut(p.Manager, "/")
		// checking if theres no release namespace, no Secrets were read there, or the release record was found, in each case theres nothing to flag
		if !ok || !idx.secretNamespaces[ns] || idx.helm[p.Manager] {
			return
		}
		missing = "Helm release " + p.Manager
	default:
		return
	}
	// invoking AddFinding to warn that the manager is gone, its defined in pkg/resource/resource.go
	o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_WARNING, ManagerNotFound,
		"carries the markers of %s, which is not in the cluster; it was probably deleted without deleting this object, so nothing manages it now",
		missing)
}

// join writes a namespace and name as namespace/name, or only the name when theres no namespace
func join(ns, name string) string {
	// checking if theres no namespace, if so the name stands alone
	if ns == "" {
		return name
	}
	return ns + "/" + name
}

// customGroup tells if an API group is a custom resource group, Kubernetes' own groups have no dot, like apps and batch, or end in .k8s.io
func customGroup(group string) bool {
	return strings.Contains(group, ".") && !strings.HasSuffix(group, ".k8s.io")
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
