// reads the kubeconfig, and says what is wrong in the user's terms and what to do when it or the cluster cant be used
// client-go's own messages assume its internals, like KUBERNETES_MASTER or discovery, so they get explained here instead

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

// reachTimeout bounds the first request to the cluster, otherwise a server that drops packets takes minutes of client-go retries to fail
const reachTimeout = 15 * time.Second

// missingExec matches client-go's error for a credential plugin that isnt installed, the group is the plugin's name
var missingExec = regexp.MustCompile(`exec: executable (\S+) not found`)

// explainKubeconfig takes a kubeconfig loading error and turns it into one that says where SetDrift looked and what to do
func explainKubeconfig(err error, rules *clientcmd.ClientConfigLoadingRules, raw clientcmdapi.Config, contextName string) error {
	// checking if --kubeconfig was given
	if rules.ExplicitPath != "" {
		// checking if that file doesnt exist, if so thats the error
		if _, statErr := os.Stat(rules.ExplicitPath); errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("--kubeconfig %s: no such file", rules.ExplicitPath)
		}
	}
	// next a file that cant be read, or a directory, the error says which for each file read
	source := "KUBECONFIG"
	// checking if --kubeconfig was given, if so the error names the flag rather than the variable
	if rules.ExplicitPath != "" {
		source = "--kubeconfig"
	}
	// going through each file client-go reads, in its order
	for _, p := range rules.GetLoadingPrecedence() {
		fi, statErr := os.Stat(p)
		// checking if the path is a directory, or a file os.Stat found, which then has to open
		switch {
		case statErr == nil && fi.IsDir():
			return fmt.Errorf("%s %s is a directory, not a kubeconfig file: give the file in it, e.g. %s", source, p, filepath.Join(p, "config"))
		case statErr == nil:
			f, openErr := os.Open(p) //#nosec G304 -- the user's kubeconfig
			// checking if permission was denied, if so the error says how to check and fix it, text.ShellQuote is defined in internal/text/text.go
			if errors.Is(openErr, os.ErrPermission) {
				return fmt.Errorf("%s %s: can't read it (permission denied). Check its owner and mode with `ls -l %s`; "+
					"a kubeconfig is normally readable by you only: chmod 600 %s", source, p, text.ShellQuote(p), text.ShellQuote(p))
			}
			// checking if it failed to open for another reason
			if openErr != nil {
				return fmt.Errorf("%s %s: %w", source, p, openErr)
			}
			// closing it right away, it was only opened to see that it can be read
			_ = f.Close()
		}
	}
	// invoking sortedContexts to list the contexts the kubeconfig has, its defined below
	contexts := sortedContexts(raw)
	// checking if the context asked for is missing, if no context is current, or if theres no kubeconfig at all
	// invoking listContexts and lookedIn to word the lists in those errors, theyre defined below
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
	// anything else is a terminal error, wrapped as is
	return fmt.Errorf("reading the kubeconfig: %w", err)
}

// expandHome takes a path and expands a leading ~ the shell left alone, as in --kubeconfig=~/x
func expandHome(path string) string {
	// checking if the path doesnt start with ~ or ~/, if so it stays as it is
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	// checking if the home directory is unknown, if so the path stays as it is
	if err != nil {
		return path
	}
	return filepath.Join(home, path[1:])
}

// lookedIn names the kubeconfig files SetDrift read, saying which are missing or empty, for the no kubeconfig found error
func lookedIn(rules *clientcmd.ClientConfigLoadingRules) string {
	var out []string
	// going through each file client-go reads
	for _, p := range rules.GetLoadingPrecedence() {
		// checking if the file is missing or empty, if so the name says so
		switch fi, err := os.Stat(p); {
		case errors.Is(err, os.ErrNotExist):
			p += " (missing)"
		case err == nil && fi.Size() == 0:
			p += " (empty)"
		}
		out = append(out, p)
	}
	// checking if KUBECONFIG is set, if so the list says the files came from it
	if v := os.Getenv("KUBECONFIG"); v != "" {
		return "KUBECONFIG: " + strings.Join(out, ", ")
	}
	return strings.Join(out, ", ")
}

// sortedContexts gives back the kubeconfig's context names sorted
func sortedContexts(raw clientcmdapi.Config) []string {
	return slices.Sorted(maps.Keys(raw.Contexts))
}

// listContexts lists up to ten context names for an error message, pointing to kubectl for the rest
func listContexts(names []string) string {
	// checking if there are more than ten, if so the rest are only counted
	if len(names) > 10 {
		return strings.Join(names[:10], ", ") + fmt.Sprintf(" and %d more (`kubectl config get-contexts`)", len(names)-10)
	}
	return strings.Join(names, ", ")
}

// warnInsecure warns when the context turns off the API server's certificate check (insecure-skip-tls-verify)
// then what export or verify reads comes from whatever answers at that address
func warnInsecure(w io.Writer, kc *kubeContext) {
	// checking if the context skips the certificate check, if so the warning is printed
	if kc.config.Insecure {
		fmt.Fprintf(w, "warning: context %s skips the API server's certificate check (insecure-skip-tls-verify): "+
			"anything on the network path to %s could answer in the cluster's place\n", kc.name, kc.config.Host)
	}
}

// checkReachable asks the API server its version with a short timeout, so an unreachable cluster or rejected credentials fail at once and clearly
func checkReachable(ctx context.Context, kc *kubeContext) error {
	cfg := rest.CopyConfig(kc.config)
	cfg.Timeout = reachTimeout
	// invoking discover.New for a client to send the request with, its defined in internal/discover/discover.go
	dc, err := discover.New(cfg)
	if err != nil {
		return err
	}
	// sending GET /version like ServerVersion does, but with ctx so ctrl-c ends the wait
	err = dc.RESTClient().Get().AbsPath("/version").Do(ctx).Error()
	// checking if the cluster answered, if so its reachable
	if err == nil {
		return nil
	}
	// checking if the context was canceled, like by ctrl-c, if so thats the error rather than an unreachable cluster
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	// invoking explainUnreachable to say why the request failed, its defined below
	return explainUnreachable(err, kc.name, kc.config.Host)
}

// explainUnreachable takes the error of the first request to the cluster and says why it failed and what to check
// some causes are told apart by the error's text, client-go wraps a credential plugin's, a refused connection's and a certificate's failures without a type to check
func explainUnreachable(err error, contextName, host string) error {
	// the kubectl command every message suggests, text.ShellQuote is defined in internal/text/text.go
	check := fmt.Sprintf("check that `kubectl --context %s get namespaces` works", text.ShellQuote(contextName))
	var netErr net.Error
	var dnsErr *net.DNSError
	msg := err.Error()
	// checking which kind of failure it is, rejected credentials, a refusal, a credential plugin, dns, a timeout, a refused connection or a certificate
	// invoking capitalize where the check starts a sentence, its defined below
	switch {
	case apierrors.IsUnauthorized(err):
		return fmt.Errorf("the cluster rejected the credentials of context %s (401 Unauthorized): they may have expired. "+
			"Refresh them (for example `aws sso login`, `az login`, `gcloud auth login`, or get the kubeconfig again), and %s", contextName, check)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("the cluster refused context %s (403 Forbidden): your identity is not allowed to read it; %s", contextName, check)
	case strings.Contains(msg, "getting credentials"):
		// checking if the credential plugin isnt installed, if so the error names it and where to get it
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
	// anything else is a terminal error, wrapped with the check to try
	return fmt.Errorf("can't reach the API server of context %s at %s: %w; %s", contextName, host, err, check)
}

// capitalize gives back s with its first letter upper-cased, so a message can start a sentence
// it assumes s starts with an ascii letter, a multi-byte first character would be cut
func capitalize(s string) string {
	// checking if s is empty, if so theres nothing to upper-case
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// kubeContext is what the cli takes from the kubeconfig for one context, loadConfig fills it
type kubeContext struct {
	// the client config to connect with
	config *rest.Config
	// the context's name
	name string
	// the context's namespace, empty if it sets none
	namespace string
	// the EKS cluster name and region and the AWS profile, when the context shows them
	// that is an EKS ARN, `aws eks get-token` credentials, or an EKS API server address
	eksName, eksRegion, awsProfile string
	// what the platform classifier reads from the context, its first pass
	platform platform.Kubeconfig
}

// loadConfig reads the kubeconfig at kubeconfig, or the default ones, for the context contextName, or the current one
// it gives back the client config tuned for setdrift, with the eks details the context shows
func loadConfig(kubeconfig, contextName string) (*kubeContext, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	// checking if a kubeconfig path was given, if so its the file read
	if kubeconfig != "" {
		// invoking expandHome to turn a leading ~ into the home directory, its defined above
		rules.ExplicitPath = expandHome(kubeconfig)
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: contextName})
	raw, err := cc.RawConfig()
	// checking if the kubeconfig couldnt be loaded, like unparseable yaml or a file that cant be read
	// invoking explainKubeconfig to say where it looked and what to do, its defined above
	if err != nil {
		return nil, explainKubeconfig(err, rules, raw, contextName)
	}
	cfg, err := cc.ClientConfig()
	// checking if the context cant be turned into a client config, if so explainKubeconfig says why, its defined above
	if err != nil {
		return nil, explainKubeconfig(err, rules, raw, contextName)
	}
	cfg.QPS, cfg.Burst = 50, 100
	cfg.UserAgent = "setdrift/" + version.Version
	// silencing api deprecation warnings, like v1 Endpoints listed by discovery, theyre not the user's to act on
	cfg.WarningHandler = rest.NoWarnings{}
	// checking if no context was asked for, if so its the current one
	if contextName == "" {
		contextName = raw.CurrentContext
	}
	kc := &kubeContext{config: cfg, name: contextName, platform: platform.Kubeconfig{Context: contextName, Server: cfg.Host}}
	// checking if the context is in the kubeconfig, if so its namespace is read from it directly
	// ClientConfig().Namespace() would give "default" when the context sets none
	if kctx := raw.Contexts[contextName]; kctx != nil {
		kc.namespace = kctx.Namespace
		// checking if the context's user gets its credentials from a command, if so that command can name the eks cluster
		if user := raw.AuthInfos[kctx.AuthInfo]; user != nil && user.Exec != nil {
			// invoking eksExec to read the cluster, region and profile from it, its defined below
			kc.eksName, kc.eksRegion, kc.awsProfile = eksExec(user.Exec.Command, user.Exec.Args, user.Exec.Env)
			kc.platform.ExecCmd, kc.platform.ExecArgs = user.Exec.Command, user.Exec.Args
		}
	}
	// invoking eksARN and checking if the context name is an eks cluster arn, if so its region and name win, its defined below
	if region, name, ok := eksARN(contextName); ok {
		kc.eksRegion, kc.eksName = region, name
	}
	// checking if the region is still unknown, if so eksServerRegion tries the api server address, its defined below
	if kc.eksRegion == "" {
		kc.eksRegion = eksServerRegion(cfg.Host)
	}
	return kc, nil
}

// eksExec reads the EKS cluster name, region and AWS profile from a kubeconfig's `aws eks get-token` credential plugin
func eksExec(command string, args []string, env []clientcmdapi.ExecEnvVar) (name, region, profile string) {
	// checking if the command isnt aws, or aws.exe on windows, possibly as a full path, if so theres nothing to read
	if strings.TrimSuffix(filepath.Base(command), ".exe") != "aws" {
		return "", "", ""
	}
	// going through the arguments, taking both --region us-east-2 and --region=us-east-2
	for i := 0; i < len(args); i++ {
		flag, value, hasValue := strings.Cut(args[i], "=")
		// checking if the value isnt after an =, if so its the next argument
		if !hasValue {
			// checking if this is the last argument, if so theres no value left to read
			if i+1 >= len(args) {
				break
			}
			value = args[i+1]
		}
		// checking which flag it is, and keeping the value of the ones that matter
		switch flag {
		case "--cluster-name", "--cluster-id":
			name = value
		case "--region":
			region = value
		case "--profile":
			profile = value
		}
	}
	// checking if theres no --profile, if so AWS_PROFILE is used, --profile wins in the aws cli
	if profile == "" {
		// going through the plugin's environment to find AWS_PROFILE
		for _, e := range env {
			// checking if its AWS_PROFILE
			if e.Name == "AWS_PROFILE" {
				profile = e.Value
			}
		}
	}
	return name, region, profile
}

// eksARN takes a string and gives back the region and name when its an EKS cluster ARN
func eksARN(s string) (region, name string, ok bool) {
	parts := strings.SplitN(s, ":", 6)
	// checking if its not shaped like arn:partition:eks:region:account:cluster/name, if so its not an eks cluster arn
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "eks" || !strings.HasPrefix(parts[5], "cluster/") {
		return "", "", false
	}
	return parts[3], strings.TrimPrefix(parts[5], "cluster/"), true
}

// eksServerRegion takes an api server address and gives back its region when its an EKS one, empty otherwise
func eksServerRegion(host string) string {
	// stripping the scheme, the port and any path, leaving the host name
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host, _, _ = strings.Cut(host, ":")
	host, _, _ = strings.Cut(host, "/")
	parts := strings.Split(host, ".")
	// checking if its an eks address, if so the region is the part before eks.amazonaws.com
	if n := len(parts); n >= 4 && strings.HasSuffix(host, ".eks.amazonaws.com") {
		return parts[n-4]
	}
	return ""
}

// applyContext fills in the cluster's platform, provider id and name from the kubeconfig context, where the snapshot didnt set them
func applyContext(c *snapshotv1alpha1.Cluster, kc *kubeContext) {
	contextName := kc.name
	// checking if the context showed an eks cluster name or region, if so the platform is eks
	if kc.eksName != "" || kc.eksRegion != "" {
		c.Platform = "eks"
	}
	// invoking eksARN and checking if the context name is an eks cluster arn, if so it is the provider id, its defined above
	if _, name, ok := eksARN(contextName); ok {
		c.Platform = "eks"
		c.ProviderId = contextName
		// checking if the cluster has no name yet, if so it takes the one in the arn
		if c.Name == "" {
			c.Name = name
		}
	}
	// checking if the cluster still has no name, an aliased eks context, if so the credentials name the cluster
	if c.Name == "" {
		c.Name = kc.eksName
	}
	// checking if theres still no name, if so the context's name is used
	if c.Name == "" {
		c.Name = contextName
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
