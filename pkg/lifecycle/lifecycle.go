// Package lifecycle drops what is finished or temporary from the export, objects being deleted, terminated Pods, and Helm and Argo CD hooks
// it keeps finished Jobs in the snapshot as a record of what ran, and marks them so the emitter leaves them out of the repository
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

// hookAnnotations are the annotations that mark Helm and Argo CD hooks, objects a sync creates and runs, not configuration
var hookAnnotations = []string{"helm.sh/hook", "argocd.argoproj.io/hook"}

// Filter is the pipeline stage that drops what is finished or temporary, and marks the Jobs that ran once
// those are kept in the snapshot as a record of what ran, but not written to the export, since applying one runs it again
type Filter struct{}

// RanOnce is the finding code of a standalone Job that has finished, the emitter leaves it out of the repository and RESTORE.md lists it
const RanOnce = "RAN_ONCE"

// Name gives back the stage's name, for resource.Stage
func (Filter) Name() string { return "lifecycle" }

// skipCounts counts the dropped objects per namespace, the empty namespace is cluster scope
type skipCounts map[string]int

// Process takes the collected objects and gives back the ones to keep, with one finding per namespace for each kind of object dropped, for resource.Stage
func (f Filter) Process(_ context.Context, objs []*resource.Object) ([]*resource.Object, []*snapshotv1alpha1.Finding, error) {
	deleting, terminated, finished, hooks := skipCounts{}, skipCounts{}, skipCounts{}, skipCounts{}

	// the namespace controller deletes everything in a Namespace being deleted, including objects that dont have a deletionTimestamp yet
	deletingNS := map[string]bool{}
	// going over the objects to find the Namespaces being deleted
	for _, o := range objs {
		// checking if its a core Namespace whose deletion has started, invoking beingDeleted for that, its defined below
		if gk := o.GroupVersionKind().GroupKind(); gk.Group == "" && gk.Kind == "Namespace" && beingDeleted(o) {
			deletingNS[o.GetName()] = true
		}
	}

	out := make([]*resource.Object, 0, len(objs))
	// going over each object to decide if it is dropped, kept with a mark, or kept
	for _, o := range objs {
		gk := o.GroupVersionKind().GroupKind()
		isPod := gk.Group == "" && gk.Kind == "Pod"
		isJob := gk.Group == "batch" && gk.Kind == "Job"
		// Argo CD hooks are often Argo Workflows, which run like Jobs
		runs := isPod || isJob || (gk.Group == "argoproj.io" && gk.Kind == "Workflow")
		ns := o.GetNamespace()

		// checking which case the object falls in, the first that matches wins
		switch {
		// checking if the object or its namespace is being deleted, if so its dropped, invoking beingDeleted, its defined below
		case beingDeleted(o) || deletingNS[ns]:
			deleting[ns]++
		// checking if something that runs carries a hook annotation, if so its dropped, invoking IsHook, its defined below
		case runs && IsHook(o):
			hooks[ns]++
		// checking if its a Pod that has Succeeded or Failed, if so its dropped, invoking podTerminated, its defined below
		case isPod && podTerminated(o):
			terminated[ns]++
		// checking if its a finished Job, if so its kept for the record but never written since it would run again, invoking JobFinished, its defined below
		case isJob && JobFinished(o):
			finished[ns]++
			// invoking AddFinding to mark it RanOnce, so the emitter leaves it out, its defined in pkg/resource/resource.go
			// invoking jobOutcome to say how it ended, its defined below
			o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, RanOnce,
				"finished Job (%s): kept in the snapshot, not written to the export, as applying it would run it again; RESTORE.md lists it to re-run by hand if a new cluster needs it",
				jobOutcome(o))
			out = append(out, o)
		default:
			// checking if its a Pod with no controller at all, that gets a warning, its a different case from a Pod whose controller is missing
			if isPod && metav1.GetControllerOfNoCopy(o) == nil {
				// invoking AddFinding to attach the warning, its defined in pkg/resource/resource.go
				o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_WARNING, "BARE_POD",
					"pod has no controller and will not be recreated if deleted; consider a Deployment or Job")
			}
			// invoking runOnceWarning to warn about a Job or bare Pod that would run again from Git, its defined below
			runOnceWarning(o, isPod, isJob)
			// checking if its a hook resource that doesnt run, like a ConfigMap or RBAC, those are kept
			// they can outlive the hook, but the hook recreates them on each sync
			if !runs && IsHook(o) {
				// invoking AddFinding to note that, its defined in pkg/resource/resource.go
				o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "HOOK_RESOURCE",
					"created by a Helm or Argo CD hook, which recreates it on each sync; keep it in the chart or app rather than committing it separately")
			}
			out = append(out, o)
		}
	}
	// invoking findings on each count to turn it into one finding per namespace, its defined below
	var findings []*snapshotv1alpha1.Finding
	findings = append(findings, deleting.findings("DELETING_SKIPPED", "object being deleted (or in a Namespace being deleted)", "objects being deleted (or in a Namespace being deleted)", "")...)
	findings = append(findings, terminated.findings("TERMINATED_PODS_SKIPPED", "terminated Pod (Succeeded or Failed)", "terminated Pods (Succeeded or Failed)", "")...)
	findings = append(findings, finished.findings("FINISHED_JOBS_NOT_WRITTEN", "finished Job", "finished Jobs",
		"; kept in the snapshot as a record of what ran, as applying one runs it again; RESTORE.md lists them")...)
	findings = append(findings, hooks.findings("HOOKS_SKIPPED", "Helm or Argo CD hook (Pod, Job or Workflow)", "Helm or Argo CD hooks (Pods, Jobs and Workflows)", "")...)
	return out, findings, nil
}

// findings takes a finding code, the nouns for one and many, and a hint to append, and reports the counts as one finding per namespace, sorted by namespace
func (c skipCounts) findings(code, singular, plural, hint string) []*snapshotv1alpha1.Finding {
	namespaces := make([]string, 0, len(c))
	// collecting the namespaces so the findings come out in a stable order
	for ns := range c {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	out := make([]*snapshotv1alpha1.Finding, 0, len(namespaces))
	// going over each namespace and building its finding
	for _, ns := range namespaces {
		// invoking resource.NewFinding to build it, its defined in pkg/resource/resource.go
		// invoking text.Plural and text.Word for the count and the verb, theyre defined in internal/text/text.go, and where for the place, its defined below
		out = append(out, resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, code,
			"%s %s %s not exported%s", text.Plural(c[ns], singular, plural), where(ns), text.Word(c[ns], "was", "were"), hint))
	}
	return out
}

// where takes a namespace and says where objects are, in that namespace, or at cluster scope when its empty
func where(ns string) string {
	// checking if theres no namespace, if so the objects are cluster-scoped
	if ns == "" {
		return "at cluster scope"
	}
	return fmt.Sprintf("in namespace %q", ns)
}

// IsHook tells if o carries a Helm or Argo CD hook annotation
func IsHook(o *resource.Object) bool {
	ann := o.GetAnnotations()
	// going over each hook annotation
	for _, a := range hookAnnotations {
		// checking if the object has it, with any value
		if _, ok := ann[a]; ok {
			return true
		}
	}
	return false
}

// beingDeleted tells if the object's deletion has started
// an object can stay like that for a long time if a finalizer never completes
func beingDeleted(o *resource.Object) bool {
	return o.GetDeletionTimestamp() != nil
}

// podTerminated tells if a Pod has Succeeded or Failed, by its status phase
func podTerminated(o *resource.Object) bool {
	phase, _, _ := unstructured.NestedString(o.Object, "status", "phase")
	return phase == "Succeeded" || phase == "Failed"
}

// JobResult takes a Job and gives back its outcome from its conditions, "Complete" or "Failed", and whether it has finished at all
// SuccessCriteriaMet and FailureTarget, from Kubernetes 1.31 on, count too since they come first, while the Job's Pods are still being terminated
func JobResult(o *resource.Object) (result string, finished bool) {
	conds, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	// going over each condition looking for one that says the Job is done
	for _, c := range conds {
		m, ok := c.(map[string]any)
		// checking if the condition is malformed or not true, if so it says nothing
		if !ok || m["status"] != "True" {
			continue
		}
		// checking which condition it is, a failure or a success
		switch m["type"] {
		case "Failed", "FailureTarget":
			return "Failed", true
		case "Complete", "SuccessCriteriaMet":
			return "Complete", true
		}
	}
	return "", false
}

// JobFinished tells if a Job has finished, completed or failed
func JobFinished(o *resource.Object) bool {
	// invoking JobResult to read the Job's conditions, its defined above
	_, finished := JobResult(o)
	return finished
}

// jobOutcome gives back JobResult's result, "Complete" or "Failed", for a message
func jobOutcome(o *resource.Object) string {
	// invoking JobResult to read the Job's conditions, its defined above
	result, _ := JobResult(o)
	return result
}

// runOnceWarning takes an exported object and warns when it runs once, a Job or a Pod with no controller
// committed to Git, a GitOps tool recreates such an object whenever its deleted and it runs again
// a Job with ttlSecondsAfterFinished re-runs every time the TTL deletes it
func runOnceWarning(o *resource.Object, isPod, isJob bool) {
	const hint = "consider a CronJob, or a sync hook (argocd.argoproj.io/hook or helm.sh/hook) so it runs on deploy"
	// checking if its a Job, or a Pod with no controller
	switch {
	case isJob:
		// checking if the Job sets ttlSecondsAfterFinished, if so it gets deleted and recreated over and over, which the warning says
		if ttl, found, _ := unstructured.NestedInt64(o.Object, "spec", "ttlSecondsAfterFinished"); found {
			// invoking AddFinding to attach the warning, its defined in pkg/resource/resource.go
			o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_WARNING, "RUN_ONCE_EXPORTED",
				"Job is deleted %ds after it finishes; committed to Git, a GitOps tool then recreates it and it runs again, repeatedly; %s", ttl, hint)
			return
		}
		// invoking AddFinding to attach the warning, its defined in pkg/resource/resource.go
		o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_WARNING, "RUN_ONCE_EXPORTED",
			"Job runs once; committed to Git, a GitOps tool recreates it whenever it is deleted, and it runs again; %s", hint)
	case isPod && metav1.GetControllerOfNoCopy(o) == nil:
		// checking if the Pod's restartPolicy makes it run to an end, Never or OnFailure, an Always Pod keeps running instead
		if policy, _, _ := unstructured.NestedString(o.Object, "spec", "restartPolicy"); policy == "Never" || policy == "OnFailure" {
			// invoking AddFinding to attach the warning, its defined in pkg/resource/resource.go
			o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_WARNING, "RUN_ONCE_EXPORTED",
				"Pod runs once (restartPolicy %s); committed to Git, a GitOps tool recreates it whenever it is deleted, and it runs again; %s", policy, hint)
		}
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
