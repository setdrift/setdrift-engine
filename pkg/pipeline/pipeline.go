// Package pipeline runs the stages in order: collect, redact, (the
// inventory), hygiene, ownership, lifecycle, platform, provenance,
// normalize, then builds the snapshot.
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
	"setdrift.io/setdrift-engine/pkg/provenance"
	"setdrift.io/setdrift-engine/pkg/redact"
	"setdrift.io/setdrift-engine/pkg/resource"
	"setdrift.io/setdrift-engine/pkg/snapshot"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// Pipeline is a capture's stages. Their order is set here, not by callers.
type Pipeline struct {
	// Collector reads the objects; required.
	Collector collect.Collector
	// Redactor removes secret values; required.
	Redactor resource.Stage
	// The other stages are optional: nil skips one.
	//
	// Ownership is required when the collector lists types metadata-only
	// (a ClusterCollector with a Metadata client): it drops the objects left
	// metadata-only, and Run fails if any reach the end.
	Hygiene   resource.Stage
	Ownership resource.Stage
	Lifecycle resource.Stage
	// Platform classifies the platform from the collected objects (with the
	// evidence it was given before); it runs before Provenance, whose
	// platform rules use the result, and the snapshot records it.
	Platform   *platform.Stage
	Provenance resource.Stage
	Normalizer resource.Stage
}

// New returns a pipeline with the default stages.
func New(c collect.Collector) *Pipeline {
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

// Run collects, runs the stages and returns the snapshot.
func (p *Pipeline) Run(ctx context.Context, meta snapshot.Meta) (*snapshotv1alpha1.Snapshot, error) {
	if p.Collector == nil {
		return nil, errors.New("pipeline: collector is required")
	}
	if p.Redactor == nil {
		return nil, errors.New("pipeline: redactor is required")
	}

	ctx = resource.WithCaptureTime(ctx, meta.CapturedAt)
	res, err := p.Collector.Collect(ctx)
	if err != nil {
		return nil, fmt.Errorf("collect: %w", err)
	}

	objs := res.Objects
	// Copies, so the caller's Meta is not changed.
	meta.Findings = append(slices.Clone(meta.Findings), res.Findings...)
	if meta.Scope == nil {
		meta.Scope = res.Scope
	}
	run := func(stage resource.Stage) error {
		out, findings, err := stage.Process(ctx, objs)
		if err != nil {
			return fmt.Errorf("%s: %w", stage.Name(), err)
		}
		objs = out
		meta.Findings = append(meta.Findings, findings...)
		return nil
	}
	if err := run(p.Redactor); err != nil {
		return nil, err
	}
	// Right after redaction, before later stages drop volumes, StatefulSet
	// claims and status.
	meta.Inventory = inventory.Build(objs)
	stages := []resource.Stage{p.Hygiene, p.Ownership, p.Lifecycle}
	if p.Platform != nil {
		stages = append(stages, p.Platform)
	}
	for _, stage := range append(stages, p.Provenance, p.Normalizer) {
		if stage == nil {
			continue
		}
		if err := run(stage); err != nil {
			return nil, err
		}
	}

	for _, o := range objs {
		if o.Partial {
			if p.Ownership == nil {
				return nil, fmt.Errorf("pipeline: %s is metadata-only; the Ownership stage is required when the collector lists types metadata-only", o.Ref())
			}
			break // snapshot.Build reports it
		}
	}

	cluster := &snapshotv1alpha1.Cluster{}
	if meta.Cluster != nil {
		cluster = proto.Clone(meta.Cluster).(*snapshotv1alpha1.Cluster)
	}
	if cluster.KubernetesVersion == "" {
		cluster.KubernetesVersion = res.KubernetesVersion
	}
	if st := p.Platform; st != nil && st.Result != nil && len(st.Result.Evidence) > 0 {
		cluster.Classification = st.Result.Proto()
		// the older field: the distribution, when it is sure enough to act on
		if cluster.Platform == "" && st.Result.Is(st.Result.Distribution) {
			cluster.Platform = string(st.Result.Distribution)
		}
	}
	meta.Cluster = cluster
	return snapshot.Build(meta, objs)
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
