// asks the cluster what the credentials may read before reading anything, who it says they are, whether a namespace exists and whether they can read in it
// every command that reads a cluster checks the same way, so a missing or unreadable namespace gets the same answer from export, diff and verify

package collect

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"setdrift.io/setdrift-engine/internal/text"
)

// the resources the dynamic client goes through to ask who we are, what we may do, and which namespaces there are
var (
	selfReviewGVR   = schema.GroupVersionResource{Group: "authentication.k8s.io", Version: "v1", Resource: "selfsubjectreviews"}
	accessReviewGVR = schema.GroupVersionResource{Group: "authorization.k8s.io", Version: "v1", Resource: "selfsubjectaccessreviews"}
	namespacesGVR   = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
)

// forbiddenUser picks out the user the API server names in a Forbidden error
var forbiddenUser = regexp.MustCompile(`User "([^"]+)"`)

// Identity gives back who the cluster says the credentials are, the user name from a SelfSubjectReview (kubernetes 1.28 and later), else the one a Forbidden error in errs names, else empty
func Identity(ctx context.Context, dyn dynamic.Interface, errs ...error) string {
	review := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview"}}
	// checking if the cluster answers the SelfSubjectReview, older clusters dont have it
	if out, err := dyn.Resource(selfReviewGVR).Create(ctx, review, metav1.CreateOptions{}); err == nil {
		// checking if the answer carries a user name, if so thats who we are
		if name, _, _ := unstructured.NestedString(out.Object, "status", "userInfo", "username"); name != "" {
			return name
		}
	}
	// going through the errors given, looking for one that names the user
	for _, err := range errs {
		// checking if theres an error at all, nil ones are skipped
		if err == nil {
			continue
		}
		// checking if the error message names a user, if so thats who we are
		if m := forbiddenUser.FindStringSubmatch(err.Error()); m != nil {
			return m[1]
		}
	}
	return ""
}

// Can asks the API server whether the credentials may do verb on resource (in group) in namespace, empty for every namespace
// it uses a SelfSubjectAccessReview, which every authorizer answers, RBAC and webhooks like EKS access policies
// known is false when the review itself failed
func Can(ctx context.Context, dyn dynamic.Interface, verb, group, resource, namespace string) (allowed, known bool) {
	review := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SelfSubjectAccessReview",
		"spec": map[string]any{"resourceAttributes": map[string]any{
			"verb": verb, "group": group, "resource": resource, "namespace": namespace,
		}},
	}}
	// sending the review to the cluster, a failure means the answer is unknown
	out, err := dyn.Resource(accessReviewGVR).Create(ctx, review, metav1.CreateOptions{})
	// checking if the review couldnt be sent, if so neither allowed nor known
	if err != nil {
		return false, false
	}
	// reading the decision from the status
	allowed, found, _ := unstructured.NestedBool(out.Object, "status", "allowed")
	// checking if the status has no allowed field, an API server always answers with one so without it nothing was decided
	if !found {
		return false, false
	}
	return allowed, true
}

// readProbes are the types a reader of a namespace can almost always list, the built-in view, edit and admin roles all include them
var readProbes = []struct{ group, resource string }{{"", "configmaps"}, {"", "pods"}, {"apps", "deployments"}}

// CanReadNamespace tells if the credentials can list resources in namespace, by asking about each of readProbes
// unknown counts as yes, so an authorizer that cant answer never blocks a read the lists themselves would allow
func CanReadNamespace(ctx context.Context, dyn dynamic.Interface, namespace string) bool {
	answered := false
	// asking about each probe type until one is allowed
	for _, p := range readProbes {
		// invoking Can to ask about listing this type, its defined above
		allowed, known := Can(ctx, dyn, "list", p.group, p.resource, namespace)
		// checking if listing it is allowed, if so the namespace is readable
		if allowed {
			return true
		}
		answered = answered || known
	}
	return !answered
}

// CanReadCluster tells if the credentials can list namespaced resources across every namespace at once, as a cluster-wide read does
func CanReadCluster(ctx context.Context, dyn dynamic.Interface) bool {
	// invoking CanReadNamespace with the all-namespaces value, its defined above
	return CanReadNamespace(ctx, dyn, metav1.NamespaceAll)
}

// NamespaceError is a namespace that was asked for, with -n or as the kubeconfig context's namespace, that doesnt exist or cant be read
// its found before anything is read, so nothing gets written
// this package's access errors (NamespaceError, ReadNothingError, NoAccessError, ScopeError) are written for the person running the cli, their only reader
// so theyre complete sentences with the kubectl command that grants the access, and their fields hold the same facts for a program
type NamespaceError struct {
	// Missing are the namespaces that dont exist
	Missing []string
	// Unreadable are the ones that exist, or cant be told to because the credentials may not get Namespaces, and the credentials can list nothing in them
	Unreadable []string
	// Context is set when the namespace is the kubeconfig context's
	Context bool
	// Identity is who the cluster says the credentials are, when known
	Identity string
	// Existing lists the cluster's namespaces, when the credentials may list them
	Existing []string
}

// Error builds the message, one sentence for the missing namespaces and one for the unreadable ones, with the rolebinding that fixes it
func (e *NamespaceError) Error() string {
	var parts []string
	who := "these credentials"
	// checking if we know who the credentials belong to, if so the message names them
	if e.Identity != "" {
		who += " (" + e.Identity + ")"
	}
	// checking if some namespaces dont exist, if so the message says which
	if len(e.Missing) > 0 {
		// invoking namespaceList to name them, its defined below
		s := fmt.Sprintf("%s %s not exist in this cluster", namespaceList(e.Missing), text.Word(len(e.Missing), "does", "do"))
		// checking if the cluster's namespaces could be listed, if so the message lists them to help spot a typo
		if len(e.Existing) > 0 {
			// invoking truncateList to cap the list at 15, its defined below
			s += "; its namespaces are " + truncateList(e.Existing, 15)
		}
		parts = append(parts, s)
	}
	// checking if some namespaces cant be read, if so the message says which and how to grant access
	if len(e.Unreadable) > 0 {
		it := text.Word(len(e.Unreadable), "it", "them")
		s := fmt.Sprintf("%s can't read anything in %s", who, namespaceList(e.Unreadable))
		// checking if the namespace came from the kubeconfig context, if so the message says so since nobody typed it
		if e.Context {
			s += " (the kubeconfig context's namespace)"
		}
		// invoking Subject for the rolebinding flag that names the credentials, its defined below
		s += fmt.Sprintf(": %s, or they have no access to %s. To read %s, they need get and list there; "+
			"the built-in view role is enough: kubectl create rolebinding setdrift-read -n %s --clusterrole=view %s",
			text.Word(len(e.Unreadable), "it does not exist", "they do not exist"), it, it, e.Unreadable[0], Subject(e.Identity))
		parts = append(parts, s)
	}
	// capitalizing each part so each one reads as a sentence
	for i, p := range parts {
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, ". ")
}

// Subject gives back the kubectl create rolebinding flag that names identity, --serviceaccount with its namespace and name for a service account, else --user
func Subject(identity string) string {
	// checking if the identity is a service account, if so it gets the --serviceaccount flag with its namespace and name
	if rest, ok := strings.CutPrefix(identity, "system:serviceaccount:"); ok {
		return "--serviceaccount=" + rest
	}
	return "--user=" + cmp.Or(identity, "<user>")
}

// ReadNothingError is when the credentials can list the namespaces but read none of them
type ReadNothingError struct {
	// who the cluster says the credentials are, when known
	Identity string
	// how many namespaces the cluster has
	Namespaces int
}

// Error builds the message, with the clusterrolebinding that fixes it
func (e *ReadNothingError) Error() string {
	who := "These credentials"
	// checking if we know who the credentials belong to, if so the message names them
	if e.Identity != "" {
		who += " (" + e.Identity + ")"
	}
	// invoking Subject for the rolebinding flag that names the credentials, its defined above
	return fmt.Sprintf("%s can list the cluster's %d namespaces but read nothing in any of them. To read the cluster, they need get and list: "+
		"the built-in view ClusterRole bound cluster-wide is enough (kubectl create clusterrolebinding setdrift-read --clusterrole=view %s), "+
		"or bound in the namespaces to read with -n", who, e.Namespaces, Subject(e.Identity))
}

// NoAccessError is when every list was refused with Forbidden, so nothing could be read
type NoAccessError struct {
	// who the cluster says the credentials are, when known
	Identity string
	// the namespaces that were read, empty when it was the cluster
	Namespaces []string
	// the refusal from the API server
	Err error
}

// Error builds the message, with the binding that fixes it, cluster-wide or in the first namespace
func (e *NoAccessError) Error() string {
	who := "These credentials"
	// checking if we know who the credentials belong to, if so the message names them
	if e.Identity != "" {
		who += " (" + e.Identity + ")"
	}
	// invoking Subject for the rolebinding flag that names the credentials, its defined above
	where, bind := "the cluster", "kubectl create clusterrolebinding setdrift-read --clusterrole=view "+Subject(e.Identity)
	// checking if only some namespaces were read, if so the message names them and the fix is a rolebinding in the first one
	if len(e.Namespaces) > 0 {
		// invoking namespaceList to name them, its defined below
		where = namespaceList(e.Namespaces)
		bind = "kubectl create rolebinding setdrift-read -n " + e.Namespaces[0] + " --clusterrole=view " + Subject(e.Identity)
	}
	return fmt.Sprintf("%s may not list anything in %s (the API server refused every list: %v). They need get and list; "+
		"the built-in view role is enough: %s", who, where, e.Err, bind)
}

// Unwrap gives back the refusal, so errors.Is and IsPermission still see the Forbidden underneath
func (e *NoAccessError) Unwrap() error { return e.Err }

// CheckNamespaces checks namespaces before theyre read, each must be readable and, with requireExisting, exist
// fromContext marks the kubeconfig context's namespace, for the message
func CheckNamespaces(ctx context.Context, dyn dynamic.Interface, namespaces []string, fromContext, requireExisting bool) error {
	e := &NamespaceError{Context: fromContext}
	// invoking dedupe so a namespace given twice is checked once, its defined below
	for _, ns := range dedupe(namespaces) {
		_, err := dyn.Resource(namespacesGVR).Get(ctx, ns, metav1.GetOptions{})
		// checking what getting the namespace said
		switch {
		case apierrors.IsNotFound(err):
			// checking if missing namespaces count as a problem here, verify allows them
			if requireExisting {
				e.Missing = append(e.Missing, ns)
			}
		case err == nil || apierrors.IsForbidden(err):
			// it exists or cant be told, readable is what matters
			// invoking CanReadNamespace to check, its defined above
			if !CanReadNamespace(ctx, dyn, ns) {
				e.Unreadable = append(e.Unreadable, ns)
			}
		}
	}
	// checking if every namespace passed, if so theres no error
	if len(e.Missing)+len(e.Unreadable) == 0 {
		return nil
	}
	// invoking Identity so the error can name the credentials, its defined above
	e.Identity = Identity(ctx, dyn)
	// checking if some namespaces are missing, if so we list the existing ones for the message
	if len(e.Missing) > 0 {
		// checking if the namespaces can be listed, if not the message goes without them
		if list, err := dyn.Resource(namespacesGVR).List(ctx, metav1.ListOptions{}); err == nil {
			// collecting each namespace's name
			for _, n := range list.Items {
				e.Existing = append(e.Existing, n.GetName())
			}
			sort.Strings(e.Existing)
		}
	}
	return e
}

// readableNamespaces splits the cluster's namespaces into those the credentials can read and those they cant, asking about up to 8 at once
func readableNamespaces(ctx context.Context, dyn dynamic.Interface) (readable, unreadable []string, err error) {
	list, err := dyn.Resource(namespacesGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(list.Items))
	// collecting each namespace's name
	for _, n := range list.Items {
		names = append(names, n.GetName())
	}
	sort.Strings(names)
	ok := make([]bool, len(names))
	var g errgroup.Group
	g.SetLimit(8)
	// asking about each namespace in its own goroutine, each writes only its own slot of ok
	for i, ns := range names {
		g.Go(func() error {
			// invoking CanReadNamespace for this namespace, its defined above
			ok[i] = CanReadNamespace(ctx, dyn, ns)
			return nil
		})
	}
	_ = g.Wait() // the goroutines return no errors, a cancelled ctx is checked next
	// checking if the context was cancelled meanwhile, if so the answers cant be trusted
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	// sorting each namespace into readable or unreadable, in name order
	for i, ns := range names {
		// checking if this one can be read
		if ok[i] {
			readable = append(readable, ns)
		} else {
			unreadable = append(unreadable, ns)
		}
	}
	return readable, unreadable, nil
}

// IsPermission tells if err is the API server refusing the credentials something with Forbidden, anywhere in its chain
func IsPermission(err error) bool { return apierrors.IsForbidden(err) }

// dedupe gives back in without repeats, keeping the first of each in order
func dedupe(in []string) []string {
	var out []string
	// keeping each string not already seen
	for _, s := range in {
		// checking if its not in out yet
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// namespaceList names ns for a message, "namespace x" for one and "namespaces x, y" for several
func namespaceList(ns []string) string {
	// checking if theres only one, if so its singular
	if len(ns) == 1 {
		return "namespace " + ns[0]
	}
	return "namespaces " + strings.Join(ns, ", ")
}

// truncateList joins items for a message, showing at most n and saying how many more there are
func truncateList(items []string, n int) string {
	// checking if they all fit, if so theyre all shown
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" and %d more (kubectl get namespaces)", len(items)-n)
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
