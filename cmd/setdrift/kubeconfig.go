// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Reading the kubeconfig, and what the CLI says when it or the cluster can't
// be used: what is wrong, in the user's terms, and what to do about it.
// client-go's own messages assume its internals (KUBERNETES_MASTER,
// discovery).

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"setdrift.io/setdrift-engine/internal/discover"
	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/internal/version"
	"setdrift.io/setdrift-engine/pkg/platform"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
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
			f, openErr := os.Open(p) //#nosec G304 -- the user's kubeconfig
			if errors.Is(openErr, os.ErrPermission) {
				return fmt.Errorf("%s %s: can't read it (permission denied). Check its owner and mode with `ls -l %s`; "+
					"a kubeconfig is normally readable by you only: chmod 600 %s", source, p, text.ShellQuote(p), text.ShellQuote(p))
			}
			if openErr != nil {
				return fmt.Errorf("%s %s: %w", source, p, openErr)
			}
			_ = f.Close() // opened only to see that it can be read
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
		switch fi, err := os.Stat(p); {
		case errors.Is(err, os.ErrNotExist):
			p += " (missing)"
		case err == nil && fi.Size() == 0:
			p += " (empty)"
		}
		out = append(out, p)
	}
	if v := os.Getenv("KUBECONFIG"); v != "" {
		return "KUBECONFIG: " + strings.Join(out, ", ")
	}
	return strings.Join(out, ", ")
}

// sortedContexts returns the kubeconfig's context names, sorted.
func sortedContexts(raw clientcmdapi.Config) []string {
	return slices.Sorted(maps.Keys(raw.Contexts))
}

// listContexts lists up to ten context names.
func listContexts(names []string) string {
	if len(names) > 10 {
		return strings.Join(names[:10], ", ") + fmt.Sprintf(" and %d more (`kubectl config get-contexts`)", len(names)-10)
	}
	return strings.Join(names, ", ")
}

// warnInsecure warns when the context turns off the API server's
// certificate check (insecure-skip-tls-verify): what the export or verify
// reads then comes from whatever answers at that address.
func warnInsecure(w io.Writer, kc *kubeContext) {
	if kc.config.Insecure {
		fmt.Fprintf(w, "warning: context %s skips the API server's certificate check (insecure-skip-tls-verify): "+
			"anything on the network path to %s could answer in the cluster's place\n", kc.name, kc.config.Host)
	}
}

// checkReachable asks the API server its version, with a short timeout, so
// an unreachable cluster or rejected credentials fail at once and plainly.
func checkReachable(ctx context.Context, kc *kubeContext) error {
	cfg := rest.CopyConfig(kc.config)
	cfg.Timeout = reachTimeout
	dc, err := discover.New(cfg)
	if err != nil {
		return err
	}
	// GET /version, as ServerVersion does, but with ctx: Ctrl-C ends the wait
	err = dc.RESTClient().Get().AbsPath("/version").Do(ctx).Error()
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return explainUnreachable(err, kc.name, kc.config.Host)
}

// explainUnreachable says why the first request to the cluster failed. Some
// causes are told apart by the error's text: client-go wraps a credential
// plugin's, a refused connection's and a certificate's failures without a
// type to check.
func explainUnreachable(err error, contextName, host string) error {
	check := fmt.Sprintf("check that `kubectl --context %s get namespaces` works", text.ShellQuote(contextName))
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

// capitalize returns s with its first letter upper-cased (s is ASCII).
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// kubeContext is what the CLI takes from the kubeconfig.
type kubeContext struct {
	config *rest.Config
	name   string
	// namespace is the context's namespace; empty if it sets none.
	namespace string
	// EKS cluster name and region and the AWS profile, when the context
	// shows them (an EKS ARN, `aws eks get-token` credentials, or an EKS
	// API server address).
	eksName, eksRegion, awsProfile string
	// what the platform classifier reads from the context (its first pass)
	platform platform.Kubeconfig
}

// loadConfig reads the kubeconfig at kubeconfig (or the default ones), for
// the context contextName (or the current one).
func loadConfig(kubeconfig, contextName string) (*kubeContext, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = expandHome(kubeconfig)
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: contextName})
	raw, err := cc.RawConfig()
	if err != nil {
		// unparseable YAML, or a file that can't be read
		return nil, explainKubeconfig(err, rules, raw, contextName)
	}
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, explainKubeconfig(err, rules, raw, contextName)
	}
	cfg.QPS, cfg.Burst = 50, 100
	cfg.UserAgent = "setdrift/" + version.Version
	// API deprecation warnings (e.g. v1 Endpoints, listed by discovery) are not the user's to act on
	cfg.WarningHandler = rest.NoWarnings{}
	if contextName == "" {
		contextName = raw.CurrentContext
	}
	kc := &kubeContext{config: cfg, name: contextName, platform: platform.Kubeconfig{Context: contextName, Server: cfg.Host}}
	// Read from the context itself, ClientConfig().Namespace() gives "default" when the context sets none
	if kctx := raw.Contexts[contextName]; kctx != nil {
		kc.namespace = kctx.Namespace
		if user := raw.AuthInfos[kctx.AuthInfo]; user != nil && user.Exec != nil {
			kc.eksName, kc.eksRegion, kc.awsProfile = eksExec(user.Exec.Command, user.Exec.Args, user.Exec.Env)
			kc.platform.ExecCmd, kc.platform.ExecArgs = user.Exec.Command, user.Exec.Args
		}
	}
	if region, name, ok := eksARN(contextName); ok {
		kc.eksRegion, kc.eksName = region, name
	}
	if kc.eksRegion == "" {
		kc.eksRegion = eksServerRegion(cfg.Host)
	}
	return kc, nil
}

// eksExec reads the EKS cluster name, region and AWS profile from a
// kubeconfig's `aws eks get-token` credential plugin.
func eksExec(command string, args []string, env []clientcmdapi.ExecEnvVar) (name, region, profile string) {
	// aws, or aws.exe on Windows, possibly as a full path
	if strings.TrimSuffix(filepath.Base(command), ".exe") != "aws" {
		return "", "", ""
	}
	for i := 0; i < len(args); i++ {
		// both --region us-east-2 and --region=us-east-2
		flag, value, hasValue := strings.Cut(args[i], "=")
		if !hasValue {
			if i+1 >= len(args) {
				break
			}
			value = args[i+1]
		}
		switch flag {
		case "--cluster-name", "--cluster-id":
			name = value
		case "--region":
			region = value
		case "--profile":
			profile = value
		}
	}
	// AWS_PROFILE only when there's no --profile, which wins in the AWS CLI
	if profile == "" {
		for _, e := range env {
			if e.Name == "AWS_PROFILE" {
				profile = e.Value
			}
		}
	}
	return name, region, profile
}

// eksARN returns the region and name of an EKS cluster ARN.
func eksARN(s string) (region, name string, ok bool) {
	parts := strings.SplitN(s, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "eks" || !strings.HasPrefix(parts[5], "cluster/") {
		return "", "", false
	}
	return parts[3], strings.TrimPrefix(parts[5], "cluster/"), true
}

// eksServerRegion returns the region of an EKS API server address.
func eksServerRegion(host string) string {
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host, _, _ = strings.Cut(host, ":")
	host, _, _ = strings.Cut(host, "/")
	parts := strings.Split(host, ".")
	if n := len(parts); n >= 4 && strings.HasSuffix(host, ".eks.amazonaws.com") {
		return parts[n-4]
	}
	return ""
}

// applyContext fills in cluster details from the kubeconfig context.
func applyContext(c *snapshotv1alpha1.Cluster, kc *kubeContext) {
	contextName := kc.name
	if kc.eksName != "" || kc.eksRegion != "" {
		c.Platform = "eks"
	}
	if _, name, ok := eksARN(contextName); ok {
		c.Platform = "eks"
		c.ProviderId = contextName
		if c.Name == "" {
			c.Name = name
		}
	}
	if c.Name == "" {
		// An aliased EKS context, the credentials name the cluster
		c.Name = kc.eksName
	}
	if c.Name == "" {
		c.Name = contextName
	}
}
