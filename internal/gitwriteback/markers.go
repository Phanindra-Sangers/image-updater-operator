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

// Package gitwriteback updates image references in YAML files in a Git
// repository, located by marker comments, and commits the result. Editing is
// line-based and surgical so commits change only the marked value and produce
// clean diffs, regardless of whether the value lives in a map, an array, or a
// full image string.
package gitwriteback

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// MarkerKey is the JSON key carried in a marker comment.
const MarkerKey = "$image-policy"

// Field identifies which part of the image a marked scalar holds.
type Field string

const (
	// FieldTag (default) replaces the scalar with the selected tag, e.g. "1.2.3".
	FieldTag Field = "tag"
	// FieldImage replaces the scalar with the full "repository:tag".
	FieldImage Field = "image"
	// FieldName replaces the scalar with the repository (no tag).
	FieldName Field = "name"
)

// Marker is a parsed marker comment.
type Marker struct {
	Policy string
	Field  Field
}

// Resolved holds the values an ImagePolicy makes available for substitution.
type Resolved struct {
	Tag        string
	Image      string // repository:tag
	Repository string
}

// Value returns the substitution for the marker's field, and whether it is set.
func (r Resolved) Value(f Field) (string, bool) {
	switch f {
	case FieldImage:
		return r.Image, r.Image != ""
	case FieldName:
		return r.Repository, r.Repository != ""
	default:
		return r.Tag, r.Tag != ""
	}
}

// Update records a single line edit.
type Update struct {
	File     string
	Line     int // 1-based
	Policy   string
	Field    Field
	OldValue string
	NewValue string
}

// String renders an update for commit messages and status.
func (u Update) String() string {
	return fmt.Sprintf("%s:%d %s -> %s", u.File, u.Line, u.Policy, u.NewValue)
}

// markerRe captures the code portion of a line and its trailing marker comment.
// The comment must be a JSON object mentioning MarkerKey.
var markerRe = regexp.MustCompile(`^(?P<code>.*\S)(?P<tail>\s+#\s*(?P<json>\{[^}]*` + regexp.QuoteMeta(MarkerKey) + `[^}]*\})\s*)$`)

// valueRe splits the code portion into a "key: " (or "- ") prefix and the value.
var valueRe = regexp.MustCompile(`^(?P<prefix>\s*(?:-\s+)?(?:[^:\s][^:]*:\s+|))(?P<value>.+)$`)

// ParseMarker extracts a Marker from a marker comment's JSON object.
func ParseMarker(jsonObj string) (Marker, error) {
	var m map[string]string
	if err := json.Unmarshal([]byte(jsonObj), &m); err != nil {
		return Marker{}, fmt.Errorf("invalid marker %q: %w", jsonObj, err)
	}
	raw, ok := m[MarkerKey]
	if !ok || raw == "" {
		return Marker{}, fmt.Errorf("marker %q missing %q", jsonObj, MarkerKey)
	}
	// Format: "<policy>" or "<policy>:<field>". Policy names are DNS labels and
	// contain no colon, so a single colon split is safe.
	policy, field := raw, string(FieldTag)
	if i := strings.LastIndex(raw, ":"); i >= 0 {
		policy, field = raw[:i], raw[i+1:]
	}
	if policy == "" {
		return Marker{}, fmt.Errorf("marker %q has empty policy", jsonObj)
	}
	switch Field(field) {
	case FieldTag, FieldImage, FieldName:
	default:
		return Marker{}, fmt.Errorf("marker %q has unknown field %q", jsonObj, field)
	}
	return Marker{Policy: policy, Field: Field(field)}, nil
}

// UpdateContent rewrites marked lines in content using resolve to look up the
// value for each marker's policy. It returns the new content and the list of
// edits made. Lines whose policy does not resolve are left unchanged.
func UpdateContent(file string, content []byte, resolve func(policy string) (Resolved, bool)) ([]byte, []Update, error) {
	// Preserve the original line endings by splitting on "\n" and rejoining.
	lines := strings.Split(string(content), "\n")
	var updates []Update

	for i, line := range lines {
		mm := markerRe.FindStringSubmatch(line)
		if mm == nil {
			continue
		}
		code := mm[markerRe.SubexpIndex("code")]
		tail := mm[markerRe.SubexpIndex("tail")]
		jsonObj := mm[markerRe.SubexpIndex("json")]

		marker, err := ParseMarker(jsonObj)
		if err != nil {
			return nil, nil, fmt.Errorf("%s:%d: %w", file, i+1, err)
		}

		resolved, ok := resolve(marker.Policy)
		if !ok {
			continue // policy unknown or not scanned yet
		}
		newValue, ok := resolved.Value(marker.Field)
		if !ok {
			continue
		}

		vm := valueRe.FindStringSubmatch(code)
		if vm == nil {
			return nil, nil, fmt.Errorf("%s:%d: cannot locate value in %q", file, i+1, code)
		}
		prefix := vm[valueRe.SubexpIndex("prefix")]
		oldValue := vm[valueRe.SubexpIndex("value")]

		quoted := requote(oldValue, newValue)
		if quoted == oldValue {
			continue // already up to date
		}

		lines[i] = prefix + quoted + tail
		updates = append(updates, Update{
			File: file, Line: i + 1, Policy: marker.Policy, Field: marker.Field,
			OldValue: strings.Trim(oldValue, `"'`), NewValue: newValue,
		})
	}

	if len(updates) == 0 {
		return content, nil, nil
	}
	return []byte(strings.Join(lines, "\n")), updates, nil
}

// requote applies newValue using the same quoting style as oldValue.
func requote(oldValue, newValue string) string {
	switch {
	case strings.HasPrefix(oldValue, `"`) && strings.HasSuffix(oldValue, `"`):
		return `"` + newValue + `"`
	case strings.HasPrefix(oldValue, `'`) && strings.HasSuffix(oldValue, `'`):
		return `'` + newValue + `'`
	default:
		return newValue
	}
}
