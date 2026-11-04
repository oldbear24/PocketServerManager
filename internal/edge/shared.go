package edge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
)

// Digest identifies what an edge serves: its image and its config, as the
// panel generated them. The agent reports the digest of the last config that
// loaded, and the panel sends a new one when it differs from what it wants.
func Digest(image, caddyJSON string) string {
	sum := sha256.Sum256([]byte(image + "\n" + caddyJSON))
	return hex.EncodeToString(sum[:])
}

// CertFileID names the files of a certificate on an edge: the record's id and
// the start of the leaf certificate's SHA-256 fingerprint. A renewed certificate
// thus gets new files, and a config that refers to them is a new config: with
// the same file names, Caddy would find the config unchanged and keep serving
// the old certificate from memory.
func CertFileID(id string, chainPEM []byte) (string, error) {
	if !validID(id) {
		return "", errors.New("the certificate id is not a valid file name")
	}
	block, _ := pem.Decode(chainPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("the chain does not start with a certificate")
	}
	sum := sha256.Sum256(block.Bytes)
	return id + "-" + hex.EncodeToString(sum[:])[:16], nil
}
