package provenance

import (
	"encoding/json"
	"errors"
	"testing"
)

const (
	testNamespace = "flux-system"
	testCMCRepo   = "example-org/example-management-clusters"
	mainBranch    = "main"
	exampleOrg    = "example-org"
	exampleRepo   = "example-org/repo"
	configName    = "config"
	taggedName    = "tagged"
	pathX         = "./x"
	mc1Dir        = "management-clusters/mc1"
)

func fixtureFlux() Flux {
	return Flux{
		Kustomizations: []Kustomization{
			{Name: "flux", Namespace: testNamespace, SourceRef: SourceRef{Kind: KindGitRepository, Name: "management-clusters"}, Path: "./management-clusters/mc1"},
			{Name: configName, Namespace: testNamespace, SourceRef: SourceRef{Kind: KindGitRepository, Name: configName, Namespace: "other"}, Path: "./"},
			{Name: "oci", Namespace: testNamespace, SourceRef: SourceRef{Kind: "OCIRepository", Name: "charts"}, Path: pathX},
			{Name: taggedName, Namespace: testNamespace, SourceRef: SourceRef{Kind: KindGitRepository, Name: taggedName}, Path: pathX},
			{Name: "orphan", Namespace: testNamespace, SourceRef: SourceRef{Kind: KindGitRepository, Name: "missing"}, Path: pathX},
		},
		GitRepositories: []GitRepository{
			{Name: "management-clusters", Namespace: testNamespace, URL: "ssh://git@github.com/" + testCMCRepo + ".git", Branch: mainBranch},
			{Name: configName, Namespace: "other", URL: "https://github.com/example-org/example-config", Branch: "mc1-config"},
			{Name: taggedName, Namespace: testNamespace, URL: "https://github.com/example-org/tagged.git"},
		},
	}
}

func TestResolveFollowsKustomizationToGitRepository(t *testing.T) {
	loc, err := fixtureFlux().Resolve(testNamespace, "flux")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := Location{Repository: Repository{Owner: exampleOrg, Name: "example-management-clusters"}, Branch: mainBranch, Directory: mc1Dir}
	if loc != want {
		t.Fatalf("Resolve = %+v, want %+v", loc, want)
	}
	if got := loc.Repository.String(); got != testCMCRepo {
		t.Fatalf("Repository.String() = %q, want %q", got, testCMCRepo)
	}
	p, err := loc.Path("apps/dex/secret.yaml")
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if want := "management-clusters/mc1/apps/dex/secret.yaml"; p != want {
		t.Fatalf("Path = %q, want %q", p, want)
	}
}

func TestResolveCrossNamespaceSourceAtRepositoryRoot(t *testing.T) {
	loc, err := fixtureFlux().Resolve(testNamespace, configName)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if loc.Directory != "" || loc.Branch != "mc1-config" || loc.Repository.Name != "example-config" {
		t.Fatalf("Resolve = %+v", loc)
	}
	p, err := loc.Path("installations/mc1/secret.yaml")
	if err != nil || p != "installations/mc1/secret.yaml" {
		t.Fatalf("Path = %q, %v", p, err)
	}
}

func TestResolveErrors(t *testing.T) {
	flux := fixtureFlux()
	cases := map[string]struct {
		name string
		want error
	}{
		"unknown kustomization": {name: "nope", want: ErrNotFound},
		"non-git source":        {name: "oci", want: ErrNotGit},
		"missing repository":    {name: "orphan", want: ErrNotFound},
		"no branch":             {name: taggedName, want: ErrNoBranch},
	}
	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			_, err := flux.Resolve(testNamespace, tc.name)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Resolve(%s) error = %v, want %v", tc.name, err, tc.want)
			}
		})
	}
}

func TestPathRefusesEscapes(t *testing.T) {
	loc := Location{Directory: mc1Dir}
	for _, name := range []string{"", "/etc/passwd", "../other/secret.yaml", "..", "a/../../x"} {
		if _, err := loc.Path(name); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Path(%q) error = %v, want ErrInvalidPath", name, err)
		}
	}
	root := Location{}
	if _, err := root.Path("../x"); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("root Path(../x) error = %v, want ErrInvalidPath", err)
	}
}

func TestExplicit(t *testing.T) {
	loc, err := Explicit("example-org/teleport-fleet", mainBranch, "./clusters/hub/")
	if err != nil {
		t.Fatalf("Explicit: %v", err)
	}
	want := Location{Repository: Repository{Owner: exampleOrg, Name: "teleport-fleet"}, Branch: mainBranch, Directory: "clusters/hub"}
	if loc != want {
		t.Fatalf("Explicit = %+v, want %+v", loc, want)
	}
	if loc, err := Explicit(exampleRepo, mainBranch, "."); err != nil || loc.Directory != "" {
		t.Fatalf("Explicit root = %+v, %v", loc, err)
	}
	for _, tc := range []struct {
		repo, branch, dir string
		want              error
	}{
		{"no-slash", mainBranch, "", ErrNotGitHub},
		{"a/b/c", mainBranch, "", ErrNotGitHub},
		{exampleRepo, "", "", ErrNoBranch},
		{exampleRepo, mainBranch, "/abs", ErrInvalidPath},
		{exampleRepo, mainBranch, "../up", ErrInvalidPath},
	} {
		if _, err := Explicit(tc.repo, tc.branch, tc.dir); !errors.Is(err, tc.want) {
			t.Errorf("Explicit(%q, %q, %q) error = %v, want %v", tc.repo, tc.branch, tc.dir, err, tc.want)
		}
	}
}

func TestParseRepositoryURL(t *testing.T) {
	want := Repository{Owner: exampleOrg, Name: "example-config"}
	for _, raw := range []string{
		"https://github.com/example-org/example-config",
		"https://github.com/example-org/example-config.git",
		"ssh://git@github.com/example-org/example-config.git",
		"git@github.com:example-org/example-config.git",
		"https://GitHub.com/example-org/example-config/",
		"ssh://git@ssh.github.com:443/example-org/example-config.git",
	} {
		got, err := ParseRepositoryURL(raw)
		if err != nil || got != want {
			t.Errorf("ParseRepositoryURL(%q) = %+v, %v", raw, got, err)
		}
	}
	for _, raw := range []string{
		"https://gitlab.com/example-org/example-config.git",
		"ssh://git@ssh.gitlab.com:443/example-org/example-config.git",
		"https://github.com/example-org",
		"https://github.com/example-org/repo/extra",
		"not-a-url",
		"ssh://git@github.com/",
	} {
		if _, err := ParseRepositoryURL(raw); !errors.Is(err, ErrNotGitHub) {
			t.Errorf("ParseRepositoryURL(%q) error = %v, want ErrNotGitHub", raw, err)
		}
	}
}

func TestAddDecodedObjects(t *testing.T) {
	objects := []string{
		`{"apiVersion":"kustomize.toolkit.fluxcd.io/v1","kind":"Kustomization","metadata":{"name":"flux","namespace":"flux-system"},"spec":{"path":"./management-clusters/mc1","sourceRef":{"kind":"GitRepository","name":"management-clusters"}}}`,
		`{"apiVersion":"source.toolkit.fluxcd.io/v1","kind":"GitRepository","metadata":{"name":"management-clusters","namespace":"flux-system"},"spec":{"url":"https://github.com/example-org/example-management-clusters","ref":{"branch":"main"}}}`,
	}
	var flux Flux
	for _, raw := range objects {
		var obj map[string]any
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			t.Fatal(err)
		}
		if err := flux.Add(obj); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	loc, err := flux.Resolve(testNamespace, "flux")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if loc.Repository.String() != testCMCRepo || loc.Directory != mc1Dir || loc.Branch != mainBranch {
		t.Fatalf("Resolve = %+v", loc)
	}

	var other map[string]any
	if err := json.Unmarshal([]byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"x","namespace":"y"}}`), &other); err != nil {
		t.Fatal(err)
	}
	if err := flux.Add(other); !errors.Is(err, ErrUnknownObject) {
		t.Fatalf("Add(ConfigMap) error = %v, want ErrUnknownObject", err)
	}
	if err := flux.Add(map[string]any{"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": KindGitRepository}); err == nil {
		t.Fatal("Add without metadata: want error")
	}
}
