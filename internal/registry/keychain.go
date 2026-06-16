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

package registry

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
)

// dockerConfigJSON is the on-disk shape of a kubernetes.io/dockerconfigjson Secret.
type dockerConfigJSON struct {
	Auths map[string]dockerAuthEntry `json:"auths"`
}

type dockerAuthEntry struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	Auth          string `json:"auth,omitempty"`
	IdentityToken string `json:"identitytoken,omitempty"`
}

// secretKeychain resolves credentials from a parsed dockerconfigjson, matching
// the target registry against the configured registry entries.
type secretKeychain struct {
	auths map[string]authn.AuthConfig
}

// KeychainFromDockerConfig parses the contents of a dockerconfigjson Secret
// (the value of the .dockerconfigjson data key) into an authn.Keychain.
func KeychainFromDockerConfig(raw []byte) (authn.Keychain, error) {
	var cfg dockerConfigJSON
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing dockerconfigjson: %w", err)
	}

	auths := make(map[string]authn.AuthConfig, len(cfg.Auths))
	for registry, entry := range cfg.Auths {
		ac := authn.AuthConfig{
			Username:      entry.Username,
			Password:      entry.Password,
			IdentityToken: entry.IdentityToken,
		}
		if entry.Auth != "" && (ac.Username == "" || ac.Password == "") {
			u, p, err := decodeAuth(entry.Auth)
			if err != nil {
				return nil, fmt.Errorf("decoding auth for %q: %w", registry, err)
			}
			ac.Username, ac.Password = u, p
		}
		auths[normalizeHost(registry)] = ac
	}
	return &secretKeychain{auths: auths}, nil
}

func (k *secretKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	host := normalizeHost(target.RegistryStr())
	if ac, ok := k.auths[host]; ok {
		return authn.FromConfig(ac), nil
	}
	// Docker Hub is referenced under several aliases; try them all.
	if isDockerHub(host) {
		for _, alias := range dockerHubAliases {
			if ac, ok := k.auths[alias]; ok {
				return authn.FromConfig(ac), nil
			}
		}
	}
	return authn.Anonymous, nil
}

var dockerHubAliases = []string{"index.docker.io", "registry-1.docker.io", "docker.io"}

func isDockerHub(host string) bool {
	for _, a := range dockerHubAliases {
		if host == a {
			return true
		}
	}
	return false
}

// normalizeHost strips the scheme and any trailing path (e.g. the legacy
// "https://index.docker.io/v1/" form) leaving just the registry host[:port].
func normalizeHost(registry string) string {
	r := registry
	if i := strings.Index(r, "://"); i >= 0 {
		r = r[i+3:]
	}
	if i := strings.Index(r, "/"); i >= 0 {
		r = r[:i]
	}
	return r
}

func decodeAuth(auth string) (string, string, error) {
	decoded, err := base64.StdEncoding.DecodeString(auth)
	if err != nil {
		return "", "", err
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("malformed auth string")
	}
	return parts[0], parts[1], nil
}
