package layout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/giantswarm/gitops-commit/provenance"
)

// A new workload cluster lives where a gitops-template repository keeps
// every cluster (the files `kubectl gs gitops add workload-cluster` writes),
// under the directory the Flux Kustomization of the cluster's Organization
// builds from:
//
//	organizations/<org>/workload-clusters/kustomization.yaml  lists <cluster>.yaml
//	organizations/<org>/workload-clusters/<cluster>.yaml      the cluster's Flux Kustomization
//	organizations/<org>/workload-clusters/<cluster>/          one file per object, its own kustomization.yaml
//
// The cluster's Kustomization builds <cluster>/, so the organization's
// Kustomization applies the Kustomization and never the cluster's objects.
// A writer that adds to an existing cluster later (node pools) takes its own
// Directory under <cluster>/, as for any cluster.

const (
	// OrganizationsDirectory holds the organizations under a root.
	OrganizationsDirectory = "organizations"
	// WorkloadClustersDirectory holds an organization's clusters.
	WorkloadClustersDirectory = "workload-clusters"
)

var (
	// ErrNotReconciled: the Organization carries no Flux Kustomization, so
	// no repository owns it.
	ErrNotReconciled = errors.New("not reconciled by a Flux Kustomization")
	// ErrNoOrganization: the root has no directory for the organization.
	ErrNoOrganization = errors.New("no organization directory")
)

// ClusterReader is what a cluster's plan reads of the base branch: files,
// and the files under a directory.
type ClusterReader interface {
	commit.Reader
	commit.Lister
}

// Cluster is a new workload cluster's place in the repository that owns its
// Organization.
type Cluster struct {
	// Location is the root: the repository, branch and directory the
	// Organization's Kustomization builds from.
	provenance.Location
	// Owner is the Organization's Kustomization; the cluster's Kustomization
	// takes its source, service account, interval, timeout and keys.
	Owner provenance.Kustomization
	// Installation names the cluster's Kustomization,
	// <installation>-clusters-<cluster>.
	Installation string
	Organization string
	Name         string
}

// NewCluster places the cluster name of organization on installation: the
// Organization is reconciled by the Kustomization ownerNamespace/ownerName
// (its kustomize.toolkit.fluxcd.io labels) among the Flux objects. An
// Organization without one is ErrNotReconciled; one whose Kustomization
// does not build from a GitHub branch is refused with Resolve's error.
func NewCluster(flux provenance.Flux, ownerNamespace, ownerName, installation, organization, name string) (Cluster, error) {
	for _, s := range []struct{ what, value string }{{"installation", installation}, {"organization", organization}, {"cluster", name}} {
		if !segment(s.value) {
			return Cluster{}, fmt.Errorf("layout: %s name %q is not one path segment", s.what, s.value)
		}
	}
	if ownerName == "" {
		return Cluster{}, fmt.Errorf("organization %s: %w", organization, ErrNotReconciled)
	}
	loc, err := flux.Resolve(ownerNamespace, ownerName)
	if err != nil {
		return Cluster{}, fmt.Errorf("organization %s: %w", organization, err)
	}
	owner, _ := flux.FindKustomization(ownerNamespace, ownerName)
	return Cluster{Location: loc, Owner: owner, Installation: installation, Organization: organization, Name: name}, nil
}

// OrganizationPath is the organization's directory, repository-relative.
func (c Cluster) OrganizationPath() string {
	return path.Join(c.Location.Directory, OrganizationsDirectory, c.Organization)
}

// Directory is the cluster's own directory, <cluster>/ under the
// organization's workload-clusters/; its ObjectFile names the object files.
func (c Cluster) Directory() Directory {
	loc := c.Location
	loc.Directory = path.Join(c.OrganizationPath(), WorkloadClustersDirectory)
	return Directory{Location: loc, Name: c.Name}
}

// KustomizationPath is the file of the cluster's Flux Kustomization.
func (c Cluster) KustomizationPath() string {
	return path.Join(c.Directory().Directory, c.Name+".yaml")
}

// Kustomization is the cluster's Flux Kustomization, namespace/name, next
// to the Organization's.
func (c Cluster) Kustomization() string {
	return c.Owner.Namespace + "/" + c.Installation + "-clusters-" + c.Name
}

// ClusterPlan is the change set of a cluster's add or removal.
type ClusterPlan struct {
	*Plan
	// Kustomization is the cluster's Flux Kustomization, namespace/name.
	Kustomization string
	// Prune is the Organization's Kustomization's spec.prune. Without it a
	// removal's merge leaves the cluster's Kustomization on the installation:
	// deleting it is the live step, and Flux then removes the cluster.
	Prune bool
}

// BuildCluster decides a new cluster's files against the base branch: the
// objects in write (paths inside Directory, named by its ObjectFile, a new
// secret file encrypted as Build does), the directory's kustomization.yaml,
// the cluster's Flux Kustomization and its entry in the organization's
// workload-clusters/kustomization.yaml, created when absent. A root without
// the organization's directory is ErrNoOrganization, naming the path looked
// for. A re-run whose files are on the base changes nothing.
func BuildCluster(ctx context.Context, r ClusterReader, c Cluster, write map[string][]byte) (*ClusterPlan, error) {
	if len(write) == 0 {
		return nil, fmt.Errorf("layout: cluster %s has no object files", c.Name)
	}
	b, err := newBuilder(ctx, r, c.Directory(), write, nil)
	if err != nil {
		return nil, err
	}
	org, err := r.ListFiles(ctx, c.Repository, c.Branch, c.OrganizationPath())
	if err != nil {
		return nil, err
	}
	if len(org) == 0 {
		return nil, fmt.Errorf("%w: %s@%s has no %s/", ErrNoOrganization, c.Repository, c.Branch, c.OrganizationPath())
	}
	if _, err := b.directory(write, nil); err != nil {
		return nil, err
	}
	ks, err := c.fluxKustomization()
	if err != nil {
		return nil, err
	}
	old, err := b.read(c.KustomizationPath())
	if err != nil {
		return nil, err
	}
	b.put(c.KustomizationPath(), old, ks)
	if err := b.clusterEntry(c, true); err != nil {
		return nil, err
	}
	return c.plan(b.done()), nil
}

// RemoveCluster decides a cluster's removal against the base branch: every
// file under its directory, its Flux Kustomization and the entry in the
// organization's workload-clusters/kustomization.yaml; what the base does
// not carry is no change.
func RemoveCluster(ctx context.Context, r ClusterReader, c Cluster) (*ClusterPlan, error) {
	b, err := newBuilder(ctx, r, c.Directory(), nil, nil)
	if err != nil {
		return nil, err
	}
	files, err := r.ListFiles(ctx, c.Repository, c.Branch, c.Directory().Path())
	if err != nil {
		return nil, err
	}
	ks, err := b.read(c.KustomizationPath())
	if err != nil {
		return nil, err
	}
	if ks != nil {
		files = append(files, c.KustomizationPath())
	}
	for _, p := range files {
		b.plan.changes[p] = nil
		b.plan.Files = append(b.plan.Files, File{Path: p, Action: Remove})
	}
	if err := b.clusterEntry(c, false); err != nil {
		return nil, err
	}
	return c.plan(b.done()), nil
}

func (c Cluster) plan(p *Plan) *ClusterPlan {
	return &ClusterPlan{Plan: p, Kustomization: c.Kustomization(), Prune: c.Owner.Prune}
}

// clusterEntry lists the cluster's Kustomization file in the organization's
// workload-clusters/kustomization.yaml (creating it when absent), or takes
// the entry out; the file stays when its last entry goes.
func (b *builder) clusterEntry(c Cluster, listed bool) error {
	kpath := path.Join(c.Directory().Directory, KustomizationFile)
	old, err := b.read(kpath)
	if err != nil {
		return err
	}
	entries, err := Resources(old)
	if err != nil {
		return fmt.Errorf("%s in %s: %w", kpath, c.Repository, err)
	}
	entry := c.Name + ".yaml"
	isEntry := func(r string) bool { return strings.TrimPrefix(r, "./") == entry }
	if slices.ContainsFunc(entries, isEntry) == listed {
		return nil
	}
	if listed {
		entries = append(entries, entry)
	} else {
		entries = slices.DeleteFunc(entries, isEntry)
	}
	updated, err := WithResources(old, entries)
	if err != nil {
		return fmt.Errorf("%s in %s: %w", kpath, c.Repository, err)
	}
	b.put(kpath, old, updated)
	return nil
}

// fluxKustomization renders the cluster's Flux Kustomization: it builds the
// cluster's directory from the Organization's source with its service
// account, interval, timeout and keys (spec.decryption), prunes, and substitutes
// nothing — postBuild would rewrite ${…} in a person's values.
func (c Cluster) fluxKustomization() ([]byte, error) {
	type keys struct {
		Provider  string            `yaml:"provider"`
		SecretRef map[string]string `yaml:"secretRef,omitempty"`
	}
	type sourceRef struct {
		Kind      string `yaml:"kind"`
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace,omitempty"`
	}
	type spec struct {
		Keys               *keys     `yaml:"decryption,omitempty"`
		Interval           string    `yaml:"interval,omitempty"`
		Path               string    `yaml:"path"`
		Prune              bool      `yaml:"prune"`
		ServiceAccountName string    `yaml:"serviceAccountName,omitempty"`
		SourceRef          sourceRef `yaml:"sourceRef"`
		Timeout            string    `yaml:"timeout,omitempty"`
	}
	type metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	}
	namespace, name, _ := strings.Cut(c.Kustomization(), "/")
	s := spec{
		Interval:           c.Owner.Interval,
		Path:               "./" + c.Directory().Path(),
		Prune:              true,
		ServiceAccountName: c.Owner.ServiceAccountName,
		SourceRef:          sourceRef{Kind: c.Owner.SourceRef.Kind, Name: c.Owner.SourceRef.Name, Namespace: c.Owner.SourceRef.Namespace},
		Timeout:            c.Owner.Timeout,
	}
	if k := c.Owner.Keys; k != nil {
		s.Keys = &keys{Provider: k.Provider}
		if k.SecretRef != "" {
			s.Keys.SecretRef = map[string]string{"name": k.SecretRef}
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	err := enc.Encode(struct {
		APIVersion string   `yaml:"apiVersion"`
		Kind       string   `yaml:"kind"`
		Metadata   metadata `yaml:"metadata"`
		Spec       spec     `yaml:"spec"`
	}{"kustomize.toolkit.fluxcd.io/v1", "Kustomization", metadata{name, namespace}, s})
	if err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// segment reports whether s is one path segment.
func segment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.Contains(s, "/")
}
