package panel

import (
	"errors"
	"net/http"

	"github.com/pocketbase/pocketbase/core"
)

// PocketBase's own backups (#32). Making one goes on, without the panel's own
// files: the metrics, whose database PocketBase would copy torn, the spool of
// the panel's copies, its serve lock and its restore marker. Restoring one is
// refused: PocketBase restarts on it with the same master key and
// environment, which a backup sealed with another key would never start
// with, over and over; `panel recover` checks the key first. Its route
// answers 403, as PocketBase restores in the background once it answered.

// pocketBaseRestoreRefused says how the panel is restored instead.
const pocketBaseRestoreRefused = "Restore the panel with `panel recover`, which checks the master key first: see docs/disaster-recovery.md."

// pocketBaseRestoreRoute is the route of PocketBase's restore, as the
// router's pattern names it.
const pocketBaseRestoreRoute = http.MethodPost + " /api/backups/{key}/restore"

// pocketBaseLeftOut are the panel's files a backup of PocketBase leaves out,
// besides those PocketBase leaves out itself.
var pocketBaseLeftOut = []string{metricsFile, metricsFile + "-wal", metricsFile + "-shm", exportSpool, ".pc_serve.lock", ".pc_restored.json"}

func (p *Panel) registerPocketBaseBackups() {
	p.App.OnBackupCreate().BindFunc(func(e *core.BackupEvent) error {
		e.Exclude = append(e.Exclude, pocketBaseLeftOut...)
		return e.Next()
	})
	p.App.OnBackupRestore().BindFunc(func(*core.BackupEvent) error {
		return errors.New(pocketBaseRestoreRefused) //nolint:staticcheck // the sentence the refusal says
	})
	p.App.OnServe().BindFunc(func(e *core.ServeEvent) error {
		e.Router.BindFunc(func(e *core.RequestEvent) error {
			if e.Request.Pattern == pocketBaseRestoreRoute {
				return e.ForbiddenError(pocketBaseRestoreRefused, nil)
			}
			return e.Next()
		})
		return e.Next()
	})
}
