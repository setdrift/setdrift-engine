// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

package kustomize

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// SecretSource is one Secret setdrift secrets export copied, for its
// ExternalSecret: where its values are now, and how to rebuild it.
type SecretSource struct {
	Namespace, Name, Type string
	Labels, Annotations   map[string]string
	// Keys are the Secret keys, each a property of RemoteKey.
	Keys      []string
	RemoteKey string
	// Merge: a controller writes the Secret's other keys; the ExternalSecret
	// adds these without owning it.
	Merge bool
	// Binary are the keys stored base64-encoded, which the ExternalSecret decodes.
	Binary []string
}

// WriteExternalSecrets writes dir for the Secrets secrets export copied: an
// ExternalSecret for each, reading from x's store; the store itself, for AWS
// in region (one per namespace for a SecretStore); a kustomization.yaml
// listing them all; and a README. dir must be empty or not exist.
func WriteExternalSecrets(dir string, x *ExternalSecrets, region string, secrets []SecretSource) (err error) {
	out, err := openOutDir(dir)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.close(); err == nil {
			err = cerr
		}
	}()
	var files, namespaces []string
	for _, s := range secrets {
		if !slices.Contains(namespaces, s.Namespace) {
			namespaces = append(namespaces, s.Namespace)
		}
	}
	sort.Strings(namespaces)
	if x.StoreKind == "SecretStore" {
		for _, ns := range namespaces {
			f := "secret-store-" + SafeName(ns) + ".yaml"
			if err := out.write(f, []byte(x.awsStore(region, ns))); err != nil {
				return err
			}
			files = append(files, f)
		}
	} else {
		if err := out.write("secret-store.yaml", []byte(x.awsStore(region, ""))); err != nil {
			return err
		}
		files = append(files, "secret-store.yaml")
	}

	var rows strings.Builder
	for _, s := range secrets {
		manifest := x.manifest(s)
		b, err := yaml.Marshal(manifest)
		if err != nil {
			return err
		}
		f := filepath.ToSlash(filepath.Join("namespaces", SafeName(s.Namespace), SafeName(s.Name)+".yaml"))
		if err := out.write(filepath.FromSlash(f), b); err != nil {
			return err
		}
		files = append(files, f)
		merge := ""
		if s.Merge {
			merge = " (merge)"
		}
		fmt.Fprintf(&rows, "| %s/%s%s | `%s` | `%s` |\n", s.Namespace, s.Name, merge, s.RemoteKey, strings.Join(s.Keys, "`, `"))
	}
	if err := out.writeKustomization(".", files); err != nil {
		return err
	}
	storeLine := fmt.Sprintf("the %s `%s`, for %s in %s", x.StoreKind, x.StoreName, x.Provider, region)
	if x.StoreKind == "SecretStore" {
		storeLine += ", one in each namespace: " + strings.Join(namespaces, ", ")
	}
	readme := "# External secrets\n\n" +
		"Written by `setdrift secrets export`, which copied each Secret's values\n" +
		"into " + x.Provider + ": one ExternalSecret per Secret, reading from\n" +
		storeLine + ".\n\n" +
		"In the new cluster: install External Secrets Operator, give it access to\n" +
		x.Provider + " (EKS Pod Identity or IRSA), then apply this directory:\n" +
		"`kubectl apply -k .` creates the store and the ExternalSecrets, and the\n" +
		"operator creates each Secret. Those marked *merge* add their keys to a\n" +
		"Secret a controller creates (`creationPolicy: Merge`).\n\n" +
		"| Secret | Remote secret | Keys |\n|---|---|---|\n" + rows.String()
	return out.write("README.md", []byte(readme))
}
