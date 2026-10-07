package commit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// refusing is a GitHub that answers every request with the given status.
func refusing(t *testing.T, status int) *GitHub {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	t.Cleanup(srv.Close)
	gh, err := NewGitHub("the-persons-value-never-logged", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	return gh
}

func TestGitHubRefusedTokenIsErrAuthWithTheStatus(t *testing.T) {
	_, req, changes := fixture(t)
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		gh := refusing(t, status)
		prs, err := Open(context.Background(), gh, req, changes)
		var auth *AuthError
		if !errors.Is(err, ErrAuth) || !errors.As(err, &auth) {
			t.Fatalf("%d: want ErrAuth, got %v", status, err)
		}
		if auth.Status != status || auth.Op != OpCreateBranch {
			t.Errorf("%d: auth error carries %+v", status, auth)
		}
		if len(prs) != 0 {
			t.Errorf("%d: pull requests despite the refusal: %v", status, prs)
		}
		if strings.Contains(err.Error(), "the-persons-value") {
			t.Errorf("%d: the error carries the token: %v", status, err)
		}
		pr := PullRequest{Repository: repoA, Number: 1, HeadSHA: "abc"}
		if err := Merge(context.Background(), gh, pr, true); !errors.Is(err, ErrAuth) {
			t.Errorf("%d: merge: want ErrAuth, got %v", status, err)
		}
	}
}

func TestGitHubOtherStatusesAreOrdinaryErrors(t *testing.T) {
	_, req, changes := fixture(t)
	gh := refusing(t, http.StatusNotFound)
	_, err := Open(context.Background(), gh, req, changes)
	if err == nil || errors.Is(err, ErrAuth) {
		t.Errorf("404: want an ordinary error naming the operation, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), OpCreateBranch+": ") {
		t.Errorf("404: error does not name the operation: %v", err)
	}
}

func TestNewGitHubWithoutTokenIsErrAuth(t *testing.T) {
	if _, err := NewGitHub(""); !errors.Is(err, ErrAuth) {
		t.Errorf("want ErrAuth, got %v", err)
	}
}

// server is a GitHub API at srv.URL/api/v3 (what WithBaseURL makes of a test
// server) with GraphQL at srv.URL/api/graphql.
func server(t *testing.T) (*http.ServeMux, *GitHub) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	gh, err := NewGitHub("the-persons-value-never-logged", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	return mux, gh
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Error(err)
	}
}

func TestGitHubRefusedTokenOnEveryPullRequestSeam(t *testing.T) {
	gh := refusing(t, http.StatusForbidden)
	ctx := context.Background()
	pr := PullRequest{Repository: repoA, Number: 7, Head: featureBranch, HeadSHA: "abc"}
	_, _, findErr := gh.FindPullRequest(ctx, repoA, featureBranch)
	_, draftErr := gh.OpenDraftPullRequest(ctx, repoA, featureBranch, mainBranch, "t", "b")
	_, revertErr := gh.Revert(ctx, pr, "b", nil)
	for op, err := range map[string]error{
		OpFindPullRequest: findErr,
		OpOpenPullRequest: draftErr,
		OpEnableAutoMerge: gh.EnableAutoMerge(ctx, pr),
		OpClose:           gh.Close(ctx, pr, true),
		OpRevert:          revertErr,
	} {
		var auth *AuthError
		if !errors.As(err, &auth) || auth.Op != op || auth.Status != http.StatusForbidden {
			t.Errorf("%s: want AuthError for the operation, got %v", op, err)
		}
	}
}

func TestGitHubEnableAutoMergeSendsTheMutationWithTheRepositorysMethod(t *testing.T) {
	mux, gh := server(t)
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{numberKey: 7, "node_id": "PR_node"})
	})
	mux.HandleFunc("/api/v3/repos/acme/management-clusters", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"allow_squash_merge": false, "allow_merge_commit": false, "allow_rebase_merge": true})
	})
	var got struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	graphqlErrors := []map[string]any{}
	mux.HandleFunc("/api/graphql", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer the-persons-value-never-logged" {
			t.Errorf("graphql request: %s with authorization %q", r.Method, r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		writeJSON(t, w, map[string]any{"data": map[string]any{}, "errors": graphqlErrors})
	})
	pr := PullRequest{Repository: repoA, Number: 7}
	if err := gh.EnableAutoMerge(context.Background(), pr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Query, "enablePullRequestAutoMerge") || got.Variables["id"] != "PR_node" || got.Variables["method"] != "REBASE" {
		t.Errorf("mutation sent: %+v", got)
	}
	graphqlErrors = []map[string]any{{messageKey: "Pull request is not in the correct state"}}
	if err := gh.EnableAutoMerge(context.Background(), pr); err == nil || !strings.Contains(err.Error(), "correct state") {
		t.Errorf("GraphQL errors are not surfaced: %v", err)
	}
}

func TestGitHubCloseLeavesAClosedPullRequestAlone(t *testing.T) {
	mux, gh := server(t)
	edits := 0
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			edits++
		}
		writeJSON(t, w, map[string]any{numberKey: 7, "state": "closed", headKey: map[string]any{"ref": featureBranch}})
	})
	if err := gh.Close(context.Background(), PullRequest{Repository: repoA, Number: 7}, true); err != nil || edits != 0 {
		t.Errorf("closing a closed pull request: err=%v edits=%d", err, edits)
	}
}

func TestNewGitHubWithClientAddsNoCredentialOfItsOwn(t *testing.T) {
	if _, err := NewGitHubWithClient(nil); !errors.Is(err, ErrAuth) {
		t.Errorf("nil client: want ErrAuth, got %v", err)
	}
	var authorization string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/pulls", func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		writeJSON(t, w, []any{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: headerTransport{name: "Authorization", value: "Bearer minted-by-the-callers-transport"}}
	gh, err := NewGitHubWithClient(hc, WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := gh.FindPullRequest(context.Background(), repoA, featureBranch); err != nil || found {
		t.Fatalf("find: found=%v err=%v", found, err)
	}
	if authorization != "Bearer minted-by-the-callers-transport" {
		t.Errorf("the request carried %q, not the caller's identity", authorization)
	}
}

// headerTransport is the caller's authenticated transport: it sets one header.
type headerTransport struct{ name, value string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(h.name, h.value)
	return http.DefaultTransport.RoundTrip(r)
}

func TestGitHubApproveSubmitsAnApprovingReviewOnTheHead(t *testing.T) {
	mux, gh := server(t)
	var got map[string]any
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method %s", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(t, w, map[string]any{"id": 1, "state": "APPROVED"})
	})
	if err := gh.Approve(context.Background(), PullRequest{Repository: repoA, Number: 7, HeadSHA: "abc123"}, "approved by the team"); err != nil {
		t.Fatal(err)
	}
	if got["event"] != "APPROVE" || got["body"] != "approved by the team" || got["commit_id"] != "abc123" {
		t.Errorf("review request: %v", got)
	}
}

func TestGitHubApproveRefusedIsAnAuthError(t *testing.T) {
	mux, gh := server(t)
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		writeJSON(t, w, map[string]any{messageKey: "Resource not accessible by integration"})
	})
	err := gh.Approve(context.Background(), PullRequest{Repository: repoA, Number: 7}, "x")
	var auth *AuthError
	if !errors.As(err, &auth) || auth.Op != OpApprove || auth.Status != http.StatusForbidden {
		t.Errorf("want AuthError{%s, 403}, got %v", OpApprove, err)
	}
}

func TestGitHubCommitRemovesAPathWhoseContentIsNil(t *testing.T) {
	mux, gh := server(t)
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/git/ref/heads/remove-pool", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{refKey: "refs/heads/remove-pool", objectKey: map[string]any{shaKey: headSHA}})
	})
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/git/commits/"+headSHA, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{shaKey: headSHA, treeKey: map[string]any{shaKey: headTree}})
	})
	blobs := 0
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/git/blobs", func(w http.ResponseWriter, _ *http.Request) {
		blobs++
		writeJSON(t, w, map[string]any{shaKey: "blob1"})
	})
	var tree struct {
		BaseTree string           `json:"base_tree"`
		Tree     []map[string]any `json:"tree"`
	}
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/git/trees", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&tree); err != nil {
			t.Error(err)
		}
		writeJSON(t, w, map[string]any{shaKey: treeSHA})
	})
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/git/commits", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{shaKey: "commit1"})
	})
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/contents/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != headSHA {
			t.Errorf("contents read at %q, want the head %s", r.URL.Query().Get("ref"), headSHA)
		}
		if r.URL.Path != "/api/v3/repos/acme/management-clusters/contents/"+poolPath {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(t, w, map[string]any{messageKey: "Not Found"})
			return
		}
		writeJSON(t, w, map[string]any{typeKey: fileType, pathKey: poolPath})
	})
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/git/refs/heads/remove-pool", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{refKey: "refs/heads/remove-pool", objectKey: map[string]any{shaKey: "commit1"}})
	})
	err := gh.Commit(context.Background(), repoA, "remove-pool", "remove the pool", map[string][]byte{
		kustomizationPath: []byte("resources: []"),
		poolPath:          nil,
		"a/gone.yaml":     nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if blobs != 1 {
		t.Errorf("blobs created = %d, want 1 (none for the removed path)", blobs)
	}
	if len(tree.Tree) != 2 {
		t.Fatalf("tree entries = %v, want the content and the removal of the present path only", tree.Tree)
	}
	removed := tree.Tree[1]
	if removed["path"] != poolPath || removed[shaKey] != nil || removed["content"] != nil {
		t.Errorf("removed entry = %v, want path a/pool.yaml without sha or content", removed)
	}
	if _, ok := removed[shaKey]; !ok {
		t.Errorf("removed entry %v omits sha: GitHub keeps the file unless sha is null", removed)
	}
}

// TestGitHubCreateBranchBringsAnExistingBranchUpToDate: a new branch is one
// ref create and no merge; an existing one (422 on the create) is merged
// with its base through the branch merge — 201 the merge commit, 204 a head
// that contains the base already — and a 409 is ErrStaleBranch naming the
// branch and its open pull request.
func TestGitHubCreateBranchBringsAnExistingBranchUpToDate(t *testing.T) {
	mux, gh := server(t)
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/git/ref/heads/main", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{refKey: "refs/heads/main", objectKey: map[string]any{shaKey: baseHead}})
	})
	createStatus := http.StatusCreated
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/git/refs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(createStatus)
		if createStatus == http.StatusCreated {
			writeJSON(t, w, map[string]any{refKey: "refs/heads/feature", objectKey: map[string]any{shaKey: baseHead}})
			return
		}
		writeJSON(t, w, map[string]any{messageKey: "Reference already exists"})
	})
	mergeStatus, merges := http.StatusCreated, 0
	var merge map[string]any
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/merges", func(w http.ResponseWriter, r *http.Request) {
		merges++
		if err := json.NewDecoder(r.Body).Decode(&merge); err != nil {
			t.Error(err)
		}
		w.WriteHeader(mergeStatus)
		switch mergeStatus {
		case http.StatusCreated:
			writeJSON(t, w, map[string]any{shaKey: mergeCommit})
		case http.StatusConflict:
			writeJSON(t, w, map[string]any{messageKey: "Merge conflict"})
		}
	})
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get(headKey) != "acme:feature" || r.URL.Query().Get("state") != "open" {
			t.Errorf("pull request lookup: %s", r.URL.RawQuery)
		}
		writeJSON(t, w, []map[string]any{{numberKey: 7, "html_url": "https://github.example/acme/management-clusters/pull/7", headKey: map[string]any{refKey: featureBranch, shaKey: "old"}, baseKey: map[string]any{refKey: mainBranch}}})
	})
	ctx := context.Background()
	if err := gh.CreateBranch(ctx, repoA, featureBranch, mainBranch); err != nil || merges != 0 {
		t.Fatalf("new branch: err=%v merges=%d", err, merges)
	}
	createStatus = http.StatusUnprocessableEntity
	if err := gh.CreateBranch(ctx, repoA, featureBranch, mainBranch); err != nil {
		t.Fatalf("existing branch behind main: %v", err)
	}
	if merges != 1 || merge[baseKey] != featureBranch || merge[headKey] != mainBranch || merge["commit_message"] != "Merge branch 'main' into feature" {
		t.Errorf("merge request after %d merges: %v", merges, merge)
	}
	mergeStatus = http.StatusNoContent
	if err := gh.CreateBranch(ctx, repoA, featureBranch, mainBranch); err != nil {
		t.Errorf("existing branch that contains main: %v", err)
	}
	mergeStatus = http.StatusConflict
	err := gh.CreateBranch(ctx, repoA, featureBranch, mainBranch)
	if !errors.Is(err, ErrStaleBranch) || !strings.Contains(err.Error(), "acme/management-clusters@feature") || !strings.Contains(err.Error(), "/pull/7") {
		t.Errorf("conflict: want ErrStaleBranch naming the branch and its pull request, got %v", err)
	}
}

// TestGitHubRevertOntoAnExistingRevertBranch: the revert branch exists (422
// on the create), so the pull request's base is merged into it and the
// inverted files are committed on its head, a fast-forward; the open revert
// pull request is returned on the new head. A 409 on the merge is
// ErrStaleBranch naming the branch and the revert pull request, nothing
// committed.
func TestGitHubRevertOntoAnExistingRevertBranch(t *testing.T) {
	const (
		repoPath = "/api/v3/repos/acme/management-clusters"
		branch   = "revert-1-feature"
	)
	mux, gh := server(t)
	mux.HandleFunc("GET "+repoPath+"/pulls/1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{numberKey: 1, "title": "Change", "merged": true, "merge_commit_sha": mergeCommit,
			headKey: map[string]any{refKey: featureBranch}, baseKey: map[string]any{refKey: mainBranch}})
	})
	mux.HandleFunc("GET "+repoPath+"/git/commits/merge1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{shaKey: mergeCommit, "parents": []map[string]any{{shaKey: "parent1"}}})
	})
	mux.HandleFunc("GET "+repoPath+"/compare/parent1...merge1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"files": []map[string]any{{"filename": oneFile, "status": "added"}}})
	})
	mux.HandleFunc("GET "+repoPath+"/git/ref/heads/main", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{refKey: "refs/heads/main", objectKey: map[string]any{shaKey: baseHead}})
	})
	mux.HandleFunc("POST "+repoPath+"/git/refs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		writeJSON(t, w, map[string]any{messageKey: "Reference already exists"})
	})
	mergeStatus := http.StatusCreated
	var merge map[string]any
	mux.HandleFunc("POST "+repoPath+"/merges", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&merge); err != nil {
			t.Error(err)
		}
		w.WriteHeader(mergeStatus)
		writeJSON(t, w, map[string]any{shaKey: updatedHead})
	})
	mux.HandleFunc("GET "+repoPath+"/git/ref/heads/"+branch, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{refKey: "refs/heads/" + branch, objectKey: map[string]any{shaKey: updatedHead}})
	})
	mux.HandleFunc("GET "+repoPath+"/contents/"+oneFile, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{typeKey: fileType, pathKey: oneFile})
	})
	mux.HandleFunc("GET "+repoPath+"/git/commits/updated1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{shaKey: updatedHead, treeKey: map[string]any{shaKey: headTree}})
	})
	var tree struct {
		BaseTree string `json:"base_tree"`
	}
	mux.HandleFunc("POST "+repoPath+"/git/trees", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&tree); err != nil {
			t.Error(err)
		}
		writeJSON(t, w, map[string]any{shaKey: treeSHA})
	})
	var commit struct {
		Parents []string `json:"parents"`
	}
	mux.HandleFunc("POST "+repoPath+"/git/commits", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&commit); err != nil {
			t.Error(err)
		}
		writeJSON(t, w, map[string]any{shaKey: revertCommit})
	})
	var update map[string]any
	mux.HandleFunc("PATCH "+repoPath+"/git/refs/heads/"+branch, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			t.Error(err)
		}
		writeJSON(t, w, map[string]any{refKey: "refs/heads/" + branch, objectKey: map[string]any{shaKey: revertCommit}})
	})
	mux.HandleFunc("POST "+repoPath+"/pulls", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		writeJSON(t, w, map[string]any{messageKey: "A pull request already exists"})
	})
	mux.HandleFunc("GET "+repoPath+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get(headKey) != "acme:"+branch {
			t.Errorf("pull request lookup: %s", r.URL.RawQuery)
		}
		writeJSON(t, w, []map[string]any{{numberKey: 2, "html_url": "https://github.example/acme/management-clusters/pull/2",
			headKey: map[string]any{refKey: branch, shaKey: revertCommit}, baseKey: map[string]any{refKey: mainBranch}}})
	})
	ctx := context.Background()
	pr := PullRequest{Repository: repoA, Number: 1}
	revert, err := gh.Revert(ctx, pr, "again", nil)
	if err != nil {
		t.Fatal(err)
	}
	if merge[baseKey] != branch || merge[headKey] != mainBranch {
		t.Errorf("merge request: %v, want %s merged into %s", merge, mainBranch, branch)
	}
	if tree.BaseTree != headTree || len(commit.Parents) != 1 || commit.Parents[0] != updatedHead || update[shaKey] != revertCommit || update["force"] == true {
		t.Errorf("want the revert committed on the branch head updated1 and fast-forwarded: tree %+v, commit %+v, ref update %v", tree, commit, update)
	}
	if revert.Number != 2 || revert.Head != branch || revert.HeadSHA != revertCommit {
		t.Errorf("revert pull request: %+v", revert)
	}
	mergeStatus, update = http.StatusConflict, nil
	_, err = gh.Revert(ctx, pr, "again", nil)
	if !errors.Is(err, ErrStaleBranch) || !strings.HasPrefix(err.Error(), OpRevert+": ") || !strings.Contains(err.Error(), "acme/management-clusters@"+branch) || !strings.Contains(err.Error(), "/pull/2") {
		t.Errorf("conflict: want ErrStaleBranch naming the revert branch and its pull request, got %v", err)
	}
	if update != nil {
		t.Errorf("a refused revert moved the branch: %v", update)
	}
}

func TestGitHubReadFileReadsTheBlobAndAnswersNotFound(t *testing.T) {
	mux, gh := server(t)
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/contents/a/kustomization.yaml", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get(refKey) != mainBranch {
			t.Errorf("ref = %q", r.URL.Query().Get(refKey))
		}
		writeJSON(t, w, map[string]any{typeKey: fileType, shaKey: "blob9", pathKey: kustomizationPath})
	})
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/git/blobs/blob9", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("resources: [pool.yaml]"))
	})
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/contents/missing.yaml", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	ctx := context.Background()
	got, err := gh.ReadFile(ctx, repoA, mainBranch, kustomizationPath)
	if err != nil || string(got) != "resources: [pool.yaml]" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	if _, err := gh.ReadFile(ctx, repoA, mainBranch, "missing.yaml"); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("missing: want ErrFileNotFound, got %v", err)
	}
	if _, err := refusing(t, http.StatusUnauthorized).ReadFile(ctx, repoA, mainBranch, "x"); !errors.Is(err, ErrAuth) {
		t.Errorf("refused: want ErrAuth, got %v", err)
	}
}

func TestGitHubListFilesListsTheDirectoryFromTheRecursiveTree(t *testing.T) {
	mux, gh := server(t)
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/git/trees/main", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("recursive") == "" {
			t.Errorf("tree read without recursive")
		}
		writeJSON(t, w, map[string]any{shaKey: treeSHA, treeKey: []map[string]any{
			{pathKey: "a", typeKey: "tree"},
			{pathKey: "a/x.yaml", typeKey: blobType},
			{pathKey: "a/b", typeKey: "tree"},
			{pathKey: "a/b/y.yaml", typeKey: blobType},
			{pathKey: "ab/z.yaml", typeKey: blobType},
		}})
	})
	ctx := context.Background()
	got, err := gh.ListFiles(ctx, repoA, mainBranch, "a")
	if err != nil || strings.Join(got, ",") != "a/b/y.yaml,a/x.yaml" {
		t.Fatalf("ListFiles = %q, %v", got, err)
	}
	if got, err := gh.ListFiles(ctx, repoA, mainBranch, "missing"); err != nil || len(got) != 0 {
		t.Errorf("missing directory = %q, %v", got, err)
	}
	if _, err := refusing(t, http.StatusForbidden).ListFiles(ctx, repoA, mainBranch, "a"); !errors.Is(err, ErrAuth) {
		t.Errorf("refused: want ErrAuth, got %v", err)
	}
}

func TestGitHubListFilesRefusesATruncatedTree(t *testing.T) {
	mux, gh := server(t)
	mux.HandleFunc("/api/v3/repos/acme/management-clusters/git/trees/main", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{shaKey: treeSHA, "truncated": true, treeKey: []map[string]any{{pathKey: "a/x.yaml", typeKey: blobType}}})
	})
	if _, err := gh.ListFiles(context.Background(), repoA, mainBranch, "a"); err == nil {
		t.Fatal("truncated tree: want error")
	}
}

// treeSHA is the SHA of the tree the ListFiles fakes answer with, headTree
// the tree of a branch head the Commit and Revert fakes answer with.
const (
	treeSHA  = "tree1"
	headTree = "tree0"
)

// pathKey is a tree entry's or content's path in the GitHub API.
const pathKey = "path"

// treeKey and typeKey are a Git tree's entries and an entry's type in the GitHub API.
const (
	treeKey = "tree"
	typeKey = "type"
)

// numberKey, messageKey and objectKey are a pull request's number, an error's
// message and a ref's object in the GitHub API; headKey and baseKey a pull
// request's or a merge's head and base, fileType a content's type.
const (
	numberKey  = "number"
	messageKey = "message"
	objectKey  = "object"
	headKey    = "head"
	baseKey    = "base"
	fileType   = "file"
)

// baseHead, mergeCommit, updatedHead and revertCommit are the SHAs the
// Revert fakes answer with: the base's head, the merged pull request's merge
// commit, the revert branch's head after the update, the revert commit.
const (
	baseHead     = "base1"
	mergeCommit  = "merge1"
	updatedHead  = "updated1"
	revertCommit = "revert2"
)
