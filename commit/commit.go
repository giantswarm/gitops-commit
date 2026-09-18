// Package commit lands rendered files in GitOps repositories as the person: a
// branch, one commit per repository, the pull request carrying the caller's
// text, and the merge once the caller says approved and the checks are green.
//
// The module holds no token. A Remote is built from the token the caller
// supplies (for the managers: the person's user token of the manager's App),
// and a token GitHub refuses surfaces as ErrAuth carrying the HTTP status; the
// callers turn it into their sign-in answer. Nothing is retried with another
// token, nothing is logged, and no error carries file content.
package commit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/gitops-commit/provenance"
)

// Repository is the GitHub repository a change lands in.
type Repository = provenance.Repository

// Change is the set of files for one location: the repository, the base
// branch and the repository-relative paths the files land at (the keys of
// what sopsenc.Encrypt returns). Changes for one repository become one commit.
type Change struct {
	Location provenance.Location
	Files    map[string][]byte
}

// Request names one run: its head branch, its commit message and pull request
// title, and the pull request body — the caller's text, carried verbatim.
type Request struct {
	Branch string
	Title  string
	Body   string
}

// PullRequest identifies a pull request the module opened or found open.
// HeadSHA is the commit the caller saw; Merge refuses when the head has moved.
type PullRequest struct {
	Repository Repository
	Number     int
	URL        string
	Head       string
	Base       string
	HeadSHA    string
}

// ReviewState is the pull request's review rollup: the latest stance of every
// reviewer, approved when at least one approves and none requests changes.
type ReviewState string

// CheckState is the rollup of the commit statuses and check runs on the head.
type CheckState string

const (
	ReviewNone             ReviewState = "none"
	ReviewApproved         ReviewState = "approved"
	ReviewChangesRequested ReviewState = "changes_requested"

	// ChecksNone: the head carries no statuses and no check runs; nothing to wait for.
	ChecksNone    CheckState = "none"
	ChecksPending CheckState = "pending"
	ChecksSuccess CheckState = "success"
	ChecksFailure CheckState = "failure"
)

// Status is the pull request's state as the remote reports it.
type Status struct {
	HeadSHA string
	Merged  bool
	Review  ReviewState
	Checks  CheckState
}

// Remote is the git hosting the package talks to: GitHub in production, Fake
// in tests. Every operation acts with the token the Remote was built from.
type Remote interface {
	// CreateBranch creates branch from base; a branch that exists is left as it is.
	CreateBranch(ctx context.Context, repo Repository, branch, base string) error
	// Commit adds one commit with files to branch.
	Commit(ctx context.Context, repo Repository, branch, message string, files map[string][]byte) error
	// OpenPullRequest opens head against base, or returns the open pull request head already has.
	OpenPullRequest(ctx context.Context, repo Repository, head, base, title, body string) (PullRequest, error)
	// Status reads the pull request's head, merged flag, review and check state.
	Status(ctx context.Context, pr PullRequest) (Status, error)
	// Approve submits an approving review on the pull request as the person —
	// the identity the remote was built with. GitHub refuses a review by the
	// pull request's own author; callers check the author before the call.
	Approve(ctx context.Context, pr PullRequest, body string) error
	// Merge merges the pull request as the person, refusing when the head is not pr.HeadSHA.
	Merge(ctx context.Context, pr PullRequest) error
	// FindPullRequest returns the open pull request whose head is head, and whether there is one.
	FindPullRequest(ctx context.Context, repo Repository, head string) (PullRequest, bool, error)
	// OpenDraftPullRequest is OpenPullRequest with the pull request opened as a draft.
	OpenDraftPullRequest(ctx context.Context, repo Repository, head, base, title, body string) (PullRequest, error)
	// EnableAutoMerge arms the remote's auto-merge on the pull request with the
	// repository's merge method, so it lands once its reviews and checks pass.
	// The caller's choice, never the module's default.
	EnableAutoMerge(ctx context.Context, pr PullRequest) error
	// Close closes the pull request without merging and, when asked, deletes
	// its head branch; a closed pull request is left as it is.
	Close(ctx context.Context, pr PullRequest, deleteBranch bool) error
	// Revert opens a pull request undoing a merged one: the merged change's
	// files are inverted in one commit on the current tip of the base — added
	// files removed, changed and removed files restored — except the paths in
	// overrides, which take the given content. History is never rewritten.
	Revert(ctx context.Context, pr PullRequest, body string, overrides map[string][]byte) (PullRequest, error)
}

// Operation names of the remote calls, carried by AuthError and by Fake.Fail.
const (
	OpCreateBranch    = "create branch"
	OpCommit          = "commit"
	OpOpenPullRequest = "open pull request"
	OpStatus          = "pull request status"
	OpMerge           = "merge"
	OpApprove         = "approve pull request"
	OpFindPullRequest = "find pull request"
	OpEnableAutoMerge = "enable auto-merge"
	OpClose           = "close pull request"
	OpRevert          = "revert pull request"
)

var (
	// ErrAuth: the remote refused the caller's token (401 or 403). The callers
	// answer with their sign-in flow; the module neither retries nor substitutes.
	ErrAuth = errors.New("token refused")
	// ErrInvalidRequest: the request lacks a branch or a title.
	ErrInvalidRequest = errors.New("request needs a branch and a title")
	// ErrNoFiles: a change carries no files.
	ErrNoFiles = errors.New("no files to commit")
	// ErrConflictingBase: two changes for one repository name different branches.
	ErrConflictingBase = errors.New("one repository, two base branches")
	// ErrDuplicatePath: two changes for one repository write the same path.
	ErrDuplicatePath = errors.New("one path in two changes")
	// ErrNotApproved: the caller has not said approved.
	ErrNotApproved = errors.New("merge refused: not approved")
	// ErrChecksPending: a status or check run on the head has not finished.
	ErrChecksPending = errors.New("merge refused: checks pending")
	// ErrChecksFailed: a status or check run on the head failed.
	ErrChecksFailed = errors.New("merge refused: checks failed")
	// ErrHeadMoved: the head is no longer the commit the caller saw.
	ErrHeadMoved = errors.New("merge refused: the head moved since the pull request was seen")
	// ErrNotMerged: only a merged pull request can be reverted.
	ErrNotMerged = errors.New("revert refused: the pull request is not merged")
	// ErrNothingToRevert: the merged pull request changed no files.
	ErrNothingToRevert = errors.New("revert refused: the pull request changed no files")
)

// revertBranch, revertTitle and revertMessage name what Revert makes, the same
// on every Remote. The "revert:" prefix keeps the title past a semantic pull
// request check.
func revertBranch(pr PullRequest) string { return fmt.Sprintf("revert-%d-%s", pr.Number, pr.Head) }
func revertTitle(title string) string    { return fmt.Sprintf("revert: Revert %q", title) }
func revertMessage(pr PullRequest, title, body string) string {
	return fmt.Sprintf("Revert %q (#%d)\n\n%s", title, pr.Number, body)
}

// AuthError is ErrAuth with the status the remote answered and the operation
// it refused. errors.Is(err, ErrAuth) matches it.
type AuthError struct {
	Op     string
	Status int
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("%s: %v (github status %d)", e.Op, ErrAuth, e.Status)
}

// Unwrap makes errors.Is(err, ErrAuth) true.
func (e *AuthError) Unwrap() error { return ErrAuth }

// Open lands the changes: for every repository one branch from its base, one
// commit with all of that repository's files, one pull request with the
// caller's title and body. Repositories are handled in name order; on an
// error the pull requests opened so far are returned with it.
func Open(ctx context.Context, remote Remote, req Request, changes []Change) ([]PullRequest, error) {
	if req.Branch == "" || req.Title == "" {
		return nil, ErrInvalidRequest
	}
	groups, err := groupByRepository(changes)
	if err != nil {
		return nil, err
	}
	prs := make([]PullRequest, 0, len(groups))
	for _, g := range groups {
		if err := remote.CreateBranch(ctx, g.repo, req.Branch, g.base); err != nil {
			return prs, err
		}
		if err := remote.Commit(ctx, g.repo, req.Branch, req.Title, g.files); err != nil {
			return prs, err
		}
		pr, err := remote.OpenPullRequest(ctx, g.repo, req.Branch, g.base, req.Title, req.Body)
		if err != nil {
			return prs, err
		}
		prs = append(prs, pr)
	}
	return prs, nil
}

// Merge merges the pull request as the person. It is refused before the caller
// says approved (ErrNotApproved, no remote call), before every check on the
// head is green (ErrChecksPending, ErrChecksFailed; a head without checks has
// nothing to wait for) and when the head is not the commit the caller saw
// (ErrHeadMoved). A merged pull request is left as it is. GitHub's own rules —
// the approving review that satisfies enforce_admins and rulesets — apply at
// the merge call; no administrative path exists.
func Merge(ctx context.Context, remote Remote, pr PullRequest, approved bool) error {
	if !approved {
		return ErrNotApproved
	}
	st, err := remote.Status(ctx, pr)
	if err != nil {
		return err
	}
	if st.Merged {
		return nil
	}
	if st.HeadSHA != pr.HeadSHA {
		return fmt.Errorf("%w: head is %s, seen %s", ErrHeadMoved, st.HeadSHA, pr.HeadSHA)
	}
	switch st.Checks {
	case ChecksPending:
		return ErrChecksPending
	case ChecksFailure:
		return ErrChecksFailed
	}
	return remote.Merge(ctx, pr)
}

type group struct {
	repo  Repository
	base  string
	files map[string][]byte
}

// groupByRepository folds the changes into one commit per repository, in
// repository name order, refusing conflicting bases and colliding paths.
func groupByRepository(changes []Change) ([]group, error) {
	byRepo := map[Repository]*group{}
	for _, c := range changes {
		if len(c.Files) == 0 {
			return nil, fmt.Errorf("%w: %s", ErrNoFiles, c.Location.Repository)
		}
		g, ok := byRepo[c.Location.Repository]
		if !ok {
			g = &group{repo: c.Location.Repository, base: c.Location.Branch, files: map[string][]byte{}}
			byRepo[c.Location.Repository] = g
		}
		if g.base != c.Location.Branch {
			return nil, fmt.Errorf("%w: %s has %q and %q", ErrConflictingBase, g.repo, g.base, c.Location.Branch)
		}
		for path, content := range c.Files {
			if _, dup := g.files[path]; dup {
				return nil, fmt.Errorf("%w: %s %s", ErrDuplicatePath, g.repo, path)
			}
			g.files[path] = content
		}
	}
	groups := make([]group, 0, len(byRepo))
	for _, g := range byRepo {
		groups = append(groups, *g)
	}
	slices.SortFunc(groups, func(a, b group) int {
		return strings.Compare(a.repo.String(), b.repo.String())
	})
	return groups, nil
}
