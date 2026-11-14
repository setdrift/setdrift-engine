// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package secretexport

import (
	"context"
	"encoding/base64"
	"fmt"
	"io/fs"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var secretsGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

// ClusterSecrets reads Secrets with a dynamic client.
type ClusterSecrets struct{ Client dynamic.Interface }

// Get implements SecretReader.
func (c ClusterSecrets) Get(ctx context.Context, namespace, name string) (map[string][]byte, error) {
	u, err := c.Client.Resource(secretsGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%s/%s: %w", namespace, name, fs.ErrNotExist)
	}
	if err != nil {
		return nil, err
	}
	data, _ := u.Object["data"].(map[string]any)
	out := make(map[string][]byte, len(data))
	for k, v := range data {
		s, _ := v.(string)
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("%s/%s: key %s is not base64", namespace, name, k)
		}
		out[k] = b
	}
	return out, nil
}
