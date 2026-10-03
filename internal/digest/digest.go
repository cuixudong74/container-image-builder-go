// Package digest provides content-addressed digest helpers (SHA-256 based).
package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// Digest is a content digest string in the form "sha256:<hex>".
type Digest string

// Algorithm returns the digest algorithm prefix.
func (d Digest) Algorithm() string { return "sha256" }

// Hex returns the hex-encoded digest value.
func (d Digest) Hex() string {
	s := string(d)
	if len(s) > 7 && s[:7] == "sha256:" {
		return s[7:]
	}
	return s
}

// String returns the full "sha256:..." form.
func (d Digest) String() string { return string(d) }

// FromBytes computes the digest of b.
func FromBytes(b []byte) Digest {
	h := sha256.Sum256(b)
	return Digest("sha256:" + hex.EncodeToString(h[:]))
}

// FromString computes the digest of s.
func FromString(s string) Digest { return FromBytes([]byte(s)) }

// FromReader streams r and returns its digest.
func FromReader(r io.Reader) (Digest, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return Digest("sha256:" + hex.EncodeToString(h.Sum(nil))), nil
}

// FromFile computes the digest of the file at path.
func FromFile(path string) (Digest, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return FromReader(f)
}

// NewHasher returns a streaming hash writer.
func NewHasher() *Hasher { return &Hasher{h: sha256.New()} }

// Hasher is a streaming content hasher.
type Hasher struct {
	h interface {
		Write([]byte) (int, error)
		Sum([]byte) []byte
	}
}

// Write implements io.Writer.
func (h *Hasher) Write(p []byte) (int, error) { return h.h.Write(p) }

// Digest returns the current digest.
func (h *Hasher) Digest() Digest {
	return Digest("sha256:" + hex.EncodeToString(h.h.Sum(nil)))
}
