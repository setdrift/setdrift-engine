// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Package helmsource records which chart repository a Helm release came
// from, which Helm itself does not keep, so RESTORE.md can write a complete
// helm install command. Two ways: Repos, a file the user passes to export
// (--helm-repos), and Annotate, a Helm post-renderer (setdrift
// helm-postrender) that adds the ChartRepo annotation to every object at
// install time, which the export reads back.
package helmsource

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"

	yamlv3 "go.yaml.in/yaml/v3"
)

// ChartRepo is the annotation naming a release's chart repository, e.g.
// https://charts.external-secrets.io or oci://registry/charts. An
// annotation rather than a label, since label values cannot hold a URL.
const ChartRepo = "setdrift.io/chart-repo"

// CheckRepoURL checks a chart repository URL: http(s):// or oci://, with a
// host, and no credentials. The URL is written into RESTORE.md (and, from
// the post-renderer, into every object in the cluster), so credentials in
// it would leak into Git; they are refused rather than stripped, so the
// user notices.
func CheckRepoURL(repo string) error {
	if repo == "" {
		return errors.New("no chart repository given")
	}
	u, err := url.Parse(repo)
	if err != nil {
		return fmt.Errorf("chart repository %q is not a URL", repo)
	}
	if u.User != nil {
		return errors.New("the chart repository URL holds credentials, which would be written into the export: " +
			"pass the URL without them, and give Helm the credentials separately (--username and --password, or helm registry login)")
	}
	switch u.Scheme {
	case "http", "https", "oci":
	default:
		return fmt.Errorf("chart repository %q must start with https://, http:// or oci://", repo)
	}
	if u.Host == "" {
		return fmt.Errorf("chart repository %q has no host", repo)
	}
	return nil
}

// maxRendered bounds what Annotate reads: Helm's rendered output for one
// release is far smaller, and everything read is held until it is written.
const maxRendered = 64 << 20

// errTooLarge is the error Annotate returns past maxRendered.
var errTooLarge = fmt.Errorf("rendered manifests larger than %d MiB", maxRendered>>20)

// cappedReader reads from r until n bytes have been read, then fails.
type cappedReader struct {
	r io.Reader
	n int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.r.Read(p)
	c.n -= int64(n)
	return n, err
}

// Annotate copies the YAML documents in r to w, adding ChartRepo=repo to
// each object's metadata.annotations. It reads at most 64 MiB.
func Annotate(r io.Reader, w io.Writer, repo string) error {
	if err := CheckRepoURL(repo); err != nil {
		return err
	}
	// one byte past the limit, so input of exactly the limit still reads to EOF
	dec := yamlv3.NewDecoder(&cappedReader{r: r, n: maxRendered + 1})
	var out bytes.Buffer
	enc := yamlv3.NewEncoder(&out)
	enc.SetIndent(2)
	for {
		var doc yamlv3.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading rendered manifests: %w", err)
		}
		if len(doc.Content) == 0 || doc.Content[0].Kind != yamlv3.MappingNode {
			continue // empty documents
		}
		annotations := child(child(doc.Content[0], "metadata"), "annotations")
		set(annotations, ChartRepo, repo)
		if err := enc.Encode(&doc); err != nil {
			return err
		}
	}
	if err := enc.Close(); err != nil {
		return err
	}
	_, err := w.Write(out.Bytes())
	return err
}

// child returns the mapping under key in m, creating it if missing.
func child(m *yamlv3.Node, key string) *yamlv3.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v := m.Content[i+1]
			if v.Kind != yamlv3.MappingNode {
				// e.g. "annotations: null"
				*v = yamlv3.Node{Kind: yamlv3.MappingNode, Tag: "!!map"}
			}
			return v
		}
	}
	v := &yamlv3.Node{Kind: yamlv3.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, &yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}

func set(m *yamlv3.Node, key, value string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = &yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: value}
			return
		}
	}
	m.Content = append(m.Content,
		&yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: key},
		&yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: value})
}
