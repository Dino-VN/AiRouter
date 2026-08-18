// Package cryptobox seals upstream provider credentials before they are written
// to the database. Refresh tokens are long-lived bearer credentials for a user's
// ChatGPT / Google account, so they never sit in the database as plaintext.
package cryptobox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// Box performs authenticated encryption with a fixed 256-bit key.
type Box struct {
	aead cipher.AEAD
}

// New builds a Box from a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("cryptobox: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("cryptobox: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cryptobox: new gcm: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext and returns nonce||ciphertext.
func (b *Box) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("cryptobox: nonce: %w", err)
	}
	return b.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open reverses Seal.
func (b *Box) Open(sealed []byte) ([]byte, error) {
	nonceSize := b.aead.NonceSize()
	if len(sealed) < nonceSize {
		return nil, errors.New("cryptobox: sealed value too short")
	}
	nonce, ciphertext := sealed[:nonceSize], sealed[nonceSize:]
	plaintext, err := b.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("cryptobox: open: %w", err)
	}
	return plaintext, nil
}
