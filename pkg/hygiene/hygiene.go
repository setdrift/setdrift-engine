// Package hygiene reports what a cluster keeps that it no longer needs:
// terminated Pods, finished Jobs with no TTL, long CronJob histories and
// deletions stuck on finalizers. It changes nothing, and runs before the
// ownership stage drops the children it counts.
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

// Kubernetes' defaults for CronJob history limits.
const (
	defaultSuccessfulJobsHistoryLimit = 3
	defaultFailedJobsHistoryLimit     = 1
)

// maxNames caps how many object names a finding lists.
const maxNames = 5

// Reporter is the hygiene stage. It implements resource.Stage.
type Reporter struct{}

// Name implements resource.Stage.
func (Reporter) Name() string { return "hygiene" }

// stuckAfter is how long deletion must have been waiting before it is
// reported as stuck.
const stuckAfter = time.Hour

// podCause is why a terminated Pod is still in the cluster, by what owns it.
type podCause int

// The causes, in the order a finding lists them.
const (
	keptByCronJob  podCause = iota // a CronJob's history limit keeps their Jobs
	keptByJobNoTTL                 // a finished Job without ttlSecondsAfterFinished
	keptBare                       // no owner: only deleted by hand
	keptByWorkload                 // ReplicaSet, StatefulSet...: evicted or shut down
	keptOther                      // owner not collected
	cleanedUp                      // a TTL or a running Job will remove them
	numCauses
)

type podCounts struct {
	succeeded, failed int
	byCause           [numCauses]int
}

// Process implements resource.Stage.
func (Reporter) Process(ctx context.Context, objs []*resource.Object) ([]*resource.Object, []*snapshotv1alpha1.Finding, error) {
	pods := map[string]*podCounts{}
	noTTL := map[string][]string{}
	jobs := map[types.UID]*resource.Object{}
	for _, o := range objs {
		if gk := o.GroupVersionKind().GroupKind(); gk.Group == "batch" && gk.Kind == "Job" {
			jobs[o.GetUID()] = o
		}
	}
	var findings []*snapshotv1alpha1.Finding
	if now, ok := resource.CaptureTime(ctx); ok {
		findings = append(findings, stuckDeletions(objs, now)...)
	}

	for _, o := range objs {
		gk := o.GroupVersionKind().GroupKind()
		ns := o.GetNamespace()
		switch {
		// check pods for failed and succeeded status
		case gk.Group == "" && gk.Kind == "Pod":
			phase, _, _ := unstructured.NestedString(o.Object, "status", "phase")
			if phase != "Succeeded" && phase != "Failed" {
				continue
			}
			if pods[ns] == nil {
				pods[ns] = &podCounts{}
			}
			if phase == "Succeeded" {
				pods[ns].succeeded++
			} else {
				pods[ns].failed++
			}
			pods[ns].byCause[causeOf(o, jobs)]++
		// check jobs without a controller, missing ttlSecondsAfterFinished
		case gk.Group == "batch" && gk.Kind == "Job":
			if o.Partial || metav1.GetControllerOfNoCopy(o) != nil || lifecycle.IsHook(o) {
				continue
			}
			if _, found, _ := unstructured.NestedInt64(o.Object, "spec", "ttlSecondsAfterFinished"); !found {
				noTTL[ns] = append(noTTL[ns], o.GetName())
			}
		// check cronjob history against limits, defined below
		case gk.Group == "batch" && gk.Kind == "CronJob":
			flagCronJobHistory(o)
		}
	}
	// 2 Types of findings to report - per namespace (TERMINATED_PODS_TO_CLEANUP, JOBS_WITHOUT_TTL) and per object (CRONJOB_HISTORY_LIMIT)
	// terminated pods to cleanup
	for _, ns := range slices.Sorted(maps.Keys(pods)) {
		if f := terminatedPodsFinding(ns, pods[ns]); f != nil {
			findings = append(findings, f)
		}
	}
	// jobs without ttl
	for _, ns := range slices.Sorted(maps.Keys(noTTL)) {
		names := noTTL[ns]
		sort.Strings(names)
		findings = append(findings, resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "JOBS_WITHOUT_TTL",
			"%s in namespace %q %s no ttlSecondsAfterFinished and %s kept after finishing until deleted by hand: %s",
			text.Plural(len(names), "Job", "Jobs"), ns, text.Word(len(names), "has", "have"), text.Word(len(names), "is", "are"), nameList(names)))
	}
	return objs, findings, nil
}

// causeOf says why a terminated Pod is still in the cluster.
func causeOf(pod *resource.Object, jobs map[types.UID]*resource.Object) podCause {
	owner := metav1.GetControllerOfNoCopy(pod)
	if owner == nil {
		return keptBare
	}
	if owner.Kind != "Job" {
		if owner.Kind == "Node" {
			return keptOther
		}
		return keptByWorkload
	}
	job := jobs[owner.UID]
	if job == nil {
		return keptOther
	}
	if jobOwner := metav1.GetControllerOfNoCopy(job); jobOwner != nil {
		if jobOwner.Kind == "CronJob" {
			return keptByCronJob
		}
		return keptOther
	}
	if job.Partial {
		return keptOther
	}
	if _, found, _ := unstructured.NestedInt64(job.Object, "spec", "ttlSecondsAfterFinished"); found || !lifecycle.JobFinished(job) {
		return cleanedUp
	}
	return keptByJobNoTTL
}

// causeText words each cause of kept Pods, in the order findings list them.
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

// terminatedPodsFinding reports the terminated Pods in one namespace, by
// cause, each with what removes them. It counts every terminated Pod in the
// cluster, children included, so it is larger than lifecycle's
// TERMINATED_PODS_SKIPPED, which counts only top-level Pods left out of the
// export.
func terminatedPodsFinding(ns string, c *podCounts) *snapshotv1alpha1.Finding {
	kept := c.succeeded + c.failed - c.byCause[cleanedUp]
	if kept == 0 {
		return nil
	}
	var parts []string
	for _, k := range causeText {
		if n := c.byCause[k.cause]; n > 0 {
			parts = append(parts, fmt.Sprintf(k.text, n))
		}
	}
	msg := fmt.Sprintf("%s in namespace %q (%d Succeeded, %d Failed) %s kept in the cluster: %s",
		text.Plural(kept, "terminated Pod", "terminated Pods"), ns, c.succeeded, c.failed, text.Word(kept, "is", "are"), strings.Join(parts, "; "))
	if n := c.byCause[cleanedUp]; n > 0 {
		msg += fmt.Sprintf(". %s more will be removed with %s Job (TTL set, or still running)", text.Plural(n, "Pod", "Pods"), text.Word(n, "its", "their"))
	}
	return resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "TERMINATED_PODS_TO_CLEANUP", "%s", msg)
}

// stuckDeletions reports objects whose deletion started over stuckAfter ago
// and is waiting on finalizers. lifecycle drops them from the export.
func stuckDeletions(objs []*resource.Object, now time.Time) []*snapshotv1alpha1.Finding {
	var out []*snapshotv1alpha1.Finding
	for _, o := range objs {
		ts := o.GetDeletionTimestamp()
		fin := o.GetFinalizers()
		if ts == nil || len(fin) == 0 || now.Sub(ts.Time) < stuckAfter {
			continue
		}
		// A Namespace also waits on its spec.finalizers ("kubernetes"), which
		// clear once its contents are gone.
		out = append(out, resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_WARNING, "DELETION_STUCK",
			"%s has been deleting for %s, waiting on %s %s; the controller that added %s must be running to remove %s",
			o.Ref(), age(now.Sub(ts.Time)), text.Word(len(fin), "finalizer", "finalizers"), strings.Join(fin, ", "), text.Word(len(fin), "it", "them"), text.Word(len(fin), "it", "them")))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Message < out[j].Message })
	return out
}

func age(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	}
}

// flagCronJobHistory adds a finding to a CronJob whose history limits exceed
// the defaults: each Job it keeps keeps its Pods too.
func flagCronJobHistory(o *resource.Object) {
	success, foundS, _ := unstructured.NestedInt64(o.Object, "spec", "successfulJobsHistoryLimit")
	failed, foundF, _ := unstructured.NestedInt64(o.Object, "spec", "failedJobsHistoryLimit")
	if !foundS {
		success = defaultSuccessfulJobsHistoryLimit
	}
	if !foundF {
		failed = defaultFailedJobsHistoryLimit
	}
	if success <= defaultSuccessfulJobsHistoryLimit && failed <= defaultFailedJobsHistoryLimit {
		return
	}
	o.AddFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "CRONJOB_HISTORY_LIMIT",
		"keeps %d successful and %d failed Jobs, and their Pods (defaults are %d and %d)",
		success, failed, defaultSuccessfulJobsHistoryLimit, defaultFailedJobsHistoryLimit)
}

// nameList joins names, at most maxNames of them, saying how many more.
func nameList(names []string) string {
	if len(names) <= maxNames {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:maxNames], ", "), len(names)-maxNames)
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
