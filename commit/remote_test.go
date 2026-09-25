package commit

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const (
	readme     = "README.md"
	oneFile    = "a.yaml"
	sopsConfig = ".sops.yaml"
)

// opened seeds a branch with one commit off main and opens its pull request.
func opened(t *testing.T, f *Fake, branch string, files map[string][]byte) PullRequest {
	t.Helper()
	ctx := context.Background()
	if err := f.CreateBranch(ctx, repoA, branch, "main"); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(ctx, repoA, branch, "change", files); err != nil {
		t.Fatal(err)
	}
	pr, err := f.OpenPullRequest(ctx, repoA, branch, "main", "Change", actionID)
	if err != nil {
		t.Fatal(err)
	}
	return pr
}

func TestFindPullRequestSeesOnlyTheOpenOne(t *testing.T) {
	f, _, _ := fixture(t)
	ctx := context.Background()
	if _, found, err := f.FindPullRequest(ctx, repoA, "feature"); err != nil || found {
		t.Fatalf("before opening: found=%v err=%v", found, err)
	}
	pr := opened(t, f, "feature", map[string][]byte{oneFile: []byte("a")})
	got, found, err := f.FindPullRequest(ctx, repoA, "feature")
	if err != nil || !found || got.Number != pr.Number || got.HeadSHA != pr.HeadSHA {
		t.Fatalf("open: found=%v got=%+v err=%v", found, got, err)
	}
	if err := f.Close(ctx, pr, false); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := f.FindPullRequest(ctx, repoA, "feature"); found {
		t.Error("a closed pull request is found")
	}
	if _, err := f.OpenPullRequest(ctx, repoA, "feature", "main", "Again", actionID); err != nil {
		t.Fatal(err)
	}
	if prs := f.PullRequests(); len(prs) != 2 || !prs[0].Closed || prs[1].Closed {
		t.Errorf("reopening the branch made %+v", prs)
	}
}

func TestOpenDraftPullRequestIsADraftAndReused(t *testing.T) {
	f, _, _ := fixture(t)
	ctx := context.Background()
	if err := f.CreateBranch(ctx, repoA, "draft", "main"); err != nil {
		t.Fatal(err)
	}
	pr, err := f.OpenDraftPullRequest(ctx, repoA, "draft", "main", "Draft", actionID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.OpenPullRequest(ctx, repoA, "draft", "main", "Draft", actionID)
	if err != nil || again.Number != pr.Number {
		t.Fatalf("second open: %+v %v", again, err)
	}
	if prs := f.PullRequests(); len(prs) != 1 || !prs[0].Draft {
		t.Errorf("want one draft, got %+v", prs)
	}
}

func TestEnableAutoMergeIsTheCallersChoice(t *testing.T) {
	f, _, _ := fixture(t)
	ctx := context.Background()
	pr := opened(t, f, "auto", map[string][]byte{oneFile: []byte("a")})
	if f.PullRequests()[0].AutoMerge {
		t.Fatal("auto-merge armed without being asked")
	}
	if err := f.EnableAutoMerge(ctx, pr); err != nil {
		t.Fatal(err)
	}
	if !f.PullRequests()[0].AutoMerge {
		t.Error("auto-merge not armed")
	}
	if st, _ := f.Status(ctx, pr); st.Merged {
		t.Error("the fake merged by itself")
	}
	if err := f.Close(ctx, pr, false); err != nil {
		t.Fatal(err)
	}
	if err := f.EnableAutoMerge(ctx, pr); err == nil {
		t.Error("auto-merge armed on a closed pull request")
	}
}

func TestCloseIsIdempotentAndDeletesTheBranchOnRequest(t *testing.T) {
	f, _, _ := fixture(t)
	ctx := context.Background()
	kept := opened(t, f, "kept", map[string][]byte{oneFile: []byte("a")})
	gone := opened(t, f, "gone", map[string][]byte{"b.yaml": []byte("b")})
	if err := f.Close(ctx, kept, false); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(ctx, kept, true); err != nil {
		t.Fatalf("closing twice: %v", err)
	}
	if f.Files(repoA, "kept") == nil {
		t.Error("the branch of a pull request closed without deleteBranch is gone")
	}
	if err := f.Close(ctx, gone, true); err != nil {
		t.Fatal(err)
	}
	if f.Files(repoA, "gone") != nil {
		t.Error("the branch survived deleteBranch")
	}
	if err := Merge(ctx, f, kept, true); err == nil {
		t.Error("a closed pull request merged")
	}
	if err := f.Close(ctx, PullRequest{Repository: repoA, Number: 99}, false); err == nil {
		t.Error("closing an unknown pull request succeeded")
	}
}

func TestRevertInvertsTheMergedChangeOnTheTip(t *testing.T) {
	f, _, _ := fixture(t)
	ctx := context.Background()
	f.AddBranch(repoA, "main", map[string][]byte{
		readme:     []byte("hello"),
		sopsConfig: []byte("rules: [a]"),
		"old.yaml": []byte("old"),
	})
	pr := opened(t, f, "feature", map[string][]byte{
		"new/values.yaml": []byte("new"),
		readme:            []byte("changed"),
		sopsConfig:        []byte("rules: [a, b]"),
	})
	if _, err := f.Revert(ctx, pr, "not yet", nil); !errors.Is(err, ErrNotMerged) {
		t.Fatalf("revert before the merge: %v", err)
	}
	if err := Merge(ctx, f, pr, true); err != nil {
		t.Fatal(err)
	}
	// The tip moves on after the merge; the revert lands on top of it.
	if err := f.Commit(ctx, repoA, "main", "later", map[string][]byte{"later.yaml": []byte("later")}); err != nil {
		t.Fatal(err)
	}
	revert, err := f.Revert(ctx, pr, "Reverts the change.", map[string][]byte{sopsConfig: []byte("rules: [a, c]")})
	if err != nil {
		t.Fatal(err)
	}
	if revert.Head != "revert-1-feature" || revert.Base != "main" {
		t.Errorf("revert branch/base: %+v", revert)
	}
	files := f.Files(repoA, revert.Head)
	want := map[string]string{readme: "hello", sopsConfig: "rules: [a, c]", "old.yaml": "old", "later.yaml": "later"}
	if len(files) != len(want) {
		t.Errorf("revert tree has %d files, want %d: %q", len(files), len(want), files)
	}
	for path, content := range want {
		if string(files[path]) != content {
			t.Errorf("%s = %q, want %q", path, files[path], content)
		}
	}
	prs := f.PullRequests()
	if len(prs) != 2 || prs[1].Title != `revert: Revert "Change"` || prs[1].Body != "Reverts the change." || prs[1].Merged {
		t.Errorf("revert pull request: %+v", prs[1])
	}
	commits := f.Commits(repoA, revert.Head)
	if msg := commits[len(commits)-1].Message; !strings.HasPrefix(msg, `Revert "Change" (#1)`) || !strings.HasSuffix(msg, "Reverts the change.") {
		t.Errorf("revert commit message: %q", msg)
	}
	if again, err := f.Revert(ctx, pr, "again", nil); err != nil || again.Number != revert.Number {
		t.Errorf("a second revert did not reuse the open revert pull request: %+v %v", again, err)
	}
}

func TestRevertRefusesAMergeThatChangedNothing(t *testing.T) {
	f, _, _ := fixture(t)
	ctx := context.Background()
	if err := f.CreateBranch(ctx, repoA, "same", "main"); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(ctx, repoA, "same", "same", map[string][]byte{readme: []byte("hello")}); err != nil {
		t.Fatal(err)
	}
	pr, err := f.OpenPullRequest(ctx, repoA, "same", "main", "Same", actionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := Merge(ctx, f, pr, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Revert(ctx, pr, "", nil); !errors.Is(err, ErrNothingToRevert) {
		t.Errorf("want ErrNothingToRevert, got %v", err)
	}
}

func TestFakeFailCoversTheNewOperations(t *testing.T) {
	f, _, _ := fixture(t)
	ctx := context.Background()
	pr := opened(t, f, "fail", map[string][]byte{oneFile: []byte("a")})
	boom := errors.New("boom")
	f.Fail[OpFindPullRequest], f.Fail[OpEnableAutoMerge], f.Fail[OpClose], f.Fail[OpRevert] = boom, boom, boom, boom
	_, _, findErr := f.FindPullRequest(ctx, repoA, "fail")
	_, revertErr := f.Revert(ctx, pr, "", nil)
	for op, err := range map[string]error{
		OpFindPullRequest: findErr,
		OpEnableAutoMerge: f.EnableAutoMerge(ctx, pr),
		OpClose:           f.Close(ctx, pr, false),
		OpRevert:          revertErr,
	} {
		if !errors.Is(err, boom) {
			t.Errorf("%s: want boom, got %v", op, err)
		}
	}
}

func TestApproveRecordsTheReviewAndTheRollup(t *testing.T) {
	f, _, _ := fixture(t)
	ctx := context.Background()
	pr := opened(t, f, "approved", map[string][]byte{oneFile: []byte("a")})
	if err := f.Approve(ctx, pr, "looks right"); err != nil {
		t.Fatal(err)
	}
	st, err := f.Status(ctx, pr)
	if err != nil || st.Review != ReviewApproved {
		t.Fatalf("after Approve: %+v %v", st, err)
	}
	if prs := f.PullRequests(); len(prs) != 1 || len(prs[0].Approvals) != 1 || prs[0].Approvals[0] != "looks right" {
		t.Errorf("approvals: %+v", prs)
	}
	f.SetReview(pr, ReviewChangesRequested)
	if err := f.Approve(ctx, pr, "again"); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.Status(ctx, pr); st.Review != ReviewChangesRequested {
		t.Error("an approval cleared another reviewer's changes requested")
	}
	if err := f.Close(ctx, pr, false); err != nil {
		t.Fatal(err)
	}
	if err := f.Approve(ctx, pr, "late"); err == nil {
		t.Error("a closed pull request took an approval")
	}
	if err := f.Approve(ctx, PullRequest{Repository: repoA, Number: 99}, "x"); err == nil {
		t.Error("an unknown pull request took an approval")
	}
}

func TestCommitRemovesAPathWhoseContentIsNil(t *testing.T) {
	f := NewFake()
	f.AddBranch(repoA, "main", map[string][]byte{"a/pool.yaml": []byte("pool"), "a/kustomization.yaml": []byte("resources: [pool.yaml]")})
	ctx := context.Background()
	if err := f.CreateBranch(ctx, repoA, "remove-pool", "main"); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(ctx, repoA, "remove-pool", "remove the pool", map[string][]byte{"a/pool.yaml": nil, "a/kustomization.yaml": []byte("resources: []")}); err != nil {
		t.Fatal(err)
	}
	files := f.Files(repoA, "remove-pool")
	if _, ok := files["a/pool.yaml"]; ok {
		t.Error("a/pool.yaml is still in the tree")
	}
	if got := string(files["a/kustomization.yaml"]); got != "resources: []" {
		t.Errorf("kustomization.yaml = %q", got)
	}
}

func TestFakeReadFileAtTheBranchHead(t *testing.T) {
	f := NewFake()
	f.AddBranch(repoA, "main", map[string][]byte{".sops.yaml": []byte("creation_rules: []")})
	ctx := context.Background()
	got, err := f.ReadFile(ctx, repoA, "main", ".sops.yaml")
	if err != nil || string(got) != "creation_rules: []" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	if _, err := f.ReadFile(ctx, repoA, "main", "missing.yaml"); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("missing file: want ErrFileNotFound, got %v", err)
	}
	f.Fail[OpReadFile] = &AuthError{Op: OpReadFile, Status: 401}
	if _, err := f.ReadFile(ctx, repoA, "main", ".sops.yaml"); !errors.Is(err, ErrAuth) {
		t.Errorf("Fail: want ErrAuth, got %v", err)
	}
}
