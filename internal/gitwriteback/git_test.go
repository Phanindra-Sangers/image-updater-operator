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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
)

// TestFileTransportRoundTrip exercises the full Git write-back cycle (clone, edit
// a helm values file, commit, push) over the file:// transport against a local
// bare repo, then reads the value back out of the bare repo to confirm the push
// landed. This is the same plumbing hack/e2e-git-writeback.sh relies on.
func TestFileTransportRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	bare := filepath.Join(root, "config.git")

	gitCmd := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=seed", "GIT_AUTHOR_EMAIL=seed@x",
			"GIT_COMMITTER_NAME=seed", "GIT_COMMITTER_EMAIL=seed@x")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitCmd(root, "init", "-q", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "values.yaml"),
		[]byte("image:\n  repository: ghcr.io/org/app\n  tag: 1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(seed, "add", "values.yaml")
	gitCmd(seed, "commit", "-q", "-m", "seed")
	gitCmd(root, "init", "-q", "--bare", "-b", "main", bare)
	gitCmd(seed, "remote", "add", "origin", bare)
	gitCmd(seed, "push", "-q", "origin", "main")

	ctx := context.Background()
	repo, err := Clone(ctx, "file://"+bare, "main", filepath.Join(root, "clone"), nil)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	rel, changed, err := Apply(filepath.Join(root, "clone"), Edit{
		Target:      Target{Kind: TargetHelm, Path: "values.yaml"},
		Repository:  "ghcr.io/org/app",
		Tag:         "1.2.0",
		HelmNameKey: "image.repository",
		HelmTagKey:  "image.tag",
	})
	if err != nil || !changed {
		t.Fatalf("apply changed=%v err=%v", changed, err)
	}
	if _, err := CommitAndPush(ctx, repo, []string{rel},
		Author{Name: "op", Email: "op@x"}, "bump", "main", nil, true); err != nil {
		t.Fatalf("commit/push: %v", err)
	}

	got := fileAtHead(t, bare, "values.yaml")
	if !strings.Contains(got, "tag: 1.2.0") {
		t.Fatalf("bare repo values.yaml missing updated tag:\n%s", got)
	}
}

// fileAtHead returns the contents of path at the bare repo's HEAD commit.
func fileAtHead(t *testing.T, bare, path string) string {
	t.Helper()
	repo, err := git.PlainOpen(bare)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	f, err := commit.File(path)
	if err != nil {
		t.Fatal(err)
	}
	content, err := f.Contents()
	if err != nil {
		t.Fatal(err)
	}
	return content
}
