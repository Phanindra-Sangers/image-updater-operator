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

// Package policy evaluates an ImagePolicy against the set of tags returned by a
// registry and selects the winning tag. It supports semver, regex (with capture
// group extraction), numerical, and alphabetical selection.
package policy

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"

	imagesv1alpha1 "github.com/saphire/image-updater-operator/api/v1alpha1"
)

// Select returns the tag chosen from tags according to spec. The optional
// filterTags pattern is applied first. It returns an error if the policy is
// malformed (bad regex/semver range); it returns an empty string with no error
// when nothing matches.
func Select(tags []string, spec imagesv1alpha1.ImagePolicySpec) (string, error) {
	candidates := tags

	if spec.FilterTags != nil {
		re, err := regexp.Compile(spec.FilterTags.Pattern)
		if err != nil {
			return "", fmt.Errorf("invalid filterTags pattern %q: %w", spec.FilterTags.Pattern, err)
		}
		candidates = filter(candidates, re)
	}

	switch {
	case spec.Policy.Semver != nil:
		return selectSemver(candidates, spec.Policy.Semver.Range)
	case spec.Policy.Regex != nil:
		return selectRegex(candidates, spec.Policy.Regex)
	case spec.Policy.Numerical != nil:
		return selectOrdered(candidates, true, order(spec.Policy.Numerical.Order))
	case spec.Policy.Alphabetical != nil:
		return selectOrdered(candidates, false, order(spec.Policy.Alphabetical.Order))
	default:
		return "", fmt.Errorf("policy has no selection rule set")
	}
}

func order(o imagesv1alpha1.SortOrder) imagesv1alpha1.SortOrder {
	if o == imagesv1alpha1.SortAscending {
		return imagesv1alpha1.SortAscending
	}
	return imagesv1alpha1.SortDescending
}

func filter(tags []string, re *regexp.Regexp) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if re.MatchString(t) {
			out = append(out, t)
		}
	}
	return out
}

// selectSemver returns the highest tag satisfying the constraint range.
func selectSemver(tags []string, constraint string) (string, error) {
	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return "", fmt.Errorf("invalid semver range %q: %w", constraint, err)
	}

	var best *semver.Version
	bestTag := ""
	for _, t := range tags {
		v, err := semver.NewVersion(strings.TrimPrefix(t, "v"))
		if err != nil {
			continue // not a semver tag, skip
		}
		if !c.Check(v) {
			continue
		}
		if best == nil || v.GreaterThan(best) {
			best = v
			bestTag = t
		}
	}
	return bestTag, nil
}

// selectRegex filters tags by pattern, optionally rewrites them via the extract
// template for ordering purposes, sorts, and returns the original tag at the
// chosen end of the order.
func selectRegex(tags []string, p *imagesv1alpha1.RegexPolicy) (string, error) {
	re, err := regexp.Compile(p.Pattern)
	if err != nil {
		return "", fmt.Errorf("invalid regex pattern %q: %w", p.Pattern, err)
	}

	type pair struct {
		original string
		key      string
	}
	pairs := make([]pair, 0, len(tags))
	for _, t := range tags {
		m := re.FindStringSubmatchIndex(t)
		if m == nil {
			continue
		}
		key := t
		if p.Extract != "" {
			key = string(re.ExpandString(nil, p.Extract, t, m))
		}
		pairs = append(pairs, pair{original: t, key: key})
	}
	if len(pairs) == 0 {
		return "", nil
	}

	less := lexLess
	if p.Numeric {
		less = numericLess
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		return less(pairs[i].key, pairs[j].key)
	})

	// After ascending sort, desc picks the last element, asc picks the first.
	if order(p.Order) == imagesv1alpha1.SortAscending {
		return pairs[0].original, nil
	}
	return pairs[len(pairs)-1].original, nil
}

// selectOrdered sorts all tags (numerically or lexically) and returns the
// endpoint dictated by the order.
func selectOrdered(tags []string, numeric bool, o imagesv1alpha1.SortOrder) (string, error) {
	if len(tags) == 0 {
		return "", nil
	}
	cp := append([]string(nil), tags...)
	less := lexLess
	if numeric {
		less = numericLess
	}
	sort.SliceStable(cp, func(i, j int) bool { return less(cp[i], cp[j]) })

	if o == imagesv1alpha1.SortAscending {
		return cp[0], nil
	}
	return cp[len(cp)-1], nil
}

func lexLess(a, b string) bool { return a < b }

// numericLess compares two strings as sequences of numeric and non-numeric
// chunks so that "9" < "10" and "v1.9" < "v1.10".
func numericLess(a, b string) bool {
	return naturalCompare(a, b) < 0
}

var numChunk = regexp.MustCompile(`\d+|\D+`)

func naturalCompare(a, b string) int {
	ca := numChunk.FindAllString(a, -1)
	cb := numChunk.FindAllString(b, -1)
	for i := 0; i < len(ca) && i < len(cb); i++ {
		x, y := ca[i], cb[i]
		xNum := isDigits(x)
		yNum := isDigits(y)
		switch {
		case xNum && yNum:
			// Compare numerically, ignoring leading zeros, then by length.
			xt := strings.TrimLeft(x, "0")
			yt := strings.TrimLeft(y, "0")
			if len(xt) != len(yt) {
				if len(xt) < len(yt) {
					return -1
				}
				return 1
			}
			if xt != yt {
				if xt < yt {
					return -1
				}
				return 1
			}
		default:
			if x != y {
				if x < y {
					return -1
				}
				return 1
			}
		}
	}
	switch {
	case len(ca) < len(cb):
		return -1
	case len(ca) > len(cb):
		return 1
	default:
		return 0
	}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
