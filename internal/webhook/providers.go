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

package webhook

import (
	"encoding/json"
	"fmt"
	"strings"
)

// parseProvider extracts the affected repository references (without tags) from
// a registry push payload for the named provider. Supported providers:
// "dockerhub", "harbor", and "generic".
func parseProvider(provider string, body []byte) ([]string, error) {
	switch provider {
	case "dockerhub":
		return parseDockerHub(body)
	case "harbor":
		return parseHarbor(body)
	case "generic":
		return parseGeneric(body)
	default:
		return nil, fmt.Errorf("unknown provider %q", provider)
	}
}

// Docker Hub webhook: {"repository":{"repo_name":"org/app"}}.
func parseDockerHub(body []byte) ([]string, error) {
	var p struct {
		Repository struct {
			RepoName string `json:"repo_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	if p.Repository.RepoName == "" {
		return nil, fmt.Errorf("no repository.repo_name in payload")
	}
	return []string{p.Repository.RepoName}, nil
}

// Harbor webhook: event_data.resources[].resource_url is "host/project/repo:tag".
func parseHarbor(body []byte) ([]string, error) {
	var p struct {
		EventData struct {
			Resources []struct {
				ResourceURL string `json:"resource_url"`
			} `json:"resources"`
			Repository struct {
				RepoFullName string `json:"repo_full_name"`
			} `json:"repository"`
		} `json:"event_data"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	var repos []string
	for _, r := range p.EventData.Resources {
		if r.ResourceURL != "" {
			repos = append(repos, stripTag(r.ResourceURL))
		}
	}
	if len(repos) == 0 && p.EventData.Repository.RepoFullName != "" {
		repos = append(repos, p.EventData.Repository.RepoFullName)
	}
	if len(repos) == 0 {
		return nil, fmt.Errorf("no repository in payload")
	}
	return repos, nil
}

// Generic webhook: {"repository":"host/repo"} or {"image":"host/repo:tag"}.
func parseGeneric(body []byte) ([]string, error) {
	var p struct {
		Repository string `json:"repository"`
		Image      string `json:"image"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	switch {
	case p.Repository != "":
		return []string{stripTag(p.Repository)}, nil
	case p.Image != "":
		return []string{stripTag(p.Image)}, nil
	default:
		return nil, fmt.Errorf("payload must set \"repository\" or \"image\"")
	}
}

// stripTag removes a trailing ":tag" (but not a registry ":port") from a
// reference. It keeps everything up to the last "/" segment's colon.
func stripTag(ref string) string {
	slash := strings.LastIndex(ref, "/")
	lastSeg := ref[slash+1:]
	if i := strings.LastIndex(lastSeg, ":"); i >= 0 {
		return ref[:slash+1] + lastSeg[:i]
	}
	return ref
}
