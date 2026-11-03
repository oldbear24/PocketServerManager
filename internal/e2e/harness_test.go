// Package e2e runs a real panel and real agents against each other over gRPC with
// mutual TLS. Only the Docker Engine is a fake.
package e2e

import (
	"context"
	"io"
	"net/http"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/oldbear24/PocketServerManager/internal/agent"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/enroll"
	"github.com/oldbear24/PocketServerManager/internal/gateway"
	"github.com/oldbear24/PocketServerManager/internal/panel"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

const waitLimit = 15 * time.Second

type harness struct {
	t       *testing.T
	app     *tests.TestApp
	panel   *panel.Panel
	handler http.Handler
	docker  *fakedocker.Engine
}

// newHarness starts a panel. The options run before the gateway starts, which is
// the only safe moment to change its settings.
func newHarness(t *testing.T, options ...func(*gateway.Server)) *harness {
	t.Helper()

	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)

	p := panel.New(app, &panel.Config{
		MasterKey: testutil.MasterKey,
		GRPCAddr:  "127.0.0.1:0",
		PublicURL: "http://panel.test",
	}, fstest.MapFS{"index.html": {Data: []byte("ui")}})
	p.Observers = append(p.Observers, options...)
	p.Register()

	return &harness{
		t:       t,
		app:     app,
		panel:   p,
		handler: testutil.Handler(t, app),
		docker:  fakedocker.New(t),
	}
}

// token creates a one-time enrollment token.
func (h *harness) token(name string, roles ...string) string {
	h.t.Helper()

	_, token, err := enroll.Create(h.app, name, roles, "")
	if err != nil {
		h.t.Fatal(err)
	}

	return token
}

func (h *harness) agentConfig(name, token string) agent.Config {
	return agent.Config{
		Panel:             h.panel.Gateway.Addr(),
		CAPin:             h.panel.Authority.Pin(),
		Token:             token,
		Name:              name,
		StateDir:          h.t.TempDir(),
		HostRoot:          h.t.TempDir(),
		DockerHost:        h.docker.Host(),
		ContainerName:     "pocketcloud-agent",
		HeartbeatInterval: 100 * time.Millisecond,
		InventoryInterval: time.Hour,
		SelfCheckInterval: time.Hour,
		BackoffMin:        20 * time.Millisecond,
		BackoffMax:        200 * time.Millisecond,
		KeepaliveTime:     30 * time.Second,
	}
}

// runningAgent is an agent process under test.
type runningAgent struct {
	*agent.Agent
	config agent.Config
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
}

// stop ends the agent the way SIGTERM does, and waits for it.
func (r *runningAgent) stop() error {
	var err error
	r.once.Do(func() {
		r.cancel()
		err = <-r.done
	})
	return err
}

func (h *harness) start(config agent.Config, prepare ...func(*agent.Agent)) *runningAgent {
	h.t.Helper()

	a, err := agent.New(config, io.Discard)
	if err != nil {
		h.t.Fatal(err)
	}

	for _, p := range prepare {
		p(a)
	}

	ctx, cancel := context.WithCancel(context.Background())

	running := &runningAgent{Agent: a, config: config, cancel: cancel, done: make(chan error, 1)}

	go func() { running.done <- a.Run(ctx) }()

	h.t.Cleanup(func() { _ = running.stop() })

	return running
}

// enrolled starts an agent with a new token and waits until it is connected.
func (h *harness) enrolled(name string, prepare ...func(*agent.Agent)) (*runningAgent, *core.Record) {
	h.t.Helper()

	running := h.start(h.agentConfig(name, h.token(name, schema.HostRoleDocker)), prepare...)

	var host *core.Record
	h.waitFor("the agent to connect", func() bool {
		found, err := h.app.FindFirstRecordByData(schema.Hosts, "name", name)
		if err != nil || !found.GetBool("stream_open") {
			return false
		}
		host = found
		return true
	})

	return running, host
}

func (h *harness) host(id string) *core.Record {
	h.t.Helper()

	host, err := h.app.FindRecordById(schema.Hosts, id)
	if err != nil {
		h.t.Fatal(err)
	}

	return host
}

func (h *harness) command(id string) *core.Record {
	h.t.Helper()

	command, err := h.app.FindRecordById(schema.Commands, id)
	if err != nil {
		h.t.Fatal(err)
	}

	return command
}

// waitFor polls until a condition holds, or fails the test.
func (h *harness) waitFor(what string, condition func() bool) {
	h.t.Helper()

	deadline := time.Now().Add(waitLimit)
	for !condition() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *harness) waitForStatus(commandID string, statuses ...string) *core.Record {
	h.t.Helper()

	var record *core.Record
	h.waitFor("command "+commandID+" to reach "+statuses[0], func() bool {
		record = h.command(commandID)
		current := record.GetString("status")
		for _, status := range statuses {
			if current == status {
				return true
			}
		}
		return false
	})

	return record
}
