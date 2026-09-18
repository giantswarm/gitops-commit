// Package provenance answers where a target's GitOps files live: the owning
// GitHub repository, branch and directory. The answer comes from a cluster's
// Flux objects (a Kustomization and the GitRepository it builds from) or from
// an explicit location the caller names. Input is data the caller has read;
// the package talks to no cluster.
package provenance

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
)

const (
	gitHubHost = "github.com"
	// KindGitRepository is the only Flux source kind a commit can target.
	KindGitRepository = "GitRepository"
)

var (
	// ErrNotFound is returned when the named Kustomization or the GitRepository
	// it references is not among the Flux objects.
	ErrNotFound = errors.New("flux object not found")
	// ErrNotGit is returned when a Kustomization builds from a source that is
	// not a GitRepository; there is no repository to commit to.
	ErrNotGit = errors.New("kustomization source is not a GitRepository")
	// ErrNoBranch is returned when a GitRepository does not follow a branch;
	// a tag, semver range or commit cannot receive a commit.
	ErrNoBranch = errors.New("git repository follows no branch")
	// ErrNotGitHub is returned for a repository URL that is not a GitHub
	// owner/name repository.
	ErrNotGitHub = errors.New("not a GitHub repository URL")
	// ErrInvalidPath is returned for a directory or file name that is absolute
	// or leaves the location's directory.
	ErrInvalidPath = errors.New("path leaves the repository directory")
)

// Repository is a GitHub repository.
type Repository struct {
	Owner string
	Name  string
}

// String returns owner/name.
func (r Repository) String() string { return r.Owner + "/" + r.Name }

// Location is where a target's files live: a directory on a branch of a
// repository.
type Location struct {
	Repository Repository
	Branch     string
	// Directory is repository-relative without a leading "./" or trailing "/";
	// the empty string is the repository root.
	Directory string
}

// Path returns the repository-relative path of a file in the location. A name
// that is absolute or climbs out of the directory is refused.
func (l Location) Path(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("%w: %q", ErrInvalidPath, name)
	}
	joined := path.Join(l.Directory, name)
	if l.Directory == "" {
		if joined == "." || joined == ".." || strings.HasPrefix(joined, "../") {
			return "", fmt.Errorf("%w: %q", ErrInvalidPath, name)
		}
		return joined, nil
	}
	if !strings.HasPrefix(joined, l.Directory+"/") {
		return "", fmt.Errorf("%w: %q", ErrInvalidPath, name)
	}
	return joined, nil
}

// SourceRef names the Flux source a Kustomization builds from.
type SourceRef struct {
	Kind string
	Name string
	// Namespace is empty when the source lives next to the Kustomization.
	Namespace string
}

// Kustomization is the part of a Flux Kustomization the resolution reads.
type Kustomization struct {
	Name      string
	Namespace string
	SourceRef SourceRef
	// Path is spec.path as Flux carries it ("./management-clusters/x").
	Path string
}

// GitRepository is the part of a Flux GitRepository the resolution reads.
type GitRepository struct {
	Name      string
	Namespace string
	URL       string
	// Branch is spec.ref.branch; empty when the source follows something else.
	Branch string
}

// Flux is a cluster's Flux objects as the caller read them.
type Flux struct {
	Kustomizations  []Kustomization
	GitRepositories []GitRepository
}

// Resolve returns the location the Kustomization namespace/name builds from:
// the GitHub repository and branch of its GitRepository and its spec.path as
// the directory.
func (f Flux) Resolve(namespace, name string) (Location, error) {
	ks, ok := f.kustomization(namespace, name)
	if !ok {
		return Location{}, fmt.Errorf("%w: kustomization %s/%s", ErrNotFound, namespace, name)
	}
	if ks.SourceRef.Kind != KindGitRepository {
		return Location{}, fmt.Errorf("%w: kustomization %s/%s builds from %s %q", ErrNotGit, namespace, name, ks.SourceRef.Kind, ks.SourceRef.Name)
	}
	sourceNamespace := ks.SourceRef.Namespace
	if sourceNamespace == "" {
		sourceNamespace = namespace
	}
	repo, ok := f.gitRepository(sourceNamespace, ks.SourceRef.Name)
	if !ok {
		return Location{}, fmt.Errorf("%w: gitrepository %s/%s referenced by kustomization %s/%s", ErrNotFound, sourceNamespace, ks.SourceRef.Name, namespace, name)
	}
	if repo.Branch == "" {
		return Location{}, fmt.Errorf("%w: gitrepository %s/%s", ErrNoBranch, sourceNamespace, repo.Name)
	}
	repository, err := ParseRepositoryURL(repo.URL)
	if err != nil {
		return Location{}, fmt.Errorf("gitrepository %s/%s: %w", sourceNamespace, repo.Name, err)
	}
	directory, err := normalizeDirectory(ks.Path)
	if err != nil {
		return Location{}, fmt.Errorf("kustomization %s/%s: %w", namespace, name, err)
	}
	return Location{Repository: repository, Branch: repo.Branch, Directory: directory}, nil
}

func (f Flux) kustomization(namespace, name string) (Kustomization, bool) {
	for _, ks := range f.Kustomizations {
		if ks.Namespace == namespace && ks.Name == name {
			return ks, true
		}
	}
	return Kustomization{}, false
}

func (f Flux) gitRepository(namespace, name string) (GitRepository, bool) {
	for _, repo := range f.GitRepositories {
		if repo.Namespace == namespace && repo.Name == name {
			return repo, true
		}
	}
	return GitRepository{}, false
}

// Explicit returns the location the caller names: repository as owner/name,
// the branch, and a repository-relative directory ("" or "." for the root).
func Explicit(repository, branch, directory string) (Location, error) {
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return Location{}, fmt.Errorf("%w: repository %q is not owner/name", ErrNotGitHub, repository)
	}
	if branch == "" {
		return Location{}, fmt.Errorf("%w: %s", ErrNoBranch, repository)
	}
	dir, err := normalizeDirectory(directory)
	if err != nil {
		return Location{}, err
	}
	return Location{Repository: Repository{Owner: owner, Name: name}, Branch: branch, Directory: dir}, nil
}

// ParseRepositoryURL returns the GitHub repository a Flux GitRepository URL
// names, in its https, ssh:// or scp-like git@ form, with or without ".git".
func ParseRepositoryURL(raw string) (Repository, error) {
	host, repoPath, err := splitURL(raw)
	if err != nil {
		return Repository{}, err
	}
	if !strings.EqualFold(host, gitHubHost) {
		return Repository{}, fmt.Errorf("%w: host %q in %q", ErrNotGitHub, host, raw)
	}
	repoPath = strings.TrimSuffix(strings.Trim(repoPath, "/"), ".git")
	owner, name, ok := strings.Cut(repoPath, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return Repository{}, fmt.Errorf("%w: %q", ErrNotGitHub, raw)
	}
	return Repository{Owner: owner, Name: name}, nil
}

func splitURL(raw string) (host, repoPath string, err error) {
	if !strings.Contains(raw, "://") {
		// scp-like: git@github.com:owner/name.git
		hostPart, pathPart, ok := strings.Cut(raw, ":")
		if !ok {
			return "", "", fmt.Errorf("%w: %q", ErrNotGitHub, raw)
		}
		if _, h, hasUser := strings.Cut(hostPart, "@"); hasUser {
			hostPart = h
		}
		return hostPart, pathPart, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("%w: %q: %w", ErrNotGitHub, raw, err)
	}
	return u.Hostname(), u.Path, nil
}

// normalizeDirectory turns a Flux spec.path into a repository-relative
// directory: "./x/" and "x" become "x"; "", "." and "./" become "".
func normalizeDirectory(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%w: %q", ErrInvalidPath, p)
	}
	cleaned := path.Clean(p)
	if cleaned == "." {
		return "", nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: %q", ErrInvalidPath, p)
	}
	return cleaned, nil
}
