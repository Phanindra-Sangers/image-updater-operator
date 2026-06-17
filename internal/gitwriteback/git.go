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
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
)

// Author is the identity recorded on commits.
type Author struct {
	Name  string
	Email string
}

// HTTPSAuth builds token/password authentication for an https remote. For
// GitHub/GitLab tokens, pass the token as password; username may be any
// non-empty string.
func HTTPSAuth(username, password string) transport.AuthMethod {
	if username == "" {
		username = "git" // ignored by most providers when a token is the password
	}
	return &githttp.BasicAuth{Username: username, Password: password}
}

// SSHAuth builds public-key authentication for an ssh remote. knownHosts may be
// nil to accept any host key (use only in trusted environments).
func SSHAuth(privateKey []byte, passphrase string, knownHosts []byte) (transport.AuthMethod, error) {
	auth, err := gitssh.NewPublicKeys("git", privateKey, passphrase)
	if err != nil {
		return nil, fmt.Errorf("loading ssh key: %w", err)
	}
	if len(knownHosts) == 0 {
		// No known_hosts provided: skip verification. Document this clearly.
		auth.HostKeyCallback = ssh.InsecureIgnoreHostKey() //nolint:gosec // opt-in via empty known_hosts
	} else {
		cb, err := gitssh.NewKnownHostsCallback()
		if err == nil {
			auth.HostKeyCallback = cb
		}
	}
	return auth, nil
}

// AuthFromSecret builds a Git auth method from Secret data, choosing HTTPS or
// SSH by the remote URL scheme. HTTPS reads "username" and "password" (or
// "token"); SSH reads "identity" plus optional "known_hosts" and "password"
// (key passphrase). Empty data means no authentication (public HTTPS repo).
func AuthFromSecret(url string, data map[string][]byte) (transport.AuthMethod, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		password := string(data["password"])
		if password == "" {
			password = string(data["token"])
		}
		return HTTPSAuth(string(data["username"]), password), nil
	}
	identity := data["identity"]
	if len(identity) == 0 {
		return nil, fmt.Errorf("ssh git secret missing %q", "identity")
	}
	return SSHAuth(identity, string(data["password"]), data["known_hosts"])
}

// Clone shallow-clones a single branch into dir.
func Clone(ctx context.Context, url, branch, dir string, auth transport.AuthMethod) (*git.Repository, error) {
	repo, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{
		URL:           url,
		Auth:          auth,
		ReferenceName: plumbing.NewBranchReferenceName(branch),
		SingleBranch:  true,
		Depth:         1,
	})
	if err != nil {
		return nil, fmt.Errorf("cloning %s (%s): %w", url, branch, err)
	}
	return repo, nil
}

// CommitAndPush stages the given repo-relative paths, commits them, and pushes
// to the branch when push is true. It returns the new commit SHA.
func CommitAndPush(
	ctx context.Context,
	repo *git.Repository,
	paths []string,
	author Author,
	message string,
	branch string,
	auth transport.AuthMethod,
	push bool,
) (string, error) {
	wt, err := repo.Worktree()
	if err != nil {
		return "", err
	}
	for _, p := range paths {
		if _, err := wt.Add(p); err != nil {
			return "", fmt.Errorf("staging %s: %w", p, err)
		}
	}

	sig := &object.Signature{Name: author.Name, Email: author.Email, When: time.Now()}
	hash, err := wt.Commit(message, &git.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		return "", fmt.Errorf("committing: %w", err)
	}

	if push {
		err = repo.PushContext(ctx, &git.PushOptions{
			Auth:       auth,
			RefSpecs:   []config.RefSpec{config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", branch, branch))},
			RemoteName: "origin",
		})
		if err != nil && err != git.NoErrAlreadyUpToDate {
			return "", fmt.Errorf("pushing: %w", err)
		}
	}
	return hash.String(), nil
}
