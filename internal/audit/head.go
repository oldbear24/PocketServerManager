package audit

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
// Only an empty log is trusted unseen: the head is created with the first
// entry, sealed when the key is there. A head that was never sealed, because
// the log predates the seal or was only ever written without the key, or whose
// seal was removed, is sealed only on request, by `panel audit seal` (Seal).
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

const createHeadTable = `CREATE TABLE IF NOT EXISTS {{` + HeadTable + `}} (
	[[id]] INTEGER PRIMARY KEY CHECK ([[id]] = 1),
	[[seq]] INTEGER NOT NULL,
	[[hash]] TEXT NOT NULL,
	[[mac]] TEXT NOT NULL DEFAULT '',
	[[sealed_seq]] INTEGER NOT NULL DEFAULT 0,
	[[sealed_hash]] TEXT NOT NULL DEFAULT '',
	[[sealed_mac]] TEXT NOT NULL DEFAULT '',
	[[keyless]] TEXT NOT NULL DEFAULT '[]'
)`

// CreateHeadTable is the migration of the head. An empty log gets its head with
// its first entry. A log that already has entries gets a head at the newest
// one, unsealed: nothing proves those entries, so only `panel audit seal`
// seals it, and the check reports it until then. Running it again creates no
// trust either.
func CreateHeadTable(app core.App) error {
	lastSeq, _, err := tail(app.DB())
	if err != nil || lastSeq == 0 {
		return err
	}

	if _, err := app.DB().NewQuery(createHeadTable).Execute(); err != nil {
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
	for len(ranges) > maxKeylessRanges {
		ranges = mergeClosest(ranges)
	}
	raw, err := json.Marshal(ranges)
	return string(raw), err
}

// mergeClosest merges the two neighbouring ranges with the smallest gap. The
// list then names a few entries written with the key as well, which errs on the
// safe side; dropping a range would hide entries written without it.
func mergeClosest(ranges [][2]int) [][2]int {
	closest := 0
	for i := 1; i < len(ranges)-1; i++ {
		if ranges[i+1][0]-ranges[i][1] < ranges[closest+1][0]-ranges[closest][1] {
			closest = i
		}
	}
	ranges[closest][1] = ranges[closest+1][1]
	return append(ranges[:closest+1], ranges[closest+2:]...)
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
// Only an empty log without a head table is intact without a seal.
func (h head) intact(db dbx.Builder, key []byte, mark, lastSeq int, lastHash string, tableFound, found bool) (bool, error) {
	if !found {
		// an empty log has no head table yet; a table without its row lost it
		return !tableFound && lastSeq == 0 && mark == 0, nil
	}
	if h.Seq != lastSeq || h.Hash != lastHash || h.provenSeq() < mark {
		return false, nil
	}
	if h.MAC != "" {
		return sealed(key, h.Seq, h.Hash, h.Keyless, h.MAC), nil
	}
	if h.SealedSeq == 0 {
		// never sealed, or its seal was removed
		return false, nil
	}
	return h.sealedPointIntact(db, key)
}

// sealedPointIntact reports whether the last sealed point is sealed and the
// entry it names is still the one that was sealed.
func (h head) sealedPointIntact(db dbx.Builder, key []byte) (bool, error) {
	if !sealed(key, h.SealedSeq, h.SealedHash, h.Keyless, h.SealedMAC) {
		return false, nil
	}
	stored, ok, err := hashAt(db, h.SealedSeq)
	return ok && stored == h.SealedHash, err
}

// provenSeq is the newest entry the seal vouches for, if it is valid.
func (h head) provenSeq() int {
	if h.MAC != "" {
		return h.Seq
	}
	return h.SealedSeq
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
func (h head) next(db dbx.Builder, key []byte, mark, lastSeq int, lastHash string, tableFound, found bool, seq int, hash string) (head, error) {
	following := head{Seq: seq, Hash: hash, SealedSeq: h.SealedSeq, SealedHash: h.SealedHash, SealedMAC: h.SealedMAC, Keyless: h.Keyless}

	if key == nil {
		return following, nil
	}

	ok, err := h.intact(db, key, mark, lastSeq, lastHash, tableFound, found)
	if err != nil || !ok {
		return following, err
	}

	return h.sealAt(key, seq, hash, lastSeq)
}

// marks holds, per data directory, the newest entry this process proved sealed.
// A check that finds the log, or its seal, ending before it reports that an
// older copy was put back, which the seal alone cannot tell.
var marks = struct {
	sync.Mutex
	seq map[string]int
}{seq: map[string]int{}}

func sealMark(app core.App) int {
	marks.Lock()
	defer marks.Unlock()
	return marks.seq[app.DataDir()]
}

// raiseSealMark is called only after the seal it reports was committed, so that
// a check that starts later reads at least as far.
func raiseSealMark(app core.App, seq int) {
	marks.Lock()
	defer marks.Unlock()
	marks.seq[app.DataDir()] = max(marks.seq[app.DataDir()], seq)
}

func headTableExists(db dbx.Builder) (bool, error) {
	row := struct {
		N int `db:"n"`
	}{}
	err := db.NewQuery(`SELECT count(*) AS n FROM sqlite_master WHERE type = 'table' AND name = {:name}`).
		Bind(dbx.Params{"name": HeadTable}).One(&row)
	return row.N > 0, err
}

// SealHead seals a head that commands without the master key left unsealed. The
// panel calls it when it starts with the key.
func (w *Writer) SealHead() error {
	key := w.sealKey()
	if key == nil {
		return nil
	}

	sealedSeq := 0
	err := w.app.RunInTransaction(func(tx core.App) error {
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

		ok, err := h.intact(tx.DB(), key, sealMark(tx), lastSeq, lastHash, true, found)
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
		sealedSeq = sealedHead.Seq
		return nil
	})
	if err == nil {
		raiseSealMark(w.app, sealedSeq)
	}
	return err
}

// SealResult names the entries Seal listed as written without the key.
type SealResult struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// Seal seals a head that nothing proves: a log that predates the seal or was
// only written without the master key, or one whose seal was removed. It is
// what `panel audit seal` runs, at the owner's request, as it vouches for what
// it cannot check. It refuses a chain whose hashes do not match. Every entry
// after the last valid sealed point (all of them, if there is none) is listed
// as written without the key, and the seal itself is recorded as a security
// event, so the audit page shows what was taken on trust. An intact seal is
// left as it is.
func (w *Writer) Seal(actor Actor) (SealResult, error) {
	key := w.sealKey()
	if key == nil {
		return SealResult{}, errors.New("sealing the audit log needs the master key")
	}
	collection, err := w.app.FindCachedCollectionByNameOrId(schema.AuditLog)
	if err != nil {
		return SealResult{}, err
	}

	var result SealResult
	err = w.app.RunInTransaction(func(tx core.App) error {
		mark := sealMark(tx)
		check, err := verifyIn(tx.DB(), collection, nil, mark)
		if err != nil {
			return err
		}
		if !check.OK {
			return fmt.Errorf("the audit log is not sealed: entry %d does not check (%s)", check.BadSeq, check.Reason)
		}
		if check.LastSeq == 0 {
			return nil
		}
		if keyed, err := verifyIn(tx.DB(), collection, key, mark); err != nil || keyed.OK && keyed.Unsealed == 0 {
			// sealed already
			return err
		}

		fromSeq, _, _, err := chainStart(tx.DB())
		if err != nil {
			return err
		}
		lastSeq, lastHash, err := tail(tx.DB())
		if err != nil {
			return err
		}

		if _, err := tx.DB().NewQuery(createHeadTable).Execute(); err != nil {
			return err
		}
		h, found, err := readHead(tx.DB())
		if err != nil {
			return err
		}
		keyless, from := "[]", fromSeq
		if found && h.SealedSeq > 0 && h.SealedSeq >= mark {
			ok, err := h.sealedPointIntact(tx.DB(), key)
			if err != nil {
				return err
			}
			if ok {
				keyless, from = h.Keyless, h.SealedSeq+1
			}
		}
		if keyless, err = withKeyless(keyless, from, lastSeq); err != nil {
			return err
		}

		mac := seal(key, lastSeq, lastHash, keyless)
		err = writeHead(tx.DB(), head{Seq: lastSeq, Hash: lastHash, MAC: mac, SealedSeq: lastSeq, SealedHash: lastHash, SealedMAC: mac, Keyless: keyless})
		if err != nil {
			return err
		}
		result = SealResult{From: from, To: lastSeq}

		// sealed in turn, as the head before it now is
		return w.append(tx, entry{
			Category:    CategorySecurity,
			Action:      ActionChainSealed,
			Actor:       actor,
			RecordLabel: schema.AuditLog,
			Snapshot:    map[string]any{"from": from, "to": lastSeq},
		})
	})
	if err != nil {
		return SealResult{}, err
	}
	if result.To > 0 {
		raiseSealMark(w.app, result.To+1)
	}
	return result, nil
}

func (w *Writer) sealKey() []byte {
	if w.SealKey == nil {
		return nil
	}
	return w.SealKey()
}
