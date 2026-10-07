package commit

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
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

// TestLiveRevertOntoAnExistingRevertBranch reverts a merged pull request twice
// on a real repository, GITOPS_COMMIT_LIVE_REPO and GITHUB_TOKEN as above.
// Scratch branches gitops-commit-live/<random>/{base,feature} stand in for
// the base and a merged change: a pull request between them is opened and
// merged into the scratch base, never the default branch. The second revert
// lands on the open revert pull request's head brought up to date with the
// base; a third, after the base changed the reverted file, is refused. The
// revert pull request is closed and every scratch branch deleted at the end.
func TestLiveRevertOntoAnExistingRevertBranch(t *testing.T) {
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
	base, feature := prefix+"/base", prefix+"/feature"
	file := prefix + "/a.yaml"
	var revert PullRequest
	t.Cleanup(func() {
		if revert.Number != 0 {
			if err := gh.Close(ctx, revert, true); err != nil {
				t.Logf("closing %s: %v", revert.URL, err)
			}
		}
		for _, b := range []string{feature, base} {
			if _, err := gh.gh.Git.DeleteRef(ctx, owner, name, headsPrefix+b); err != nil && !hasStatus(err, http.StatusUnprocessableEntity) {
				t.Logf("deleting %s: %v", b, err)
			}
		}
	})
	if err := gh.CreateBranch(ctx, repo, base, settings.GetDefaultBranch()); err != nil {
		t.Fatal(err)
	}
	if err := gh.CreateBranch(ctx, repo, feature, base); err != nil {
		t.Fatal(err)
	}
	if err := gh.Commit(ctx, repo, feature, "the change", map[string][]byte{file: []byte("a\n")}); err != nil {
		t.Fatal(err)
	}
	merged, err := gh.OpenPullRequest(ctx, repo, feature, base, "test: gitops-commit live revert (scratch)", "A scratch pull request of gitops-commit's live test, merged into a scratch branch; deleted with it.")
	if err != nil {
		t.Fatal(err)
	}
	if err := gh.Merge(ctx, merged); err != nil {
		t.Fatal(err)
	}
	if revert, err = gh.Revert(ctx, merged, "the first revert", nil); err != nil {
		t.Fatal(err)
	}
	first := revert.HeadSHA
	// The base moves on; a second revert lands on the open revert pull
	// request's head brought up to date with it.
	if err := gh.Commit(ctx, repo, base, "later on the base", map[string][]byte{prefix + "/later.yaml": []byte("later\n")}); err != nil {
		t.Fatal(err)
	}
	again, err := gh.Revert(ctx, merged, "the second revert", nil)
	if err != nil {
		t.Fatalf("second revert: %v", err)
	}
	if again.Number != revert.Number || again.HeadSHA == first {
		t.Errorf("second revert: pull request %d on %s, want %d on a new head", again.Number, again.HeadSHA, revert.Number)
	}
	onBase, _, err := gh.gh.Repositories.CompareCommits(ctx, owner, name, base, again.Head, nil)
	if err != nil {
		t.Fatal(err)
	}
	onFirst, _, err := gh.gh.Repositories.CompareCommits(ctx, owner, name, first, again.Head, nil)
	if err != nil {
		t.Fatal(err)
	}
	if onBase.GetBehindBy() != 0 || onFirst.GetBehindBy() != 0 || onFirst.GetAheadBy() != 3 {
		t.Errorf("%s is %d behind %s and %d behind, %d ahead of the first revert's head, want 0, 0 and 3 (the base's later commit, the merge, the second revert)", again.Head, onBase.GetBehindBy(), base, onFirst.GetBehindBy(), onFirst.GetAheadBy())
	}
	if _, err := gh.ReadFile(ctx, repo, again.Head, file); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("the reverted file is still at the head: %v", err)
	}
	// The base changes the reverted file: it no longer merges into the revert
	// branch, and the revert is refused naming the branch and its pull request.
	if err := gh.Commit(ctx, repo, base, "the reverted file changed on the base", map[string][]byte{file: []byte("b\n")}); err != nil {
		t.Fatal(err)
	}
	_, err = gh.Revert(ctx, merged, "the third revert", nil)
	if !errors.Is(err, ErrStaleBranch) || !strings.Contains(err.Error(), target+"@"+again.Head) || !strings.Contains(err.Error(), revert.URL) {
		t.Errorf("conflict: want ErrStaleBranch naming %s and %s, got %v", again.Head, revert.URL, err)
	}
	after, _, getErr := gh.gh.Git.GetRef(ctx, owner, name, headsPrefix+again.Head)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if after.GetObject().GetSHA() != again.HeadSHA {
		t.Errorf("a refused revert moved %s from %s to %s", again.Head, again.HeadSHA, after.GetObject().GetSHA())
	}
	t.Logf("on %s: %s reverted twice on %s (%d behind %s, the first head %s plus %d commits); the third revert was refused: %v", target, merged.URL, revert.URL, onBase.GetBehindBy(), base, first, onFirst.GetAheadBy(), err)
}
