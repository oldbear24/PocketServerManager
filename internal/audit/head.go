package audit

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// HeadTable holds the head of the chain: the seq and hash of the newest entry,
// sealed with a key derived from the master key. The chain of hashes alone shows
// an edit, but not a deletion of the newest entries, and anyone with the
// database can recompute every hash after an edit; neither gets past the seal
// without the master key, which never is in the database. It is a plain table,
// not a collection, so the API and the dashboard do not see it.
//
// A command run without the master key (a migration on the command line, for
// example) still appends entries. The head then moves on unsealed, keeping the
// last sealed point, and the next write with the key seals it again, provided
// the sealed point is intact. Entries written without the key are protected
// only from then on.
const HeadTable = "_pc_audit_head"

// SealPurpose names the key derived from the master key for the seal.
const SealPurpose = "pocketcloud audit chain head"

// head is the row of HeadTable.
type head struct {
	Seq        int    `db:"seq"`
	Hash       string `db:"hash"`
	MAC        string `db:"mac"`
	SealedSeq  int    `db:"sealed_seq"`
	SealedHash string `db:"sealed_hash"`
	SealedMAC  string `db:"sealed_mac"`
}

// CreateHeadTable creates the table, starting from the newest entry, unsealed:
// the first write with the master key seals it.
func CreateHeadTable(app core.App) error {
	_, err := app.DB().NewQuery(`CREATE TABLE IF NOT EXISTS {{` + HeadTable + `}} (
		[[id]] INTEGER PRIMARY KEY CHECK ([[id]] = 1),
		[[seq]] INTEGER NOT NULL,
		[[hash]] TEXT NOT NULL,
		[[mac]] TEXT NOT NULL DEFAULT '',
		[[sealed_seq]] INTEGER NOT NULL DEFAULT 0,
		[[sealed_hash]] TEXT NOT NULL DEFAULT '',
		[[sealed_mac]] TEXT NOT NULL DEFAULT ''
	)`).Execute()
	if err != nil {
		return err
	}

	_, err = app.DB().NewQuery(`INSERT OR IGNORE INTO {{` + HeadTable + `}} ([[id]], [[seq]], [[hash]])
		SELECT 1, [[seq]], [[hash]] FROM {{` + schema.AuditLog + `}} ORDER BY [[seq]] DESC LIMIT 1`).Execute()
	return err
}

// DropHeadTable removes the table.
func DropHeadTable(app core.App) error {
	_, err := app.DB().NewQuery(`DROP TABLE IF EXISTS {{` + HeadTable + `}}`).Execute()
	return err
}

func readHead(db dbx.Builder) (head, bool, error) {
	var h head
	err := db.Select("seq", "hash", "mac", "sealed_seq", "sealed_hash", "sealed_mac").
		From(HeadTable).Where(dbx.HashExp{"id": 1}).One(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return head{}, false, nil
	}
	return h, err == nil, err
}

func writeHead(db dbx.Builder, h head) error {
	_, err := db.NewQuery(`INSERT INTO {{` + HeadTable + `}}
		([[id]], [[seq]], [[hash]], [[mac]], [[sealed_seq]], [[sealed_hash]], [[sealed_mac]])
		VALUES (1, {:seq}, {:hash}, {:mac}, {:sealed_seq}, {:sealed_hash}, {:sealed_mac})
		ON CONFLICT ([[id]]) DO UPDATE SET [[seq]] = excluded.[[seq]], [[hash]] = excluded.[[hash]],
			[[mac]] = excluded.[[mac]], [[sealed_seq]] = excluded.[[sealed_seq]],
			[[sealed_hash]] = excluded.[[sealed_hash]], [[sealed_mac]] = excluded.[[sealed_mac]]`).
		Bind(dbx.Params{
			"seq": h.Seq, "hash": h.Hash, "mac": h.MAC,
			"sealed_seq": h.SealedSeq, "sealed_hash": h.SealedHash, "sealed_mac": h.SealedMAC,
		}).Execute()
	return err
}

// seal returns the MAC of a position of the chain.
func seal(key []byte, seq int, hash string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(strconv.Itoa(seq) + "\n" + hash))
	return hex.EncodeToString(mac.Sum(nil))
}

func sealed(key []byte, seq int, hash, mac string) bool {
	if mac == "" {
		return false
	}
	return hmac.Equal([]byte(seal(key, seq, hash)), []byte(mac))
}

// tail returns the seq and hash of the newest entry, or zero values for an empty log.
func tail(db dbx.Builder) (int, string, error) {
	last := struct {
		Seq  int    `db:"seq"`
		Hash string `db:"hash"`
	}{}
	err := db.Select("seq", "hash").From(schema.AuditLog).OrderBy("seq DESC").Limit(1).One(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	return last.Seq, last.Hash, err
}

// hashAt returns the stored hash of one entry.
func hashAt(db dbx.Builder, seq int) (string, bool, error) {
	row := struct {
		Hash string `db:"hash"`
	}{}
	err := db.Select("hash").From(schema.AuditLog).Where(dbx.HashExp{"seq": seq}).One(&row)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return row.Hash, err == nil, err
}

// intact reports whether the stored head describes the entry the chain goes on
// from, so that sealing a new head does not cover up a change made before it.
func (h head) intact(db dbx.Builder, key []byte, found bool, lastSeq int, lastHash string) (bool, error) {
	if !found {
		// only an empty log has no head yet
		return lastSeq == 0, nil
	}
	if h.Seq != lastSeq || h.Hash != lastHash {
		return false, nil
	}
	if h.MAC != "" {
		return sealed(key, h.Seq, h.Hash, h.MAC), nil
	}
	if h.SealedSeq == 0 {
		// never sealed: a log from before the seal, or one only ever written
		// without the master key, is trusted once
		return true, nil
	}
	if !sealed(key, h.SealedSeq, h.SealedHash, h.SealedMAC) {
		return false, nil
	}
	stored, ok, err := hashAt(db, h.SealedSeq)
	return ok && stored == h.SealedHash, err
}

// next is the head after an entry was appended at seq with hash. With the key
// and an intact head it is sealed; otherwise it moves on unsealed and keeps the
// last sealed point, so that a check still finds whatever broke it.
func (h head) next(db dbx.Builder, key []byte, found bool, lastSeq int, lastHash string, seq int, hash string) (head, error) {
	following := head{Seq: seq, Hash: hash, SealedSeq: h.SealedSeq, SealedHash: h.SealedHash, SealedMAC: h.SealedMAC}

	if key == nil {
		return following, nil
	}

	ok, err := h.intact(db, key, found, lastSeq, lastHash)
	if err != nil || !ok {
		return following, err
	}

	following.MAC = seal(key, seq, hash)
	following.SealedSeq, following.SealedHash, following.SealedMAC = seq, hash, following.MAC

	return following, nil
}

// SealHead seals a head that commands without the master key left unsealed. The
// panel calls it when it starts with the key.
func (w *Writer) SealHead() error {
	key := w.sealKey()
	if key == nil {
		return nil
	}

	return w.app.RunInTransaction(func(tx core.App) error {
		h, found, err := readHead(tx.DB())
		if err != nil || !found || h.MAC != "" {
			return err
		}

		lastSeq, lastHash, err := tail(tx.DB())
		if err != nil {
			return err
		}

		ok, err := h.intact(tx.DB(), key, found, lastSeq, lastHash)
		if err != nil || !ok {
			// a broken head stays as it is, for the check to report
			return err
		}

		h.MAC = seal(key, h.Seq, h.Hash)
		h.SealedSeq, h.SealedHash, h.SealedMAC = h.Seq, h.Hash, h.MAC
		return writeHead(tx.DB(), h)
	})
}

func (w *Writer) sealKey() []byte {
	if w.SealKey == nil {
		return nil
	}
	return w.SealKey()
}
