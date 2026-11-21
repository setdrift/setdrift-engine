// Package platform classifies the Kubernetes platform a cluster runs on:
// its distribution (EKS, AKS, GKE, kubeadm, kind...), the infrastructure
// under it (AWS, Azure, GCP, a laptop...) and whether a provider manages
// it. Signals come in passes: the kubeconfig before anything is read, the
// API server (version, API groups, a few nodes), then the collected
// objects. Each signal is evidence with a weight; Decide weighs them all.
//
// Later stages act on the result only when it is confident: treating an
// object as the platform's leaves it out of the export, so a wrong guess
// would drop a user's object. With weak evidence, objects are exported.
package platform

import (
	"fmt"
	"sort"
	"strings"
)

// Distribution is the Kubernetes distribution or managed service.
type Distribution string

// Distributions known. Unknown means the evidence names none.
const (
	Unknown       Distribution = ""
	EKS           Distribution = "eks"
	AKS           Distribution = "aks"
	GKE           Distribution = "gke"
	OpenShift     Distribution = "openshift"
	K3s           Distribution = "k3s"
	RKE2          Distribution = "rke2"
	Kubeadm       Distribution = "kubeadm"
	Kind          Distribution = "kind"
	Minikube      Distribution = "minikube"
	DockerDesktop Distribution = "docker-desktop"
)

// Infrastructure is where the nodes run.
type Infrastructure string

// Infrastructures known. Other is a known non-cloud provider (vSphere,
// OpenStack, bare metal); Unknown means the evidence names none.
const (
	UnknownInfra Infrastructure = ""
	AWS          Infrastructure = "aws"
	Azure        Infrastructure = "azure"
	GCP          Infrastructure = "gcp"
	Local        Infrastructure = "local"
	Other        Infrastructure = "other"
)

// Confidence of a classification.
type Confidence string

// Confidences, from what a single strong signal gives to none.
const (
	HighConfidence   Confidence = "high"
	MediumConfidence Confidence = "medium"
	LowConfidence    Confidence = "low"
	NoConfidence     Confidence = ""
)

// Signal weights.
const (
	// Strong: the signal alone identifies it (an EKS API server address, a
	// kind:// node, a -eks server version).
	Strong = 10
	// Medium: usually right, but names can be chosen (a context name, a
	// CSI driver).
	Medium = 5
	// Weak: consistent with it, little more (a local API server address).
	Weak = 2
)

// Signal is one piece of evidence.
type Signal struct {
	// Source is where it came from: kubeconfig, server, node, object, aws.
	Source string
	// Detail is what was seen, for the user, e.g. `server host abc.gr7.us-east-2.eks.amazonaws.com`.
	Detail string
	// Distribution and Infrastructure it points to; either may be empty.
	Distribution   Distribution
	Infrastructure Infrastructure
	// Region, when the signal shows it.
	Region string
	Weight int
}

// Classification is the result of Decide.
type Classification struct {
	Distribution   Distribution
	Infrastructure Infrastructure
	// Managed is true for a provider-managed control plane (EKS, AKS, GKE,
	// ROSA...), false for a self-managed one (kubeadm, k3s, kind), nil when
	// the distribution is unknown.
	Managed *bool
	Region  string
	// Confidence of the distribution; InfraConfidence of the infrastructure.
	Confidence      Confidence
	InfraConfidence Confidence
	Evidence        []Signal
	// Conflicts are strong signals for another distribution or
	// infrastructure than the one chosen.
	Conflicts []string
}

// Is reports whether c is distribution d with at least medium confidence:
// the threshold for acting on it.
func (c *Classification) Is(d Distribution) bool {
	return c != nil && c.Distribution == d && (c.Confidence == HighConfidence || c.Confidence == MediumConfidence)
}

// On reports whether c runs on infrastructure i with at least medium confidence.
func (c *Classification) On(i Infrastructure) bool {
	return c != nil && c.Infrastructure == i && (c.InfraConfidence == HighConfidence || c.InfraConfidence == MediumConfidence)
}

var (
	distributionNames = map[Distribution]string{
		EKS: "EKS", AKS: "AKS", GKE: "GKE", OpenShift: "OpenShift", K3s: "k3s", RKE2: "RKE2",
		Kubeadm: "kubeadm", Kind: "kind", Minikube: "minikube", DockerDesktop: "Docker Desktop",
	}
	infrastructureNames = map[Infrastructure]string{
		AWS: "AWS", Azure: "Azure", GCP: "Google Cloud", Local: "a local machine", Other: "self-hosted infrastructure",
	}
)

// Name is a display name for the distribution, e.g. "EKS", or "" when unknown.
func (d Distribution) Name() string { return distributionNames[d] }

// Name is a display name for the infrastructure, e.g. "AWS", or "" when unknown.
func (i Infrastructure) Name() string { return infrastructureNames[i] }

// distributionFacts: the infrastructure a distribution implies, and whether
// a provider manages it.
var distributionFacts = map[Distribution]struct {
	infra   Infrastructure
	managed bool
}{
	EKS: {AWS, true}, AKS: {Azure, true}, GKE: {GCP, true},
	Kind: {Local, false}, Minikube: {Local, false}, DockerDesktop: {Local, false},
	K3s: {UnknownInfra, false}, RKE2: {UnknownInfra, false}, Kubeadm: {UnknownInfra, false},
	OpenShift: {UnknownInfra, false},
}

// Decide weighs the signals. The distribution with the most weight wins;
// its confidence is high with a strong signal and no strong signal against
// it, medium with medium evidence, low otherwise. A distribution's implied
// infrastructure (EKS: AWS) counts as infrastructure evidence.
//
// More specific distributions win ties over the ones they build on: kind
// and minikube are kubeadm clusters, and their kubeadm signals support them.
func Decide(signals []Signal) *Classification {
	c := &Classification{Evidence: signals}
	dist, infra := map[Distribution]int{}, map[Infrastructure]int{}
	strongDist, strongInfra := map[Distribution]bool{}, map[Infrastructure]bool{}
	for _, s := range signals {
		if s.Distribution != Unknown {
			dist[s.Distribution] += s.Weight
			if s.Weight >= Strong {
				strongDist[s.Distribution] = true
			}
		}
		if s.Infrastructure != UnknownInfra {
			infra[s.Infrastructure] += s.Weight
			if s.Weight >= Strong {
				strongInfra[s.Infrastructure] = true
			}
		}
		if c.Region == "" && s.Region != "" {
			c.Region = s.Region
		}
	}
	// kind and minikube are built with kubeadm: kubeadm's evidence is theirs too.
	for _, d := range []Distribution{Kind, Minikube} {
		if dist[d] > 0 {
			dist[d] += dist[Kubeadm]
			strongDist[d] = strongDist[d] || strongDist[Kubeadm]
			delete(dist, Kubeadm)
			delete(strongDist, Kubeadm)
		}
	}

	c.Distribution, c.Confidence = pick(dist, strongDist)
	if f, ok := distributionFacts[c.Distribution]; ok && c.Distribution != Unknown {
		managed := f.managed
		c.Managed = &managed
		if f.infra != UnknownInfra {
			w := Medium
			if c.Confidence == HighConfidence {
				w = Strong
			}
			infra[f.infra] += w
			if w >= Strong {
				strongInfra[f.infra] = true
			}
		}
	}
	c.Infrastructure, c.InfraConfidence = pick(infra, strongInfra)

	for d := range strongDist {
		if d != c.Distribution {
			c.Conflicts = append(c.Conflicts, fmt.Sprintf("strong evidence of %s too", d.Name()))
		}
	}
	for i := range strongInfra {
		if i != c.Infrastructure {
			c.Conflicts = append(c.Conflicts, fmt.Sprintf("strong evidence of %s infrastructure too", i.Name()))
		}
	}
	sort.Strings(c.Conflicts)
	if len(c.Conflicts) > 0 {
		// Contradicting strong evidence: act on nothing.
		c.Confidence, c.InfraConfidence = LowConfidence, LowConfidence
	}
	return c
}

// pick returns the key with the most weight and its confidence.
func pick[K ~string](weights map[K]int, strong map[K]bool) (K, Confidence) {
	var best K
	bestW, second := 0, 0
	keys := make([]K, 0, len(weights))
	for k := range weights {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		switch w := weights[k]; {
		case w > bestW:
			best, second, bestW = k, bestW, w
		case w > second:
			second = w
		}
	}
	switch {
	case bestW == 0:
		return best, NoConfidence
	case strong[best]:
		return best, HighConfidence
	case bestW >= Medium && bestW > second:
		return best, MediumConfidence
	}
	return best, LowConfidence
}

// Summary describes c in one line, e.g. "EKS on AWS (us-east-2), high confidence".
func (c *Classification) Summary() string {
	if c == nil || c.Distribution == Unknown && c.Infrastructure == UnknownInfra {
		return "unknown platform"
	}
	var b strings.Builder
	switch {
	case c.Distribution != Unknown:
		b.WriteString(c.Distribution.Name())
		if c.Infrastructure != UnknownInfra && distributionFacts[c.Distribution].infra != c.Infrastructure {
			b.WriteString(" on " + c.Infrastructure.Name())
		}
	default:
		b.WriteString("unknown distribution on " + c.Infrastructure.Name())
	}
	if c.Region != "" {
		b.WriteString(" (" + c.Region + ")")
	}
	if c.Confidence != NoConfidence {
		b.WriteString(", " + string(c.Confidence) + " confidence")
	}
	return b.String()
}

// Dir is the export's platform directory for c, relative to the cluster's
// directory: platform/<cloud>/<distribution>, with "vm" for a self-managed
// cluster on a cloud (kubeadm, k3s or RKE2 on its VMs, or an unidentified
// distribution there), e.g. platform/aws/eks, platform/azure/vm,
// platform/local/kind. Empty when neither is known with confidence: the
// export is then generic only.
func (c *Classification) Dir() string {
	if c == nil {
		return ""
	}
	infraKnown := c.InfraConfidence == HighConfidence || c.InfraConfidence == MediumConfidence
	distKnown := c.Is(c.Distribution) && c.Distribution != Unknown
	cloud := string(c.Infrastructure)
	if !infraKnown {
		if !distKnown {
			return ""
		}
		cloud = "other"
	}
	leaf := "vm"
	if distKnown {
		f := distributionFacts[c.Distribution]
		if f.managed || c.Infrastructure == Local || f.infra == Local || !infraKnown {
			leaf = string(c.Distribution)
		}
	}
	if cloud == string(Other) && distKnown {
		leaf = string(c.Distribution)
	}
	return "platform/" + cloud + "/" + leaf
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
