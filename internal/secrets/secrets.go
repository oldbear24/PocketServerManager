// Package secrets encrypts values at rest with the panel's master key.
//
// The master key lives in a file outside the database. Sealed values carry a
// version prefix so that they can be recognised and the format can change.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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

// ErrKeyExists is returned by WriteKey when the file holds another key, or
// something that is no key.
var ErrKeyExists = errors.New("the file holds another master key")

// keyIDPurpose names the key derived from the master key whose first half is
// the master key's id (KeyID).
const keyIDPurpose = "pocketcloud key id v1"

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

// DeriveKey derives a key for one purpose from the master key (HKDF-SHA256), so
// that the master key itself is only ever used to seal values.
func DeriveKey(master []byte, purpose string) ([]byte, error) {
	if len(master) != KeySize {
		return nil, fmt.Errorf("master key must be %d bytes, got %d", KeySize, len(master))
	}
	return hkdf.Key(sha256.New, master, nil, purpose, KeySize)
}

// KeyID names a master key without giving it away: 32 hex characters, the
// first half of a key derived from it for that alone (#32). The database
// keeps the id of the key that sealed it, the panel's backup and the
// recovery kit carry it, so a wrong key is refused before it opens anything.
// It is empty for a key that is not KeySize bytes, which no master key is.
func KeyID(master []byte) string {
	derived, err := DeriveKey(master, keyIDPurpose)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(derived[:16])
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

// WriteKey stores a given master key at path, as WriteKeyFile stores a new
// one: 64 hex characters and a newline, in a file of mode 0600 that it
// creates. A recovery writes the key of its kit with it (#32). It never
// overwrites a file: one that holds the same key already is fine, and one
// that holds another key, or no key, is ErrKeyExists.
func WriteKey(path string, key []byte) error {
	if len(key) != KeySize {
		return fmt.Errorf("master key must be %d bytes, got %d", KeySize, len(key))
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if existing, ok := parseKey(raw); ok && subtle.ConstantTimeCompare(existing, key) == 1 {
			return nil
		}
		return fmt.Errorf("%w: %s", ErrKeyExists, path)
	}
	if err != nil {
		return err
	}

	_, err = f.WriteString(hex.EncodeToString(key) + "\n")
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		// a file cut short would hold no key, and keep the next try out
		_ = os.Remove(path)
	}
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

	if key, ok := parseKey(raw); ok {
		return key, nil
	}

	return nil, fmt.Errorf("master key file %s must contain %d bytes as hex or base64", path, KeySize)
}

// parseKey reads a key file's content: the key as hex or base64.
func parseKey(raw []byte) ([]byte, bool) {
	text := strings.TrimSpace(string(raw))

	if key, err := hex.DecodeString(text); err == nil && len(key) == KeySize {
		return key, true
	}
	if key, err := base64.StdEncoding.DecodeString(text); err == nil && len(key) == KeySize {
		return key, true
	}
	return nil, false
}
