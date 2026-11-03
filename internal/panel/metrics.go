package panel

import (
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"

	"github.com/oldbear24/PocketServerManager/internal/metrics"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

const metricsFile = "metrics.db"

// openMetrics opens the metrics database next to PocketBase's own.
func (p *Panel) openMetrics() error {
	store, err := metrics.Open(filepath.Join(p.App.DataDir(), metricsFile))
	if err != nil {
		return fmt.Errorf("the metrics database: %w", err)
	}
	p.Metrics = store
	return nil
}

// registerMetrics binds the rollups and what follows a removed host.
func (p *Panel) registerMetrics() {
	p.App.Cron().MustAdd("pocketcloud_metrics_rollup", "* * * * *", func() {
		if p.Metrics == nil {
			return
		}
		if err := p.Metrics.Rollup(); err != nil {
			p.Log.Error("the metrics rollup failed", "error", err)
		}
	})

	p.App.OnRecordAfterDeleteSuccess(schema.Hosts).BindFunc(func(e *core.RecordEvent) error {
		if p.Metrics != nil {
			if err := p.Metrics.DeleteHost(e.Record.Id); err != nil {
				p.Log.Error("failed to remove the metrics of a host", "host", e.Record.Id, "error", err)
			}
		}
		return e.Next()
	})
}

func (p *Panel) bindMetricsRoutes(group *router.RouterGroup[*core.RequestEvent]) {
	everyone := requireRole(schema.RoleViewer, schema.RoleOperator, schema.RoleAdmin)

	group.GET("/metrics/hosts/{id}", p.handleHostMetrics).BindFunc(everyone)
	group.GET("/metrics/hosts/{id}/containers", p.handleContainerValues).BindFunc(everyone)
	group.GET("/metrics/hosts/{id}/containers/{docker}", p.handleContainerMetrics).BindFunc(everyone)
}

// metricsWindow reads the range of a request; the resolution follows from it.
func metricsWindow(e *core.RequestEvent) (metrics.Window, error) {
	name := e.Request.URL.Query().Get("range")
	if name == "" {
		name = "1h"
	}

	window, ok := metrics.Windows[name]
	if !ok {
		return metrics.Window{}, e.BadRequestError("The range must be 1h, 24h or 7d.", nil)
	}

	return window, nil
}

func (p *Panel) handleHostMetrics(e *core.RequestEvent) error {
	host, err := p.App.FindRecordById(schema.Hosts, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The server does not exist.", err)
	}

	window, err := metricsWindow(e)
	if err != nil {
		return err
	}

	points, err := p.Metrics.HostRange(host.Id, window)
	if err != nil {
		return e.InternalServerError("Failed to read the metrics.", err)
	}

	mounts, err := p.Metrics.Mounts(host.Id)
	if err != nil {
		return e.InternalServerError("Failed to read the metrics.", err)
	}

	return e.JSON(http.StatusOK, map[string]any{"points": points, "mounts": mounts})
}

func (p *Panel) handleContainerValues(e *core.RequestEvent) error {
	host, err := p.App.FindRecordById(schema.Hosts, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The server does not exist.", err)
	}

	values, err := p.Metrics.LatestContainers(host.Id)
	if err != nil {
		return e.InternalServerError("Failed to read the metrics.", err)
	}

	return e.JSON(http.StatusOK, values)
}

func (p *Panel) handleContainerMetrics(e *core.RequestEvent) error {
	host, err := p.App.FindRecordById(schema.Hosts, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The server does not exist.", err)
	}

	window, err := metricsWindow(e)
	if err != nil {
		return err
	}

	points, err := p.Metrics.ContainerRange(host.Id, e.Request.PathValue("docker"), window)
	if err != nil {
		return e.InternalServerError("Failed to read the metrics.", err)
	}

	return e.JSON(http.StatusOK, points)
}
