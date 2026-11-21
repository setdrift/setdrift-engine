// asks the cluster itself for platform evidence, then finishes the classification as a pipeline stage and converts it to and from the snapshot

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

// probeNodes is how many nodes Probe reads, enough to see the provider and the managed service's labels, few enough to cost nothing
const probeNodes = 5

// nodesGVR is how the dynamic client finds nodes, Probe lists them through it
var nodesGVR = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}

// Probe is the second pass, it reads the API server's version and groups and a few nodes and gives back the signals they hold
// nodes need cluster-wide list permission, without it the signals are fewer and the result less certain, and notes says so
func Probe(ctx context.Context, d discover.Interface, dyn dynamic.Interface) (signals []Signal, notes []string) {
	gitVersion := ""
	// invoking ServerVersionWithContext and checking if the version could be read, if not the version gives no signal, its defined in internal/discover/discover.go
	if v, err := d.ServerVersionWithContext(ctx); err == nil {
		gitVersion = v.GitVersion
	}
	var groups []string
	// invoking ServerGroupsWithContext and checking if the groups could be read, if so their names are collected, its defined in internal/discover/discover.go
	if gl, err := d.ServerGroupsWithContext(ctx); err == nil {
		// collecting each group's name
		for _, g := range gl.Groups {
			groups = append(groups, g.Name)
		}
	}
	// invoking FromServer to read the signals in the version and groups, its defined in signals.go
	signals = FromServer(gitVersion, groups)

	// listing a few nodes, probeNodes at most
	list, err := dyn.Resource(nodesGVR).List(ctx, metav1.ListOptions{Limit: probeNodes})
	// checking if the nodes couldnt be read, if so the classification goes on with less evidence and a note says why
	if err != nil {
		// invoking shortError to keep the reason short, its defined below
		notes = append(notes, "nodes could not be read ("+shortError(err)+"), so the platform is classified from less evidence")
		return signals, notes
	}
	nodes := make([]Node, 0, len(list.Items))
	// keeping each node's name, provider id and labels, thats all FromNodes reads
	for _, n := range list.Items {
		id, _, _ := unstructured.NestedString(n.Object, "spec", "providerID")
		nodes = append(nodes, Node{Name: n.GetName(), ProviderID: id, Labels: n.GetLabels()})
	}
	// invoking FromNodes to read the signals in the nodes, its defined in signals.go
	return append(signals, FromNodes(nodes)...), notes
}

// shortError takes an error and gives back a short reason for the note, "forbidden" for a permission error, otherwise the message cut to 120 bytes
func shortError(err error) string {
	s := err.Error()
	// checking if the part before the first ": " says forbidden, if so thats all the note needs
	if i := strings.Index(s, ": "); i > 0 && strings.Contains(s[:i], "forbidden") {
		return "forbidden"
	}
	// checking if the message is long, if so its cut
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// Stage is the pipeline stage that finishes the classification
// it adds the collected objects' signals to Initial, decides, and stores the result in Result for the export and the stages after it, like the provenance classifier's platform rules
type Stage struct {
	// the signals from the kubeconfig and Probe
	Initial []Signal
	// the notes from Probe, each one becomes a finding
	Notes []string
	// Result is set by Process
	Result *Classification
}

// Name gives back the stage's name, it implements resource.Stage, which is defined in pkg/resource/resource.go
func (s *Stage) Name() string { return "platform" }

// Process implements resource.Stage, it classifies the platform from Initial and the objects and reports it as findings
// the objects pass through unchanged
func (s *Stage) Process(_ context.Context, objs []*resource.Object) ([]*resource.Object, []*snapshotv1alpha1.Finding, error) {
	// invoking FromObjects for the objects' signals, its defined in signals.go, and Decide to weigh them with Initial, its defined in platform.go
	// Initial is copied first so the append never writes into its backing array
	c := Decide(append(append([]Signal(nil), s.Initial...), FromObjects(objs)...))
	s.Result = c
	var findings []*snapshotv1alpha1.Finding
	var evidence []string
	// collecting what each signal saw, for the finding's message
	for _, e := range c.Evidence {
		evidence = append(evidence, e.Detail)
	}
	// invoking Summary to describe the classification in one line, its defined in platform.go
	msg := c.Summary()
	// checking if theres any evidence, if so the message lists it
	if len(evidence) > 0 {
		msg += "; from " + strings.Join(evidence, ", ")
	}
	// checking if anything about the platform is known, if so its reported
	if c.Distribution != Unknown || c.Infrastructure != UnknownInfra {
		// invoking resource.NewFinding to build the finding, its defined in pkg/resource/resource.go
		findings = append(findings, resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "PLATFORM", "%s", msg))
	}
	// reporting each note from Probe, like nodes that couldnt be read
	for _, n := range s.Notes {
		findings = append(findings, resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "PLATFORM_EVIDENCE_LIMITED", "%s", n))
	}
	// checking if the evidence conflicts, if so a warning says no platform rule is applied
	if len(c.Conflicts) > 0 {
		findings = append(findings, resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_WARNING, "PLATFORM_CONFLICT",
			"the evidence disagrees (%s), so no platform-specific rule is applied and every object is exported",
			strings.Join(c.Conflicts, "; ")))
	}
	return objs, findings, nil
}

// Proto converts c to its snapshot form, nil stays nil
func (c *Classification) Proto() *snapshotv1alpha1.Platform {
	// checking if theres no classification, if so the snapshot gets none
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
	// checking whether its managed, unknown leaves the field empty, otherwise its managed or self-managed
	switch {
	case c.Managed == nil:
	case *c.Managed:
		p.Managed = "managed"
	default:
		p.Managed = "self-managed"
	}
	// converting each signal to its snapshot form
	for _, e := range c.Evidence {
		p.Evidence = append(p.Evidence, &snapshotv1alpha1.PlatformEvidence{
			Source: e.Source, Detail: e.Detail, Distribution: string(e.Distribution),
			Infrastructure: string(e.Infrastructure), Weight: int32(e.Weight), //#nosec G115 -- weights are small constants (2, 5, 10)
		})
	}
	return p
}

// FromProto rebuilds a Classification from a snapshot, for the emitter
// a missing platform gives back an empty Classification, never nil
func FromProto(p *snapshotv1alpha1.Platform) *Classification {
	// checking if the snapshot has no platform, if so the classification is empty
	if p == nil {
		return &Classification{}
	}
	c := &Classification{
		Distribution: Distribution(p.Distribution), Infrastructure: Infrastructure(p.Infrastructure), Region: p.Region,
		Confidence: Confidence(p.Confidence), InfraConfidence: Confidence(p.InfraConfidence), Conflicts: p.Conflicts,
	}
	// checking if the snapshot says managed or self-managed, anything else leaves Managed nil as unknown
	switch p.Managed {
	case "managed", "self-managed":
		m := p.Managed == "managed"
		c.Managed = &m
	}
	// converting each piece of evidence back to a signal
	for _, e := range p.Evidence {
		c.Evidence = append(c.Evidence, Signal{Source: e.Source, Detail: e.Detail, Distribution: Distribution(e.Distribution),
			Infrastructure: Infrastructure(e.Infrastructure), Weight: int(e.Weight)})
	}
	return c
}

// String describes the signal in one line, for debugging
func (s Signal) String() string {
	return fmt.Sprintf("%s: %s (%s/%s, %d)", s.Source, s.Detail, s.Distribution, s.Infrastructure, s.Weight)
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
