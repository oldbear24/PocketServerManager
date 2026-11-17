package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/agent"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/gateway"
	"github.com/oldbear24/PocketServerManager/internal/panel"
	"github.com/oldbear24/PocketServerManager/internal/panelbackup"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakefetch"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakerestic"
)

// The panel's backup of itself and its recovery on a new machine (#32), end
// to end: a panel whose carrier node-1 backs it up to a fake rest-server
// with the fake restic, a recovery kit downloaded and checked, then the
// panel's machine lost: `pocketcloud-agent panel-recovery fetch` takes the
// copy out of the repository in node-1's job container, `panel recover`
// puts pb_data and the master key in place, and a second panel starts there
// at the address the agents dial.

// kitPassphrase seals the tests' kits: 12 characters at least, and not the
// sign-in password.
const kitPassphrase = "Příliš žluťoučký kůň úpěl ďábelské ódy"

// enrollMethod is the gRPC method an agent enrolls with.
const enrollMethod = "/pocketcloud.agent.v1.AgentService/Enroll"

// panelMachine is a panel on a machine of its own: its harness, with the
// carrier node-1, and a stop that ends it once, as SIGTERM does, and removes
// its data dir.
type panelMachine struct {
	*jobsHarness
	stop func()
}

// recovery is a panel whose own backup job its carrier node-1 runs to a fake
// rest-server, the machine it will be restored on, and the kit.
type recovery struct {
	t *testing.T
	// a is the panel that is lost
	a *panelMachine
	// addr is the address the agents dial, which the second panel takes over
	addr string
	// engine is node-1's, which runs its backup jobs and the recovery's
	engine  *fakedocker.Engine
	restic  string
	rest    *core.Record
	job     string
	kitFile string
	// options change the gateways before they start
	options []func(*gateway.Server)
}

// recoverySetup changes how newRecovery starts the first panel: its
// gateway's options, node-1's agent before it starts, and the panel before
// node-1 enrolls.
type recoverySetup struct {
	options []func(*gateway.Server)
	prepare []func(*agent.Agent)
	panel   func(*panel.Panel)
}

// freeAddr is a free port of the loopback, which the first panel listens on
// and the second takes over.
func freeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// startPanel starts a panel on app; its agents' jobs run on engine. The
// options change its gateway before it starts. stop ends it once.
func startPanel(t *testing.T, app *tests.TestApp, config *panel.Config, engine *fakedocker.Engine, options ...func(*gateway.Server)) (*harness, func()) {
	t.Helper()
	p := panel.New(app, config, fstest.MapFS{"index.html": {Data: []byte("ui")}})
	p.Observers = append(p.Observers, options...)
	p.Register()
	var once sync.Once
	stop := func() { once.Do(app.Cleanup) }
	t.Cleanup(stop)
	return &harness{t: t, app: app, panel: p, handler: testutil.Handler(t, app), docker: engine}, stop
}

// machineOn makes a harness of a running panel whose carrier is node-1:
// what its backup runs, and their finishers, are kept for the tests.
func machineOn(h *harness, stop func(), restic string, carrier *runningAgent) *panelMachine {
	b := &backupHarness{harness: h, restic: restic, agent: carrier}
	b.panel.Commands.OnFinish("backup.init", func(command *core.Record) { b.finished.Store(command.Id, true) })
	b.panel.Commands.OnFinish("backup.run", func(command *core.Record) { b.finished.Store(command.Id, true) })
	return &panelMachine{jobsHarness: &jobsHarness{backupHarness: b}, stop: stop}
}

// newRecovery starts the first panel at a fixed address, its carrier node-1
// with a job engine and the fake restic, a fake rest-server target on
// node-1 and the panel's backup job to it.
func newRecovery(t *testing.T, setup recoverySetup) *recovery {
	t.Helper()
	r := &recovery{t: t, addr: freeAddr(t), options: setup.options}
	r.restic = fakerestic.Install(t)
	r.engine = fakedocker.New(t)
	r.engine.AddImage("sha256:"+strings.Repeat("a", 64), nil)
	r.engine.AddContainer("pocketcloud-agent", "ghcr.io/oldbear24/pocketcloud-agent:dev", map[string]string{"pocketcloud.role": "agent"})
	r.engine.VolumeRoot = t.TempDir()
	fakerestic.RunJobsWith(r.engine, agent.BackupTool, fakerestic.Options{})

	h, stop := startPanel(t, testutil.NewApp(t), &panel.Config{MasterKey: testutil.MasterKey, GRPCAddr: r.addr, PublicURL: "http://panel.test"}, r.engine, setup.options...)
	r.a = machineOn(h, stop, r.restic, nil)
	if setup.panel != nil {
		setup.panel(h.panel)
	}
	run := r.engine.OnJob
	r.engine.OnJob = func(job fakedocker.Job) fakedocker.JobResult {
		r.a.mu.Lock()
		r.a.jobs = append(r.a.jobs, job)
		r.a.mu.Unlock()
		return run(job)
	}

	config := h.agentConfig("node-1", h.token("node-1", schema.HostRoleDocker))
	config.HostRoot = fakeHostRoot(t)
	r.a.agent = h.start(config, append([]func(*agent.Agent){quickRestores}, setup.prepare...)...)
	h.waitFor("node-1 to connect", func() bool {
		found, err := h.app.FindFirstRecordByData(schema.Hosts, "name", "node-1")
		if err != nil || !found.GetBool("stream_open") {
			return false
		}
		r.a.host = found
		return true
	})
	admin, token := testutil.NewUser(t, h.app, "admin@example.com", schema.RoleAdmin)
	r.a.admin, r.a.adminID = token, admin.Id

	status, out := r.a.send(http.MethodPost, "/api/pc/backup-targets", r.a.admin, map[string]any{
		"name": "panel-rest", "kind": "rest", "host": r.a.host.Id, "config": map[string]any{"url": "https://backup-node:8000/homelab/panel32/"},
		"credentials": map[string]any{"username": "homelab", "password": "rest-server-password"},
	})
	if status != http.StatusCreated {
		t.Fatalf("the rest target: %d %v", status, out)
	}
	r.a.checksDone()
	r.rest = r.a.targetNamed("panel-rest")

	status, out = r.a.send(http.MethodPost, "/api/pc/backup-jobs", r.a.admin, map[string]any{
		"name": "panel (pb_data)", "source_kind": schema.SourcePanel, "host": r.a.host.Id, "targets": []string{r.rest.Id},
		"schedule": neverDue, "retention": map[string]any{"daily": 14, "weekly": 8, "monthly": 12},
	})
	if status != http.StatusCreated {
		t.Fatalf("the panel's job: %d %v", status, out)
	}
	r.job = out["id"].(string)
	return r
}

// downloadKit downloads a kit as the admin, with the sign-in password, saves
// it and checks it: the panel then counts it saved.
func (r *recovery) downloadKit() {
	r.t.Helper()
	status, out := r.a.send(http.MethodPost, "/api/pc/recovery/kit", r.a.admin, map[string]any{
		"passphrase": kitPassphrase, "password": "correct horse battery staple",
	})
	if status != http.StatusOK {
		r.t.Fatalf("the kit: %d %v", status, out)
	}
	content, _ := out["content"].(string)
	r.kitFile = filepath.Join(r.t.TempDir(), out["file_name"].(string))
	if err := os.WriteFile(r.kitFile, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
	status, out = r.a.send(http.MethodPost, "/api/pc/recovery/kit/check", r.a.admin, map[string]any{"kit": content, "passphrase": kitPassphrase})
	if status != http.StatusOK || out["current"] != true || out["status"] != "saved" {
		r.t.Fatalf("the kit's check: %d %v", status, out)
	}
}

// runPanelJob runs the panel's job on a panel and waits until its run ended.
func (r *recovery) runPanelJob(m *panelMachine) *core.Record {
	r.t.Helper()
	runs := m.runNow(r.job)
	if len(runs) != 1 {
		r.t.Fatalf("%d runs", len(runs))
	}
	return runs[0]
}

// copies are the panel's copies in the rest-server's repository, oldest
// first, with their snapshots.
func (r *recovery) copies() ([][]byte, []fakerestic.Snapshot) {
	r.t.Helper()
	repos, err := os.ReadDir(filepath.Join(r.restic, "repos"))
	if err != nil {
		r.t.Fatal(err)
	}
	var found [][]byte
	var snapshots []fakerestic.Snapshot
	for _, repo := range repos {
		for _, snapshot := range fakerestic.Snapshots(r.t, r.restic, filepath.Join(r.restic, "repos", repo.Name())) {
			if slices.Contains(snapshot.Tags, "job:"+r.job) {
				found = append(found, []byte(snapshot.Files["/"+backup.PanelFileName]))
				snapshots = append(snapshots, snapshot)
			}
		}
	}
	return found, snapshots
}

// newestCopy is the panel's newest copy in the repository.
func (r *recovery) newestCopy() []byte {
	r.t.Helper()
	found, _ := r.copies()
	if len(found) == 0 {
		r.t.Fatal("the repository holds no copy of the panel")
	}
	return found[len(found)-1]
}

// restored is the second panel, on the new machine, and what its restore
// said.
type restored struct {
	*panelMachine
	started time.Time
	summary string
	fetched string
}

// moveToNewMachine loses the first panel's machine: the panel stops, node-1
// fetches its newest copy out of the repository with the kit, `panel
// recover` restores it into a new directory, and the second panel starts
// there with the kit's key, at the address the agents dial.
func (r *recovery) moveToNewMachine() *restored {
	r.t.Helper()
	r.a.stop()

	out := filepath.Join(r.t.TempDir(), backup.PanelFileName)
	fetcher, err := agent.NewRecovery(r.a.agent.config, io.Discard)
	if err != nil {
		r.t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	env := func(name string) string {
		if name == "POCKETCLOUD_KIT_PASSPHRASE" {
			return kitPassphrase
		}
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := fetcher.PanelRecovery(ctx, []string{"fetch", "--kit", r.kitFile, "--target", "panel-rest", "--out", out},
		agent.RecoveryIO{Stdout: &stdout, Stderr: &stderr, Getenv: env}); err != nil {
		r.t.Fatalf("panel-recovery fetch: %v\n%s", err, stderr.String())
	}

	machine := r.t.TempDir()
	dir := filepath.Join(machine, "pb_data")
	keyFile := filepath.Join(machine, "pocketcloud-master-key")
	var summary bytes.Buffer
	if err := panel.Recover(panel.RecoverOptions{Kit: r.kitFile, From: out, Dir: dir, MasterKeyFile: keyFile, Passphrase: kitPassphrase,
		EnvOut: filepath.Join(machine, "panel.env"), Out: &summary}); err != nil {
		r.t.Fatalf("panel recover: %v\n%s", err, summary.String())
	}

	started := time.Now()
	h, stop := startPanel(r.t, testutil.NewAppFrom(r.t, dir), &panel.Config{MasterKeyFile: keyFile, GRPCAddr: r.addr, PublicURL: "http://panel.test"},
		r.engine, r.options...)
	b := machineOn(h, stop, r.restic, r.a.agent)
	b.host = h.host(r.a.host.Id)
	// the sessions from before the disaster ended: the admin signs in again
	admin, err := h.app.FindAuthRecordByEmail(schema.Users, "admin@example.com")
	if err != nil {
		r.t.Fatal(err)
	}
	token, err := admin.NewAuthToken()
	if err != nil {
		r.t.Fatal(err)
	}
	b.admin, b.adminID = token, admin.Id
	return &restored{panelMachine: b, started: started, summary: summary.String(), fetched: stderr.String()}
}

// enrollCounter counts the enrollments a gateway answers.
type enrollCounter struct{ n atomic.Int32 }

func (c *enrollCounter) option(s *gateway.Server) {
	s.UnaryInterceptors = append(s.UnaryInterceptors, func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info.FullMethod == enrollMethod {
			c.n.Add(1)
		}
		return handler(ctx, req)
	})
}

// identityOf is the digest of each file of an agent's identity.
func identityOf(t *testing.T, stateDir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	root := filepath.Join(stateDir, "identity")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, err := os.ReadFile(path) //nolint:gosec // the test's own state dir
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		files[strings.TrimPrefix(path, root)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil || len(files) < 4 {
		t.Fatalf("the identity in %s: %v, %v", stateDir, files, err)
	}
	return files
}

// clientAs is a gRPC client of the panel at addr with an agent's identity,
// as the agent dials it.
func clientAs(t *testing.T, addr, stateDir string) agentv1.AgentServiceClient {
	t.Helper()
	identity, err := agent.LoadIdentity(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(identity.TLSConfig())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return agentv1.NewAgentServiceClient(conn)
}

// fetchFirst asks for an artifact and reads its first chunk.
func fetchFirst(client agentv1.AgentServiceClient, artifact string) (*agentv1.FetchArtifactResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.FetchArtifact(ctx, &agentv1.FetchArtifactRequest{ArtifactId: artifact})
	if err != nil {
		return nil, err
	}
	return stream.Recv()
}

// alertsOf are the alerts of a rule, whatever their status.
func (m *panelMachine) alertsOf(rule string) []*core.Record {
	m.t.Helper()
	alerts, err := m.app.FindRecordsByFilter(schema.Alerts, "rule={:rule}", "created", 0, 0, dbx.Params{"rule": rule})
	if err != nil {
		m.t.Fatal(err)
	}
	return alerts
}

// TestThePanelComesBackOnANewMachineAndKeepsItsAgents (#32, the AC): the
// panel's machine is lost after its daily backup, and a panel restored from
// that backup and the kit on a new machine at the same address gets its
// servers back: the same CA, both agents reconnected with their own
// identities and no enrollment, the audit log intact with the sealed
// panel_restored entry newest, the restore's marker gone, the queued command
// cancelled, panel_restored resolved and no server alerted offline; the run
// that made the backup ends as it did, the panel's job runs on the new
// machine, and the kit checked before the disaster is still saved.
func TestThePanelComesBackOnANewMachineAndKeepsItsAgents(t *testing.T) {
	before, after := &enrollCounter{}, &enrollCounter{}
	r := newRecovery(t, recoverySetup{options: []func(*gateway.Server){before.option}})
	second := r.a.enrolledOn("node-2", nil)
	if before.n.Load() != 2 {
		t.Fatalf("%d enrollments", before.n.Load())
	}
	r.downloadKit()

	// node-2 is down, and a command for it waits in the queue
	if err := second.agent.stop(); err != nil {
		t.Fatal(err)
	}
	r.a.waitFor("node-2 to be offline", func() bool { return !r.a.harness.host(second.host.Id).GetBool("stream_open") })
	queued := r.a.create(second.host.Id, pull("nginx:1.27"))
	if queued.GetString("status") != schema.CommandQueued {
		t.Fatalf("the command is %s", queued.GetString("status"))
	}

	// the last backup before the disaster: encrypted on the panel
	backedUp := r.runPanelJob(r.a)
	if backedUp.GetString("status") != schema.RunSucceeded {
		t.Fatalf("the backup: %v", backedUp.FieldsData())
	}
	stored := r.newestCopy()
	if !bytes.HasPrefix(stored, []byte("PCPB")) || bytes.Contains(stored, []byte("SQLite format 3")) || bytes.Contains(stored, []byte("pcs1:")) {
		t.Fatalf("the repository holds a copy of %d bytes that is not sealed: %q", len(stored), stored[:min(len(stored), 64)])
	}
	if _, snapshots := r.copies(); snapshots[0].Hostname != backup.PanelHostName || !slices.Contains(snapshots[0].Tags, backup.PanelTag) {
		t.Errorf("the snapshot: %+v", snapshots[0])
	}

	pin := r.a.panel.Authority.Pin()
	serials := map[string]string{}
	for _, id := range []string{r.a.host.Id, second.host.Id} {
		serials[id] = r.a.harness.host(id).GetString("cert_serial")
	}
	identities := map[string]map[string]string{
		r.a.host.Id:    identityOf(t, r.a.agent.config.StateDir),
		second.host.Id: identityOf(t, second.agent.config.StateDir),
	}

	r.options = []func(*gateway.Server){after.option}
	b := r.moveToNewMachine()
	if !strings.Contains(b.fetched, "every part opens with the kit's key") || !strings.Contains(b.summary, "Checked it: every part opens with the kit's key") {
		t.Errorf("the fetch said:\n%s\nthe restore said:\n%s", b.fetched, b.summary)
	}
	if b.panel.Authority.Pin() != pin {
		t.Fatalf("the new panel's CA is %s, the lost one's %s", b.panel.Authority.Pin(), pin)
	}
	// node-2 comes back too
	b.start(second.agent.config)
	b.waitFor("both servers to be back", func() bool {
		return b.harness.host(r.a.host.Id).GetBool("stream_open") && b.harness.host(second.host.Id).GetBool("stream_open")
	})
	if after.n.Load() != 0 {
		t.Errorf("%d agents enrolled again", after.n.Load())
	}
	for id, serial := range serials {
		if got := b.harness.host(id).GetString("cert_serial"); got != serial {
			t.Errorf("the serial of %s is %s, was %s", id, got, serial)
		}
		if got := b.harness.host(id).GetString("panel_address"); got != r.addr {
			t.Errorf("%s dials %s", id, got)
		}
	}
	for id, stateDir := range map[string]string{r.a.host.Id: r.a.agent.config.StateDir, second.host.Id: second.agent.config.StateDir} {
		if got := identityOf(t, stateDir); fmt.Sprint(got) != fmt.Sprint(identities[id]) {
			t.Errorf("the identity of %s changed: %v, was %v", id, got, identities[id])
		}
	}

	// the audit log is intact, and the restore its newest security entry
	verified, err := audit.Verify(b.app, b.panel.AuditKey())
	if err != nil || !verified.OK || !verified.Sealed {
		t.Errorf("the audit log: %+v, %v", verified, err)
	}
	entries, err := b.app.FindRecordsByFilter(schema.AuditLog, "category = 'security'", "-seq", 1, 0)
	if err != nil || len(entries) != 1 || entries[0].GetString("action") != audit.ActionPanelRestored {
		t.Errorf("the newest security entry: %v, %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(b.app.DataDir(), ".pc_restored.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the restore's marker: %v", err)
	}
	if cancelled := b.command(queued.Id); cancelled.GetString("status") != schema.CommandCancelled ||
		!strings.HasPrefix(cancelled.GetString("error"), "The panel was restored from a backup made ") {
		t.Errorf("the queued command: %v", cancelled.FieldsData())
	}
	b.waitFor("panel_restored to resolve", func() bool {
		alerts := b.alertsOf(backup.AlertPanelRestored)
		return len(alerts) == 1 && alerts[0].GetString("status") == schema.AlertResolved
	})
	for _, alert := range b.alertsOf("agent_offline") {
		if alert.GetDateTime("created").Time().After(b.started) {
			t.Errorf("a server was alerted offline: %v", alert.FieldsData())
		}
	}

	// the run that made the backup ends as its carrier says
	b.waitFor("the backup's run to end", func() bool {
		run, err := b.app.FindRecordById(schema.BackupRuns, backedUp.Id)
		return err == nil && run.GetString("status") == schema.RunSucceeded
	})
	// the panel backs itself up from the new machine, with the targets'
	// sealed secrets of the old one
	if run := r.runPanelJob(b.panelMachine); run.GetString("status") != schema.RunSucceeded {
		t.Fatalf("the panel's job on the new machine: %v", run.FieldsData())
	}
	if found, _ := r.copies(); len(found) != 2 {
		t.Errorf("%d copies", len(found))
	}

	// the old sessions ended; the kit checked before the disaster is saved
	if status, out := b.send(http.MethodGet, "/api/pc/recovery", r.a.admin, nil); status != http.StatusUnauthorized {
		t.Errorf("an old session: %d %v", status, out)
	}
	status, out := b.send(http.MethodGet, "/api/pc/recovery", b.admin, nil)
	kit, _ := out["kit"].(map[string]any)
	restore, _ := out["restored"].(map[string]any)
	if status != http.StatusOK || kit["status"] != "saved" || out["ca_pin"] != pin || restore["back"] != float64(2) || restore["total"] != float64(2) {
		t.Errorf("the recovery: %d %v", status, out)
	}
}

// TestAServerEnrolledAfterTheBackupMustEnrollAgain (#32): the restored panel
// does not know a server enrolled after its backup was made, and refuses it
// with the sentence that tells to enroll it again; the others come back.
func TestAServerEnrolledAfterTheBackupMustEnrollAgain(t *testing.T) {
	r := newRecovery(t, recoverySetup{})
	r.downloadKit()
	if run := r.runPanelJob(r.a); run.GetString("status") != schema.RunSucceeded {
		t.Fatalf("the backup: %v", run.FieldsData())
	}
	late := r.a.enrolledOn("node-late", nil)

	b := r.moveToNewMachine()
	b.waitFor("node-1 to be back", func() bool { return b.harness.host(r.a.host.Id).GetBool("stream_open") })
	if _, err := b.app.FindRecordById(schema.Hosts, late.host.Id); err == nil {
		t.Fatal("the restored panel knows the server enrolled after its backup")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := clientAs(t, r.addr, late.agent.config.StateDir).Heartbeat(ctx, &agentv1.HeartbeatRequest{})
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(status.Convert(err).Message(), "enroll it again") {
		t.Fatalf("the late server's heartbeat: %v", err)
	}
	if err := agent.Healthy(late.agent.config); err == nil {
		t.Error("the late server's agent says it is connected")
	}
}

// TestAnotherServerCannotFetchThePanelExport (#32): the panel's copy goes to
// its run's carrier alone, while the run goes: another server is refused,
// and so is the carrier once its run has ended.
func TestAnotherServerCannotFetchThePanelExport(t *testing.T) {
	r := newRecovery(t, recoverySetup{})
	second := r.a.enrolledOn("node-2", nil)
	entered, release := r.a.gateJobs(isBackupRun, nil)
	status, out := r.a.send(http.MethodPost, "/api/pc/backup-jobs/"+r.job+"/run", r.a.admin, nil)
	if status != http.StatusOK {
		t.Fatalf("run now: %d %v", status, out)
	}
	runID := out["runs"].([]any)[0].(string)
	select {
	case <-entered:
	case <-time.After(waitLimit):
		t.Fatal("the run's job did not start")
	}
	artifact := backup.PanelExportID(runID)
	carrier := clientAs(t, r.addr, r.a.agent.config.StateDir)

	if _, err := fetchFirst(clientAs(t, r.addr, second.agent.config.StateDir), artifact); grpcCode(err) != codes.PermissionDenied {
		t.Errorf("another server fetched the copy: %v", err)
	}
	first, err := fetchFirst(carrier, artifact)
	if err != nil || first.GetTotalBytes() == 0 || len(first.GetSha256()) != 64 {
		t.Fatalf("the carrier's fetch: %v, %v", first, err)
	}

	release()
	if run := r.a.runEnded(runID); run.GetString("status") != schema.RunSucceeded {
		t.Fatalf("the run: %v", run.FieldsData())
	}
	if _, err := fetchFirst(carrier, artifact); grpcCode(err) != codes.PermissionDenied {
		t.Errorf("the carrier fetched the copy of a run that ended: %v", err)
	}
	if _, err := fetchFirst(carrier, "panel-export:"+strings.Repeat("a", 15)); grpcCode(err) != codes.PermissionDenied {
		t.Errorf("the copy of no run: %v", err)
	}
}

// grpcCode is an error's gRPC code.
func grpcCode(err error) codes.Code { return status.Code(err) }

// TestABrokenExportStreamIsResumedThenRetried (#32): a fetch of the panel's
// copy that breaks off is resumed from where it got to, and the snapshot
// holds the whole copy; one that keeps breaking off fails the run as
// interrupted, which the panel tries again later.
func TestABrokenExportStreamIsResumedThenRetried(t *testing.T) {
	breaker := fakefetch.NewBreaker(fakefetch.Fault{After: 4096, Code: codes.Unavailable, Message: "the connection broke"})
	r := newRecovery(t, recoverySetup{
		options: []func(*gateway.Server){func(s *gateway.Server) { s.StreamInterceptors = append(s.StreamInterceptors, breaker.Interceptor) }},
		prepare: []func(*agent.Agent){func(a *agent.Agent) {
			a.PanelFetchWaits = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}
		}},
	})

	run := r.runPanelJob(r.a)
	if run.GetString("status") != schema.RunSucceeded || breaker.Broken() != 1 {
		t.Fatalf("the resumed run: %v, %d broken", run.FieldsData(), breaker.Broken())
	}
	stored := r.newestCopy()
	if len(stored) <= 4096 {
		t.Fatalf("the copy has %d bytes", len(stored))
	}
	if _, err := panelbackup.Verify(bytes.NewReader(stored), testutil.MasterKey); err != nil {
		t.Fatalf("the resumed copy: %v", err)
	}

	// the first fetch and its three resumes break off
	broken := make([]fakefetch.Fault, 4)
	for i := range broken {
		broken[i] = fakefetch.Fault{After: 4096 * (i + 1), Code: codes.Unavailable, Message: "the connection broke"}
	}
	breaker.Add(broken...)
	failed := r.runPanelJob(r.a)
	retry := failed.GetDateTime("retry_at").Time()
	if failed.GetString("status") != schema.RunFailed || failed.GetString("failure") != backup.FailureInterrupted ||
		!strings.HasPrefix(failed.GetString("error"), "Restic saved no snapshot: the panel's copy broke off after ") ||
		retry.Before(time.Now().Add(4*time.Minute)) || retry.After(time.Now().Add(6*time.Minute)) {
		t.Errorf("the run that kept breaking off: %v", failed.FieldsData())
	}
	if breaker.Broken() != 5 {
		t.Errorf("%d fetches broke", breaker.Broken())
	}
	if found, _ := r.copies(); len(found) != 1 {
		t.Errorf("%d copies", len(found))
	}
}

// TestAPanelBackupFollowsAnEnrollment (#32): a server enrolled after the
// panel's backup is in its next one, a change run soon after the enrollment.
func TestAPanelBackupFollowsAnEnrollment(t *testing.T) {
	// node-1 enrolls before the panel has its job: the change run that
	// follows is due at once, and finds nothing to back up
	r := newRecovery(t, recoverySetup{panel: func(p *panel.Panel) { p.SetChangeBackupTiming(time.Millisecond, time.Hour) }})
	r.a.panel.SetChangeBackupTiming(300*time.Millisecond, time.Hour)
	r.a.enrolledOn("node-2", nil)

	var change *core.Record
	r.a.waitFor("a change run", func() bool {
		runs, err := r.a.app.FindRecordsByFilter(schema.BackupRuns, "job={:job} && trigger={:change}", "", 0, 0,
			dbx.Params{"job": r.job, "change": schema.RunChange})
		if err != nil || len(runs) == 0 {
			return false
		}
		change = runs[0]
		return true
	})
	if run := r.a.runEnded(change.Id); run.GetString("status") != schema.RunSucceeded {
		t.Fatalf("the change run: %v", run.FieldsData())
	}
	reader, err := panelbackup.NewReader(bytes.NewReader(r.newestCopy()), testutil.MasterKey)
	if err != nil {
		t.Fatal(err)
	}
	if manifest := reader.Manifest(); manifest.Hosts != 2 || manifest.Run != change.Id {
		t.Errorf("the copy's manifest: %+v", manifest)
	}
	if runs := r.a.runs(r.job); len(runs) != 1 {
		t.Errorf("%d runs", len(runs))
	}
}

// serveErr starts a panel's hooks as serving does, and says why they
// refused.
func serveErr(app core.App) error {
	router, err := apis.NewRouter(app)
	if err != nil {
		return err
	}
	return app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(*core.ServeEvent) error { return nil })
}

// copyOf is a consistent copy of a running panel's database, as its own
// backup makes one, in a data dir of its own.
func copyOf(t *testing.T, app core.App) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := app.ConcurrentDB().NewQuery("VACUUM INTO {:path}").Bind(dbx.Params{"path": filepath.Join(dir, "data.db")}).Execute(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// startOn starts a panel on an app whose data dir is a copy of dir, with a
// change of its config, and says why it refused; the panel and its app are
// returned too.
func startOn(t *testing.T, dir string, change func(*panel.Config)) (*panel.Panel, *tests.TestApp, error) {
	t.Helper()
	app := testutil.NewAppFrom(t, dir)
	t.Cleanup(app.Cleanup)
	config := &panel.Config{MasterKey: testutil.MasterKey, GRPCAddr: "127.0.0.1:0", PublicURL: "http://panel.test"}
	if change != nil {
		change(config)
	}
	p := panel.New(app, config, fstest.MapFS{"index.html": {Data: []byte("ui")}})
	p.Register()
	return p, app, serveErr(app)
}

// caRows are the ids of a database's internal CAs.
func caRows(t *testing.T, app core.App) []string {
	t.Helper()
	var ids []string
	if err := app.DB().NewQuery("SELECT id FROM " + schema.InternalCA + " ORDER BY id").Column(&ids); err != nil {
		t.Fatal(err)
	}
	return ids
}

// TestAWrongStartFailsClosed (#32): a panel started where its servers would
// lose it refuses with its sentence, makes no CA and starts no gateway: with
// the expected pin on an empty directory, with another master key than the
// one that sealed its database, over a database that holds two CAs, and
// over one that had servers but lost its CA.
func TestAWrongStartFailsClosed(t *testing.T) {
	h := newHarness(t)
	testutil.NewHost(t, h.app, "node-1")
	pin := h.panel.Authority.Pin()
	sealed := copyOf(t, h.app)

	fresh := testutil.NewApp(t)
	empty := copyOf(t, fresh)
	fresh.Cleanup()

	other := make([]byte, secrets.KeySize)
	copy(other, "another master key of 32 bytes!!")
	twoCAs := copyOf(t, h.app)
	noCA := copyOf(t, h.app)

	for _, test := range []struct {
		name    string
		dir     string
		change  func(*panel.Config)
		prepare func(app core.App)
		refusal string
		cas     int
	}{
		{name: "the expected pin on an empty directory", dir: empty, change: func(c *panel.Config) { c.ExpectCAPin = pin },
			refusal: "This database has no internal CA, but POCKETCLOUD_EXPECT_CA_PIN expects " + pin + ": restore the panel's backup into it first."},
		{name: "another master key", dir: sealed, change: func(c *panel.Config) { c.MasterKey = other }, cas: 1,
			refusal: "This master key (id " + secrets.KeyID(other) + ") did not seal this database (id " + secrets.KeyID(testutil.MasterKey) +
				"): start the panel with the key file of its recovery kit."},
		{name: "two CAs", dir: twoCAs, cas: 2, prepare: func(app core.App) {
			var columns []string
			if err := app.DB().NewQuery("SELECT name FROM pragma_table_info('" + schema.InternalCA + "') WHERE name != 'id'").Column(&columns); err != nil {
				t.Fatal(err)
			}
			list := strings.Join(columns, ", ")
			if _, err := app.DB().NewQuery("INSERT INTO " + schema.InternalCA + " (id, " + list + ") SELECT 'secondcaxxxxxxx', " + list + " FROM " + schema.InternalCA).Execute(); err != nil {
				t.Fatal(err)
			}
		}, refusal: "The database holds two internal CAs: restore it whole, never merge."},
		{name: "servers without a CA", dir: noCA, prepare: func(app core.App) {
			if _, err := app.DB().NewQuery("DELETE FROM " + schema.InternalCA).Execute(); err != nil {
				t.Fatal(err)
			}
		}, refusal: "This database had servers or a CA (pin " + pin + ") but holds no internal CA: it is damaged or not this panel's. Restore it from a backup: a new CA would make every server enroll again."},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.prepare != nil {
				app := testutil.NewAppFrom(t, test.dir)
				test.prepare(app)
				test.dir = copyOf(t, app)
				app.Cleanup()
			}
			p, app, err := startOn(t, test.dir, test.change)
			if err == nil || err.Error() != test.refusal {
				t.Fatalf("the start: %v", err)
			}
			if p.Authority != nil || p.Gateway != nil {
				t.Error("the panel opened its CA or its gateway")
			}
			if got := caRows(t, app); len(got) != test.cas {
				t.Errorf("the database holds %d CAs", len(got))
			}
		})
	}
}
