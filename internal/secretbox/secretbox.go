// Package secretbox seals values that must not be readable through SQL (the temporary secrets
// Zach gives a workflow run) with AES-256-GCM, under the server's INTELLIGENCE_SECRETS_KEY.
// Each sealed value is bound to the row it belongs to by its additional data, so a ciphertext
// copied into another row does not open.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
)

// Box seals and opens values with one key.
type Box struct {
	aead cipher.AEAD
}

// New reads the key: 32 random bytes in base64 (openssl rand -base64 32).
func New(b64 string) (*Box, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(key) != 32 {
		return nil, errors.New("INTELLIGENCE_SECRETS_KEY must be 32 random bytes in base64 (openssl rand -base64 32)")
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

// Seal is a fresh random nonce followed by plain sealed with aad as additional data.
func (b *Box) Seal(plain []byte, aad string) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize(), b.aead.NonceSize()+len(plain)+b.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plain, []byte(aad)), nil
}

// ErrOpen is any failure to open: wrong key, wrong row or tampered bytes. It never says which.
var ErrOpen = errors.New("cannot open the sealed secret (INTELLIGENCE_SECRETS_KEY changed, or the row was altered)")

// Open reverses Seal for the same aad.
func (b *Box) Open(sealed []byte, aad string) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n+b.aead.Overhead() {
		return nil, ErrOpen
	}
	plain, err := b.aead.Open(nil, sealed[:n], sealed[n:], []byte(aad))
	if err != nil {
		return nil, ErrOpen
	}
	return plain, nil
}
