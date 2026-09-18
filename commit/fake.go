package commit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sync"
)

// Fake is an in-process Remote for tests — the module's own and its callers':
// branches with commits, pull requests with review and check state, and a way
// to make any operation fail. It never touches the network.
type Fake struct {
	// Fail maps an operation name (OpCreateBranch, OpCommit, ...) to the error
	// that operation returns instead of acting.
	Fail map[string]error

	mu      sync.Mutex
	heads   map[string]string     // repo#branch → head sha
	commits map[string]FakeCommit // sha → commit
	prs     []*FakePullRequest
}

// FakeCommit is one commit the Fake holds; Files is the full tree at that commit.
type FakeCommit struct {
	Repository Repository
	Branch     string
	Message    string
	Parent     string
	SHA        string
	Files      map[string][]byte
}

// FakePullRequest is a pull request the Fake holds, with what the remote knows about it.
type FakePullRequest struct {
	PullRequest
	Title     string
	Body      string
	Draft     bool
	Merged    bool
	Closed    bool // closed without a merge
	AutoMerge bool
	Review    ReviewState
	// Approvals are the bodies of the approving reviews Approve submitted, in order.
	Approvals []string
	Checks    CheckState

	mergeBase string // head of the base when the pull request was merged
}

// open is what FindPullRequest and OpenPullRequest's reuse look for.
func (p *FakePullRequest) open() bool { return !p.Merged && !p.Closed }

// NewFake returns an empty Fake; seed base branches with AddBranch.
func NewFake() *Fake {
	return &Fake{Fail: map[string]error{}, heads: map[string]string{}, commits: map[string]FakeCommit{}}
}

// AddBranch seeds branch in repo with one commit holding files (nil for an empty tree).
func (f *Fake) AddBranch(repo Repository, branch string, files map[string][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.write(repo, branch, "seed", "", files)
}

// Commits returns the commits made on branch after its seed, oldest first.
func (f *Fake) Commits(repo Repository, branch string) []FakeCommit {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []FakeCommit
	for sha := f.heads[key(repo, branch)]; sha != ""; {
		c := f.commits[sha]
		if c.Parent == "" {
			break
		}
		out = append(out, c)
		sha = c.Parent
	}
	slices.Reverse(out)
	return out
}

// Files returns the tree at the head of branch.
func (f *Fake) Files(repo Repository, branch string) map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.commits[f.heads[key(repo, branch)]].Files)
}

// PullRequests returns every pull request the Fake holds, oldest first.
func (f *Fake) PullRequests() []FakePullRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakePullRequest, 0, len(f.prs))
	for _, pr := range f.prs {
		out = append(out, *pr)
	}
	return out
}

// SetReview sets the review rollup the Fake reports for pr.
func (f *Fake) SetReview(pr PullRequest, state ReviewState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.find(pr); p != nil {
		p.Review = state
	}
}

// SetChecks sets the check rollup the Fake reports for pr.
func (f *Fake) SetChecks(pr PullRequest, state CheckState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.find(pr); p != nil {
		p.Checks = state
	}
}

// CreateBranch implements Remote.
func (f *Fake) CreateBranch(_ context.Context, repo Repository, branch, base string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.Fail[OpCreateBranch]; err != nil {
		return err
	}
	baseSHA, ok := f.heads[key(repo, base)]
	if !ok {
		return fmt.Errorf("%s: %s has no branch %q", OpCreateBranch, repo, base)
	}
	if _, exists := f.heads[key(repo, branch)]; !exists {
		f.heads[key(repo, branch)] = baseSHA
	}
	return nil
}

// Commit implements Remote.
func (f *Fake) Commit(_ context.Context, repo Repository, branch, message string, files map[string][]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.Fail[OpCommit]; err != nil {
		return err
	}
	if len(files) == 0 {
		return ErrNoFiles
	}
	parent, ok := f.heads[key(repo, branch)]
	if !ok {
		return fmt.Errorf("%s: %s has no branch %q", OpCommit, repo, branch)
	}
	tree := maps.Clone(f.commits[parent].Files)
	if tree == nil {
		tree = map[string][]byte{}
	}
	maps.Copy(tree, files)
	f.write(repo, branch, message, parent, tree)
	return nil
}

// OpenPullRequest implements Remote.
func (f *Fake) OpenPullRequest(_ context.Context, repo Repository, head, base, title, body string) (PullRequest, error) {
	return f.openPullRequest(repo, head, base, title, body, false)
}

// OpenDraftPullRequest implements Remote.
func (f *Fake) OpenDraftPullRequest(_ context.Context, repo Repository, head, base, title, body string) (PullRequest, error) {
	return f.openPullRequest(repo, head, base, title, body, true)
}

func (f *Fake) openPullRequest(repo Repository, head, base, title, body string, draft bool) (PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.openLocked(repo, head, base, title, body, draft)
}

// openLocked is openPullRequest for a caller that holds the lock.
func (f *Fake) openLocked(repo Repository, head, base, title, body string, draft bool) (PullRequest, error) {
	if err := f.Fail[OpOpenPullRequest]; err != nil {
		return PullRequest{}, err
	}
	headSHA, ok := f.heads[key(repo, head)]
	if !ok {
		return PullRequest{}, fmt.Errorf("%s: %s has no branch %q", OpOpenPullRequest, repo, head)
	}
	if pr := f.findOpen(repo, head); pr != nil {
		pr.HeadSHA = headSHA
		return pr.PullRequest, nil
	}
	pr := &FakePullRequest{
		PullRequest: PullRequest{
			Repository: repo,
			Number:     len(f.prs) + 1,
			URL:        fmt.Sprintf("https://github.example/%s/pull/%d", repo, len(f.prs)+1),
			Head:       head,
			Base:       base,
			HeadSHA:    headSHA,
		},
		Title:  title,
		Body:   body,
		Draft:  draft,
		Review: ReviewNone,
		Checks: ChecksNone,
	}
	f.prs = append(f.prs, pr)
	return pr.PullRequest, nil
}

// FindPullRequest implements Remote.
func (f *Fake) FindPullRequest(_ context.Context, repo Repository, head string) (PullRequest, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.Fail[OpFindPullRequest]; err != nil {
		return PullRequest{}, false, err
	}
	pr := f.findOpen(repo, head)
	if pr == nil {
		return PullRequest{}, false, nil
	}
	pr.HeadSHA = f.heads[key(repo, head)]
	return pr.PullRequest, true, nil
}

// EnableAutoMerge implements Remote: the pull request is marked, nothing merges by itself.
func (f *Fake) EnableAutoMerge(_ context.Context, pr PullRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.Fail[OpEnableAutoMerge]; err != nil {
		return err
	}
	p := f.find(pr)
	if p == nil {
		return fmt.Errorf("%s: %s has no pull request %d", OpEnableAutoMerge, pr.Repository, pr.Number)
	}
	if !p.open() {
		return fmt.Errorf("%s: pull request %d is not open", OpEnableAutoMerge, pr.Number)
	}
	p.AutoMerge = true
	return nil
}

// Close implements Remote.
func (f *Fake) Close(_ context.Context, pr PullRequest, deleteBranch bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.Fail[OpClose]; err != nil {
		return err
	}
	p := f.find(pr)
	if p == nil {
		return fmt.Errorf("%s: %s has no pull request %d", OpClose, pr.Repository, pr.Number)
	}
	if !p.open() {
		return nil
	}
	p.Closed = true
	if deleteBranch {
		delete(f.heads, key(p.Repository, p.Head))
	}
	return nil
}

// Revert implements Remote: the merged pull request's files, inverted against
// the base as it was at the merge, in one commit on the base's tip.
func (f *Fake) Revert(_ context.Context, pr PullRequest, body string, overrides map[string][]byte) (PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.Fail[OpRevert]; err != nil {
		return PullRequest{}, err
	}
	p := f.find(pr)
	if p == nil {
		return PullRequest{}, fmt.Errorf("%s: %s has no pull request %d", OpRevert, pr.Repository, pr.Number)
	}
	if !p.Merged {
		return PullRequest{}, ErrNotMerged
	}
	before, after := f.commits[p.mergeBase].Files, f.commits[p.HeadSHA].Files
	changed := map[string]bool{}
	for path, content := range after {
		if was, ok := before[path]; !ok || string(was) != string(content) {
			changed[path] = true
		}
	}
	for path := range before {
		if _, kept := after[path]; !kept {
			changed[path] = true
		}
	}
	if len(changed) == 0 {
		return PullRequest{}, ErrNothingToRevert
	}
	tipSHA := f.heads[key(p.Repository, p.Base)]
	tree := maps.Clone(f.commits[tipSHA].Files)
	if tree == nil {
		tree = map[string][]byte{}
	}
	for path := range changed {
		switch content, was := before[path]; {
		case overrides[path] != nil:
			tree[path] = overrides[path]
		case was:
			tree[path] = content
		default:
			delete(tree, path)
		}
	}
	branch := revertBranch(p.PullRequest)
	if _, exists := f.heads[key(p.Repository, branch)]; !exists {
		f.heads[key(p.Repository, branch)] = tipSHA
	}
	f.write(p.Repository, branch, revertMessage(p.PullRequest, p.Title, body), tipSHA, tree)
	return f.openLocked(p.Repository, branch, p.Base, revertTitle(p.Title), body, false)
}

// Status implements Remote.
func (f *Fake) Status(_ context.Context, pr PullRequest) (Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.Fail[OpStatus]; err != nil {
		return Status{}, err
	}
	p := f.find(pr)
	if p == nil {
		return Status{}, fmt.Errorf("%s: %s has no pull request %d", OpStatus, pr.Repository, pr.Number)
	}
	return Status{HeadSHA: f.heads[key(p.Repository, p.Head)], Merged: p.Merged, Review: p.Review, Checks: p.Checks}, nil
}

// Merge implements Remote: the base fast-forwards to the head.
func (f *Fake) Merge(_ context.Context, pr PullRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.Fail[OpMerge]; err != nil {
		return err
	}
	p := f.find(pr)
	if p == nil {
		return fmt.Errorf("%s: %s has no pull request %d", OpMerge, pr.Repository, pr.Number)
	}
	if p.Closed {
		return fmt.Errorf("%s: pull request %d is closed", OpMerge, pr.Number)
	}
	if head := f.heads[key(p.Repository, p.Head)]; head != pr.HeadSHA {
		return fmt.Errorf("%s: head is %s, not %s", OpMerge, head, pr.HeadSHA)
	}
	p.Merged = true
	p.HeadSHA = pr.HeadSHA
	p.mergeBase = f.heads[key(p.Repository, p.Base)]
	f.heads[key(p.Repository, p.Base)] = pr.HeadSHA
	return nil
}

func (f *Fake) findOpen(repo Repository, head string) *FakePullRequest {
	for _, p := range f.prs {
		if p.Repository == repo && p.Head == head && p.open() {
			return p
		}
	}
	return nil
}

func (f *Fake) find(pr PullRequest) *FakePullRequest {
	for _, p := range f.prs {
		if p.Repository == pr.Repository && p.Number == pr.Number {
			return p
		}
	}
	return nil
}

// write stores a commit whose sha is derived from its parent, message and tree,
// and moves the branch head to it. The caller holds the lock.
func (f *Fake) write(repo Repository, branch, message, parent string, tree map[string][]byte) {
	h := sha256.New()
	h.Write([]byte(parent + "\n" + message + "\n"))
	for _, path := range slices.Sorted(maps.Keys(tree)) {
		h.Write([]byte(path + "\n"))
		h.Write(tree[path])
	}
	sha := hex.EncodeToString(h.Sum(nil))[:40]
	f.commits[sha] = FakeCommit{Repository: repo, Branch: branch, Message: message, Parent: parent, SHA: sha, Files: maps.Clone(tree)}
	f.heads[key(repo, branch)] = sha
}

func key(repo Repository, branch string) string { return repo.String() + "#" + branch }

// Approve implements Remote: the approval is recorded on the pull request and
// the review rollup becomes approved unless changes are requested. An unknown
// or closed pull request is an error.
func (f *Fake) Approve(_ context.Context, pr PullRequest, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.Fail[OpApprove]; err != nil {
		return err
	}
	p := f.find(pr)
	if p == nil {
		return fmt.Errorf("%s: %s has no pull request #%d", OpApprove, pr.Repository, pr.Number)
	}
	if !p.open() {
		return fmt.Errorf("%s: %s#%d is not open", OpApprove, pr.Repository, pr.Number)
	}
	p.Approvals = append(p.Approvals, body)
	if p.Review != ReviewChangesRequested {
		p.Review = ReviewApproved
	}
	return nil
}
