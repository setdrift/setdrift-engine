// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Package lifecycle drops what is finished or temporary from the export:
// objects being deleted, terminated Pods, and Helm and Argo CD hooks. It
// keeps finished Jobs in the snapshot, as a record of what ran, and marks
// them so the emitter leaves them out of the repository.
package lifecycle

import (
	"context"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// hookAnnotations mark Helm and Argo CD hooks: objects a sync creates and
// runs, not configuration.
var hookAnnotations = []string{"helm.sh/hook", "argocd.argoproj.io/hook"}

// Filter drops what is finished or temporary, and marks the Jobs that ran
// once: kept in the snapshot, as a record of what ran, but not written to
// the export, as applying one runs it again.
type Filter struct{}

// RanOnce is the finding code of a standalone Job that has finished: the
// emitter leaves it out of the repository, and RESTORE.md lists it.
const RanOnce = "RAN_ONCE"

// Name implements resource.Stage.
func (Filter) Name() string { return "lifecycle" }

// skipCounts counts dropped objects per namespace.
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
		// a finished Job: kept for the record, never written (it would run again)
		case isJob && JobFinished(o):
			finished[ns]++
			o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, RanOnce,
				"finished Job (%s): kept in the snapshot, not written to the export, as applying it would run it again; RESTORE.md lists it to re-run by hand if a new cluster needs it",
				jobOutcome(o))
			out = append(out, o)
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
	findings = append(findings, finished.findings("FINISHED_JOBS_NOT_WRITTEN", "finished Job", "finished Jobs",
		"; kept in the snapshot as a record of what ran, as applying one runs it again; RESTORE.md lists them")...)
	findings = append(findings, hooks.findings("HOOKS_SKIPPED", "Helm or Argo CD hook (Pod, Job or Workflow)", "Helm or Argo CD hooks (Pods, Jobs and Workflows)", "")...)
	return out, findings, nil
}

// findings reports the counts, one finding per namespace.
func (c skipCounts) findings(code, singular, plural, hint string) []*snapshotv1alpha1.Finding {
	namespaces := make([]string, 0, len(c))
	for ns := range c {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	out := make([]*snapshotv1alpha1.Finding, 0, len(namespaces))
	for _, ns := range namespaces {
		out = append(out, resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, code,
			"%s %s %s not exported%s", text.Plural(c[ns], singular, plural), where(ns), text.Word(c[ns], "was", "were"), hint))
	}
	return out
}

// where says where objects are: in a namespace, or at cluster scope.
func where(ns string) string {
	if ns == "" {
		return "at cluster scope"
	}
	return fmt.Sprintf("in namespace %q", ns)
}

// IsHook reports whether o carries a Helm or Argo CD hook annotation.
func IsHook(o *resource.Object) bool {
	ann := o.GetAnnotations()
	for _, a := range hookAnnotations {
		if _, ok := ann[a]; ok {
			return true
		}
	}
	return false
}

// beingDeleted reports whether deletion has started; an object can stay so
// for a long time if a finalizer never completes.
func beingDeleted(o *resource.Object) bool {
	return o.GetDeletionTimestamp() != nil
}

// podTerminated reports whether a Pod has Succeeded or Failed.
func podTerminated(o *resource.Object) bool {
	phase, _, _ := unstructured.NestedString(o.Object, "status", "phase")
	return phase == "Succeeded" || phase == "Failed"
}

// JobResult is a finished Job's outcome, "Complete" or "Failed", from its
// conditions, and whether it has finished at all. SuccessCriteriaMet and
// FailureTarget (Kubernetes 1.31+) come first, while the Job's Pods are
// still being terminated.
func JobResult(o *resource.Object) (result string, finished bool) {
	conds, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["status"] != "True" {
			continue
		}
		switch m["type"] {
		case "Failed", "FailureTarget":
			return "Failed", true
		case "Complete", "SuccessCriteriaMet":
			return "Complete", true
		}
	}
	return "", false
}

// JobFinished reports whether a Job has finished, completed or failed.
func JobFinished(o *resource.Object) bool {
	_, finished := JobResult(o)
	return finished
}

// jobOutcome is JobResult's result, for a message: "Complete", "Failed".
func jobOutcome(o *resource.Object) string {
	result, _ := JobResult(o)
	return result
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
