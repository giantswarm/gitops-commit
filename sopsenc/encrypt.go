// Package sopsenc encrypts a repository's secret files for the age recipients
// of the matching creation rule of its .sops.yaml. It holds public keys only:
// there is no private key and no decryption code path anywhere in the package
// (sopsenc's tests assert this for the whole module). Secret values it
// generates exist only in the encrypted output; the package logs nothing.
package sopsenc

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	sops "github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/aes"
	sopage "github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/stores/yaml"
	"github.com/getsops/sops/v3/version"
)

var (
	// ErrDuplicatePath is returned when a fileset names one path twice.
	ErrDuplicatePath = errors.New("duplicate file path")
	// ErrGeneratedInPlainFile is returned when a file that is not a secret file
	// declares generated values: a generated value only lives encrypted.
	ErrGeneratedInPlainFile = errors.New("generated values are only allowed in secret files")
	// ErrValueFrozen is returned when a new secret file shares a generated
	// value with a secret file that already exists in the repository. The
	// existing value cannot be reproduced without decryption, so the caller
	// has to reference the existing Secret instead.
	ErrValueFrozen = errors.New("generated value exists in an encrypted file already in the repository")
)

// File is one rendered file of a repository's fileset.
type File struct {
	// Path is repository-relative, the path .sops.yaml's path_regex sees.
	Path    string
	Content []byte
	// Generated declares the placeholders in Content that receive generated
	// values. Only a secret file may declare them.
	Generated []Generated
}

// IsSecretFile reports whether a path names a secret file by the file-naming
// convention: a base name containing "secret" or "credential". It is the
// naming contract for callers that render secret files, not the encryption
// decision: which files Encrypt encrypts follows the repository's .sops.yaml
// (Encryptor.IsSecretFile), whose path_regex rules match paths — a file under
// a secrets/ directory is a secret file whatever its name.
func IsSecretFile(p string) bool {
	base := path.Base(p)
	return strings.Contains(base, "secret") || strings.Contains(base, "credential")
}

// Encryptor encrypts secret files for one repository's .sops.yaml.
type Encryptor struct {
	config Config
}

// New parses the repository's .sops.yaml.
func New(sopsYAML []byte) (*Encryptor, error) {
	config, err := ParseConfig(sopsYAML)
	if err != nil {
		return nil, err
	}
	return &Encryptor{config: config}, nil
}

// IsSecretFile reports whether Encrypt treats the repository-relative path as
// a secret file, by the repository's .sops.yaml (Config.IsSecretFile).
func (e *Encryptor) IsSecretFile(path string) bool {
	return e.config.IsSecretFile(path)
}

// Encrypt returns the fileset ready to commit, keyed by repository-relative
// path. Files the repository's .sops.yaml does not make secret files
// (IsSecretFile) pass through unchanged. A secret file that exists in the
// repository (exists reports its path) is left as it is and absent from the
// result: it is never re-generated. Every other secret file has its generated
// values filled in — one value per name across all files — and is encrypted
// for the recipients of its creation rule; a secret file no rule covers is
// refused with ErrNoRule.
func (e *Encryptor) Encrypt(files []File, exists func(path string) bool) (map[string][]byte, error) {
	if err := e.validateFileset(files); err != nil {
		return nil, err
	}
	frozen := map[string]string{} // generated name -> existing file that holds it
	var pending []File
	result := make(map[string][]byte, len(files))
	for _, f := range files {
		switch {
		case !e.IsSecretFile(f.Path):
			result[f.Path] = f.Content
		case exists(f.Path):
			for _, g := range f.Generated {
				frozen[g.Name] = f.Path
			}
		default:
			pending = append(pending, f)
		}
	}
	values, err := generateValues(pending, frozen)
	if err != nil {
		return nil, err
	}
	for _, f := range pending {
		rule, err := e.config.Rule(f.Path)
		if err != nil {
			return nil, err
		}
		plaintext := f.Content
		for _, g := range f.Generated {
			plaintext = bytes.ReplaceAll(plaintext, []byte(g.Placeholder), []byte(values[g.Name]))
		}
		encrypted, err := encrypt(plaintext, rule)
		if err != nil {
			return nil, fmt.Errorf("encrypting %s: %w", f.Path, err)
		}
		result[f.Path] = encrypted
	}
	return result, nil
}

func (e *Encryptor) validateFileset(files []File) error {
	seen := make(map[string]struct{}, len(files))
	for _, f := range files {
		if _, dup := seen[f.Path]; dup {
			return fmt.Errorf("%w: %s", ErrDuplicatePath, f.Path)
		}
		seen[f.Path] = struct{}{}
		if len(f.Generated) > 0 && !e.IsSecretFile(f.Path) {
			return fmt.Errorf("%w: %s", ErrGeneratedInPlainFile, f.Path)
		}
		for _, g := range f.Generated {
			if err := g.validate(); err != nil {
				return fmt.Errorf("%s: %w", f.Path, err)
			}
			if !bytes.Contains(f.Content, []byte(g.Placeholder)) {
				return fmt.Errorf("%w: placeholder of %q not found in %s", ErrInvalidGenerated, g.Name, f.Path)
			}
		}
	}
	return nil
}

// generateValues draws one value per generated name across the pending files,
// refusing names whose value is frozen in an existing encrypted file and
// declarations of one name that disagree on shape.
func generateValues(pending []File, frozen map[string]string) (map[string]string, error) {
	declared := map[string]Generated{}
	var names []string
	for _, f := range pending {
		for _, g := range f.Generated {
			if holder, ok := frozen[g.Name]; ok {
				return nil, fmt.Errorf("%w: %q is in %s, needed by %s", ErrValueFrozen, g.Name, holder, f.Path)
			}
			if prev, ok := declared[g.Name]; ok {
				if !prev.sameShape(g) {
					return nil, fmt.Errorf("%w: %q declared as %s/%d and %s/%d", ErrInvalidGenerated, g.Name, prev.Kind, prev.Length, g.Kind, g.Length)
				}
				continue
			}
			declared[g.Name] = g
			names = append(names, g.Name)
		}
	}
	sort.Strings(names)
	values := make(map[string]string, len(names))
	for _, name := range names {
		value, err := declared[name].generate()
		if err != nil {
			return nil, err
		}
		values[name] = value
	}
	return values, nil
}

// encrypt produces a SOPS-encrypted YAML file for the rule's age recipients.
// Every scalar value the rule's encrypted/unencrypted settings select is
// encrypted; keys stay plaintext.
func encrypt(plaintext []byte, rule Rule) ([]byte, error) {
	masterKeys, err := sopage.MasterKeysFromRecipients(strings.Join(rule.Recipients, ","))
	if err != nil {
		return nil, fmt.Errorf("parsing age recipients: %w", err)
	}
	store := &yaml.Store{}
	branches, err := store.LoadPlainFile(plaintext)
	if err != nil {
		return nil, fmt.Errorf("loading plaintext YAML: %w", err)
	}
	keyGroup := make(sops.KeyGroup, len(masterKeys))
	for i, mk := range masterKeys {
		keyGroup[i] = mk
	}
	tree := sops.Tree{
		Branches: branches,
		Metadata: sops.Metadata{
			KeyGroups:         []sops.KeyGroup{keyGroup},
			Version:           version.Version,
			EncryptedRegex:    rule.EncryptedRegex,
			UnencryptedRegex:  rule.UnencryptedRegex,
			EncryptedSuffix:   rule.EncryptedSuffix,
			UnencryptedSuffix: rule.UnencryptedSuffix,
		},
	}
	dataKey, errs := tree.GenerateDataKey()
	if len(errs) > 0 {
		return nil, fmt.Errorf("generating data key: %v", errs)
	}
	cipher := aes.NewCipher()
	unencryptedMAC, err := tree.Encrypt(dataKey, cipher)
	if err != nil {
		return nil, fmt.Errorf("encrypting tree: %w", err)
	}
	tree.Metadata.LastModified = time.Now().UTC()
	tree.Metadata.MessageAuthenticationCode, err = cipher.Encrypt(unencryptedMAC, dataKey, tree.Metadata.LastModified.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("encrypting MAC: %w", err)
	}
	out, err := store.EmitEncryptedFile(tree)
	if err != nil {
		return nil, fmt.Errorf("emitting encrypted YAML: %w", err)
	}
	return out, nil
}
