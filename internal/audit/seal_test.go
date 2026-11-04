package audit_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// sealedEnv returns a panel with n hosts, each written and sealed with the key.
func sealedEnv(t *testing.T, n int) (*env, []byte) {
	t.Helper()
	env := newEnv(t)
	for i := range n {
		testutil.NewHost(t, env.app, fmt.Sprintf("host-%d", i))
	}
	key := env.panel.AuditKey()
	if result, err := audit.Verify(env.app, key); err != nil || !result.OK || !result.Sealed {
		t.Fatalf("clean chain: %+v, %v", result, err)
	}
	return env, key
}

func lastSeq(t *testing.T, env *env) int {
	t.Helper()
	row := struct {
		Seq int `db:"seq"`
	}{}
	if err := env.app.DB().NewQuery(`SELECT MAX(seq) AS seq FROM audit_log`).One(&row); err != nil {
		t.Fatal(err)
	}
	return row.Seq
}

// assertCaught checks the log now, then again after the panel sealed what it
// could and wrote one more entry, so that a later seal does not cover it up.
func assertCaught(t *testing.T, env *env, key []byte, want string) {
	t.Helper()
	result, err := audit.Verify(env.app, key)
	if err != nil || result.OK || (want != "" && !strings.Contains(result.Reason, want)) {
		t.Fatalf("not caught: %+v, %v", result, err)
	}
	if err := env.panel.Audit.SealHead(); err != nil {
		t.Fatal(err)
	}
	testutil.NewHost(t, env.app, "later")
	testutil.NewHost(t, env.app, "later-2")
	if result, _ := audit.Verify(env.app, key); result.OK {
		t.Fatalf("a later seal covered it up: %+v", result)
	}
}

// TestADeletedHeadIsNotSealedOver: a head row that is gone is written anew by
// the next entry, unsealed. That must not make it a head the panel trusts.
func TestADeletedHeadIsNotSealedOver(t *testing.T) {
	env, key := sealedEnv(t, 3)

	// the newest entries go, and with them the head
	exec(t, env, `DELETE FROM audit_log WHERE seq >= 3`)
	exec(t, env, `DELETE FROM `+audit.HeadTable)
	audit.ForgetSealMarks()

	assertCaught(t, env, key, "")
}

// TestRerunningTheHeadMigrationGrantsNoTrust: running the migration of the
// head again (down and up, or by removing it from _migrations) recreates the
// head from the newest entry, which must not be sealed unseen.
func TestRerunningTheHeadMigrationGrantsNoTrust(t *testing.T) {
	env, key := sealedEnv(t, 3)

	exec(t, env, `DELETE FROM audit_log WHERE seq >= 3`)
	if err := audit.DropHeadTable(env.app); err != nil {
		t.Fatal(err)
	}
	if err := audit.CreateHeadTable(env.app); err != nil {
		t.Fatal(err)
	}
	audit.ForgetSealMarks()

	assertCaught(t, env, key, "")
}

// TestAForgedCheckpointIsReported: whoever has the database but not the key can
// append a retention checkpoint that moves the start of the chain on, remove
// the sealed entries before it and let the panel seal the rest.
func TestAForgedCheckpointIsReported(t *testing.T) {
	env, key := sealedEnv(t, 4)
	sealedSeq := lastSeq(t, env)

	prev := struct {
		Hash string `db:"hash"`
	}{}
	if err := env.app.DB().NewQuery(fmt.Sprintf(`SELECT hash FROM audit_log WHERE seq = %d`, sealedSeq-1)).One(&prev); err != nil {
		t.Fatal(err)
	}
	exec(t, env, fmt.Sprintf(`INSERT INTO audit_log (id, seq, at, category, action, actor_kind, actor_label, source, snapshot, prev_hash, hash)
		VALUES ('forgedcheckpt1', %d, '2026-01-01 00:00:00.000Z', 'system', '%s', 'system', 'audit retention', 'panel',
			'{"from_seq":%d,"from_prev_hash":"%s"}', '', '')`, sealedSeq+1, audit.ActionCheckpoint, sealedSeq, prev.Hash))
	if err := audit.Rehash(env.app, sealedSeq+1); err != nil {
		t.Fatal(err)
	}
	exec(t, env, `UPDATE `+audit.HeadTable+` SET mac = ''`)
	exec(t, env, fmt.Sprintf(`DELETE FROM audit_log WHERE seq < %d`, sealedSeq))

	// without the key it looks like retention
	if result, err := audit.Verify(env.app, nil); err != nil || !result.OK {
		t.Fatalf("the forged checkpoint without the key: %+v, %v", result, err)
	}
	assertCaught(t, env, key, "checkpoint")
}

// TestADuplicateSeqIsReported: with the unique index on seq dropped, a second
// entry with the seq of the last one of a batch would be skipped by the walk.
func TestADuplicateSeqIsReported(t *testing.T) {
	env := newEnv(t)
	host := testutil.NewHost(t, env.app, "busy")
	for i := 0; lastSeq(t, env) < 501; i++ {
		host.Set("labels", map[string]any{"i": i})
		if err := env.app.Save(host); err != nil {
			t.Fatal(err)
		}
	}
	key := env.panel.AuditKey()

	exec(t, env, `DROP INDEX idx_audit_log_seq`)
	exec(t, env, `INSERT INTO audit_log (id, seq, at, category, action, actor_kind, actor_label, source, collection, record_id, record_label, prev_hash, hash)
		SELECT 'duplicate00001', seq, at, category, action, actor_kind, 'evil', source, collection, record_id, 'evil', prev_hash, hash
		FROM audit_log WHERE seq = 500`)

	result, err := audit.Verify(env.app, key)
	if err != nil || result.OK {
		t.Fatalf("a duplicate seq passes: %+v, %v", result, err)
	}
}

// TestManyKeylessRangesAreKept: past the cap the oldest ranges are merged, not
// dropped, so that entries written without the key stay listed.
func TestManyKeylessRangesAreKept(t *testing.T) {
	text := "[]"
	var err error
	for i := range 150 {
		if text, err = audit.WithKeyless(text, 10*i+1, 10*i+2); err != nil {
			t.Fatal(err)
		}
	}
	ranges, err := audit.KeylessRanges(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) > 100 {
		t.Errorf("%d ranges, want at most 100", len(ranges))
	}
	for i := range 150 {
		if !covered(ranges, 10*i+1) || !covered(ranges, 10*i+2) {
			t.Fatalf("entries %d and %d are no longer listed: %v", 10*i+1, 10*i+2, ranges)
		}
	}
}

func covered(ranges [][2]int, seq int) bool {
	for _, r := range ranges {
		if r[0] <= seq && seq <= r[1] {
			return true
		}
	}
	return false
}

// TestAWipedLogIsReported: removing every entry and the head row leaves the
// table of the head, which an empty log never has.
func TestAWipedLogIsReported(t *testing.T) {
	env, key := sealedEnv(t, 2)

	exec(t, env, `DELETE FROM audit_log`)
	exec(t, env, `DELETE FROM `+audit.HeadTable)
	audit.ForgetSealMarks()

	assertCaught(t, env, key, "wiped")
}

// TestAnOlderSealIsNotTakenBack: an older head, sealed and copied aside, put
// back after the newest entries were removed. The panel remembers how far the
// log was sealed.
func TestAnOlderSealIsNotTakenBack(t *testing.T) {
	env, key := sealedEnv(t, 2)
	older := lastSeq(t, env)
	exec(t, env, `CREATE TABLE head_copy AS SELECT * FROM `+audit.HeadTable)

	testutil.NewHost(t, env.app, "newer-1")
	testutil.NewHost(t, env.app, "newer-2")
	if result, err := audit.Verify(env.app, key); err != nil || !result.OK {
		t.Fatalf("newer entries: %+v, %v", result, err)
	}

	exec(t, env, fmt.Sprintf(`DELETE FROM audit_log WHERE seq > %d`, older))
	exec(t, env, `DELETE FROM `+audit.HeadTable)
	exec(t, env, `INSERT INTO `+audit.HeadTable+` SELECT * FROM head_copy`)

	// the seal is valid, but the log ends before where it was sealed
	if result, err := audit.Verify(env.app, nil); err != nil || result.OK {
		t.Fatalf("a rolled back log without the key: %+v, %v", result, err)
	}
	assertCaught(t, env, key, "rolled back")

	t.Run("and padded without the key", func(t *testing.T) {
		env, key := sealedEnv(t, 2)
		older := lastSeq(t, env)
		exec(t, env, `CREATE TABLE head_copy AS SELECT * FROM `+audit.HeadTable)
		testutil.NewHost(t, env.app, "newer")
		if result, err := audit.Verify(env.app, key); err != nil || !result.OK {
			t.Fatalf("newer entries: %+v, %v", result, err)
		}

		exec(t, env, fmt.Sprintf(`DELETE FROM audit_log WHERE seq > %d`, older))
		exec(t, env, `DELETE FROM `+audit.HeadTable)
		exec(t, env, `INSERT INTO `+audit.HeadTable+` SELECT * FROM head_copy`)
		env.panel.Audit.SealKey = func() []byte { return nil }
		for i := range 3 {
			testutil.NewHost(t, env.app, fmt.Sprintf("padding-%d", i))
		}
		env.panel.Audit.SealKey = func() []byte { return key }

		assertCaught(t, env, key, "older head")
	})

	t.Run("and the head table dropped", func(t *testing.T) {
		env, key := sealedEnv(t, 2)
		exec(t, env, `DELETE FROM audit_log`)
		if err := audit.DropHeadTable(env.app); err != nil {
			t.Fatal(err)
		}
		assertCaught(t, env, key, "")
	})
}

// TestAnOldLogIsSealedOnRequest: a log that predates the seal, or that was only
// written without the key, is sealed by `panel audit seal`, which lists every
// entry it could not prove as written without the key and records itself.
func TestAnOldLogIsSealedOnRequest(t *testing.T) {
	env := newEnv(t)
	key := env.panel.AuditKey()
	seal := env.panel.Audit.SealKey
	env.panel.Audit.SealKey = func() []byte { return nil }
	for i := range 3 {
		testutil.NewHost(t, env.app, fmt.Sprintf("host-%d", i))
	}
	env.panel.Audit.SealKey = seal
	keyless := lastSeq(t, env)

	// the panel does not seal it unseen
	if err := env.panel.Audit.SealHead(); err != nil {
		t.Fatal(err)
	}
	testutil.NewHost(t, env.app, "keyed")
	result, err := audit.Verify(env.app, key)
	if err != nil || result.OK || !strings.Contains(result.Reason, "panel audit seal") {
		t.Fatalf("an unsealed log: %+v, %v", result, err)
	}

	sealedResult, err := env.panel.Audit.Seal(audit.System("panel audit seal", audit.SourceCommand))
	if err != nil {
		t.Fatal(err)
	}
	if sealedResult.From != 1 || sealedResult.To != keyless+1 {
		t.Errorf("sealed %+v, want entries 1 to %d", sealedResult, keyless+1)
	}

	result, err = audit.Verify(env.app, key)
	if err != nil || !result.OK || !result.Sealed || result.Unsealed != 0 ||
		len(result.Keyless) != 1 || result.Keyless[0] != [2]int{1, keyless + 1} {
		t.Fatalf("after sealing: %+v, %v", result, err)
	}

	entry := lastEntry(t, env)
	if entry.GetString("action") != audit.ActionChainSealed || entry.GetString("category") != audit.CategorySecurity {
		t.Errorf("the seal is not recorded: %v", entry)
	}

	t.Run("a broken chain is not sealed", func(t *testing.T) {
		env := newEnv(t)
		env.panel.Audit.SealKey = func() []byte { return nil }
		for i := range 3 {
			testutil.NewHost(t, env.app, fmt.Sprintf("host-%d", i))
		}
		env.panel.Audit.SealKey = func() []byte { return key }
		exec(t, env, `UPDATE audit_log SET record_label = 'evil' WHERE seq = 2`)

		if _, err := env.panel.Audit.Seal(audit.System("panel audit seal", audit.SourceCommand)); err == nil {
			t.Fatal("a broken chain was sealed")
		}
	})

	t.Run("without the key", func(t *testing.T) {
		env := newEnv(t)
		env.panel.Audit.SealKey = func() []byte { return nil }
		if _, err := env.panel.Audit.Seal(audit.System("panel audit seal", audit.SourceCommand)); err == nil {
			t.Fatal("sealed without the key")
		}
	})
}

func lastEntry(t *testing.T, env *env) *core.Record {
	t.Helper()
	records, err := env.app.FindRecordsByFilter("audit_log", "", "-seq", 1, 0)
	if err != nil || len(records) == 0 {
		t.Fatalf("no entries: %v", err)
	}
	return records[0]
}
