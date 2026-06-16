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

import "testing"

func resolver(m map[string]Resolved) func(string) (Resolved, bool) {
	return func(p string) (Resolved, bool) { r, ok := m[p]; return r, ok }
}

func TestUpdateContent_MapTagField(t *testing.T) {
	in := `image:
  repository: ghcr.io/org/app
  tag: 1.2.0  # {"$image-policy": "app-stable"}
replicas: 2
`
	want := `image:
  repository: ghcr.io/org/app
  tag: 1.3.0  # {"$image-policy": "app-stable"}
replicas: 2
`
	got, updates, err := UpdateContent("values.yaml", []byte(in),
		resolver(map[string]Resolved{"app-stable": {Tag: "1.3.0", Image: "ghcr.io/org/app:1.3.0", Repository: "ghcr.io/org/app"}}))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if len(updates) != 1 || updates[0].NewValue != "1.3.0" || updates[0].Line != 3 {
		t.Errorf("unexpected updates: %+v", updates)
	}
}

func TestUpdateContent_FullImageStringInArray(t *testing.T) {
	in := `containers:
  - name: app
    image: ghcr.io/org/app:1.2.0  # {"$image-policy": "app-stable:image"}
  - name: side
    image: ghcr.io/org/side:0.1.0
`
	want := `containers:
  - name: app
    image: ghcr.io/org/app:1.3.0  # {"$image-policy": "app-stable:image"}
  - name: side
    image: ghcr.io/org/side:0.1.0
`
	got, updates, err := UpdateContent("deploy.yaml", []byte(in),
		resolver(map[string]Resolved{"app-stable": {Tag: "1.3.0", Image: "ghcr.io/org/app:1.3.0", Repository: "ghcr.io/org/app"}}))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
}

func TestUpdateContent_PreservesQuotes(t *testing.T) {
	in := "tag: \"1.2.0\"  # {\"$image-policy\": \"app\"}\n"
	want := "tag: \"1.3.0\"  # {\"$image-policy\": \"app\"}\n"
	got, _, err := UpdateContent("v.yaml", []byte(in),
		resolver(map[string]Resolved{"app": {Tag: "1.3.0"}}))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("quote not preserved:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestUpdateContent_NoChangeWhenSame(t *testing.T) {
	in := "tag: 1.3.0  # {\"$image-policy\": \"app\"}\n"
	got, updates, err := UpdateContent("v.yaml", []byte(in),
		resolver(map[string]Resolved{"app": {Tag: "1.3.0"}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 0 {
		t.Errorf("expected no updates, got %+v", updates)
	}
	if string(got) != in {
		t.Errorf("content changed unexpectedly")
	}
}

func TestUpdateContent_UnknownPolicyLeftAlone(t *testing.T) {
	in := "tag: 1.2.0  # {\"$image-policy\": \"missing\"}\n"
	got, updates, err := UpdateContent("v.yaml", []byte(in), resolver(map[string]Resolved{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 0 || string(got) != in {
		t.Errorf("unknown policy should be left unchanged")
	}
}

func TestUpdateContent_LinesWithoutMarkerUntouched(t *testing.T) {
	in := `tag: 1.2.0
other: 1.2.0  # just a comment
url: http://x:5000/y  # not a marker
`
	got, updates, err := UpdateContent("v.yaml", []byte(in),
		resolver(map[string]Resolved{"app": {Tag: "9.9.9"}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 0 || string(got) != in {
		t.Errorf("non-marked lines must be untouched; updates=%+v", updates)
	}
}

func TestParseMarker(t *testing.T) {
	cases := []struct {
		in         string
		wantPolicy string
		wantField  Field
		wantErr    bool
	}{
		{`{"$image-policy": "app"}`, "app", FieldTag, false},
		{`{"$image-policy": "app:tag"}`, "app", FieldTag, false},
		{`{"$image-policy": "app:image"}`, "app", FieldImage, false},
		{`{"$image-policy": "app:name"}`, "app", FieldName, false},
		{`{"$image-policy": "app:bogus"}`, "", "", true},
		{`{"other": "app"}`, "", "", true},
	}
	for _, c := range cases {
		m, err := ParseMarker(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseMarker(%q) expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMarker(%q) error: %v", c.in, err)
			continue
		}
		if m.Policy != c.wantPolicy || m.Field != c.wantField {
			t.Errorf("ParseMarker(%q) = %+v, want policy=%s field=%s", c.in, m, c.wantPolicy, c.wantField)
		}
	}
}
