// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// classifies which system manages each object: Argo CD, Flux, Helm, an operator, or nothing (kubectl apply)
package provenance

import (
	"context"
	"slices"
	"sort"
	"strings"

	"setdrift.io/setdrift-engine/pkg/platform"
	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// labels and annotations that signal external system management
const (
	argoTrackingID = "argocd.argoproj.io/tracking-id"
	// DefaultArgoInstanceLabel unless argo sets instanceLabelKey explicitly
	DefaultArgoInstanceLabel = "app.kubernetes.io/instance"
	fluxKustomizeName        = "kustomize.toolkit.fluxcd.io/name"
	fluxKustomizeNS          = "kustomize.toolkit.fluxcd.io/namespace"
	fluxHelmName             = "helm.toolkit.fluxcd.io/name"
	fluxHelmNS               = "helm.toolkit.fluxcd.io/namespace"
	managedByLabel           = "app.kubernetes.io/managed-by"
	helmReleaseName          = "meta.helm.sh/release-name"
	helmReleaseNS            = "meta.helm.sh/release-namespace"
	confidenceKey            = "confidence"
	lowConfidence            = "low"
)

// group and kind of external systems
const (
	argoGroup          = "argoproj.io"
	fluxKustomizeGroup = "kustomize.toolkit.fluxcd.io"
	fluxHelmGroup      = "helm.toolkit.fluxcd.io"
)

// LabelClassifier classifies objects from the labels and annotations that GitOps
// tools stamp on what they apply. Checks run from strongest to weakest
// evidence, and tool-specific markers before Helm's: Flux and Argo CD also
// render Helm charts, so their objects can carry Helm markers too.
//
//  0. The Kubernetes platform (see platform): objects EKS creates and keeps
//     up to date, which a new cluster gets from EKS.
//  1. Flux labels (Kustomization, then HelmRelease).
//  2. The Argo CD tracking-id annotation, if it names this object. A tracking
//     id naming another object was copied (kubectl get | kubectl apply), and
//     Argo CD does not manage the copy.
//  3. The meta.helm.sh/release-* annotations, which only `helm install` and
//     `helm upgrade` write.
//  4. The Argo CD instance label (label tracking, the default before Argo CD
//     3.0). Low confidence: many charts set app.kubernetes.io/instance too.
//  5. The app.kubernetes.io/managed-by: Helm label alone. Low confidence:
//     chart templates set it, so it is also on charts that Argo CD rendered
//     with `helm template` and that Helm does not manage.
//
// The stage then checks that each manager exists in the collected objects
// (see checkManagers), and drops Helm's release records, which are Helm's
// state rather than configuration.
//
//  6. An owner reference to an object outside the export: an operator
//     created it.
//
// TODO: detect operator-managed objects from managedFields managers (needs
// real cluster dumps to learn the manager names), and resolve Argo
// Applications to their Git source.

// override default argo instance label
type LabelClassifier struct {
	ArgoInstanceLabel string
	// CIFieldManagers are the kubectl field managers of CI/CD pipelines
	// (kubectl apply --field-manager=github-actions): an object one of them
	// wrote, with no other manager, is CI/CD's, with low confidence.
	CIFieldManagers []string
	// Platform is the stage that classified the platform, which runs before
	// this one; its Result decides which name-based platform rules apply
	// (kind's add-ons). Nil, or an unsure result, applies only the rules
	// whose evidence names its platform (EKS's and kubeadm's field managers).
	Platform *platform.Stage
}

// Name implements resource.Stage
func (LabelClassifier) Name() string { return "provenance" }

// classifies provenance, prioritized by specificity e.g. an argo project that deploys a helm chart will be classified as argo
func (c LabelClassifier) Process(_ context.Context, objs []*resource.Object) ([]*resource.Object, []*snapshotv1alpha1.Finding, error) {
	idx := newManagerIndex(objs)
	out := make([]*resource.Object, 0, len(objs))
	releases := map[string]int{}
	// runs each object through classifier checks
	for _, o := range objs {
		if _, ok := helmReleaseRecord(o); ok {
			releases[o.GetNamespace()]++
			continue
		}
		o.Provenance = c.classify(o, c.platformResult())
		idx.check(o)
		o.Provenance = c.pipeline(o, o.Provenance)
		out = append(out, o)
	}
	// attach findings scoped to namespace if relvant
	var findings []*snapshotv1alpha1.Finding
	for _, ns := range sortedKeys(releases) {
		findings = append(findings, resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "HELM_RELEASE_RECORDS_SKIPPED",
			"%s in namespace %q %s not exported (Helm's release history; applying it would corrupt Helm's state)",
			resource.Plural(releases[ns], "Helm release record", "Helm release records"), ns, resource.Verb(releases[ns])))
	}
	return out, findings, nil
}

func (c LabelClassifier) platformResult() *platform.Classification {
	if c.Platform == nil {
		return nil
	}
	return c.Platform.Result
}

// Classify provenance from labels and annotations, attaches metadata to object
func (c LabelClassifier) Classify(o *resource.Object) *snapshotv1alpha1.Provenance {
	return c.pipeline(o, c.classify(o, c.platformResult()))
}

// The annotations a CI/CD pipeline stamps on what it deploys, so its
// objects are not taken for ones applied by hand. Only the repository is
// required. Values, e.g. in GitHub Actions:
//
//	setdrift.io/source-repo: $GITHUB_SERVER_URL/$GITHUB_REPOSITORY
//	setdrift.io/source-path: deploy/prod
//	setdrift.io/source-revision: $GITHUB_SHA
//	setdrift.io/deployed-by: $GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID
const (
	SourceRepoAnnotation     = "setdrift.io/source-repo"
	SourcePathAnnotation     = "setdrift.io/source-path"
	SourceRevisionAnnotation = "setdrift.io/source-revision"
	DeployedByAnnotation     = "setdrift.io/deployed-by"
)

// SourceAnnotations are the CI/CD source annotations.
var SourceAnnotations = []string{SourceRepoAnnotation, SourcePathAnnotation, SourceRevisionAnnotation, DeployedByAnnotation}

// pipeline applies a CI/CD pipeline's evidence, after the GitOps markers:
//   - the source annotations make an object no tool claims, or that only a
//     low-confidence marker does (helm template | kubectl apply leaves
//     Helm's label), CI/CD's, managed from its repository;
//   - on an object Argo CD, Flux, Helm, an operator or the platform manages,
//     they are kept as extra evidence: those markers are stronger;
//   - with neither, a write by one of CIFieldManagers makes it CI/CD's.
func (c LabelClassifier) pipeline(o *resource.Object, p *snapshotv1alpha1.Provenance) *snapshotv1alpha1.Provenance {
	ann := o.GetAnnotations()
	src := map[string]string{}
	for _, k := range SourceAnnotations {
		if v := ann[k]; v != "" {
			src[k] = v
		}
	}
	weak := p.GetSource() == snapshotv1alpha1.Source_SOURCE_UNMANAGED || p.GetSource() == snapshotv1alpha1.Source_SOURCE_UNSPECIFIED ||
		p.GetEvidence()[confidenceKey] == lowConfidence
	if repo := src[SourceRepoAnnotation]; repo != "" && weak {
		manager := repo
		if path := src[SourcePathAnnotation]; path != "" {
			manager += "//" + strings.TrimPrefix(path, "/")
		}
		return &snapshotv1alpha1.Provenance{Source: snapshotv1alpha1.Source_SOURCE_CI_CD, Manager: manager, Evidence: src}
	}
	if len(src) > 0 && !weak {
		ev := make(map[string]string, len(p.GetEvidence())+len(src))
		for k, v := range p.GetEvidence() {
			ev[k] = v
		}
		for k, v := range src {
			ev[k] = v
		}
		p.Evidence = ev
		return p
	}
	if p.GetSource() == snapshotv1alpha1.Source_SOURCE_UNMANAGED || p.GetSource() == snapshotv1alpha1.Source_SOURCE_UNSPECIFIED {
		for _, m := range o.GetManagedFields() {
			if m.Subresource != "status" && slices.Contains(c.CIFieldManagers, m.Manager) {
				ev := map[string]string{"fieldManager": m.Manager, confidenceKey: lowConfidence}
				for k, v := range src {
					ev[k] = v
				}
				return &snapshotv1alpha1.Provenance{Source: snapshotv1alpha1.Source_SOURCE_CI_CD, Evidence: ev}
			}
		}
	}
	return p
}

func (c LabelClassifier) classify(o *resource.Object, cls *platform.Classification) *snapshotv1alpha1.Provenance {
	labels, ann := o.GetLabels(), o.GetAnnotations()
	instanceLabel := c.ArgoInstanceLabel
	// use default argo identifier if none provided
	if instanceLabel == "" {
		instanceLabel = DefaultArgoInstanceLabel
	}
	p := func(src snapshotv1alpha1.Source, manager string, evidence map[string]string) *snapshotv1alpha1.Provenance {
		return &snapshotv1alpha1.Provenance{Source: src, Manager: manager, Evidence: evidence}
	}
	if manager, ev := platformObject(o, cls); ev != nil {
		return p(snapshotv1alpha1.Source_SOURCE_PLATFORM, manager, ev)
	}
	// kustomize name label for flux, namespace/name format
	if labels[fluxKustomizeName] != "" {
		return p(snapshotv1alpha1.Source_SOURCE_FLUX, join(labels[fluxKustomizeNS], labels[fluxKustomizeName]),
			map[string]string{fluxKustomizeName: labels[fluxKustomizeName]})
	}
	// helm name label for flux, namespace/name format
	if labels[fluxHelmName] != "" {
		return p(snapshotv1alpha1.Source_SOURCE_FLUX, join(labels[fluxHelmNS], labels[fluxHelmName]),
			map[string]string{fluxHelmName: labels[fluxHelmName]})
	}
	// argo tracking id
	if id := ann[argoTrackingID]; id != "" {
		if t, ok := parseTrackingID(id); ok && t.matches(o) {
			return p(snapshotv1alpha1.Source_SOURCE_ARGO_CD, t.app, map[string]string{argoTrackingID: id})
		}
		// attach finding if tracking id references a higher level object
		o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "ARGOCD_TRACKING_ID_MISMATCH",
			"the %s annotation %q names another object, so Argo CD does not manage this one; it was probably copied from that object",
			argoTrackingID, id)
	}
	// helm release name, namespace/name format
	if name := ann[helmReleaseName]; name != "" {
		ev := map[string]string{helmReleaseName: name}
		if v := labels[managedByLabel]; v != "" {
			ev[managedByLabel] = v
		}
		ns := ann[helmReleaseNS]
		if ns == "" {
			ns = o.GetNamespace()
		}
		return p(snapshotv1alpha1.Source_SOURCE_HELM, join(ns, name), ev)
	}
	// checks for legacy argo annotations
	if app := labels[instanceLabel]; app != "" {
		return p(snapshotv1alpha1.Source_SOURCE_ARGO_CD, argoApp(app),
			map[string]string{instanceLabel: app, confidenceKey: lowConfidence})
	}
	// always attributes low confidence if only helm
	if labels[managedByLabel] == "Helm" {
		return p(snapshotv1alpha1.Source_SOURCE_HELM, "",
			map[string]string{managedByLabel: "Helm", confidenceKey: lowConfidence})
	}

	// owned by an object that wasnt collected
	for _, r := range o.GetOwnerReferences() {
		if group, _, _ := strings.Cut(r.APIVersion, "/"); customGroup(group) {
			return p(snapshotv1alpha1.Source_SOURCE_OPERATOR, r.Kind+"/"+r.Name,
				map[string]string{"ownerReference": r.APIVersion + " " + r.Kind + " " + r.Name})
		}
	}
	return p(snapshotv1alpha1.Source_SOURCE_UNMANAGED, "", nil)
}

// Field managers of the components EKS runs: add-ons (eks), the control
// plane's own objects (eks-internal) and Auto Mode (eks-auto-mode).
var eksManagers = map[string]bool{"eks": true, "eks-internal": true, "eks-auto-mode": true}

// Managers of Kubernetes' own components, which write status and defaults
// into objects whoever created them.
var coreManagers = map[string]bool{
	"kube-apiserver": true, "kube-controller-manager": true, "api-priority-and-fairness-config-consumer-v1": true,
}

// eksBootstrap names objects EKS creates with kubectl when it creates a
// cluster, so their only field manager is kubectl-client-side-apply, as if
// a person had applied them. Names starting with "eks:" are EKS's too.
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

// CRD groups of the controllers EKS runs in its control plane (VPC
// resource controller, network policy controller, Auto Mode).
var eksCRDGroups = []string{"eks.amazonaws.com", "vpcresources.k8s.aws", "networking.k8s.aws"}

// platformObject returns who of the platform manages o (eks, kubeadm, kind)
// and the evidence, or nil evidence. A new cluster gets its own copy of these
// objects from its platform; exported, they would carry the old cluster's
// values (webhook CA bundles, endpoints, kubelet settings) and, applied to
// another cluster, overwrite its own.
//
// EKS's and kubeadm's own field managers name their platform, so those rules
// apply on any cluster. kind's add-ons were applied with kubectl and are known
// only by name: those rules apply only on a cluster classified as kind.
func platformObject(o *resource.Object, cls *platform.Classification) (string, map[string]string) {
	if ev := eks(o); ev != nil {
		return "eks", ev
	}
	if ev := kubeadm(o, cls); ev != nil {
		return "kubeadm", ev
	}
	if cls.Is(platform.Kind) {
		if ev := kind(o); ev != nil {
			return "kind", ev
		}
	}
	return "", nil
}

// kubeadm returns the evidence that kubeadm created o and nothing changed it
// since: its only field manager is kubeadm (besides Kubernetes' own). An
// object kubeadm created that someone changed (a CoreDNS Corefile edit) is
// the user's configuration: it is exported, with a finding saying so.
//
// On a cluster known to be built with kubeadm (kubeadm, kind, minikube), the
// service accounts kubeadm creates without a field manager count too.
func kubeadm(o *resource.Object, cls *platform.Classification) map[string]string {
	var managers, others []string
	for _, m := range o.GetManagedFields() {
		if coreManagers[m.Manager] {
			continue
		}
		managers = append(managers, m.Manager)
		if m.Manager != "kubeadm" {
			others = append(others, m.Manager)
		}
	}
	switch {
	case len(managers) > 0 && len(others) == 0:
		return map[string]string{"managedFields": "kubeadm"}
	case len(others) < len(managers):
		o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "PLATFORM_OBJECT_CHANGED",
			"was created by kubeadm and changed since by %s, so it is exported to keep the change; a cluster built with kubeadm creates its own",
			strings.Join(others, ", "))
		return nil
	}
	builtWithKubeadm := cls.Is(platform.Kubeadm) || cls.Is(platform.Kind) || cls.Is(platform.Minikube)
	if builtWithKubeadm && len(managers) == 0 && o.GetNamespace() == "kube-system" && o.GetKind() == "ServiceAccount" &&
		kubeadmServiceAccounts[o.GetName()] {
		return map[string]string{"name": o.GetName()}
	}
	return nil
}

// kubeadmServiceAccounts are created by kubeadm without a field manager.
var kubeadmServiceAccounts = map[string]bool{"coredns": true, "kube-proxy": true, "bootstrap-signer": true}

// kind returns the evidence that o is one of kind's own add-ons: the kindnet
// network plugin and the local-path storage provisioner, with its namespace
// and its "standard" StorageClass.
func kind(o *resource.Object) map[string]string {
	gk := o.GroupVersionKind().GroupKind()
	name, ns := o.GetName(), o.GetNamespace()
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

// eks returns the evidence that EKS manages o, or nil. EKS creates these
// objects in every cluster (or with an add-on) and keeps them up to date;
// exported, they would carry the old cluster's values (webhook CA bundles,
// endpoints) and some are rejected on apply.
func eks(o *resource.Object) map[string]string {
	if v := o.GetLabels()[managedByLabel]; strings.EqualFold(v, "eks") {
		return map[string]string{managedByLabel: v}
	}
	var managers []string
	onlyEKS := true
	for _, m := range o.GetManagedFields() {
		if coreManagers[m.Manager] {
			continue
		}
		managers = append(managers, m.Manager)
		onlyEKS = onlyEKS && eksManagers[m.Manager]
	}
	if len(managers) > 0 && onlyEKS {
		return map[string]string{"managedFields": strings.Join(managers, ",")}
	}

	gvk := o.GroupVersionKind()
	gk := gvk.Group + "/" + gvk.Kind
	name := o.GetName()
	switch {
	case strings.HasPrefix(gk, "rbac.authorization.k8s.io/") && strings.HasPrefix(name, "eks:"):
		return map[string]string{"name": name}
	case eksBootstrap[gk][name]:
		return map[string]string{"name": name}
	case gk == "storage.k8s.io/StorageClass" && name == "gp2" && o.Object["provisioner"] == "kubernetes.io/aws-ebs":
		// Created by EKS in every cluster, with the in-tree provisioner.
		return map[string]string{"name": name}
	case gk == "apiextensions.k8s.io/CustomResourceDefinition":
		_, group, _ := strings.Cut(name, ".")
		for _, g := range eksCRDGroups {
			if group == g {
				return map[string]string{"group": group}
			}
		}
	}
	return nil
}

// trackingID is a parsed argo tracking id, format app:group/kind:namespace/name
type trackingID struct {
	app                   string
	group, kind, ns, name string
}

// parse format of argo tracking ids
func parseTrackingID(s string) (trackingID, bool) {
	app, rest, ok := strings.Cut(s, ":")
	if !ok || app == "" {
		return trackingID{}, false
	}
	gk, nsName, ok := strings.Cut(rest, ":")
	if !ok {
		return trackingID{}, false
	}
	group, kind, ok := strings.Cut(gk, "/")
	if !ok || kind == "" {
		return trackingID{}, false
	}
	ns, name, ok := strings.Cut(nsName, "/")
	if !ok || name == "" {
		return trackingID{}, false
	}
	return trackingID{app: argoApp(app), group: group, kind: kind, ns: ns, name: name}, true
}

// verify tracking id names this object, doesnt apply to cluster scoped objects
func (t trackingID) matches(o *resource.Object) bool {
	gvk := o.GroupVersionKind()
	if t.group != gvk.Group || t.kind != gvk.Kind || t.name != o.GetName() {
		return false
	}
	return t.ns == "" || o.GetNamespace() == "" || t.ns == o.GetNamespace()
}

// convert argo app instance name to namespace/app format
func argoApp(s string) string {
	if ns, app, ok := strings.Cut(s, "_"); ok && ns != "" && app != "" {
		return ns + "/" + app
	}
	return s
}

// helmReleaseRecord reports whether o is one of the records Helm keeps per
// release revision: a Secret of type helm.sh/release.v1 (the default
// driver) or a ConfigMap labelled owner=helm (HELM_DRIVER=configmap).
// checks whether object is a helm release related object
func helmReleaseRecord(o *resource.Object) (string, bool) {
	gvk := o.GroupVersionKind()
	// check if object is a release revision
	if gvk.Group != "" || !strings.HasPrefix(o.GetName(), "sh.helm.release.v1.") {
		return "", false
	}
	switch gvk.Kind {
	// check if release secret
	case "Secret":
		if t, _ := o.Object["type"].(string); t != "helm.sh/release.v1" {
			return "", false
		}
	// check if release configmap
	case "ConfigMap":
		if o.GetLabels()["owner"] != "helm" {
			return "", false
		}
	default:
		return "", false
	}
	return o.GetLabels()["name"], true
}

// holds managers found in collected objects, so they can be identified even if deleted while running
type managerIndex struct {
	// argoApps maps app name to namespace
	argoApps map[string][]string
	// flux holds group/namespace/name
	flux map[string]bool
	// helm holds namespace/release
	helm map[string]bool

	// Whether the manager's objects can be seen at all e.g. argo running in a remote cluster
	argoSeen, fluxSeen bool
	secretNamespaces   map[string]bool
}

// generate manager index and flags manager recognition
func newManagerIndex(objs []*resource.Object) *managerIndex {
	idx := &managerIndex{
		argoApps:         map[string][]string{},
		flux:             map[string]bool{},
		helm:             map[string]bool{},
		secretNamespaces: map[string]bool{},
	}
	for _, o := range objs {
		gvk := o.GroupVersionKind()
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
		if name, ok := helmReleaseRecord(o); ok && name != "" {
			idx.helm[o.GetNamespace()+"/"+name] = true
		}
	}
	return idx
}

// check fills in the app namespace when it can, and flags when manager isnt in the cluster
func (idx *managerIndex) check(o *resource.Object) {
	p := o.Provenance
	missing := ""
	switch p.Source {
	case snapshotv1alpha1.Source_SOURCE_ARGO_CD:
		if !idx.argoSeen || p.Manager == "" {
			return
		}
		if p.Evidence[confidenceKey] == lowConfidence && !idx.hasArgoApp(p.Manager) {
			// Only the instance label, which many charts and installers
			// set too (Flux's own objects say flux-system), and Argo CD
			// runs here without such an Application: not Argo CD's.
			o.Provenance = &snapshotv1alpha1.Provenance{Source: snapshotv1alpha1.Source_SOURCE_UNMANAGED, Evidence: p.Evidence}
			return
		}
		if ns, app, ok := strings.Cut(p.Manager, "/"); ok {
			for _, n := range idx.argoApps[app] {
				if n == ns {
					return
				}
			}
		} else if nss := idx.argoApps[p.Manager]; len(nss) == 1 {
			p.Manager = nss[0] + "/" + p.Manager
			return
		} else if len(nss) > 1 {
			return // ambiguous; leave the namespace out
		}
		missing = "Argo CD Application " + p.Manager
	case snapshotv1alpha1.Source_SOURCE_FLUX:
		if !idx.fluxSeen {
			return
		}
		group, kind := fluxKustomizeGroup, "Kustomization"
		if _, ok := p.Evidence[fluxHelmName]; ok {
			group, kind = fluxHelmGroup, "HelmRelease"
		}
		if idx.flux[group+"/"+p.Manager] {
			return
		}
		missing = "Flux " + kind + " " + p.Manager
	case snapshotv1alpha1.Source_SOURCE_HELM:
		ns, _, ok := strings.Cut(p.Manager, "/")
		if !ok || !idx.secretNamespaces[ns] || idx.helm[p.Manager] {
			return
		}
		missing = "Helm release " + p.Manager
	default:
		return
	}
	o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_WARNING, "MANAGER_NOT_FOUND",
		"carries the markers of %s, which is not in the cluster; it was probably deleted without deleting this object, so nothing manages it now",
		missing)
}

// hasArgoApp reports whether an Argo CD Application named by manager
// (name or namespace/name) was collected.
func (idx *managerIndex) hasArgoApp(manager string) bool {
	if ns, app, ok := strings.Cut(manager, "/"); ok {
		return slices.Contains(idx.argoApps[app], ns)
	}
	return len(idx.argoApps[manager]) > 0
}

// return concatenated namespace/name format
func join(ns, name string) string {
	if ns == "" {
		return name
	}
	return ns + "/" + name
}

// create sorted array
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// customGroup reports whether an API group is a custom resource group, k8s own groups have no dot (apps, batch) or end in .k8s.io
func customGroup(group string) bool {
	return strings.Contains(group, ".") && !strings.HasSuffix(group, ".k8s.io")
}
