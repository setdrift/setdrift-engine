// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Collecting manifests from a directory, not a cluster.

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

// FileSource reads the manifests in a directory: a value type with no client and no state.
type FileSource struct {
	Dir string
}

// Collect implements Collector.
func (f FileSource) Collect(ctx context.Context) (*Result, error) {
	var paths []string
	// standard tree traversal
	err := filepath.WalkDir(f.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Skip .git, .github and other dot-directories, so Dir can be a
		// repository checkout.
		if d.IsDir() && path != f.Dir && strings.HasPrefix(d.Name(), ".") {
			return fs.SkipDir
		}
		// A Kustomization that only lists files in the tree builds exactly those files, so it is skipped. One that
		// changes them (patches, a namespace, images, generators) or pulls in files from elsewhere is refused:
		// read as manifests, a patch and its base would be the same object twice, and the export would keep the
		// base's values.
		if !d.IsDir() && kustomizationFiles[d.Name()] {
			if reason := f.kustomization(path); reason != "" {
				return &KustomizeError{Dir: filepath.Dir(path), Reason: reason}
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".yaml", ".yml", ".json":
			// guardrail against a directory called like foo.yaml
			if !d.IsDir() {
				paths = append(paths, path)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// standardizes object order
	sort.Strings(paths)

	res := &Result{}
	skipped := map[string]int{}
	for _, path := range paths {
		// checks ctx.Err() between files
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		objs, err := readFile(path)

		// first bad file fails loudly for directory collection (ClusterCollector just gives a warning), includes the path with the error
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for _, o := range objs {
			gk := o.GroupVersionKind().GroupKind()
			switch {
			case gk.Group == kustomizeGroup:
				// Kustomization and Component files describe how to build
				// manifests; they are not cluster objects.
			case DefaultSkipKinds[gk]:
				// The same runtime-state types the cluster collector skips.
				skipped[gk.Kind]++
			default:
				res.Objects = append(res.Objects, o)
			}
		}
	}
	kinds := make([]string, 0, len(skipped))
	for k := range skipped {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		res.Findings = append(res.Findings, resource.NewFinding(snapshotv1alpha1.Severity_SEVERITY_INFO, "RUNTIME_STATE_SKIPPED",
			"%d %s %s not exported: the cluster writes this kind itself", skipped[k], k, objectsWord(skipped[k])))
	}
	return res, nil
}

const kustomizeGroup = "kustomize.config.k8s.io"

// kustomizationFiles are the file names Kustomize reads a directory's Kustomization from
var kustomizationFiles = map[string]bool{"kustomization.yaml": true, "kustomization.yml": true, "Kustomization": true}

// KustomizeError means the directory to read holds a Kustomization that builds something other than its files,
// so it must be rendered before it can be exported.
type KustomizeError struct{ Dir, Reason string }

func (e *KustomizeError) Error() string {
	return fmt.Sprintf("%s is a Kustomize directory whose output differs from its files (%s); "+
		"render it into a directory of its own and export that, e.g. kubectl kustomize %s > rendered/all.yaml", e.Dir, e.Reason, e.Dir)
}

// kustomization says why the Kustomization at path builds something other than the files in the tree, or "" when
// it only lists them.
func (f FileSource) kustomization(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	var k map[string]any
	if err := yaml.Unmarshal(data, &k); err != nil {
		return "it does not parse"
	}
	var other []string
	for key := range k {
		switch key {
		case "apiVersion", "kind", "metadata", "resources":
		default:
			other = append(other, key)
		}
	}
	if len(other) > 0 {
		sort.Strings(other)
		return "it sets " + strings.Join(other, ", ")
	}
	resources, _ := k["resources"].([]any)
	root, _ := filepath.Abs(f.Dir)
	for _, r := range resources {
		ref, _ := r.(string)
		if strings.Contains(ref, "://") || strings.Contains(ref, "?ref=") || strings.HasPrefix(ref, "github.com/") {
			return "it pulls in " + ref
		}
		abs, err := filepath.Abs(filepath.Join(filepath.Dir(path), ref))
		if err != nil {
			return "it pulls in " + ref
		}
		if rel, err := filepath.Rel(root, abs); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "it pulls in " + ref + ", outside the directory read"
		}
	}
	return ""
}

// objectsWord is "object was" or "objects were", for a count
func objectsWord(n int) string {
	if n == 1 {
		return "object was"
	}
	return "objects were"
}

// four step process to convert YAML to resource.Object - Read YAML > Convert to JSON > Decode Schema > Unstructured / UnstructuredList
func readFile(path string) ([]*resource.Object, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var out []*resource.Object
	// splits multi document files, --- delimiter
	reader := utilyaml.NewYAMLReader(bufio.NewReader(file))
	for n := 1; ; n++ {
		// one document
		doc, err := reader.Read()
		//  break if eof
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		// return any error other than eof
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", n, err)
		}
		// skip blank documents or ones only containing tabs and spaces
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		data, err := yaml.YAMLToJSON(doc)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", n, err)
		}

		// documents only holding comments, null, or ~ evaluate to null
		if string(bytes.TrimSpace(data)) == "null" {
			continue
		}
		// keep numbers as int64, requires api version and kind
		obj, _, err := unstructured.UnstructuredJSONScheme.Decode(data, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", n, decodeError(err))
		}
		// returns a Unstructured Object, an UnstructuredList with one Object per Item, anything else as an Error
		switch o := obj.(type) {
		case *unstructured.Unstructured:
			// The decoder accepts a kind with no apiVersion.
			if o.GetAPIVersion() == "" {
				return nil, fmt.Errorf("document %d: object has no apiVersion", n)
			}
			out = append(out, resource.New(o))
		case *unstructured.UnstructuredList:
			err := o.EachListItem(func(item runtime.Object) error {
				u := item.(*unstructured.Unstructured)
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

// decodeError describes a decode error without the document
func decodeError(err error) error {
	switch {
	case runtime.IsMissingKind(err):
		return errors.New("object has no kind")
	case runtime.IsMissingVersion(err):
		return errors.New("object has no apiVersion")
	default:
		return errors.New("not a Kubernetes object (content left out of this error, as it may hold secrets)")
	}
}
