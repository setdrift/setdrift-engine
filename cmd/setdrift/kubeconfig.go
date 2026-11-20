// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// What the CLI says when the kubeconfig or the cluster can't be used: what
// is wrong, in the user's terms, and what to do about it. client-go's own
// messages assume its internals (KUBERNETES_MASTER, discovery).

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// reachTimeout bounds the first request: a server that drops packets
// otherwise takes minutes of client-go retries to fail.
const reachTimeout = 15 * time.Second

// missingExec matches client-go's error for a credential plugin that is not installed.
var missingExec = regexp.MustCompile(`exec: executable (\S+) not found`)

// explainKubeconfig turns a kubeconfig loading error into one that says
// where SetDrift looked and what to do.
func explainKubeconfig(err error, rules *clientcmd.ClientConfigLoadingRules, raw clientcmdapi.Config, contextName string) error {
	if rules.ExplicitPath != "" {
		if _, statErr := os.Stat(rules.ExplicitPath); errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("--kubeconfig %s: no such file", rules.ExplicitPath)
		}
	}
	// a file that can't be read, or a directory: say which, for each file read
	source := "KUBECONFIG"
	if rules.ExplicitPath != "" {
		source = "--kubeconfig"
	}
	for _, p := range rules.GetLoadingPrecedence() {
		fi, statErr := os.Stat(p)
		switch {
		case statErr == nil && fi.IsDir():
			return fmt.Errorf("%s %s is a directory, not a kubeconfig file: give the file in it, e.g. %s", source, p, filepath.Join(p, "config"))
		case statErr == nil:
			if f, openErr := os.Open(p); openErr != nil {
				if errors.Is(openErr, os.ErrPermission) {
					return fmt.Errorf("%s %s: can't read it (permission denied). Check its owner and mode with `ls -l %s`; "+
						"a kubeconfig is normally readable by you only: chmod 600 %s", source, p, shellArg(p), shellArg(p))
				}
				return fmt.Errorf("%s %s: %w", source, p, openErr)
			} else {
				f.Close()
			}
		}
	}
	contexts := sortedContexts(raw)
	switch {
	case contextName != "" && raw.Contexts[contextName] == nil && len(contexts) > 0:
		return fmt.Errorf("the kubeconfig has no context %q; its contexts are: %s", contextName, listContexts(contexts))
	case clientcmd.IsEmptyConfig(err) && len(contexts) > 0 && raw.CurrentContext == "":
		return fmt.Errorf("the kubeconfig sets no current context: pass --context with one of %s, or choose one with `kubectl config use-context <name>`",
			listContexts(contexts))
	case clientcmd.IsEmptyConfig(err):
		return fmt.Errorf("no kubeconfig found (looked in %s). Point kubectl at a cluster first, for example with "+
			"`aws eks update-kubeconfig --name <cluster>`, `az aks get-credentials`, `gcloud container clusters get-credentials` "+
			"or `kind create cluster`, then check `kubectl get namespaces` works; or pass --kubeconfig <file>", lookedIn(rules))
	}
	return fmt.Errorf("reading the kubeconfig: %w", err)
}

// expandHome expands a leading ~ in a path the shell left alone, as in --kubeconfig=~/x.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[1:])
}

// lookedIn names the kubeconfig files SetDrift read, saying which are missing.
func lookedIn(rules *clientcmd.ClientConfigLoadingRules) string {
	var out []string
	for _, p := range rules.GetLoadingPrecedence() {
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			p += " (missing)"
		} else if fi, err := os.Stat(p); err == nil && fi.Size() == 0 {
			p += " (empty)"
		}
		out = append(out, p)
	}
	if v := os.Getenv("KUBECONFIG"); v != "" {
		return "KUBECONFIG: " + strings.Join(out, ", ")
	}
	return strings.Join(out, ", ")
}

func sortedContexts(raw clientcmdapi.Config) []string {
	names := make([]string, 0, len(raw.Contexts))
	for n := range raw.Contexts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// listContexts lists up to ten context names.
func listContexts(names []string) string {
	if len(names) > 10 {
		return strings.Join(names[:10], ", ") + fmt.Sprintf(" and %d more (`kubectl config get-contexts`)", len(names)-10)
	}
	return strings.Join(names, ", ")
}

// checkReachable asks the API server its version, with a short timeout, so
// an unreachable cluster or rejected credentials fail at once and plainly.
func checkReachable(ctx context.Context, kc *kubeContext) error {
	cfg := rest.CopyConfig(kc.config)
	cfg.Timeout = reachTimeout
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { _, err := dc.ServerVersion(); done <- err }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err = <-done:
	}
	if err == nil {
		return nil
	}
	return explainUnreachable(err, kc.name, kc.config.Host)
}

// explainUnreachable says why the first request to the cluster failed.
func explainUnreachable(err error, contextName, host string) error {
	check := fmt.Sprintf("check that `kubectl --context %s get namespaces` works", shellArg(contextName))
	var netErr net.Error
	var dnsErr *net.DNSError
	msg := err.Error()
	switch {
	case apierrors.IsUnauthorized(err):
		return fmt.Errorf("the cluster rejected the credentials of context %s (401 Unauthorized): they may have expired. "+
			"Refresh them (for example `aws sso login`, `az login`, `gcloud auth login`, or get the kubeconfig again), and %s", contextName, check)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("the cluster refused context %s (403 Forbidden): your identity is not allowed to read it; %s", contextName, check)
	case strings.Contains(msg, "getting credentials"):
		if m := missingExec.FindStringSubmatch(msg); m != nil {
			return fmt.Errorf("context %s gets its credentials by running `%s`, which is not installed or not on your PATH. "+
				"Install it (for EKS the AWS CLI, for AKS kubelogin, for GKE gke-gcloud-auth-plugin), then %s", contextName, m[1], check)
		}
		return fmt.Errorf("context %s gets its credentials from a command that failed: %w", contextName, err)
	case errors.As(err, &dnsErr):
		return fmt.Errorf("can't reach the API server of context %s: %s does not resolve (%v). If the cluster is private, connect to its network (VPN) first; %s",
			contextName, host, dnsErr.Name, check)
	case errors.As(err, &netErr) && netErr.Timeout(), errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("can't reach the API server of context %s at %s: no answer in %s. Is the cluster running, and reachable from here "+
			"(VPN, private endpoint, firewall)? %s", contextName, host, reachTimeout, capitalize(check))
	case strings.Contains(msg, "connection refused"):
		return fmt.Errorf("can't reach the API server of context %s at %s: connection refused. Is the cluster running? %s",
			contextName, host, capitalize(check))
	case strings.Contains(msg, "x509:") || strings.Contains(msg, "certificate"):
		return fmt.Errorf("can't trust the API server of context %s at %s: %v. The kubeconfig's certificate authority may be out of date: get the kubeconfig again",
			contextName, host, err)
	}
	return fmt.Errorf("can't reach the API server of context %s at %s: %w; %s", contextName, host, err, check)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// shellArg quotes s for a shell command shown to the user.
func shellArg(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:@", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
