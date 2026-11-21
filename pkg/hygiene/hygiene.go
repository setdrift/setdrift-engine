// Package hygiene reports what a cluster keeps that it no longer needs, terminated Pods, finished Jobs with no TTL, long CronJob histories and deletions stuck on finalizers
// it changes nothing, and runs before the ownership stage drops the children it counts
package hygiene

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/lifecycle"
	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// Kubernetes' defaults for the CronJob history limits, a CronJob keeping more than these gets a finding
const (
	defaultSuccessfulJobsHistoryLimit = 3
	defaultFailedJobsHistoryLimit     = 1
)

// maxNames caps how many object names a finding lists
const maxNames = 5

// Reporter is the hygiene stage, for resource.Stage, it only adds findings and passes every object through
type Reporter struct{}

// Name gives back the stage's name, for resource.Stage
func (Reporter) Name() string { return "hygiene" }

// stuckAfter is how long a deletion must have been waiting before its reported as stuck
const stuckAfter = time.Hour

// podCause is why a terminated Pod is still in the cluster, going by what owns it
type podCause int

// the causes, in the order a finding lists them
const (
	// a CronJob's history limit keeps their Jobs
	keptByCronJob podCause = iota
	// a finished Job without ttlSecondsAfterFinished
	keptByJobNoTTL
	// no owner, only deleted by hand
	keptBare
	// a ReplicaSet, StatefulSet and the like, the Pod was evicted or shut down
	keptByWorkload
	// the owner was not collected
	keptOther
	// a TTL or a running Job will remove them
	cleanedUp
	// the number of causes, to size podCounts.byCause
	numCauses
)

// podCounts counts the terminated Pods of one namespace, by phase and by cause
type podCounts struct {
	succeeded, failed int
	// how many Pods each podCause accounts for
	byCause [numCauses]int
}

// Process takes the collected objects and gives them back unchanged, with findings about terminated Pods, Jobs without a TTL and stuck deletions, for resource.Stage
// CronJobs with long histories get their finding attached to the object itself
func (Reporter) Process(ctx context.Context, objs []*resource.Object) ([]*resource.Object, []*snapshotv1alpha1.Finding, error) {
	pods := map[string]*podCounts{}
	noTTL := map[string][]string{}
	jobs := map[types.UID]*resource.Object{}
	// going over the objects to index the Jobs by uid, so a Pod's owner can be looked up
	for _, o := range objs {
		// checking if its a Job
		if gk := o.GroupVersionKind().GroupKind(); gk.Group == "batch" && gk.Kind == "Job" {
			jobs[o.GetUID()] = o
		}
	}
	var findings []*snapshotv1alpha1.Finding
	// invoking resource.CaptureTime and checking if the run has a capture time, its defined in pkg/resource/resource.go
	// without one theres no telling how long a deletion has waited, so stuck deletions are only reported with it
	if now, ok := resource.CaptureTime(ctx); ok {
		// invoking stuckDeletions to report deletions waiting on finalizers, its defined below
		findings = append(findings, stuckDeletions(objs, now)...)
	}

	// going over each object, counting terminated Pods, Jobs without a TTL and checking CronJob histories
	for _, o := range objs {
		gk := o.GroupVersionKind().GroupKind()
		ns := o.GetNamespace()
		// checking what kind the object is
		switch {
		// checking if its a Pod, those that Succeeded or Failed are counted
		case gk.Group == "" && gk.Kind == "Pod":
			phase, _, _ := unstructured.NestedString(o.Object, "status", "phase")
			// checking if the Pod has terminated, if not it isnt counted
			if phase != "Succeeded" && phase != "Failed" {
				continue
			}
			// checking if this is the namespace's first terminated Pod, if so its counts start here
			if pods[ns] == nil {
				pods[ns] = &podCounts{}
			}
			// checking which way it ended
			if phase == "Succeeded" {
				pods[ns].succeeded++
			} else {
				pods[ns].failed++
			}
			// invoking causeOf to tell why the Pod is still around, its defined below
			pods[ns].byCause[causeOf(o, jobs)]++
		// checking if its a Job, a standalone one without ttlSecondsAfterFinished is kept until deleted by hand
		case gk.Group == "batch" && gk.Kind == "Job":
			// checking if the Job is partial, has a controller, or is a hook, invoking lifecycle.IsHook, its defined in pkg/lifecycle/lifecycle.go
			// those arent reported, their owner or the sync takes care of them
			if o.Partial || metav1.GetControllerOfNoCopy(o) != nil || lifecycle.IsHook(o) {
				continue
			}
			// checking if the Job has no ttlSecondsAfterFinished
			if _, found, _ := unstructured.NestedInt64(o.Object, "spec", "ttlSecondsAfterFinished"); !found {
				noTTL[ns] = append(noTTL[ns], o.GetName())
			}
		// checking if its a CronJob, its history limits are compared against the defaults
		case gk.Group == "batch" && gk.Kind == "CronJob":
			// invoking flagCronJobHistory to flag a history longer than the defaults, its defined below
			flagCronJobHistory(o)
		}
	}
	// two kinds of findings come out, per namespace (TERMINATED_PODS_TO_CLEANUP, JOBS_WITHOUT_TTL) and per object (CRONJOB_HISTORY_LIMIT, attached above)
	// going over the namespaces with terminated Pods, in order
	for _, ns := range slices.Sorted(maps.Keys(pods)) {
		// invoking terminatedPodsFinding and checking if it has something to report, its defined below
		if f := terminatedPodsFinding(ns, pods[ns]); f != nil {
			findings = append(findings, f)
		}
	}
	// going over the namespaces with Jobs without a TTL, in order
	for _, ns := range slices.Sorted(maps.Keys(noTTL)) {
		names := noTTL[ns]
		sort.Strings(names)
		// invoking resource.NewFinding to build the finding, its defined in pkg/resource/resource.go
		// invoking text.Plural and text.Word for the wording, theyre defined in internal/text/text.go, and nameList for the names, its defined below
		findings = append(findings, resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "JOBS_WITHOUT_TTL",
			"%s in namespace %q %s no ttlSecondsAfterFinished and %s kept after finishing until deleted by hand: %s",
			text.Plural(len(names), "Job", "Jobs"), ns, text.Word(len(names), "has", "have"), text.Word(len(names), "is", "are"), nameList(names)))
	}
	return objs, findings, nil
}

// causeOf takes a terminated Pod and the Jobs by uid and says why the Pod is still in the cluster, by following its controller
func causeOf(pod *resource.Object, jobs map[types.UID]*resource.Object) podCause {
	owner := metav1.GetControllerOfNoCopy(pod)
	// checking if the Pod has no controller, if so only deleting it by hand removes it
	if owner == nil {
		return keptBare
	}
	// checking if the controller is something other than a Job
	if owner.Kind != "Job" {
		// checking if its a Node, a static Pod's mirror, which we count as owned by something not collected
		if owner.Kind == "Node" {
			return keptOther
		}
		return keptByWorkload
	}
	job := jobs[owner.UID]
	// checking if the owning Job wasnt collected
	if job == nil {
		return keptOther
	}
	// checking if the Job has a controller of its own
	if jobOwner := metav1.GetControllerOfNoCopy(job); jobOwner != nil {
		// checking if that controller is a CronJob, if so its history limit keeps the Job and its Pods
		if jobOwner.Kind == "CronJob" {
			return keptByCronJob
		}
		return keptOther
	}
	// checking if the Job is partial, if so we dont know enough about it
	if job.Partial {
		return keptOther
	}
	// checking if the Job has a TTL or is still running, invoking lifecycle.JobFinished, its defined in pkg/lifecycle/lifecycle.go
	// either way its Pods will be removed with it
	if _, found, _ := unstructured.NestedInt64(job.Object, "spec", "ttlSecondsAfterFinished"); found || !lifecycle.JobFinished(job) {
		return cleanedUp
	}
	return keptByJobNoTTL
}

// causeText is the wording for each cause of kept Pods, in the order findings list them, cleanedUp is worded apart
var causeText = []struct {
	cause podCause
	text  string
}{
	{keptByCronJob, "%d kept by CronJob history limits (see CRONJOB_HISTORY_LIMIT)"},
	{keptByJobNoTTL, "%d kept by finished Jobs with no ttlSecondsAfterFinished (see JOBS_WITHOUT_TTL)"},
	{keptBare, "%d with no owner, which only deleting them removes"},
	{keptByWorkload, "%d from ReplicaSets, StatefulSets or DaemonSets (evicted or shut down), which the Pod garbage collector only removes once the cluster has 12,500 terminated Pods; delete them by hand"},
	{keptOther, "%d owned by objects not collected"},
}

// terminatedPodsFinding takes a namespace and its counts and reports the terminated Pods kept there, by cause, each with what removes them, nil when none are kept
// it counts every terminated Pod in the cluster, children included, so its larger than lifecycle's TERMINATED_PODS_SKIPPED, which counts only top-level Pods left out of the export
func terminatedPodsFinding(ns string, c *podCounts) *snapshotv1alpha1.Finding {
	kept := c.succeeded + c.failed - c.byCause[cleanedUp]
	// checking if every terminated Pod will be cleaned up anyway, if so theres nothing to report
	if kept == 0 {
		return nil
	}
	var parts []string
	// going over each cause in order
	for _, k := range causeText {
		// checking if any Pods have this cause, only those get a part
		if n := c.byCause[k.cause]; n > 0 {
			parts = append(parts, fmt.Sprintf(k.text, n))
		}
	}
	// invoking text.Plural and text.Word for the wording, theyre defined in internal/text/text.go
	msg := fmt.Sprintf("%s in namespace %q (%d Succeeded, %d Failed) %s kept in the cluster: %s",
		text.Plural(kept, "terminated Pod", "terminated Pods"), ns, c.succeeded, c.failed, text.Word(kept, "is", "are"), strings.Join(parts, "; "))
	// checking if some Pods will be removed with their Job, if so the message mentions them after the kept ones
	if n := c.byCause[cleanedUp]; n > 0 {
		msg += fmt.Sprintf(". %s more will be removed with %s Job (TTL set, or still running)", text.Plural(n, "Pod", "Pods"), text.Word(n, "its", "their"))
	}
	// invoking resource.NewFinding to build the finding, its defined in pkg/resource/resource.go
	return resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "TERMINATED_PODS_TO_CLEANUP", "%s", msg)
}

// stuckDeletions takes the objects and the capture time and reports those whose deletion started more than stuckAfter ago and is waiting on finalizers
// lifecycle drops these from the export, this only reports them
func stuckDeletions(objs []*resource.Object, now time.Time) []*snapshotv1alpha1.Finding {
	var out []*snapshotv1alpha1.Finding
	// going over each object
	for _, o := range objs {
		ts := o.GetDeletionTimestamp()
		fin := o.GetFinalizers()
		// checking if the object isnt being deleted, has no finalizers, or started deleting less than stuckAfter ago, if so its not stuck
		if ts == nil || len(fin) == 0 || now.Sub(ts.Time) < stuckAfter {
			continue
		}
		// a Namespace also waits on its spec.finalizers ("kubernetes"), which clear once its contents are gone
		// invoking resource.NewFinding to build the warning, its defined in pkg/resource/resource.go
		// invoking age for how long its waited, its defined below, and text.Word for the wording, its defined in internal/text/text.go
		out = append(out, resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_WARNING, "DELETION_STUCK",
			"%s has been deleting for %s, waiting on %s %s; the controller that added %s must be running to remove %s",
			o.Ref(), age(now.Sub(ts.Time)), text.Word(len(fin), "finalizer", "finalizers"), strings.Join(fin, ", "), text.Word(len(fin), "it", "them"), text.Word(len(fin), "it", "them")))
	}
	// sorting by message so the findings come out the same every run
	sort.Slice(out, func(i, j int) bool { return out[i].Message < out[j].Message })
	return out
}

// age takes a duration and words it roughly, in days from two days on, in hours from two hours on, otherwise in minutes
func age(d time.Duration) string {
	// checking how long it is to pick the unit
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	}
}

// flagCronJobHistory takes a CronJob and adds a finding to it when its history limits exceed the defaults, since each Job it keeps keeps its Pods too
func flagCronJobHistory(o *resource.Object) {
	success, foundS, _ := unstructured.NestedInt64(o.Object, "spec", "successfulJobsHistoryLimit")
	failed, foundF, _ := unstructured.NestedInt64(o.Object, "spec", "failedJobsHistoryLimit")
	// checking if the successful limit is unset, if so Kubernetes uses its default
	if !foundS {
		success = defaultSuccessfulJobsHistoryLimit
	}
	// checking if the failed limit is unset, if so Kubernetes uses its default
	if !foundF {
		failed = defaultFailedJobsHistoryLimit
	}
	// checking if both limits are within the defaults, if so theres nothing to flag
	if success <= defaultSuccessfulJobsHistoryLimit && failed <= defaultFailedJobsHistoryLimit {
		return
	}
	// invoking AddFinding to attach the finding to the CronJob, its defined in pkg/resource/resource.go
	o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "CRONJOB_HISTORY_LIMIT",
		"keeps %d successful and %d failed Jobs, and their Pods (defaults are %d and %d)",
		success, failed, defaultSuccessfulJobsHistoryLimit, defaultFailedJobsHistoryLimit)
}

// nameList takes names and joins at most maxNames of them, saying how many more there are
func nameList(names []string) string {
	// checking if they all fit
	if len(names) <= maxNames {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:maxNames], ", "), len(names)-maxNames)
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
