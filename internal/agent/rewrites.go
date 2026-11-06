package agent

import (
	"context"
	"errors"
	"net/http"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/dns/rewrite"
)

// rewriteClient asks the DNS servers of the LAN: plain HTTP to an address the
// panel gives, without a proxy, as they are on the server's own network.
var rewriteClient = &http.Client{
	Timeout:   20 * time.Second,
	Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 15 * time.Second, IdleConnTimeout: 30 * time.Second},
	// a redirect would go somewhere the panel did not name
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func (a *Agent) registerRewriteHandlers() {
	a.Executor.Handle("dns.rewrite", a.handleDNSRewrite)
}

// handleDNSRewrite reads or changes the local DNS records of AdGuard Home or
// Pi-hole for the panel (#26).
func (a *Agent) handleDNSRewrite(ctx context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	payload := command.GetDnsRewrite()
	client, err := rewrite.New(payload.GetKind(), payload.GetUrl(), payload.GetUsername(), payload.GetPassword(), rewriteClient)
	if err != nil {
		return nil, err
	}
	records := make([]rewrite.Record, 0, len(payload.GetRecords()))
	for _, r := range payload.GetRecords() {
		records = append(records, rewrite.Record{Name: r.GetName(), Type: r.GetType(), Value: r.GetValue()})
	}
	switch payload.GetOp() {
	case agentv1.DnsRewriteOp_DNS_REWRITE_OP_LIST:
		found, err := client.List(ctx)
		if err != nil {
			return nil, err
		}
		out := []any{}
		for _, r := range rewrite.InZone(found, payload.GetZone()) {
			out = append(out, map[string]any{"name": r.Name, "type": r.Type, "value": r.Value})
		}
		return map[string]any{"records": out}, nil
	case agentv1.DnsRewriteOp_DNS_REWRITE_OP_APPEND:
		return map[string]any{"changed": float64(len(records))}, client.Append(ctx, records)
	case agentv1.DnsRewriteOp_DNS_REWRITE_OP_DELETE:
		return map[string]any{"changed": float64(len(records))}, client.Delete(ctx, records)
	}
	return nil, errors.New("the command names no operation")
}
