package commit

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/google/go-github/v88/github"
)

const (
	headsPrefix    = "heads/"
	refsHeadPrefix = "refs/heads/"
	blobMode       = "100644"
	blobType       = "blob"
	pageSize       = 100
)

// GitHub is the Remote for github.com (or a GitHub Enterprise Server) acting
// with the token the caller supplies. It talks REST through google/go-github,
// never a git or sops binary.
type GitHub struct {
	gh *github.Client
}

// GitHubOption configures NewGitHub.
type GitHubOption = github.ClientOptionsFunc

// WithBaseURL points the client at another API root: a GitHub Enterprise
// Server, or a test server.
func WithBaseURL(baseURL string) GitHubOption {
	return github.WithEnterpriseURLs(baseURL, baseURL)
}

// WithHTTPClient sets the HTTP client the requests go through.
func WithHTTPClient(c *http.Client) GitHubOption {
	return github.WithHTTPClient(c)
}

// NewGitHub builds the Remote from the caller's token. The token is the
// person's; the module keeps none of its own.
func NewGitHub(token string, opts ...GitHubOption) (*GitHub, error) {
	if token == "" {
		return nil, &AuthError{Op: "new client", Status: http.StatusUnauthorized}
	}
	opts = append([]GitHubOption{github.WithAuthToken(token)}, opts...)
	gh, err := github.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("github client: %w", err)
	}
	return &GitHub{gh: gh}, nil
}

// CreateBranch creates branch at base's head; an existing branch is kept.
func (g *GitHub) CreateBranch(ctx context.Context, repo Repository, branch, base string) error {
	baseRef, _, err := g.gh.Git.GetRef(ctx, repo.Owner, repo.Name, headsPrefix+base)
	if err != nil {
		return wrap(OpCreateBranch, err)
	}
	_, _, err = g.gh.Git.CreateRef(ctx, repo.Owner, repo.Name, github.CreateRef{
		Ref: refsHeadPrefix + branch,
		SHA: baseRef.GetObject().GetSHA(),
	})
	if err != nil && !hasStatus(err, http.StatusUnprocessableEntity) {
		return wrap(OpCreateBranch, err)
	}
	return nil
}

// Commit writes one commit with files on top of branch: blobs, a tree on the
// head's tree, the commit, and a fast-forward of the branch.
func (g *GitHub) Commit(ctx context.Context, repo Repository, branch, message string, files map[string][]byte) error {
	if len(files) == 0 {
		return ErrNoFiles
	}
	ref, _, err := g.gh.Git.GetRef(ctx, repo.Owner, repo.Name, headsPrefix+branch)
	if err != nil {
		return wrap(OpCommit, err)
	}
	headSHA := ref.GetObject().GetSHA()
	head, _, err := g.gh.Git.GetCommit(ctx, repo.Owner, repo.Name, headSHA)
	if err != nil {
		return wrap(OpCommit, err)
	}
	paths := slices.Sorted(maps.Keys(files))
	entries := make([]*github.TreeEntry, 0, len(paths))
	for _, path := range paths {
		blob, _, err := g.gh.Git.CreateBlob(ctx, repo.Owner, repo.Name, github.Blob{
			Content:  github.Ptr(base64.StdEncoding.EncodeToString(files[path])),
			Encoding: github.Ptr("base64"),
		})
		if err != nil {
			return wrap(OpCommit, err)
		}
		entries = append(entries, &github.TreeEntry{
			Path: github.Ptr(path),
			Mode: github.Ptr(blobMode),
			Type: github.Ptr(blobType),
			SHA:  github.Ptr(blob.GetSHA()),
		})
	}
	tree, _, err := g.gh.Git.CreateTree(ctx, repo.Owner, repo.Name, head.GetTree().GetSHA(), entries)
	if err != nil {
		return wrap(OpCommit, err)
	}
	commit, _, err := g.gh.Git.CreateCommit(ctx, repo.Owner, repo.Name, github.Commit{
		Message: github.Ptr(message),
		Tree:    &github.Tree{SHA: github.Ptr(tree.GetSHA())},
		Parents: []*github.Commit{{SHA: github.Ptr(headSHA)}},
	}, nil)
	if err != nil {
		return wrap(OpCommit, err)
	}
	_, _, err = g.gh.Git.UpdateRef(ctx, repo.Owner, repo.Name, refsHeadPrefix+branch, github.UpdateRef{SHA: commit.GetSHA()})
	if err != nil {
		return wrap(OpCommit, err)
	}
	return nil
}

// OpenPullRequest opens head against base with the caller's title and body.
// When head already has an open pull request, that one is returned.
func (g *GitHub) OpenPullRequest(ctx context.Context, repo Repository, head, base, title, body string) (PullRequest, error) {
	pr, _, err := g.gh.PullRequests.Create(ctx, repo.Owner, repo.Name, &github.NewPullRequest{
		Title: github.Ptr(title),
		Head:  github.Ptr(head),
		Base:  github.Ptr(base),
		Body:  github.Ptr(body),
	})
	if err == nil {
		return toPullRequest(repo, pr), nil
	}
	if !hasStatus(err, http.StatusUnprocessableEntity) {
		return PullRequest{}, wrap(OpOpenPullRequest, err)
	}
	open, _, listErr := g.gh.PullRequests.List(ctx, repo.Owner, repo.Name, &github.PullRequestListOptions{
		State:       "open",
		Head:        repo.Owner + ":" + head,
		ListOptions: github.ListOptions{PerPage: 1},
	})
	if listErr != nil {
		return PullRequest{}, wrap(OpOpenPullRequest, listErr)
	}
	if len(open) == 0 {
		return PullRequest{}, wrap(OpOpenPullRequest, err)
	}
	return toPullRequest(repo, open[0]), nil
}

// Status reads the pull request, its reviews, and the statuses and check runs
// of its head.
func (g *GitHub) Status(ctx context.Context, pr PullRequest) (Status, error) {
	got, _, err := g.gh.PullRequests.Get(ctx, pr.Repository.Owner, pr.Repository.Name, pr.Number)
	if err != nil {
		return Status{}, wrap(OpStatus, err)
	}
	st := Status{HeadSHA: got.GetHead().GetSHA(), Merged: got.GetMerged()}
	if st.Review, err = g.reviewState(ctx, pr); err != nil {
		return Status{}, err
	}
	if st.Checks, err = g.checkState(ctx, pr.Repository, st.HeadSHA); err != nil {
		return Status{}, err
	}
	return st, nil
}

// Merge merges the pull request with the repository's merge method (squash
// where allowed, else merge commit, else rebase), only if the head is still
// pr.HeadSHA. GitHub applies its own protection rules to the call.
func (g *GitHub) Merge(ctx context.Context, pr PullRequest) error {
	settings, _, err := g.gh.Repositories.Get(ctx, pr.Repository.Owner, pr.Repository.Name)
	if err != nil {
		return wrap(OpMerge, err)
	}
	var method string
	switch {
	case settings.GetAllowSquashMerge():
		method = "squash"
	case settings.GetAllowMergeCommit():
		method = "merge"
	case settings.GetAllowRebaseMerge():
		method = "rebase"
	default:
		return fmt.Errorf("%s: %s allows no merge method", OpMerge, pr.Repository)
	}
	_, _, err = g.gh.PullRequests.Merge(ctx, pr.Repository.Owner, pr.Repository.Name, pr.Number, "", &github.PullRequestOptions{
		MergeMethod: method,
		SHA:         pr.HeadSHA,
	})
	if err != nil {
		return wrap(OpMerge, err)
	}
	return nil
}

// reviewState folds the reviews into the latest stance per reviewer.
// Comments do not change a stance; a dismissal clears it.
func (g *GitHub) reviewState(ctx context.Context, pr PullRequest) (ReviewState, error) {
	latest := map[string]string{}
	opts := &github.ListOptions{PerPage: pageSize}
	for {
		reviews, resp, err := g.gh.PullRequests.ListReviews(ctx, pr.Repository.Owner, pr.Repository.Name, pr.Number, opts)
		if err != nil {
			return "", wrap(OpStatus, err)
		}
		for _, r := range reviews {
			switch state := r.GetState(); state {
			case "APPROVED", "CHANGES_REQUESTED":
				latest[r.GetUser().GetLogin()] = state
			case "DISMISSED":
				delete(latest, r.GetUser().GetLogin())
			}
		}
		if resp.NextPage == 0 {
			return rollupReviews(latest), nil
		}
		opts.Page = resp.NextPage
	}
}

func rollupReviews(latest map[string]string) ReviewState {
	state := ReviewNone
	for _, s := range latest {
		if s == "CHANGES_REQUESTED" {
			return ReviewChangesRequested
		}
		state = ReviewApproved
	}
	return state
}

// checkState folds the combined commit status and every check run of the
// head into one state: a failure anywhere is failure, anything unfinished is
// pending, nothing at all is none.
func (g *GitHub) checkState(ctx context.Context, repo Repository, sha string) (CheckState, error) {
	combined, _, err := g.gh.Repositories.GetCombinedStatus(ctx, repo.Owner, repo.Name, sha, &github.ListOptions{PerPage: pageSize})
	if err != nil {
		return "", wrap(OpStatus, err)
	}
	total := combined.GetTotalCount()
	failed := total > 0 && combined.GetState() == "failure"
	pending := total > 0 && combined.GetState() == "pending"
	opts := &github.ListCheckRunsOptions{ListOptions: github.ListOptions{PerPage: pageSize}}
	for {
		runs, resp, err := g.gh.Checks.ListCheckRunsForRef(ctx, repo.Owner, repo.Name, sha, opts)
		if err != nil {
			return "", wrap(OpStatus, err)
		}
		total += len(runs.CheckRuns)
		for _, run := range runs.CheckRuns {
			if run.GetStatus() != "completed" {
				pending = true
				continue
			}
			switch run.GetConclusion() {
			case "success", "neutral", "skipped":
			default:
				failed = true
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	switch {
	case total == 0:
		return ChecksNone, nil
	case failed:
		return ChecksFailure, nil
	case pending:
		return ChecksPending, nil
	}
	return ChecksSuccess, nil
}

func toPullRequest(repo Repository, pr *github.PullRequest) PullRequest {
	return PullRequest{
		Repository: repo,
		Number:     pr.GetNumber(),
		URL:        pr.GetHTMLURL(),
		Head:       pr.GetHead().GetRef(),
		Base:       pr.GetBase().GetRef(),
		HeadSHA:    pr.GetHead().GetSHA(),
	}
}

// wrap names the operation on a GitHub error and turns a refused token —
// 401 or 403 — into AuthError. Rate limits are their own go-github types and
// stay ordinary errors.
func wrap(op string, err error) error {
	var ghErr *github.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil {
		switch code := ghErr.Response.StatusCode; code {
		case http.StatusUnauthorized, http.StatusForbidden:
			return &AuthError{Op: op, Status: code}
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}

func hasStatus(err error, code int) bool {
	var ghErr *github.ErrorResponse
	return errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == code
}
