package agent

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// series are the lines of one histogram of the pinned Caddy's metrics, as
// read on real Docker: cumulative counts for each bound, and the count.
func series(code, handler, host string, cumulative ...int) []string {
	labels := fmt.Sprintf(`code=%q,handler=%q,host=%q,method="GET",server="http"`, code, handler, host)
	lines := []string{}
	for i, bound := range latencyBounds {
		lines = append(lines, fmt.Sprintf("caddy_http_request_duration_seconds_bucket{%s,le=%q} %d", labels, bound, cumulative[i]))
	}
	return append(lines, fmt.Sprintf("caddy_http_request_duration_seconds_count{%s} %d", labels, cumulative[len(cumulative)-1]))
}

func metricsOf(groups ...[]string) []byte {
	lines := []string{"# HELP caddy_http_request_duration_seconds Histogram of round-trip request durations."}
	for _, group := range groups {
		lines = append(lines, group...)
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func TestTheDurationsOfAHostsRequestsAreReadFromCaddy(t *testing.T) {
	// real counts of a Laravel site under load, and two 502s of another handler
	ok := series("200", "encode", "shop.test", 0, 16, 50, 166, 841, 2710, 3052, 3110, 3154, 3181, 3185, 3185)
	failed := series("502", "subroute", "shop.test", 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2)
	other := series("200", "file_server", "_other", 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9)
	first := readLatency(metricsOf(ok, failed, other))
	if _, found := first["_other"]; found {
		t.Error("a name of no route was counted")
	}
	if shop := first["shop.test"]; shop.cumulative[0] != 2 || shop.cumulative[11] != 3187 || shop.errors != 2 {
		t.Fatalf("%+v", shop)
	}

	// the first reading is all there is, one bucket each
	since := latencySince(nil, first)["shop.test"]
	if want := []uint64{2, 16, 34, 116, 675, 1869, 342, 58, 44, 27, 4, 0}; !slices.Equal(since.GetBuckets(), want) || since.GetErrors() != 2 {
		t.Errorf("first: %v", since)
	}

	// then only what came since: 15 more requests that took over 10 s, one more 502
	slower := series("200", "encode", "shop.test", 0, 16, 50, 166, 841, 2710, 3052, 3110, 3154, 3181, 3185, 3200)
	more := series("502", "subroute", "shop.test", 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3)
	next := readLatency(metricsOf(slower, more))
	since = latencySince(first, next)["shop.test"]
	if since.GetErrors() != 1 || !slices.Equal(since.GetBuckets(), []uint64{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 15}) {
		t.Errorf("since: %v", since)
	}
	// nothing new: the hostname is left out
	if _, found := latencySince(next, next)["shop.test"]; found {
		t.Error("a hostname without requests was reported")
	}
	// an edge that restarted counts from 0
	restarted := readLatency(metricsOf(series("200", "encode", "shop.test", 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1)))
	if since := latencySince(next, restarted)["shop.test"]; !slices.Equal(since.GetBuckets(), []uint64{0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}) || since.GetErrors() != 0 {
		t.Errorf("after a restart: %v", since)
	}
}
