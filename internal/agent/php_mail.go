package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
	"github.com/oldbear24/PocketServerManager/internal/sites"
)

// Outgoing mail of PHP sites (#51): PHP's mail() hands a message to msmtp,
// which the runtime image points at the panel's SMTP relay, and msmtp logs
// one line per message to the container's log: the counts come from there.

const (
	// mailWindow is how far back the counts go.
	mailWindow = 24 * time.Hour
	// mailFailuresKept is how many of the latest failures are returned.
	mailFailuresKept = 10
	// mailLogLimit is how much of the log is read at most.
	mailLogLimit = 64 << 20
)

// msmtpLine is msmtp's own line after Docker's timestamp: its date, then the
// relay. A line the site's PHP wrote to its log, as with a visitor's text in
// it, is not one.
var msmtpLine = regexp.MustCompile(`^\S+ [A-Z][a-z]{2} [ 0-9]\d \d\d:\d\d:\d\d host=\S+ `)

// msmtpField is a field of msmtp's log line: name=value or name='value'.
var msmtpField = regexp.MustCompile(`(\w+)=('([^']*)'|\S+)`)

// mailFailure is a message the relay did not take.
type mailFailure struct {
	At         string `json:"at"`
	Recipients string `json:"recipients"`
	Error      string `json:"error"`
}

// mailCounts is what the log says of the mail sent.
type mailCounts struct {
	Sent       int
	Failed     int
	LastSentAt string
	Failures   []mailFailure
}

func (a *Agent) handleSitePHPMail(ctx context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	site := command.GetSitePhpMail().GetSite()
	if err := sites.ValidName(site); err != nil {
		return nil, err
	}
	current, err := a.sitePHP(ctx, site, sites.PHPContainer(site))
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, fmt.Errorf("%s does not run", sites.PHPContainer(site))
	}
	// the counts start with the container: a replaced one starts a new log
	since := time.Now().Add(-mailWindow)
	if started := current.State.StartedAt; started.After(since) {
		since = started
	}
	logs, err := a.Docker.Logs(ctx, current.ID, dockerapi.LogOptions{Tail: -1, Timestamps: true, Since: since})
	if err != nil {
		return nil, err
	}
	defer func() { _ = logs.Close() }()
	reader, writer := io.Pipe()
	go func() {
		limit := 0
		_ = dockerapi.ReadMultiplexed(logs, func(stream byte, data []byte) {
			if stream == 2 && limit < mailLogLimit {
				limit += len(data)
				_, _ = writer.Write(data)
			}
		})
		_ = writer.Close()
	}()
	counts := countMail(reader)
	_, _ = io.Copy(io.Discard, reader)

	failures := make([]any, 0, len(counts.Failures))
	for _, failure := range counts.Failures {
		failures = append(failures, map[string]any{"at": failure.At, "recipients": failure.Recipients, "error": failure.Error})
	}
	return map[string]any{
		"sent": float64(counts.Sent), "failed": float64(counts.Failed),
		"last_sent_at": counts.LastSentAt, "failures": failures, "since": since.UTC().Format(time.RFC3339),
	}, nil
}

// countMail reads msmtp's lines from a log with Docker's timestamps: a line
// ends with exitcode=EX_OK for a message the relay took, and with another
// code and an errormsg for one it did not. The latest failures come first.
func countMail(log io.Reader) mailCounts {
	var counts mailCounts
	scanner := bufio.NewScanner(log)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !msmtpLine.MatchString(line) || !strings.Contains(line, " exitcode=") || !strings.Contains(line, " recipients=") {
			continue
		}
		at, _, _ := strings.Cut(line, " ")
		fields := map[string]string{}
		for _, match := range msmtpField.FindAllStringSubmatch(line, -1) {
			value := match[2]
			if strings.HasPrefix(value, "'") {
				value = match[3]
			}
			fields[match[1]] = value
		}
		if fields["exitcode"] == "EX_OK" {
			counts.Sent++
			counts.LastSentAt = at
			continue
		}
		counts.Failed++
		reason := fields["errormsg"]
		if reason == "" {
			reason = fields["smtpmsg"]
		}
		if reason == "" {
			reason = fields["exitcode"]
		}
		counts.Failures = append([]mailFailure{{At: at, Recipients: fields["recipients"], Error: reason}}, counts.Failures...)
		if len(counts.Failures) > mailFailuresKept {
			counts.Failures = counts.Failures[:mailFailuresKept]
		}
	}
	return counts
}
