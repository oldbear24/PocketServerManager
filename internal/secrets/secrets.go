// Package secrets encrypts values at rest with the panel's master key.
//
// The master key lives in a file outside the database. Sealed values carry a
// version prefix so that they can be recognised and the format can change.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	// KeySize is the size of the master key in bytes (AES-256).
	KeySize = 32

	prefix = "pcs1:"
)

// ErrKeyMissing is returned by LoadKeyFile when the key file does not exist.
var ErrKeyMissing = errors.New("master key file does not exist")

// Box seals and opens values with one master key. It is safe for concurrent use.
type Box struct {
	aead cipher.AEAD
}

// New creates a Box from a raw 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("master key must be %d bytes, got %d", KeySize, len(key))
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

// Seal encrypts plaintext and returns a printable value safe to store in the database.
func (b *Box) Seal(plaintext []byte) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}

	sealed := b.aead.Seal(nonce, nonce, plaintext, nil)

	return prefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// SealString is Seal for strings.
func (b *Box) SealString(plaintext string) (string, error) {
	return b.Seal([]byte(plaintext))
}

// Open decrypts a value produced by Seal. It fails if the value was tampered
// with or was sealed with a different key.
func (b *Box) Open(sealed string) ([]byte, error) {
	if !IsSealed(sealed) {
		return nil, errors.New("value is not sealed")
	}

	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(sealed, prefix))
	if err != nil {
		return nil, fmt.Errorf("malformed sealed value: %w", err)
	}

	nonceSize := b.aead.NonceSize()
	if len(raw) < nonceSize {
		return nil, errors.New("malformed sealed value: too short")
	}

	plaintext, err := b.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		return nil, errors.New("cannot decrypt value: wrong master key or corrupted data")
	}

	return plaintext, nil
}

// OpenString is Open for strings.
func (b *Box) OpenString(sealed string) (string, error) {
	plaintext, err := b.Open(sealed)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// IsSealed reports whether a value looks like the output of Seal.
func IsSealed(value string) bool {
	return strings.HasPrefix(value, prefix)
}

// GenerateKey returns a new random master key.
func GenerateKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

// WriteKeyFile generates a new master key and stores it at path. It never
// overwrites an existing file, because that would make every secret unreadable.
func WriteKeyFile(path string) error {
	key, err := GenerateKey()
	if err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("refusing to overwrite the existing key file %s", path)
		}
		return err
	}
	defer f.Close()

	_, err = f.WriteString(hex.EncodeToString(key) + "\n")

	return err
}

// LoadKeyFile reads a master key written by WriteKeyFile (hex or base64).
func LoadKeyFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrKeyMissing, path)
		}
		return nil, err
	}

	text := strings.TrimSpace(string(raw))

	if key, err := hex.DecodeString(text); err == nil && len(key) == KeySize {
		return key, nil
	}
	if key, err := base64.StdEncoding.DecodeString(text); err == nil && len(key) == KeySize {
		return key, nil
	}

	return nil, fmt.Errorf("master key file %s must contain %d bytes as hex or base64", path, KeySize)
}
