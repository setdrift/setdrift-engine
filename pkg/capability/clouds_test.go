package capability

import (
	"slices"
	"strings"
	"testing"

	"setdrift.io/setdrift-engine/pkg/platform"
)

// a platform or cloud missing a required setting, or naming a strategy SetDrift doesnt know, fails when the map loads
// so a new one thats incomplete is caught here, not with a generic answer in someone's translation
func TestCheckRefusesWhatsMissing(t *testing.T) {
	for name, spoil := range map[string]func(m *Map){
		"no cloud":                func(m *Map) { m.Platforms[0].Cloud = "" },
		"an unknown cloud":        func(m *Map) { m.Platforms[0].Cloud = "oracle" },
		"no disk name":            func(m *Map) { m.Platforms[0].Translate.DiskName = "" },
		"no create command":       func(m *Map) { m.Platforms[0].Translate.CreateCommand = "" },
		"an unknown image copy":   func(m *Map) { m.Platforms[0].Translate.Images.Strategy = "rsync" },
		"no enforcement wording":  func(m *Map) { m.Platforms[0].Translate.NetworkPolicy.HowToEnforce = "" },
		"a shared alias":          func(m *Map) { m.Platforms[1].Aliases = append(m.Platforms[1].Aliases, m.Platforms[0].Aliases[0]) },
		"a tier naming no tier":   func(m *Map) { m.Platforms[0].Translate.Storage[0].Fallback = "tape" },
		"no federation":           func(m *Map) { m.Clouds[0].Identity.Federation = "" },
		"a token without a path":  func(m *Map) { m.Clouds[0].Identity.TokenDir = "" },
		"an unknown trust":        func(m *Map) { m.Clouds[0].Identity.Trust = "hope" },
		"an unknown bucket":       func(m *Map) { m.Clouds[0].Buckets = "s4" },
		"a registry without sign": func(m *Map) { m.Clouds[0].Registry.Login.Password = "" },
	} {
		m, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		spoil(m)
		if err := m.check(); err == nil {
			t.Errorf("%s: the map loaded", name)
		}
	}
}

// none is a value a platform can say explicitly, for what it has no such thing for, like a platform with no cloud
func TestCheckTakesNone(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	m.Platforms[0].Cloud = None
	m.Clouds[0].Buckets, m.Clouds[0].SecretManager.Format = None, None
	if err := m.check(); err != nil {
		t.Error(err)
	}
}

// what a platform writes is its own, every class, provisioner, driver and label key a translation to it writes, the platform's markers own
// otherwise an object translated to it would read as no platform's, and a translation away from it would leave it as it was, on a cluster where nothing serves it
func TestEachPlatformOwnsWhatItWrites(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range m.Platforms {
		owns := func(what, v string, owners []platform.Distribution) {
			if v != "" && !slices.Contains(owners, p.Distribution) {
				t.Errorf("%s writes %s %s, and its markers don't own it", p.Distribution, what, v)
			}
		}
		tr := p.Translate
		for _, st := range tr.Storage {
			owns("provisioner", st.Class.Provisioner, m.ProvisionerOwners(st.Class.Provisioner))
		}
		owns("snapshot driver", tr.SnapshotDriver, m.ProvisionerOwners(tr.SnapshotDriver))
		owns("load balancer class", tr.LoadBalancer.Class, m.LoadBalancerClassOwners(tr.LoadBalancer.Class))
		for _, ann := range []map[string]string{tr.LoadBalancer.Internal, tr.LoadBalancer.External} {
			for k := range ann {
				owns("Service annotation", k, m.ServiceAnnotationOwners(k))
			}
		}
		for _, cls := range []string{tr.Ingress.ClassName, tr.Ingress.InternalClass, tr.Ingress.ExternalClass} {
			owns("ingress class", cls, m.IngressClassOwners(cls))
		}
		for _, ann := range []map[string]string{tr.Ingress.Internal, tr.Ingress.External} {
			for k := range ann {
				owns("Ingress annotation", k, m.IngressAnnotationOwners(k))
			}
		}
		if len(tr.Nodes.Pools) > 0 {
			owns("node pool label", tr.Nodes.Pools[0], m.NodeLabelOwners(tr.Nodes.Pools[0]))
		}
		if len(tr.Nodes.Spot) > 0 {
			owns("spot label", tr.Nodes.Spot[0].Key, m.NodeLabelOwners(tr.Nodes.Spot[0].Key))
		}
		if c := m.CloudOf(p.Distribution); c != nil {
			owns("ServiceAccount annotation", c.Identity.Annotation, m.ServiceAccountAnnotationOwners(c.Identity.Annotation))
			owns("secret provider", c.SecretManager.Provider, m.SecretProviderClassOwners(c.SecretManager.Provider))
		}
	}
}

// --to reads a platform's distribution or its aliases, without regard to case, and the names are the map's
func TestDistributionAndNames(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]platform.Distribution{"EKS": platform.EKS, " gcp ": platform.GKE, "google-cloud": platform.GKE, "azure": platform.AKS} {
		if got, ok := m.Distribution(in); !ok || got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
	if _, ok := m.Distribution("openshift"); ok {
		t.Error("openshift read as a platform")
	}
	if got := m.Names("or"); !strings.Contains(got, "eks") || !strings.Contains(got, " or ") {
		t.Errorf("names %q", got)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
