package commit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
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

// FakeCommit is one commit the Fake holds; Files is the full tree at that
// commit. MergeParent is set on the merge commit CreateBranch makes when it
// brings an existing branch up to date: the base's head it merged in.
type FakeCommit struct {
	Repository  Repository
	Branch      string
	Message     string
	Parent      string
	MergeParent string
	SHA         string
	Files       map[string][]byte
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

// CreateBranch implements Remote: a new branch starts at base's head; an
// existing one that lacks base's head takes a merge commit with base's
// changes since their common ancestor, or ErrStaleBranch when a path changed
// on both sides differently.
func (f *Fake) CreateBranch(_ context.Context, repo Repository, branch, base string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.Fail[OpCreateBranch]; err != nil {
		return err
	}
	return f.createBranch(OpCreateBranch, repo, branch, base)
}

// createBranch is CreateBranch with its errors named for op. The caller
// holds the lock.
func (f *Fake) createBranch(op string, repo Repository, branch, base string) error {
	baseSHA, ok := f.heads[key(repo, base)]
	if !ok {
		return fmt.Errorf("%s: %s has no branch %q", op, repo, base)
	}
	head, exists := f.heads[key(repo, branch)]
	if !exists {
		f.heads[key(repo, branch)] = baseSHA
		return nil
	}
	if f.contains(head, baseSHA) {
		return nil
	}
	tree, ok := f.merge(head, baseSHA)
	if !ok {
		if pr := f.findOpen(repo, branch); pr != nil {
			return staleBranch(op, repo, branch, base, pr.URL)
		}
		return staleBranch(op, repo, branch, base, "")
	}
	f.writeMerge(repo, branch, updateMessage(branch, base), head, baseSHA, tree)
	return nil
}

// contains reports whether target is sha or in its history, through both
// parents of a merge commit.
func (f *Fake) contains(sha, target string) bool {
	seen := map[string]bool{}
	for stack := []string{sha}; len(stack) > 0; {
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if s == target {
			return true
		}
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		c := f.commits[s]
		stack = append(stack, c.Parent, c.MergeParent)
	}
	return false
}

// mergeBase is the nearest commit in both histories, "" (the empty tree)
// when they never met.
func (f *Fake) mergeBase(a, b string) string {
	ancestors := map[string]bool{}
	for stack := []string{a}; len(stack) > 0; {
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if s == "" || ancestors[s] {
			continue
		}
		ancestors[s] = true
		c := f.commits[s]
		stack = append(stack, c.Parent, c.MergeParent)
	}
	seen := map[string]bool{}
	for queue := []string{b}; len(queue) > 0; {
		s := queue[0]
		queue = queue[1:]
		if s == "" || seen[s] {
			continue
		}
		if ancestors[s] {
			return s
		}
		seen[s] = true
		c := f.commits[s]
		queue = append(queue, c.Parent, c.MergeParent)
	}
	return ""
}

// merge is the three-way merge of the trees at ours and theirs against their
// common ancestor: a path only theirs changed takes theirs, a path only ours
// changed keeps ours, a path both changed the same way is no conflict; a path
// both changed differently is, and the merge is refused.
func (f *Fake) merge(ours, theirs string) (map[string][]byte, bool) {
	common := f.commits[f.mergeBase(ours, theirs)].Files
	ourTree, theirTree := f.commits[ours].Files, f.commits[theirs].Files
	tree := maps.Clone(ourTree)
	if tree == nil {
		tree = map[string][]byte{}
	}
	paths := map[string]bool{}
	for path := range theirTree {
		paths[path] = true
	}
	for path := range common {
		paths[path] = true
	}
	for path := range paths {
		was, inCommon := common[path]
		their, inTheirs := theirTree[path]
		our, inOurs := ourTree[path]
		if sameEntry(their, inTheirs, was, inCommon) {
			continue
		}
		if !sameEntry(our, inOurs, was, inCommon) && !sameEntry(our, inOurs, their, inTheirs) {
			return nil, false
		}
		if inTheirs {
			tree[path] = their
		} else {
			delete(tree, path)
		}
	}
	return tree, true
}

// sameEntry: two tree entries are the same when both are absent or both carry the same content.
func sameEntry(a []byte, inA bool, b []byte, inB bool) bool {
	return inA == inB && bytes.Equal(a, b)
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
	return f.commit(OpCommit, repo, branch, message, files)
}

// commit writes one commit with files on top of branch's head, its errors
// named for op. The caller holds the lock.
func (f *Fake) commit(op string, repo Repository, branch, message string, files map[string][]byte) error {
	parent, ok := f.heads[key(repo, branch)]
	if !ok {
		return fmt.Errorf("%s: %s has no branch %q", op, repo, branch)
	}
	tree := maps.Clone(f.commits[parent].Files)
	if tree == nil {
		tree = map[string][]byte{}
	}
	for path, content := range files {
		if content == nil {
			delete(tree, path)
			continue
		}
		tree[path] = content
	}
	f.write(repo, branch, message, parent, tree)
	return nil
}

// ReadFile implements Reader.
func (f *Fake) ReadFile(_ context.Context, repo Repository, branch, path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.Fail[OpReadFile]; err != nil {
		return nil, err
	}
	sha, ok := f.heads[key(repo, branch)]
	if !ok {
		return nil, fmt.Errorf("%s: %s has no branch %q", OpReadFile, repo, branch)
	}
	content, ok := f.commits[sha].Files[path]
	if !ok {
		return nil, fmt.Errorf("%s %s/%s@%s: %w", OpReadFile, repo, path, branch, ErrFileNotFound)
	}
	return slices.Clone(content), nil
}

// ListFiles implements Lister.
func (f *Fake) ListFiles(_ context.Context, repo Repository, branch, dir string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.Fail[OpListFiles]; err != nil {
		return nil, err
	}
	sha, ok := f.heads[key(repo, branch)]
	if !ok {
		return nil, fmt.Errorf("%s: %s has no branch %q", OpListFiles, repo, branch)
	}
	var out []string
	for p := range f.commits[sha].Files {
		if strings.HasPrefix(p, dir+"/") {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out, nil
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
// the base as it was at the merge, in one commit on the head of the revert
// branch, created at the base's tip or brought up to date with it as
// CreateBranch does.
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
	files := make(map[string][]byte, len(changed))
	for path := range changed {
		switch content, was := before[path]; {
		case overrides[path] != nil:
			files[path] = overrides[path]
		case was:
			files[path] = content
		default:
			files[path] = nil
		}
	}
	branch := revertBranch(p.PullRequest)
	if err := f.createBranch(OpRevert, p.Repository, branch, p.Base); err != nil {
		return PullRequest{}, err
	}
	if err := f.commit(OpRevert, p.Repository, branch, revertMessage(p.PullRequest, p.Title, body), files); err != nil {
		return PullRequest{}, err
	}
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
	f.writeMerge(repo, branch, message, parent, "", tree)
}

// writeMerge is write for a commit with a second parent, the base head a
// merge brought in; "" for an ordinary commit.
func (f *Fake) writeMerge(repo Repository, branch, message, parent, mergeParent string, tree map[string][]byte) {
	h := sha256.New()
	h.Write([]byte(parent + "\n" + mergeParent + "\n" + message + "\n"))
	for _, path := range slices.Sorted(maps.Keys(tree)) {
		h.Write([]byte(path + "\n"))
		h.Write(tree[path])
	}
	sha := hex.EncodeToString(h.Sum(nil))[:40]
	f.commits[sha] = FakeCommit{Repository: repo, Branch: branch, Message: message, Parent: parent, MergeParent: mergeParent, SHA: sha, Files: maps.Clone(tree)}
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
