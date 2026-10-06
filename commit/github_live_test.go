package commit

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"testing"
)

// TestLiveCreateBranchOnAStaleBranch reproduces a branch left behind its base
// on a real repository. GITOPS_COMMIT_LIVE_REPO names it (owner/name) and
// GITHUB_TOKEN a token allowed to write its branches; without both the test
// is skipped. Two scratch branches, gitops-commit-live/<random>/{base,stale},
// stand in for the base and the branch an earlier run left behind; both are
// deleted at the end, nothing else in the repository is touched and no pull
// request is opened.
func TestLiveCreateBranchOnAStaleBranch(t *testing.T) {
	target, token := os.Getenv("GITOPS_COMMIT_LIVE_REPO"), os.Getenv("GITHUB_TOKEN")
	if target == "" || token == "" {
		t.Skip("set GITOPS_COMMIT_LIVE_REPO=owner/name and GITHUB_TOKEN to run against a real repository")
	}
	owner, name, ok := strings.Cut(target, "/")
	if !ok {
		t.Fatalf("GITOPS_COMMIT_LIVE_REPO = %q, want owner/name", target)
	}
	repo := Repository{Owner: owner, Name: name}
	gh, err := NewGitHub(token)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	settings, _, err := gh.gh.Repositories.Get(ctx, owner, name)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "gitops-commit-live/" + strings.ToLower(rand.Text()[:8])
	base, stale := prefix+"/base", prefix+"/stale"
	dir := prefix
	t.Cleanup(func() {
		for _, b := range []string{stale, base} {
			if _, err := gh.gh.Git.DeleteRef(ctx, owner, name, headsPrefix+b); err != nil {
				t.Logf("deleting %s: %v", b, err)
			}
		}
	})
	// The base is a scratch branch off the default branch; the stale branch
	// starts at the base and the earlier run commits there.
	if err := gh.CreateBranch(ctx, repo, base, settings.GetDefaultBranch()); err != nil {
		t.Fatal(err)
	}
	if err := gh.CreateBranch(ctx, repo, stale, base); err != nil {
		t.Fatal(err)
	}
	if err := gh.Commit(ctx, repo, stale, "the earlier run", map[string][]byte{dir + "/kustomization.yaml": []byte("resources: [a]\n"), dir + "/a.yaml": []byte("a\n")}); err != nil {
		t.Fatal(err)
	}
	// The base moves on: a directory added since, the issue's case.
	if err := gh.Commit(ctx, repo, base, "later on the base", map[string][]byte{dir + "/later/values.yaml": []byte("later\n")}); err != nil {
		t.Fatal(err)
	}
	// Outcome one: the stale branch is brought up to date and the second run's
	// commit lands on a head that contains the base.
	if err := gh.CreateBranch(ctx, repo, stale, base); err != nil {
		t.Fatalf("stale branch behind its base: %v", err)
	}
	if err := gh.Commit(ctx, repo, stale, "the second run", map[string][]byte{dir + "/kustomization.yaml": []byte("resources: [a, later]\n")}); err != nil {
		t.Fatal(err)
	}
	compare, _, err := gh.gh.Repositories.CompareCommits(ctx, owner, name, base, stale, nil)
	if err != nil {
		t.Fatal(err)
	}
	if compare.GetBehindBy() != 0 || compare.GetAheadBy() != 3 {
		t.Errorf("%s is %d behind and %d ahead of %s, want 0 behind and 3 ahead (the earlier run, the merge, the second run)", stale, compare.GetBehindBy(), compare.GetAheadBy(), base)
	}
	if got, err := gh.ReadFile(ctx, repo, stale, dir+"/later/values.yaml"); err != nil || string(got) != "later\n" {
		t.Errorf("the head lacks the base's later file: %q, %v", got, err)
	}
	// Outcome two: the base changes the file the stale branch changed; the
	// update is refused naming the branch, and the head stays.
	if err := gh.Commit(ctx, repo, base, "conflicting on the base", map[string][]byte{dir + "/kustomization.yaml": []byte("resources: [b]\n")}); err != nil {
		t.Fatal(err)
	}
	before, _, err := gh.gh.Git.GetRef(ctx, owner, name, headsPrefix+stale)
	if err != nil {
		t.Fatal(err)
	}
	err = gh.CreateBranch(ctx, repo, stale, base)
	if !errors.Is(err, ErrStaleBranch) || !strings.Contains(err.Error(), target+"@"+stale) {
		t.Errorf("conflict: want ErrStaleBranch naming %s, got %v", stale, err)
	}
	after, _, getErr := gh.gh.Git.GetRef(ctx, owner, name, headsPrefix+stale)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if after.GetObject().GetSHA() != before.GetObject().GetSHA() {
		t.Errorf("a refused update moved %s from %s to %s", stale, before.GetObject().GetSHA(), after.GetObject().GetSHA())
	}
	t.Logf("on %s: %s behind %s was brought up to date (0 behind, %d ahead) and the second run's commit landed; the conflicting update was refused: %v", target, stale, base, compare.GetAheadBy(), err)
}
