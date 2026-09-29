// Package digest narrows OCI content digests (github.com/opencontainers/go-digest)
// to what Schepherd accepts: every wire format supports sha256 with lower-case
// hex only, and a well-formed digest of another algorithm is reported as
// unsupported rather than treated as an opaque string.
package digest

import (
	// go-digest leaves registering the hash implementation to its users.
	_ "crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"strings"

	godigest "github.com/opencontainers/go-digest"
)

// Algorithm is the only supported digest algorithm.
const Algorithm = string(godigest.SHA256)

const prefix = Algorithm + ":"

// ErrUnsupportedAlgorithm reports a well-formed digest with another algorithm.
var ErrUnsupportedAlgorithm = errors.New("unsupported digest algorithm")

// ErrInvalid reports a malformed digest string.
var ErrInvalid = errors.New("invalid digest")

// Validate checks that s is "sha256:" followed by 64 lowercase hex digits.
func Validate(s string) error {
	algorithm, encoded, ok := strings.Cut(s, ":")
	if !ok || algorithm == "" || encoded == "" {
		return fmt.Errorf("%w %q: expected sha256:<64 lowercase hex digits>", ErrInvalid, s)
	}

	if algorithm != Algorithm {
		// go-digest exports no grammar for the algorithm alone; with a
		// placeholder encoding its digest grammar checks just that part.
		if godigest.DigestRegexpAnchored.MatchString(algorithm + ":0") {
			return fmt.Errorf("%w %q in %q (only %s is supported)", ErrUnsupportedAlgorithm, algorithm, s, Algorithm)
		}

		return fmt.Errorf("%w %q: malformed algorithm", ErrInvalid, s)
	}

	switch err := godigest.Digest(s).Validate(); {
	case err == nil:
		return nil
	case errors.Is(err, godigest.ErrDigestInvalidLength):
		return fmt.Errorf("%w %q: expected 64 hex digits", ErrInvalid, s)
	default:
		return fmt.Errorf("%w %q: expected lowercase hex digits", ErrInvalid, s)
	}
}

// Hex returns the encoded part of a validated digest.
func Hex(s string) string {
	return strings.TrimPrefix(s, prefix)
}

// FromBytes returns the sha256 digest of b.
func FromBytes(b []byte) string {
	return godigest.SHA256.FromBytes(b).String()
}

// NewHash returns a hash for the supported algorithm.
func NewHash() hash.Hash {
	return godigest.SHA256.Hash()
}

// FromHash formats the current sum of h.
func FromHash(h hash.Hash) string {
	return godigest.NewDigest(godigest.SHA256, h).String()
}
