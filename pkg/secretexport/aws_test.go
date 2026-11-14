// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package secretexport

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

type fakeSM struct {
	created, put []string
	tags         []smtypes.Tag
}

func (f *fakeSM) DescribeSecret(_ context.Context, in *secretsmanager.DescribeSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	if aws.ToString(in.SecretId) == "exists" {
		return &secretsmanager.DescribeSecretOutput{}, nil
	}
	return nil, &smtypes.ResourceNotFoundException{Message: aws.String("not found")}
}
func (f *fakeSM) CreateSecret(_ context.Context, in *secretsmanager.CreateSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error) {
	f.created = append(f.created, aws.ToString(in.Name))
	f.tags = in.Tags
	return &secretsmanager.CreateSecretOutput{}, nil
}
func (f *fakeSM) PutSecretValue(_ context.Context, in *secretsmanager.PutSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error) {
	f.put = append(f.put, aws.ToString(in.SecretId))
	return &secretsmanager.PutSecretValueOutput{}, nil
}

func TestSecretsManager(t *testing.T) {
	f := &fakeSM{}
	p := SecretsManager{Client: f, Tags: map[string]string{"setdrift.io/source-cluster": "prod"}}
	if ok, err := p.Exists(context.Background(), "exists"); !ok || err != nil {
		t.Errorf("exists: %v %v", ok, err)
	}
	if ok, err := p.Exists(context.Background(), "new"); ok || err != nil {
		t.Errorf("new: %v %v", ok, err)
	}
	_ = p.Put(context.Background(), "new", "{}", false)
	_ = p.Put(context.Background(), "exists", "{}", true)
	if len(f.created) != 1 || f.created[0] != "new" || len(f.put) != 1 || f.put[0] != "exists" || len(f.tags) != 1 {
		t.Errorf("created %v put %v tags %v", f.created, f.put, f.tags)
	}
	if err := p.Put(context.Background(), "big", strings.Repeat("x", secretsManagerMax+1), false); err == nil {
		t.Error("oversized value accepted")
	}
}

type fakeSSM struct{ in *ssm.PutParameterInput }

func (f *fakeSSM) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	return nil, &ssmtypes.ParameterNotFound{Message: aws.String("nope")}
}
func (f *fakeSSM) PutParameter(_ context.Context, in *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	f.in = in
	return &ssm.PutParameterOutput{}, nil
}

func TestParameterStore(t *testing.T) {
	f := &fakeSSM{}
	p := ParameterStore{Client: f}
	if ok, err := p.Exists(context.Background(), "/x"); ok || err != nil {
		t.Errorf("%v %v", ok, err)
	}
	_ = p.Put(context.Background(), "/x", strings.Repeat("x", 5000), false)
	if f.in.Type != ssmtypes.ParameterTypeSecureString || f.in.Tier != ssmtypes.ParameterTierAdvanced {
		t.Errorf("put %+v", f.in)
	}
	if err := p.Put(context.Background(), "/x", strings.Repeat("x", parameterAdvancedMax+1), false); err == nil {
		t.Error("oversized value accepted")
	}
}
