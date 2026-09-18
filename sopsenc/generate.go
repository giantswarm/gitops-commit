package sopsenc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Kind is the shape of a generated secret value.
type Kind string

const (
	// Base64 is Length random bytes in standard base64 with padding.
	Base64 Kind = "base64"
	// Alphanumeric is Length characters drawn from [A-Za-z0-9].
	Alphanumeric Kind = "alphanumeric"
	// KeyPairES256 is an ECDSA P-256 key pair — the pair behind the JWS
	// algorithm ES256 — drawn once per name; each declaration names the Half
	// its placeholder receives. The private half is a PKCS #8 PEM, the public
	// half a SubjectPublicKeyInfo PEM: what `openssl pkey` writes and jose's
	// importPKCS8/importSPKI read. A half replaces its placeholder as a
	// double-quoted YAML scalar whose line breaks are \n escapes, so the
	// placeholder stands as the whole, unquoted value of a key and the PEM
	// survives nesting inside a values document. Length is not used.
	KeyPairES256 Kind = "keypair-es256"
)

// Half is the half of a key pair a placeholder receives.
type Half string

const (
	// Private is the private key; only a secret file may receive it.
	Private Half = "private"
	// Public is the public key; a plain file may receive it too.
	Public Half = "public"
)

const alphanumericAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// ErrInvalidGenerated is returned for a Generated declaration that is
// incomplete, inconsistent across files, or whose placeholder is absent from
// the file it is declared on.
var ErrInvalidGenerated = errors.New("invalid generated value declaration")

// Generated declares a secret value the package generates at commit time and
// writes in place of Placeholder in the declaring file. The same Name in two
// files is one value; its Kind and Length must agree wherever it is declared.
// A key pair's declarations name the Half each placeholder receives; its
// private half exists only in the encrypted output.
type Generated struct {
	Name        string
	Placeholder string
	Kind        Kind
	// Length is the size of a Base64 or Alphanumeric value; a key pair has none.
	Length int
	// Half is the half of a KeyPairES256 the placeholder receives; a Base64 or
	// Alphanumeric value has none.
	Half Half
}

func (g Generated) validate() error {
	if g.Name == "" || g.Placeholder == "" {
		return fmt.Errorf("%w: name and placeholder are required (name %q)", ErrInvalidGenerated, g.Name)
	}
	switch g.Kind {
	case Base64, Alphanumeric:
		if g.Length <= 0 {
			return fmt.Errorf("%w: %q needs a positive length", ErrInvalidGenerated, g.Name)
		}
		if g.Half != "" {
			return fmt.Errorf("%w: %q is a %s value, not a key pair, and names a half", ErrInvalidGenerated, g.Name, g.Kind)
		}
		return nil
	case KeyPairES256:
		if g.Length != 0 {
			return fmt.Errorf("%w: key pair %q has no length", ErrInvalidGenerated, g.Name)
		}
		if g.Half != Private && g.Half != Public {
			return fmt.Errorf("%w: key pair %q must name the half %q or %q, not %q", ErrInvalidGenerated, g.Name, Private, Public, g.Half)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown kind %q for %q", ErrInvalidGenerated, g.Kind, g.Name)
	}
}

// sameShape reports whether two declarations of one name agree.
func (g Generated) sameShape(other Generated) bool {
	return g.Kind == other.Kind && g.Length == other.Length
}

// material is what one generated name yields: a value, or the two halves of a
// key pair.
type material struct {
	value, private, public string
}

// pick is the string the declaration's placeholder receives.
func (g Generated) pick(m material) string {
	switch g.Half {
	case Private:
		return m.private
	case Public:
		return m.public
	}
	return m.value
}

// generate draws fresh material of the declared shape from crypto/rand.
func (g Generated) generate() (material, error) {
	switch g.Kind {
	case Base64:
		buf := make([]byte, g.Length)
		if _, err := rand.Read(buf); err != nil {
			return material{}, fmt.Errorf("generating %q: %w", g.Name, err)
		}
		return material{value: base64.StdEncoding.EncodeToString(buf)}, nil
	case Alphanumeric:
		out := make([]byte, g.Length)
		alphabetLen := big.NewInt(int64(len(alphanumericAlphabet)))
		for i := range out {
			n, err := rand.Int(rand.Reader, alphabetLen)
			if err != nil {
				return material{}, fmt.Errorf("generating %q: %w", g.Name, err)
			}
			out[i] = alphanumericAlphabet[n.Int64()]
		}
		return material{value: string(out)}, nil
	case KeyPairES256:
		return generateKeyPairES256(g.Name)
	default:
		return material{}, fmt.Errorf("%w: unknown kind %q", ErrInvalidGenerated, g.Kind)
	}
}

// generateKeyPairES256 draws an ECDSA P-256 key pair and encodes each half as
// a PEM inside a one-line YAML scalar.
func generateKeyPairES256(name string) (material, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return material{}, fmt.Errorf("generating %q: %w", name, err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return material{}, fmt.Errorf("encoding the private key of %q: %w", name, err)
	}
	public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return material{}, fmt.Errorf("encoding the public key of %q: %w", name, err)
	}
	return material{
		private: yamlScalar(pemBlock("PRIVATE KEY", private)),
		public:  yamlScalar(pemBlock("PUBLIC KEY", public)),
	}, nil
}

func pemBlock(kind string, der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
}

// yamlScalar is s as a double-quoted YAML scalar: its line breaks become \n
// escapes, so the value stays on one line wherever the placeholder stands.
func yamlScalar(s string) string {
	return `"` + strings.ReplaceAll(s, "\n", `\n`) + `"`
}
