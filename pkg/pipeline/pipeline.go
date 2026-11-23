// Package pipeline runs the capture stages in order, collect, redact, the inventory, hygiene, ownership, lifecycle, platform, provenance, normalize, then builds the snapshot
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	"setdrift.io/setdrift-engine/pkg/collect"
	"setdrift.io/setdrift-engine/pkg/hygiene"
	"setdrift.io/setdrift-engine/pkg/inventory"
	"setdrift.io/setdrift-engine/pkg/lifecycle"
	"setdrift.io/setdrift-engine/pkg/normalize"
	"setdrift.io/setdrift-engine/pkg/ownership"
	"setdrift.io/setdrift-engine/pkg/platform"
	"setdrift.io/setdrift-engine/pkg/profile"
	"setdrift.io/setdrift-engine/pkg/provenance"
	"setdrift.io/setdrift-engine/pkg/redact"
	"setdrift.io/setdrift-engine/pkg/resource"
	"setdrift.io/setdrift-engine/pkg/snapshot"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// Pipeline holds the stages of one capture, their order is set by Run, not by callers
type Pipeline struct {
	// Collector reads the objects from the cluster or files, its required
	Collector collect.Collector
	// Redactor removes secret values, its required
	Redactor resource.Stage
	// the stages from here on are optional, a nil one is skipped
	// Hygiene reports hygiene findings about the objects
	Hygiene resource.Stage
	// Ownership is required when the collector lists types metadata-only (a ClusterCollector with a Metadata client)
	// it drops the objects left metadata-only, and Run fails if any reach the end
	Ownership resource.Stage
	// Lifecycle drops what is finished or temporary, and marks the Jobs that ran once
	Lifecycle resource.Stage
	// Platform classifies the platform from the collected objects, together with the evidence it was given before
	// it runs before Provenance, whose platform rules use the result, and the snapshot records it
	Platform *platform.Stage
	// Provenance tells which tool manages each object, Argo CD, Helm or an operator, from its labels, annotations and owners
	Provenance resource.Stage
	// Normalizer strips server-owned state as the last stage, controller-owned fields, per-kind defaults, status and server-set metadata
	Normalizer resource.Stage
}

// New takes a collector and gives back a pipeline with the default stages, the cli uses it for a normal capture
func New(c collect.Collector) *Pipeline {
	// one platform stage shared with provenance, so provenance's platform rules see the classification it makes
	plat := &platform.Stage{}
	return &Pipeline{
		Collector:  c,
		Redactor:   redact.SecretRedactor{},
		Hygiene:    hygiene.Reporter{},
		Ownership:  ownership.ControllerOwnerResolver{},
		Lifecycle:  lifecycle.Filter{},
		Platform:   plat,
		Provenance: provenance.LabelClassifier{Platform: plat},
		Normalizer: normalize.Normalizer{},
	}
}

// Run takes the snapshot metadata, collects the objects, runs every stage over them in order and gives back the snapshot
// meta is taken by value and its findings and cluster are copied, so the caller's Meta never changes
func (p *Pipeline) Run(ctx context.Context, meta snapshot.Meta) (*snapshotv1alpha1.Snapshot, error) {
	// checking if theres no collector, if so theres nothing to read the objects with
	if p.Collector == nil {
		return nil, errors.New("pipeline: collector is required")
	}
	// checking if theres no redactor, a capture never runs without removing secret values
	if p.Redactor == nil {
		return nil, errors.New("pipeline: redactor is required")
	}

	// invoking resource.WithCaptureTime to carry the capture time to the stages through ctx, its defined in pkg/resource/resource.go
	ctx = resource.WithCaptureTime(ctx, meta.CapturedAt)
	// invoking the collector's Collect to read the objects, the Collector interface is defined in pkg/collect/collect.go
	res, err := p.Collector.Collect(ctx)
	if err != nil {
		return nil, fmt.Errorf("collect: %w", err)
	}

	objs := res.Objects
	// cloning before appending, so the caller's Findings backing array is not written into
	meta.Findings = append(slices.Clone(meta.Findings), res.Findings...)
	// checking if the caller gave no scope, if so the one the collector reports is used
	if meta.Scope == nil {
		meta.Scope = res.Scope
	}
	// run takes one stage, runs it over the objects and keeps its output and findings for the next stage
	run := func(stage resource.Stage) error {
		// invoking the stage's Process, the Stage interface is defined in pkg/resource/resource.go
		out, findings, err := stage.Process(ctx, objs)
		if err != nil {
			return fmt.Errorf("%s: %w", stage.Name(), err)
		}
		objs = out
		meta.Findings = append(meta.Findings, findings...)
		return nil
	}
	// invoking run with the redactor first, so no later stage ever sees a secret value, its defined above
	if err := run(p.Redactor); err != nil {
		return nil, err
	}
	// invoking inventory.Build right after redaction, before later stages drop volumes, StatefulSet claims and status, its defined in pkg/inventory/inventory.go
	meta.Inventory = inventory.Build(objs)
	// checking if the inventory found nothing, if so one is started for the profile, which every capture has
	if meta.Inventory == nil {
		meta.Inventory = &snapshotv1alpha1.Inventory{}
	}
	// invoking profile.Build at the same point, while status and assigned values like a Service's clusterIP are still there, its defined in pkg/profile/profile.go
	meta.Inventory.Profile = profile.Build(objs)
	stages := []resource.Stage{p.Hygiene, p.Ownership, p.Lifecycle}
	// checking if theres a platform stage, its added only when set since a nil *platform.Stage in the interface slice wouldnt compare equal to nil below
	if p.Platform != nil {
		stages = append(stages, p.Platform)
	}
	// going through the remaining stages in order, platform before provenance
	for _, stage := range append(stages, p.Provenance, p.Normalizer) {
		// checking if the stage was left out, if so its skipped
		if stage == nil {
			continue
		}
		// invoking run with the stage, its defined above
		if err := run(stage); err != nil {
			return nil, err
		}
	}

	// looking for an object still metadata-only after every stage
	for _, o := range objs {
		// checking if this object is still metadata-only
		if o.Partial {
			// checking if the ownership stage was left out, if so the error names it, clearer than the one snapshot.Build would give
			if p.Ownership == nil {
				// invoking Ref to name the object, its defined in pkg/resource/resource.go
				return nil, fmt.Errorf("pipeline: %s is metadata-only; the Ownership stage is required when the collector lists types metadata-only", o.Ref())
			}
			break // snapshot.Build reports it
		}
	}

	cluster := &snapshotv1alpha1.Cluster{}
	// checking if the caller gave cluster details, if so they get cloned so the caller's Cluster is not changed
	if meta.Cluster != nil {
		cluster = proto.Clone(meta.Cluster).(*snapshotv1alpha1.Cluster)
	}
	// checking if the caller gave no kubernetes version, if so the one the collector found is used
	if cluster.KubernetesVersion == "" {
		cluster.KubernetesVersion = res.KubernetesVersion
	}
	// checking if the platform stage ran and found any evidence, if so the snapshot records its classification
	if st := p.Platform; st != nil && st.Result != nil && len(st.Result.Evidence) > 0 {
		// invoking Proto to turn the classification into its snapshot form, its defined in pkg/platform/probe.go
		cluster.Classification = st.Result.Proto()
		// checking if the older Platform field is empty and the distribution is sure enough to act on, if so it gets the distribution
		// invoking Is for that, its defined in pkg/platform/platform.go
		if cluster.Platform == "" && st.Result.Is(st.Result.Distribution) {
			cluster.Platform = string(st.Result.Distribution)
		}
	}
	// invoking profile.SetPlatform to give the profile the cloud and region the classification found, and the account in an EKS ARN, its defined in pkg/profile/nodes.go
	meta.Inventory.Profile = profile.SetPlatform(meta.Inventory.Profile, cluster)
	// checking if the profile found nothing, like for a directory of app manifests, if so its left out rather than written as a page of gaps
	if len(meta.Inventory.Profile.GetEvidence()) == 0 {
		meta.Inventory.Profile = nil
	}
	// checking if the inventory is empty after all, if so theres no inventory, as before the profile
	if proto.Size(meta.Inventory) == 0 {
		meta.Inventory = nil
	}
	meta.Cluster = cluster
	// invoking snapshot.Build to put the snapshot together from the metadata and the objects, its defined in pkg/snapshot/snapshot.go
	return snapshot.Build(meta, objs)
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
