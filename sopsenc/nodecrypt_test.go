package sopsenc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// forbiddenIdentifiers are the decryption symbols of the sops and age
// libraries and anything named after decryption.
var forbiddenIdentifiers = regexp.MustCompile(`(?i)decrypt|LoadEncryptedFile|ParsedIdentities|X25519Identity|ParseIdentities`)

// forbiddenImports are the packages a decryption path or a sops binary
// invocation would need; the module's non-test code has no use for them.
var forbiddenImports = map[string]string{
	"filippo.io/age":                        "age identities are private keys",
	"os/exec":                               "no sops binary invocation",
	"github.com/getsops/sops/v3/decrypt":    "decryption",
	"github.com/getsops/sops/v3/keyservice": "key service serves private keys",
}

// TestModuleHasNoDecryptionPath parses every non-test Go file of the module
// and fails on any decryption symbol, private-key type or forbidden import.
// The tests decrypt with the fixture key; the module never does.
func TestModuleHasNoDecryptionPath(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	fset := token.NewFileSet()
	var checked int
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name != "." && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		checked++
		file, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if why, forbidden := forbiddenImports[path]; forbidden {
				t.Errorf("%s imports %s (%s)", rel, path, why)
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && forbiddenIdentifiers.MatchString(id.Name) {
				t.Errorf("%s: identifier %q at %s", rel, id.Name, fset.Position(id.Pos()))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 4 {
		t.Fatalf("checked only %d non-test files; the walk missed the packages", checked)
	}
}
