package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/pki"
)

// Repointing (#32). An agent dials the address it enrolled with for good:
// a panel restored on a machine that cannot take that address over is
// reached once each server's agent is told the new one. `pocketcloud-agent
// repoint --panel HOST:PORT` dials it with the agent's own certificate and
// its pinned CA, as the agent does, and makes one heartbeat, a probe, which
// passes only a panel that holds this server's CA and knows the server, and
// which the panel keeps nothing of: a server a restored panel marked offline
// stays so until its agent comes back. Only then it rewrites the address in
// identity.json, and nothing else: the key and the certificates stay as
// they are. --check rewrites nothing.

// repointTimeout is how long repoint waits for the panel's answer.
const repointTimeout = 20 * time.Second

// Repoint runs `pocketcloud-agent repoint` with args, and says what it found
// to out. It needs the agent's state directory and nothing else.
func Repoint(ctx context.Context, config Config, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("repoint", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	address := flags.String("panel", "", "the panel's gateway, HOST:PORT")
	check := flags.Bool("check", false, "only check that the panel there holds this server's CA and knows it")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("repoint takes flags only, not %q", flags.Arg(0))
	}
	if err := checkPanelAddress(*address); err != nil {
		return err
	}
	identity, err := LoadIdentity(config.StateDir)
	if errors.Is(err, ErrNotEnrolled) {
		return errors.New("this agent has not enrolled: there is no panel to repoint it from")
	}
	if err != nil {
		return fmt.Errorf("the agent's identity cannot be read: %w", err)
	}
	pin, err := identityPin(identity)
	if err != nil {
		return err
	}

	// the agent's own TLS: its certificate, and the panel's checked under
	// the pinned root; a panel whose certificate is not, holds another CA
	tlsConfig := identity.TLSConfig()
	verify := tlsConfig.VerifyConnection
	var otherCA atomic.Bool
	tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
		err := verify(state)
		if err != nil {
			otherCA.Store(true)
		}
		return err
	}
	conn, err := grpc.NewClient(*address, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		return fmt.Errorf("the panel at %s cannot be dialled: %w", *address, err)
	}
	defer conn.Close()
	callCtx, cancel := context.WithTimeout(ctx, repointTimeout)
	defer cancel()
	_, err = agentv1.NewAgentServiceClient(conn).Heartbeat(callCtx, &agentv1.HeartbeatRequest{SentAt: timestamppb.Now(), Probe: true})
	switch {
	case otherCA.Load():
		return fmt.Errorf("The panel at %s does not hold this server's CA: nothing was changed.", *address) //nolint:staticcheck // the sentence people read
	case status.Code(err) == codes.PermissionDenied, status.Code(err) == codes.Unauthenticated:
		return fmt.Errorf("The panel at %s does not know this server: nothing was changed. The panel said: %s", *address, status.Convert(err).Message()) //nolint:staticcheck // the sentence people read
	case err != nil:
		return fmt.Errorf("The panel at %s cannot be reached: %s. Nothing was changed.", *address, status.Convert(err).Message()) //nolint:staticcheck // the sentence people read
	}
	found := fmt.Sprintf("The panel at %s holds this server's CA (%s) and knows it as %s.", *address, pin, identity.Name)
	if *check {
		fmt.Fprintln(out, found+" Nothing was changed (--check).")
		return nil
	}
	if err := rewritePanel(config.StateDir, *address); err != nil {
		return fmt.Errorf("%s But the address cannot be written: %w", found, err)
	}
	fmt.Fprintf(out, "%s This server now dials %s: restart the agent (docker restart pocketcloud-agent).\n", found, *address)
	return nil
}

// checkPanelAddress checks a gateway's address: a host and a port.
func checkPanelAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	n, portErr := strconv.Atoi(port)
	if err != nil || host == "" || portErr != nil || n < 1 || n > 65535 {
		return fmt.Errorf("--panel is the panel's gateway as HOST:PORT, such as panel.example.com:9443, not %q", address)
	}
	return nil
}

// identityPin is the pin of the CA the identity trusts.
func identityPin(identity *Identity) (string, error) {
	raw, err := os.ReadFile(filepath.Join(identity.dir, caFile))
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return "", errors.New("malformed CA certificate in the identity")
	}
	root, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("malformed CA certificate in the identity: %w", err)
	}
	return pki.PinOf(root.Raw), nil
}

// rewritePanel sets the panel of identity.json to address, and keeps the
// rest of the file as it is.
func rewritePanel(stateDir, address string) error {
	file := filepath.Join(stateDir, identityDir, identityMeta)
	info, err := os.Stat(file)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("malformed identity: %w", err)
	}
	value, err := json.Marshal(address)
	if err != nil {
		return err
	}
	fields["panel"] = value
	rewritten, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(file, rewritten, info.Mode().Perm())
}
