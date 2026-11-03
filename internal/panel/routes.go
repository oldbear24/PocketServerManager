package panel

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/enroll"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/version"
)

var hostNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func (p *Panel) bindRoutes(group *router.RouterGroup[*core.RequestEvent]) {
	group.GET("/info", p.handleInfo)
	group.POST("/enrollment-tokens", p.handleCreateEnrollmentToken).BindFunc(requireRole(schema.RoleAdmin))
	group.POST("/hosts/{id}/update", p.handleUpdateHost).BindFunc(requireRole(schema.RoleAdmin))
	group.POST("/hosts/update-all", p.handleUpdateAllHosts).BindFunc(requireRole(schema.RoleAdmin))
}

// bindPublicRoutes adds what an unauthenticated server needs for connecting: the
// install script and the root certificate. Neither of them is secret.
func (p *Panel) bindPublicRoutes(se *core.ServeEvent) {
	p.bindSetupRoutes(se)

	se.Router.GET("/api/pc/ca.pem", func(e *core.RequestEvent) error {
		e.Response.Header().Set("Content-Type", "application/x-pem-file")
		e.Response.Header().Set("Content-Disposition", `attachment; filename="pocketcloud-root-ca.pem"`)
		_, err := e.Response.Write(p.Authority.RootPEM())
		return err
	})

	se.Router.GET("/api/pc/install.sh", func(e *core.RequestEvent) error {
		e.Response.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		_, err := e.Response.Write([]byte(p.installScript(e.Request.Host)))
		return err
	})
}

// handleInfo tells the UI about this panel. The gateway's address is what the
// install command shows; the counts feed the status card in the sidebar.
func (p *Panel) handleInfo(e *core.RequestEvent) error {
	connected := struct {
		Count int `db:"count"`
	}{}
	total := struct {
		Count int `db:"count"`
	}{}

	err := p.App.RecordQuery(schema.Hosts).
		Select("count(*) as count").
		AndWhere(dbx.NewExp("[[stream_open]] = true")).
		One(&connected)
	if err != nil {
		return e.InternalServerError("", err)
	}

	err = p.App.RecordQuery(schema.Hosts).Select("count(*) as count").One(&total)
	if err != nil {
		return e.InternalServerError("", err)
	}

	return e.JSON(http.StatusOK, map[string]any{
		"version":         version.Version,
		"public_url":      p.Config.PublicURL,
		"grpc_address":    p.Config.AgentEndpointFor(e.Request.Host),
		"gateway_running": p.Gateway != nil && p.Config.GRPCAddr != "",
		"ca_pin":          p.Authority.Pin(),
		"agent_image":     version.AgentRef(),
		"updater_image":   version.UpdaterRef(),
		"images_pinned":   version.Pinned(),
		"agents": map[string]int{
			"connected": connected.Count,
			"total":     total.Count,
		},
	})
}

// handleCreateEnrollmentToken creates a one-time token. The plain token leaves
// the panel only in this response; only its hash is stored.
func (p *Panel) handleCreateEnrollmentToken(e *core.RequestEvent) error {
	var body struct {
		Name  string   `json:"name"`
		Roles []string `json:"roles"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}

	if !hostNamePattern.MatchString(body.Name) || len(body.Name) > 64 {
		return e.BadRequestError("A server name can only hold lower case letters, digits and dashes.", nil)
	}

	allowed := []string{schema.HostRoleDocker, schema.HostRoleEdge, schema.HostRoleBackup}
	for _, role := range body.Roles {
		if !slices.Contains(allowed, role) {
			return e.BadRequestError(fmt.Sprintf("Unknown role %q.", role), nil)
		}
	}

	if _, err := p.App.FindFirstRecordByData(schema.Hosts, "name", body.Name); err == nil {
		return e.BadRequestError(fmt.Sprintf("A server named %q already exists.", body.Name), nil)
	}

	createdBy := ""
	if !e.HasSuperuserAuth() {
		createdBy = e.Auth.Id
	}

	record, token, err := enroll.Create(p.App, body.Name, body.Roles, createdBy)
	if err != nil {
		return e.InternalServerError("Failed to create the token.", err)
	}

	p.Audit.Security(audit.ActionEnrollmentToken, audit.ActorFromRequest(e), body.Name, map[string]any{
		"token_id": record.Id,
		"hint":     record.GetString("hint"),
		"roles":    body.Roles,
	})

	return e.JSON(http.StatusOK, map[string]any{
		"id":          record.Id,
		"token":       token,
		"name":        body.Name,
		"roles":       body.Roles,
		"expires_at":  record.GetDateTime("expires_at"),
		"panel":       p.Config.AgentEndpointFor(e.Request.Host),
		"ca_pin":      p.Authority.Pin(),
		"agent_image": version.AgentRef(),
	})
}

// installScript is the wrapper for hosts where a one-liner is more convenient: it
// checks Docker and runs the same `docker run` as the UI shows.
func (p *Panel) installScript(requestHost string) string {
	return fmt.Sprintf(`#!/bin/sh
# PocketCloud agent installer: checks for Docker, pulls the agent image and runs
# the same "docker run" as the "Connect a server" screen shows.
#
#   curl -fsSL <panel URL>/api/pc/install.sh | TOKEN=ENR-... NAME=my-server ROLES=docker sh
set -eu

: "${TOKEN:?set TOKEN to the one-time token from the Connect a server screen}"
: "${NAME:?set NAME to the name of this server}"
ROLES="${ROLES:-docker}"
PANEL="${PANEL:-%[1]s}"
CA_PIN="${CA_PIN:-%[2]s}"
IMAGE="${IMAGE:-%[3]s}"

if ! command -v docker >/dev/null 2>&1; then
	echo "Docker is not installed. Install Docker 24 or newer and run this again: https://docs.docker.com/engine/install/" >&2
	exit 1
fi

if ! docker info >/dev/null 2>&1; then
	echo "Docker does not answer. Is the daemon running, and may this user use it?" >&2
	exit 1
fi

docker pull "$IMAGE"

docker run -d --name pocketcloud-agent --restart unless-stopped \
	-v /var/run/docker.sock:/var/run/docker.sock \
	-v pocketcloud-agent:/var/lib/pocketcloud \
	-v /:/host:ro,rslave \
	-e PANEL="$PANEL" -e CA_PIN="$CA_PIN" \
	-e TOKEN="$TOKEN" \
	-e NAME="$NAME" -e ROLES="$ROLES" \
	"$IMAGE"

echo "The agent is running. The server appears in the panel within a few seconds."
`, p.Config.AgentEndpointFor(requestHost), p.Authority.Pin(), version.AgentRef())
}

// updateBatches updates hosts one at a time: a bad release stops at the first
// host instead of reaching all of them. A batch lives in memory; after a panel
// restart the remaining hosts simply show up as outdated again.
type updateBatches struct {
	panel *Panel

	mu      sync.Mutex
	pending map[string]*updateBatch
}

type updateBatch struct {
	hosts     []string
	userID    string
	requester string
}

func newUpdateBatches(p *Panel) *updateBatches {
	return &updateBatches{panel: p, pending: make(map[string]*updateBatch)}
}

func (u *updateBatches) register() {
	u.panel.Commands.OnFinish("agent.update", u.next)
}

// next starts the following host of a batch once the previous one has finished.
// A failed update stops the batch: the other hosts stay outdated and visible.
func (u *updateBatches) next(finished *core.Record) {
	correlation := finished.GetString("correlation_id")

	u.mu.Lock()
	batch := u.pending[correlation]
	if batch == nil {
		u.mu.Unlock()
		return
	}

	if finished.GetString("status") != schema.CommandSucceeded || len(batch.hosts) == 0 {
		delete(u.pending, correlation)
		u.mu.Unlock()
		return
	}

	hostID := batch.hosts[0]
	batch.hosts = batch.hosts[1:]
	u.mu.Unlock()

	if _, err := u.panel.createUpdate(hostID, batch.userID, batch.requester, correlation); err != nil {
		u.panel.Log.Error("failed to continue an update batch", "host", hostID, "error", err)
		u.mu.Lock()
		delete(u.pending, correlation)
		u.mu.Unlock()
	}
}

const updateHealthTimeout = 3 * time.Minute

// createUpdate queues an agent.update with the images of this panel's release,
// always pinned by digest.
func (p *Panel) createUpdate(hostID, userID, requester, correlation string) (*core.Record, error) {
	return p.Commands.Create(context.Background(), commands.Options{
		Host:        hostID,
		Label:       "agent " + version.Version,
		RequestedBy: userID,
		Requester:   requester,
		Correlation: correlation,
		Command: &agentv1.Command{Payload: &agentv1.Command_AgentUpdate{AgentUpdate: &agentv1.AgentUpdate{
			AgentImage:     version.AgentRef(),
			UpdaterImage:   version.UpdaterRef(),
			Version:        version.Version,
			HealthTimeoutS: int32(updateHealthTimeout.Seconds()),
		}}},
	})
}

func requester(e *core.RequestEvent) (id, label string) {
	if e.HasSuperuserAuth() {
		return "", "superuser " + e.Auth.Email()
	}
	return e.Auth.Id, e.Auth.Email()
}

// handleUpdateHost updates the agent of one host.
func (p *Panel) handleUpdateHost(e *core.RequestEvent) error {
	if !version.Pinned() {
		return e.BadRequestError("This build of the panel does not know the digests of its agent images, so it cannot update agents.", nil)
	}

	host, err := p.App.FindRecordById(schema.Hosts, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The server does not exist.", err)
	}

	userID, label := requester(e)

	record, err := p.createUpdate(host.Id, userID, label, "")
	if err != nil {
		return e.InternalServerError("Failed to queue the update.", err)
	}

	return e.JSON(http.StatusOK, record)
}

// handleUpdateAllHosts updates every outdated agent, one host at a time.
func (p *Panel) handleUpdateAllHosts(e *core.RequestEvent) error {
	if !version.Pinned() {
		return e.BadRequestError("This build of the panel does not know the digests of its agent images, so it cannot update agents.", nil)
	}

	hosts := []*core.Record{}
	err := p.App.RecordQuery(schema.Hosts).
		AndWhere(dbx.NewExp("[[agent_version]] != {:version} AND [[status]] != {:offline}", dbx.Params{
			"version": strings.TrimPrefix(version.Version, "v"),
			"offline": schema.HostOffline,
		})).
		OrderBy("name ASC").
		All(&hosts)
	if err != nil {
		return e.InternalServerError("Failed to read the servers.", err)
	}
	if len(hosts) == 0 {
		return e.BadRequestError("Every connected agent is already up to date.", nil)
	}

	userID, label := requester(e)
	correlation := core.GenerateDefaultRandomId()

	rest := make([]string, 0, len(hosts)-1)
	for _, host := range hosts[1:] {
		rest = append(rest, host.Id)
	}

	p.Updates.mu.Lock()
	p.Updates.pending[correlation] = &updateBatch{hosts: rest, userID: userID, requester: label}
	p.Updates.mu.Unlock()

	if _, err := p.createUpdate(hosts[0].Id, userID, label, correlation); err != nil {
		p.Updates.mu.Lock()
		delete(p.Updates.pending, correlation)
		p.Updates.mu.Unlock()
		return e.InternalServerError("Failed to queue the update.", err)
	}

	return e.JSON(http.StatusOK, map[string]any{"correlation_id": correlation, "hosts": len(hosts)})
}
