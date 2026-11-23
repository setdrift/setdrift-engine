// tests that the permission templates grant what the docs say and the code uses, the AWS ones exactly the calls export --aws makes, and that the docs link to every one

package permissions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"setdrift.io/setdrift-engine/pkg/awsinventory"
)

func awsCalls() []string {
	var calls []string
	for prefix, iface := range map[string]reflect.Type{
		"eks": reflect.TypeFor[awsinventory.EKS](), "iam": reflect.TypeFor[awsinventory.IAM](), "ec2": reflect.TypeFor[awsinventory.EC2](),
	} {
		for i := range iface.NumMethod() {
			calls = append(calls, prefix+":"+iface.Method(i).Name)
		}
	}
	slices.Sort(calls)
	return calls
}

type statement struct {
	Sid      string
	Action   any
	Resource any
}

func actions(t *testing.T, sts []statement) []string {
	t.Helper()
	var out []string
	for _, st := range sts {
		var these []string
		switch a := st.Action.(type) {
		case string:
			these = []string{a}
		case []any:
			for _, x := range a {
				these = append(these, x.(string))
			}
		}
		if st.Resource == "*" && slices.ContainsFunc(these, func(a string) bool { return !strings.HasPrefix(a, "ec2:Describe") }) {
			t.Errorf("statement %s gives Resource * to %v; only ec2:Describe actions take no resource", st.Sid, these)
		}
		out = append(out, these...)
	}
	slices.Sort(out)
	return out
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// check that the IAM policy file allows exactly the calls export --aws makes, and is the policy docs/permissions-aws.md shows
func TestAWSPolicy(t *testing.T) {
	var p struct{ Statement []statement }
	file := read(t, "aws/setdrift-export-iam-policy.json")
	if err := json.Unmarshal(file, &p); err != nil {
		t.Fatal(err)
	}
	if got := actions(t, p.Statement); !slices.Equal(got, awsCalls()) {
		t.Errorf("the policy allows %v, the code calls %v", got, awsCalls())
	}
	m := regexp.MustCompile("(?s)```json\n(.*?)\n```").FindSubmatch(read(t, "../../docs/permissions-aws.md"))
	if m == nil {
		t.Fatal("docs/permissions-aws.md has no policy")
	}
	var inDoc, inFile any
	if json.Unmarshal(m[1], &inDoc) != nil || json.Unmarshal(file, &inFile) != nil || !reflect.DeepEqual(inDoc, inFile) {
		t.Error("aws/setdrift-export-iam-policy.json differs from the policy in docs/permissions-aws.md")
	}
}

// check that the CloudFormation template's policy allows exactly the calls export --aws makes, and its access entry gives one of the two SetDrift groups
func TestAWSCloudFormation(t *testing.T) {
	var cfn struct {
		Parameters map[string]struct{ AllowedValues []string }
		Resources  map[string]struct {
			Type       string
			Properties struct {
				PolicyDocument struct{ Statement []statement }
			}
		}
	}
	if err := json.Unmarshal(read(t, "aws/setdrift-export.cfn.json"), &cfn); err != nil {
		t.Fatal(err)
	}
	if got := actions(t, cfn.Resources["ExportPolicy"].Properties.PolicyDocument.Statement); !slices.Equal(got, awsCalls()) {
		t.Errorf("the template's policy allows %v, the code calls %v", got, awsCalls())
	}
	if cfn.Resources["AccessEntry"].Type != "AWS::EKS::AccessEntry" || !slices.Equal(cfn.Parameters["KubernetesGroup"].AllowedValues, []string{"setdrift-export", "setdrift-verify"}) {
		t.Errorf("access entry %+v, groups %v", cfn.Resources["AccessEntry"], cfn.Parameters["KubernetesGroup"].AllowedValues)
	}
}

// check that each Kubernetes role grants exactly its verbs on everything, and binds the group of its own name
func TestKubernetesRoles(t *testing.T) {
	for file, verbs := range map[string][]string{
		"kubernetes/setdrift-export.yaml":           {"get", "list"},
		"kubernetes/setdrift-export-namespace.yaml": {"get", "list"},
		"kubernetes/setdrift-verify.yaml":           {"get", "list", "patch"},
		"kubernetes/setdrift-verify-namespace.yaml": {"get", "list", "patch"},
	} {
		docs := strings.Split(string(read(t, file)), "\n---\n")
		if len(docs) != 2 {
			t.Fatalf("%s holds %d documents, want a role and its binding", file, len(docs))
		}
		var role struct {
			Kind     string
			Metadata struct{ Name, Namespace string }
			Rules    []struct{ APIGroups, Resources, Verbs []string }
		}
		var binding struct {
			Kind     string
			Metadata struct{ Namespace string }
			RoleRef  struct{ Kind, Name string }
			Subjects []struct{ Kind, Name string }
		}
		if err := yaml.Unmarshal([]byte(docs[0]), &role); err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal([]byte(docs[1]), &binding); err != nil {
			t.Fatal(err)
		}
		if len(role.Rules) != 1 || !slices.Equal(role.Rules[0].Verbs, verbs) || !slices.Equal(role.Rules[0].APIGroups, []string{"*"}) || !slices.Equal(role.Rules[0].Resources, []string{"*"}) {
			t.Errorf("%s: rules %+v, want %v on everything", file, role.Rules, verbs)
		}
		if binding.RoleRef.Kind != role.Kind || binding.RoleRef.Name != role.Metadata.Name || len(binding.Subjects) != 1 || binding.Subjects[0].Name != role.Metadata.Name {
			t.Errorf("%s: binding %+v doesn't bind %s to the group %s", file, binding, role.Metadata.Name, role.Metadata.Name)
		}
		if role.Metadata.Namespace != "" || binding.Metadata.Namespace != "" {
			t.Errorf("%s names a namespace; the namespace ones take it from kubectl apply -n", file)
		}
	}
}

// check that the Azure roles grant the kubeconfig actions, that only verify can write, and that each data action is one docs/permissions-azure.md lists
func TestAzureRoles(t *testing.T) {
	var arm struct {
		Variables map[string][]string
		Resources []struct {
			Properties struct {
				RoleName    string
				Permissions []struct {
					Actions     any
					DataActions []string
				}
			}
		}
	}
	if err := json.Unmarshal(read(t, "azure/setdrift-roles.json"), &arm); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(arm.Variables["clusterAccess"], []string{"Microsoft.ContainerService/managedClusters/read", "Microsoft.ContainerService/managedClusters/listClusterUserCredential/action"}) {
		t.Errorf("kubeconfig actions %v", arm.Variables["clusterAccess"])
	}
	doc := string(read(t, "../../docs/permissions-azure.md"))
	roles := map[string][]string{}
	for _, r := range arm.Resources {
		roles[r.Properties.RoleName] = r.Properties.Permissions[0].DataActions
		for _, a := range r.Properties.Permissions[0].DataActions {
			if !strings.Contains(doc, "`"+a+"`") {
				t.Errorf("%s's data action %s is not in docs/permissions-azure.md", r.Properties.RoleName, a)
			}
		}
	}
	if len(roles["SetDrift Cluster Access"]) != 0 || slices.ContainsFunc(roles["SetDrift Export"], func(a string) bool { return strings.HasSuffix(a, "/write") }) || !slices.Contains(roles["SetDrift Verify"], "Microsoft.ContainerService/managedClusters/*/write") {
		t.Errorf("roles %v: cluster access has no data actions, export no writes, verify writes", roles)
	}
}

// check that each Google Cloud role holds only permissions docs/permissions-gcp.md lists
func TestGCPRoles(t *testing.T) {
	doc := string(read(t, "../../docs/permissions-gcp.md"))
	for file, want := range map[string][]string{
		"gcp/setdrift-cluster-access-role.yaml": {"container.clusters.get"},
		"gcp/setdrift-secrets-reader-role.yaml": {"container.secrets.list"},
	} {
		var role struct {
			Stage               string
			IncludedPermissions []string `json:"includedPermissions"`
		}
		if err := yaml.Unmarshal(read(t, file), &role); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(role.IncludedPermissions, want) || role.Stage != "GA" {
			t.Errorf("%s: %v, want %v", file, role.IncludedPermissions, want)
		}
		for _, p := range role.IncludedPermissions {
			if !strings.Contains(doc, "`"+p+"`") {
				t.Errorf("%s's %s is not in docs/permissions-gcp.md", file, p)
			}
		}
	}
}

// check that the docs link to every template, so none is left for a reader to find alone
func TestDocsLinkEveryTemplate(t *testing.T) {
	var docs string
	pages, _ := filepath.Glob("../../docs/permissions*.md")
	for _, p := range pages {
		docs += string(read(t, p))
	}
	files, _ := filepath.Glob("*/*")
	for _, f := range files {
		if !strings.Contains(docs, "deploy/permissions/"+filepath.ToSlash(f)) {
			t.Errorf("no docs/permissions*.md page links deploy/permissions/%s", f)
		}
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
