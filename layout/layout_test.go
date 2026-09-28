package layout

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/giantswarm/gitops-commit/provenance"
)

var repo = provenance.Repository{Owner: "acme", Name: "fleet"}

const (
	parentKustomization = "# the org's clusters\napiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - cluster.yaml # the cluster\n"
	plainFile           = "clusters/org-acme/model-manager/qwen.yaml"
	encFile          = "clusters/org-acme/model-manager/qwen-secret.enc.yaml"
	dirKustomization    = "clusters/org-acme/model-manager/kustomization.yaml"
	parentPath          = "clusters/org-acme/kustomization.yaml"
)

func dir() Directory {
	return Directory{Location: provenance.Location{Repository: repo, Branch: "main", Directory: "clusters/org-acme"}, Name: "model-manager"}
}

// sopsYAML encrypts every secret file under clusters/ for a fresh age key;
// no private key is checked in.
func sopsYAML(t *testing.T) []byte {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return []byte("creation_rules:\n  - age: " + id.Recipient().String() + "\n    path_regex: clusters/.*(secret|credential|secrets/).*\n    encrypted_regex: ^(data|stringData)$\n")
}

func seed(t *testing.T, files map[string][]byte) *commit.Fake {
	t.Helper()
	f := commit.NewFake()
	f.AddBranch(repo, "main", files)
	return f
}

func actions(p *Plan) map[string]Action {
	out := map[string]Action{}
	for _, f := range p.Files {
		out[f.Path] = f.Action
	}
	return out
}

func write() map[string][]byte {
	return map[string][]byte{
		plainFile:  []byte("kind: ModelConfig\nmetadata:\n  name: qwen\n"),
		encFile: []byte("kind: Secret\nmetadata:\n  name: qwen\nstringData:\n  OPENAI_API_KEY: placeholder\n"),
	}
}

func TestBuildCreate(t *testing.T) {
	fake := seed(t, map[string][]byte{SOPSConfigFile: sopsYAML(t), parentPath: []byte(parentKustomization)})
	plan, err := Build(context.Background(), fake, dir(), write(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Action{plainFile: Add, encFile: Add, dirKustomization: Add, parentPath: Update}
	if got := actions(plan); !maps.Equal(got, want) {
		t.Fatalf("actions = %v, want %v", got, want)
	}
	change := plan.Change()
	if change.Location != dir().Location {
		t.Errorf("location = %v", change.Location)
	}
	if enc := change.Files[encFile]; !bytes.Contains(enc, []byte("sops:")) || bytes.Contains(enc, []byte("placeholder")) {
		t.Errorf("secret file not encrypted:\n%s", enc)
	}
	for _, f := range plan.Files {
		if f.Path == encFile && f.Content != nil {
			t.Error("a secret file's content is carried in the plan")
		}
	}
	res, err := Resources(change.Files[dirKustomization])
	if err != nil || strings.Join(res, ",") != "qwen-secret.enc.yaml,qwen.yaml" {
		t.Errorf("directory resources = %v, %v", res, err)
	}
	parent := string(change.Files[parentPath])
	for _, keep := range []string{"# the org's clusters", "cluster.yaml # the cluster", "- model-manager"} {
		if !strings.Contains(parent, keep) {
			t.Errorf("parent kustomization lost %q:\n%s", keep, parent)
		}
	}
}

func TestBuildRerunChangesNothing(t *testing.T) {
	base := map[string][]byte{SOPSConfigFile: sopsYAML(t), parentPath: []byte(parentKustomization)}
	first, err := Build(context.Background(), seed(t, base), dir(), write(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for p, c := range first.Change().Files {
		base[p] = c
	}
	again, err := Build(context.Background(), seed(t, base), dir(), write(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed() {
		t.Fatalf("re-run changes %v", again.Change().Files)
	}
	for _, f := range again.Files {
		if f.Action != Unchanged {
			t.Errorf("%s: %s, want unchanged", f.Path, f.Action)
		}
	}
}

func TestBuildRemoveLast(t *testing.T) {
	base := map[string][]byte{SOPSConfigFile: sopsYAML(t), parentPath: []byte(parentKustomization)}
	first, err := Build(context.Background(), seed(t, base), dir(), write(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for p, c := range first.Change().Files {
		base[p] = c
	}
	plan, err := Build(context.Background(), seed(t, base), dir(), nil, []string{plainFile, encFile, plainFile, "clusters/org-acme/model-manager/gone.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Action{plainFile: Remove, encFile: Remove, dirKustomization: Remove, parentPath: Update}
	if got := actions(plan); !maps.Equal(got, want) {
		t.Fatalf("actions = %v, want %v", got, want)
	}
	if got := string(plan.Change().Files[parentPath]); got != parentKustomization {
		t.Errorf("parent kustomization = %q, want the original back", got)
	}
}

func TestBuildRemoveOneKeepsDirectory(t *testing.T) {
	base := map[string][]byte{SOPSConfigFile: sopsYAML(t)}
	first, err := Build(context.Background(), seed(t, base), dir(), write(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for p, c := range first.Change().Files {
		base[p] = c
	}
	plan, err := Build(context.Background(), seed(t, base), dir(), nil, []string{plainFile})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Action{plainFile: Remove, dirKustomization: Update}
	if got := actions(plan); !maps.Equal(got, want) {
		t.Fatalf("actions = %v, want %v (no parent kustomization: none touched)", got, want)
	}
	if res, _ := Resources(plan.Change().Files[dirKustomization]); strings.Join(res, ",") != "qwen-secret.enc.yaml" {
		t.Errorf("directory resources = %v", res)
	}
}

func TestBuildSecretByDirectory(t *testing.T) {
	p := "clusters/org-acme/model-manager/secrets/token.yaml"
	plan, err := Build(context.Background(), seed(t, map[string][]byte{SOPSConfigFile: sopsYAML(t)}), dir(), map[string][]byte{p: []byte("stringData:\n  token: t0p\n")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c := plan.Change().Files[p]; bytes.Contains(c, []byte("t0p")) {
		t.Errorf("a file .sops.yaml makes secret by its path is written in plaintext:\n%s", c)
	}
}

func TestBuildRefusesSecretWithoutRecipients(t *testing.T) {
	for name, base := range map[string]map[string][]byte{
		"no .sops.yaml":  nil,
		"broken":         {SOPSConfigFile: []byte("creation_rules: [")},
		"no rule covers": {SOPSConfigFile: []byte("creation_rules:\n  - age: age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqs3290gq\n    path_regex: other/.*\n")},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Build(context.Background(), seed(t, base), dir(), write(), nil)
			var se *SecretError
			if !errors.As(err, &se) || len(se.Paths) != 1 || se.Paths[0] != encFile {
				t.Fatalf("err = %v, want a SecretError naming %s", err, encFile)
			}
		})
	}
}

func TestBuildRefusesPathsOutsideTheDirectory(t *testing.T) {
	for _, p := range []string{"clusters/org-acme/other.yaml", "clusters/org-acme/model-manager/../x.yaml", dirKustomization} {
		if _, err := Build(context.Background(), seed(t, nil), dir(), map[string][]byte{p: []byte("x: 1\n")}, nil); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
	if _, err := Build(context.Background(), seed(t, nil), Directory{Location: dir().Location, Name: "a/b"}, nil, nil); err == nil {
		t.Error("a directory name of two segments accepted")
	}
}

func TestBuildReturnsTheRemotesRefusal(t *testing.T) {
	fake := seed(t, nil)
	fake.Fail[commit.OpReadFile] = &commit.AuthError{Op: commit.OpReadFile, Status: 403}
	_, err := Build(context.Background(), fake, dir(), write(), nil)
	if !errors.Is(err, commit.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

func TestObjectFile(t *testing.T) {
	if got := dir().ObjectFile("Secret", "qwen"); got != encFile {
		t.Errorf("Secret file = %s", got)
	}
	if got := dir().ObjectFile("ModelConfig", "qwen"); got != plainFile {
		t.Errorf("ModelConfig file = %s", got)
	}
}

func TestWithResources(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"new file":      {"", newKustomization + "resources:\n  - a.yaml\n"},
		"comments only": {"# managed by hand\n", "# managed by hand\nresources:\n  - a.yaml\n"},
		"null list":     {"kind: Kustomization\nresources:\n", "kind: Kustomization\nresources:\n  - a.yaml\n"},
		"kept keys":     {"# head\nkind: Kustomization\nnamespace: org-acme # ns\nresources: [a.yaml]\n", "# head\nkind: Kustomization\nnamespace: org-acme # ns\nresources: [a.yaml]\n"},
		"flow list":     {"kind: Kustomization\nnamespace: org-acme # ns\nresources: [b.yaml]\n", "kind: Kustomization\nnamespace: org-acme # ns\nresources: [a.yaml]\n"},
	} {
		t.Run(name, func(t *testing.T) {
			var in []byte
			if tc.in != "" {
				in = []byte(tc.in)
			}
			out, err := WithResources(in, []string{"a.yaml"})
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.want {
				t.Errorf("got\n%s\nwant\n%s", out, tc.want)
			}
		})
	}
	if _, err := WithResources([]byte("- a\n"), nil); !errors.Is(err, errNoMapping) {
		t.Errorf("a list document: err = %v", err)
	}
}
