package commit

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

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

// NewGitHubWithClient builds the Remote on an HTTP client that already
// carries the caller's identity — a GitHub App installation transport, a
// rotated token file — so no token string changes hands. The module adds no
// credential of its own to it.
func NewGitHubWithClient(hc *http.Client, opts ...GitHubOption) (*GitHub, error) {
	if hc == nil {
		return nil, &AuthError{Op: "new client", Status: http.StatusUnauthorized}
	}
	opts = append([]GitHubOption{github.WithHTTPClient(hc)}, opts...)
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
// head's tree, the commit, and a fast-forward of the branch. A nil content
// removes the path from the tree.
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
		if files[path] == nil {
			entries = append(entries, deletedEntry(path))
			continue
		}
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
	return g.openPullRequest(ctx, repo, head, base, title, body, false)
}

// OpenDraftPullRequest is OpenPullRequest as a draft.
func (g *GitHub) OpenDraftPullRequest(ctx context.Context, repo Repository, head, base, title, body string) (PullRequest, error) {
	return g.openPullRequest(ctx, repo, head, base, title, body, true)
}

func (g *GitHub) openPullRequest(ctx context.Context, repo Repository, head, base, title, body string, draft bool) (PullRequest, error) {
	pr, _, err := g.gh.PullRequests.Create(ctx, repo.Owner, repo.Name, &github.NewPullRequest{
		Title: github.Ptr(title),
		Head:  github.Ptr(head),
		Base:  github.Ptr(base),
		Body:  github.Ptr(body),
		Draft: github.Ptr(draft),
	})
	if err == nil {
		return toPullRequest(repo, pr), nil
	}
	if !hasStatus(err, http.StatusUnprocessableEntity) {
		return PullRequest{}, wrap(OpOpenPullRequest, err)
	}
	open, found, listErr := g.findPullRequest(ctx, repo, head)
	if listErr != nil {
		return PullRequest{}, wrap(OpOpenPullRequest, listErr)
	}
	if !found {
		return PullRequest{}, wrap(OpOpenPullRequest, err)
	}
	return open, nil
}

// FindPullRequest returns the open pull request of head, if there is one.
func (g *GitHub) FindPullRequest(ctx context.Context, repo Repository, head string) (PullRequest, bool, error) {
	pr, found, err := g.findPullRequest(ctx, repo, head)
	if err != nil {
		return PullRequest{}, false, wrap(OpFindPullRequest, err)
	}
	return pr, found, nil
}

func (g *GitHub) findPullRequest(ctx context.Context, repo Repository, head string) (PullRequest, bool, error) {
	open, _, err := g.gh.PullRequests.List(ctx, repo.Owner, repo.Name, &github.PullRequestListOptions{
		State:       "open",
		Head:        repo.Owner + ":" + head,
		ListOptions: github.ListOptions{PerPage: 1},
	})
	if err != nil {
		return PullRequest{}, false, err
	}
	if len(open) == 0 {
		return PullRequest{}, false, nil
	}
	return toPullRequest(repo, open[0]), true, nil
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
	method, err := g.mergeMethod(ctx, pr.Repository)
	if err != nil {
		return wrap(OpMerge, err)
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

// mergeMethod is the repository's merge method: squash where allowed, else
// merge commit, else rebase.
func (g *GitHub) mergeMethod(ctx context.Context, repo Repository) (string, error) {
	settings, _, err := g.gh.Repositories.Get(ctx, repo.Owner, repo.Name)
	if err != nil {
		return "", err
	}
	switch {
	case settings.GetAllowSquashMerge():
		return "squash", nil
	case settings.GetAllowMergeCommit():
		return "merge", nil
	case settings.GetAllowRebaseMerge():
		return "rebase", nil
	}
	return "", fmt.Errorf("%s allows no merge method", repo)
}

// EnableAutoMerge arms GitHub's auto-merge on the pull request with the
// repository's merge method. Auto-merge is GraphQL-only; the mutation goes
// through the same client and identity as every other call.
func (g *GitHub) EnableAutoMerge(ctx context.Context, pr PullRequest) error {
	got, _, err := g.gh.PullRequests.Get(ctx, pr.Repository.Owner, pr.Repository.Name, pr.Number)
	if err != nil {
		return wrap(OpEnableAutoMerge, err)
	}
	method, err := g.mergeMethod(ctx, pr.Repository)
	if err != nil {
		return wrap(OpEnableAutoMerge, err)
	}
	req, err := g.gh.NewRequest(ctx, http.MethodPost, graphqlURL(g.gh.BaseURL()), map[string]any{
		"query": `mutation($id: ID!, $method: PullRequestMergeMethod!) { enablePullRequestAutoMerge(input: {pullRequestId: $id, mergeMethod: $method}) { clientMutationId } }`,
		"variables": map[string]any{
			"id":     got.GetNodeID(),
			"method": strings.ToUpper(method),
		},
	})
	if err != nil {
		return wrap(OpEnableAutoMerge, err)
	}
	var result struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if _, err := g.gh.Do(req, &result); err != nil {
		return wrap(OpEnableAutoMerge, err)
	}
	if len(result.Errors) > 0 {
		return fmt.Errorf("%s: %s", OpEnableAutoMerge, result.Errors[0].Message)
	}
	return nil
}

// graphqlURL is the GraphQL endpoint next to the REST root: api.github.com/graphql
// for github.com, <host>/api/graphql for a GitHub Enterprise Server.
func graphqlURL(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	if u.Host == "api.github.com" {
		u.Path = "/graphql"
		return u.String()
	}
	u.Path = strings.TrimSuffix(u.Path, "api/v3/") + "api/graphql"
	return u.String()
}

// Close closes the pull request and, when asked, deletes its head branch. A
// pull request that is closed already (merged or not) is left as it is.
func (g *GitHub) Close(ctx context.Context, pr PullRequest, deleteBranch bool) error {
	got, _, err := g.gh.PullRequests.Get(ctx, pr.Repository.Owner, pr.Repository.Name, pr.Number)
	if err != nil {
		return wrap(OpClose, err)
	}
	if got.GetState() == "closed" {
		return nil
	}
	_, _, err = g.gh.PullRequests.Edit(ctx, pr.Repository.Owner, pr.Repository.Name, pr.Number, &github.PullRequest{State: github.Ptr("closed")})
	if err != nil {
		return wrap(OpClose, err)
	}
	if deleteBranch && got.GetHead().GetRef() != "" {
		if _, err := g.gh.Git.DeleteRef(ctx, pr.Repository.Owner, pr.Repository.Name, headsPrefix+got.GetHead().GetRef()); err != nil {
			return wrap(OpClose, err)
		}
	}
	return nil
}

// Revert opens a pull request undoing the merged pr: the merge commit's diff
// against its first parent, inverted into one commit on the tip of the
// repository's default branch, on branch revert-<number>-<head>.
func (g *GitHub) Revert(ctx context.Context, pr PullRequest, body string, overrides map[string][]byte) (PullRequest, error) {
	owner, name := pr.Repository.Owner, pr.Repository.Name
	got, _, err := g.gh.PullRequests.Get(ctx, owner, name, pr.Number)
	if err != nil {
		return PullRequest{}, wrap(OpRevert, err)
	}
	if !got.GetMerged() {
		return PullRequest{}, ErrNotMerged
	}
	mergeSHA := got.GetMergeCommitSHA()
	merge, _, err := g.gh.Git.GetCommit(ctx, owner, name, mergeSHA)
	if err != nil {
		return PullRequest{}, wrap(OpRevert, err)
	}
	if len(merge.Parents) == 0 {
		return PullRequest{}, fmt.Errorf("%s: merge commit %s has no parent", OpRevert, mergeSHA)
	}
	parentSHA := merge.Parents[0].GetSHA()
	compare, _, err := g.gh.Repositories.CompareCommits(ctx, owner, name, parentSHA, mergeSHA, nil)
	if err != nil {
		return PullRequest{}, wrap(OpRevert, err)
	}
	if len(compare.Files) == 0 {
		return PullRequest{}, ErrNothingToRevert
	}
	settings, _, err := g.gh.Repositories.Get(ctx, owner, name)
	if err != nil {
		return PullRequest{}, wrap(OpRevert, err)
	}
	base := settings.GetDefaultBranch()
	tipRef, _, err := g.gh.Git.GetRef(ctx, owner, name, headsPrefix+base)
	if err != nil {
		return PullRequest{}, wrap(OpRevert, err)
	}
	tipSHA := tipRef.GetObject().GetSHA()
	tip, _, err := g.gh.Git.GetCommit(ctx, owner, name, tipSHA)
	if err != nil {
		return PullRequest{}, wrap(OpRevert, err)
	}
	entries, err := g.revertEntries(ctx, pr.Repository, parentSHA, compare.Files, overrides)
	if err != nil {
		return PullRequest{}, err
	}
	tree, _, err := g.gh.Git.CreateTree(ctx, owner, name, tip.GetTree().GetSHA(), entries)
	if err != nil {
		return PullRequest{}, wrap(OpRevert, err)
	}
	pr.Head = got.GetHead().GetRef()
	commit, _, err := g.gh.Git.CreateCommit(ctx, owner, name, github.Commit{
		Message: github.Ptr(revertMessage(pr, got.GetTitle(), body)),
		Tree:    &github.Tree{SHA: github.Ptr(tree.GetSHA())},
		Parents: []*github.Commit{{SHA: github.Ptr(tipSHA)}},
	}, nil)
	if err != nil {
		return PullRequest{}, wrap(OpRevert, err)
	}
	branch := revertBranch(pr)
	_, _, err = g.gh.Git.CreateRef(ctx, owner, name, github.CreateRef{Ref: refsHeadPrefix + branch, SHA: commit.GetSHA()})
	if err != nil && !hasStatus(err, http.StatusUnprocessableEntity) {
		return PullRequest{}, wrap(OpRevert, err)
	}
	return g.openPullRequest(ctx, pr.Repository, branch, base, revertTitle(got.GetTitle()), body, false)
}

// revertEntries inverts a compare's file list: an added file is deleted, a
// changed or removed file restored from parentSHA, a rename undone on both
// names. A path in overrides takes that content instead.
func (g *GitHub) revertEntries(ctx context.Context, repo Repository, parentSHA string, files []*github.CommitFile, overrides map[string][]byte) ([]*github.TreeEntry, error) {
	entries := make([]*github.TreeEntry, 0, len(files))
	for _, f := range files {
		path := f.GetFilename()
		if content, ok := overrides[path]; ok {
			entry, err := g.blobEntry(ctx, repo, path, content)
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry)
			continue
		}
		switch f.GetStatus() {
		case "added":
			entries = append(entries, deletedEntry(path))
		case "removed", "modified", "changed":
			entry, err := g.restoredEntry(ctx, repo, parentSHA, path)
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry)
		case "renamed":
			entry, err := g.restoredEntry(ctx, repo, parentSHA, f.GetPreviousFilename())
			if err != nil {
				return nil, err
			}
			entries = append(entries, deletedEntry(path), entry)
		default:
			return nil, fmt.Errorf("%s: %s has status %q", OpRevert, path, f.GetStatus())
		}
	}
	return entries, nil
}

func (g *GitHub) restoredEntry(ctx context.Context, repo Repository, sha, path string) (*github.TreeEntry, error) {
	file, _, _, err := g.gh.Repositories.GetContents(ctx, repo.Owner, repo.Name, path, &github.RepositoryContentGetOptions{Ref: sha})
	if err != nil {
		return nil, wrap(OpRevert, err)
	}
	content, err := file.GetContent()
	if err != nil {
		return nil, fmt.Errorf("%s: %s at %s: %w", OpRevert, path, sha, err)
	}
	return g.blobEntry(ctx, repo, path, []byte(content))
}

func (g *GitHub) blobEntry(ctx context.Context, repo Repository, path string, content []byte) (*github.TreeEntry, error) {
	blob, _, err := g.gh.Git.CreateBlob(ctx, repo.Owner, repo.Name, github.Blob{
		Content:  github.Ptr(base64.StdEncoding.EncodeToString(content)),
		Encoding: github.Ptr("base64"),
	})
	if err != nil {
		return nil, wrap(OpRevert, err)
	}
	return &github.TreeEntry{Path: github.Ptr(path), Mode: github.Ptr(blobMode), Type: github.Ptr(blobType), SHA: github.Ptr(blob.GetSHA())}, nil
}

// ReadFile returns the content of path at the head of branch, through the
// Git blob so a file past the contents API's 1 MB inline limit reads whole.
func (g *GitHub) ReadFile(ctx context.Context, repo Repository, branch, path string) ([]byte, error) {
	file, _, _, err := g.gh.Repositories.GetContents(ctx, repo.Owner, repo.Name, path, &github.RepositoryContentGetOptions{Ref: branch})
	if hasStatus(err, http.StatusNotFound) {
		return nil, fmt.Errorf("%s %s/%s@%s: %w", OpReadFile, repo, path, branch, ErrFileNotFound)
	}
	if err != nil {
		return nil, wrap(OpReadFile, err)
	}
	if file == nil || file.GetType() != "file" {
		return nil, fmt.Errorf("%s %s/%s@%s: not a file: %w", OpReadFile, repo, path, branch, ErrFileNotFound)
	}
	raw, _, err := g.gh.Git.GetBlobRaw(ctx, repo.Owner, repo.Name, file.GetSHA())
	if err != nil {
		return nil, wrap(OpReadFile, err)
	}
	return raw, nil
}

// deletedEntry removes path from the tree: an entry without sha and content.
func deletedEntry(path string) *github.TreeEntry {
	return &github.TreeEntry{Path: github.Ptr(path), Mode: github.Ptr(blobMode), Type: github.Ptr(blobType)}
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

// Approve submits an approving review on the pull request as the person,
// pinned to pr.HeadSHA when the caller names it. GitHub applies its own
// rules to the call: the author of a pull request cannot approve it.
func (g *GitHub) Approve(ctx context.Context, pr PullRequest, body string) error {
	review := &github.PullRequestReviewRequest{Event: github.Ptr("APPROVE"), Body: github.Ptr(body)}
	if pr.HeadSHA != "" {
		review.CommitID = github.Ptr(pr.HeadSHA)
	}
	if _, _, err := g.gh.PullRequests.CreateReview(ctx, pr.Repository.Owner, pr.Repository.Name, pr.Number, review); err != nil {
		return wrap(OpApprove, err)
	}
	return nil
}
