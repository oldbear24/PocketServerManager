package audit

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gowebpki/jcs"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// chainedFields are the fields of an entry covered by its hash, besides the
// hash chain itself. JSON fields are included as JSON, the others as strings.
var (
	chainedTextFields = []string{
		"at", "category", "action", "actor_kind", "actor", "actor_label", "source",
		"ip", "user_agent", "collection", "record_id", "record_label", "host",
		"command", "revision", "note",
	}
	chainedJSONFields = []string{"changes", "snapshot"}
)

// canonicalEntry returns the RFC 8785 canonical JSON of an entry. It only looks
// at values read from the record, so it gives the same bytes when verifying.
func canonicalEntry(record *core.Record) ([]byte, error) {
	doc := make(map[string]any, len(chainedTextFields)+3)
	doc["seq"] = record.GetInt("seq")

	for _, name := range chainedTextFields {
		if value := record.GetString(name); value != "" {
			doc[name] = value
		}
	}

	for _, name := range chainedJSONFields {
		raw := record.GetString(name)
		if raw == "" || raw == "null" {
			continue
		}
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			return nil, fmt.Errorf("field %s: %w", name, err)
		}
		doc[name] = value
	}

	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}

	return jcs.Transform(raw)
}

// entryHash computes SHA-256(prev_hash ‖ canonical JSON of the entry).
func entryHash(prevHash string, canonical []byte) string {
	h := sha256.New()
	h.Write([]byte(prevHash))
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil))
}

// VerifyResult is the outcome of an integrity check of the hash chain.
type VerifyResult struct {
	OK      bool   `json:"ok"`
	Checked int    `json:"checked"`
	LastSeq int    `json:"last_seq"`
	BadSeq  int    `json:"bad_seq,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

const verifyBatch = 500

// Verify walks the whole chain and reports the first entry that does not match.
func Verify(app core.App) (VerifyResult, error) {
	collection, err := app.FindCachedCollectionByNameOrId(schema.AuditLog)
	if err != nil {
		return VerifyResult{}, err
	}

	fromSeq, prevHash, err := chainStart(app)
	if err != nil {
		return VerifyResult{}, err
	}

	result := VerifyResult{OK: true}
	expectedSeq := fromSeq
	cursor := fromSeq - 1

	for {
		rows := []dbx.NullStringMap{}
		err := app.RecordQuery(collection).
			AndWhere(dbx.NewExp("[[seq]] > {:cursor}", dbx.Params{"cursor": cursor})).
			OrderBy("[[seq]] ASC").
			Limit(verifyBatch).
			All(&rows)
		if err != nil {
			return result, err
		}
		if len(rows) == 0 {
			return result, nil
		}

		for _, row := range rows {
			record := core.NewRecord(collection)
			record.Load(rowValues(collection, row))

			seq := record.GetInt("seq")
			cursor = seq

			switch {
			case seq != expectedSeq:
				return result.fail(expectedSeq, fmt.Sprintf("entry %d is missing", expectedSeq)), nil
			case record.GetString("prev_hash") != prevHash:
				return result.fail(seq, "prev_hash does not match the previous entry"), nil
			}

			canonical, err := canonicalEntry(record)
			if err != nil {
				return result.fail(seq, err.Error()), nil
			}

			hash := entryHash(prevHash, canonical)
			if hash != record.GetString("hash") {
				return result.fail(seq, "the entry was changed after it was written"), nil
			}

			prevHash = hash
			expectedSeq = seq + 1
			result.Checked++
			result.LastSeq = seq
		}
	}
}

func (r VerifyResult) fail(seq int, reason string) VerifyResult {
	r.OK = false
	r.BadSeq = seq
	r.Reason = reason
	return r
}

// rowValues converts a raw row so that record.Load normalises it like PostScan.
func rowValues(collection *core.Collection, row dbx.NullStringMap) map[string]any {
	values := make(map[string]any, len(row))
	for key, value := range row {
		if value.Valid {
			values[key] = value.String
		}
	}
	return values
}

// chainStart returns where the chain begins: the first entry if nothing has been
// pruned, or the position stored by the newest retention checkpoint.
func chainStart(app core.App) (int, string, error) {
	row := struct {
		Snapshot string `db:"snapshot"`
	}{}

	err := app.DB().
		Select("snapshot").
		From(schema.AuditLog).
		Where(dbx.HashExp{"action": ActionCheckpoint}).
		OrderBy("seq DESC").
		Limit(1).
		One(&row)
	if errors.Is(err, sql.ErrNoRows) {
		// no checkpoint: the chain starts at the very first entry
		return 1, "", nil
	}
	if err != nil {
		return 0, "", err
	}

	var checkpoint struct {
		FromSeq      int    `json:"from_seq"`
		FromPrevHash string `json:"from_prev_hash"`
	}
	if err := json.Unmarshal([]byte(row.Snapshot), &checkpoint); err != nil {
		return 0, "", fmt.Errorf("malformed checkpoint: %w", err)
	}

	return checkpoint.FromSeq, checkpoint.FromPrevHash, nil
}
