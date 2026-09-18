package commit

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/giantswarm/gitops-commit/provenance"
)

var (
	repoA = Repository{Owner: "acme", Name: "management-clusters"}
	repoB = Repository{Owner: "acme", Name: "shared-configs"}
)

const (
	generatedContent = "gen-3f9a1c7e-not-for-logs"
	actionID    = "action 0f6b2d; approved in the team review"
)

func fixture(t *testing.T) (*Fake, Request, []Change) {
	t.Helper()
	f := NewFake()
	f.AddBranch(repoA, "main", map[string][]byte{"README.md": []byte("hello")})
	f.AddBranch(repoB, "main", nil)
	locA1 := location(t, repoA, "main", "management-clusters/example/dex")
	locA2 := location(t, repoA, "main", "management-clusters/example/portal")
	locB := location(t, repoB, "main", "")
	changes := []Change{
		{Location: locA1, Files: map[string][]byte{path(t, locA1, "kustomization.yaml"): []byte("resources: [a]"), path(t, locA1, "secret.enc.yaml"): []byte(generatedContent)}},
		{Location: locA2, Files: map[string][]byte{path(t, locA2, "values.yaml"): []byte("portal: on")}},
		{Location: locB, Files: map[string][]byte{path(t, locB, "clients.yaml"): []byte("client: portal")}},
	}
	return f, Request{Branch: "capability/dex-0f6b2d", Title: "Enable Dex on example", Body: actionID}, changes
}

func location(t *testing.T, repo Repository, branch, dir string) provenance.Location {
	t.Helper()
	loc, err := provenance.Explicit(repo.String(), branch, dir)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func path(t *testing.T, loc provenance.Location, name string) string {
	t.Helper()
	p, err := loc.Path(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpenOneCommitPerRepositoryAndTheCallersBody(t *testing.T) {
	f, req, changes := fixture(t)
	prs, err := Open(context.Background(), f, req, changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 2 {
		t.Fatalf("got %d pull requests, want one per repository", len(prs))
	}
	for _, repo := range []Repository{repoA, repoB} {
		commits := f.Commits(repo, req.Branch)
		if len(commits) != 1 {
			t.Errorf("%s: %d commits on %s, want one", repo, len(commits), req.Branch)
		}
		if commits[0].Message != req.Title {
			t.Errorf("%s: commit message %q, want %q", repo, commits[0].Message, req.Title)
		}
	}
	tree := f.Files(repoA, req.Branch)
	for _, want := range []string{"README.md", "management-clusters/example/dex/kustomization.yaml", "management-clusters/example/dex/secret.enc.yaml", "management-clusters/example/portal/values.yaml"} {
		if _, ok := tree[want]; !ok {
			t.Errorf("%s: %s missing from the head tree", repoA, want)
		}
	}
	if got := string(f.Files(repoB, req.Branch)["clients.yaml"]); got != "client: portal" {
		t.Errorf("%s: clients.yaml = %q", repoB, got)
	}
	held := f.PullRequests()
	for i, pr := range prs {
		if pr.Repository != held[i].Repository || pr.Number != held[i].Number {
			t.Fatalf("pull request %d does not match the remote's: %+v vs %+v", i, pr, held[i].PullRequest)
		}
		if held[i].Body != actionID || held[i].Title != req.Title {
			t.Errorf("%s#%d: title %q body %q, want the caller's", pr.Repository, pr.Number, held[i].Title, held[i].Body)
		}
		if pr.Head != req.Branch || pr.Base != "main" || pr.HeadSHA == "" {
			t.Errorf("%s#%d: head %q base %q sha %q", pr.Repository, pr.Number, pr.Head, pr.Base, pr.HeadSHA)
		}
	}
	if prs[0].Repository != repoA || prs[1].Repository != repoB {
		t.Errorf("repositories out of name order: %s, %s", prs[0].Repository, prs[1].Repository)
	}
}

func TestOpenAgainReusesTheOpenPullRequest(t *testing.T) {
	f, req, changes := fixture(t)
	first, err := Open(context.Background(), f, req, changes)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(context.Background(), f, req, changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.PullRequests()) != 2 || second[0].Number != first[0].Number {
		t.Errorf("second run opened new pull requests: %d held", len(f.PullRequests()))
	}
	if second[0].HeadSHA == first[0].HeadSHA {
		t.Error("second run made no commit")
	}
}

func TestOpenRefusesBadInput(t *testing.T) {
	f, req, changes := fixture(t)
	ctx := context.Background()
	if _, err := Open(ctx, f, Request{Title: "x"}, changes); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("no branch: %v", err)
	}
	if _, err := Open(ctx, f, req, []Change{{Location: changes[0].Location}}); !errors.Is(err, ErrNoFiles) {
		t.Errorf("no files: %v", err)
	}
	other := changes[1]
	other.Location.Branch = "release"
	if _, err := Open(ctx, f, req, []Change{changes[0], other}); !errors.Is(err, ErrConflictingBase) {
		t.Errorf("conflicting base: %v", err)
	}
	if _, err := Open(ctx, f, req, []Change{changes[0], changes[0]}); !errors.Is(err, ErrDuplicatePath) {
		t.Errorf("duplicate path: %v", err)
	}
	if len(f.PullRequests()) != 0 {
		t.Error("a refused request reached the remote")
	}
}

func TestMergeRefusedBeforeApprovalAndBeforeGreen(t *testing.T) {
	f, req, changes := fixture(t)
	ctx := context.Background()
	prs, err := Open(ctx, f, req, changes)
	if err != nil {
		t.Fatal(err)
	}
	pr := prs[0]
	f.Fail[OpStatus] = errors.New("status must not be read before approval")
	if err := Merge(ctx, f, pr, false); !errors.Is(err, ErrNotApproved) {
		t.Errorf("before approval: %v", err)
	}
	delete(f.Fail, OpStatus)
	f.SetChecks(pr, ChecksPending)
	if err := Merge(ctx, f, pr, true); !errors.Is(err, ErrChecksPending) {
		t.Errorf("checks pending: %v", err)
	}
	f.SetChecks(pr, ChecksFailure)
	if err := Merge(ctx, f, pr, true); !errors.Is(err, ErrChecksFailed) {
		t.Errorf("checks failed: %v", err)
	}
	if f.PullRequests()[0].Merged {
		t.Fatal("merged although refused")
	}
	f.SetChecks(pr, ChecksSuccess)
	if err := Merge(ctx, f, pr, true); err != nil {
		t.Fatalf("approved and green: %v", err)
	}
	if !f.PullRequests()[0].Merged {
		t.Error("not merged although approved and green")
	}
	if err := Merge(ctx, f, pr, true); err != nil {
		t.Errorf("merging a merged pull request: %v", err)
	}
	if got := f.Files(repoA, "main")["management-clusters/example/dex/kustomization.yaml"]; string(got) != "resources: [a]" {
		t.Error("main does not carry the merged files")
	}
}

func TestMergeRefusedWhenTheHeadMoved(t *testing.T) {
	f, req, changes := fixture(t)
	ctx := context.Background()
	prs, err := Open(ctx, f, req, changes)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(ctx, repoA, req.Branch, "someone else", map[string][]byte{"x": []byte("y")}); err != nil {
		t.Fatal(err)
	}
	if err := Merge(ctx, f, prs[0], true); !errors.Is(err, ErrHeadMoved) {
		t.Errorf("head moved: %v", err)
	}
	if f.PullRequests()[0].Merged {
		t.Error("merged a head the caller never saw")
	}
}

func TestMergeWithoutChecksHasNothingToWaitFor(t *testing.T) {
	f, req, changes := fixture(t)
	ctx := context.Background()
	prs, err := Open(ctx, f, req, changes)
	if err != nil {
		t.Fatal(err)
	}
	if err := Merge(ctx, f, prs[1], true); err != nil {
		t.Fatal(err)
	}
}

func TestAuthErrorFromTheRemoteIsErrAuthWithStatus(t *testing.T) {
	f, req, changes := fixture(t)
	f.Fail[OpCommit] = &AuthError{Op: OpCommit, Status: 403}
	prs, err := Open(context.Background(), f, req, changes)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
	var auth *AuthError
	if !errors.As(err, &auth) || auth.Status != 403 || auth.Op != OpCommit {
		t.Errorf("auth error carries %+v", auth)
	}
	if len(prs) != 0 {
		t.Errorf("pull requests opened before the refusal: %d", len(prs))
	}
}

func TestOpenReturnsWhatLandedBeforeAnError(t *testing.T) {
	f, req, changes := fixture(t)
	changes[2].Location.Branch = "missing"
	prs, err := Open(context.Background(), f, req, changes)
	if err == nil || len(prs) != 1 || prs[0].Repository != repoA {
		t.Errorf("want repoA's pull request with the error, got %v / %v", prs, err)
	}
}

// TestNoSecretValueInErrorsOrLogs: an error from any step names the operation,
// never the content; and the package has no way to log at all.
func TestNoSecretValueInErrorsOrLogs(t *testing.T) {
	f, req, changes := fixture(t)
	for _, op := range []string{OpCreateBranch, OpCommit, OpOpenPullRequest} {
		f.Fail = map[string]error{op: errors.New(op + " refused")}
		_, err := Open(context.Background(), f, req, changes)
		if err == nil || strings.Contains(err.Error(), generatedContent) {
			t.Errorf("%s: error carries the secret value: %v", op, err)
		}
	}
	forbidden := regexp.MustCompile(`^(Print|Println|Printf|Fprint|Fprintln|Fprintf|Stdout|Stderr)$`)
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, e.Name(), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			switch p := strings.Trim(imp.Path.Value, `"`); p {
			case "log", "log/slog", "os", "io":
				t.Errorf("%s imports %s: the package must not log", e.Name(), p)
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && forbidden.MatchString(id.Name) {
				t.Errorf("%s: %s at %s", e.Name(), id.Name, fset.Position(id.Pos()))
			}
			return true
		})
	}
}
