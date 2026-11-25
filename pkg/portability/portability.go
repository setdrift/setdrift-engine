// finds values bound to one region or cloud account, which work where they are and break when the cluster is rebuilt elsewhere
// each kind names the cloud's own page on its format, so a pattern can be checked against the source

package portability

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Kind is one sort of region- or account-bound value, with how to find it and the cloud's page on its format
type Kind struct {
	// Name words the kind for a finding, like "ECR image"
	Name string
	// Doc is the cloud's page on the value's format
	Doc string
	re  *regexp.Regexp
	// bound takes the expression's submatches and words what the value is bound to, like "us-east-2, account 111122223333"
	bound func(m []string) string
	// label, when set, takes the submatches and names the kind more closely, like "AWS ARN (kms)"
	label func(m []string) string
}

// Kinds are the region- and account-bound values found, in the order findings list them
var Kinds = []Kind{
	{
		Name: "ECR image", Doc: "https://docs.aws.amazon.com/AmazonECR/latest/userguide/Registries.html",
		re:    regexp.MustCompile(`\b(\d{12})\.dkr\.ecr\.([a-z]{2}(?:-[a-z]+)+-\d)\.amazonaws\.com(?:\.cn)?\b`),
		bound: func(m []string) string { return m[2] + ", account " + m[1] },
	},
	{
		Name: "AWS ARN", Doc: "https://docs.aws.amazon.com/IAM/latest/UserGuide/reference-arns.html",
		re: regexp.MustCompile(`\barn:aws(?:-cn|-us-gov)?:(acm|kms|sqs|sns|secretsmanager|iam|elasticloadbalancing|rds|dynamodb|lambda|s3):([a-z0-9-]*):(\d{12})?:[^\s"',]+`),
		bound: func(m []string) string {
			// checking if the ARN names a region, IAM's dont, so theyre bound to the account only
			if m[2] == "" {
				return "account " + m[3]
			}
			return m[2] + ", account " + m[3]
		},
		label: func(m []string) string { return "AWS ARN (" + m[1] + ")" },
	},
	{
		Name: "AWS subnet ID", Doc: "https://docs.aws.amazon.com/vpc/latest/userguide/configure-subnets.html",
		re:    regexp.MustCompile(`\bsubnet-[0-9a-f]{8}(?:[0-9a-f]{9})?\b`),
		bound: func([]string) string { return "one VPC's subnet" },
	},
	{
		Name: "AWS security group ID", Doc: "https://docs.aws.amazon.com/vpc/latest/userguide/vpc-security-groups.html",
		re:    regexp.MustCompile(`\bsg-[0-9a-f]{8}(?:[0-9a-f]{9})?\b`),
		bound: func([]string) string { return "one VPC's security group" },
	},
	{
		Name: "EKS OIDC issuer", Doc: "https://docs.aws.amazon.com/eks/latest/userguide/enable-iam-roles-for-service-accounts.html",
		re:    regexp.MustCompile(`\boidc\.eks\.([a-z]{2}(?:-[a-z]+)+-\d)\.amazonaws\.com/id/[0-9A-F]{32}\b`),
		bound: func(m []string) string { return "one EKS cluster, " + m[1] },
	},
	{
		Name: "Azure resource ID", Doc: "https://learn.microsoft.com/azure/azure-resource-manager/management/resource-name-rules",
		re:    regexp.MustCompile(`(?i)/subscriptions/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/resourceGroups/([^/\s"']+)`),
		bound: func(m []string) string { return "subscription " + m[1] + ", resource group " + m[2] },
	},
	{
		Name: "Azure Key Vault URL", Doc: "https://learn.microsoft.com/azure/key-vault/general/about-keys-secrets-certificates",
		re:    regexp.MustCompile(`\bhttps://([a-z0-9-]{3,24})\.vault\.azure\.net\b`),
		bound: func(m []string) string { return "Key Vault " + m[1] },
	},
	{
		Name: "Artifact Registry image", Doc: "https://cloud.google.com/artifact-registry/docs/docker/names",
		re:    regexp.MustCompile(`\b([a-z]+(?:-[a-z]+)*\d?)-docker\.pkg\.dev/([a-z][a-z0-9-]{4,28}[a-z0-9])/`),
		bound: func(m []string) string { return m[1] + ", project " + m[2] },
	},
}

// Match is one region- or account-bound value found in a string
type Match struct {
	Kind  string
	Value string
	// Bound words what the value is bound to, like "us-east-2, account 111122223333"
	Bound string
	Doc   string
}

// Find takes a string and gives back each region- or account-bound value in it
func Find(s string) []Match {
	var out []Match
	// going through each kind, each value can be found by one or more
	for _, k := range Kinds {
		// going through each place the kind is found
		for _, m := range k.re.FindAllStringSubmatch(s, -1) {
			kind := k.Name
			// checking if the kind names itself more closely from the value, if so thats its name
			if k.label != nil {
				kind = k.label(m)
			}
			out = append(out, Match{Kind: kind, Value: m[0], Bound: k.bound(m), Doc: k.Doc})
		}
	}
	return out
}

// SameKinds tells if two values each hold region- or account-bound values, and of the same kinds, like one subnet ID and another
// a difference between them is the clusters' own infrastructure, not a change to the workload
func SameKinds(a, b string) bool {
	ka, kb := kinds(Find(a)), kinds(Find(b))
	return len(ka) > 0 && strings.Join(ka, ",") == strings.Join(kb, ",")
}

// kinds gives back the kinds of the matches, sorted and once each
func kinds(ms []Match) []string {
	seen := map[string]bool{}
	var out []string
	// going through each match and keeping its kind once
	for _, m := range ms {
		// checking if the kind is already kept
		if !seen[m.Kind] {
			seen[m.Kind] = true
			out = append(out, m.Kind)
		}
	}
	sort.Strings(out)
	return out
}

// Finding is a region- or account-bound value in an object, where it was found
type Finding struct {
	// Object names the object, like "Deployment sd-apps/shop-worker"
	Object string
	// Path is the field, like spec.template.spec.containers[0].image
	Path string
	Match
}

// Scan takes an object's name and its manifest and gives back each region- or account-bound value in its string fields
func Scan(object string, manifest map[string]any) []Finding {
	var out []Finding
	walk("", manifest, func(path, s string) {
		// going through each bound value in the string
		for _, m := range Find(s) {
			out = append(out, Finding{Object: object, Path: path, Match: m})
		}
	})
	return out
}

// walk calls fn with the path and value of each string in v, depth first, map keys sorted
func walk(path string, v any, fn func(path, s string)) {
	switch t := v.(type) {
	case string:
		fn(path, t)
	case map[string]any:
		keys := make([]string, 0, len(t))
		// collecting the keys so the walk is in a stable order
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		// going into each value, the key added to the path
		for _, k := range keys {
			p := k
			// checking if theres a path already, if so the key is added after a dot
			if path != "" {
				p = path + "." + k
			}
			walk(p, t[k], fn)
		}
	case []any:
		// going into each item, its index added to the path
		for i, item := range t {
			walk(fmt.Sprintf("%s[%d]", path, i), item, fn)
		}
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
