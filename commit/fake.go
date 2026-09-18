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
	Title  string
	Body   string
	Merged bool
	Review ReviewState
	Checks CheckState
}

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
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.Fail[OpOpenPullRequest]; err != nil {
		return PullRequest{}, err
	}
	headSHA, ok := f.heads[key(repo, head)]
	if !ok {
		return PullRequest{}, fmt.Errorf("%s: %s has no branch %q", OpOpenPullRequest, repo, head)
	}
	for _, pr := range f.prs {
		if pr.Repository == repo && pr.Head == head && !pr.Merged {
			pr.HeadSHA = headSHA
			return pr.PullRequest, nil
		}
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
		Review: ReviewNone,
		Checks: ChecksNone,
	}
	f.prs = append(f.prs, pr)
	return pr.PullRequest, nil
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
	if head := f.heads[key(p.Repository, p.Head)]; head != pr.HeadSHA {
		return fmt.Errorf("%s: head is %s, not %s", OpMerge, head, pr.HeadSHA)
	}
	p.Merged = true
	f.heads[key(p.Repository, p.Base)] = pr.HeadSHA
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
