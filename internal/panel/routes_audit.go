package panel

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

const auditExportBatch = 500

// bindAuditRoutes adds the integrity check, the JSONL export, the revert API and
// the daily jobs of the change audit.
func (p *Panel) bindAuditRoutes() {
	p.App.OnServe().BindFunc(func(e *core.ServeEvent) error {
		group := e.Router.Group("/api/pc/audit")
		group.Bind(apis.RequireAuth())
		group.BindFunc(requireRole(schema.RoleAdmin))

		group.POST("/verify", p.handleAuditVerify)
		group.GET("/export", p.handleAuditExport)
		group.POST("/{id}/revert", p.handleAuditRevert)

		return e.Next()
	})

	p.App.Cron().MustAdd("pocketcloud_audit_verify", "20 3 * * *", func() {
		if _, err := p.verifyAudit(audit.System("audit integrity job", audit.SourceScheduler)); err != nil {
			p.Log.Error("audit verification failed", "error", err)
		}
	})

	p.App.Cron().MustAdd("pocketcloud_audit_retention", "40 3 * * *", func() {
		removed, err := p.Audit.Prune(p.auditRetention())
		if err != nil {
			p.Log.Error("audit retention failed", "error", err)
			return
		}
		if removed > 0 {
			p.Log.Info("audit retention removed old entries", "count", removed)
		}
	})
}

func (p *Panel) auditRetention() time.Duration {
	days := 365
	if record, err := p.settingsRecord(); err == nil && record.GetInt("audit_retention_days") > 0 {
		days = record.GetInt("audit_retention_days")
	}
	return time.Duration(days) * 24 * time.Hour
}

func (p *Panel) settingsRecord() (*core.Record, error) {
	records, err := p.App.FindRecordsByFilter(schema.Settings, "", "-created", 1, 0)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, errors.New("the settings record is missing")
	}
	return records[0], nil
}

// verifyAudit runs the integrity check. A broken chain raises an alert and a
// security event, because it means that someone edited the database directly.
func (p *Panel) verifyAudit(actor audit.Actor) (audit.VerifyResult, error) {
	result, err := audit.Verify(p.App)
	if err != nil {
		return result, err
	}

	if !result.OK {
		p.Log.Error("audit log integrity check failed", "seq", result.BadSeq, "reason", result.Reason)
		p.Audit.Security(audit.ActionIntegrityBroken, actor, "audit_log", map[string]any{"seq": result.BadSeq, "reason": result.Reason})
		if err := events.RaiseAlert(p.App, "audit_integrity", "audit_log", "", schema.SeverityCritical, "Audit log integrity check failed: "+result.Reason); err != nil {
			return result, err
		}
	}

	return result, nil
}

func (p *Panel) handleAuditVerify(e *core.RequestEvent) error {
	result, err := p.verifyAudit(audit.ActorFromRequest(e))
	if err != nil {
		return e.InternalServerError("Failed to verify the audit log.", err)
	}
	return e.JSON(http.StatusOK, result)
}

// handleAuditExport streams the entries matching an optional filter as JSONL,
// with their hashes, so that the chain can be verified outside the panel.
func (p *Panel) handleAuditExport(e *core.RequestEvent) error {
	filter := e.Request.URL.Query().Get("filter")

	// fail with a proper error before the response starts
	if _, err := p.App.FindRecordsByFilter(schema.AuditLog, filter, "seq", 1, 0); err != nil {
		return e.BadRequestError("Invalid filter.", err)
	}

	e.Response.Header().Set("Content-Type", "application/x-ndjson")
	e.Response.Header().Set("Content-Disposition", `attachment; filename="audit-log.jsonl"`)
	e.Response.WriteHeader(http.StatusOK)

	out := bufio.NewWriter(e.Response)
	defer out.Flush()

	encoder := json.NewEncoder(out)

	for offset := 0; ; offset += auditExportBatch {
		records, err := p.App.FindRecordsByFilter(schema.AuditLog, filter, "seq", auditExportBatch, offset)
		if err != nil {
			return err
		}
		if len(records) == 0 {
			return nil
		}

		for _, record := range records {
			if err := encoder.Encode(record.FieldsData()); err != nil {
				return err
			}
		}
	}
}

// handleAuditRevert puts back the previous non-secret values of a change, as a
// new audited change. Objects with revisions are reverted by their own feature
// (a stack deploys the old revision as a new one).
func (p *Panel) handleAuditRevert(e *core.RequestEvent) error {
	entry, err := p.App.FindRecordById(schema.AuditLog, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The audit entry does not exist.", err)
	}

	actor := audit.ActorFromRequest(e)
	actor.Note = "revert of entry " + entry.Id

	if err := audit.Revert(e.Request.Context(), p.App, entry, actor); err != nil {
		var refused *audit.RevertError
		if errors.As(err, &refused) {
			return e.BadRequestError(refused.Error(), nil)
		}
		return e.InternalServerError("Failed to revert the change.", err)
	}

	return e.NoContent(http.StatusNoContent)
}

// requireRole only lets users with one of the roles through. Superusers pass.
func requireRole(roles ...string) func(e *core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		if e.HasSuperuserAuth() {
			return e.Next()
		}
		if e.Auth != nil && slices.Contains(roles, e.Auth.GetString("role")) {
			return e.Next()
		}
		return e.ForbiddenError("You are not allowed to do this.", nil)
	}
}
