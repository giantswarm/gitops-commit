package sopsenc

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
)

// Kind is the shape of a generated secret value.
type Kind string

const (
	// Base64 is Length random bytes in standard base64 with padding.
	Base64 Kind = "base64"
	// Alphanumeric is Length characters drawn from [A-Za-z0-9].
	Alphanumeric Kind = "alphanumeric"
)

const alphanumericAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// ErrInvalidGenerated is returned for a Generated declaration that is
// incomplete, inconsistent across files, or whose placeholder is absent from
// the file it is declared on.
var ErrInvalidGenerated = errors.New("invalid generated value declaration")

// Generated declares a secret value the package generates at commit time and
// writes in place of Placeholder in the declaring file. The same Name in two
// files is one value; its Kind and Length must agree wherever it is declared.
// The value exists only in the encrypted output.
type Generated struct {
	Name        string
	Placeholder string
	Kind        Kind
	Length      int
}

func (g Generated) validate() error {
	if g.Name == "" || g.Placeholder == "" || g.Length <= 0 {
		return fmt.Errorf("%w: name, placeholder and a positive length are required (name %q)", ErrInvalidGenerated, g.Name)
	}
	switch g.Kind {
	case Base64, Alphanumeric:
		return nil
	default:
		return fmt.Errorf("%w: unknown kind %q for %q", ErrInvalidGenerated, g.Kind, g.Name)
	}
}

// sameShape reports whether two declarations of one name agree.
func (g Generated) sameShape(other Generated) bool {
	return g.Kind == other.Kind && g.Length == other.Length
}

// generate draws a fresh value of the declared shape from crypto/rand.
func (g Generated) generate() (string, error) {
	switch g.Kind {
	case Base64:
		buf := make([]byte, g.Length)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generating %q: %w", g.Name, err)
		}
		return base64.StdEncoding.EncodeToString(buf), nil
	case Alphanumeric:
		out := make([]byte, g.Length)
		alphabetLen := big.NewInt(int64(len(alphanumericAlphabet)))
		for i := range out {
			n, err := rand.Int(rand.Reader, alphabetLen)
			if err != nil {
				return "", fmt.Errorf("generating %q: %w", g.Name, err)
			}
			out[i] = alphanumericAlphabet[n.Int64()]
		}
		return string(out), nil
	default:
		return "", fmt.Errorf("%w: unknown kind %q", ErrInvalidGenerated, g.Kind)
	}
}
