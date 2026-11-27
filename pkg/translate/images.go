// the image copy plan: images in a cloud's private registry that the target cluster can't pull from, with the commands that copy them to the target cloud's registry
// SetDrift writes the commands, the user runs them with their own credentials, nothing is copied by SetDrift

package translate

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/internal/text"
	"setdrift.io/setdrift-engine/pkg/capability"
	"setdrift.io/setdrift-engine/pkg/platform"
)

// Image is an image the workloads use from a cloud's private registry, which the target cloud can't pull from as is
type Image struct {
	// Ref is the image as the manifests name it, like 111122223333.dkr.ecr.us-east-2.amazonaws.com/acme/shop:1.4
	Ref string
	// Host is its registry, Repo its repository path, and Tag its tag or digest with the : or @ in front
	Host, Repo, Tag string
	// Cloud is the registry's cloud, aws, azure or gcp
	Cloud string
	// Region is the registry's region where the host names one, ECR's
	Region string
}

// shellSafe matches a word a shell reads as it is, nothing in it expands or ends the command
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9@%+=:,./_-]+$`)

// shq quotes s for a shell command a user pastes, so a value from the cluster, like an image or an annotation, is only ever one word
// a value that needs no quoting stays as it is, so ordinary commands read plainly
func shq(s string) string {
	// checking if the shell reads it as it is, if so its left unquoted
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// cloudOf takes an image's registry host and gives back its cloud's id and region, empty for a public or other registry
func cloudOf(m *capability.Map, host string) (cloud, region string) {
	if c, r := m.RegistryCloud(host); c != nil {
		return c.ID, r
	}
	return "", ""
}

// public tells if an image is in a well-known public repository on a cloud's registry host, pulled from anywhere without credentials
func public(m *capability.Map, ref string) bool {
	for _, c := range m.Clouds {
		if slices.ContainsFunc(c.Registry.PublicRepositories, func(p string) bool { return strings.HasPrefix(ref, p) }) {
			return true
		}
	}
	return false
}

// parseImage splits an image reference into its registry host, repository path and tag or digest
func parseImage(ref string) (host, repo, tag string) {
	i := strings.Index(ref, "/")
	// checking if theres no host part, like nginx:1.27, if so its Docker Hub's and not a cloud's
	if i < 0 || !strings.ContainsAny(ref[:i], ".:") {
		return "", ref, ""
	}
	host, rest := ref[:i], ref[i+1:]
	// checking for a digest first, then a tag after the last slash
	if at := strings.Index(rest, "@"); at >= 0 {
		return host, rest[:at], rest[at:]
	}
	if c := strings.LastIndex(rest, ":"); c > strings.LastIndex(rest, "/") {
		return host, rest[:c], rest[c:]
	}
	return host, rest, ""
}

// imagesToCopy takes the export's objects and gives back the images in another cloud's private registry than the target's, once each, sorted
func imagesToCopy(m *capability.Map, objs []*unstructured.Unstructured, to platform.Distribution) []Image {
	target := m.For(to).Cloud
	seen := map[string]bool{}
	var out []Image
	// going through each object that has a pod template, or is a Pod
	for _, o := range objs {
		base, ok := podSpecPath(o.GetKind())
		// checking if it runs containers, if not theres no image
		if !ok {
			continue
		}
		// going through its init containers and containers
		for _, field := range []string{"initContainers", "containers"} {
			cs, _, _ := unstructured.NestedSlice(o.Object, append(append([]string{}, base...), field)...)
			for _, c := range cs {
				ref, _ := c.(map[string]any)["image"].(string)
				host, repo, tag := parseImage(ref)
				cloud, region := cloudOf(m, host)
				// checking if its in a cloud's private registry other than the target's, not a well-known public one, and not listed yet
				if cloud == "" || cloud == target || seen[ref] || public(m, ref) {
					continue
				}
				seen[ref] = true
				out = append(out, Image{Ref: ref, Host: host, Repo: repo, Tag: tag, Cloud: cloud, Region: region})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// sourceLogin gives back the user name and the command that prints a password for pulling from an image's registry, as its cloud's YAML says
func sourceLogin(m *capability.Map, img Image) (user, password string) {
	c := m.Cloud(img.Cloud)
	if c == nil {
		return "", ""
	}
	name, _, _ := strings.Cut(img.Host, ".")
	return c.Registry.Login.User, strings.NewReplacer("{region}", img.Region, "{name}", name).Replace(c.Registry.Login.Password)
}

// imagesStep writes RESTORE.md's step that copies the images to the target's registry, then points the translation at the copies
func imagesStep(m *capability.Map, b *strings.Builder, imgs []Image, to platform.Distribution, dir string) {
	fmt.Fprintf(b, "The workloads use %s from another cloud's registry, which the new cluster can't pull from without that cloud's credentials. Copy each to %s's registry, then point the translation at the copies. An image you know is public pulls as it is, skip it. Copy by tag as shown, or pin a digest (`crane digest <image>`). `crane copy` copies every platform of a multi-platform image.\n\n",
		text.Plural(len(imgs), "image", "images"), to.Name())
	ic := m.For(to).Translate.Images
	cmds := append([]string{}, ic.Setup...)
	// invoking the target's copy strategy for the commands after its setup, theyre in strategies.go
	if copyImages := imageCopies[ic.Strategy]; copyImages != nil {
		cmds = append(cmds, copyImages(m, imgs, ic)...)
	}
	fmt.Fprintf(b, "```sh\n%s\n```\n\n", strings.Join(cmds, "\n"))
	fmt.Fprintf(b, "Then add these to %s/kustomization.yaml, with your registry in place of `<registry>`, so the Pods pull the copies:\n\n```yaml\nimages:\n", dir)
	names := []string{}
	for _, img := range imgs {
		name := img.Host + "/" + img.Repo
		if !slices.Contains(names, name) {
			names = append(names, name)
			// quoted, so a value from the cluster stays one YAML string
			fmt.Fprintf(b, "- name: %s\n  newName: %s\n", strconv.Quote(name), strconv.Quote("<registry>/"+img.Repo))
		}
	}
	b.WriteString("```\n\n")
}

// loginLines gives back the crane sign-in to each source registry
func loginLines(m *capability.Map, imgs []Image) []string {
	// collecting each source registry once, for its sign-in line
	var hosts, out []string
	for _, img := range imgs {
		if !slices.Contains(hosts, img.Host) {
			hosts = append(hosts, img.Host)
		}
	}
	for _, h := range hosts {
		i := slices.IndexFunc(imgs, func(img Image) bool { return img.Host == h })
		user, pw := sourceLogin(m, imgs[i])
		out = append(out, fmt.Sprintf("%s | crane auth login %s -u %s --password-stdin", pw, h, user))
	}
	return out
}

// tagOrLatest gives back the tag or digest, or :latest when the image names none, which is what a pull takes
func tagOrLatest(tag string) string {
	if tag == "" {
		return ":latest"
	}
	// a digest copies to a tag named after it, since az acr import and a new repository need a tag
	if strings.HasPrefix(tag, "@") {
		return ":" + strings.TrimPrefix(strings.ReplaceAll(tag, ":", "-"), "@")
	}
	return tag
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
