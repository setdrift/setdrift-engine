// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package secretexport

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

type fakeSTS struct{ arn string }

func (f fakeSTS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{Account: aws.String("111122223333"), Arn: aws.String(f.arn)}, nil
}

type fakeIAM struct {
	deny     map[string]bool // actions denied
	simulate bool            // false: simulating is refused
	path     string
}

func (f fakeIAM) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	return &iam.GetRoleOutput{Role: &iamtypes.Role{Arn: aws.String("arn:aws:iam::111122223333:role" + f.path + aws.ToString(in.RoleName))}}, nil
}
func (f fakeIAM) SimulatePrincipalPolicy(_ context.Context, in *iam.SimulatePrincipalPolicyInput, _ ...func(*iam.Options)) (*iam.SimulatePrincipalPolicyOutput, error) {
	if !f.simulate {
		return nil, &smithy.GenericAPIError{Code: "AccessDenied", Message: "no"}
	}
	out := &iam.SimulatePrincipalPolicyOutput{}
	for _, a := range in.ActionNames {
		for _, r := range in.ResourceArns {
			d := iamtypes.PolicyEvaluationDecisionTypeAllowed
			if f.deny[a] {
				d = iamtypes.PolicyEvaluationDecisionTypeImplicitDeny
			}
			out.EvaluationResults = append(out.EvaluationResults, iamtypes.EvaluationResult{EvalActionName: aws.String(a), EvalResourceName: aws.String(r), EvalDecision: d})
		}
	}
	return out, nil
}

func TestIdentityAndSimulate(t *testing.T) {
	ctx := context.Background()
	c, err := Identity(ctx, fakeSTS{"arn:aws:sts::111122223333:assumed-role/admin/me"}, fakeIAM{path: "/team/"})
	if err != nil || c.Principal != "arn:aws:iam::111122223333:role/team/admin" {
		t.Errorf("assumed role: %+v %v", c, err)
	}
	keys := []string{"prod/shop/db", "prod/shop/api"}
	if p := Simulate(ctx, fakeIAM{simulate: true}, c, SecretsManagerID, "us-east-2", keys, ""); !p.Simulated || len(p.Denied) != 0 {
		t.Errorf("allowed: %+v", p)
	}
	p := Simulate(ctx, fakeIAM{simulate: true, deny: map[string]bool{"secretsmanager:CreateSecret": true}}, c, SecretsManagerID, "us-east-2", keys, "")
	if len(p.Denied) != 1 || p.Denied[0] != "secretsmanager:CreateSecret on 2 of the 2 keys" {
		t.Errorf("denied: %+v", p)
	}
	p = Simulate(ctx, fakeIAM{}, c, SecretsManagerID, "us-east-2", keys, "")
	if p.Simulated || !strings.Contains(p.Unverified, "iam:SimulatePrincipalPolicy") {
		t.Errorf("no simulator: %+v", p)
	}
	root, _ := Identity(ctx, fakeSTS{"arn:aws:iam::111122223333:root"}, fakeIAM{})
	if p := Simulate(ctx, fakeIAM{simulate: true}, root, SecretsManagerID, "us-east-2", keys, ""); !strings.Contains(p.Unverified, "root") {
		t.Errorf("root: %+v", p)
	}
	if got := ResourceARN(ParameterStoreID, "aws", "us-east-2", "1", "/prod/shop/db"); got != "arn:aws:ssm:us-east-2:1:parameter/prod/shop/db" {
		t.Errorf("parameter ARN %s", got)
	}
}
