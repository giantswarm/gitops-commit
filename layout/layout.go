// Package layout is where a writer's files go in a GitOps repository and
// what one write changes there: every writer (a manager, a CLI) owns one
// directory under the directory a Flux Kustomization builds from, holding
// one file per object and its own kustomization.yaml that lists them, and
// the parent's kustomization.yaml, where there is one, lists the directory.
// Flux then builds the directory whether the parent has a kustomization.yaml
// (the entry is added there) or not (Flux generates one that includes every
// directory with a kustomization.yaml).
//
// Build decides a write against the base branch — which files are added,
// updated, removed or already there — encrypts new secret files for the
// repository's .sops.yaml recipients and never re-encrypts one that exists.
// The plan's Change is what commit.Open lands as the person.
package layout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/giantswarm/gitops-commit/provenance"
	"github.com/giantswarm/gitops-commit/sopsenc"
)

// KustomizationFile lists a directory's files for kustomize.
const KustomizationFile = "kustomization.yaml"

// SOPSConfigFile is the repository's encryption rules, read at its root.
const SOPSConfigFile = ".sops.yaml"

// Action is what a write does to one file.
type Action string

// The actions of a plan's files.
const (
	Add       Action = "add"
	Update    Action = "update"
	Remove    Action = "remove"
	Unchanged Action = "unchanged"
)

// Directory is a writer's directory: Name under the directory of Location,
// on its repository and branch.
type Directory struct {
	provenance.Location
	// Name is the writer's directory name, one path segment
	// ("cluster-manager", "model-manager").
	Name string
}

// Path is the directory, repository-relative.
func (d Directory) Path() string {
	return path.Join(d.Directory, d.Name)
}

// ObjectFile is the path of an object's file in the directory: named after
// the object, a Secret in a secret file of its own (sopsenc's naming
// convention), so it is encrypted and never written in plaintext.
func (d Directory) ObjectFile(kind, name string) string {
	if kind == "Secret" {
		return path.Join(d.Path(), name+"-secret.enc.yaml")
	}
	return path.Join(d.Path(), name+".yaml")
}

func (d Directory) validate() error {
	if d.Name == "" || strings.Contains(d.Name, "/") || d.Name == "." || d.Name == ".." {
		return fmt.Errorf("layout: directory name %q is not one path segment", d.Name)
	}
	return nil
}

// within reports whether p is a file inside the directory.
func (d Directory) within(p string) bool {
	return path.Clean(p) == p && strings.HasPrefix(p, d.Path()+"/")
}

// File is one file of a plan. Content is the new content of an added or
// updated plain file, for a dry run to show; a secret file's content is
// never carried.
type File struct {
	Path    string
	Action  Action
	Content []byte
}

// Plan is the change set of one write in a directory.
type Plan struct {
	Directory Directory
	// Files are every file the write decided on, sorted by path — unchanged
	// ones included, so a dry run shows the whole directory the write owns.
	Files []File

	changes map[string][]byte
}

// Changed reports whether the plan writes or removes anything.
func (p *Plan) Changed() bool { return len(p.changes) > 0 }

// Change is the plan as commit.Open takes it: the written files with their
// content, the removed ones nil.
func (p *Plan) Change() commit.Change {
	return commit.Change{Location: p.Directory.Location, Files: maps.Clone(p.changes)}
}

// SecretError refuses secret files the repository cannot take encrypted:
// no .sops.yaml, one that does not parse, or no creation rule with
// recipients for their paths. They are never written in plaintext; the
// caller names its way out.
type SecretError struct {
	Paths  []string
	Reason string
}

func (e *SecretError) Error() string {
	return fmt.Sprintf("%s: %s", strings.Join(e.Paths, ", "), e.Reason)
}

// Build decides a write in dir against the base branch: write are the files
// to land by path, remove the paths to take away (a path the base does not
// carry is no change); every path lies inside the directory. A plain file is
// added, updated or unchanged; a new secret file is encrypted for the
// recipients of the repository's .sops.yaml, one that exists is left as it
// is. The directory's kustomization.yaml lists every file it keeps and is
// removed with the last one; the parent's kustomization.yaml, where there
// is one, lists the directory while it has files. Both are edited on their
// YAML nodes: comments and every other key stay. Read errors from r are
// returned as they are (commit.AuthError included).
func Build(ctx context.Context, r commit.Reader, dir Directory, write map[string][]byte, remove []string) (*Plan, error) {
	if err := dir.validate(); err != nil {
		return nil, err
	}
	for _, p := range slices.Concat(slices.Collect(maps.Keys(write)), remove) {
		if !dir.within(p) || path.Base(p) == KustomizationFile {
			return nil, fmt.Errorf("layout: %s is not an object file of %s", p, dir.Path())
		}
	}
	b := &builder{ctx: ctx, r: r, dir: dir, base: map[string][]byte{}, plan: &Plan{Directory: dir, changes: map[string][]byte{}}}
	if err := b.objects(write, remove); err != nil {
		return nil, err
	}
	emptied, err := b.kustomization()
	if err != nil {
		return nil, err
	}
	if err := b.parent(emptied); err != nil {
		return nil, err
	}
	slices.SortFunc(b.plan.Files, func(x, y File) int { return strings.Compare(x.Path, y.Path) })
	return b.plan, nil
}

type builder struct {
	ctx  context.Context
	r    commit.Reader
	dir  Directory
	base map[string][]byte
	plan *Plan
}

// read is a file at the base branch, nil when the branch does not carry it.
func (b *builder) read(p string) ([]byte, error) {
	if raw, ok := b.base[p]; ok {
		return raw, nil
	}
	raw, err := b.r.ReadFile(b.ctx, b.dir.Repository, b.dir.Branch, p)
	if errors.Is(err, commit.ErrFileNotFound) {
		raw, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	b.base[p] = raw
	return raw, nil
}

// objects decides the written and removed object files.
func (b *builder) objects(write map[string][]byte, remove []string) error {
	cfg, err := b.read(SOPSConfigFile)
	if err != nil {
		return err
	}
	// A .sops.yaml that does not parse refuses only a write with secret
	// files; the naming convention still tells them apart.
	var enc *sopsenc.Encryptor
	var encErr error
	if cfg != nil {
		enc, encErr = sopsenc.New(cfg)
	}
	isSecret := sopsenc.IsSecretFile
	if enc != nil {
		isSecret = enc.IsSecretFile
	}
	var secrets []sopsenc.File
	for _, p := range slices.Sorted(maps.Keys(write)) {
		old, err := b.read(p)
		if err != nil {
			return err
		}
		switch {
		case !isSecret(p):
			b.put(p, old, write[p])
		case old != nil:
			b.plan.Files = append(b.plan.Files, File{Path: p, Action: Unchanged})
		default:
			secrets = append(secrets, sopsenc.File{Path: p, Content: write[p]})
		}
	}
	if len(secrets) > 0 {
		if err := b.encrypt(cfg, enc, encErr, secrets); err != nil {
			return err
		}
	}
	for _, p := range slices.Sorted(slices.Values(remove)) {
		if _, done := b.plan.changes[p]; done {
			continue
		}
		old, err := b.read(p)
		if err != nil {
			return err
		}
		if old != nil {
			b.plan.changes[p] = nil
			b.plan.Files = append(b.plan.Files, File{Path: p, Action: Remove})
		}
	}
	return nil
}

// encrypt adds the new secret files, encrypted for the recipients the
// repository's .sops.yaml names for their paths.
func (b *builder) encrypt(cfg []byte, enc *sopsenc.Encryptor, encErr error, secrets []sopsenc.File) error {
	paths := make([]string, len(secrets))
	for i, f := range secrets {
		paths[i] = f.Path
	}
	switch {
	case cfg == nil:
		return &SecretError{Paths: paths, Reason: b.dir.Repository.String() + " has no " + SOPSConfigFile}
	case encErr != nil:
		return &SecretError{Paths: paths, Reason: fmt.Sprintf("its %s cannot be used (%v)", SOPSConfigFile, encErr)}
	}
	out, err := enc.Encrypt(secrets, func(string) bool { return false })
	if err != nil {
		return &SecretError{Paths: paths, Reason: fmt.Sprintf("its %s does not cover them (%v)", SOPSConfigFile, err)}
	}
	for _, f := range secrets {
		b.plan.changes[f.Path] = out[f.Path]
		b.plan.Files = append(b.plan.Files, File{Path: f.Path, Action: Add})
	}
	return nil
}

// put records a written plain file against its base content.
func (b *builder) put(p string, old, content []byte) {
	switch {
	case old == nil:
		b.plan.changes[p] = content
		b.plan.Files = append(b.plan.Files, File{Path: p, Action: Add, Content: content})
	case bytes.Equal(old, content):
		b.plan.Files = append(b.plan.Files, File{Path: p, Action: Unchanged})
	default:
		b.plan.changes[p] = content
		b.plan.Files = append(b.plan.Files, File{Path: p, Action: Update, Content: content})
	}
}

// kustomization lists every file the directory keeps in its own
// kustomization.yaml, or removes it with the last file; emptied reports
// that the directory holds nothing after the write.
func (b *builder) kustomization() (emptied bool, err error) {
	kpath := path.Join(b.dir.Path(), KustomizationFile)
	old, err := b.read(kpath)
	if err != nil {
		return false, err
	}
	resources, err := Resources(old)
	if err != nil {
		return false, fmt.Errorf("%s in %s: %w", kpath, b.dir.Repository, err)
	}
	for _, f := range b.plan.Files {
		name := strings.TrimPrefix(f.Path, b.dir.Path()+"/")
		switch {
		case f.Action == Remove:
			resources = slices.DeleteFunc(resources, func(r string) bool { return r == name })
		case !slices.Contains(resources, name):
			resources = append(resources, name)
		}
	}
	slices.Sort(resources)
	if len(resources) == 0 {
		if old != nil {
			b.plan.changes[kpath] = nil
			b.plan.Files = append(b.plan.Files, File{Path: kpath, Action: Remove})
		}
		return true, nil
	}
	updated, err := WithResources(old, resources)
	if err != nil {
		return false, fmt.Errorf("%s in %s: %w", kpath, b.dir.Repository, err)
	}
	b.put(kpath, old, updated)
	return false, nil
}

// parent lists the directory in the parent's kustomization.yaml, where
// there is one, while it has files, and takes the entry out with the last.
func (b *builder) parent(emptied bool) error {
	ppath := path.Join(b.dir.Directory, KustomizationFile)
	parent, err := b.read(ppath)
	if err != nil || parent == nil {
		return err
	}
	entries, err := Resources(parent)
	if err != nil {
		return fmt.Errorf("%s in %s: %w", ppath, b.dir.Repository, err)
	}
	isDir := func(r string) bool { return strings.TrimSuffix(strings.TrimPrefix(r, "./"), "/") == b.dir.Name }
	has := slices.ContainsFunc(entries, isDir)
	var updated []byte
	switch {
	case !emptied && !has:
		updated, err = WithResources(parent, append(entries, b.dir.Name))
	case emptied && has:
		updated, err = WithResources(parent, slices.DeleteFunc(entries, isDir))
	default:
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s in %s: %w", ppath, b.dir.Repository, err)
	}
	b.put(ppath, parent, updated)
	return nil
}
