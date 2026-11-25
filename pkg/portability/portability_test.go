package portability

import "testing"

// check that each kind finds its value in the cloud's format, says what its bound to, and leaves look-alikes alone
func TestFind(t *testing.T) {
	for _, tc := range []struct {
		in          string
		kind, bound string
	}{
		{"111122223333.dkr.ecr.us-east-2.amazonaws.com/acme/shop:1.4", "ECR image", "us-east-2, account 111122223333"},
		{"arn:aws:acm:us-east-2:111122223333:certificate/0f2c9a4e-7b1d-4c3a-9e8f-1a2b3c4d5e6f", "AWS ARN (acm)", "us-east-2, account 111122223333"},
		{"arn:aws:kms:us-east-2:111122223333:key/6a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", "AWS ARN (kms)", "us-east-2, account 111122223333"},
		{"arn:aws:sqs:us-east-2:111122223333:shop-orders", "AWS ARN (sqs)", "us-east-2, account 111122223333"},
		{"arn:aws:iam::111122223333:role/shop", "AWS ARN (iam)", "account 111122223333"},
		{"subnet-0a1b2c3d4e5f6a7b8", "AWS subnet ID", "one VPC's subnet"},
		{"sg-0c3d4e5f6a7b8c9d0", "AWS security group ID", "one VPC's security group"},
		{"https://oidc.eks.us-east-2.amazonaws.com/id/0123456789ABCDEF0123456789ABCDEF", "EKS OIDC issuer", "one EKS cluster, us-east-2"},
		{"/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/acme-shop/providers/Microsoft.ManagedIdentity/userAssignedIdentities/shop", "Azure resource ID", "subscription 00000000-0000-0000-0000-000000000000, resource group acme-shop"},
		{"https://acme-shop-kv.vault.azure.net/", "Azure Key Vault URL", "Key Vault acme-shop-kv"},
		{"us-central1-docker.pkg.dev/example-project/acme/shop:1.4", "Artifact Registry image", "us-central1, project example-project"},
	} {
		ms := Find(tc.in)
		if len(ms) != 1 || ms[0].Kind != tc.kind || ms[0].Bound != tc.bound || ms[0].Doc == "" {
			t.Errorf("%s: got %+v, want one %s bound to %q", tc.in, ms, tc.kind, tc.bound)
		}
	}
	// values that look close but arent bound to a region or account
	for _, in := range []string{"public.ecr.aws/docker/library/busybox:1.36", "docker.io/acme/shop:1.4", "subnet-mask", "sg-shop", "acme.vault.example.com", "ghcr.io/acme/shop"} {
		if ms := Find(in); len(ms) != 0 {
			t.Errorf("%s: got %+v, want nothing", in, ms)
		}
	}
}

// check that two values of the same kinds count as each cluster's own, and different kinds or none dont
func TestSameKinds(t *testing.T) {
	if !SameKinds("subnet-0a1b2c3d4e5f6a7b8", "subnet-0b2c3d4e5f6a7b8c9") {
		t.Error("two subnet IDs: want the same kinds")
	}
	if SameKinds("subnet-0a1b2c3d4e5f6a7b8", "sg-0c3d4e5f6a7b8c9d0") {
		t.Error("a subnet and a security group: want different kinds")
	}
	if SameKinds("shop", "web") {
		t.Error("no bound values: want false")
	}
}

// check that Scan finds values in nested fields with their paths
func TestScan(t *testing.T) {
	obj := map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{
		map[string]any{"image": "111122223333.dkr.ecr.us-east-2.amazonaws.com/acme/shop:1.4"}}}}}}
	fs := Scan("Deployment sd-apps/shop-worker", obj)
	if len(fs) != 1 || fs[0].Path != "spec.template.spec.containers[0].image" || fs[0].Object != "Deployment sd-apps/shop-worker" {
		t.Errorf("got %+v", fs)
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
