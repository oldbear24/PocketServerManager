// Package events writes the timeline of hosts and resources, and opens and
// resolves alerts.
package events

import (
	"log/slog"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// Log adds an entry to the events collection. A failure here is logged and never
// breaks the operation being recorded.
func Log(app core.App, host, kind, severity, message string, data map[string]any) {
	collection, err := app.FindCachedCollectionByNameOrId(schema.Events)
	if err != nil {
		slog.Error("failed to log an event", "kind", kind, "error", err)
		return
	}

	record := core.NewRecord(collection)
	record.Set("host", host)
	record.Set("kind", kind)
	record.Set("severity", severity)
	record.Set("message", message)
	if data != nil {
		record.Set("data", data)
	}

	if err := app.Save(record); err != nil {
		slog.Error("failed to log an event", "kind", kind, "error", err)
	}
}

// RaiseAlert opens an alert for a rule and target, or refreshes the open one, so
// that a problem that lasts never produces more than one alert. The open one
// takes the new message and severity.
func RaiseAlert(app core.App, rule, target, host, severity, message string) error {
	now := types.NowDateTime()

	open, err := openAlerts(app, rule, target)
	if err != nil {
		return err
	}
	if len(open) > 0 {
		open[0].Set("last_seen", now)
		open[0].Set("message", message)
		// a problem that grows, as a certificate closer to its expiry
		open[0].Set("severity", severity)
		return app.Save(open[0])
	}

	collection, err := app.FindCachedCollectionByNameOrId(schema.Alerts)
	if err != nil {
		return err
	}

	alert := core.NewRecord(collection)
	alert.Set("rule", rule)
	alert.Set("target", target)
	alert.Set("host", host)
	alert.Set("severity", severity)
	alert.Set("status", schema.AlertOpen)
	alert.Set("first_seen", now)
	alert.Set("last_seen", now)
	alert.Set("message", message)

	return app.Save(alert)
}

// ResolveAlert closes the open alert of a rule and target, if there is one.
func ResolveAlert(app core.App, rule, target string) error {
	open, err := openAlerts(app, rule, target)
	if err != nil {
		return err
	}

	for _, alert := range open {
		alert.Set("status", schema.AlertResolved)
		alert.Set("resolved_at", types.NowDateTime())
		if err := app.Save(alert); err != nil {
			return err
		}
	}

	return nil
}

func openAlerts(app core.App, rule, target string) ([]*core.Record, error) {
	records := []*core.Record{}
	err := app.RecordQuery(schema.Alerts).
		AndWhere(dbx.HashExp{"rule": rule, "target": target}).
		AndWhere(dbx.NewExp("[[status]] != {:resolved}", dbx.Params{"resolved": schema.AlertResolved})).
		All(&records)
	return records, err
}

// Prune removes events older than the retention period.
func Prune(app core.App, retention time.Duration) error {
	cutoff, err := types.ParseDateTime(time.Now().UTC().Add(-retention))
	if err != nil {
		return err
	}

	_, err = app.DB().NewQuery("DELETE FROM {{" + schema.Events + "}} WHERE [[created]] < {:cutoff}").
		Bind(dbx.Params{"cutoff": cutoff.String()}).Execute()

	return err
}
