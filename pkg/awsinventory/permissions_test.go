// checks docs/permissions/aws.md lists exactly the AWS calls export --aws can make, in its table and in its example policy

package awsinventory

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// check that the AWS actions docs/permissions/aws.md documents are exactly the calls the EKS, IAM and EC2 interfaces allow, in the table and in the policy
func TestPermissionsDocMatchesCalls(t *testing.T) {
	var calls []string
	// the interfaces are the only way Collect reaches AWS, so their methods are every call it can make
	for prefix, iface := range map[string]reflect.Type{
		"eks": reflect.TypeFor[EKS](), "iam": reflect.TypeFor[IAM](), "ec2": reflect.TypeFor[EC2](),
	} {
		for i := range iface.NumMethod() {
			calls = append(calls, prefix+":"+iface.Method(i).Name)
		}
	}
	slices.Sort(calls)

	b, err := os.ReadFile("../../docs/permissions/aws.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	// the actions in the table, one per row starting with a backticked action
	var table []string
	for _, m := range regexp.MustCompile("(?m)^\\| `((?:eks|iam|ec2|sts|[a-z0-9]+):[A-Za-z]+)` \\|").FindAllStringSubmatch(doc, -1) {
		table = append(table, m[1])
	}
	slices.Sort(table)
	if !slices.Equal(table, calls) {
		t.Errorf("docs/permissions/aws.md's AWS table lists %v, the code calls %v", table, calls)
	}

	// the actions in the example policy
	m := regexp.MustCompile("(?s)```json\n(.*?)\n```").FindStringSubmatch(doc)
	if m == nil {
		t.Fatal("docs/permissions/aws.md has no example policy")
	}
	var policy struct {
		Statement []struct {
			Action   json.RawMessage
			Resource json.RawMessage
		}
	}
	if err := json.Unmarshal([]byte(m[1]), &policy); err != nil {
		t.Fatalf("the example policy is not valid JSON: %v", err)
	}
	var inPolicy []string
	for _, st := range policy.Statement {
		var one string
		var many []string
		// an Action is one string or a list of them
		if json.Unmarshal(st.Action, &one) == nil {
			many = []string{one}
		} else if err := json.Unmarshal(st.Action, &many); err != nil {
			t.Fatal(err)
		}
		inPolicy = append(inPolicy, many...)
		// EC2's Describe calls take no resource, so only a statement of those may allow every resource
		if strings.Contains(string(st.Resource), `"*"`) && slices.ContainsFunc(many, func(a string) bool { return !strings.HasPrefix(a, "ec2:Describe") }) {
			t.Errorf("only ec2:Describe actions may have Resource *, not %v", many)
		}
	}
	slices.Sort(inPolicy)
	if !slices.Equal(inPolicy, calls) {
		t.Errorf("the example policy allows %v, the code calls %v", inPolicy, calls)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
