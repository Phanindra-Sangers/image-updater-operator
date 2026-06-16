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

// Package registry lists tags from any Docker Registry v2 compatible registry
// (Docker Hub, Nexus, Harbor, GHCR, ECR, ...) using go-containerregistry and
// assembles credentials from a dockerconfigjson Secret plus the ambient default
// keychain.
package registry

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// BuildKeychain returns a keychain for talking to a registry. When dockerConfig
// is non-nil it is parsed as the contents of a dockerconfigjson Secret and
// preferred over the default keychain. The default keychain reads the ambient
// docker config (which is how ECR/GCR/ACR credential helpers and IRSA surface).
func BuildKeychain(dockerConfig []byte) (authn.Keychain, error) {
	if len(dockerConfig) == 0 {
		return authn.DefaultKeychain, nil
	}
	kc, err := KeychainFromDockerConfig(dockerConfig)
	if err != nil {
		return nil, err
	}
	return authn.NewMultiKeychain(kc, authn.DefaultKeychain), nil
}

// ListTags returns every tag available for the given repository (no tag part),
// e.g. "docker.io/library/nginx" or "ghcr.io/org/app".
func ListTags(ctx context.Context, repository string, kc authn.Keychain, insecure bool) ([]string, error) {
	var nameOpts []name.Option
	if insecure {
		nameOpts = append(nameOpts, name.Insecure)
	}
	repo, err := name.NewRepository(repository, nameOpts...)
	if err != nil {
		return nil, fmt.Errorf("parsing repository %q: %w", repository, err)
	}

	tags, err := remote.List(repo,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(kc),
	)
	if err != nil {
		return nil, fmt.Errorf("listing tags for %q: %w", repository, err)
	}
	return tags, nil
}

// SplitImage parses a container image reference into its repository (registry
// plus path, normalized) and its tag. A digest reference yields an empty tag.
func SplitImage(image string) (repository, tag string, err error) {
	ref, err := name.ParseReference(image)
	if err != nil {
		return "", "", fmt.Errorf("parsing image %q: %w", image, err)
	}
	repository = ref.Context().Name()
	if t, ok := ref.(name.Tag); ok {
		tag = t.TagStr()
	}
	return repository, tag, nil
}

// JoinImage builds a "repository:tag" reference.
func JoinImage(repository, tag string) string {
	return fmt.Sprintf("%s:%s", repository, tag)
}

// CanonicalRepository normalizes a repository string to its fully-qualified
// form (expanding the default registry and library namespace) so values from
// different sources can be compared, e.g. "nginx" and "docker.io/library/nginx"
// both canonicalize to "index.docker.io/library/nginx".
func CanonicalRepository(repository string) (string, error) {
	repo, err := name.NewRepository(repository)
	if err != nil {
		return "", fmt.Errorf("parsing repository %q: %w", repository, err)
	}
	return repo.Name(), nil
}
