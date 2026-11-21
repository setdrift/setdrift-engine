// Package platform classifies the Kubernetes platform a cluster runs on, its distribution like EKS, AKS, GKE, kubeadm or kind, the infrastructure under it like AWS, Azure, GCP or a laptop, and whether a provider manages it
// the signals come in passes, the kubeconfig before anything is read, then the API server with its version, API groups and a few nodes, then the collected objects
// each signal is evidence with a weight, and Decide weighs them all
// later stages act on the result only when its confident, since treating an object as the platform's leaves it out of the export and a wrong guess would drop a user's object
// with weak evidence the objects are exported
package platform

import (
	"fmt"
	"sort"
	"strings"
)

// Distribution is the Kubernetes distribution or managed service
type Distribution string

// the distributions known, Unknown means the evidence names none
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

// Infrastructure is where the nodes run
type Infrastructure string

// the infrastructures known, Other is a known non-cloud provider like vSphere, OpenStack or bare metal
// UnknownInfra means the evidence names none
const (
	UnknownInfra Infrastructure = ""
	AWS          Infrastructure = "aws"
	Azure        Infrastructure = "azure"
	GCP          Infrastructure = "gcp"
	Local        Infrastructure = "local"
	Other        Infrastructure = "other"
)

// Confidence is how sure a classification is
type Confidence string

// the confidences, from what a single strong signal gives down to none
const (
	HighConfidence   Confidence = "high"
	MediumConfidence Confidence = "medium"
	LowConfidence    Confidence = "low"
	NoConfidence     Confidence = ""
)

// the signal weights
const (
	// Strong is for a signal that alone identifies the platform, like an EKS API server address, a kind:// node or a -eks server version
	Strong = 10
	// Medium is for a signal thats usually right but whose name can be chosen, like a context name or a CSI driver
	Medium = 5
	// Weak is for a signal thats consistent with the platform and little more, like a local API server address
	Weak = 2
)

// Signal is one piece of evidence about the platform
type Signal struct {
	// where it came from, kubeconfig, server, node, object or aws
	Source string
	// what was seen, shown to the user, e.g. `server host abc.gr7.us-east-2.eks.amazonaws.com`
	Detail string
	// the distribution and infrastructure it points to, either may be empty
	Distribution   Distribution
	Infrastructure Infrastructure
	// the region, when the signal shows it
	Region string
	// how much the signal counts, Strong, Medium or Weak
	Weight int
}

// Classification is what Decide concludes about the platform
type Classification struct {
	Distribution   Distribution
	Infrastructure Infrastructure
	// true for a provider-managed control plane like EKS, AKS, GKE or ROSA, false for a self-managed one like kubeadm, k3s or kind
	// nil when the distribution is unknown
	Managed *bool
	Region  string
	// Confidence is about the distribution, InfraConfidence about the infrastructure
	Confidence      Confidence
	InfraConfidence Confidence
	// every signal that went into the decision
	Evidence []Signal
	// strong signals for another distribution or infrastructure than the one chosen
	Conflicts []string
}

// Is tells if c is distribution d with at least medium confidence, the threshold for acting on it
func (c *Classification) Is(d Distribution) bool {
	return c != nil && c.Distribution == d && (c.Confidence == HighConfidence || c.Confidence == MediumConfidence)
}

// On tells if c runs on infrastructure i with at least medium confidence
func (c *Classification) On(i Infrastructure) bool {
	return c != nil && c.Infrastructure == i && (c.InfraConfidence == HighConfidence || c.InfraConfidence == MediumConfidence)
}

// the display names of the distributions and infrastructures, the unknown ones have none
var (
	distributionNames = map[Distribution]string{
		EKS: "EKS", AKS: "AKS", GKE: "GKE", OpenShift: "OpenShift", K3s: "k3s", RKE2: "RKE2",
		Kubeadm: "kubeadm", Kind: "kind", Minikube: "minikube", DockerDesktop: "Docker Desktop",
	}
	infrastructureNames = map[Infrastructure]string{
		AWS: "AWS", Azure: "Azure", GCP: "Google Cloud", Local: "a local machine", Other: "self-hosted infrastructure",
	}
)

// Name gives back a display name for the distribution, e.g. "EKS", or "" when its unknown
func (d Distribution) Name() string { return distributionNames[d] }

// Name gives back a display name for the infrastructure, e.g. "AWS", or "" when its unknown
func (i Infrastructure) Name() string { return infrastructureNames[i] }

// distributionFacts holds the infrastructure each distribution implies, and whether a provider manages it
var distributionFacts = map[Distribution]struct {
	infra   Infrastructure
	managed bool
}{
	EKS: {AWS, true}, AKS: {Azure, true}, GKE: {GCP, true},
	Kind: {Local, false}, Minikube: {Local, false}, DockerDesktop: {Local, false},
	K3s: {UnknownInfra, false}, RKE2: {UnknownInfra, false}, Kubeadm: {UnknownInfra, false},
	OpenShift: {UnknownInfra, false},
}

// Decide weighs the signals and gives back the classification, the distribution with the most weight wins
// its confidence is high with a strong signal and no strong signal against it, medium with medium evidence, low otherwise
// a distribution's implied infrastructure, like AWS for EKS, counts as infrastructure evidence
// more specific distributions win ties over the ones they build on, kind and minikube are kubeadm clusters so their kubeadm signals support them
func Decide(signals []Signal) *Classification {
	c := &Classification{Evidence: signals}
	dist, infra := map[Distribution]int{}, map[Infrastructure]int{}
	strongDist, strongInfra := map[Distribution]bool{}, map[Infrastructure]bool{}
	// adding up the weight of each signal for its distribution and infrastructure
	for _, s := range signals {
		// checking if the signal names a distribution, if so its weight counts for it
		if s.Distribution != Unknown {
			dist[s.Distribution] += s.Weight
			// checking if its a strong signal, if so the distribution has strong evidence
			if s.Weight >= Strong {
				strongDist[s.Distribution] = true
			}
		}
		// checking if the signal names an infrastructure, if so its weight counts for it
		if s.Infrastructure != UnknownInfra {
			infra[s.Infrastructure] += s.Weight
			// checking if its a strong signal, if so the infrastructure has strong evidence
			if s.Weight >= Strong {
				strongInfra[s.Infrastructure] = true
			}
		}
		// checking if no region was found yet and this signal has one, if so the first region seen is kept
		if c.Region == "" && s.Region != "" {
			c.Region = s.Region
		}
	}
	// kind and minikube are built with kubeadm, so kubeadm's evidence is theirs too
	for _, d := range []Distribution{Kind, Minikube} {
		// checking if theres evidence for kind or minikube, if so kubeadm's weight moves over to it
		if dist[d] > 0 {
			dist[d] += dist[Kubeadm]
			strongDist[d] = strongDist[d] || strongDist[Kubeadm]
			delete(dist, Kubeadm)
			delete(strongDist, Kubeadm)
		}
	}

	// invoking pick to choose the distribution with the most weight and its confidence, its defined below
	c.Distribution, c.Confidence = pick(dist, strongDist)
	// checking if a known distribution was chosen, if so its facts tell whether its managed and where it runs
	if f, ok := distributionFacts[c.Distribution]; ok && c.Distribution != Unknown {
		managed := f.managed
		c.Managed = &managed
		// checking if the distribution implies an infrastructure, if so that counts as infrastructure evidence
		if f.infra != UnknownInfra {
			w := Medium
			// checking if the distribution is highly confident, if so the implied infrastructure counts as strong
			if c.Confidence == HighConfidence {
				w = Strong
			}
			infra[f.infra] += w
			// checking if the implied evidence is strong, if so the infrastructure has strong evidence
			if w >= Strong {
				strongInfra[f.infra] = true
			}
		}
	}
	// invoking pick again to choose the infrastructure, its defined below
	c.Infrastructure, c.InfraConfidence = pick(infra, strongInfra)

	// noting each distribution with strong evidence that wasnt chosen
	for d := range strongDist {
		// checking if this isnt the chosen distribution, if so its a conflict
		if d != c.Distribution {
			c.Conflicts = append(c.Conflicts, fmt.Sprintf("strong evidence of %s too", d.Name()))
		}
	}
	// noting each infrastructure with strong evidence that wasnt chosen
	for i := range strongInfra {
		// checking if this isnt the chosen infrastructure, if so its a conflict
		if i != c.Infrastructure {
			c.Conflicts = append(c.Conflicts, fmt.Sprintf("strong evidence of %s infrastructure too", i.Name()))
		}
	}
	sort.Strings(c.Conflicts)
	// checking if strong evidence contradicts the choice, if so the confidence drops to low so nothing acts on it
	if len(c.Conflicts) > 0 {
		c.Confidence, c.InfraConfidence = LowConfidence, LowConfidence
	}
	return c
}

// pick takes the weights and which keys have strong evidence, and gives back the key with the most weight and its confidence
// the keys are walked in sorted order, so a tie always goes the same way
func pick[K ~string](weights map[K]int, strong map[K]bool) (K, Confidence) {
	var best K
	bestW, second := 0, 0
	keys := make([]K, 0, len(weights))
	// collecting the keys so they can be sorted
	for k := range weights {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	// finding the heaviest key and the second heaviest weight
	for _, k := range keys {
		// checking if this weight beats the best so far or only the second best
		switch w := weights[k]; {
		case w > bestW:
			best, second, bestW = k, bestW, w
		case w > second:
			second = w
		}
	}
	// checking how sure the choice is, none without weight, high with strong evidence, medium when its at least medium and ahead of the runner-up
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

// Summary describes c in one line, e.g. "EKS on AWS (us-east-2), high confidence"
func (c *Classification) Summary() string {
	// checking if neither the distribution nor the infrastructure is known, if so theres only an unknown platform to report
	if c == nil || c.Distribution == Unknown && c.Infrastructure == UnknownInfra {
		return "unknown platform"
	}
	var b strings.Builder
	// checking if the distribution is known, if so it leads, otherwise only the infrastructure is named
	switch {
	case c.Distribution != Unknown:
		b.WriteString(c.Distribution.Name())
		// checking if the infrastructure is known and isnt the one the distribution implies, if so its named too
		if c.Infrastructure != UnknownInfra && distributionFacts[c.Distribution].infra != c.Infrastructure {
			b.WriteString(" on " + c.Infrastructure.Name())
		}
	default:
		b.WriteString("unknown distribution on " + c.Infrastructure.Name())
	}
	// checking if the region is known, if so its added
	if c.Region != "" {
		b.WriteString(" (" + c.Region + ")")
	}
	// checking if theres any confidence, if so its added
	if c.Confidence != NoConfidence {
		b.WriteString(", " + string(c.Confidence) + " confidence")
	}
	return b.String()
}

// Dir gives back the export's platform directory for c, relative to the cluster's directory, as platform/<cloud>/<distribution>
// a self-managed cluster on a cloud gets "vm" as its distribution, that covers kubeadm, k3s or RKE2 on the cloud's VMs and an unidentified distribution there
// e.g. platform/aws/eks, platform/azure/vm, platform/local/kind
// its empty when neither is known with confidence, the export is then generic only
func (c *Classification) Dir() string {
	// checking if theres no classification at all, if so theres no platform directory
	if c == nil {
		return ""
	}
	infraKnown := c.InfraConfidence == HighConfidence || c.InfraConfidence == MediumConfidence
	// invoking Is to tell if the distribution is known with confidence, its defined above
	distKnown := c.Is(c.Distribution) && c.Distribution != Unknown
	cloud := string(c.Infrastructure)
	// checking if the infrastructure isnt known, if so a known distribution goes under other and an unknown one gets no directory
	if !infraKnown {
		// checking if the distribution isnt known either, if so theres no platform directory
		if !distKnown {
			return ""
		}
		cloud = "other"
	}
	leaf := "vm"
	// checking if the distribution is known, if so it names the leaf when its managed, local, or the infrastructure is unknown
	if distKnown {
		f := distributionFacts[c.Distribution]
		// checking if the distribution is managed, runs locally or has no known infrastructure, those get their own name rather than vm
		if f.managed || c.Infrastructure == Local || f.infra == Local || !infraKnown {
			leaf = string(c.Distribution)
		}
	}
	// checking if the cluster is on other infrastructure with a known distribution, if so the distribution names the leaf
	if cloud == string(Other) && distKnown {
		leaf = string(c.Distribution)
	}
	return "platform/" + cloud + "/" + leaf
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
