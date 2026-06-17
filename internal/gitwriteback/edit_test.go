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
	"strings"
	"testing"
)

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in       string
		wantKind TargetKind
		wantPath string
		wantErr  bool
	}{
		{"helm:env/prod/values.yaml", TargetHelm, "env/prod/values.yaml", false},
		{"kustomize:overlays/prod", TargetKustomize, "overlays/prod", false},
		{"manifest:apps/web.yaml", TargetManifest, "apps/web.yaml", false},
		{"helm:", "", "", true},
		{"bogus:x", "", "", true},
		{"nopath", "", "", true},
	}
	for _, c := range cases {
		got, err := ParseTarget(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseTarget(%q) expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTarget(%q) error: %v", c.in, err)
			continue
		}
		if got.Kind != c.wantKind || got.Path != c.wantPath {
			t.Errorf("ParseTarget(%q) = %+v, want kind=%s path=%s", c.in, got, c.wantKind, c.wantPath)
		}
	}
}

func TestEditHelmValues(t *testing.T) {
	in := "image:\n  repository: ghcr.io/org/app\n  tag: 1.0.0\nreplicas: 2\n"
	out, changed, err := EditHelmValues([]byte(in), "ghcr.io/org/app", "1.4.0", "image.repository", "image.tag")
	if err != nil || !changed {
		t.Fatalf("EditHelmValues changed=%v err=%v", changed, err)
	}
	s := string(out)
	if !strings.Contains(s, "tag: 1.4.0") {
		t.Errorf("tag not updated:\n%s", s)
	}
	if !strings.Contains(s, "replicas: 2") {
		t.Errorf("unrelated content lost:\n%s", s)
	}
}

func TestEditHelmValues_TagOnlyNoChange(t *testing.T) {
	in := "image:\n  tag: 1.4.0\n"
	_, changed, err := EditHelmValues([]byte(in), "ghcr.io/org/app", "1.4.0", "", "image.tag")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Errorf("expected no change when tag already current")
	}
}

func TestEditHelmValues_ArrayIndex(t *testing.T) {
	in := "images:\n- name: app\n  repository: ghcr.io/org/app\n  tag: 1.0.0\n- name: proxy\n  repository: ghcr.io/org/proxy\n  tag: 9.9.9\n"
	// Target the first list element's tag via a numeric path segment.
	out, changed, err := EditHelmValues([]byte(in), "ghcr.io/org/app", "1.4.0", "images.0.repository", "images.0.tag")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	s := string(out)
	if !strings.Contains(s, "tag: 1.4.0") {
		t.Errorf("first element tag not updated:\n%s", s)
	}
	if !strings.Contains(s, "tag: 9.9.9") {
		t.Errorf("second element tag should be untouched:\n%s", s)
	}
}

func TestEditHelmValues_IndexOutOfRange(t *testing.T) {
	in := "images:\n- tag: 1.0.0\n"
	if _, _, err := EditHelmValues([]byte(in), "x", "1.4.0", "", "images.5.tag"); err == nil {
		t.Errorf("expected out-of-range error")
	}
}

func TestEditKustomization_UpsertExisting(t *testing.T) {
	in := "images:\n- name: ghcr.io/org/app\n  newTag: 1.0.0\n"
	out, changed, err := EditKustomization([]byte(in), "ghcr.io/org/app", "1.4.0")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if !strings.Contains(string(out), "newTag: 1.4.0") {
		t.Errorf("newTag not updated:\n%s", out)
	}
}

func TestEditKustomization_AddsEntry(t *testing.T) {
	in := "resources:\n- deployment.yaml\n"
	out, changed, err := EditKustomization([]byte(in), "ghcr.io/org/app", "1.4.0")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	s := string(out)
	if !strings.Contains(s, "name: ghcr.io/org/app") || !strings.Contains(s, "newTag: 1.4.0") {
		t.Errorf("images entry not added:\n%s", s)
	}
}

func TestEditManifest(t *testing.T) {
	in := "apiVersion: apps/v1\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n      - name: app\n        image: ghcr.io/org/app:1.0.0\n      - name: proxy\n        image: ghcr.io/org/proxy:2.0.0\n"
	out, changed, err := EditManifest([]byte(in), "ghcr.io/org/app", "1.4.0", "app")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	s := string(out)
	if !strings.Contains(s, "ghcr.io/org/app:1.4.0") {
		t.Errorf("app image not updated:\n%s", s)
	}
	if !strings.Contains(s, "ghcr.io/org/proxy:2.0.0") {
		t.Errorf("proxy image should be untouched:\n%s", s)
	}
}

func TestRepoOf(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/org/app:1.0.0":        "ghcr.io/org/app",
		"ghcr.io/org/app":              "ghcr.io/org/app",
		"localhost:5000/app:1.0.0":     "localhost:5000/app",
		"nginx:1.25":                   "nginx",
		"ghcr.io/org/app@sha256:abcde": "ghcr.io/org/app",
	}
	for in, want := range cases {
		if got := repoOf(in); got != want {
			t.Errorf("repoOf(%q) = %q, want %q", in, got, want)
		}
	}
}
