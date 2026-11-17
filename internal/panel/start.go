package panel

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pocketbase/dbx"

	"github.com/oldbear24/PocketServerManager/internal/ca"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
)

// The guards of a panel's start (#32): a wrong but well-formed master key,
// a database without its CA, or a second panel on the same pb_data must
// fail closed before anything is written, and never make a new CA that would
// have every server enroll again. See docs/architecture.md, "Panel backup
// and disaster recovery (#32)".

// serveLockName is the file in the data directory a serving panel holds
// locked for its whole life.
const serveLockName = ".pc_serve.lock"

// refusal is a sentence that refuses a start or a recovery, as the panel
// prints it.
type refusal string

func (r refusal) Error() string { return string(r) }

func refusalf(format string, args ...any) error { return refusal(fmt.Sprintf(format, args...)) }

// panelState is the row of the plain table schema.PanelTable.
type panelState struct {
	KeyID        string `db:"key_id"`
	CAPin        string `db:"ca_pin"`
	RestoredAt   string `db:"restored_at"`
	RestoredFrom string `db:"restored_from"`
}

// readPanelState reads the panel's row; a table without it reads as empty.
func readPanelState(db dbx.Builder) (panelState, error) {
	var row panelState
	err := db.Select("key_id", "ca_pin", "restored_at", "restored_from").
		From(schema.PanelTable).Where(dbx.HashExp{"id": 1}).One(&row)
	if errors.Is(err, sql.ErrNoRows) {
		return panelState{RestoredFrom: "{}"}, nil
	}
	return row, err
}

// setPanelState sets fields of the panel's row, making the row if it is
// missing.
func setPanelState(db dbx.Builder, fields dbx.Params) error {
	if _, err := db.NewQuery("INSERT OR IGNORE INTO {{" + schema.PanelTable + "}} ([[id]]) VALUES (1)").Execute(); err != nil {
		return err
	}
	_, err := db.Update(schema.PanelTable, fields, dbx.HashExp{"id": 1}).Execute()
	return err
}

// takeServeLock locks <data dir>/.pc_serve.lock for the panel's life: two
// panels on one pb_data would both command the servers. The file is opened
// close-on-exec, so a restart that executes the panel again takes it anew.
func (p *Panel) takeServeLock() error {
	if p.serveLock != nil {
		return nil
	}
	dir := p.App.DataDir()
	file, err := os.OpenFile(filepath.Join(dir, serveLockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("the serve lock of %s: %w", dir, err)
	}
	held, err := tryLock(file)
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("the serve lock of %s: %w", dir, err)
	}
	if !held {
		_ = file.Close()
		return refusalf("Another panel serves %s: two panels on one pb_data would both command the servers.", dir)
	}
	p.serveLock = file
	return nil
}

// releaseServeLock lets another panel serve the data directory.
func (p *Panel) releaseServeLock() {
	if p.serveLock != nil {
		_ = p.serveLock.Close()
		p.serveLock = nil
	}
}

// checkKeyID refuses a master key other than the one that sealed the
// database, before anything is written: a wrong but well-formed key would
// otherwise get as far as the CA, or seal the audit log with itself.
func (p *Panel) checkKeyID() error {
	row, err := readPanelState(p.App.DB())
	if err != nil {
		return fmt.Errorf("reading %s: %w", schema.PanelTable, err)
	}
	id := secrets.KeyID(p.masterKey)
	if row.KeyID != "" && row.KeyID != id {
		return refusalf("This master key (id %s) did not seal this database (id %s): start the panel with the key file of its recovery kit.", id, row.KeyID)
	}
	return nil
}

// openAuthority opens the internal CA with the guards of a start: a new CA
// only for a database that never had servers or a CA, and the one
// POCKETCLOUD_EXPECT_CA_PIN names if it names one. Once it opened, which
// proves the master key, an empty key id and pin are recorded: the upgrade
// of a panel from before them.
func (p *Panel) openAuthority() (*ca.Authority, error) {
	row, err := readPanelState(p.App.DB())
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", schema.PanelTable, err)
	}
	hosts, err := p.App.CountRecords(schema.Hosts)
	if err != nil {
		return nil, err
	}
	revoked, err := p.App.CountRecords(schema.RevokedCerts)
	if err != nil {
		return nil, err
	}
	authority, err := ca.OpenWith(p.App, p.Box, p.serverNames(), ca.Options{
		MayCreate: row.CAPin == "" && hosts == 0 && revoked == 0,
		HadPin:    row.CAPin,
		ExpectPin: p.Config.ExpectCAPin,
	})
	if err != nil {
		return nil, err
	}

	fields := dbx.Params{}
	if row.KeyID == "" {
		fields["key_id"] = secrets.KeyID(p.masterKey)
	}
	if row.CAPin == "" {
		fields["ca_pin"] = authority.Pin()
	}
	if len(fields) > 0 {
		if err := setPanelState(p.App.DB(), fields); err != nil {
			return nil, fmt.Errorf("recording the key id and the CA's pin: %w", err)
		}
	}
	return authority, nil
}
