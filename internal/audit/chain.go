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
	// Sealed says that the check proved the head with the seal key, so that no
	// entry up to the last sealed one was rewritten or removed.
	Sealed bool `json:"sealed"`
	// Unsealed counts the newest entries that were written without the master
	// key and are not sealed yet; the panel seals them when it starts.
	Unsealed int `json:"unsealed,omitempty"`
	// Keyless lists the sealed entries, as [from, to], that were written without
	// the master key: only their hashes vouch for them.
	Keyless [][2]int `json:"keyless,omitempty"`
}

const verifyBatch = 500

// Verify walks the whole chain and reports the first entry that does not match,
// then checks the head of the chain (see HeadTable): without it, removing the
// newest entries would go unnoticed. With the seal key it also proves the head,
// which nobody can rewrite without the master key; without the key it checks
// only that the head matches the newest entry. It reads one snapshot of the
// database, so entries written meanwhile are not mistaken for a break.
func Verify(app core.App, key []byte) (VerifyResult, error) {
	collection, err := app.FindCachedCollectionByNameOrId(schema.AuditLog)
	if err != nil {
		return VerifyResult{}, err
	}

	db, ok := app.ConcurrentDB().(*dbx.DB)
	if !ok {
		return VerifyResult{}, errors.New("the database does not support read transactions")
	}
	// read before the snapshot starts: whatever raised the mark was committed
	mark := sealMark(app)
	tx, err := db.Begin()
	if err != nil {
		return VerifyResult{}, err
	}
	// it only reads
	defer func() { _ = tx.Rollback() }()

	result, err := verifyIn(tx, collection, key, mark)
	if err == nil && result.OK && result.Sealed {
		raiseSealMark(app, result.LastSeq-result.Unsealed)
	}
	return result, err
}

// verifyIn checks the chain as db sees it. mark is the newest entry this process
// saw sealed.
func verifyIn(tx dbx.Builder, collection *core.Collection, key []byte, mark int) (VerifyResult, error) {
	fromSeq, prevHash, checkpointSeq, err := chainStart(tx)
	if err != nil {
		return VerifyResult{}, err
	}

	result := VerifyResult{OK: true}

	// the checkpoint is an entry of the chain it starts, so it is checked too;
	// one that pointed past itself would let the check skip any edit
	if checkpointSeq > 0 && checkpointSeq < fromSeq {
		return result.fail(checkpointSeq, "the retention checkpoint points past itself"), nil
	}

	// nothing may come before the start: the first entry, or the position where
	// retention removed everything before
	older := struct {
		Seq int `db:"seq"`
	}{}
	err = tx.Select("seq").From(schema.AuditLog).
		Where(dbx.NewExp("[[seq]] < {:from}", dbx.Params{"from": fromSeq})).
		OrderBy("seq ASC").Limit(1).One(&older)
	if err == nil {
		return result.fail(older.Seq, fmt.Sprintf("entry %d comes before the start of the chain and would not be checked", older.Seq)), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}

	expectedSeq := fromSeq
	cursor := fromSeq - 1

	for {
		rows := []dbx.NullStringMap{}
		err := tx.Select("*").From(schema.AuditLog).
			Where(dbx.NewExp("[[seq]] > {:cursor}", dbx.Params{"cursor": cursor})).
			OrderBy("[[seq]] ASC").
			Limit(verifyBatch).
			All(&rows)
		if err != nil {
			return result, err
		}
		if len(rows) == 0 {
			if broken, err := duplicateSeq(tx, result); err != nil || !broken.OK {
				return broken, err
			}
			return checkHead(tx, key, result, chainPoint{fromSeq, checkpointSeq, mark}, prevHash)
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

// duplicateSeq looks for entries the walk skipped: with the unique index on seq
// removed, a second entry with the seq of another one is not read by a walk
// that pages by seq.
func duplicateSeq(db dbx.Builder, result VerifyResult) (VerifyResult, error) {
	row := struct {
		N int `db:"n"`
	}{}
	if err := db.NewQuery(`SELECT count(*) AS n FROM {{` + schema.AuditLog + `}}`).One(&row); err != nil {
		return result, err
	}
	if row.N == result.Checked {
		return result, nil
	}

	duplicate := struct {
		Seq int `db:"seq"`
	}{}
	err := db.NewQuery(`SELECT [[seq]] FROM {{` + schema.AuditLog + `}} GROUP BY [[seq]] HAVING count(*) > 1 ORDER BY [[seq]] LIMIT 1`).One(&duplicate)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	return result.fail(duplicate.Seq, fmt.Sprintf("the log holds %d entries, but the chain only %d: entries share a seq", row.N, result.Checked)), nil
}

// chainPoint is where the walk started and how far the log was sealed before.
type chainPoint struct {
	fromSeq       int
	checkpointSeq int
	// mark is the newest entry this process saw sealed
	mark int
}

// checkHead compares the head of the chain with the newest entry the walk
// reached, whose hash is lastHash, and proves it with the seal key.
func checkHead(db dbx.Builder, key []byte, result VerifyResult, at chainPoint, lastHash string) (VerifyResult, error) {
	tableFound, err := headTableExists(db)
	if err != nil {
		return result, err
	}
	var h head
	found := false
	if tableFound {
		if h, found, err = readHead(db); err != nil {
			return result, err
		}
	}

	switch {
	case !found && result.LastSeq == 0 && at.mark > 0:
		return result.fail(1, fmt.Sprintf("the log is empty, but it held %d entries when it was checked before: it was wiped", at.mark)), nil
	case !found && result.LastSeq == 0 && tableFound:
		// the head is created with the first entry
		return result.fail(1, "the log is empty, but it had a head: it was wiped"), nil
	case !found && result.LastSeq == 0:
		// an empty log
		return result, nil
	case !found:
		return result.fail(result.LastSeq, "the head of the chain is missing"), nil
	case h.Seq > result.LastSeq:
		return result.fail(result.LastSeq+1, fmt.Sprintf("entry %d is missing", result.LastSeq+1)), nil
	case h.Seq != result.LastSeq || h.Hash != lastHash:
		return result.fail(result.LastSeq, fmt.Sprintf("the head of the chain does not match entry %d", result.LastSeq)), nil
	case result.LastSeq < at.mark:
		return result.fail(result.LastSeq+1, fmt.Sprintf("the log ends at entry %d, but it was sealed up to %d before: it was rolled back", result.LastSeq, at.mark)), nil
	}

	if key == nil {
		return result, nil
	}

	keyless, err := keylessRanges(h.Keyless)
	if err != nil {
		// a finding of the check, not an error of it
		return result.fail(h.Seq, "the seal of the chain is damaged"), nil //nolint:nilerr
	}

	switch {
	case h.MAC != "":
		if !sealed(key, h.Seq, h.Hash, h.Keyless, h.MAC) {
			return result.fail(h.Seq, "the seal of the chain does not match: entries were rewritten"), nil
		}
	case h.SealedSeq == 0:
		// only the process that created a head trusts it before its first seal,
		// and it seals it right away; a seal that is gone was removed
		return result.fail(h.Seq, "the chain has no seal: it was removed, or the log predates the seal or was only written without the master key; then `panel audit seal` seals it"), nil
	default:
		// entries written without the master key since the last sealed point
		if !sealed(key, h.SealedSeq, h.SealedHash, h.Keyless, h.SealedMAC) {
			return result.fail(h.SealedSeq, "the seal of the chain does not match: entries were rewritten"), nil
		}
		stored, ok, err := hashAt(db, h.SealedSeq)
		if err != nil {
			return result, err
		}
		if !ok || stored != h.SealedHash {
			return result.fail(h.SealedSeq, fmt.Sprintf("entry %d was rewritten after it was sealed", h.SealedSeq)), nil
		}
		result.Unsealed = result.LastSeq - h.SealedSeq
	}

	if proven := h.provenSeq(); proven < at.mark {
		return result.fail(proven+1, fmt.Sprintf("the seal ends at entry %d, but the log was sealed up to %d before: an older head was put back", proven, at.mark)), nil
	}

	// only retention writes checkpoints, and only with the key; one written
	// without it moves the start of the chain past entries that were sealed
	if at.checkpointSeq > h.provenSeq() || inRanges(keyless, at.checkpointSeq) {
		return result.fail(at.checkpointSeq, "the retention checkpoint was written without the master key"), nil
	}

	result.Sealed = true
	for _, r := range keyless {
		// retention may have removed some of them
		if r[1] >= at.fromSeq {
			result.Keyless = append(result.Keyless, [2]int{max(r[0], at.fromSeq), r[1]})
		}
	}

	return result, nil
}

func inRanges(ranges [][2]int, seq int) bool {
	for _, r := range ranges {
		if r[0] <= seq && seq <= r[1] {
			return true
		}
	}
	return false
}

// chainStart returns where the chain begins: the first entry if nothing has been
// pruned, or the position stored by the newest retention checkpoint, together
// with the seq of that checkpoint (zero without one).
func chainStart(db dbx.Builder) (int, string, int, error) {
	row := struct {
		Seq      int    `db:"seq"`
		Snapshot string `db:"snapshot"`
	}{}

	err := db.
		Select("seq", "snapshot").
		From(schema.AuditLog).
		Where(dbx.HashExp{"action": ActionCheckpoint}).
		OrderBy("seq DESC").
		Limit(1).
		One(&row)
	if errors.Is(err, sql.ErrNoRows) {
		// no checkpoint: the chain starts at the very first entry
		return 1, "", 0, nil
	}
	if err != nil {
		return 0, "", 0, err
	}

	var checkpoint struct {
		FromSeq      int    `json:"from_seq"`
		FromPrevHash string `json:"from_prev_hash"`
	}
	if err := json.Unmarshal([]byte(row.Snapshot), &checkpoint); err != nil {
		return 0, "", 0, fmt.Errorf("malformed checkpoint: %w", err)
	}

	return checkpoint.FromSeq, checkpoint.FromPrevHash, row.Seq, nil
}
