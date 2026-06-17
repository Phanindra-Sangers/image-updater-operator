/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package gitwriteback edits image references in a Git working tree so a GitOps
// controller can sync them. It supports three write-back targets, selected by a
// workload annotation: helm values files, kustomization images sections, and
// plain Kubernetes manifests. Edits are made through the YAML node tree, so
// unrelated content and comments are preserved.
package gitwriteback

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// TargetKind identifies the YAML structure a write-back edits.
type TargetKind string

const (
	// TargetHelm edits dotted keys in a Helm values file.
	TargetHelm TargetKind = "helm"
	// TargetKustomize upserts an entry in a kustomization images list.
	TargetKustomize TargetKind = "kustomize"
	// TargetManifest edits container image fields in a plain manifest.
	TargetManifest TargetKind = "manifest"
)

// Target is a parsed write-back-target annotation: a kind and a repo-relative
// path (a file for helm/manifest, a directory for kustomize).
type Target struct {
	Kind TargetKind
	Path string
}

// ParseTarget parses a "kind:path" annotation value, e.g. "helm:prod/values.yaml".
func ParseTarget(s string) (Target, error) {
	kind, path, ok := strings.Cut(s, ":")
	if !ok || path == "" {
		return Target{}, fmt.Errorf("invalid write-back-target %q, want \"kind:path\"", s)
	}
	switch TargetKind(kind) {
	case TargetHelm, TargetKustomize, TargetManifest:
		return Target{Kind: TargetKind(kind), Path: path}, nil
	default:
		return Target{}, fmt.Errorf("unknown write-back-target kind %q", kind)
	}
}

// EditHelmValues sets the repository and tag dotted keys in a Helm values file.
// nameKey may be empty to leave the repository untouched (tag-only update).
// It returns the rewritten content and whether anything changed.
func EditHelmValues(content []byte, repository, tag, nameKey, tagKey string) ([]byte, bool, error) {
	if tagKey == "" {
		return nil, false, fmt.Errorf("helm target requires a helm.image-tag key")
	}
	root, err := parseDoc(content)
	if err != nil {
		return nil, false, err
	}
	changed := false
	if nameKey != "" {
		c, err := setByPath(root, strings.Split(nameKey, "."), repository)
		if err != nil {
			return nil, false, err
		}
		changed = changed || c
	}
	c, err := setByPath(root, strings.Split(tagKey, "."), tag)
	if err != nil {
		return nil, false, err
	}
	changed = changed || c
	return marshalIfChanged(content, root, changed)
}

// EditKustomization upserts an entry in the kustomization "images" list whose
// name matches repository, setting its newTag to tag. A missing entry is added.
func EditKustomization(content []byte, repository, tag string) ([]byte, bool, error) {
	root, err := parseDoc(content)
	if err != nil {
		return nil, false, err
	}
	images := mappingValue(root, "images")
	if images == nil {
		images = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		setMappingValue(root, "images", images)
	}
	for _, item := range images.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		if name := mappingValue(item, "name"); name != nil && name.Value == repository {
			changed := upsertScalar(item, "newTag", tag)
			return marshalIfChanged(content, root, changed)
		}
	}
	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	upsertScalar(entry, "name", repository)
	upsertScalar(entry, "newTag", tag)
	images.Content = append(images.Content, entry)
	return marshalIfChanged(content, root, true)
}

// EditManifest sets every container image referencing repository to
// repository:tag, across all documents in the file. When container is
// non-empty, only the container with that name is updated.
func EditManifest(content []byte, repository, tag, container string) ([]byte, bool, error) {
	docs, err := parseDocs(content)
	if err != nil {
		return nil, false, err
	}
	desired := repository + ":" + tag
	changed := false
	for _, doc := range docs {
		walkContainers(doc, func(c *yaml.Node) {
			image := mappingValue(c, "image")
			if image == nil || image.Kind != yaml.ScalarNode {
				return
			}
			if repoOf(image.Value) != repository {
				return
			}
			if container != "" {
				name := mappingValue(c, "name")
				if name == nil || name.Value != container {
					return
				}
			}
			if image.Value != desired {
				image.Value = desired
				image.Tag = "!!str"
				changed = true
			}
		})
	}
	if !changed {
		return content, false, nil
	}
	out, err := marshalDocs(docs)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// repoOf returns the repository part of an "image" reference, dropping any tag
// or digest. It tolerates registry ports (host:5000/repo:tag).
func repoOf(image string) string {
	if at := strings.IndexByte(image, '@'); at >= 0 {
		image = image[:at]
	}
	slash := strings.LastIndexByte(image, '/')
	if colon := strings.LastIndexByte(image, ':'); colon > slash {
		return image[:colon]
	}
	return image
}

func parseDoc(content []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("parsing yaml: %w", err)
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil, fmt.Errorf("empty yaml document")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected a mapping at the document root")
	}
	return root, nil
}

func parseDocs(content []byte) ([]*yaml.Node, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(content)))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("parsing yaml: %w", err)
		}
		docs = append(docs, &doc)
	}
	return docs, nil
}

func marshalIfChanged(orig []byte, root *yaml.Node, changed bool) ([]byte, bool, error) {
	if !changed {
		return orig, false, nil
	}
	out, err := marshalDocs([]*yaml.Node{{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}})
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func marshalDocs(docs []*yaml.Node) ([]byte, error) {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	for _, doc := range docs {
		if err := enc.Encode(doc); err != nil {
			return nil, fmt.Errorf("encoding yaml: %w", err)
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// setByPath walks a dotted key path from the document root and sets the leaf
// scalar, returning whether the value changed. Segments address mapping keys;
// a numeric segment indexes into a sequence (e.g. images.0.tag,
// containers.1.image), letting array-form values be targeted. Missing
// intermediate mappings are created; sequence indices must already exist.
func setByPath(root *yaml.Node, path []string, value string) (bool, error) {
	if len(path) == 0 {
		return false, fmt.Errorf("empty key path")
	}
	node := root
	for i, seg := range path {
		last := i == len(path)-1
		switch node.Kind {
		case yaml.MappingNode:
			if last {
				return upsertScalar(node, seg, value), nil
			}
			next := mappingValue(node, seg)
			if next == nil {
				next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				setMappingValue(node, seg, next)
			}
			node = next
		case yaml.SequenceNode:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node.Content) {
				return false, fmt.Errorf("path index %q out of range for a %d-element list", seg, len(node.Content))
			}
			if last {
				return setScalar(node.Content[idx], value), nil
			}
			node = node.Content[idx]
		default:
			return false, fmt.Errorf("cannot descend into %q: not a mapping or list", seg)
		}
	}
	return false, fmt.Errorf("empty key path")
}

// setScalar sets a scalar node's string value, returning whether it changed.
func setScalar(node *yaml.Node, value string) bool {
	if node.Kind == yaml.ScalarNode && node.Value == value {
		return false
	}
	node.Kind = yaml.ScalarNode
	node.Tag = "!!str"
	node.Style = 0
	node.Value = value
	node.Content = nil
	return true
}

// mappingValue returns the value node for key in a mapping, or nil.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// setMappingValue sets key to val in a mapping, replacing or appending.
func setMappingValue(node *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content[i+1] = val
			return
		}
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
}

// upsertScalar sets key to a string scalar, returning whether it changed.
func upsertScalar(node *yaml.Node, key, value string) bool {
	if cur := mappingValue(node, key); cur != nil {
		if cur.Value == value {
			return false
		}
		cur.Value = value
		cur.Tag = "!!str"
		cur.Style = 0
		return true
	}
	setMappingValue(node, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
	return true
}

// walkContainers invokes fn for every node that looks like a container, that is
// a mapping carrying both "name" and "image" keys, anywhere in the tree.
func walkContainers(node *yaml.Node, fn func(*yaml.Node)) {
	if node == nil {
		return
	}
	if node.Kind == yaml.MappingNode {
		if mappingValue(node, "image") != nil {
			fn(node)
		}
	}
	for _, child := range node.Content {
		walkContainers(child, fn)
	}
}
