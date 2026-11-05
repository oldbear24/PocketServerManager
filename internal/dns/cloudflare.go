package dns

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/libdns/cloudflare"
	"github.com/libdns/libdns"
)

// cloudflareAPI is the base of Cloudflare's API.
const cloudflareAPI = "https://api.cloudflare.com/client/v4"

// httpClient is what the providers' APIs are asked with; tests point it at
// fakes of those APIs.
var httpClient atomic.Pointer[http.Client]

func init() {
	httpClient.Store(&http.Client{Timeout: 30 * time.Second})
}

// SetHTTPClient replaces the client the providers' APIs are asked with.
func SetHTTPClient(c *http.Client) {
	httpClient.Store(c)
}

// cloudflareProvider is libdns/cloudflare with a zone list that pages:
// libdns/cloudflare asks for the first page only, 20 zones.
type cloudflareProvider struct {
	*cloudflare.Provider
}

func newCloudflare(token string) *cloudflareProvider {
	return &cloudflareProvider{Provider: &cloudflare.Provider{APIToken: token, HTTPClient: httpClient.Load()}}
}

func (p *cloudflareProvider) ListZones(ctx context.Context) ([]libdns.Zone, error) {
	var zones []libdns.Zone
	for page := 1; ; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/zones?per_page=50&page=%d", cloudflareAPI, page), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+p.APIToken)
		response, err := httpClient.Load().Do(req)
		if err != nil {
			return nil, err
		}
		var body struct {
			Success bool `json:"success"`
			Errors  []struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"errors"`
			Result []struct {
				Name string `json:"name"`
			} `json:"result"`
			ResultInfo struct {
				Page       int `json:"page"`
				TotalPages int `json:"total_pages"`
			} `json:"result_info"`
		}
		err = json.NewDecoder(response.Body).Decode(&body)
		_ = response.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("HTTP %d: the answer is not JSON: %w", response.StatusCode, err)
		}
		if !body.Success {
			messages := make([]string, 0, len(body.Errors))
			for _, e := range body.Errors {
				messages = append(messages, fmt.Sprintf("%d %s", e.Code, e.Message))
			}
			return nil, fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.Join(messages, "; "))
		}
		for _, z := range body.Result {
			zones = append(zones, libdns.Zone{Name: z.Name + "."})
		}
		if body.ResultInfo.TotalPages <= page || len(body.Result) == 0 {
			return zones, nil
		}
	}
}
