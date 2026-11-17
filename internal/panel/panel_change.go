package panel

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// The panel's backup after a change of its servers (#32). A server enrolled
// after the panel's last backup would be refused by a panel restored from
// it, and a server removed or a certificate revoked since would be back: so
// 10 minutes after a server is enrolled or removed, or a certificate
// revoked, the panel backs itself up, one run per target of its job, with
// the trigger change. Changes that come meanwhile wait for the same run. A
// run of the job that is still going postpones it 10 minutes, and a change
// run starts no sooner than 30 minutes after the one before.

const (
	// changeDelay is how long after a change the panel backs itself up.
	changeDelay = 10 * time.Minute
	// changeSpacing is how long after a change run started the next one
	// may start.
	changeSpacing = 30 * time.Minute
)

// changeBackups is the panel's pending backup after a change.
type changeBackups struct {
	mu      sync.Mutex
	pending bool
	// delay and spacing replace changeDelay and changeSpacing when set
	delay, spacing atomic.Int64
}

// SetChangeBackupTiming sets how long after a change of the servers the
// panel backs itself up, and how long after a change run the next may start,
// for tests; zero keeps 10 and 30 minutes.
func (p *Panel) SetChangeBackupTiming(delay, spacing time.Duration) {
	p.changes.delay.Store(int64(delay))
	p.changes.spacing.Store(int64(spacing))
}

func (p *Panel) changeDelay() time.Duration {
	if delay := time.Duration(p.changes.delay.Load()); delay > 0 {
		return delay
	}
	return changeDelay
}

func (p *Panel) changeSpacing() time.Duration {
	if spacing := time.Duration(p.changes.spacing.Load()); spacing > 0 {
		return spacing
	}
	return changeSpacing
}

// registerChangeBackups backs the panel up after a server is enrolled or
// removed, or a certificate is revoked.
func (p *Panel) registerChangeBackups() {
	soon := func(e *core.RecordEvent) error {
		p.panelBackupSoon(p.changeDelay())
		return e.Next()
	}
	p.App.OnRecordAfterCreateSuccess(schema.Hosts).BindFunc(soon)
	p.App.OnRecordAfterDeleteSuccess(schema.Hosts).BindFunc(soon)
	p.App.OnRecordAfterCreateSuccess(schema.RevokedCerts).BindFunc(soon)
}

// panelBackupSoon backs the panel up after a delay, unless such a backup is
// pending already: it takes the change too.
func (p *Panel) panelBackupSoon(delay time.Duration) {
	p.changes.mu.Lock()
	defer p.changes.mu.Unlock()
	if p.changes.pending {
		return
	}
	p.changes.pending = true
	p.later.after(delay, p.panelBackupAfterChange)
}

// panelBackupAfterChange starts the panel's change runs, one per target of
// its job, if it has an enabled one with targets, as the scheduler starts
// its runs; or waits for a run of it that is still going, or for the time
// a change run leaves before the next.
func (p *Panel) panelBackupAfterChange() {
	p.changes.mu.Lock()
	p.changes.pending = false
	p.changes.mu.Unlock()

	// one pass of the scheduler at a time, this one included
	p.backupPass.Lock()
	defer p.backupPass.Unlock()
	jobs, err := p.App.FindRecordsByFilter(schema.BackupJobs, "source_kind={:panel} && enabled=true", "", 1, 0,
		dbx.Params{"panel": schema.SourcePanel})
	if err != nil {
		p.Log.Error("failed to read the panel's backup job", "error", err)
		return
	}
	if len(jobs) == 0 {
		return
	}
	job := jobs[0]
	targets := p.jobTargets(job)
	if len(targets) == 0 {
		return
	}
	// its copy may be older than the change
	for _, target := range targets {
		if p.runGoing(job, target) {
			p.panelBackupSoon(p.changeDelay())
			return
		}
	}
	// a run that was skipped did not start
	last, err := p.App.FindRecordsByFilter(schema.BackupRuns, "job={:job} && trigger={:change} && status!={:skipped}", "-created", 1, 0,
		dbx.Params{"job": job.Id, "change": schema.RunChange, "skipped": schema.RunSkipped})
	if err != nil {
		p.Log.Error("failed to read the panel's backup runs", "error", err)
		return
	}
	if len(last) > 0 {
		if next := last[0].GetDateTime("created").Time().Add(p.changeSpacing()); time.Now().Before(next) {
			p.panelBackupSoon(time.Until(next))
			return
		}
	}
	p.Log.Info("the panel backs itself up after a change of its servers", "job", job.Id)
	p.startJobRuns(job, schema.RunChange, time.Now().UTC().Truncate(time.Minute))
}
