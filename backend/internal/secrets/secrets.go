// Package secrets encrypts values the platform stores but must not keep in
// plaintext, such as app env vars: a copy of the database alone reveals
// nothing.
//
// It's AES-256-GCM from the standard library. GCM is authenticated: a value
// that's been altered, or opened with the wrong key or associated data, fails
// to decrypt rather than decrypting to garbage. The key comes from the
// environment (PLATFORM_SECRET_KEY), never the database or the repo.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// KeySize is the key length in bytes (AES-256).
const KeySize = 32

// formatV1 marks a value sealed by this version of the code: a format byte,
// then GCM's random 12-byte nonce, then the ciphertext and its 16-byte tag.
// Storing the version means the format (or key, for a rotation) can change
// later while old values stay readable.
const formatV1 byte = 1

// ErrDecrypt means a value couldn't be decrypted: wrong key, wrong associated
// data, or the stored bytes were altered. Deliberately one error for all
// three, as GCM can't tell them apart.
var ErrDecrypt = errors.New("secrets: value could not be decrypted")

// Box seals and opens values with one key.
type Box struct {
	aead cipher.AEAD
}

// ParseKey decodes a hex key, as `openssl rand -hex 32` prints.
func ParseKey(s string) ([]byte, error) {
	key, err := hex.DecodeString(s)
	if err != nil || len(key) != KeySize {
		// The value itself stays out of the error: it's a secret.
		return nil, fmt.Errorf("want %d bytes as %d hex characters (openssl rand -hex %d)", KeySize, KeySize*2, KeySize)
	}
	return key, nil
}

// New returns a Box using key, which must be KeySize bytes.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secrets: key is %d bytes, want %d", len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext. associatedData isn't stored, but the same bytes
// must be passed to Open: it binds the value to its context (e.g. which app
// and env var it belongs to), so a value copied to another row can't be
// opened there.
//
// Each call uses a fresh random nonce, so sealing the same value twice gives
// different bytes, and identical secrets aren't visible as such.
func (b *Box) Seal(plaintext, associatedData []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	// Random 96-bit nonces are safe for far more values than the platform
	// will ever store under one key (NIST's limit is 2^32 per key).
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+b.aead.Overhead())
	out = append(out, formatV1)
	out = append(out, nonce...)
	return b.aead.Seal(out, nonce, plaintext, associatedData), nil
}

// Open decrypts a value from Seal, given the same associatedData.
func (b *Box) Open(sealed, associatedData []byte) ([]byte, error) {
	nonceSize := b.aead.NonceSize()
	if len(sealed) < 1+nonceSize+b.aead.Overhead() || sealed[0] != formatV1 {
		return nil, ErrDecrypt
	}
	nonce := sealed[1 : 1+nonceSize]
	plaintext, err := b.aead.Open(nil, nonce, sealed[1+nonceSize:], associatedData)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}
