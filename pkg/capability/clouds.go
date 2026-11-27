// what belongs to a cloud wherever a Kubernetes platform on it runs, its workload identity, image registry, secret manager and object storage, one YAML file per cloud under clouds/
// and the strategies: where clouds or platforms do something by a different mechanism, not only with different values, the YAML names one of these, and translate implements each
// a new platform names its cloud, or none, and picks its strategies, a name SetDrift doesnt know fails when the map loads

package capability

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"setdrift.io/setdrift-engine/pkg/platform"
)

// None is the value of a cloud or a strategy a platform has no such thing for, said explicitly so a missing one fails when the map loads
const None = "none"

// the strategies, by what they do, each a mechanism translate implements, Strategies lists them for the map's checks and translate's test
const (
	// ProjectedToken federates an identity off its own platform with a projected ServiceAccount token and the environment its SDK reads, CredentialFile with a credential configuration file the user writes
	ProjectedToken = "projected-token"
	CredentialFile = "credential-file"
	// the ways a cloud trusts a new cluster's OIDC issuer for an identity, written as RESTORE.md's steps
	TrustAWSIAMRole               = "aws-iam-role"
	TrustAzureFederatedCredential = "azure-federated-credential"
	TrustGCPWorkloadIdentityPool  = "gcp-workload-identity-pool"
	SecretsAWSObjects             = "aws-objects"
	SecretsAzureObjects           = "azure-objects"
	SecretsGCPSecrets             = "gcp-secrets"
	BucketsACKS3                  = "ack-s3"
	BucketsAzureServiceOperator   = "azure-service-operator"
	BucketsConfigConnector        = "config-connector"
	ImagesCrane                   = "crane"
	ImagesACRImport               = "acr-import"
	CreateAzAKS                   = "az-aks-create"
)

// Strategies are the strategy names the map takes for each kind of strategy, None included where a platform can have none
var Strategies = map[string][]string{
	"identity.federation":     {ProjectedToken, CredentialFile, None},
	"identity.trust":          {TrustAWSIAMRole, TrustAzureFederatedCredential, TrustGCPWorkloadIdentityPool, None},
	"secretManager.format":    {SecretsAWSObjects, SecretsAzureObjects, SecretsGCPSecrets, None},
	"buckets":                 {BucketsACKS3, BucketsAzureServiceOperator, BucketsConfigConnector, None},
	"translate.images":        {ImagesCrane, ImagesACRImport, None},
	"translate.createCommand": {CreateAzAKS, None},
}

// Cloud is one cloud's entry, read from its YAML file
type Cloud struct {
	// ID is how platforms name it, like aws, Name how its written for people, like AWS
	ID   string `json:"id"`
	Name string `json:"name"`
	// Identity is its workload identity, how a ServiceAccount names one and how it works off its own platform
	Identity CloudIdentity `json:"identity"`
	// Registry is its private image registry
	Registry Registry `json:"registry"`
	// SecretManager is how a Secrets Store CSI class names its secrets for this cloud
	SecretManager SecretManager `json:"secretManager"`
	// Buckets is the strategy that writes this cloud's object storage bucket, for equivalent
	Buckets string `json:"buckets"`
}

// CloudIdentity is a cloud's workload identity
type CloudIdentity struct {
	// Annotation names the identity on a ServiceAccount, TenantAnnotation its tenant where the cloud has one
	Annotation       string `json:"annotation"`
	TenantAnnotation string `json:"tenantAnnotation,omitempty"`
	// Federation is how a Pod uses the identity off its own platform, a strategy
	Federation string `json:"federation"`
	// Webhook is the platform whose webhook a projected token stands in for, like eks
	Webhook platform.Distribution `json:"webhook,omitempty"`
	// Audience, TokenDir and TokenFile are the projected token's, Env the environment that names the identity and the token
	Audience  string   `json:"audience,omitempty"`
	TokenDir  string   `json:"tokenDir,omitempty"`
	TokenFile string   `json:"tokenFile,omitempty"`
	Env       []EnvVar `json:"env,omitempty"`
	// Trust is how RESTORE.md has the cloud trust a new cluster's OIDC issuer, a strategy
	Trust string `json:"trust"`
}

// EnvVar is an environment variable a federation injects, its Value may hold {id}, {tenant}, {tokenPath} and {region}
// an entry whose value comes out empty is left out, or written as Placeholder with a note to fill it in when it has one
type EnvVar struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Placeholder string `json:"placeholder,omitempty"`
}

// Registry is a cloud's private image registry
type Registry struct {
	// Hosts are patterns of its hosts, a first group in one is the registry's region
	Hosts []string `json:"hosts"`
	// Login is how to sign in to pull from it
	Login RegistryLogin `json:"login"`
	// PublicRepositories are well-known public repositories on its hosts, by prefix, pulled from anywhere without credentials
	PublicRepositories []string `json:"publicRepositories,omitempty"`
	hosts              []*regexp.Regexp
}

// RegistryLogin is a registry's user name and the command that prints a password, Password may hold {region} and {name}, the host's first label
type RegistryLogin struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// Match tells if a host is this registry's, and gives back the region it names, empty when it names none
func (r *Registry) Match(host string) (region string, ok bool) {
	for _, re := range r.hosts {
		if m := re.FindStringSubmatch(host); m != nil {
			if len(m) > 1 {
				region = m[1]
			}
			return region, true
		}
	}
	return "", false
}

// SecretManager is how a Secrets Store CSI class names its secrets for a cloud
type SecretManager struct {
	// Format is the strategy that writes the class's parameters, Provider the class's provider
	Format   string `json:"format"`
	Provider string `json:"provider,omitempty"`
	// Parameters are the parameters every class names, placeholders for the user to fill in
	Parameters map[string]string `json:"parameters,omitempty"`
	// NameCharacters are the characters a secret name may have, as a regular expression's class, anything else becomes a dash, empty when any name is allowed
	NameCharacters string `json:"nameCharacters,omitempty"`
	notAllowed     *regexp.Regexp
}

// SecretName renames a secret for this secret manager, every character it doesnt allow becomes a dash
func (s *SecretManager) SecretName(n string) string {
	if s.notAllowed == nil {
		return n
	}
	return s.notAllowed.ReplaceAllString(n, "-")
}

// ImageCopy is how RESTORE.md copies images to a platform's registry
type ImageCopy struct {
	// Strategy is how they're copied, Setup the lines before, naming and signing in to the registry
	Strategy string   `json:"strategy"`
	Setup    []string `json:"setup,omitempty"`
	// CreateRepository is the command that creates a repository before copying to it, {repository} is its name, empty when the registry needs none
	CreateRepository string `json:"createRepository,omitempty"`
}

// NetworkPolicy is how a platform enforces NetworkPolicies
type NetworkPolicy struct {
	// HowToEnforce says how to switch enforcement on, for the messages and RESTORE.md
	HowToEnforce string `json:"howToEnforce"`
}

// OwnObject is one of a platform's own objects, which nothing reads on another, so a translation to another leaves it out
type OwnObject struct {
	// Group, Kind, Namespace and Name are the object's, the group empty for the core group
	Group     string `json:"group,omitempty"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// What names it for people, Then says what to do instead on the target
	What string `json:"what"`
	Then string `json:"then,omitempty"`
}

// CloudOf gives back a platform's cloud, nil when it has none or the map doesnt know the platform
func (m *Map) CloudOf(d platform.Distribution) *Cloud {
	p := m.For(d)
	if p == nil {
		return nil
	}
	return m.Cloud(p.Cloud)
}

// Cloud gives back the cloud with an id, like aws, nil when theres none
func (m *Map) Cloud(id string) *Cloud {
	for i := range m.Clouds {
		if m.Clouds[i].ID == id {
			return &m.Clouds[i]
		}
	}
	return nil
}

// CloudName gives back how a platform's cloud is written for people, like AWS, the platform's own name when it has no cloud
func (m *Map) CloudName(d platform.Distribution) string {
	if c := m.CloudOf(d); c != nil {
		return c.Name
	}
	return m.name(d)
}

// IdentityCloud gives back the cloud a ServiceAccount annotation names an identity of, nil when its none
func (m *Map) IdentityCloud(annotation string) *Cloud {
	for i := range m.Clouds {
		if m.Clouds[i].Identity.Annotation == annotation {
			return &m.Clouds[i]
		}
	}
	return nil
}

// RegistryCloud gives back the cloud whose private registry a host is, and the region the host names, nil when its no cloud's
func (m *Map) RegistryCloud(host string) (*Cloud, string) {
	for i := range m.Clouds {
		if region, ok := m.Clouds[i].Registry.Match(host); ok {
			return &m.Clouds[i], region
		}
	}
	return nil, ""
}

// Distribution reads a platform as --to names it, its distribution or an alias, without regard to case, ok false when no platform has it
func (m *Map) Distribution(s string) (platform.Distribution, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, p := range m.Platforms {
		if string(p.Distribution) == s || slices.Contains(p.Aliases, s) {
			return p.Distribution, true
		}
	}
	return platform.Unknown, false
}

// Names lists the platforms, their distributions, joined for people with the last after conj, like aks, eks or gke
func (m *Map) Names(conj string) string {
	var ds []string
	for _, p := range m.Platforms {
		ds = append(ds, string(p.Distribution))
	}
	return join(ds, conj)
}

// AliasNames lists every platform's aliases, comma-separated, like aws, azure, gcp
func (m *Map) AliasNames() string {
	var as []string
	for _, p := range m.Platforms {
		if len(p.Aliases) > 0 {
			as = append(as, p.Aliases[0])
		}
	}
	return strings.Join(as, ", ")
}

// HowToEnforce says how a platform switches NetworkPolicy enforcement on, by distribution name, a generic answer for a platform the map doesnt know
func (m *Map) HowToEnforce(d string) string {
	if p := m.For(platform.Distribution(d)); p != nil {
		return p.Translate.NetworkPolicy.HowToEnforce
	}
	return "run a network policy engine, like Calico or Cilium"
}

// join writes a list for people, a, b conj c
func join(xs []string, conj string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " " + conj + " " + xs[len(xs)-1]
}

// check checks the map once its read, every platform's and cloud's required settings are there and every strategy is one SetDrift knows
// so a new platform or cloud thats missing one fails here, when the map loads, not with a generic answer in a translation
//
//revive:disable-next-line:cognitive-complexity one independent rule per setting, clearest in one place
func (m *Map) check() error {
	var errs []string
	bad := func(where, format string, args ...any) { errs = append(errs, where+": "+fmt.Sprintf(format, args...)) }
	strategy := func(where, kind, v string) {
		if !slices.Contains(Strategies[kind], v) {
			bad(where, "%s is %q, which must be one of %s", kind, v, strings.Join(Strategies[kind], ", "))
		}
	}
	for i := range m.Clouds {
		c := &m.Clouds[i]
		w := "clouds/" + c.ID
		if c.ID == "" || c.ID == None || c.Name == "" {
			bad(w, "id and name are required, and the id can't be %s", None)
		}
		id := c.Identity
		strategy(w, "identity.federation", id.Federation)
		strategy(w, "identity.trust", id.Trust)
		if id.Federation != None && id.Annotation == "" {
			bad(w, "identity.annotation is required with a federation")
		}
		if id.Federation == ProjectedToken {
			if m.For(id.Webhook) == nil || id.Audience == "" || id.TokenDir == "" || id.TokenFile == "" || len(id.Env) == 0 {
				bad(w, "a %s federation needs webhook (a platform), audience, tokenDir, tokenFile and env", ProjectedToken)
			}
		}
		c.Registry.hosts = nil
		for _, h := range c.Registry.Hosts {
			re, err := regexp.Compile(h)
			if err != nil {
				bad(w, "registry host %q: %v", h, err)
				continue
			}
			c.Registry.hosts = append(c.Registry.hosts, re)
		}
		if len(c.Registry.Hosts) > 0 && (c.Registry.Login.User == "" || c.Registry.Login.Password == "") {
			bad(w, "registry.login needs user and password")
		}
		strategy(w, "secretManager.format", c.SecretManager.Format)
		if c.SecretManager.Format != None && c.SecretManager.Provider == "" {
			bad(w, "secretManager.provider is required with a format")
		}
		if cs := c.SecretManager.NameCharacters; cs != "" {
			re, err := regexp.Compile("[^" + cs + "]")
			if err != nil {
				bad(w, "secretManager.nameCharacters %q: %v", cs, err)
			}
			c.SecretManager.notAllowed = re
		}
		strategy(w, "buckets", c.Buckets)
	}
	seen := map[string]string{}
	for _, p := range m.Platforms {
		w := "platforms/" + string(p.Distribution)
		if p.Distribution == "" || p.Name == "" || p.Code == "" {
			bad(w, "distribution, name and code are required")
		}
		for _, n := range append([]string{string(p.Distribution), "code " + p.Code}, p.Aliases...) {
			if other, dup := seen[n]; dup {
				bad(w, "%s is also %s's", n, other)
			}
			seen[n] = string(p.Distribution)
		}
		if p.Cloud != None && m.Cloud(p.Cloud) == nil {
			bad(w, "cloud is %q, which must be a cloud under clouds/ or %s", p.Cloud, None)
		}
		tr := p.Translate
		if tr.Dir == "" || tr.DiskName == "" || tr.OIDCIssuerCommand == "" || tr.NetworkPolicy.HowToEnforce == "" {
			bad(w, "translate needs dir, diskName, oidcIssuerCommand and networkPolicy.howToEnforce")
		}
		strategy(w, "translate.images", tr.Images.Strategy)
		strategy(w, "translate.createCommand", tr.CreateCommand)
		tiers := map[string]bool{}
		for _, st := range tr.Storage {
			tiers[st.Tier] = true
		}
		for _, st := range tr.Storage {
			for _, ref := range []string{st.Promote, st.Fallback} {
				if ref != "" && !tiers[ref] {
					bad(w, "storage tier %s names tier %s, which the platform doesn't have", st.Tier, ref)
				}
			}
		}
	}
	if len(errs) > 0 {
		return errors.New("capability map: " + strings.Join(errs, "; "))
	}
	return nil
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
