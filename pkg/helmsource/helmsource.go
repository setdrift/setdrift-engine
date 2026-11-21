// Package helmsource records which chart repository a Helm release came from, which Helm itself doesnt keep, so RESTORE.md can write a complete helm install command
// it does it two ways, Repos, a file the user passes to export with --helm-repos, and Annotate, a Helm post-renderer (setdrift helm-postrender)
// the post-renderer adds the ChartRepo annotation to every object at install time, and the export reads it back
package helmsource

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"

	yamlv3 "go.yaml.in/yaml/v3"
)

// ChartRepo is the annotation naming a release's chart repository, like https://charts.external-secrets.io or oci://registry/charts
// its an annotation rather than a label, since label values cant hold a URL
const ChartRepo = "setdrift.io/chart-repo"

// CheckRepoURL takes a chart repository URL and checks it is http(s):// or oci://, with a host, and no credentials
// the URL is written into RESTORE.md, and from the post-renderer into every object in the cluster, so credentials in it would leak into Git
// theyre refused rather than stripped, so the user notices
func CheckRepoURL(repo string) error {
	// checking if no URL was given at all
	if repo == "" {
		return errors.New("no chart repository given")
	}
	u, err := url.Parse(repo)
	// checking if it doesnt parse as a URL
	if err != nil {
		return fmt.Errorf("chart repository %q is not a URL", repo)
	}
	// checking if the URL holds a user or password, those must never reach the export
	if u.User != nil {
		return errors.New("the chart repository URL holds credentials, which would be written into the export: " +
			"pass the URL without them, and give Helm the credentials separately (--username and --password, or helm registry login)")
	}
	// checking the scheme, only the ones helm install takes are allowed
	switch u.Scheme {
	case "http", "https", "oci":
	default:
		return fmt.Errorf("chart repository %q must start with https://, http:// or oci://", repo)
	}
	// checking if theres a host, a URL like https:///charts parses but points nowhere
	if u.Host == "" {
		return fmt.Errorf("chart repository %q has no host", repo)
	}
	return nil
}

// maxRendered bounds what Annotate reads, Helm's rendered output for one release is far smaller, and everything read is held until its written
const maxRendered = 64 << 20

// errTooLarge is the error Annotate gives back once the input goes past maxRendered
var errTooLarge = fmt.Errorf("rendered manifests larger than %d MiB", maxRendered>>20)

// cappedReader reads from r until n bytes have been read, then fails with errTooLarge
type cappedReader struct {
	r io.Reader
	// how many bytes are still allowed
	n int64
}

// Read reads into p from the underlying reader, never past the bytes still allowed, and fails once theyre used up
func (c *cappedReader) Read(p []byte) (int, error) {
	// checking if the allowance is used up, if so the input is too large
	if c.n <= 0 {
		return 0, errTooLarge
	}
	// checking if p asks for more than is still allowed, if so its shortened
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.r.Read(p)
	c.n -= int64(n)
	return n, err
}

// Annotate copies the YAML documents in r to w, adding ChartRepo=repo to each object's metadata.annotations, its what setdrift helm-postrender runs
// it reads at most 64 MiB, and writes nothing until every document was read, so a failure leaves w untouched
func Annotate(r io.Reader, w io.Writer, repo string) error {
	// invoking CheckRepoURL to refuse a bad or credentialed URL before anything is read, its defined above
	if err := CheckRepoURL(repo); err != nil {
		return err
	}
	// one byte past the limit, so input of exactly the limit still reads to EOF
	dec := yamlv3.NewDecoder(&cappedReader{r: r, n: maxRendered + 1})
	var out bytes.Buffer
	enc := yamlv3.NewEncoder(&out)
	enc.SetIndent(2)
	// reading one document at a time until the input ends
	for {
		var doc yamlv3.Node
		err := dec.Decode(&doc)
		// checking if the input has ended, if so the loop is done
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading rendered manifests: %w", err)
		}
		// checking if the document is empty or not a mapping, if so its dropped instead of annotated
		if len(doc.Content) == 0 || doc.Content[0].Kind != yamlv3.MappingNode {
			continue
		}
		// invoking child twice to reach metadata.annotations, creating them when missing, its defined below
		annotations := child(child(doc.Content[0], "metadata"), "annotations")
		// invoking set to add or replace the ChartRepo annotation, its defined below
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

// child takes a mapping node and a key and gives back the mapping under that key, creating it when missing
func child(m *yamlv3.Node, key string) *yamlv3.Node {
	// going over the mapping's key and value pairs
	for i := 0; i+1 < len(m.Content); i += 2 {
		// checking if this is the key
		if m.Content[i].Value == key {
			v := m.Content[i+1]
			// checking if the value isnt a mapping, like "annotations: null", if so its replaced with an empty one
			if v.Kind != yamlv3.MappingNode {
				*v = yamlv3.Node{Kind: yamlv3.MappingNode, Tag: "!!map"}
			}
			return v
		}
	}
	v := &yamlv3.Node{Kind: yamlv3.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, &yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}

// set takes a mapping node, a key and a value and sets the key to that string value, replacing what was there or appending it
func set(m *yamlv3.Node, key, value string) {
	// going over the mapping's key and value pairs
	for i := 0; i+1 < len(m.Content); i += 2 {
		// checking if this is the key, if so its value is replaced
		if m.Content[i].Value == key {
			m.Content[i+1] = &yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: value}
			return
		}
	}
	m.Content = append(m.Content,
		&yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: key},
		&yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: value})
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
