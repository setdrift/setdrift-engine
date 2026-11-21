// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"setdrift.io/setdrift-engine/internal/discover"
	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// probeNodes is how many nodes Probe reads: enough to see the provider and
// the managed service's labels, few enough to cost nothing.
const probeNodes = 5

var nodesGVR = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}

// Probe reads the API server's version and groups and a few nodes: the
// second pass. Nodes need cluster-wide list permission; without it the
// signals are fewer and the result less certain, and notes says so.
func Probe(ctx context.Context, d discover.Interface, dyn dynamic.Interface) (signals []Signal, notes []string) {
	gitVersion := ""
	if v, err := d.ServerVersionWithContext(ctx); err == nil {
		gitVersion = v.GitVersion
	}
	var groups []string
	if gl, err := d.ServerGroupsWithContext(ctx); err == nil {
		for _, g := range gl.Groups {
			groups = append(groups, g.Name)
		}
	}
	signals = FromServer(gitVersion, groups)

	list, err := dyn.Resource(nodesGVR).List(ctx, metav1.ListOptions{Limit: probeNodes})
	if err != nil {
		notes = append(notes, "nodes could not be read ("+shortError(err)+"), so the platform is classified from less evidence")
		return signals, notes
	}
	nodes := make([]Node, 0, len(list.Items))
	for _, n := range list.Items {
		id, _, _ := unstructured.NestedString(n.Object, "spec", "providerID")
		nodes = append(nodes, Node{Name: n.GetName(), ProviderID: id, Labels: n.GetLabels()})
	}
	return append(signals, FromNodes(nodes)...), notes
}

func shortError(err error) string {
	s := err.Error()
	if i := strings.Index(s, ": "); i > 0 && strings.Contains(s[:i], "forbidden") {
		return "forbidden"
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// Stage is the pipeline stage that finishes the classification: it adds the
// collected objects' signals to Initial (from the kubeconfig and Probe),
// decides, and stores the result in Result for the stages after it (the
// provenance classifier's platform rules) and for the export.
type Stage struct {
	Initial []Signal
	Notes   []string
	// Result is set by Process.
	Result *Classification
}

// Name implements resource.Stage.
func (s *Stage) Name() string { return "platform" }

// Process implements resource.Stage. Objects pass through unchanged.
func (s *Stage) Process(_ context.Context, objs []*resource.Object) ([]*resource.Object, []*snapshotv1alpha1.Finding, error) {
	c := Decide(append(append([]Signal(nil), s.Initial...), FromObjects(objs)...))
	s.Result = c
	var findings []*snapshotv1alpha1.Finding
	var evidence []string
	for _, e := range c.Evidence {
		evidence = append(evidence, e.Detail)
	}
	msg := c.Summary()
	if len(evidence) > 0 {
		msg += "; from " + strings.Join(evidence, ", ")
	}
	if c.Distribution != Unknown || c.Infrastructure != UnknownInfra {
		findings = append(findings, resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "PLATFORM", "%s", msg))
	}
	for _, n := range s.Notes {
		findings = append(findings, resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "PLATFORM_EVIDENCE_LIMITED", "%s", n))
	}
	if len(c.Conflicts) > 0 {
		findings = append(findings, resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_WARNING, "PLATFORM_CONFLICT",
			"the evidence disagrees (%s), so no platform-specific rule is applied and every object is exported",
			strings.Join(c.Conflicts, "; ")))
	}
	return objs, findings, nil
}

// Proto converts c for the snapshot.
func (c *Classification) Proto() *snapshotv1alpha1.Platform {
	if c == nil {
		return nil
	}
	p := &snapshotv1alpha1.Platform{
		Distribution:    string(c.Distribution),
		Infrastructure:  string(c.Infrastructure),
		Region:          c.Region,
		Confidence:      string(c.Confidence),
		InfraConfidence: string(c.InfraConfidence),
		Conflicts:       c.Conflicts,
	}
	switch {
	case c.Managed == nil:
	case *c.Managed:
		p.Managed = "managed"
	default:
		p.Managed = "self-managed"
	}
	for _, e := range c.Evidence {
		p.Evidence = append(p.Evidence, &snapshotv1alpha1.PlatformEvidence{
			Source: e.Source, Detail: e.Detail, Distribution: string(e.Distribution),
			Infrastructure: string(e.Infrastructure), Weight: int32(e.Weight), //#nosec G115 -- weights are small constants (2, 5, 10)
		})
	}
	return p
}

// FromProto rebuilds a Classification from a snapshot, for the emitter.
func FromProto(p *snapshotv1alpha1.Platform) *Classification {
	if p == nil {
		return &Classification{}
	}
	c := &Classification{
		Distribution: Distribution(p.Distribution), Infrastructure: Infrastructure(p.Infrastructure), Region: p.Region,
		Confidence: Confidence(p.Confidence), InfraConfidence: Confidence(p.InfraConfidence), Conflicts: p.Conflicts,
	}
	switch p.Managed {
	case "managed", "self-managed":
		m := p.Managed == "managed"
		c.Managed = &m
	}
	for _, e := range p.Evidence {
		c.Evidence = append(c.Evidence, Signal{Source: e.Source, Detail: e.Detail, Distribution: Distribution(e.Distribution),
			Infrastructure: Infrastructure(e.Infrastructure), Weight: int(e.Weight)})
	}
	return c
}

// String is for debugging.
func (s Signal) String() string {
	return fmt.Sprintf("%s: %s (%s/%s, %d)", s.Source, s.Detail, s.Distribution, s.Infrastructure, s.Weight)
}
