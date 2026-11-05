package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/dns"
	"github.com/oldbear24/PocketServerManager/internal/dns/rewrite"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// DNS servers of a LAN (#26), AdGuard Home and Pi-hole, are providers like
// the others, but the panel reaches them through the agent of a server on
// their network: every read and change is a dns.rewrite command, and the
// address and credentials are added when the command is delivered. Their
// zones are named by hand, such as lan or home.arpa, and their records point
// at a server's address on its network.

// rewriteWait is how long the panel waits for the agent's answer.
const rewriteWait = time.Minute

var rewriteOps = map[string]agentv1.DnsRewriteOp{
	dns.RewriteList:   agentv1.DnsRewriteOp_DNS_REWRITE_OP_LIST,
	dns.RewriteAppend: agentv1.DnsRewriteOp_DNS_REWRITE_OP_APPEND,
	dns.RewriteDelete: agentv1.DnsRewriteOp_DNS_REWRITE_OP_DELETE,
}

// rewriter runs the operations of a provider on the agent of its server.
func (p *Panel) rewriter(provider *core.Record, driver dns.Driver) (dns.Rewriter, error) {
	hostID := provider.GetString("host")
	if hostID == "" {
		return nil, fmt.Errorf("choose the server whose agent reaches %s", driver.Label)
	}
	providerID, name := provider.Id, provider.GetString("name")
	return func(ctx context.Context, op, zone string, records []rewrite.Record) ([]rewrite.Record, error) {
		host, err := p.App.FindRecordById(schema.Hosts, hostID)
		if err != nil {
			return nil, fmt.Errorf("the server whose agent reaches %s is gone: choose another", driver.Label)
		}
		if !p.Commands.Connected(hostID) {
			return nil, fmt.Errorf("%s is offline, and %s is reached through its agent", host.GetString("name"), driver.Label)
		}
		payload := &agentv1.DnsRewrite{ProviderId: providerID, Op: rewriteOps[op], Zone: zone}
		described := make([]string, 0, len(records))
		for _, r := range records {
			payload.Records = append(payload.Records, &agentv1.DnsRewriteRecord{Name: r.Name, Type: r.Type, Value: r.Value})
			described = append(described, r.Name+" "+r.Type+" "+r.Value)
		}
		label := fmt.Sprintf("Read the records of %s in %s", zone, name)
		switch op {
		case dns.RewriteAppend:
			label = fmt.Sprintf("Add %s to %s", strings.Join(described, ", "), name)
		case dns.RewriteDelete:
			label = fmt.Sprintf("Remove %s from %s", strings.Join(described, ", "), name)
		}
		command, err := p.Commands.Create(ctx, commands.Options{
			Host:    hostID,
			Command: &agentv1.Command{Payload: &agentv1.Command_DnsRewrite{DnsRewrite: payload}},
			Label:   truncateText(label, 255),
		})
		if err != nil {
			return nil, err
		}
		done := p.waitForCommand(ctx, command.Id, rewriteWait)
		if done == nil {
			// a change that arrives later would surprise whoever waits no more
			_ = p.Commands.Cancel(command.Id, "The panel stopped waiting for it.")
			return nil, fmt.Errorf("the agent of %s did not answer in time", host.GetString("name"))
		}
		if done.GetString("status") != schema.CommandSucceeded {
			message := done.GetString("error")
			if message == "" {
				message = "the command " + done.GetString("status")
			}
			return nil, errors.New(message)
		}
		if op != dns.RewriteList {
			return nil, nil
		}
		var result struct {
			Records []rewrite.Record `json:"records"`
		}
		if err := json.Unmarshal([]byte(done.GetString("result")), &result); err != nil {
			return nil, fmt.Errorf("the agent's answer cannot be read: %w", err)
		}
		return result.Records, nil
	}, nil
}

// enrichDNSRewrite adds the server's address and credentials when the
// command is delivered: viewers read commands.
func (p *Panel) enrichDNSRewrite(_ *core.Record, command *agentv1.Command) error {
	payload := command.GetDnsRewrite()
	provider, err := p.App.FindRecordById(schema.DNSProviders, payload.GetProviderId())
	if err != nil {
		return fmt.Errorf("the provider is gone: %w", err)
	}
	credentials, err := p.credentialsOf(provider)
	if err != nil {
		return err
	}
	payload.Kind = provider.GetString("kind")
	payload.Url = credentials["url"]
	payload.Username = credentials["username"]
	payload.Password = credentials["password"]
	return nil
}

// lanDriver tells whether a provider is a DNS server of a LAN.
func lanDriver(provider *core.Record) bool {
	if provider == nil {
		return false
	}
	driver, ok := dns.Lookup(provider.GetString("kind"))
	return ok && driver.Agent
}

// checkLANProvider checks what a DNS server of a LAN needs: the address of
// its web interface, and a server whose agent reaches it.
func (p *Panel) checkLANProvider(credentials map[string]string, hostID string) error {
	if _, err := rewrite.ParseURL(credentials["url"]); err != nil {
		return err
	}
	if hostID == "" {
		return errors.New("choose the server whose agent reaches it: one on the same network")
	}
	if _, err := p.App.FindRecordById(schema.Hosts, hostID); err != nil {
		return errors.New("the server does not exist")
	}
	return nil
}
