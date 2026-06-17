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

package gitwriteback

import (
	"fmt"
	"os"
	"path/filepath"
)

// Edit fully describes one image write-back: which target to touch and the
// repository/tag to write into it.
type Edit struct {
	Target Target
	// Repository and Tag are the selected image, e.g. ghcr.io/org/app and 1.4.0.
	Repository string
	Tag        string
	// Container scopes a manifest edit to a single container by name; empty
	// means every container referencing Repository.
	Container string
	// HelmNameKey and HelmTagKey are the dotted values keys for a helm target.
	HelmNameKey string
	HelmTagKey  string
}

// Apply edits the file backing the target under root. It returns the
// repo-relative path that changed (for staging) and whether anything changed.
func Apply(root string, e Edit) (relPath string, changed bool, err error) {
	file, err := targetFile(root, e.Target)
	if err != nil {
		return "", false, err
	}
	content, err := os.ReadFile(file)
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", file, err)
	}

	var out []byte
	switch e.Target.Kind {
	case TargetHelm:
		out, changed, err = EditHelmValues(content, e.Repository, e.Tag, e.HelmNameKey, e.HelmTagKey)
	case TargetKustomize:
		out, changed, err = EditKustomization(content, e.Repository, e.Tag)
	case TargetManifest:
		out, changed, err = EditManifest(content, e.Repository, e.Tag, e.Container)
	default:
		return "", false, fmt.Errorf("unknown target kind %q", e.Target.Kind)
	}
	if err != nil {
		return "", false, err
	}
	if !changed {
		return "", false, nil
	}
	if err := os.WriteFile(file, out, fileMode(file)); err != nil {
		return "", false, fmt.Errorf("writing %s: %w", file, err)
	}
	rel, err := filepath.Rel(root, file)
	if err != nil {
		return "", false, err
	}
	return rel, true, nil
}

// targetFile resolves the absolute file to edit for a target. For kustomize the
// path is a directory holding kustomization.yaml (or .yml); otherwise it is the
// file itself. The result is constrained to root.
func targetFile(root string, t Target) (string, error) {
	clean := filepath.Clean(filepath.Join(root, t.Path))
	if rel, err := filepath.Rel(root, clean); err != nil || rel == ".." || filepath.IsAbs(rel) ||
		(len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("target path %q escapes the repository", t.Path)
	}
	if t.Kind != TargetKustomize {
		return clean, nil
	}
	for _, name := range []string{"kustomization.yaml", "kustomization.yml"} {
		candidate := filepath.Join(clean, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no kustomization.yaml in %s", t.Path)
}

func fileMode(path string) os.FileMode {
	if info, err := os.Stat(path); err == nil {
		return info.Mode().Perm()
	}
	return 0o644
}
