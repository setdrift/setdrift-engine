// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package secretexport

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// IsAccessDenied reports whether an AWS error is a refusal of the caller.
func IsAccessDenied(err error) bool {
	var api smithy.APIError
	if !errors.As(err, &api) {
		return false
	}
	switch api.ErrorCode() {
	case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation", "UnrecognizedClientException":
		return true
	}
	return false
}

// STSAPI, IAMAPI and KMSAPI are the parts of those clients used.
type (
	STSAPI interface {
		GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
	}
	IAMAPI interface {
		SimulatePrincipalPolicy(context.Context, *iam.SimulatePrincipalPolicyInput, ...func(*iam.Options)) (*iam.SimulatePrincipalPolicyOutput, error)
		GetRole(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	}
	KMSAPI interface {
		DescribeKey(context.Context, *kms.DescribeKeyInput, ...func(*kms.Options)) (*kms.DescribeKeyOutput, error)
	}
)

// Caller is who the AWS credentials are.
type Caller struct {
	Account string
	// ARN as STS returns it, e.g. arn:aws:sts::1:assumed-role/admin/session.
	ARN string
	// Principal is the IAM ARN its permissions are simulated for: the role
	// of an assumed role, the user, or "" for the account's root user.
	Principal string
}

// Identity asks STS who the credentials are, and finds the IAM principal
// behind them (an assumed role's role, with its path when IAM says).
func Identity(ctx context.Context, s STSAPI, i IAMAPI) (Caller, error) {
	out, err := s.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return Caller{}, err
	}
	c := Caller{Account: aws.ToString(out.Account), ARN: aws.ToString(out.Arn)}
	parts := strings.SplitN(c.ARN, ":", 6)
	if len(parts) < 6 {
		return c, nil
	}
	resource := parts[5]
	switch {
	case resource == "root":
	case strings.HasPrefix(resource, "assumed-role/"):
		role := strings.Split(strings.TrimPrefix(resource, "assumed-role/"), "/")[0]
		c.Principal = fmt.Sprintf("arn:%s:iam::%s:role/%s", parts[1], c.Account, role)
		// a role with a path (role/team/name) needs it in the ARN to simulate
		if r, err := i.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(role)}); err == nil && r.Role != nil {
			c.Principal = aws.ToString(r.Role.Arn)
		}
	default:
		c.Principal = c.ARN
	}
	return c, nil
}

// KMSKey checks a KMS key exists and is enabled, and returns its ARN.
func KMSKey(ctx context.Context, k KMSAPI, id string) (string, error) {
	out, err := k.DescribeKey(ctx, &kms.DescribeKeyInput{KeyId: aws.String(id)})
	if err != nil {
		return "", err
	}
	if !out.KeyMetadata.Enabled {
		return "", fmt.Errorf("KMS key %s is %s, not enabled", id, out.KeyMetadata.KeyState)
	}
	return aws.ToString(out.KeyMetadata.Arn), nil
}

// Permissions is the outcome of checking the AWS permissions a run needs.
type Permissions struct {
	// Simulated: IAM's policy simulator answered for every action and key.
	Simulated bool
	// Denied are the actions (with the keys) IAM's policies do not allow.
	Denied []string
	// Unverified says why some could not be checked in advance.
	Unverified string
}

// Actions are the AWS actions a run needs, for a manager: to read, to
// write, to tag, and with a KMS key, to use it.
func Actions(m Manager, kmsKey bool) []string {
	var a []string
	switch m {
	case SecretsManagerID:
		a = []string{"secretsmanager:DescribeSecret", "secretsmanager:GetSecretValue", "secretsmanager:CreateSecret",
			"secretsmanager:PutSecretValue", "secretsmanager:TagResource"}
	case ParameterStoreID:
		a = []string{"ssm:GetParameter", "ssm:PutParameter", "ssm:AddTagsToResource"}
	}
	if kmsKey {
		a = append(a, "kms:Encrypt", "kms:Decrypt", "kms:GenerateDataKey")
	}
	return a
}

// ResourceARN is the ARN IAM evaluates for a remote secret's key. A Secrets
// Manager secret's ARN ends in six random characters, unknown before it
// exists; any six stand in for them.
func ResourceARN(m Manager, partition, region, account, key string) string {
	if m == ParameterStoreID {
		return fmt.Sprintf("arn:%s:ssm:%s:%s:parameter%s", partition, region, account, key)
	}
	return fmt.Sprintf("arn:%s:secretsmanager:%s:%s:secret:%s-AbCdEf", partition, region, account, key)
}

// Simulate asks IAM's policy simulator whether the principal's policies
// allow the actions on every key (and on the KMS key, when given). Without
// permission to simulate it reports that writes could not be verified,
// rather than failing: the first write then stops the run.
func Simulate(ctx context.Context, i IAMAPI, c Caller, m Manager, region string, keys []string, kmsARN string) Permissions {
	if c.Principal == "" {
		return Permissions{Unverified: "the account's root user can do everything, and IAM can't simulate it"}
	}
	partition := strings.SplitN(c.ARN, ":", 3)[1]
	arns := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		arns = append(arns, ResourceARN(m, partition, region, c.Account, k))
	}
	actions := Actions(m, kmsARN != "")
	var kmsActions, managerActions []string
	for _, a := range actions {
		if strings.HasPrefix(a, "kms:") {
			kmsActions = append(kmsActions, a)
		} else {
			managerActions = append(managerActions, a)
		}
	}
	denied := map[string][]string{}
	run := func(actions, resources []string) error {
		in := &iam.SimulatePrincipalPolicyInput{PolicySourceArn: aws.String(c.Principal), ActionNames: actions, ResourceArns: resources}
		p := iam.NewSimulatePrincipalPolicyPaginator(i, in)
		for p.HasMorePages() {
			out, err := p.NextPage(ctx)
			if err != nil {
				return err
			}
			for _, r := range out.EvaluationResults {
				if r.EvalDecision != iamtypes.PolicyEvaluationDecisionTypeAllowed {
					denied[aws.ToString(r.EvalActionName)] = append(denied[aws.ToString(r.EvalActionName)], aws.ToString(r.EvalResourceName))
				}
			}
		}
		return nil
	}
	err := run(managerActions, arns)
	if err == nil && kmsARN != "" {
		err = run(kmsActions, []string{kmsARN})
	}
	if err != nil {
		why := err.Error()
		if IsAccessDenied(err) {
			why = "these credentials may not use IAM's policy simulator (iam:SimulatePrincipalPolicy)"
		}
		return Permissions{Unverified: why}
	}
	res := Permissions{Simulated: true}
	for a, rs := range denied {
		res.Denied = append(res.Denied, fmt.Sprintf("%s on %d of the %d keys", a, len(rs), len(arns)))
	}
	sort.Strings(res.Denied)
	return res
}

// Probe checks, without simulation, that each key can at least be read:
// asking whether it exists gives Access Denied, or an answer.
func Probe(ctx context.Context, p Provider, keys []string) []string {
	var denied []string
	for _, k := range keys {
		if _, err := p.Exists(ctx, k); err != nil && IsAccessDenied(err) {
			denied = append(denied, k)
		}
	}
	return denied
}
