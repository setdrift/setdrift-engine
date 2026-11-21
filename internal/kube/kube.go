// Package kube holds the small Kubernetes helpers several SetDrift packages
// share: an apiVersion's group, a managedFields entry decoded, and telling
// an error about one object from one that stops a run.
package kube

import (
	"bytes"
	"context"
	"errors"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
)

// Group returns the API group of an apiVersion: "apps" for apps/v1, "" for
// the core group's v1.
func Group(apiVersion string) string {
	g, _, ok := strings.Cut(apiVersion, "/")
	if !ok {
		return ""
	}
	return g
}

// FieldSet decodes the fields a managedFields entry owns. It returns nil and
// no error for an entry that records no fields.
func FieldSet(e metav1.ManagedFieldsEntry) (*fieldpath.Set, error) {
	if e.FieldsV1 == nil {
		return nil, nil
	}
	s := fieldpath.NewSet()
	if err := s.FromJSON(bytes.NewReader(e.FieldsV1.GetRawBytes())); err != nil {
		return nil, err
	}
	return s, nil
}

// IsObjectError reports whether err is the API server's answer about one
// request (Forbidden, NotFound, Invalid, a conflict...), which a caller can
// record and go on from, as opposed to cancellation, a deadline, rejected
// credentials or a broken connection, which stop the run.
func IsObjectError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || apierrors.IsUnauthorized(err) {
		return false
	}
	var status apierrors.APIStatus
	return errors.As(err, &status)
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
