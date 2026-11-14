// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// drop event based objects

package lifecycle

import (
	"context"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// annotations added to signify ad hoc events, helm and argo common annotations
var hookAnnotations = []string{"helm.sh/hook", "argocd.argoproj.io/hook"}

// flag to keep finished jobs, default dropped
type Filter struct {
	// IncludeFinishedJobs exports Jobs that have completed or failed.
	IncludeFinishedJobs bool
}

// Name implements resource.Stage.
func (Filter) Name() string { return "lifecycle" }

// counts dropped objects per namespace
type skipCounts map[string]int

// Process implements resource.Stage.
func (f Filter) Process(_ context.Context, objs []*resource.Object) ([]*resource.Object, []*snapshotv1alpha1.Finding, error) {
	deleting, terminated, finished, hooks := skipCounts{}, skipCounts{}, skipCounts{}, skipCounts{}

	// namespace controller deletes everything in a Namespace being deleted, including objects that don't have a deletionTimestamp yet
	deletingNS := map[string]bool{}
	for _, o := range objs {
		if gk := o.GroupVersionKind().GroupKind(); gk.Group == "" && gk.Kind == "Namespace" && beingDeleted(o) {
			deletingNS[o.GetName()] = true
		}
	}

	out := make([]*resource.Object, 0, len(objs))
	for _, o := range objs {
		gk := o.GroupVersionKind().GroupKind()
		isPod := gk.Group == "" && gk.Kind == "Pod"
		isJob := gk.Group == "batch" && gk.Kind == "Job"
		// Argo CD hooks are often Argo Workflows, which run like Jobs.
		runs := isPod || isJob || (gk.Group == "argoproj.io" && gk.Kind == "Workflow")
		ns := o.GetNamespace()

		switch {
		// drop objects with a deletionTimestamp
		case beingDeleted(o) || deletingNS[ns]:
			deleting[ns]++
		// drop objects with a hook annotation
		case runs && IsHook(o):
			hooks[ns]++
		// drop pods with failed or succeeded status
		case isPod && podTerminated(o):
			terminated[ns]++
		// drop finished jobs if flag is not set
		case isJob && jobFinished(o) && !f.IncludeFinishedJobs:
			finished[ns]++
		default:
			// add warning to pods with no controllers, different than pods with missing controller
			if isPod && metav1.GetControllerOfNoCopy(o) == nil {
				o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_WARNING, "BARE_POD",
					"pod has no controller and will not be recreated if deleted; consider a Deployment or Job")
			}
			runOnceWarning(o, isPod, isJob)
			// Other hook resources (ConfigMaps, RBAC) are kept: they can
			// outlive the hook, but the hook recreates them on each sync.
			if !runs && IsHook(o) {
				o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "HOOK_RESOURCE",
					"created by a Helm or Argo CD hook, which recreates it on each sync; keep it in the chart or app rather than committing it separately")
			}
			out = append(out, o)
		}
	}
	// generate findings
	var findings []*snapshotv1alpha1.Finding
	findings = append(findings, deleting.findings("DELETING_SKIPPED", "object being deleted (or in a Namespace being deleted)", "objects being deleted (or in a Namespace being deleted)", "")...)
	findings = append(findings, terminated.findings("TERMINATED_PODS_SKIPPED", "terminated Pod (Succeeded or Failed)", "terminated Pods (Succeeded or Failed)", "")...)
	findings = append(findings, finished.findings("FINISHED_JOBS_SKIPPED", "finished Job", "finished Jobs", "; use --include-finished-jobs to export them")...)
	findings = append(findings, hooks.findings("HOOKS_SKIPPED", "Helm or Argo CD hook (Pod, Job or Workflow)", "Helm or Argo CD hooks (Pods, Jobs and Workflows)", "")...)
	return out, findings, nil
}

// aggregate skip counts from namespace
func (c skipCounts) findings(code, singular, plural, hint string) []*snapshotv1alpha1.Finding {
	namespaces := make([]string, 0, len(c))
	for ns := range c {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	out := make([]*snapshotv1alpha1.Finding, 0, len(namespaces))
	for _, ns := range namespaces {
		out = append(out, resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, code,
			"%s %s %s not exported%s", resource.Plural(c[ns], singular, plural), where(ns), resource.Verb(c[ns]), hint))
	}
	return out
}

// if non-namespaced obbject, return cluster scope
func where(ns string) string {
	if ns == "" {
		return "at cluster scope"
	}
	return fmt.Sprintf("in namespace %q", ns)
}

// checks if object has hook annotation as defined above
func IsHook(o *resource.Object) bool {
	ann := o.GetAnnotations()
	for _, a := range hookAnnotations {
		if _, ok := ann[a]; ok {
			return true
		}
	}
	return false
}

// reports whether deletion has started, can exist for a long time if finalizer never completes
func beingDeleted(o *resource.Object) bool {
	return o.GetDeletionTimestamp() != nil
}

// check pod status for succeeded or failed, signifies not running
func podTerminated(o *resource.Object) bool {
	phase, _, _ := unstructured.NestedString(o.Object, "status", "phase")
	return phase == "Succeeded" || phase == "Failed"
}

// check job for completion status
func jobFinished(o *resource.Object) bool {
	conds, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		// SuccessCriteriaMet and FailureTarget (Kubernetes 1.31+) come
		// first, while the Job's Pods are still being terminated.
		switch m["type"] {
		case "Complete", "Failed", "SuccessCriteriaMet", "FailureTarget":
			if m["status"] == "True" {
				return true
			}
		}
	}
	return false
}

// runOnceWarning flags exported objects that run once. Committed to Git, a
// GitOps tool recreates them whenever they are deleted, and they run again:
// a Job with ttlSecondsAfterFinished re-runs every time the TTL deletes it.
func runOnceWarning(o *resource.Object, isPod, isJob bool) {
	const hint = "consider a CronJob, or a sync hook (argocd.argoproj.io/hook or helm.sh/hook) so it runs on deploy"
	switch {
	case isJob:
		if ttl, found, _ := unstructured.NestedInt64(o.Object, "spec", "ttlSecondsAfterFinished"); found {
			o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_WARNING, "RUN_ONCE_EXPORTED",
				"Job is deleted %ds after it finishes; committed to Git, a GitOps tool then recreates it and it runs again, repeatedly; %s", ttl, hint)
			return
		}
		o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_WARNING, "RUN_ONCE_EXPORTED",
			"Job runs once; committed to Git, a GitOps tool recreates it whenever it is deleted, and it runs again; %s", hint)
	case isPod && metav1.GetControllerOfNoCopy(o) == nil:
		if policy, _, _ := unstructured.NestedString(o.Object, "spec", "restartPolicy"); policy == "Never" || policy == "OnFailure" {
			o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_WARNING, "RUN_ONCE_EXPORTED",
				"Pod runs once (restartPolicy %s); committed to Git, a GitOps tool recreates it whenever it is deleted, and it runs again; %s", policy, hint)
		}
	}
}
