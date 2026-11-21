// collects manifests from a directory instead of a cluster, for export --from-dir
// it reads every yaml and json file in the tree, skipping what isnt a manifest and refusing a Kustomization that would build something else

package collect

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"setdrift.io/setdrift-engine/pkg/resource"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// FileSource reads the manifests in a directory, a value type with no client and no state
type FileSource struct {
	// the directory to read
	Dir string
}

// manifestPaths lists the manifest files under f.Dir, refusing a Kustomization that changes the files it lists
func (f FileSource) manifestPaths() ([]string, error) {
	var paths []string
	// walking the tree, the func below decides for each entry
	err := filepath.WalkDir(f.Dir, func(path string, d fs.DirEntry, err error) error {

		if err != nil {
			return err
		}
		// checking if its a symlink, if so its not followed since it could lead out of the directory read
		// WalkDir follows none to directories, this covers files
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		// checking if its a dot-directory like .git or .github, if so its skipped so Dir can be a repository checkout
		if d.IsDir() && path != f.Dir && strings.HasPrefix(d.Name(), ".") {
			return fs.SkipDir
		}
		// checking if its one of an export's own records beside its clusters/, the snapshots and verify reports, which arent manifests
		if d.IsDir() && exportRecords[d.Name()] {
			// checking if theres a clusters/ beside it, if so its an export's and its skipped
			if fi, err := os.Stat(filepath.Join(filepath.Dir(path), "clusters")); err == nil && fi.IsDir() {
				return fs.SkipDir
			}
		}
		// checking if its a Kustomization file, a Kustomization that only lists files in the tree builds exactly those files, so its skipped
		// one that changes them (patches, a namespace, images, generators) or pulls in files from elsewhere is refused
		// read as manifests, a patch and its base would be the same object twice, and the export would keep the base's values
		if !d.IsDir() && kustomizationFiles[d.Name()] {
			// invoking kustomization to tell if it builds anything other than its files, its defined below
			reason, err := f.kustomization(path)
			if err != nil {
				return err
			}
			// checking if it gave a reason, if so the directory is refused with a KustomizeError
			if reason != "" {
				return &KustomizeError{Dir: filepath.Dir(path), Reason: reason}
			}
			return nil
		}
		// checking the extension, yaml and json files are manifests
		switch filepath.Ext(path) {
		case ".yaml", ".yml", ".json":
			// checking its not a directory named like foo.yaml
			if !d.IsDir() {
				paths = append(paths, path)
			}
		}
		return nil
	})
	return paths, err
}

// Collect reads every manifest under f.Dir, it makes FileSource a Collector
// runtime state kinds are left out with an info finding, and Kustomization files are dropped since theyre not cluster objects
func (f FileSource) Collect(ctx context.Context) (*Result, error) {
	// invoking manifestPaths to find the files, its defined above
	paths, err := f.manifestPaths()
	if err != nil {
		return nil, err
	}
	// sorting the paths so the objects come out in the same order every run
	sort.Strings(paths)

	res := &Result{}
	skipped := map[string]int{}
	// reading each file in order
	for _, path := range paths {
		// checking if the context was cancelled, between files
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// invoking readFile to decode the file's objects, its defined below
		objs, err := readFile(path)

		// checking if the file couldnt be read, if so the first bad file is a terminal error with its path, unlike ClusterCollector which only warns
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		// sorting each object into kept, skipped or dropped
		for _, o := range objs {
			gk := o.GroupVersionKind().GroupKind()
			// checking which of those it is
			switch {
			case gk.Group == kustomizeGroup:
				// Kustomization and Component files describe how to build manifests, theyre not cluster objects
			case DefaultSkipKind(gk):
				// invoking DefaultSkipKind for the same runtime state types the cluster collector skips, its defined in cluster.go
				skipped[gk.Kind]++
			default:
				res.Objects = append(res.Objects, o)
			}
		}
	}
	kinds := make([]string, 0, len(skipped))
	// collecting the skipped kinds, to sort them for a stable order
	for k := range skipped {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	// adding one info finding per skipped kind
	for _, k := range kinds {
		// invoking resource.NewFinding to build it, its defined in pkg/resource/resource.go, and objectsWord for the wording, its defined below
		res.Findings = append(res.Findings, resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "RUNTIME_STATE_SKIPPED",
			"%d %s %s not exported: the cluster writes this kind itself", skipped[k], k, objectsWord(skipped[k])))
	}
	return res, nil
}

// kustomizeGroup is the api group of Kustomization and Component, which describe a build and arent cluster objects
const kustomizeGroup = "kustomize.config.k8s.io"

// kustomizationFiles are the file names kustomize reads a directory's Kustomization from
var kustomizationFiles = map[string]bool{"kustomization.yaml": true, "kustomization.yml": true, "Kustomization": true}

// exportRecords are the directories setdrift writes at an export's root beside clusters/, export's snapshots and verify's reports
var exportRecords = map[string]bool{"snapshots": true, "verify": true}

// KustomizeError means the directory to read holds a Kustomization that builds something other than its files, so it must be rendered before it can be exported
// Dir is the Kustomization's directory and Reason says what it changes or pulls in
type KustomizeError struct{ Dir, Reason string }

// Error builds the message, with the kubectl kustomize command that renders the directory
func (e *KustomizeError) Error() string {
	return fmt.Sprintf("%s is a Kustomize directory whose output differs from its files (%s); "+
		"render it into a directory of its own and export that, e.g. kubectl kustomize %s > rendered/all.yaml", e.Dir, e.Reason, e.Dir)
}

// kustomization says why the Kustomization at path builds something other than the files in the tree, or empty when it only lists them
func (f FileSource) kustomization(path string) (reason string, err error) {
	data, err := os.ReadFile(path) //#nosec G304 -- a Kustomization in the directory the user named
	if err != nil {
		return "", err
	}
	var k map[string]any
	// checking if it parses, one that doesnt is refused rather than guessed at
	if err := yaml.Unmarshal(data, &k); err != nil {
		return "it does not parse", nil
	}
	var other []string
	// collecting each top-level key beyond the ones that only list files
	for key := range k {
		// checking if its one of apiVersion, kind, metadata or resources, anything else changes the build
		switch key {
		case "apiVersion", "kind", "metadata", "resources":
		default:
			other = append(other, key)
		}
	}
	// checking if any other key is set, if so the reason names them
	if len(other) > 0 {
		sort.Strings(other)
		return "it sets " + strings.Join(other, ", "), nil
	}
	resources, _ := k["resources"].([]any)
	root, err := filepath.Abs(f.Dir)
	if err != nil {
		return "", err
	}
	// checking each listed resource stays inside the directory read
	for _, r := range resources {
		ref, _ := r.(string)
		// checking if its a remote reference, a url, a git ref or a github path
		if strings.Contains(ref, "://") || strings.Contains(ref, "?ref=") || strings.HasPrefix(ref, "github.com/") {
			return "it pulls in " + ref, nil
		}
		abs, err := filepath.Abs(filepath.Join(filepath.Dir(path), ref))
		// checking if the path cant be resolved, if so its refused as pulled in
		if err != nil {
			return "it pulls in " + ref, nil
		}
		// checking if the path leads outside the directory read
		if rel, err := filepath.Rel(root, abs); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "it pulls in " + ref + ", outside the directory read", nil
		}
	}
	return "", nil
}

// objectsWord gives back "object was" or "objects were", for a count
func objectsWord(n int) string {
	// checking if its exactly one, if so its singular
	if n == 1 {
		return "object was"
	}
	return "objects were"
}

// maxManifestFile caps the size of one manifest file, far above any real one (kubernetes caps an object at about 1.5 MiB), low enough that a stray huge file cant exhaust memory
const maxManifestFile = 64 << 20

// readFile reads the objects in a yaml or json file, each document as an object or a List of them
func readFile(path string) ([]*resource.Object, error) {
	file, err := os.Open(path) //#nosec G304 -- a file in the directory the user named with --from-dir
	if err != nil {
		return nil, err
	}
	defer file.Close()
	fi, err := file.Stat()
	if err != nil {
		return nil, err
	}
	// checking if the file is over the cap, if so its a terminal error before reading it
	if fi.Size() > maxManifestFile {
		return nil, fmt.Errorf("%d MiB, more than a manifest file can be (%d MiB)", fi.Size()>>20, maxManifestFile>>20)
	}
	// invoking decodeManifests on the file, limited in case it grows while being read, its defined below
	return decodeManifests(io.LimitReader(file, maxManifestFile))
}

// decodeManifests decodes the objects in yaml or json documents, separated by ---
func decodeManifests(r io.Reader) ([]*resource.Object, error) {
	var out []*resource.Object
	// the yaml reader splits a multi document file on the --- delimiter
	reader := utilyaml.NewYAMLReader(bufio.NewReader(r))
	// reading one document at a time, n counts them for the error messages
	for n := 1; ; n++ {
		doc, err := reader.Read()
		// checking if the documents ran out, if so were done
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		// checking for any other error, the document number goes with it
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", n, err)
		}
		// checking if the document is blank or only tabs and spaces, if so its skipped
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		data, err := yaml.YAMLToJSON(doc)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", n, err)
		}

		// checking if the document came out null, documents holding only comments, null or ~ do, if so its skipped
		if string(bytes.TrimSpace(data)) == "null" {
			continue
		}
		// decoding with the unstructured scheme, which keeps numbers as int64 and requires apiVersion and kind
		obj, _, err := unstructured.UnstructuredJSONScheme.Decode(data, nil, nil)
		// checking if it failed, if so decodeError says why without the content, its defined below
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", n, decodeError(err))
		}
		// checking what came out, an object is kept, a List gives one object per item, anything else is a terminal error
		switch o := obj.(type) {
		case *unstructured.Unstructured:
			// checking if the apiVersion is missing, the decoder accepts a kind with no apiVersion
			if o.GetAPIVersion() == "" {
				return nil, fmt.Errorf("document %d: object has no apiVersion", n)
			}
			out = append(out, resource.New(o))
		case *unstructured.UnstructuredList:
			// going through each item of the List
			err := o.EachListItem(func(item runtime.Object) error {
				u, ok := item.(*unstructured.Unstructured)
				// checking if the item isnt Unstructured, the decoder always gives that so anything else is a terminal error
				if !ok {
					return fmt.Errorf("unexpected list item type %T", item)
				}
				// checking if the item has no apiVersion, same as for a single object
				if u.GetAPIVersion() == "" {
					return errors.New("list item has no apiVersion")
				}
				out = append(out, resource.New(u))
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("document %d: %w", n, err)
			}
		default:
			return nil, fmt.Errorf("document %d: unexpected object type %T", n, obj)
		}
	}
}

// decodeError describes a decode error without the document, since the content may hold secrets
func decodeError(err error) error {
	// checking which part was missing, kind or apiVersion, anything else is not a kubernetes object
	switch {
	case runtime.IsMissingKind(err):
		return errors.New("object has no kind")
	case runtime.IsMissingVersion(err):
		return errors.New("object has no apiVersion")
	default:
		return errors.New("not a Kubernetes object (content left out of this error, as it may hold secrets)")
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
