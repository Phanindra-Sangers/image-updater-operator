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
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ScanResult is the outcome of scanning a tree for marker updates.
type ScanResult struct {
	// ChangedFiles are repo-relative paths whose contents were rewritten.
	ChangedFiles []string
	// Updates are the individual line edits made, across all files.
	Updates []Update
}

// ScanAndUpdate walks subPath under root for YAML files, applies marker-based
// updates, and writes changed files back in place. Returned ChangedFiles are
// relative to root (suitable for staging in the repo).
func ScanAndUpdate(root, subPath string, resolve func(policy string) (Resolved, bool)) (ScanResult, error) {
	var res ScanResult
	scanRoot := filepath.Join(root, filepath.Clean(subPath))

	err := filepath.WalkDir(scanRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !isYAML(d.Name()) {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		newContent, updates, err := UpdateContent(rel, content, resolve)
		if err != nil {
			return err
		}
		if len(updates) == 0 {
			return nil
		}
		if err := os.WriteFile(path, newContent, fileMode(d)); err != nil {
			return err
		}
		res.ChangedFiles = append(res.ChangedFiles, rel)
		res.Updates = append(res.Updates, updates...)
		return nil
	})
	if err != nil {
		return ScanResult{}, err
	}
	return res, nil
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}

func fileMode(d fs.DirEntry) os.FileMode {
	if info, err := d.Info(); err == nil {
		return info.Mode().Perm()
	}
	return 0o644
}
