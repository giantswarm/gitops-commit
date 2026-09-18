package sopsenc

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
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
	// directorySecretPath is a secret file by its directory only: the rule's
	// path_regex matches "secrets/", the base name says nothing.
	directorySecretPath      = "management-clusters/mc1/extras/agent-platform/secrets/kagent-anthropic-key.yaml"
	secretsKustomizationPath = "management-clusters/mc1/extras/agent-platform/secrets/kustomization.yaml"
	sharedName               = "dex-client"
	placeholderDex           = "__DEX_CLIENT_SECRET__"
	placeholderValkey        = "__VALKEY__"
	placeholderKey           = "__KEY__"
	placeholderP             = "__P__"
	// pluginKeysSecretPath carries a key pair inside a nested values document,
	// the shape of a HelmRelease's valuesFrom Secret; pluginPublicPath is a
	// plain file carrying the pair's public half.
	pluginKeysSecretPath = "management-clusters/mc1/apps/portal/plugin-keys-secret.yaml"
	pluginPublicPath     = "management-clusters/mc1/apps/portal/configmap.yaml"
	pairName             = "plugin-keys"
	placeholderPrivate   = "__PRIVATE__"
	placeholderPublic    = "__PUBLIC__"
)

func keyPair(half Half, placeholder string) Generated {
	return Generated{Name: pairName, Placeholder: placeholder, Kind: KeyPairES256, Half: half}
}

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
	return mapValue(t, plain, "stringData", key)
}

// mapValue is doc's section.key, read back through the YAML parser.
func mapValue(t *testing.T, doc []byte, section, key string) string {
	t.Helper()
	var parsed map[string]any
	if err := yamlv3.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("not a YAML document: %v\n%s", err, doc)
	}
	values, _ := parsed[section].(map[string]any)
	value, _ := values[key].(string)
	return value
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

	// A key pair's public half cannot be derived from a private half that is
	// already encrypted: the pair is frozen as a whole.
	keys := File{Path: pluginKeysSecretPath, Content: []byte("a: " + placeholderPrivate + "\n"), Generated: []Generated{keyPair(Private, placeholderPrivate)}}
	publicOnly := File{Path: pluginPublicPath, Content: []byte("a: " + placeholderPublic + "\n"), Generated: []Generated{keyPair(Public, placeholderPublic)}}
	_, err = fx.encryptor.Encrypt([]File{keys, publicOnly}, func(p string) bool { return p == pluginKeysSecretPath })
	if !errors.Is(err, ErrValueFrozen) {
		t.Fatalf("public half of an existing key pair: error = %v, want ErrValueFrozen", err)
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
		"generated in a secrets/ kustomization": {files: []File{{Path: secretsKustomizationPath, Content: []byte(placeholderP), Generated: []Generated{gen}}},
			want: ErrGeneratedInPlainFile},
		"private half in a plain file": {files: []File{{Path: plainPath, Content: []byte(placeholderPrivate), Generated: []Generated{keyPair(Private, placeholderPrivate)}}},
			want: ErrGeneratedInPlainFile},
		"key pair without a half": {files: []File{{Path: dexSecretPath, Content: []byte("a: __P__\n"),
			Generated: []Generated{{Name: "n", Placeholder: placeholderP, Kind: KeyPairES256}}}}, want: ErrInvalidGenerated},
		"key pair with a length": {files: []File{{Path: dexSecretPath, Content: []byte("a: __P__\n"),
			Generated: []Generated{{Name: "n", Placeholder: placeholderP, Kind: KeyPairES256, Half: Private, Length: 32}}}}, want: ErrInvalidGenerated},
		"half on a value": {files: []File{{Path: dexSecretPath, Content: []byte("a: __P__\n"),
			Generated: []Generated{{Name: "n", Placeholder: placeholderP, Kind: Base64, Length: 8, Half: Public}}}}, want: ErrInvalidGenerated},
		"public half without a private half": {files: []File{{Path: dexSecretPath, Content: []byte("a: " + placeholderPublic + "\n"), Generated: []Generated{keyPair(Public, placeholderPublic)}}},
			want: ErrInvalidGenerated},
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

func TestEncryptDecidesSecretFilesByThePathRules(t *testing.T) {
	fx := newFixture(t)
	key := File{
		Path:      directorySecretPath,
		Content:   secretYAML("kagent-anthropic-key", "apiKey", placeholderKey),
		Generated: []Generated{{Name: "anthropic-key", Placeholder: placeholderKey, Kind: Alphanumeric, Length: 16}},
	}
	kustomization := File{Path: secretsKustomizationPath, Content: []byte("resources:\n  - kagent-anthropic-key.yaml\n")}

	out, err := fx.encryptor.Encrypt([]File{kustomization, key}, nothingExists)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if string(out[secretsKustomizationPath]) != string(kustomization.Content) {
		t.Fatalf("the kustomization in secrets/ did not stay plaintext: %q", out[secretsKustomizationPath])
	}
	if bytes.Contains(out[directorySecretPath], []byte(placeholderKey)) || !bytes.Contains(out[directorySecretPath], []byte("sops:")) {
		t.Fatalf("file matched by its directory was not encrypted:\n%s", out[directorySecretPath])
	}
	apiKey := stringDataValue(t, decrypt(t, out[directorySecretPath], fx.identity), "apiKey")
	if len(apiKey) != 16 || apiKey == placeholderKey {
		t.Fatalf("apiKey not generated: %q", apiKey)
	}

	out, err = fx.encryptor.Encrypt([]File{kustomization, key}, func(p string) bool { return p == directorySecretPath })
	if err != nil {
		t.Fatalf("Encrypt with the key file existing: %v", err)
	}
	if _, present := out[directorySecretPath]; present || len(out) != 1 {
		t.Fatalf("existing file matched by its directory was re-generated: %v", out)
	}
}

func TestEncryptorIsSecretFile(t *testing.T) {
	fx := newFixture(t)
	for p, want := range map[string]bool{
		directorySecretPath:      true,  // by its directory
		secretsKustomizationPath: false, // kustomize reads it before decryption
		"management-clusters/mc1/extras/agent-platform/secrets/Kustomization": false,
		"management-clusters/mc1/extras/agent-platform/kustomization.yaml":    false,
		dexSecretPath:                 true,
		plainPath:                     false,
		"installations/x/secret.yaml": true, // by name; Encrypt refuses it with ErrNoRule
		"installations/x/values.yaml": false,
	} {
		if got := fx.encryptor.IsSecretFile(p); got != want {
			t.Errorf("IsSecretFile(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestCatchAllRuleNamesRecipientsWithoutMakingFilesSecret(t *testing.T) {
	fx := newFixture(t)
	enc, err := New([]byte("creation_rules:\n  - age: " + fx.identity.Recipient().String() + "\n"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if enc.IsSecretFile(plainPath) || !enc.IsSecretFile(consumerSecretPath) {
		t.Fatal("a rule without path_regex decided which files are secret files")
	}
	out, err := enc.Encrypt([]File{{Path: plainPath, Content: []byte("a: b\n")}, {Path: consumerSecretPath, Content: secretYAML("mcp", "k", "v")}}, nothingExists)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if string(out[plainPath]) != "a: b\n" {
		t.Fatalf("plain file changed under a catch-all rule: %q", out[plainPath])
	}
	if got := stringDataValue(t, decrypt(t, out[consumerSecretPath], fx.identity), "k"); got != "v" {
		t.Fatalf("secret file not encrypted for the catch-all rule: %q", got)
	}
}

func TestEncryptKeyPairHalvesMatchAcrossFilesAndThePrivateHalfStaysEncrypted(t *testing.T) {
	fx := newFixture(t)
	secret := []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: plugin-keys\n  namespace: mc1\nstringData:\n  values: |\n    pluginKeys:\n      - keyId: portal\n        publicKey: " + placeholderPublic + "\n        privateKey: " + placeholderPrivate + "\n")
	configMap := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: plugin-public-key\n  namespace: mc1\ndata:\n  public.key: " + placeholderPublic + "\n")
	files := []File{
		{Path: pluginKeysSecretPath, Content: secret, Generated: []Generated{keyPair(Public, placeholderPublic), keyPair(Private, placeholderPrivate)}},
		{Path: pluginPublicPath, Content: configMap, Generated: []Generated{keyPair(Public, placeholderPublic)}},
	}

	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	out, err := fx.encryptor.Encrypt(files, nothingExists)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("Encrypt returned %d files, want 2", len(out))
	}

	public := parsePublicKey(t, mapValue(t, out[pluginPublicPath], "data", "public.key"))

	var nested struct {
		PluginKeys []struct {
			PublicKey  string `yaml:"publicKey"`
			PrivateKey string `yaml:"privateKey"`
		} `yaml:"pluginKeys"`
	}
	values := stringDataValue(t, decrypt(t, out[pluginKeysSecretPath], fx.identity), "values")
	if err := yamlv3.Unmarshal([]byte(values), &nested); err != nil || len(nested.PluginKeys) != 1 {
		t.Fatalf("the nested values document did not survive the halves: %v\n%s", err, values)
	}
	private := parsePrivateKey(t, nested.PluginKeys[0].PrivateKey)
	if !private.PublicKey.Equal(public) {
		t.Fatal("the plain file's public half is not the counterpart of the encrypted private half")
	}
	if !parsePublicKey(t, nested.PluginKeys[0].PublicKey).Equal(public) {
		t.Fatal("the secret file's public half differs from the plain file's")
	}

	privateBody := strings.Split(nested.PluginKeys[0].PrivateKey, "\n")[1]
	for p, content := range out {
		for _, value := range []string{privateBody, placeholderPrivate, placeholderPublic} {
			if bytes.Contains(content, []byte(value)) {
				t.Errorf("%s carries %q", p, value)
			}
		}
	}
	if logs.Len() != 0 {
		t.Errorf("the package logged: %s", logs.String())
	}
}

func parsePublicKey(t *testing.T, pemText string) *ecdsa.PublicKey {
	t.Helper()
	block, _ := pem.Decode([]byte(pemText))
	if block == nil || block.Type != "PUBLIC KEY" {
		t.Fatalf("not a SubjectPublicKeyInfo PEM: %q", pemText)
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("parsing the public key: %v", err)
	}
	public, ok := key.(*ecdsa.PublicKey)
	if !ok || public.Curve != elliptic.P256() {
		t.Fatalf("public key is %T on %v, want ECDSA P-256", key, public.Curve)
	}
	return public
}

func parsePrivateKey(t *testing.T, pemText string) *ecdsa.PrivateKey {
	t.Helper()
	block, _ := pem.Decode([]byte(pemText))
	if block == nil || block.Type != "PRIVATE KEY" {
		t.Fatalf("not a PKCS #8 PEM: %q", pemText)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parsing the private key: %v", err)
	}
	private, ok := key.(*ecdsa.PrivateKey)
	if !ok || private.Curve != elliptic.P256() {
		t.Fatalf("private key is %T, want ECDSA P-256", key)
	}
	return private
}
