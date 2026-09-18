package sopsenc

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/getsops/sops/v3/aes"
	sopage "github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/stores/yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

const (
	dexSecretPath      = "management-clusters/mc1/apps/dex-app/secret-values.yaml"
	consumerSecretPath = "management-clusters/mc1/apps/mcp/secret.yaml"
	plainPath          = "management-clusters/mc1/apps/mcp/values.yaml"
	sharedName         = "dex-client"
	placeholderDex     = "__DEX_CLIENT_SECRET__"
	placeholderValkey  = "__VALKEY__"
	placeholderP       = "__P__"
)

// fixture is a fresh age key pair with the testdata .sops.yaml encrypting for it.
type fixture struct {
	identity  *age.X25519Identity
	other     *age.X25519Identity
	sopsYAML  []byte
	encryptor *Encryptor
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/sops.yaml")
	if err != nil {
		t.Fatal(err)
	}
	sopsYAML := bytes.ReplaceAll(raw, []byte("AGE_RECIPIENT"), []byte(identity.Recipient().String()))
	sopsYAML = bytes.ReplaceAll(sopsYAML, []byte("OTHER_RECIPIENT"), []byte(other.Recipient().String()))
	encryptor, err := New(sopsYAML)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return fixture{identity: identity, other: other, sopsYAML: sopsYAML, encryptor: encryptor}
}

// decrypt is the test's own decryption with the fixture's private key; the
// package under test has none.
func decrypt(t *testing.T, ciphertext []byte, identity *age.X25519Identity) []byte {
	t.Helper()
	store := &yaml.Store{}
	tree, err := store.LoadEncryptedFile(ciphertext)
	if err != nil {
		t.Fatalf("loading encrypted file: %v", err)
	}
	var ids sopage.ParsedIdentities
	if err := ids.Import(identity.String()); err != nil {
		t.Fatal(err)
	}
	var dataKey []byte
	for _, group := range tree.Metadata.KeyGroups {
		for _, key := range group {
			mk, ok := key.(*sopage.MasterKey)
			if !ok {
				continue
			}
			ids.ApplyToMasterKey(mk)
			if dk, err := mk.Decrypt(); err == nil {
				dataKey = dk
			}
		}
	}
	if dataKey == nil {
		t.Fatal("fixture key cannot open the data key")
	}
	if _, err := tree.Decrypt(dataKey, aes.NewCipher()); err != nil {
		t.Fatalf("decrypting tree: %v", err)
	}
	plain, err := store.EmitPlainFile(tree.Branches)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

func secretYAML(name, key, value string) []byte {
	return []byte(fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\n  namespace: mc1\nstringData:\n  %s: %s\n", name, key, value))
}

func stringDataValue(t *testing.T, plain []byte, key string) string {
	t.Helper()
	var doc struct {
		Metadata   struct{ Name string } `yaml:"metadata"`
		StringData map[string]string     `yaml:"stringData"`
	}
	if err := yamlv3.Unmarshal(plain, &doc); err != nil {
		t.Fatalf("plaintext is not the Secret: %v\n%s", err, plain)
	}
	return doc.StringData[key]
}

func nothingExists(string) bool { return false }

func TestEncryptSharedValueGeneratedOnceAndDecryptsWithFixtureKey(t *testing.T) {
	fx := newFixture(t)
	files := []File{
		{Path: plainPath, Content: []byte("replicas: 1\n")},
		{
			Path:      dexSecretPath,
			Content:   secretYAML("dex-app", "clientSecret", placeholderDex),
			Generated: []Generated{{Name: sharedName, Placeholder: placeholderDex, Kind: Base64, Length: 32}},
		},
		{
			Path:    consumerSecretPath,
			Content: []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: mcp\n  namespace: mc1\nstringData:\n  clientSecret: " + placeholderDex + "\n  valkeyPassword: " + placeholderValkey + "\n"),
			Generated: []Generated{
				{Name: sharedName, Placeholder: placeholderDex, Kind: Base64, Length: 32},
				{Name: "valkey-password", Placeholder: placeholderValkey, Kind: Alphanumeric, Length: 20},
			},
		},
	}

	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	out, err := fx.encryptor.Encrypt(files, nothingExists)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("Encrypt returned %d files, want 3", len(out))
	}
	if string(out[plainPath]) != "replicas: 1\n" {
		t.Fatalf("plain file changed: %q", out[plainPath])
	}

	dexPlain := decrypt(t, out[dexSecretPath], fx.identity)
	consumerPlain := decrypt(t, out[consumerSecretPath], fx.identity)
	secret := stringDataValue(t, dexPlain, "clientSecret")
	if secret == "" || secret == placeholderDex {
		t.Fatalf("dex clientSecret not generated: %q", secret)
	}
	if got := stringDataValue(t, consumerPlain, "clientSecret"); got != secret {
		t.Fatalf("shared value differs: dex %q, consumer %q", secret, got)
	}
	password := stringDataValue(t, consumerPlain, "valkeyPassword")
	if len(password) != 20 || strings.Trim(password, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789") != "" {
		t.Fatalf("valkey password is not 20 alphanumeric chars: %q", password)
	}
	if !strings.Contains(string(dexPlain), "name: dex-app") {
		t.Fatalf("metadata was not preserved:\n%s", dexPlain)
	}

	for p, content := range out {
		if p == plainPath {
			continue
		}
		for _, value := range []string{secret, password, placeholderDex, placeholderValkey} {
			if bytes.Contains(content, []byte(value)) {
				t.Errorf("%s carries %q in the encrypted output", p, value)
			}
		}
		if !bytes.Contains(content, []byte("sops:")) || !bytes.Contains(content, []byte(fx.identity.Recipient().String())) {
			t.Errorf("%s is not a sops file for the fixture recipient", p)
		}
		if !bytes.Contains(content, []byte("name: dex-app")) && !bytes.Contains(content, []byte("name: mcp")) {
			t.Errorf("%s: metadata (outside encrypted_regex) should stay plaintext", p)
		}
	}
	for _, value := range []string{secret, password} {
		if strings.Contains(logs.String(), value) {
			t.Errorf("a log line carries a secret value")
		}
	}
	if logs.Len() != 0 {
		t.Errorf("the package logged: %s", logs.String())
	}
}

func TestEncryptUsesTheMatchingRuleRecipients(t *testing.T) {
	fx := newFixture(t)
	otherPath := "management-clusters/other/secret.yaml"
	out, err := fx.encryptor.Encrypt([]File{{Path: otherPath, Content: secretYAML("x", "k", "v")}}, nothingExists)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if got := stringDataValue(t, decrypt(t, out[otherPath], fx.other), "k"); got != "v" {
		t.Fatalf("decrypted with the other rule's key: %q", got)
	}
	if bytes.Contains(out[otherPath], []byte(fx.identity.Recipient().String())) {
		t.Fatal("file for the other rule was encrypted for the first rule's recipient")
	}
}

func TestEncryptKeepsExistingSecretFilesAndRefusesFrozenValues(t *testing.T) {
	fx := newFixture(t)
	dex := File{Path: dexSecretPath, Content: secretYAML("dex-app", "clientSecret", placeholderDex),
		Generated: []Generated{{Name: sharedName, Placeholder: placeholderDex, Kind: Base64, Length: 32}}}
	exists := func(p string) bool { return p == dexSecretPath }

	out, err := fx.encryptor.Encrypt([]File{dex, {Path: plainPath, Content: []byte("a: b\n")}}, exists)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, present := out[dexSecretPath]; present {
		t.Fatal("existing secret file was re-generated")
	}
	if len(out) != 1 {
		t.Fatalf("Encrypt returned %d files, want the plain file only", len(out))
	}

	consumer := File{Path: consumerSecretPath, Content: secretYAML("mcp", "clientSecret", placeholderDex),
		Generated: []Generated{{Name: sharedName, Placeholder: placeholderDex, Kind: Base64, Length: 32}}}
	_, err = fx.encryptor.Encrypt([]File{dex, consumer}, exists)
	if !errors.Is(err, ErrValueFrozen) {
		t.Fatalf("shared value with an existing file: error = %v, want ErrValueFrozen", err)
	}
}

func TestEncryptValidation(t *testing.T) {
	fx := newFixture(t)
	gen := Generated{Name: "n", Placeholder: placeholderP, Kind: Base64, Length: 8}
	cases := map[string]struct {
		files []File
		want  error
	}{
		"duplicate path": {files: []File{{Path: plainPath}, {Path: plainPath}}, want: ErrDuplicatePath},
		"generated in plain file": {files: []File{{Path: plainPath, Content: []byte(placeholderP), Generated: []Generated{gen}}},
			want: ErrGeneratedInPlainFile},
		"placeholder absent": {files: []File{{Path: dexSecretPath, Content: []byte("a: b\n"), Generated: []Generated{gen}}},
			want: ErrInvalidGenerated},
		"unknown kind": {files: []File{{Path: dexSecretPath, Content: []byte("a: __P__\n"),
			Generated: []Generated{{Name: "n", Placeholder: placeholderP, Kind: "hex", Length: 8}}}}, want: ErrInvalidGenerated},
		"zero length": {files: []File{{Path: dexSecretPath, Content: []byte("a: __P__\n"),
			Generated: []Generated{{Name: "n", Placeholder: placeholderP, Kind: Base64}}}}, want: ErrInvalidGenerated},
		"shape disagrees": {files: []File{
			{Path: dexSecretPath, Content: []byte("a: __P__\n"), Generated: []Generated{gen}},
			{Path: consumerSecretPath, Content: []byte("a: __P__\n"), Generated: []Generated{{Name: "n", Placeholder: placeholderP, Kind: Alphanumeric, Length: 8}}},
		}, want: ErrInvalidGenerated},
		"no matching rule": {files: []File{{Path: "installations/x/secret.yaml", Content: []byte("a: b\n")}}, want: ErrNoRule},
	}
	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			_, err := fx.encryptor.Encrypt(tc.files, nothingExists)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParseConfigRejectsBadRules(t *testing.T) {
	for label, yamlText := range map[string]string{
		"empty":             "creation_rules: []\n",
		"no recipients":     "creation_rules:\n  - path_regex: x\n",
		"bad recipient":     "creation_rules:\n  - age: not-an-age-key\n",
		"bad regex":         "creation_rules:\n  - age: age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqs3290gq\n    path_regex: '('\n",
		"pgp only":          "creation_rules:\n  - pgp: ABCDEF\n",
		"not a sops config": "just: text\n",
	} {
		t.Run(label, func(t *testing.T) {
			if _, err := New([]byte(yamlText)); err == nil {
				t.Fatal("New accepted an invalid .sops.yaml")
			}
		})
	}
}

func TestIsSecretFile(t *testing.T) {
	for p, want := range map[string]bool{
		"management-clusters/mc1/secret.yaml":                         true,
		"management-clusters/mc1/apps/x/secret-values.yaml.patch":     true,
		"management-clusters/mc1/apps/x/aws-credentials.yaml":         true,
		"management-clusters/mc1/apps/x/values.yaml":                  false,
		"management-clusters/secretive-mc/apps/x/values.yaml":         false,
		"management-clusters/mc1/apps/x/configmap.yaml":               false,
		"management-clusters/mc1/kustomization.yaml":                  false,
		"management-clusters/mc1/apps/x/Secret.yaml":                  false,
		"management-clusters/mc1/apps/x/credential-values.yaml.patch": true,
	} {
		if got := IsSecretFile(p); got != want {
			t.Errorf("IsSecretFile(%q) = %v, want %v", p, got, want)
		}
	}
}
