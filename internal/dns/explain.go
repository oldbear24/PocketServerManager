package dns

import (
	"context"
	"errors"
	"net"
	"strings"
)

// explainHTTP reads the usual failures of a provider's HTTP API.
func explainHTTP(err error) string {
	text := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "The provider did not answer in time."
	case isNetwork(err):
		return "The panel could not reach the provider's API: " + err.Error()
	// Cloudflare answers 403 with code 10000 "Authentication error" both for a
	// wrong token and for one without a permission it needs
	case strings.Contains(text, "10000") || strings.Contains(text, "403") || strings.Contains(text, "forbidden") || strings.Contains(text, "permission"):
		return "The provider refused the request: check the token, and that it may read the zones and edit their DNS records (on Cloudflare: Zone:Read and Zone.DNS:Edit)."
	case strings.Contains(text, "401") || strings.Contains(text, "unauthorized") || strings.Contains(text, "invalid api token") ||
		strings.Contains(text, "authentication"):
		return "The provider refused the credentials: check the token, and that it has not expired."
	// libdns/cloudflare's words for a zone the token cannot see
	case strings.Contains(text, "expected 1 zone, got 0"):
		return "The token cannot see this zone: give it the Zone:Read permission for the zone."
	case strings.Contains(text, "not found") || strings.Contains(text, "404") || strings.Contains(text, "zone could not be found"):
		return "The provider does not know this zone, or the credentials cannot see it."
	case strings.Contains(text, "429") || strings.Contains(text, "rate limit"):
		return "The provider limits how often it may be asked: try again in a minute."
	}
	return ""
}

// explainWEDOS adds what WAPI's allow-list means to the usual failures. WAPI
// says why it refused a request in its result text.
func explainWEDOS(err error) string {
	var refused *WEDOSError
	if errors.As(err, &refused) {
		text := strings.ToLower(refused.Result)
		switch {
		case strings.Contains(text, "ip"):
			return "WEDOS refused the panel's IP address: add the panel's public address to the WAPI allow-list in the WEDOS administration, then test again. WAPI said: " + refused.Result
		case strings.Contains(text, "auth") || strings.Contains(text, "login") || strings.Contains(text, "password"):
			return "WEDOS refused the user name or the WAPI password; the WAPI password is not the password of the WEDOS account. WAPI said: " + refused.Result
		case strings.Contains(text, "domain") || strings.Contains(text, "doména"):
			return "WEDOS does not know this zone for this account. WAPI said: " + refused.Result
		}
		return "WEDOS refused the request: " + refused.Error()
	}
	return explainHTTP(err)
}

func isNetwork(err error) bool {
	var netErr net.Error
	var dnsErr *net.DNSError
	var opErr *net.OpError
	return errors.As(err, &dnsErr) || errors.As(err, &opErr) || (errors.As(err, &netErr) && netErr.Timeout())
}
