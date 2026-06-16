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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestWritebackRoundTrip clones a local bare repo, applies a marker update, and
// pushes it, then re-clones and verifies the change landed. It uses no network
// and no git binary (go-git is pure Go).
func TestWritebackRoundTrip(t *testing.T) {
	ctx := context.Background()
	const branch = "master" // go-git's default for a freshly initialized repo

	// Origin: a bare repo. go-git cannot clone an empty repo, so seed it by
	// committing in a non-bare repo and pushing to the bare origin.
	origin := t.TempDir()
	if _, err := git.PlainInit(origin, true); err != nil {
		t.Fatal(err)
	}
	seedDir := t.TempDir()
	seed, err := git.PlainInit(seedDir, false)
	if err != nil {
		t.Fatal(err)
	}
	values := "image:\n  repository: ghcr.io/org/app\n  tag: 1.0.0  # {\"$image-policy\": \"app\"}\n"
	if err := os.WriteFile(filepath.Join(seedDir, "values.yaml"), []byte(values), 0o644); err != nil {
		t.Fatal(err)
	}
	seedWT, _ := seed.Worktree()
	if _, err := seedWT.Add("values.yaml"); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "seed", Email: "seed@test"}
	if _, err := seedWT.Commit("seed", &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{origin}}); err != nil {
		t.Fatal(err)
	}
	if err := seed.Push(&git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec("refs/heads/" + branch + ":refs/heads/" + branch)},
	}); err != nil {
		t.Fatal(err)
	}

	// Exercise the writeback: clone, update marker to 1.2.0, commit, push.
	cloneDir := t.TempDir()
	repo, err := Clone(ctx, origin, branch, cloneDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(p string) (Resolved, bool) {
		if p == "app" {
			return Resolved{Tag: "1.2.0", Image: "ghcr.io/org/app:1.2.0", Repository: "ghcr.io/org/app"}, true
		}
		return Resolved{}, false
	}
	res, err := ScanAndUpdate(cloneDir, ".", resolve)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(res.Updates))
	}
	sha, err := CommitAndPush(ctx, repo, res.ChangedFiles, Author{Name: "op", Email: "op@test"},
		"chore: bump", branch, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if sha == "" {
		t.Fatal("expected a commit sha")
	}

	// Verify: a fresh clone of origin now has tag 1.2.0.
	verifyDir := t.TempDir()
	if _, err := git.PlainClone(verifyDir, false, &git.CloneOptions{URL: origin}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(verifyDir, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "tag: 1.2.0") {
		t.Errorf("pushed file missing tag 1.2.0:\n%s", got)
	}
	if strings.Contains(string(got), "tag: 1.0.0") {
		t.Errorf("pushed file still has old tag:\n%s", got)
	}
}
