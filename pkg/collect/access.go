// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// What the credentials may read, asked before reading: who the cluster
// says they are, whether a namespace exists, and whether they can read in
// it. Every command that reads a cluster checks the same way, so a missing
// or unreadable namespace gets the same answer from export, diff and verify.

package collect

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var (
	selfReviewGVR   = schema.GroupVersionResource{Group: "authentication.k8s.io", Version: "v1", Resource: "selfsubjectreviews"}
	accessReviewGVR = schema.GroupVersionResource{Group: "authorization.k8s.io", Version: "v1", Resource: "selfsubjectaccessreviews"}
	namespacesGVR   = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
)

// forbiddenUser finds the user the API server names in a Forbidden error.
var forbiddenUser = regexp.MustCompile(`User "([^"]+)"`)

// Identity returns who the cluster says the credentials are: the user name
// from a SelfSubjectReview (Kubernetes 1.28 and later), else the one a
// Forbidden error names, else "".
func Identity(ctx context.Context, dyn dynamic.Interface, errs ...error) string {
	review := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview"}}
	if out, err := dyn.Resource(selfReviewGVR).Create(ctx, review, metav1.CreateOptions{}); err == nil {
		if name, _, _ := unstructured.NestedString(out.Object, "status", "userInfo", "username"); name != "" {
			return name
		}
	}
	for _, err := range errs {
		if err == nil {
			continue
		}
		if m := forbiddenUser.FindStringSubmatch(err.Error()); m != nil {
			return m[1]
		}
	}
	return ""
}

// can asks the API server whether the credentials may verb resource in
// namespace ("" for every namespace), with a SelfSubjectAccessReview, which
// every authorizer answers (RBAC, and webhooks such as EKS access
// policies). known is false when the review itself failed.
func can(ctx context.Context, dyn dynamic.Interface, verb, group, resource, namespace string) (allowed, known bool) {
	review := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SelfSubjectAccessReview",
		"spec": map[string]any{"resourceAttributes": map[string]any{
			"verb": verb, "group": group, "resource": resource, "namespace": namespace,
		}},
	}}
	out, err := dyn.Resource(accessReviewGVR).Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, false
	}
	// an API server always answers with a status; without one, nothing was decided
	allowed, found, _ := unstructured.NestedBool(out.Object, "status", "allowed")
	if !found {
		return false, false
	}
	return allowed, true
}

// Can asks the API server whether the credentials may verb resource (in
// group) in namespace; known is false when it could not say.
func Can(ctx context.Context, dyn dynamic.Interface, verb, group, resource, namespace string) (allowed, known bool) {
	return can(ctx, dyn, verb, group, resource, namespace)
}

// readProbes are the types a reader of a namespace can almost always list:
// the built-in view, edit and admin roles all include them.
var readProbes = []struct{ group, resource string }{{"", "configmaps"}, {"", "pods"}, {"apps", "deployments"}}

// CanReadNamespace reports whether the credentials can list resources in
// namespace: unknown counts as yes, so an authorizer that can't answer
// never blocks a read the lists themselves would allow.
func CanReadNamespace(ctx context.Context, dyn dynamic.Interface, namespace string) bool {
	answered := false
	for _, p := range readProbes {
		allowed, known := can(ctx, dyn, "list", p.group, p.resource, namespace)
		if allowed {
			return true
		}
		answered = answered || known
	}
	return !answered
}

// CanReadCluster reports whether the credentials can list namespaced
// resources across every namespace at once, as a whole-cluster read does.
func CanReadCluster(ctx context.Context, dyn dynamic.Interface) bool {
	return CanReadNamespace(ctx, dyn, metav1.NamespaceAll)
}

// NamespaceError is a namespace that was asked for (with -n, or as the
// kubeconfig context's namespace) but does not exist or can't be read. It
// is found before anything is read, so nothing is written.
type NamespaceError struct {
	// Missing do not exist.
	Missing []string
	// Unreadable exist, or can't be told to (the credentials may not get
	// Namespaces), and the credentials can list nothing in them.
	Unreadable []string
	// Context is set when the namespace is the kubeconfig context's.
	Context bool
	// Identity is who the cluster says the credentials are, when known.
	Identity string
	// Existing lists the cluster's namespaces, when the credentials may list them.
	Existing []string
}

func (e *NamespaceError) Error() string {
	var parts []string
	who := "these credentials"
	if e.Identity != "" {
		who += " (" + e.Identity + ")"
	}
	if len(e.Missing) > 0 {
		s := fmt.Sprintf("%s %s not exist in this cluster", namespaceList(e.Missing), choose(len(e.Missing) == 1, "does", "do"))
		if len(e.Existing) > 0 {
			s += "; its namespaces are " + truncateList(e.Existing, 15)
		}
		parts = append(parts, s)
	}
	if len(e.Unreadable) > 0 {
		it := choose(len(e.Unreadable) == 1, "it", "them")
		s := fmt.Sprintf("%s can't read anything in %s", who, namespaceList(e.Unreadable))
		if e.Context {
			s += " (the kubeconfig context's namespace)"
		}
		s += fmt.Sprintf(": %s, or they have no access to %s. To read %s, they need get and list there; "+
			"the built-in view role is enough: kubectl create rolebinding setdrift-read -n %s --clusterrole=view %s",
			choose(len(e.Unreadable) == 1, "it does not exist", "they do not exist"), it, it, e.Unreadable[0], Subject(e.Identity))
		parts = append(parts, s)
	}
	for i, p := range parts {
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, ". ")
}

// Subject is the kubectl create rolebinding flag that names identity:
// --serviceaccount=ns:name for a service account, else --user.
func Subject(identity string) string {
	if rest, ok := strings.CutPrefix(identity, "system:serviceaccount:"); ok {
		return "--serviceaccount=" + rest
	}
	return "--user=" + cmpOr(identity, "<user>")
}

// ReadNothingError: the credentials can list namespaces but read none of them.
type ReadNothingError struct {
	Identity   string
	Namespaces int
}

func (e *ReadNothingError) Error() string {
	who := "These credentials"
	if e.Identity != "" {
		who += " (" + e.Identity + ")"
	}
	return fmt.Sprintf("%s can list the cluster's %d namespaces but read nothing in any of them. To read the cluster, they need get and list: "+
		"the built-in view ClusterRole bound cluster-wide is enough (kubectl create clusterrolebinding setdrift-read --clusterrole=view %s), "+
		"or bound in the namespaces to read with -n", who, e.Namespaces, Subject(e.Identity))
}

// NoAccessError: every list was refused (Forbidden), so nothing could be read.
type NoAccessError struct {
	Identity string
	// Namespaces read; empty for the whole cluster.
	Namespaces []string
	Err        error
}

func (e *NoAccessError) Error() string {
	who := "These credentials"
	if e.Identity != "" {
		who += " (" + e.Identity + ")"
	}
	where, bind := "the cluster", "kubectl create clusterrolebinding setdrift-read --clusterrole=view "+Subject(e.Identity)
	if len(e.Namespaces) > 0 {
		where = namespaceList(e.Namespaces)
		bind = "kubectl create rolebinding setdrift-read -n " + e.Namespaces[0] + " --clusterrole=view " + Subject(e.Identity)
	}
	return fmt.Sprintf("%s may not list anything in %s (the API server refused every list: %v). They need get and list; "+
		"the built-in view role is enough: %s", who, where, e.Err, bind)
}

func (e *NoAccessError) Unwrap() error { return e.Err }

// CheckNamespaces checks namespaces before they are read: each must be
// readable and, with requireExisting, exist. context marks the kubeconfig
// context's namespace, for the message.
func CheckNamespaces(ctx context.Context, dyn dynamic.Interface, namespaces []string, context, requireExisting bool) error {
	e := &NamespaceError{Context: context}
	for _, ns := range dedupe(namespaces) {
		_, err := dyn.Resource(namespacesGVR).Get(ctx, ns, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			if requireExisting {
				e.Missing = append(e.Missing, ns)
			}
		case err == nil || apierrors.IsForbidden(err):
			// exists, or can't be told: readable is what matters
			if !CanReadNamespace(ctx, dyn, ns) {
				e.Unreadable = append(e.Unreadable, ns)
			}
		}
	}
	if len(e.Missing)+len(e.Unreadable) == 0 {
		return nil
	}
	e.Identity = Identity(ctx, dyn)
	if len(e.Missing) > 0 {
		if list, err := dyn.Resource(namespacesGVR).List(ctx, metav1.ListOptions{}); err == nil {
			for _, n := range list.Items {
				e.Existing = append(e.Existing, n.GetName())
			}
			sort.Strings(e.Existing)
		}
	}
	return e
}

// readableNamespaces splits the cluster's namespaces into those the
// credentials can read and those they can't, asking for each at once (up to 8).
func readableNamespaces(ctx context.Context, dyn dynamic.Interface) (readable, unreadable []string, err error) {
	list, err := dyn.Resource(namespacesGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(list.Items))
	for _, n := range list.Items {
		names = append(names, n.GetName())
	}
	sort.Strings(names)
	ok := make([]bool, len(names))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, ns := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ok[i] = CanReadNamespace(ctx, dyn, ns)
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	for i, ns := range names {
		if ok[i] {
			readable = append(readable, ns)
		} else {
			unreadable = append(unreadable, ns)
		}
	}
	return readable, unreadable, nil
}

// IsPermission reports whether err is the API server refusing the
// credentials something (Forbidden), anywhere in its chain.
func IsPermission(err error) bool {
	var status apierrors.APIStatus
	return errors.As(err, &status) && apierrors.IsForbidden(err)
}

func dedupe(in []string) []string {
	var out []string
	for _, s := range in {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func namespaceList(ns []string) string {
	if len(ns) == 1 {
		return "namespace " + ns[0]
	}
	return "namespaces " + strings.Join(ns, ", ")
}

func truncateList(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" and %d more (kubectl get namespaces)", len(items)-n)
}

func choose(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
