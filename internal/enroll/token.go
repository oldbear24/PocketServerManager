// Package enroll holds the one-time tokens that connect a server to the panel.
package enroll

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// TokenTTL is how long a token can be used.
const TokenTTL = 15 * time.Minute

// Crockford's alphabet: no I, L, O or U, so a token survives being read aloud.
var encoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// NewToken returns a new 128-bit token formatted as ENR-XXXX-XXXX-…
func NewToken() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}

	encoded := encoding.EncodeToString(raw)

	var b strings.Builder
	b.WriteString("ENR")
	for i := 0; i < len(encoded); i += 4 {
		b.WriteByte('-')
		b.WriteString(encoded[i:min(i+4, len(encoded))])
	}

	return b.String(), nil
}

// Hash returns what is stored for a token. It has enough entropy for one round
// of SHA-256; only this hash reaches the database.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(strings.ToUpper(strings.TrimSpace(token))))
	return hex.EncodeToString(sum[:])
}

// Create stores a new token and returns its record and the plain token, which
// is shown once and never stored.
func Create(app core.App, name string, roles []string, createdBy string) (*core.Record, string, error) {
	token, err := NewToken()
	if err != nil {
		return nil, "", err
	}

	collection, err := app.FindCachedCollectionByNameOrId(schema.EnrollmentTokens)
	if err != nil {
		return nil, "", err
	}

	expires, err := types.ParseDateTime(time.Now().UTC().Add(TokenTTL))
	if err != nil {
		return nil, "", err
	}

	record := core.NewRecord(collection)
	record.Set("token_hash", Hash(token))
	record.Set("hint", token[len(token)-4:])
	record.Set("name", name)
	record.Set("roles", roles)
	record.Set("expires_at", expires)
	record.Set("created_by", createdBy)

	if err := app.Save(record); err != nil {
		return nil, "", err
	}

	return record, token, nil
}
