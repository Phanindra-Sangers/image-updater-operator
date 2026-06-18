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

package policy

import (
	"testing"

	imagesv1alpha1 "github.com/saphire/image-updater-operator/api/v1alpha1"
)

func TestSelect(t *testing.T) {
	tags := []string{"1.0.0", "1.2.0", "1.2.5", "2.0.0", "latest", "v1.9", "v1.10", "nightly-20240101", "nightly-20240115"}

	tests := []struct {
		name string
		spec imagesv1alpha1.ImagePolicySpec
		want string
	}{
		{
			// "v1.10" parses as 1.10.0 (the "v" prefix is stripped), which is the
			// highest version below 2.0.0, above 1.2.5.
			name: "semver range picks highest within range, v-prefix aware",
			spec: imagesv1alpha1.ImagePolicySpec{
				Policy: imagesv1alpha1.Policy{Semver: &imagesv1alpha1.SemverPolicy{Range: ">=1.0.0 <2.0.0"}},
			},
			want: "v1.10",
		},
		{
			name: "semver tilde",
			spec: imagesv1alpha1.ImagePolicySpec{
				Policy: imagesv1alpha1.Policy{Semver: &imagesv1alpha1.SemverPolicy{Range: "~1.2.0"}},
			},
			want: "1.2.5",
		},
		{
			name: "regex numeric desc picks v1.10 over v1.9",
			spec: imagesv1alpha1.ImagePolicySpec{
				Policy: imagesv1alpha1.Policy{Regex: &imagesv1alpha1.RegexPolicy{
					Pattern: `^v(\d+\.\d+)$`, Numeric: true, Order: imagesv1alpha1.SortDescending,
				}},
			},
			want: "v1.10",
		},
		{
			name: "regex with filter on nightly builds, numeric desc",
			spec: imagesv1alpha1.ImagePolicySpec{
				FilterTags: &imagesv1alpha1.TagFilter{Pattern: `^nightly-`},
				Policy: imagesv1alpha1.Policy{Regex: &imagesv1alpha1.RegexPolicy{
					Pattern: `^nightly-(\d+)$`, Extract: "$1", Numeric: true, Order: imagesv1alpha1.SortDescending,
				}},
			},
			want: "nightly-20240115",
		},
		{
			name: "alphabetical asc",
			spec: imagesv1alpha1.ImagePolicySpec{
				FilterTags: &imagesv1alpha1.TagFilter{Pattern: `^nightly-`},
				Policy:     imagesv1alpha1.Policy{Alphabetical: &imagesv1alpha1.OrderedPolicy{Order: imagesv1alpha1.SortAscending}},
			},
			want: "nightly-20240101",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Select(tags, tc.spec)
			if err != nil {
				t.Fatalf("Select() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("Select() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSelectNoMatch(t *testing.T) {
	got, err := Select([]string{"alpha", "beta"}, imagesv1alpha1.ImagePolicySpec{
		Policy: imagesv1alpha1.Policy{Semver: &imagesv1alpha1.SemverPolicy{Range: ">=1.0.0"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty selection, got %q", got)
	}
}

func TestSelectInvalidRegex(t *testing.T) {
	_, err := Select([]string{"1.0"}, imagesv1alpha1.ImagePolicySpec{
		Policy: imagesv1alpha1.Policy{Regex: &imagesv1alpha1.RegexPolicy{Pattern: "("}},
	})
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
}

func TestNaturalCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"9", "10", -1},
		{"10", "9", 1},
		{"v1.9", "v1.10", -1},
		{"1.0.0", "1.0.0", 0},
		{"abc", "abd", -1},
	}
	for _, c := range cases {
		if got := naturalCompare(c.a, c.b); got != c.want {
			t.Errorf("naturalCompare(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
