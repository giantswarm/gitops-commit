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
	pr := PullRequest{Repository: repoA, Number: 7, Head: "feature", HeadSHA: "abc"}
	_, _, findErr := gh.FindPullRequest(ctx, repoA, "feature")
	_, draftErr := gh.OpenDraftPullRequest(ctx, repoA, "feature", "main", "t", "b")
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
		writeJSON(t, w, map[string]any{"number": 7, "node_id": "PR_node"})
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
	graphqlErrors = []map[string]any{{"message": "Pull request is not in the correct state"}}
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
		writeJSON(t, w, map[string]any{"number": 7, "state": "closed", "head": map[string]any{"ref": "feature"}})
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
	if _, found, err := gh.FindPullRequest(context.Background(), repoA, "feature"); err != nil || found {
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
