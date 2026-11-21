// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package secretexport

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// Size limits of a secret's value.
const (
	secretsManagerMax = 65536
	// Parameters larger than this need the advanced tier, which costs.
	parameterStandardMax = 4096
	parameterAdvancedMax = 8192
)

// SecretsManagerAPI is the part of the Secrets Manager client used.
type SecretsManagerAPI interface {
	DescribeSecret(context.Context, *secretsmanager.DescribeSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error)
	CreateSecret(context.Context, *secretsmanager.CreateSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error)
	PutSecretValue(context.Context, *secretsmanager.PutSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error)
	GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
	ListSecrets(context.Context, *secretsmanager.ListSecretsInput, ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error)
}

// SecretsManager writes to AWS Secrets Manager.
type SecretsManager struct {
	Client SecretsManagerAPI
	// KMSKeyID encrypts new secrets; empty uses the account's default key.
	KMSKeyID string
	// Tags are added to new secrets, e.g. which cluster they came from.
	Tags map[string]string
}

// Name implements Provider.
func (SecretsManager) Name() string { return "AWS Secrets Manager" }

// Exists implements Provider.
func (s SecretsManager) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.Client.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(key)})
	var nf *smtypes.ResourceNotFoundException
	if errors.As(err, &nf) {
		return false, nil
	}
	return err == nil, err
}

// Put implements Provider.
func (s SecretsManager) Put(ctx context.Context, key, value string, overwrite bool) error {
	if len(value) > secretsManagerMax {
		return fmt.Errorf("%d bytes, more than Secrets Manager's %d", len(value), secretsManagerMax)
	}
	if overwrite {
		_, err := s.Client.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{SecretId: aws.String(key), SecretString: aws.String(value)})
		return err
	}
	in := &secretsmanager.CreateSecretInput{Name: aws.String(key), SecretString: aws.String(value),
		Description: aws.String("Copied by setdrift secrets export")}
	if s.KMSKeyID != "" {
		in.KmsKeyId = aws.String(s.KMSKeyID)
	}
	for _, k := range sortedKeys(s.Tags) {
		in.Tags = append(in.Tags, smtypes.Tag{Key: aws.String(k), Value: aws.String(s.Tags[k])})
	}
	_, err := s.Client.CreateSecret(ctx, in)
	return err
}

// Get implements Provider.
func (s SecretsManager) Get(ctx context.Context, key string) (string, bool, error) {
	out, err := s.Client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(key)})
	var nf *smtypes.ResourceNotFoundException
	if errors.As(err, &nf) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return aws.ToString(out.SecretString), true, nil
}

// Answers checks the service answers in the client's region: a list of at
// most one secret. Refused access still means the region is right.
func (s SecretsManager) Answers(ctx context.Context) error {
	_, err := s.Client.ListSecrets(ctx, &secretsmanager.ListSecretsInput{MaxResults: aws.Int32(1)})
	if err != nil && !IsAccessDenied(err) {
		return err
	}
	return nil
}

// ParameterStoreAPI is the part of the SSM client used.
type ParameterStoreAPI interface {
	GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
	PutParameter(context.Context, *ssm.PutParameterInput, ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
	DescribeParameters(context.Context, *ssm.DescribeParametersInput, ...func(*ssm.Options)) (*ssm.DescribeParametersOutput, error)
}

// ParameterStore writes SecureString parameters to AWS Systems Manager
// Parameter Store.
type ParameterStore struct {
	Client   ParameterStoreAPI
	KMSKeyID string
	Tags     map[string]string
}

// Name implements Provider.
func (ParameterStore) Name() string { return "AWS Systems Manager Parameter Store" }

// Exists implements Provider.
func (s ParameterStore) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.Client.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(key)})
	var nf *ssmtypes.ParameterNotFound
	if errors.As(err, &nf) {
		return false, nil
	}
	return err == nil, err
}

// Get implements Provider.
func (s ParameterStore) Get(ctx context.Context, key string) (string, bool, error) {
	out, err := s.Client.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(key), WithDecryption: aws.Bool(true)})
	var nf *ssmtypes.ParameterNotFound
	if errors.As(err, &nf) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return aws.ToString(out.Parameter.Value), true, nil
}

// Answers checks the service answers in the client's region.
func (s ParameterStore) Answers(ctx context.Context) error {
	_, err := s.Client.DescribeParameters(ctx, &ssm.DescribeParametersInput{MaxResults: aws.Int32(1)})
	if err != nil && !IsAccessDenied(err) {
		return err
	}
	return nil
}

// Put implements Provider. Values over 4 KB use the advanced tier, which
// AWS charges for; over 8 KB they don't fit.
func (s ParameterStore) Put(ctx context.Context, key, value string, overwrite bool) error {
	if len(value) > parameterAdvancedMax {
		return fmt.Errorf("%d bytes, more than Parameter Store's %d; use Secrets Manager", len(value), parameterAdvancedMax)
	}
	in := &ssm.PutParameterInput{Name: aws.String(key), Value: aws.String(value), Type: ssmtypes.ParameterTypeSecureString,
		Overwrite: aws.Bool(overwrite), Description: aws.String("Copied by setdrift secrets export")}
	if len(value) > parameterStandardMax {
		in.Tier = ssmtypes.ParameterTierAdvanced
	}
	if s.KMSKeyID != "" {
		in.KeyId = aws.String(s.KMSKeyID)
	}
	// Tags can't be given with Overwrite.
	if !overwrite {
		for _, k := range sortedKeys(s.Tags) {
			in.Tags = append(in.Tags, ssmtypes.Tag{Key: aws.String(k), Value: aws.String(s.Tags[k])})
		}
	}
	_, err := s.Client.PutParameter(ctx, in)
	return err
}
