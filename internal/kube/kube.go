// Package kube holds the small kubernetes helpers several setdrift packages share, an apiVersion's group, a managedFields entry decoded, and telling an error about one object apart from one that stops a run
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

// Group takes an apiVersion and gives back its API group, "apps" for apps/v1 and "" for the core group's v1
func Group(apiVersion string) string {
	g, _, ok := strings.Cut(apiVersion, "/")
	// checking if theres no slash, if so its the core group, which has no name
	if !ok {
		return ""
	}
	return g
}

// FieldSet decodes the fields a managedFields entry owns, so callers can tell which manager set what
// it gives back nil and no error for an entry that records no fields
func FieldSet(e metav1.ManagedFieldsEntry) (*fieldpath.Set, error) {
	// checking if the entry records no fields, if so theres nothing to decode
	if e.FieldsV1 == nil {
		return nil, nil
	}
	s := fieldpath.NewSet()
	// decoding the raw fieldsV1 json into the set
	if err := s.FromJSON(bytes.NewReader(e.FieldsV1.GetRawBytes())); err != nil {
		return nil, err
	}
	return s, nil
}

// IsObjectError tells if err is the API server's answer about one request (Forbidden, NotFound, Invalid, a conflict and so on), which a caller can record and go on from
// cancellation, a deadline, rejected credentials or a broken connection are not, those stop the run
func IsObjectError(err error) bool {
	// checking if the run was cancelled, timed out or the credentials were rejected, if so its not about one object
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || apierrors.IsUnauthorized(err) {
		return false
	}
	// an error carrying an API status came from the server about this request, anything else like a broken connection doesnt
	var status apierrors.APIStatus
	return errors.As(err, &status)
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
