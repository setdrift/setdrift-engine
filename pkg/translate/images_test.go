package translate

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/platform"
)

// check that images split into host, repository and tag or digest, and only a cloud's private registry has a cloud
func TestParseImage(t *testing.T) {
	for _, tc := range []struct{ ref, host, repo, tag, cloud string }{
		{"111122223333.dkr.ecr.us-east-2.amazonaws.com/acme/shop:1.4", "111122223333.dkr.ecr.us-east-2.amazonaws.com", "acme/shop", ":1.4", "aws"},
		{"acmeshop.azurecr.io/acme/shop@sha256:abc", "acmeshop.azurecr.io", "acme/shop", "@sha256:abc", "azure"},
		{"us-central1-docker.pkg.dev/example-project/acme/shop", "us-central1-docker.pkg.dev", "example-project/acme/shop", "", "gcp"},
		{"public.ecr.aws/nginx/nginx:1.27", "public.ecr.aws", "nginx/nginx", ":1.27", ""},
		{"localhost:5000/shop:1", "localhost:5000", "shop", ":1", ""},
		{"nginx:1.27", "", "nginx:1.27", "", ""},
	} {
		host, repo, tag := parseImage(tc.ref)
		cloud, _ := cloudOf(host)
		if host != tc.host || repo != tc.repo || tag != tc.tag || cloud != tc.cloud {
			t.Errorf("%s: got %q %q %q %q", tc.ref, host, repo, tag, cloud)
		}
	}
}

// check that only images in another cloud's private registry are copied, once each, and RESTORE.md's step names the target's commands
func TestImagesToCopy(t *testing.T) {
	deploy := func(name string, images ...string) *unstructured.Unstructured {
		var cs []any
		for _, img := range images {
			cs = append(cs, map[string]any{"name": "c", "image": img})
		}
		return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": name, "namespace": "shop"},
			"spec":     map[string]any{"template": map[string]any{"spec": map[string]any{"containers": cs}}}}}
	}
	ecr := "111122223333.dkr.ecr.us-east-2.amazonaws.com/acme/shop:1.4"
	objs := []*unstructured.Unstructured{
		deploy("web", ecr, "public.ecr.aws/nginx/nginx:1.27"),
		deploy("worker", ecr, "acmeshop.azurecr.io/acme/worker:2", "gcr.io/google-samples/gb-frontend:v5"),
	}
	imgs := imagesToCopy(objs, platform.AKS)
	if len(imgs) != 1 || imgs[0].Ref != ecr || imgs[0].Region != "us-east-2" {
		t.Fatalf("to AKS: got %+v, want only the ECR image", imgs)
	}
	var b strings.Builder
	imagesStep(&b, imgs, platform.AKS, "translations/te2a")
	for _, want := range []string{
		`az acr import --name "$ACR" --source ` + ecr + ` --image acme/shop:1.4 --username AWS --password "$(aws ecr get-login-password --region us-east-2)"`,
		"- name: 111122223333.dkr.ecr.us-east-2.amazonaws.com/acme/shop\n  newName: <registry>/acme/shop",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("AKS step lacks %q:\n%s", want, b.String())
		}
	}
	if got := imagesToCopy(objs, platform.GKE); len(got) != 2 {
		t.Errorf("to GKE: got %+v, want the ECR and ACR images", got)
	}
	b.Reset()
	imagesStep(&b, imagesToCopy(objs, platform.GKE), platform.GKE, "translations/te2g")
	if !strings.Contains(b.String(), `crane copy `+ecr+` "$REGISTRY/acme/shop:1.4"`) || !strings.Contains(b.String(), "crane auth login acmeshop.azurecr.io") {
		t.Errorf("GKE step:\n%s", b.String())
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
