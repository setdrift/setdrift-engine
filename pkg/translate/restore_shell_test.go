// runs every shell command a translation's RESTORE.md writes against stub tools, to prove hostile values from the cluster never run as commands

package translate

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"setdrift.io/setdrift-engine/pkg/platform"
)

// hostile is a value a cluster could carry in an image, an annotation or a setting, every way a shell would run part of it creates the file PWNED
const hostile = "h$(touch PWNED)`touch PWNED`;touch PWNED&&touch PWNED|touch PWNED\ntouch PWNED #'\""

// check that no command in a translation's RESTORE.md runs anything but what it names when the cluster's values are hostile, users paste these with their cloud credentials
// every line of every shell block runs with stub az, aws, crane, gcloud, kubectl and setdrift, for each target, and no PWNED file may appear
func TestTranslateRestoreQuotesValues(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	bin, work := t.TempDir(), t.TempDir()
	log := filepath.Join(work, "ran.log")
	for _, name := range []string{"az", "aws", "crane", "gcloud", "kubectl", "setdrift"} {
		stub := "#!/bin/sh\necho " + name + " >> '" + log + "'\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(stub), 0o755); err != nil { // #nosec G306 -- a stub the test runs
			t.Fatal(err)
		}
	}
	env := append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
	imgs := []Image{
		{Ref: "111122223333.dkr.ecr.us-east-2.amazonaws.com/" + hostile, Host: "111122223333.dkr.ecr.us-east-2.amazonaws.com", Repo: hostile, Tag: ":" + hostile, Cloud: "aws", Region: "us-east-2"},
		{Ref: "acmeshop.azurecr.io/" + hostile, Host: "acmeshop.azurecr.io", Repo: hostile, Cloud: "azure"},
	}
	ids := []Identity{
		{Namespace: "shop", ServiceAccount: "reports", Cloud: "aws", ID: "arn:aws:iam::111122223333:role/" + hostile},
		{Namespace: "shop", ServiceAccount: "orders", Cloud: "azure", ID: hostile},
		{Namespace: "shop", ServiceAccount: "web", Cloud: "gcp", ID: hostile},
	}
	shape := Shape{Nodes: 2, MilliCPU: 4000, MemoryBytes: 16 << 30, PodCIDRs: []string{hostile}, ServiceCIDRs: []string{hostile}}
	blocks := 0
	for _, to := range []platform.Distribution{platform.AKS, platform.EKS, platform.GKE} {
		r := &Result{From: platform.Unknown, To: to, Dir: "translations/tx", Images: imgs, Identities: ids, NetworkPolicyEngine: "Calico", NetworkPolicies: 1}
		md := restore(r, RestoreInfo{Cluster: "prod", SourceVersion: "v1.37.1" + hostile, Shape: shape})
		for _, block := range shellBlocks(md) {
			blocks++
			for _, line := range commandLines(block) {
				cmd := exec.Command(sh, "-c", line) // #nosec G204 -- the commands under test, with stub tools
				cmd.Dir, cmd.Env = work, env
				_ = cmd.Run() // placeholders like <resource group> make some lines fail, only what they run matters
				if _, err := os.Stat(filepath.Join(work, "PWNED")); err == nil {
					t.Fatalf("%s: this line ran an injected command:\n%s", to, line)
				}
			}
		}
	}
	if blocks < 6 {
		t.Fatalf("only %d shell blocks rendered: the fixture no longer reaches the steps", blocks)
	}
	if ran, err := os.ReadFile(log); err != nil || len(ran) == 0 { // #nosec G304 -- the test's own log
		t.Fatal("no command ran at all: the test checks nothing")
	}
}

// shellBlocks gives back the contents of the ```sh blocks in md
func shellBlocks(md string) []string {
	var out []string
	for _, part := range strings.Split(md, "```sh\n")[1:] {
		if block, _, ok := strings.Cut(part, "\n```"); ok {
			out = append(out, block)
		}
	}
	return out
}

// commandLines splits a block into the commands a shell reads from it, at each newline outside quotes, joining a line that ends in a backslash with the next
func commandLines(block string) []string {
	var out []string
	var cur strings.Builder
	var quote rune // the open quote, or 0
	rs := []rune(block)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			}
		case r == '\\' && i+1 < len(rs):
			if rs[i+1] == '\n' && quote == 0 {
				i++ // a line continuation
				cur.WriteRune(' ')
				continue
			}
			cur.WriteRune(r)
			i++
			r = rs[i]
		case quote == '"':
			if r == '"' {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '#' && (i == 0 || rs[i-1] == ' ' || rs[i-1] == '\t' || rs[i-1] == '\n'):
			// a comment, to the end of the line, quotes in it are text
			for i+1 < len(rs) && rs[i+1] != '\n' {
				i++
			}
			continue
		case r == '\n':
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	return append(out, cur.String())
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
