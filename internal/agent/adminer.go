package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
)

const (
	adminerPort      = 8080
	adminerPluginDir = "/var/www/html/plugins-enabled"
	// labelAdminer marks an Adminer container and its network with their session.
	labelAdminer      = "pocketcloud.adminer"
	adminerMaxTTL     = time.Hour
	adminerDialLimit  = 10 * time.Second
	adminerChunkSize  = 32 << 10
	adminerCleanLimit = 30 * time.Second
)

// adminerPlugin signs Adminer in with the credentials in its container's
// environment, so that they never pass through the browser: the login form only
// keeps its button. It is written for Adminer 4.
const adminerPlugin = `<?php
// PocketCloud: the credentials come from this container's environment.
class AdminerPocketCloud {
	function credentials() {
		return array(getenv('PC_SERVER'), getenv('PC_USER'), getenv('PC_PASSWORD'));
	}

	function login($login, $password) {
		return true;
	}

	function database() {
		return getenv('PC_DATABASE');
	}

	function loginFormField($name, $heading, $value) {
		$values = array(
			'driver' => getenv('PC_DRIVER'),
			'server' => getenv('PC_SERVER'),
			'username' => getenv('PC_USER'),
			'password' => 'pocketcloud',
			'db' => getenv('PC_DATABASE'),
		);
		if (!array_key_exists($name, $values)) {
			return null;
		}
		return '<input type="hidden" name="auth[' . $name . ']" value="' . htmlspecialchars($values[$name]) . '">';
	}
}

return new AdminerPocketCloud();
`

// handleAdminerStart starts a temporary Adminer container on a network of its own,
// which the database server joins under its name, and relays connections to it.
// Nothing about it is published: the agent joins that network to reach it.
func (a *Agent) handleAdminerStart(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetDbAdminerStart()
	connection := payload.GetConnection()

	engine, err := connectionOf(connection)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(payload.GetNetwork(), "pcadm-") || payload.GetSessionId() == "" {
		return nil, errors.New("an Adminer session needs a session id and a network of its own")
	}

	client := a.client.Load()
	if client == nil {
		return nil, errors.New("not connected")
	}

	ttl := time.Duration(payload.GetTtlS()) * time.Second
	if ttl <= 0 || ttl > adminerMaxTTL {
		ttl = adminerMaxTTL
	}

	session := &adminerSession{agent: a, id: payload.GetSessionId(), network: payload.GetNetwork(), server: connection.GetContainerId()}

	if err := session.start(ctx, payload, engine, reporter, ttl); err != nil {
		session.cleanup()
		return nil, err
	}

	relayCtx, cancel := context.WithTimeout(context.Background(), ttl)
	stream, err := (*client).Relay(relayCtx)
	if err != nil {
		cancel()
		session.cleanup()
		return nil, err
	}
	if err := stream.Send(&agentv1.RelayRequest{SessionId: session.id}); err != nil {
		cancel()
		session.cleanup()
		return nil, err
	}

	a.adminerSessions.add(session.id, cancel)

	go func() {
		defer cancel()
		defer a.adminerSessions.remove(session.id)
		defer session.cleanup()
		session.relay(relayCtx, stream)
	}()

	return map[string]any{
		"session":    session.id,
		"container":  session.container,
		"expires_at": time.Now().Add(ttl).UTC().Format(time.RFC3339),
	}, nil
}

func (a *Agent) handleAdminerStop(_ context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	a.adminerSessions.stop(command.GetDbAdminerStop().GetSessionId())
	return nil, nil
}

// adminerSession is one Adminer container and the relay to it.
type adminerSession struct {
	agent   *Agent
	id      string
	network string
	server  string

	container string
	self      string
	address   string
}

func (s *adminerSession) start(ctx context.Context, payload *agentv1.DbAdminerStart, engine databases.Engine, reporter *Reporter, ttl time.Duration) error {
	a := s.agent
	labels := map[string]string{"pocketcloud.role": "job", labelAdminer: s.id}

	reporter.Progress("network", "Preparing the network of the session", 10)
	if _, err := a.Docker.CreateNetwork(ctx, s.network, dockerapi.NetworkOptions{Internal: true, Labels: labels}); err != nil {
		return fmt.Errorf("the session's network: %w", err)
	}
	if err := a.attach(ctx, s.network, s.server, []string{payload.GetServerAlias()}); err != nil {
		return fmt.Errorf("attaching the database server: %w", err)
	}

	reporter.Progress("pull", "Pulling "+payload.GetImage(), 30)
	if err := a.Docker.Pull(ctx, payload.GetImage(), nil, nil); err != nil {
		return fmt.Errorf("failed to pull %s: %w", payload.GetImage(), err)
	}

	user, password := payload.GetUser(), payload.GetPassword()
	if user == "" {
		user, password = payload.GetConnection().GetAdminUser(), payload.GetConnection().GetAdminPassword()
	}
	driver := "server"
	if engine.Name == databases.Postgres.Name {
		driver = "pgsql"
	}

	labels["pocketcloud.expires"] = strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)

	id, err := a.Docker.Create(ctx, "pcadm-"+shortID(s.id), map[string]any{
		"Image": payload.GetImage(),
		"Env": []string{
			"ADMINER_DEFAULT_SERVER=" + payload.GetServerAlias(),
			"PC_DRIVER=" + driver,
			"PC_SERVER=" + payload.GetServerAlias(),
			"PC_USER=" + user,
			"PC_PASSWORD=" + password,
			"PC_DATABASE=" + payload.GetDatabase(),
		},
		"Labels": labels,
		"HostConfig": map[string]any{
			"NetworkMode": s.network,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create the Adminer container: %w", err)
	}
	s.container = id

	if err := a.Docker.CopyFile(ctx, id, adminerPluginDir, "pocketcloud.php", []byte(adminerPlugin), 0o644); err != nil {
		return fmt.Errorf("failed to install the sign-in plugin: %w", err)
	}

	reporter.Progress("start", "Starting Adminer", 70)
	if err := a.Docker.Start(ctx, id); err != nil {
		return fmt.Errorf("failed to start Adminer: %w", err)
	}

	// the agent reaches Adminer on the session's network; outside of a container,
	// as in development, there is nothing to join
	if self, err := a.ownContainer(ctx); err == nil {
		if err := a.attach(ctx, s.network, self.ID, nil); err != nil {
			return fmt.Errorf("the agent could not join the session's network: %w", err)
		}
		s.self = self.ID
	} else {
		a.Log.Debug("the agent has no container to join the session's network with", "error", err)
	}

	info, err := a.Docker.Inspect(ctx, id)
	if err != nil {
		return err
	}
	endpoint := info.NetworkSettings.Networks[s.network]
	ip, _ := endpoint["IPAddress"].(string)
	if ip == "" {
		return errors.New("the Adminer container got no address on the session's network")
	}
	s.address = net.JoinHostPort(ip, strconv.Itoa(adminerPort))

	return nil
}

// cleanup removes everything the session made. It runs when the relay ends: at
// the end of its time, when the panel ends it, or when the connection is lost.
func (s *adminerSession) cleanup() {
	a := s.agent
	ctx, cancel := context.WithTimeout(context.Background(), adminerCleanLimit)
	defer cancel()

	if s.container != "" {
		if err := a.Docker.Remove(ctx, s.container, true, true); err != nil && !dockerapi.IsNotFound(err) {
			a.Log.Warn("failed to remove an Adminer container", "session", s.id, "error", err)
		}
	}
	for _, member := range []string{s.self, s.server} {
		if member == "" {
			continue
		}
		if err := a.Docker.DisconnectNetwork(ctx, s.network, member, true); err != nil && !dockerapi.IsNotFound(err) {
			a.Log.Debug("failed to leave an Adminer network", "session", s.id, "error", err)
		}
	}
	if err := a.Docker.RemoveNetwork(ctx, s.network); err != nil && !dockerapi.IsNotFound(err) {
		a.Log.Warn("failed to remove an Adminer network", "session", s.id, "error", err)
	}
}

// relay carries the connections of the browser session to Adminer. The panel
// numbers them; each is a connection of its own to Adminer, which serves one
// request per connection.
func (s *adminerSession) relay(ctx context.Context, stream agentv1.AgentService_RelayClient) {
	var sendMu sync.Mutex
	send := func(message *agentv1.RelayRequest) bool {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(message) == nil
	}

	var connsMu sync.Mutex
	conns := map[uint32]net.Conn{}

	defer func() {
		connsMu.Lock()
		for _, conn := range conns {
			conn.Close()
		}
		connsMu.Unlock()
		_ = stream.CloseSend()
	}()

	dial := s.agent.DialTool
	if dial == nil {
		dial = func(ctx context.Context, address string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", address)
		}
	}

	for {
		message, err := stream.Recv()
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				s.agent.Log.Debug("an Adminer relay ended", "session", s.id, "error", err)
			}
			return
		}

		id := message.GetConnId()
		if id == 0 {
			if message.GetClose() {
				return
			}
			continue
		}

		switch {
		case message.GetOpen():
			dialCtx, cancelDial := context.WithTimeout(ctx, adminerDialLimit)
			conn, err := dial(dialCtx, s.address)
			cancelDial()
			if err != nil {
				send(&agentv1.RelayRequest{ConnId: id, Closed: true, Error: "Adminer does not answer: " + err.Error()})
				continue
			}

			connsMu.Lock()
			conns[id] = conn
			connsMu.Unlock()

			go func() {
				buffer := make([]byte, adminerChunkSize)
				for {
					count, err := conn.Read(buffer)
					if count > 0 && !send(&agentv1.RelayRequest{ConnId: id, Data: append([]byte(nil), buffer[:count]...)}) {
						return
					}
					if err != nil {
						connsMu.Lock()
						delete(conns, id)
						connsMu.Unlock()
						conn.Close()
						send(&agentv1.RelayRequest{ConnId: id, Closed: true})
						return
					}
				}
			}()

		case message.GetClose():
			connsMu.Lock()
			conn := conns[id]
			delete(conns, id)
			connsMu.Unlock()
			if conn != nil {
				conn.Close()
			}

		case len(message.GetData()) > 0:
			connsMu.Lock()
			conn := conns[id]
			connsMu.Unlock()
			if conn == nil {
				continue
			}
			if _, err := conn.Write(message.GetData()); err != nil {
				conn.Close()
			}
		}
	}
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// removeStaleAdminers removes Adminer containers and networks that a previous run
// of the agent left behind: their relays ended with it.
func (a *Agent) removeStaleAdminers(ctx context.Context) {
	containers, err := a.Docker.Containers(ctx)
	if err != nil {
		return
	}
	for _, container := range containers {
		if container.Labels[labelAdminer] == "" {
			continue
		}
		if err := a.Docker.Remove(ctx, container.ID, true, true); err != nil && !dockerapi.IsNotFound(err) {
			a.Log.Warn("failed to remove a stale Adminer container", "container", container.ID, "error", err)
		}
	}

	networks, err := a.Docker.Networks(ctx)
	if err != nil {
		return
	}
	for _, network := range networks {
		if network.Labels[labelAdminer] == "" {
			continue
		}
		// the database server and the agent itself are still on it
		if info, err := a.Docker.InspectNetwork(ctx, network.Name); err == nil {
			for id := range info.Containers {
				if err := a.Docker.DisconnectNetwork(ctx, network.Name, id, true); err != nil && !dockerapi.IsNotFound(err) {
					a.Log.Debug("failed to leave a stale Adminer network", "network", network.Name, "error", err)
				}
			}
		}
		if err := a.Docker.RemoveNetwork(ctx, network.Name); err != nil && !dockerapi.IsNotFound(err) {
			a.Log.Debug("failed to remove a stale Adminer network", "network", network.Name, "error", err)
		}
	}
}
