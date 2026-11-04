package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/agent"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/gateway"
	"github.com/oldbear24/PocketServerManager/internal/inventory"
	"github.com/oldbear24/PocketServerManager/internal/pki"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

func TestEnrollmentConnectsAServer(t *testing.T) {
	h := newHarness(t)
	h.docker.AddContainer("nginx", "nginx:1", nil)

	started := time.Now()
	running, host := h.enrolled("nas-02")

	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("enrollment took %s", took)
	}

	if host.GetStringSlice("roles")[0] != schema.HostRoleDocker {
		t.Errorf("roles: %v", host.GetStringSlice("roles"))
	}
	if host.GetString("cert_serial") == "" || host.GetDateTime("cert_expires_at").IsZero() {
		t.Error("the certificate was not recorded")
	}
	if !host.GetDateTime("cert_expires_at").Time().After(time.Now().Add(179 * 24 * time.Hour)) {
		t.Errorf("the certificate must be valid for 180 days: %s", host.GetDateTime("cert_expires_at"))
	}

	// the key is on the volume, and the token is used up
	info, err := os.Stat(filepath.Join(running.config.StateDir, "identity", "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Error("the key file is empty")
	}

	tokens, err := h.app.FindRecordsByFilter(schema.EnrollmentTokens, "name='nas-02'", "", 0, 0)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("%d tokens, %v", len(tokens), err)
	}
	if tokens[0].GetDateTime("used_at").IsZero() || tokens[0].GetString("host") != host.Id {
		t.Errorf("the token was not consumed: %v", tokens[0].FieldsData())
	}

	// the checklist's last step: the inventory arrives
	h.waitFor("the inventory", func() bool {
		return !h.host(host.Id).GetDateTime("inventory_at").IsZero()
	})

	// facts and vitals from the first heartbeat
	h.waitFor("the heartbeat", func() bool {
		fresh := h.host(host.Id)
		return fresh.GetString("docker_version") == "27.3.1" && !fresh.GetDateTime("last_seen").IsZero()
	})

	events, err := h.app.FindRecordsByFilter(schema.AuditLog, "action={:a}", "", 0, 0, dbx.Params{"a": audit.ActionEnrollment})
	if err != nil || len(events) != 1 {
		t.Fatalf("the enrollment was not audited: %d, %v", len(events), err)
	}
}

func TestBadTokensFailWithAHelpfulMessage(t *testing.T) {
	h := newHarness(t)

	enroll := func(config agent.Config) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := agent.Enroll(ctx, config, nil)
		return err
	}

	t.Run("unknown", func(t *testing.T) {
		err := enroll(h.agentConfig("unknown", "ENR-NOT-A-REAL-TOKEN"))
		if status.Code(err) != codes.Unauthenticated || !strings.Contains(err.Error(), "not valid") {
			t.Fatalf("%v", err)
		}
	})

	t.Run("reused", func(t *testing.T) {
		token := h.token("reused", schema.HostRoleDocker)
		if err := enroll(h.agentConfig("reused", token)); err != nil {
			t.Fatal(err)
		}
		err := enroll(h.agentConfig("reused-again", token))
		if status.Code(err) != codes.Unauthenticated || !strings.Contains(err.Error(), "already been used") {
			t.Fatalf("%v", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		token := h.token("expired", schema.HostRoleDocker)

		records, _ := h.app.FindRecordsByFilter(schema.EnrollmentTokens, "name='expired'", "", 1, 0)
		records[0].Set("expires_at", time.Now().Add(-time.Minute).UTC().Format("2006-01-02 15:04:05.000Z"))
		if err := h.app.Save(records[0]); err != nil {
			t.Fatal(err)
		}

		err := enroll(h.agentConfig("expired", token))
		if status.Code(err) != codes.Unauthenticated || !strings.Contains(err.Error(), "expired") {
			t.Fatalf("%v", err)
		}
	})

	t.Run("name taken", func(t *testing.T) {
		if err := enroll(h.agentConfig("taken", h.token("taken"))); err != nil {
			t.Fatal(err)
		}
		err := enroll(h.agentConfig("taken", h.token("taken")))
		if status.Code(err) != codes.AlreadyExists {
			t.Fatalf("%v", err)
		}
	})

	failures, err := h.app.FindRecordsByFilter(schema.AuditLog, "action={:a}", "", 0, 0, dbx.Params{"a": audit.ActionEnrollmentFailed})
	if err != nil || len(failures) < 3 {
		t.Fatalf("failed enrollments must be audited: %d, %v", len(failures), err)
	}
}

// TestWrongPinIsRefused means that the first connection cannot be intercepted.
func TestWrongPinIsRefused(t *testing.T) {
	h := newHarness(t)

	config := h.agentConfig("pinned", h.token("pinned"))
	config.CAPin = "sha256:" + strings.Repeat("00", 32)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := agent.Enroll(ctx, config, nil)
	if err == nil || !strings.Contains(err.Error(), "CA_PIN") {
		t.Fatalf("expected a pin failure, got %v", err)
	}

	if hosts, _ := h.app.FindRecordsByFilter(schema.Hosts, "name='pinned'", "", 0, 0); len(hosts) != 0 {
		t.Fatal("a host was created over an unverified connection")
	}
}

func TestEnrollIsRateLimited(t *testing.T) {
	h := newHarness(t)

	limited := false
	for range 15 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := agent.Enroll(ctx, h.agentConfig("limited", "ENR-WRONG"), nil)
		cancel()
		if status.Code(err) == codes.ResourceExhausted {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("repeated attempts were not limited")
	}
}

// anonymousClient connects with a verified panel but without a client certificate.
func (h *harness) anonymousClient() agentv1.AgentServiceClient {
	h.t.Helper()

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(h.panel.Authority.RootPEM()) {
		h.t.Fatal("bad root")
	}

	conn, err := grpc.NewClient(h.panel.Gateway.Addr(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, //nolint:gosec // VerifyConnection does the verification
		VerifyConnection:   func(state tls.ConnectionState) error { return pki.VerifyPanel(state, roots) },
	})))
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { conn.Close() })

	return agentv1.NewAgentServiceClient(conn)
}

func TestOnlyEnrollWorksWithoutAClientCertificate(t *testing.T) {
	h := newHarness(t)
	client := h.anonymousClient()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	unary := map[string]func() error{
		"Heartbeat": func() error { _, err := client.Heartbeat(ctx, &agentv1.HeartbeatRequest{}); return err },
		"ReportCommandUpdate": func() error {
			_, err := client.ReportCommandUpdate(ctx, &agentv1.ReportCommandUpdateRequest{})
			return err
		},
		"ReportInventory":  func() error { _, err := client.ReportInventory(ctx, &agentv1.ReportInventoryRequest{}); return err },
		"RenewCertificate": func() error { _, err := client.RenewCertificate(ctx, &agentv1.RenewCertificateRequest{}); return err },
		"SubscribeCommands": func() error {
			stream, err := client.SubscribeCommands(ctx, &agentv1.SubscribeCommandsRequest{})
			if err != nil {
				return err
			}
			_, err = stream.Recv()
			return err
		},
		"FetchArtifact": func() error {
			stream, err := client.FetchArtifact(ctx, &agentv1.FetchArtifactRequest{})
			if err != nil {
				return err
			}
			_, err = stream.Recv()
			return err
		},
		"PushMetrics": func() error {
			stream, err := client.PushMetrics(ctx)
			if err != nil {
				return err
			}
			_, err = stream.CloseAndRecv()
			return err
		},
		"Exec": func() error {
			stream, err := client.Exec(ctx)
			if err != nil {
				return err
			}
			_, err = stream.Recv()
			return err
		},
	}

	for name, call := range unary {
		if err := call(); status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s without a certificate: %v", name, err)
		}
	}

	// Enroll answers; the token is what is wrong
	_, err := client.Enroll(ctx, &agentv1.EnrollRequest{Token: "x"})
	if status.Code(err) != codes.Unauthenticated || !strings.Contains(err.Error(), "token") {
		t.Errorf("Enroll: %v", err)
	}
}

func TestRevokedCertificateIsRejected(t *testing.T) {
	h := newHarness(t)

	running, host := h.enrolled("revoked")

	if err := gateway.Revoke(h.app, host.GetString("cert_serial"), host.Id, "revoked", "test"); err != nil {
		t.Fatal(err)
	}

	// the open stream's next call fails: every RPC checks the denylist
	h.waitFor("the agent to lose its connection", func() bool {
		return agent.Healthy(running.config) != nil
	})

	h.waitFor("the rejection event", func() bool {
		events, err := h.app.FindRecordsByFilter(schema.Events, "kind='agent.rejected' && host={:host}", "", 0, 0, dbx.Params{"host": host.Id})
		return err == nil && len(events) > 0
	})

	// the security event is written right after the event, in a transaction of its own
	h.waitFor("the rejection to be a security event", func() bool {
		rejected, err := h.app.FindRecordsByFilter(schema.AuditLog, "action={:a}", "", 0, 0, dbx.Params{"a": audit.ActionCertRejected})
		return err == nil && len(rejected) > 0
	})

	identity, err := agent.LoadIdentity(running.config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	_, callErr := dialWith(t, h, identity).Heartbeat(context.Background(), &agentv1.HeartbeatRequest{})
	if status.Code(callErr) != codes.PermissionDenied || !strings.Contains(callErr.Error(), "revoked") {
		t.Fatalf("%v", callErr)
	}
}

func dialWith(t *testing.T, h *harness, identity *agent.Identity) agentv1.AgentServiceClient {
	t.Helper()

	conn, err := grpc.NewClient(h.panel.Gateway.Addr(), grpc.WithTransportCredentials(credentials.NewTLS(identity.TLSConfig())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	return agentv1.NewAgentServiceClient(conn)
}

func TestRemovingAHostRevokesItsCertificate(t *testing.T) {
	h := newHarness(t)

	running, host := h.enrolled("removed")
	serial := host.GetString("cert_serial")

	if err := h.app.Delete(h.host(host.Id)); err != nil {
		t.Fatal(err)
	}

	if _, err := h.app.FindFirstRecordByData(schema.RevokedCerts, "serial", serial); err != nil {
		t.Fatalf("the certificate was not revoked: %v", err)
	}

	h.waitFor("the agent to lose its connection", func() bool {
		return agent.Healthy(running.config) != nil
	})
}

func TestExpiredCertificateIsRejected(t *testing.T) {
	h := newHarness(t, func(server *gateway.Server) { server.Authority.AgentTTL = 2 * time.Second })

	config := h.agentConfig("expiring", h.token("expiring"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	identity, err := agent.Enroll(ctx, config, nil)
	if err != nil {
		t.Fatal(err)
	}

	client := dialWith(t, h, identity)

	if _, err := client.Heartbeat(ctx, &agentv1.HeartbeatRequest{}); err != nil {
		t.Fatalf("a valid certificate must work: %v", err)
	}

	time.Sleep(time.Until(identity.Certificate().NotAfter) + 100*time.Millisecond)

	_, err = client.Heartbeat(ctx, &agentv1.HeartbeatRequest{})
	if status.Code(err) != codes.Unauthenticated || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("%v", err)
	}

	h.waitFor("the rejection event", func() bool {
		events, err := h.app.FindRecordsByFilter(schema.Events, "kind='agent.rejected'", "", 0, 0)
		return err == nil && len(events) > 0
	})
}

// TestCertificateRenewal runs an agent well past the life of its first certificate.
func TestCertificateRenewal(t *testing.T) {
	h := newHarness(t, func(server *gateway.Server) { server.Authority.AgentTTL = 3 * time.Second })

	running, host := h.enrolled("renewing")

	first := host.GetString("cert_serial")

	h.waitFor("the certificate to be renewed", func() bool {
		return h.host(host.Id).GetString("cert_serial") != first
	})

	// the agent reconnected with the new certificate, which retires the old one
	h.waitFor("the old certificate to be retired", func() bool {
		_, err := h.app.FindFirstRecordByData(schema.RevokedCerts, "serial", first)
		return err == nil
	})

	time.Sleep(3500 * time.Millisecond)

	// past the first certificate's expiry, and still connected and reporting
	if err := agent.Healthy(running.config); err != nil {
		t.Fatalf("the agent is not healthy: %v", err)
	}
	seen := h.host(host.Id).GetDateTime("last_seen").Time()
	if time.Since(seen) > 2*time.Second {
		t.Fatalf("the last heartbeat is old: %s", seen)
	}

	if identity, err := agent.LoadIdentity(running.config.StateDir); err != nil {
		t.Fatal(err)
	} else if identity.Certificate().NotAfter.Before(time.Now()) {
		t.Fatal("the stored certificate has expired")
	}
}

// TestLostRenewalResponseDoesNotLockTheAgentOut: the old certificate stays valid
// until the agent actually uses the new one.
func TestOldCertificateWorksUntilTheNewOneIsUsed(t *testing.T) {
	h := newHarness(t)

	config := h.agentConfig("lost-response", h.token("lost-response"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	identity, err := agent.Enroll(ctx, config, nil)
	if err != nil {
		t.Fatal(err)
	}

	client := dialWith(t, h, identity)

	// the panel issues a new certificate, the agent never gets it
	_, csr, err := pki.NewKeyAndCSR("x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RenewCertificate(ctx, &agentv1.RenewCertificateRequest{CsrPem: csr}); err != nil {
		t.Fatal(err)
	}

	if _, err := client.Heartbeat(ctx, &agentv1.HeartbeatRequest{}); err != nil {
		t.Fatalf("the old certificate must still work: %v", err)
	}

	// it can simply try again
	if err := identity.Renew(ctx, client); err != nil {
		t.Fatalf("renewing again: %v", err)
	}
}

func TestRecreatingTheContainerKeepsTheIdentity(t *testing.T) {
	h := newHarness(t)

	running, host := h.enrolled("recreated")
	if err := running.stop(); err != nil {
		t.Fatal(err)
	}

	// a new container on the same volume, with the used token still in its environment
	second := h.start(running.config)

	h.waitFor("the agent to reconnect", func() bool {
		return agent.Healthy(second.config) == nil
	})

	hosts, err := h.app.FindRecordsByFilter(schema.Hosts, "name='recreated'", "", 0, 0)
	if err != nil || len(hosts) != 1 || hosts[0].Id != host.Id {
		t.Fatalf("the identity changed: %d hosts, %v", len(hosts), err)
	}
}

func TestAgentReconnectsAfterAPanelRestart(t *testing.T) {
	h := newHarness(t)

	running, host := h.enrolled("reconnecting")

	addr := h.panel.Gateway.Addr()
	first := h.panel.Gateway

	h.waitFor("a healthy agent", func() bool { return agent.Healthy(running.config) == nil })

	first.Stop()

	h.waitFor("the agent to notice", func() bool { return agent.Healthy(running.config) != nil })

	second := &gateway.Server{
		App:       h.app,
		Authority: h.panel.Authority,
		Commands:  h.panel.Commands,
		Audit:     h.panel.Audit,
		Inventory: &inventory.Store{App: h.app},
	}
	if err := second.Start(addr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Stop)

	h.waitFor("the agent to reconnect", func() bool {
		return agent.Healthy(running.config) == nil && h.host(host.Id).GetBool("stream_open")
	})
}

func TestHealthOnlyAfterTheConnection(t *testing.T) {
	h := newHarness(t)

	config := h.agentConfig("health", h.token("health"))

	if err := agent.Healthy(config); err == nil {
		t.Fatal("healthy before it has ever connected")
	}

	running := h.start(config)

	h.waitFor("healthy", func() bool { return agent.Healthy(config) == nil })

	if err := running.stop(); err != nil {
		t.Fatal(err)
	}

	if err := agent.Healthy(config); err == nil {
		t.Fatal("healthy after the agent stopped")
	}
}

// TestStoppingAnAgentMarksTheHostOffline covers the offline detection of #11.
func TestStoppingAnAgentMarksTheHostOffline(t *testing.T) {
	h := newHarness(t, func(server *gateway.Server) { server.OfflineAfter = 300 * time.Millisecond })

	running, host := h.enrolled("offline")

	if err := running.stop(); err != nil {
		t.Fatal(err)
	}

	h.waitFor("the host to be marked offline", func() bool {
		h.panel.Gateway.CheckOffline()
		return h.host(host.Id).GetString("status") == schema.HostOffline
	})

	alerts, err := h.app.FindRecordsByFilter(schema.Alerts, "rule='agent_offline' && target={:t} && status='open'", "", 0, 0, dbx.Params{"t": host.Id})
	if err != nil || len(alerts) != 1 {
		t.Fatalf("expected one open alert: %d, %v", len(alerts), err)
	}

	// silence does not raise a second alert
	h.panel.Gateway.CheckOffline()
	alerts, _ = h.app.FindRecordsByFilter(schema.Alerts, "rule='agent_offline' && target={:t}", "", 0, 0, dbx.Params{"t": host.Id})
	if len(alerts) != 1 {
		t.Fatalf("the alert was repeated: %d", len(alerts))
	}

	events, err := h.app.FindRecordsByFilter(schema.Events, "kind='agent.offline' && host={:h}", "", 0, 0, dbx.Params{"h": host.Id})
	if err != nil || len(events) != 1 {
		t.Fatalf("expected one offline event: %d, %v", len(events), err)
	}

	// it comes back: the host recovers, and the alert resolves
	second := h.start(running.config)
	h.waitFor("the agent to reconnect", func() bool { return agent.Healthy(second.config) == nil })

	h.waitFor("the alert to resolve", func() bool {
		open, err := h.app.FindRecordsByFilter(schema.Alerts, "rule='agent_offline' && target={:t} && status='open'", "", 0, 0, dbx.Params{"t": host.Id})
		return err == nil && len(open) == 0
	})

	if status := h.host(host.Id).GetString("status"); status == schema.HostOffline {
		t.Fatalf("status after coming back: %s", status)
	}
}

// TestHeartbeatsDoNotUndoAnAdminsEdit: a heartbeat saves the whole host record, so
// it must never write back the old version of what someone has just changed.
func TestHeartbeatsDoNotUndoAnAdminsEdit(t *testing.T) {
	h := newHarness(t)

	_, adminToken := testutil.NewUser(t, h.app, "admin@example.com", schema.RoleAdmin)

	config := h.agentConfig("busy", h.token("busy", schema.HostRoleDocker))
	config.HeartbeatInterval = 2 * time.Millisecond

	h.start(config)
	h.waitFor("the agent to connect", func() bool { return agent.Healthy(config) == nil })

	host, err := h.app.FindFirstRecordByData(schema.Hosts, "name", "busy")
	if err != nil {
		t.Fatal(err)
	}

	url := "/api/collections/hosts/records/" + host.Id

	for i := range 60 {
		value := fmt.Sprintf(`{"attempt":"%d"}`, i)

		status, body := testutil.Do(t, h.handler, testutil.Request{Method: "PATCH", URL: url, Token: adminToken, Body: `{"labels":` + value + `}`})
		if status != http.StatusOK {
			t.Fatalf("%d %s", status, body)
		}

		time.Sleep(5 * time.Millisecond)

		labels := strings.ReplaceAll(h.host(host.Id).GetString("labels"), " ", "")
		if labels != value {
			t.Fatalf("attempt %d: the edit was undone, the labels are %s", i, labels)
		}
	}
}

func TestHeartbeatStoresRTTAndClockSkew(t *testing.T) {
	h := newHarness(t)

	_, host := h.enrolled("vitals")

	h.waitFor("the RTT to be reported", func() bool {
		return h.host(host.Id).GetFloat("rtt_ms") > 0
	})

	if skew := h.host(host.Id).GetFloat("clock_skew_ms"); skew > 1000 || skew < -1000 {
		t.Errorf("the skew on one machine must be small: %f", skew)
	}

	runtime := h.host(host.Id).GetString("runtime")
	if !strings.Contains(runtime, "goroutines") {
		t.Errorf("the runtime stats are missing: %s", runtime)
	}
}

func TestSelfChecksAreReportedAsTransitions(t *testing.T) {
	h := newHarness(t)

	// the temporary host root has no /proc: this check fails right away
	running, host := h.enrolled("self-checks")

	h.waitFor("the failed self-check event", func() bool {
		events, err := h.app.FindRecordsByFilter(schema.Events, "kind='agent.self_check_failed' && host={:h}", "", 0, 0, dbx.Params{"h": host.Id})
		return err == nil && len(events) > 0
	})

	h.waitFor("a degraded host", func() bool {
		return h.host(host.Id).GetString("status") == schema.HostDegraded
	})

	checks := h.host(host.Id).GetString("self_checks")
	for _, name := range []string{"docker", "host_mount", "edge", "restic", "time"} {
		if !strings.Contains(checks, `"`+name+`"`) {
			t.Errorf("the check %s is missing: %s", name, checks)
		}
	}

	// heartbeats do not repeat an unchanged failure
	first, _ := h.app.FindRecordsByFilter(schema.Events, "kind='agent.self_check_failed' && host={:h}", "", 0, 0, dbx.Params{"h": host.Id})
	time.Sleep(400 * time.Millisecond)
	second, _ := h.app.FindRecordsByFilter(schema.Events, "kind='agent.self_check_failed' && host={:h}", "", 0, 0, dbx.Params{"h": host.Id})
	if len(first) != len(second) {
		t.Errorf("the failure was repeated: %d → %d", len(first), len(second))
	}

	_ = running
}
