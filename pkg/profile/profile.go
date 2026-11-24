// Package profile describes a cluster as infrastructure, its cloud, networks, DNS, storage and the system components that run it
// it reads the objects right after redaction, before normalize drops the status and assigned values much of it comes from
// each value goes into the profile once, with every place it was found as evidence, so a reader can check it
// it reads settings only, the objects it gets have had every secret value replaced already
package profile

import (
	"cmp"
	"slices"
	"sort"
	"strings"

	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// builder holds the profile while its being filled in, with the evidence already recorded so none is listed twice
type builder struct {
	// the profile being filled in
	p *snapshotv1alpha1.Profile
	// evidence already recorded, by value and source
	seen map[[2]string]bool
	// claims per storage class name, "" for claims that name none and get the default class
	claims map[string]int32
	// azure cni overlay, from cns-config, and the ranges azure's ip-masq-agent leaves unmasqueraded with where they were found
	// under overlay those are the pod network, under azure cni's other modes theyre the vnet, so theyre only used once overlay is known
	azureOverlay   bool
	azureNoMasq    []string
	azureNoMasqSrc string
}

// newBuilder takes a profile, or nil for a new one, and gives back a builder that adds to it
func newBuilder(p *snapshotv1alpha1.Profile) *builder {
	// checking if theres no profile yet, if so a new one is started
	if p == nil {
		p = &snapshotv1alpha1.Profile{}
	}
	// checking each part the builder writes into, a missing one is started so the detectors can write without checking
	if p.Cloud == nil {
		p.Cloud = &snapshotv1alpha1.ProfileCloud{}
	}
	if p.Network == nil {
		p.Network = &snapshotv1alpha1.ProfileNetwork{}
	}
	if p.Dns == nil {
		p.Dns = &snapshotv1alpha1.ProfileDns{}
	}
	b := &builder{p: p, seen: map[[2]string]bool{}, claims: map[string]int32{}}
	// remembering the evidence the profile already has, so adding to it again doesnt repeat any
	for _, e := range p.Evidence {
		b.seen[[2]string{e.Value, e.Source}] = true
	}
	return b
}

// Build takes the objects as the redactor gave them and gives back the cluster's profile
// the objects are read in the order of their names, so the same cluster always gives the same profile
func Build(objs []*resource.Object) *snapshotv1alpha1.Profile {
	b := newBuilder(nil)
	sorted := slices.Clone(objs)
	// sorting by the object's name, the order the collector gives depends on how its lists finished
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Ref() < sorted[j].Ref() })
	// going through each object and handing it to the detector for its kind
	for _, o := range sorted {
		// invoking read to take what the object says about the cluster, its defined in network.go
		b.read(o)
	}
	// invoking finish to settle what depends on everything read, its defined below
	b.finish()
	return b.p
}

// found records one piece of evidence, what was found and where, once
func (b *builder) found(value, source string) {
	key := [2]string{value, source}
	// checking if its already recorded
	if b.seen[key] {
		return
	}
	b.seen[key] = true
	b.p.Evidence = append(b.p.Evidence, &snapshotv1alpha1.ProfileEvidence{Value: value, Source: source})
}

// set takes a field, a value, what the value is and where it was found, sets the field if its still empty and records the evidence either way
// a second source with the same value is more evidence, one with a different value is kept as evidence so a reader sees both
func (b *builder) set(field *string, value, what, source string) {
	value = strings.TrimSpace(value)
	// checking if theres a value at all
	if value == "" {
		return
	}
	// checking if the field is still empty, if so it takes the value
	if *field == "" {
		*field = value
	}
	b.found(what+" "+value, source)
}

// add takes a list, a value, what the value is and where it was found, adds the value once and records the evidence
func (b *builder) add(list *[]string, value, what, source string) {
	value = strings.TrimSpace(value)
	// checking if theres a value at all
	if value == "" {
		return
	}
	// checking if its already listed
	if !slices.Contains(*list, value) {
		*list = append(*list, value)
	}
	b.found(what+" "+value, source)
}

// addSubnet takes a subnet and where it was found and adds it once, a later sighting can fill in the zone or range an earlier one lacked
func (b *builder) addSubnet(s *snapshotv1alpha1.ProfileSubnet, source string) {
	s.Id, s.Cidr = strings.TrimSpace(s.Id), strings.TrimSpace(s.Cidr)
	// checking if theres an id or a range to go by, without either there is nothing to add
	if s.Id == "" && s.Cidr == "" {
		return
	}
	// going through the subnets already listed, looking for this one
	for _, have := range b.p.Network.Subnets {
		// checking if its the same subnet in the same use
		if have.Use == s.Use && ((s.Id != "" && have.Id == s.Id) || (s.Id == "" && have.Cidr == s.Cidr)) {
			have.Zone = cmp.Or(have.Zone, s.Zone)
			have.Cidr = cmp.Or(have.Cidr, s.Cidr)
			b.found(describeSubnet(s), source)
			return
		}
	}
	b.p.Network.Subnets = append(b.p.Network.Subnets, s)
	b.found(describeSubnet(s), source)
}

// describeSubnet gives back how a subnet reads as evidence, like "nodes subnet subnet-0abc (us-east-1a)"
func describeSubnet(s *snapshotv1alpha1.ProfileSubnet) string {
	out := s.Use + " subnet " + cmp.Or(s.Id, s.Cidr)
	// checking if both an id and a range are known, if so the range goes after the id
	if s.Id != "" && s.Cidr != "" {
		out += " " + s.Cidr
	}
	// checking if the zone is known
	if s.Zone != "" {
		out += " (" + s.Zone + ")"
	}
	return out
}

// finish settles what depends on everything read, the claims per storage class, and sorts every list so the profile reads the same each time
func (b *builder) finish() {
	// checking if its azure cni overlay and nothing gave the pod network, if so the ip-masq-agent's ranges are it
	if b.azureOverlay && len(b.p.Network.PodCidrs) == 0 {
		for _, c := range b.azureNoMasq {
			b.add(&b.p.Network.PodCidrs, c, "pod network", b.azureNoMasqSrc)
		}
	}
	def := ""
	// going through the storage classes, finding the default one and giving each its claims
	for _, sc := range b.p.StorageClasses {
		// checking if its the default class, claims that name no class get it
		if sc.Default {
			def = sc.Name
		}
	}
	// going through the storage classes again, now the default is known
	for _, sc := range b.p.StorageClasses {
		sc.Claims = b.claims[sc.Name]
		// checking if its the default class, if so it also gets the claims that named none
		if sc.Name == def {
			sc.Claims += b.claims[""]
		}
	}
	b.sort()
}

// sort puts every list of the profile in order, so the same cluster always gives the same bytes
func (b *builder) sort() {
	n := b.p.Network
	slices.Sort(b.p.Cloud.Zones)
	slices.Sort(n.PodCidrs)
	slices.Sort(n.ServiceCidrs)
	slices.Sort(n.CniSettings)
	slices.Sort(n.IpFamilies)
	slices.Sort(n.SecurityGroups)
	slices.Sort(n.ApiPublicAccessCidrs)
	slices.Sort(n.VpcCidrs)
	slices.Sort(b.p.Dns.Upstreams)
	slices.Sort(b.p.Dns.Hostnames)
	slices.Sort(b.p.Dns.ManagedDomains)
	// subnets by use, then zone, then id or range
	slices.SortFunc(n.Subnets, func(x, y *snapshotv1alpha1.ProfileSubnet) int {
		return cmp.Or(cmp.Compare(x.Use, y.Use), cmp.Compare(x.Zone, y.Zone), cmp.Compare(x.Id, y.Id), cmp.Compare(x.Cidr, y.Cidr))
	})
	slices.SortFunc(n.NodeRanges, func(x, y *snapshotv1alpha1.ProfileSubnet) int {
		return cmp.Or(cmp.Compare(x.Zone, y.Zone), cmp.Compare(x.Cidr, y.Cidr))
	})
	slices.SortFunc(b.p.StorageClasses, func(x, y *snapshotv1alpha1.ProfileStorageClass) int { return cmp.Compare(x.Name, y.Name) })
	slices.SortFunc(b.p.Components, func(x, y *snapshotv1alpha1.ProfileComponent) int {
		return cmp.Or(cmp.Compare(x.Role, y.Role), cmp.Compare(x.Name, y.Name))
	})
	slices.SortFunc(b.p.Evidence, func(x, y *snapshotv1alpha1.ProfileEvidence) int {
		return cmp.Or(cmp.Compare(x.Value, y.Value), cmp.Compare(x.Source, y.Source))
	})
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
