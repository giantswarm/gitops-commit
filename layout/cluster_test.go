package layout

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/giantswarm/gitops-commit/provenance"
)

const (
	orgFile       = "management-clusters/mc1/organizations/acme/acme.yaml"
	wcsKust       = "management-clusters/mc1/organizations/acme/workload-clusters/kustomization.yaml"
	clusterKust   = "management-clusters/mc1/organizations/acme/workload-clusters/wc1.yaml"
	clusterDir    = "management-clusters/mc1/organizations/acme/workload-clusters/wc1"
	clusterObj    = clusterDir + "/wc1.yaml"
	clusterSecret = clusterDir + "/wc1-secret.enc.yaml"
	clusterOwn    = clusterDir + "/kustomization.yaml"
	clusterPool   = clusterDir + "/cluster-manager/pool.yaml"
	otherKust     = "# the org's clusters\napiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - other.yaml # kept\n"
)

// fleet is the Flux objects of a gitops-template repository: the
// management cluster's Kustomization reconciles its organizations.
func fleet(prune bool) provenance.Flux {
	return provenance.Flux{
		Kustomizations: []provenance.Kustomization{{
			Name: "flux", Namespace: "default", Path: "./management-clusters/mc1",
			SourceRef:          provenance.SourceRef{Kind: provenance.KindGitRepository, Name: "fleet"},
			ServiceAccountName: "automation", Interval: "1m", Timeout: "2m", Prune: prune,
			Keys: &provenance.Keys{Provider: "sops", SecretRef: "sops-gpg-master"},
		}, {
			Name: "from-oci", Namespace: "default", Path: "./",
			SourceRef: provenance.SourceRef{Kind: "OCIRepository", Name: "artifacts"},
		}},
		GitRepositories: []provenance.GitRepository{{Name: "fleet", Namespace: "default", URL: "https://github.com/acme/fleet", Branch: "main"}},
	}
}

func cluster(t *testing.T, prune bool) Cluster {
	t.Helper()
	c, err := NewCluster(fleet(prune), "default", "flux", "mc1", "acme", "wc1")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func clusterWrite() map[string][]byte {
	return map[string][]byte{
		clusterObj:    []byte("kind: HelmRelease\nmetadata:\n  name: wc1\n"),
		clusterSecret: []byte("kind: Secret\nmetadata:\n  name: wc1\nstringData:\n  values: placeholder\n"),
	}
}

const wantClusterKustomization = `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: mc1-clusters-wc1
  namespace: default
spec:
  decryption:
    provider: sops
    secretRef:
      name: sops-gpg-master
  interval: 1m
  path: ./management-clusters/mc1/organizations/acme/workload-clusters/wc1
  prune: true
  serviceAccountName: automation
  sourceRef:
    kind: GitRepository
    name: fleet
  timeout: 2m
`

func TestBuildCluster(t *testing.T) {
	for _, tc := range []struct {
		name     string
		base     map[string][]byte
		wantKust Action
		wantList string
	}{{
		name:     "workload-clusters/kustomization.yaml exists",
		base:     map[string][]byte{wcsKust: []byte(otherKust)},
		wantKust: Update,
		wantList: "# the org's clusters\napiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - other.yaml # kept\n  - wc1.yaml\n",
	}, {
		name:     "workload-clusters/kustomization.yaml absent",
		base:     map[string][]byte{},
		wantKust: Add,
		wantList: "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - wc1.yaml\n",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			base := maps.Clone(tc.base)
			base[SOPSConfigFile] = sopsYAMLFor(t, "management-clusters/.*secret.*")
			base[orgFile] = []byte("kind: Organization\n")
			fake := seed(t, base)
			plan, err := BuildCluster(context.Background(), fake, cluster(t, true), clusterWrite())
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]Action{clusterObj: Add, clusterSecret: Add, clusterOwn: Add, clusterKust: Add, wcsKust: tc.wantKust}
			if got := actions(plan.Plan); !maps.Equal(got, want) {
				t.Fatalf("actions = %v, want %v", got, want)
			}
			files := plan.Change().Files
			if got := string(files[clusterKust]); got != wantClusterKustomization {
				t.Errorf("cluster Kustomization:\n%s\nwant:\n%s", got, wantClusterKustomization)
			}
			if got := string(files[wcsKust]); got != tc.wantList {
				t.Errorf("workload-clusters/kustomization.yaml:\n%s\nwant:\n%s", got, tc.wantList)
			}
			if got := string(files[clusterOwn]); got != "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - wc1-secret.enc.yaml\n  - wc1.yaml\n" {
				t.Errorf("cluster kustomization.yaml:\n%s", got)
			}
			if enc := files[clusterSecret]; !bytes.Contains(enc, []byte("sops:")) || bytes.Contains(enc, []byte("placeholder")) {
				t.Errorf("secret file not encrypted:\n%s", enc)
			}
			if plan.Kustomization != "default/mc1-clusters-wc1" || !plan.Prune {
				t.Errorf("kustomization = %q, prune = %v", plan.Kustomization, plan.Prune)
			}

			// A re-run against the merged files changes nothing.
			merged := maps.Clone(base)
			maps.Copy(merged, files)
			again, err := BuildCluster(context.Background(), seed(t, merged), cluster(t, true), clusterWrite())
			if err != nil {
				t.Fatal(err)
			}
			if again.Changed() {
				t.Errorf("re-run changed %v", actions(again.Plan))
			}
		})
	}
}

func TestRemoveCluster(t *testing.T) {
	base := map[string][]byte{
		orgFile:       []byte("kind: Organization\n"),
		wcsKust:       []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - other.yaml\n  - wc1.yaml\n"),
		clusterKust:   []byte(wantClusterKustomization),
		clusterObj:    []byte("kind: HelmRelease\n"),
		clusterSecret: []byte("sops: encrypted\n"),
		clusterOwn:    []byte("resources: [wc1.yaml, wc1-secret.enc.yaml, cluster-manager]\n"),
		clusterPool:   []byte("kind: HelmRelease\n"),
		"management-clusters/mc1/organizations/acme/workload-clusters/wc10/x.yaml": []byte("kept"),
	}
	for _, tc := range []struct {
		name  string
		prune bool
	}{{"pruning owner", true}, {"non-pruning owner", false}} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := RemoveCluster(context.Background(), seed(t, base), cluster(t, tc.prune))
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]Action{clusterKust: Remove, clusterObj: Remove, clusterSecret: Remove, clusterOwn: Remove, clusterPool: Remove, wcsKust: Update}
			if got := actions(plan.Plan); !maps.Equal(got, want) {
				t.Fatalf("actions = %v, want %v", got, want)
			}
			if got := string(plan.Change().Files[wcsKust]); got != "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - other.yaml\n" {
				t.Errorf("workload-clusters/kustomization.yaml:\n%s", got)
			}
			if plan.Prune != tc.prune || plan.Kustomization != "default/mc1-clusters-wc1" {
				t.Errorf("prune = %v, kustomization = %q", plan.Prune, plan.Kustomization)
			}
		})
	}

	t.Run("already removed", func(t *testing.T) {
		plan, err := RemoveCluster(context.Background(), seed(t, map[string][]byte{orgFile: []byte("x")}), cluster(t, true))
		if err != nil || plan.Changed() {
			t.Fatalf("plan = %v, %v", plan, err)
		}
	})
}

func TestClusterRefusals(t *testing.T) {
	noBranch := fleet(true)
	noBranch.GitRepositories[0].Branch = ""
	for _, tc := range []struct {
		name                 string
		flux                 provenance.Flux
		owner                string
		inst, org, clusterID string
		want                 error
	}{
		{"organization without a Kustomization", fleet(true), "", "mc1", "acme", "wc1", ErrNotReconciled},
		{"Kustomization not from a GitRepository", fleet(true), "from-oci", "mc1", "acme", "wc1", provenance.ErrNotGit},
		{"GitRepository on no branch", noBranch, "flux", "mc1", "acme", "wc1", provenance.ErrNoBranch},
		{"Kustomization not among the Flux objects", fleet(true), "missing", "mc1", "acme", "wc1", provenance.ErrNotFound},
		{"cluster name with a slash", fleet(true), "flux", "mc1", "acme", "wc1/x", nil},
		{"empty organization", fleet(true), "flux", "mc1", "", "wc1", nil},
		{"installation ..", fleet(true), "flux", "..", "acme", "wc1", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewCluster(tc.flux, "default", tc.owner, tc.inst, tc.org, tc.clusterID)
			if err == nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("NewCluster error = %v, want %v", err, tc.want)
			}
		})
	}

	withOrg := map[string][]byte{orgFile: []byte("kind: Organization\n")}
	for _, tc := range []struct {
		name  string
		base  map[string][]byte
		write map[string][]byte
		want  error
	}{
		{"root without the organization's directory", map[string][]byte{"management-clusters/mc1/organizations/other/other.yaml": []byte("x")}, clusterWrite(), ErrNoOrganization},
		{"no object files", withOrg, nil, nil},
		{"object outside the cluster's directory", withOrg, map[string][]byte{clusterKust: []byte("x")}, nil},
		{"object named kustomization.yaml", withOrg, map[string][]byte{clusterOwn: []byte("x")}, nil},
		{"secret without .sops.yaml", withOrg, clusterWrite(), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildCluster(context.Background(), seed(t, tc.base), cluster(t, true), tc.write)
			if err == nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("BuildCluster error = %v, want %v", err, tc.want)
			}
		})
	}

	t.Run("organization's directory named in the refusal", func(t *testing.T) {
		_, err := BuildCluster(context.Background(), seed(t, nil), cluster(t, true), clusterWrite())
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte("management-clusters/mc1/organizations/acme/")) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("read errors are returned as they are", func(t *testing.T) {
		fake := seed(t, withOrg)
		fake.Fail[commit.OpListFiles] = &commit.AuthError{Op: commit.OpListFiles, Status: 401}
		if _, err := BuildCluster(context.Background(), fake, cluster(t, true), clusterWrite()); !errors.Is(err, commit.ErrAuth) {
			t.Fatalf("BuildCluster error = %v, want ErrAuth", err)
		}
		if _, err := RemoveCluster(context.Background(), fake, cluster(t, true)); !errors.Is(err, commit.ErrAuth) {
			t.Fatalf("RemoveCluster error = %v, want ErrAuth", err)
		}
	})
}
