package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// Outgoing mail of PHP sites (#51): the panel's SMTP relay reaches every PHP
// site's PHP-FPM as POCKETCLOUD_SMTP_*, from which the runtime image writes
// msmtp's settings for mail(); the password is a secret, delivered with the
// apply and never stored in a command. msmtp logs one line per message, and
// the site page counts them over the last day.

// mailFailure is a message the relay did not take.
type mailFailure struct {
	At         string `json:"at"`
	Recipients string `json:"recipients"`
	Error      string `json:"error"`
}

// smtpHost is a relay's host name or address.
var smtpHost = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)*[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$|^[0-9a-fA-F:.]+$`)

// smtpSafe is a value msmtp's settings can hold: no quotes, backslashes or
// line breaks, as the runtime image refuses them.
var smtpSafe = regexp.MustCompile(`^[^"'\\\x00-\x1f\x7f]*$`)

func (p *Panel) bindMailRoutes(group *router.RouterGroup[*core.RequestEvent]) {
	everyone := requireRole(schema.RoleViewer, schema.RoleOperator, schema.RoleAdmin)
	admins := requireRole(schema.RoleAdmin)
	group.PUT("/settings/smtp", p.handleSMTPSettings).BindFunc(admins)
	group.GET("/sites/{id}/php/mail", p.handlePHPMail).BindFunc(everyone)
}

// smtpEnv adds the relay to a PHP-FPM container's variables; none when the
// panel has no relay, and mail() then fails.
func (p *Panel) smtpEnv(env map[string]string) map[string]string {
	settings, err := p.settingsRecord()
	if err != nil || settings.GetString("smtp_host") == "" {
		return env
	}
	if env == nil {
		env = map[string]string{}
	}
	env["POCKETCLOUD_SMTP_HOST"] = settings.GetString("smtp_host")
	env["POCKETCLOUD_SMTP_PORT"] = strconv.Itoa(smtpPort(settings))
	env["POCKETCLOUD_SMTP_TLS"] = smtpTLS(settings)
	if user := settings.GetString("smtp_user"); user != "" {
		env["POCKETCLOUD_SMTP_USER"] = user
	}
	if from := settings.GetString("smtp_from"); from != "" {
		env["POCKETCLOUD_SMTP_FROM"] = from
	}
	return env
}

// smtpSecret adds the relay's password to a PHP-FPM container's secrets.
func (p *Panel) smtpSecret(secrets map[string]string) (map[string]string, error) {
	settings, err := p.settingsRecord()
	if err != nil {
		return nil, err
	}
	if settings.GetString("smtp_host") == "" || settings.GetString("smtp_password") == "" {
		return secrets, nil
	}
	password, err := p.Box.OpenString(settings.GetString("smtp_password"))
	if err != nil {
		return nil, fmt.Errorf("the SMTP relay's password cannot be opened: %w", err)
	}
	if secrets == nil {
		secrets = map[string]string{}
	}
	secrets["POCKETCLOUD_SMTP_PASSWORD"] = password
	return secrets, nil
}

func smtpPort(settings *core.Record) int {
	if port := settings.GetInt("smtp_port"); port > 0 {
		return port
	}
	return 587
}

func smtpTLS(settings *core.Record) string {
	if tls := settings.GetString("smtp_tls"); tls != "" {
		return tls
	}
	// port 465 speaks TLS from the start, the others STARTTLS
	if smtpPort(settings) == 465 {
		return "tls"
	}
	return "starttls"
}

// handleSMTPSettings sets the relay: a field left out stays, an empty host
// turns mail off, and an empty password removes it. Every PHP site that runs
// takes the change.
func (p *Panel) handleSMTPSettings(e *core.RequestEvent) error {
	var body struct {
		Host     *string `json:"host"`
		Port     *int    `json:"port"`
		TLS      *string `json:"tls"`
		User     *string `json:"user"`
		Password *string `json:"password"`
		From     *string `json:"from"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}
	settings, err := p.settingsRecord()
	if err != nil {
		return e.InternalServerError("", err)
	}
	if body.Host != nil {
		host := strings.TrimSpace(*body.Host)
		if host != "" && (len(host) > 253 || !smtpHost.MatchString(host)) {
			return e.BadRequestError("The relay is a host name or an address, such as smtp.example.org.", nil)
		}
		settings.Set("smtp_host", host)
	}
	if body.Port != nil {
		if *body.Port < 1 || *body.Port > 65535 {
			return e.BadRequestError("The port is between 1 and 65535.", nil)
		}
		settings.Set("smtp_port", *body.Port)
	}
	if body.TLS != nil {
		switch *body.TLS {
		case "starttls", "tls", "off":
			settings.Set("smtp_tls", *body.TLS)
		default:
			return e.BadRequestError("TLS is starttls, tls or off.", nil)
		}
	}
	for field, value := range map[string]*string{"smtp_user": body.User, "smtp_from": body.From} {
		if value == nil {
			continue
		}
		text := strings.TrimSpace(*value)
		if len(text) > 255 || !smtpSafe.MatchString(text) {
			return e.BadRequestError("The user and the sender hold no quotes, backslashes or line breaks.", nil)
		}
		settings.Set(field, text)
	}
	if body.Password != nil {
		if strings.ContainsAny(*body.Password, "\x00\r\n") {
			return e.BadRequestError("The password holds no line breaks.", nil)
		}
		settings.Set("smtp_password", *body.Password)
		settings.Set("smtp_password_set", *body.Password != "")
	}
	// the envelope sender: msmtp of the runtime image cannot take it from
	// each message, and a relay refuses mail without one
	if settings.GetString("smtp_host") != "" && settings.GetString("smtp_from") == "" {
		return e.BadRequestError("A relay needs a sender, such as noreply@example.org.", nil)
	}
	if err := e.App.SaveWithContext(audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e)), settings); err != nil {
		return e.BadRequestError("The relay could not be saved: "+err.Error(), nil)
	}

	// PHP-FPM reads the relay when it starts
	id, label := requester(e)
	applied := 0
	// every PHP site's, including one whose last apply failed while its old
	// container serves on
	all, _ := e.App.FindRecordsByFilter(schema.SitePHP, "", "", 0, 0)
	for _, php := range all {
		site, err := e.App.FindRecordById(schema.Sites, php.GetString("site"))
		if err != nil {
			continue
		}
		if _, err := p.applyPHP(context.WithoutCancel(e.Request.Context()), site, commands.Options{RequestedBy: id, Requester: label}); err != nil {
			p.Log.Warn("a PHP site did not take the new relay", "site", site.GetString("name"), "error", err)
			continue
		}
		applied++
	}
	return e.JSON(http.StatusOK, map[string]any{
		"host": settings.GetString("smtp_host"), "port": smtpPort(settings), "tls": smtpTLS(settings),
		"user": settings.GetString("smtp_user"), "from": settings.GetString("smtp_from"),
		"password_set": settings.GetBool("smtp_password_set"), "applied": applied,
	})
}

// handlePHPMail counts the mail a PHP site sent through the relay in the last
// day, and its latest failures.
func (p *Panel) handlePHPMail(e *core.RequestEvent) error {
	site, err := p.findSite(e)
	if err != nil {
		return err
	}
	if site.GetString("type") != schema.SitePHPApp {
		return e.BadRequestError("This is not a PHP site.", nil)
	}
	// null without a relay: a nil map would be {}, which the page takes for one
	var relay any
	if settings, err := p.settingsRecord(); err == nil && settings.GetString("smtp_host") != "" {
		relay = map[string]any{"host": settings.GetString("smtp_host"), "port": smtpPort(settings)}
	}
	ctx := e.Request.Context()
	id, label := requester(e)
	command, err := p.Commands.Create(ctx, commands.Options{
		Host:        site.GetString("host"),
		Command:     &agentv1.Command{Payload: &agentv1.Command_SitePhpMail{SitePhpMail: &agentv1.SitePhpMail{Site: site.GetString("name")}}},
		Label:       "Count the mail of " + site.GetString("name"),
		RequestedBy: id,
		Requester:   label,
	})
	if err != nil {
		return e.BadRequestError(capitalize(err.Error())+".", nil)
	}
	done := p.waitForCommand(ctx, command.Id, phpStatusWait)
	if done == nil {
		_ = p.Commands.Cancel(command.Id, "The panel stopped waiting for it.")
		return e.Error(http.StatusGatewayTimeout, "The server did not answer in time.", nil)
	}
	// a read the page makes now and then: once answered, it is no history
	defer func() {
		if err := p.App.Delete(done); err != nil {
			p.Log.Warn("failed to remove a read of a site's mail", "command", done.Id, "error", err)
		}
	}()
	if done.GetString("status") != schema.CommandSucceeded {
		return e.Error(http.StatusBadGateway, capitalize(strings.TrimSuffix(done.GetString("error"), "."))+".", nil)
	}
	var result struct {
		Sent       int           `json:"sent"`
		Failed     int           `json:"failed"`
		LastSentAt string        `json:"last_sent_at"`
		Failures   []mailFailure `json:"failures"`
		Since      string        `json:"since"`
	}
	raw, _ := json.Marshal(done.Get("result"))
	_ = json.Unmarshal(raw, &result)
	if result.Failures == nil {
		result.Failures = []mailFailure{}
	}
	return e.JSON(http.StatusOK, map[string]any{
		"relay": relay, "sent": result.Sent, "failed": result.Failed,
		"last_sent_at": result.LastSentAt, "failures": result.Failures, "since": result.Since,
	})
}
