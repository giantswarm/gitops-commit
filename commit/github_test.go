package commit

import (
	"context"
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
