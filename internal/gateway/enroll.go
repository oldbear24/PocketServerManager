package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/enroll"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// Enroll is the only RPC without a client certificate. A valid token proves that
// someone with access to the panel asked for this server to be connected.
func (s *Server) Enroll(ctx context.Context, req *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
	addr := ""
	if p, ok := peer.FromContext(ctx); ok {
		addr = peerHost(p.Addr.String())
	}

	if !s.limiter.Allow(addr) {
		s.enrollFailed(addr, req.GetName(), "too many attempts")
		return nil, status.Error(codes.ResourceExhausted, "too many enrollment attempts from this address: wait a minute and try again")
	}

	token, err := s.App.FindFirstRecordByData(schema.EnrollmentTokens, "token_hash", enroll.Hash(req.GetToken()))
	if err != nil {
		s.enrollFailed(addr, req.GetName(), "unknown token")
		return nil, status.Error(codes.Unauthenticated, "this enrollment token is not valid: create a new one on the “Connect a server” page")
	}

	if !token.GetDateTime("used_at").IsZero() {
		s.enrollFailed(addr, token.GetString("name"), "token already used")
		return nil, status.Error(codes.Unauthenticated, "this enrollment token has already been used: tokens work once, create a new one on the “Connect a server” page")
	}

	if token.GetDateTime("expires_at").Time().Before(time.Now()) {
		s.enrollFailed(addr, token.GetString("name"), "token expired")
		return nil, status.Error(codes.Unauthenticated, "this enrollment token has expired: tokens are valid for 15 minutes, create a new one on the “Connect a server” page")
	}

	name := token.GetString("name")
	if req.GetName() != "" {
		name = req.GetName()
	}
	roles := token.GetStringSlice("roles")
	if len(req.GetRoles()) > 0 {
		roles = req.GetRoles()
	}

	if _, err := s.App.FindFirstRecordByData(schema.Hosts, "name", name); err == nil {
		s.enrollFailed(addr, name, "name taken")
		return nil, status.Errorf(codes.AlreadyExists, "a server named %q already exists: remove it first or pick another name", name)
	}

	hosts, err := s.App.FindCachedCollectionByNameOrId(schema.Hosts)
	if err != nil {
		return nil, status.Error(codes.Internal, "the panel is not ready")
	}

	host := core.NewRecord(hosts)
	host.Id = core.GenerateDefaultRandomId()

	issued, err := s.Authority.IssueAgentCert(req.GetCsrPem(), host.Id)
	if err != nil {
		s.enrollFailed(addr, name, err.Error())
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}

	expires, err := types.ParseDateTime(issued.NotAfter)
	if err != nil {
		return nil, status.Error(codes.Internal, "invalid certificate expiry")
	}

	host.Set("name", name)
	host.Set("roles", roles)
	host.Set("status", schema.HostOffline)
	host.Set("cert_serial", issued.Serial)
	host.Set("cert_expires_at", expires)
	applyFacts(host, req.GetFacts())

	actor := audit.System("enrollment", audit.SourceAgent)
	actor.IP = addr

	// the token is consumed in the same transaction that creates the host, so two
	// agents racing with one token cannot both succeed
	err = s.App.RunInTransaction(func(tx core.App) error {
		fresh, err := tx.FindRecordById(schema.EnrollmentTokens, token.Id)
		if err != nil {
			return err
		}
		if !fresh.GetDateTime("used_at").IsZero() {
			return errTokenRaced
		}

		if err := tx.SaveWithContext(audit.WithActor(ctx, actor), host); err != nil {
			return err
		}

		fresh.Set("used_at", types.NowDateTime())
		fresh.Set("host", host.Id)

		return tx.Save(fresh)
	})
	if err != nil {
		if errors.Is(err, errTokenRaced) {
			return nil, status.Error(codes.Unauthenticated, "this enrollment token has already been used")
		}
		s.enrollFailed(addr, name, err.Error())
		return nil, status.Errorf(codes.InvalidArgument, "the server could not be created: %v", err)
	}

	s.Audit.Security(audit.ActionEnrollment, actor, name, map[string]any{"host": host.Id, "serial": issued.Serial})
	events.Log(s.App, host.Id, "agent.enrolled", schema.SeverityInfo,
		fmt.Sprintf("Server %s was connected to the panel", name), map[string]any{"addr": addr})

	return &agentv1.EnrollResponse{
		HostId:         host.Id,
		Name:           name,
		Roles:          roles,
		CertificatePem: issued.ChainPEM,
		CaPem:          s.Authority.RootPEM(),
		ExpiresAt:      timestamppb.New(issued.NotAfter),
	}, nil
}

var errTokenRaced = errors.New("token already used")

func (s *Server) enrollFailed(addr, name, reason string) {
	s.Log.Warn("a server failed to enroll", "addr", addr, "name", name, "reason", reason)

	actor := audit.System("enrollment", audit.SourceAgent)
	actor.IP = addr
	s.Audit.Security(audit.ActionEnrollmentFailed, actor, name, map[string]any{"reason": reason})
}

// RenewCertificate rotates the agent's identity over the existing channel. The
// previous certificate stays valid until the agent first calls with the new one,
// so a response lost on the way cannot lock the agent out.
func (s *Server) RenewCertificate(ctx context.Context, req *agentv1.RenewCertificateRequest) (*agentv1.RenewCertificateResponse, error) {
	hostID := hostIDFrom(ctx)

	issued, err := s.Authority.IssueAgentCert(req.GetCsrPem(), hostID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}

	expires, err := types.ParseDateTime(issued.NotAfter)
	if err != nil {
		return nil, status.Error(codes.Internal, "invalid certificate expiry")
	}

	current, _ := ctx.Value(identityKey{}).(identity)

	err = s.updateHost(hostID, func(host *core.Record) {
		host.Set("cert_prev_serial", current.Serial)
		host.Set("cert_serial", issued.Serial)
		host.Set("cert_expires_at", expires)
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to store the renewed certificate: %v", err)
	}

	events.Log(s.App, hostID, "agent.certificate_renewed", schema.SeverityInfo,
		"The agent certificate was renewed until "+issued.NotAfter.Format(time.DateOnly), map[string]any{"serial": issued.Serial})

	return &agentv1.RenewCertificateResponse{
		CertificatePem: issued.ChainPEM,
		ExpiresAt:      timestamppb.New(issued.NotAfter),
	}, nil
}

func (s *Server) retirePreviousCert(hostID string) {
	var previous, name string

	err := s.updateHost(hostID, func(host *core.Record) {
		previous = host.GetString("cert_prev_serial")
		name = host.GetString("name")
		host.Set("cert_prev_serial", "")
	})
	if err != nil {
		s.Log.Error("failed to retire a certificate", "host", hostID, "error", err)
		return
	}

	if previous == "" {
		return
	}

	if err := Revoke(s.App, previous, hostID, name, "replaced by a renewed certificate"); err != nil {
		s.Log.Error("failed to retire a certificate", "host", hostID, "error", err)
	}
}

// Revoke adds a serial to the denylist.
func Revoke(app core.App, serial, hostID, hostName, reason string) error {
	if serial == "" {
		return nil
	}

	if _, err := app.FindFirstRecordByData(schema.RevokedCerts, "serial", serial); err == nil {
		return nil
	}

	collection, err := app.FindCachedCollectionByNameOrId(schema.RevokedCerts)
	if err != nil {
		return err
	}

	record := core.NewRecord(collection)
	record.Set("serial", serial)
	record.Set("host", hostID)
	record.Set("host_name", hostName)
	record.Set("reason", reason)

	return app.Save(record)
}

// BindHostRemoval revokes the certificates of a host when it is removed, and
// drops its open command stream.
func (s *Server) BindHostRemoval() {
	s.App.OnRecordAfterDeleteSuccess(schema.Hosts).BindFunc(func(e *core.RecordEvent) error {
		name := e.Record.GetString("name")

		for _, serial := range []string{e.Record.GetString("cert_serial"), e.Record.GetString("cert_prev_serial")} {
			if serial == "" {
				continue
			}
			if err := Revoke(e.App, serial, e.Record.Id, name, "the host was removed"); err != nil {
				s.Log.Error("failed to revoke a certificate", "host", e.Record.Id, "error", err)
				continue
			}
			s.Audit.Security(audit.ActionCertRevoked, audit.System("host removal", audit.SourcePanel), name, map[string]any{"serial": serial})
		}

		return e.Next()
	})
}

func applyFacts(host *core.Record, facts *agentv1.HostFacts) {
	if facts == nil {
		return
	}

	set := func(field, value string) {
		if value != "" {
			host.Set(field, value)
		}
	}

	set("hostname", facts.GetHostname())
	set("os", facts.GetOs())
	set("kernel", facts.GetKernel())
	set("arch", facts.GetArch())
	set("ip4", facts.GetIp4())
	set("ip6", facts.GetIp6())
	set("docker_version", facts.GetDockerVersion())
	set("agent_version", strings.TrimPrefix(facts.GetAgentVersion(), "v"))
	set("agent_image_digest", facts.GetAgentImageDigest())
	set("agent_container_id", facts.GetAgentContainerId())

	if facts.GetCpuCores() > 0 {
		host.Set("cpu_cores", facts.GetCpuCores())
	}
	if facts.GetMemBytes() > 0 {
		host.Set("mem_bytes", facts.GetMemBytes())
	}
}
