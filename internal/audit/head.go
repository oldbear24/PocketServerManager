package audit

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"sync"

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
// the sealed point is intact. The seal then lists those entries as written
// without the key, so that nobody can pass off entries of their own as such
// without it showing.
//
// A head that was never sealed is trusted only by the process that created it:
// the one that ran the migration or wrote the first entry. A head whose seal
// was removed is never trusted again.
const HeadTable = "_pc_audit_head"

// SealPurpose names the key derived from the master key for the seal.
const SealPurpose = "pocketcloud audit chain head"

// maxKeylessRanges bounds the list of entries written without the key.
const maxKeylessRanges = 100

// head is the row of HeadTable.
type head struct {
	Seq        int    `db:"seq"`
	Hash       string `db:"hash"`
	MAC        string `db:"mac"`
	SealedSeq  int    `db:"sealed_seq"`
	SealedHash string `db:"sealed_hash"`
	SealedMAC  string `db:"sealed_mac"`
	// Keyless lists, as JSON [[from, to], …], the entries a seal covered that
	// were written without the master key. It is part of what is sealed.
	Keyless string `db:"keyless"`
}

// trustedHeads holds the data directories whose head this process created and
// has not sealed yet.
var trustedHeads sync.Map

func trustHead(app core.App)        { trustedHeads.Store(app.DataDir(), true) }
func untrustHead(app core.App)      { trustedHeads.Delete(app.DataDir()) }
func headTrusted(app core.App) bool { _, ok := trustedHeads.Load(app.DataDir()); return ok }

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
		[[sealed_mac]] TEXT NOT NULL DEFAULT '',
		[[keyless]] TEXT NOT NULL DEFAULT '[]'
	)`).Execute()
	if err != nil {
		return err
	}

	_, err = app.DB().NewQuery(`INSERT OR IGNORE INTO {{` + HeadTable + `}} ([[id]], [[seq]], [[hash]])
		SELECT 1, [[seq]], [[hash]] FROM {{` + schema.AuditLog + `}} ORDER BY [[seq]] DESC LIMIT 1`).Execute()
	if err != nil {
		return err
	}

	trustHead(app)
	return nil
}

// DropHeadTable removes the table.
func DropHeadTable(app core.App) error {
	_, err := app.DB().NewQuery(`DROP TABLE IF EXISTS {{` + HeadTable + `}}`).Execute()
	return err
}

func readHead(db dbx.Builder) (head, bool, error) {
	var h head
	err := db.Select("seq", "hash", "mac", "sealed_seq", "sealed_hash", "sealed_mac", "keyless").
		From(HeadTable).Where(dbx.HashExp{"id": 1}).One(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return head{}, false, nil
	}
	return h, err == nil, err
}

func writeHead(db dbx.Builder, h head) error {
	if h.Keyless == "" {
		h.Keyless = "[]"
	}
	_, err := db.NewQuery(`INSERT INTO {{` + HeadTable + `}}
		([[id]], [[seq]], [[hash]], [[mac]], [[sealed_seq]], [[sealed_hash]], [[sealed_mac]], [[keyless]])
		VALUES (1, {:seq}, {:hash}, {:mac}, {:sealed_seq}, {:sealed_hash}, {:sealed_mac}, {:keyless})
		ON CONFLICT ([[id]]) DO UPDATE SET [[seq]] = excluded.[[seq]], [[hash]] = excluded.[[hash]],
			[[mac]] = excluded.[[mac]], [[sealed_seq]] = excluded.[[sealed_seq]],
			[[sealed_hash]] = excluded.[[sealed_hash]], [[sealed_mac]] = excluded.[[sealed_mac]],
			[[keyless]] = excluded.[[keyless]]`).
		Bind(dbx.Params{
			"seq": h.Seq, "hash": h.Hash, "mac": h.MAC,
			"sealed_seq": h.SealedSeq, "sealed_hash": h.SealedHash, "sealed_mac": h.SealedMAC,
			"keyless": h.Keyless,
		}).Execute()
	return err
}

// seal returns the MAC of a position of the chain and of the entries written
// without the key up to there.
func seal(key []byte, seq int, hash, keyless string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(strconv.Itoa(seq) + "\n" + hash + "\n" + keyless))
	return hex.EncodeToString(mac.Sum(nil))
}

func sealed(key []byte, seq int, hash, keyless, mac string) bool {
	if mac == "" {
		return false
	}
	return hmac.Equal([]byte(seal(key, seq, hash, keyless)), []byte(mac))
}

// keylessRanges reads the list of entries written without the key.
func keylessRanges(text string) ([][2]int, error) {
	if text == "" {
		return nil, nil
	}
	var ranges [][2]int
	if err := json.Unmarshal([]byte(text), &ranges); err != nil {
		return nil, err
	}
	return ranges, nil
}

// withKeyless adds the entries from..to to the list, merging it with the last
// range when they touch.
func withKeyless(text string, from, to int) (string, error) {
	ranges, err := keylessRanges(text)
	if err != nil {
		return "", err
	}
	if n := len(ranges); n > 0 && ranges[n-1][1]+1 >= from {
		ranges[n-1][1] = max(ranges[n-1][1], to)
	} else {
		ranges = append(ranges, [2]int{from, to})
	}
	if len(ranges) > maxKeylessRanges {
		ranges = ranges[len(ranges)-maxKeylessRanges:]
	}
	raw, err := json.Marshal(ranges)
	return string(raw), err
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
// trusted says that this process created the head and may seal it unseen.
func (h head) intact(db dbx.Builder, key []byte, trusted, found bool, lastSeq int, lastHash string) (bool, error) {
	if !found {
		// only an empty log has no head yet
		return lastSeq == 0, nil
	}
	if h.Seq != lastSeq || h.Hash != lastHash {
		return false, nil
	}
	if h.MAC != "" {
		return sealed(key, h.Seq, h.Hash, h.Keyless, h.MAC), nil
	}
	if h.SealedSeq == 0 {
		return trusted, nil
	}
	if !sealed(key, h.SealedSeq, h.SealedHash, h.Keyless, h.SealedMAC) {
		return false, nil
	}
	stored, ok, err := hashAt(db, h.SealedSeq)
	return ok && stored == h.SealedHash, err
}

// sealAt seals the head at seq and hash. Entries since the last sealed point,
// up to keylessTo, were written without the key and are listed as such.
func (h head) sealAt(key []byte, seq int, hash string, keylessTo int) (head, error) {
	keyless := h.Keyless
	if keyless == "" {
		// as stored
		keyless = "[]"
	}
	if h.MAC == "" && h.SealedSeq > 0 && keylessTo > h.SealedSeq {
		var err error
		if keyless, err = withKeyless(keyless, h.SealedSeq+1, keylessTo); err != nil {
			return h, err
		}
	}
	mac := seal(key, seq, hash, keyless)
	return head{Seq: seq, Hash: hash, MAC: mac, SealedSeq: seq, SealedHash: hash, SealedMAC: mac, Keyless: keyless}, nil
}

// next is the head after an entry was appended at seq with hash. With the key
// and an intact head it is sealed; otherwise it moves on unsealed and keeps the
// last sealed point, so that a check still finds whatever broke it.
func (h head) next(db dbx.Builder, key []byte, trusted, found bool, lastSeq int, lastHash string, seq int, hash string) (head, error) {
	following := head{Seq: seq, Hash: hash, SealedSeq: h.SealedSeq, SealedHash: h.SealedHash, SealedMAC: h.SealedMAC, Keyless: h.Keyless}

	if key == nil {
		return following, nil
	}

	ok, err := h.intact(db, key, trusted, found, lastSeq, lastHash)
	if err != nil || !ok {
		return following, err
	}

	return h.sealAt(key, seq, hash, lastSeq)
}

// SealHead seals a head that commands without the master key left unsealed. The
// panel calls it when it starts with the key.
func (w *Writer) SealHead() error {
	key := w.sealKey()
	if key == nil {
		return nil
	}

	return w.app.RunInTransaction(func(tx core.App) error {
		if !tx.HasTable(HeadTable) {
			return nil
		}
		h, found, err := readHead(tx.DB())
		if err != nil || !found || h.MAC != "" {
			return err
		}

		lastSeq, lastHash, err := tail(tx.DB())
		if err != nil {
			return err
		}

		ok, err := h.intact(tx.DB(), key, headTrusted(tx), found, lastSeq, lastHash)
		if err != nil || !ok {
			// a broken head stays as it is, for the check to report
			return err
		}

		sealedHead, err := h.sealAt(key, h.Seq, h.Hash, h.Seq)
		if err != nil {
			return err
		}
		if err := writeHead(tx.DB(), sealedHead); err != nil {
			return err
		}
		untrustHead(tx)
		return nil
	})
}

func (w *Writer) sealKey() []byte {
	if w.SealKey == nil {
		return nil
	}
	return w.SealKey()
}
