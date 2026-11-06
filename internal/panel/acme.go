package panel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/pocketbase/ozzo-validation/v4/is"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/pocketbase/pocketbase/tools/types"
	"golang.org/x/net/publicsuffix"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/acme"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/dns"
	"github.com/oldbear24/PocketServerManager/internal/edge"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// The panel gets the certificates of the edges from an ACME CA (#23): one
// for each domain that no certificate covers, as soon as it is added, and the
// ones admins ask for, such as wildcards. Each renews 30 days before it
// expires, or a third of its lifetime for a short one; a failed attempt is
// tried again with backoff and at most acmeDailyAttempts times a day. The
// certificates reach the edges the way uploaded ones do: cert.install, then
// edge.apply.
const (
	acmeRenewBefore   = 30 * 24 * time.Hour
	acmeDailyAttempts = 5
	acmeHistoryLength = 20
	// acmeChallengeWait is how long an edge has to put an HTTP-01 answer in place.
	acmeChallengeWait = 90 * time.Second
	// acmeSettle lets the edge config of a new domain go out before the panel
	// looks at it.
	acmeSettle = time.Second
	// acmeAlertCritical is when a certificate about to expire is critical.
	acmeAlertCritical = 7 * 24 * time.Hour

	alertCertificateFailed   = "certificate_failed"
	alertCertificateExpiring = "certificate_expiring"
)

// acmeBackoff is the wait after each failure in a row.
var acmeBackoff = []time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour, 4 * time.Hour, 12 * time.Hour, 24 * time.Hour}

// noAccount: no certificate can be issued before an admin sets an account.
const noAccount = "Add an ACME account in Settings, so that PocketCloud can get certificates."

// errAccountChanged: an account was set anew while an attempt used the old one.
var errAccountChanged = errors.New("the account changed meanwhile")

// errWaiting is a reason to wait that is no failed attempt.
type errWaiting struct{ reason string }

func (e errWaiting) Error() string { return e.reason }

// acmeState is the ACME work of a running panel.
type acmeState struct {
	httpClient atomic.Pointer[http.Client]
	// propagation is how long and how often a DNS-01 answer is waited for.
	propagation atomic.Pointer[[2]time.Duration]
	// issuing holds the certificates being issued right now.
	issuing sync.Map
	// slots limits how many certificates are issued at once.
	slots    chan struct{}
	planning sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
}

func (s *acmeState) init() {
	s.slots = make(chan struct{}, 2)
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.propagation.Store(&[2]time.Duration{10 * time.Minute, 5 * time.Second})
}

// SetACMEHTTPClient sets the client the panel talks to CAs with, as tests do
// for a CA of their own; nil is lego's default.
func (p *Panel) SetACMEHTTPClient(client *http.Client) { p.acme.httpClient.Store(client) }

// SetACMEPropagation sets how long a DNS-01 answer is waited for, and how often
// the name servers are asked.
func (p *Panel) SetACMEPropagation(timeout, interval time.Duration) {
	p.acme.propagation.Store(&[2]time.Duration{timeout, interval})
}

func (p *Panel) registerACME() {
	p.App.Cron().MustAdd("pocketcloud_acme", "* * * * *", p.RunACME)

	soon := func(e *core.RecordEvent) error {
		p.later.after(acmeSettle, p.RunACME)
		return e.Next()
	}
	p.App.OnRecordAfterCreateSuccess(schema.Domains).BindFunc(soon)
	p.App.OnRecordAfterUpdateSuccess(schema.Domains).BindFunc(soon)
	// a certificate that is gone has nothing left to alert about
	p.App.OnRecordAfterDeleteSuccess(schema.Certificates).BindFunc(func(e *core.RecordEvent) error {
		p.resolveCertificateAlerts(e.Record.Id, alertCertificateFailed, alertCertificateExpiring)
		return e.Next()
	})
	// an edge that serves its new config can answer HTTP-01 now
	p.Commands.OnFinish("edge.apply", func(command *core.Record) {
		if command.GetString("status") == schema.CommandSucceeded {
			p.later.after(0, p.RunACME)
		}
	})
}

func (p *Panel) stopACME() { p.acme.cancel() }

func (p *Panel) bindACMERoutes(group *router.RouterGroup[*core.RequestEvent]) {
	operators := requireRole(schema.RoleOperator, schema.RoleAdmin)
	admins := requireRole(schema.RoleAdmin)

	group.PUT("/acme-accounts/{role}", p.handleSetACMEAccount).BindFunc(admins)
	group.DELETE("/acme-accounts/{role}", p.handleRemoveACMEAccount).BindFunc(admins)
	group.POST("/certificates/acme", p.handleRequestCertificate).BindFunc(admins)
	group.POST("/certificates/{id}/retry", p.handleRetryCertificate).BindFunc(operators)
}

func (p *Panel) acmeHTTPClient() *http.Client { return p.acme.httpClient.Load() }

// acmeAccount is an account as the panel stored it, opened.
type acmeAccount struct {
	record  *core.Record
	account *acme.Account
}

func (p *Panel) acmeAccount(role string) (*acmeAccount, error) {
	record, err := p.App.FindFirstRecordByData(schema.ACMEAccounts, "role", role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	plain, err := p.Box.OpenString(record.GetString("key"))
	if err != nil {
		return nil, fmt.Errorf("the key of the %s ACME account cannot be opened: %w", role, err)
	}
	key, err := acme.DecodeKey(plain)
	if err != nil {
		return nil, err
	}
	account := &acme.Account{
		DirectoryURL: record.GetString("directory_url"),
		Email:        record.GetString("email"),
		Key:          key,
		URI:          record.GetString("uri"),
		EABKid:       record.GetString("eab_kid"),
	}
	if sealed := record.GetString("eab_hmac"); sealed != "" {
		if account.EABHMAC, err = p.Box.OpenString(sealed); err != nil {
			return nil, fmt.Errorf("the EAB key of the %s ACME account cannot be opened: %w", role, err)
		}
	}
	return &acmeAccount{record: record, account: account}, nil
}

type acmeAccountBody struct {
	DirectoryURL string `json:"directory_url"`
	Email        string `json:"email"`
	EABKid       string `json:"eab_kid"`
	EABHMAC      string `json:"eab_hmac"`
}

func validDirectory(value any) error {
	text, _ := value.(string)
	parsed, err := url.Parse(text)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return errors.New("an ACME directory is an https:// URL")
	}
	return nil
}

// handleSetACMEAccount registers an account at its CA and keeps it. A change
// of CA, email or binding is a new account with a new key; one the CA refuses
// is not kept, and the CA's answer says why.
func (p *Panel) handleSetACMEAccount(e *core.RequestEvent) error {
	role := e.Request.PathValue("role")
	if role != schema.ACMEPrimary && role != schema.ACMEFallback {
		return e.NotFoundError("There are only a primary and a fallback ACME account.", nil)
	}
	var body acmeAccountBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid request.", err)
	}
	body.DirectoryURL = strings.TrimSpace(body.DirectoryURL)
	body.Email = strings.TrimSpace(body.Email)
	body.EABKid = strings.TrimSpace(body.EABKid)
	body.EABHMAC = strings.TrimSpace(body.EABHMAC)

	existing, _ := e.App.FindFirstRecordByData(schema.ACMEAccounts, "role", role)
	// the HMAC key is not shown again: the same key ID keeps the stored one,
	// unless a new one is typed
	storedHMAC := ""
	if existing != nil && existing.GetString("eab_hmac") != "" {
		if plain, err := p.Box.OpenString(existing.GetString("eab_hmac")); err == nil {
			storedHMAC = plain
		}
	}
	if body.EABHMAC == "" && body.EABKid != "" && existing != nil && existing.GetString("eab_kid") == body.EABKid {
		body.EABHMAC = storedHMAC
	}

	errs := validation.Errors{
		"directory_url": validation.Validate(body.DirectoryURL, validation.Required, validation.By(validDirectory)),
		"email":         validation.Validate(body.Email, validation.Required, is.EmailFormat),
	}
	if body.EABKid != "" && body.EABHMAC == "" {
		errs["eab_hmac"] = errors.New("the HMAC key belongs to the key ID")
	}
	if body.EABKid == "" && body.EABHMAC != "" {
		errs["eab_kid"] = errors.New("the key ID belongs to the HMAC key")
	}
	if body.DirectoryURL == acme.ZeroSSL && body.EABKid == "" {
		errs["eab_kid"] = errors.New("ZeroSSL needs the EAB key ID and HMAC key from its dashboard")
	}
	if err := errs.Filter(); err != nil {
		return e.BadRequestError("The ACME account is not valid.", err)
	}

	// the same account again is no new registration, if it is whole: one
	// whose key cannot be opened is registered anew
	unchanged := existing != nil && existing.GetString("uri") != "" &&
		existing.GetString("directory_url") == body.DirectoryURL && existing.GetString("email") == body.Email &&
		existing.GetString("eab_kid") == body.EABKid && body.EABHMAC == storedHMAC
	if unchanged {
		if _, err := p.acmeAccount(role); err != nil {
			unchanged = false
		}
	}
	if unchanged {
		return e.JSON(http.StatusOK, accountResponse(existing))
	}

	key, err := acme.NewKey()
	if err != nil {
		return e.InternalServerError("", err)
	}
	account := &acme.Account{DirectoryURL: body.DirectoryURL, Email: body.Email, Key: key, EABKid: body.EABKid, EABHMAC: body.EABHMAC}
	uri, err := acme.Register(account, p.acmeHTTPClient())
	if err != nil {
		return e.BadRequestError("The CA did not take the account: "+acme.Explain(err), nil)
	}
	keyPEM, err := acme.EncodeKey(key)
	if err != nil {
		return e.InternalServerError("", err)
	}

	record := existing
	if record == nil {
		collection, err := e.App.FindCachedCollectionByNameOrId(schema.ACMEAccounts)
		if err != nil {
			return e.InternalServerError("", err)
		}
		record = core.NewRecord(collection)
		record.Set("role", role)
	}
	record.Set("directory_url", body.DirectoryURL)
	record.Set("email", body.Email)
	record.Set("key", keyPEM)
	record.Set("eab_kid", body.EABKid)
	record.Set("eab_hmac", body.EABHMAC)
	record.Set("uri", uri)
	record.Set("status", schema.ACMEOK)
	record.Set("last_error", "")
	record.Set("registered_at", types.NowDateTime())
	if err := e.App.SaveWithContext(audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e)), record); err != nil {
		return e.BadRequestError("Failed to store the ACME account.", err)
	}

	// certificates that waited for an account
	p.later.after(0, p.RunACME)
	return e.JSON(http.StatusOK, accountResponse(record))
}

func accountResponse(record *core.Record) map[string]any {
	return map[string]any{
		"id": record.Id, "role": record.GetString("role"), "directory_url": record.GetString("directory_url"),
		"email": record.GetString("email"), "eab_kid": record.GetString("eab_kid"), "status": record.GetString("status"),
		"registered_at": record.GetString("registered_at"),
	}
}

func (p *Panel) handleRemoveACMEAccount(e *core.RequestEvent) error {
	record, err := e.App.FindFirstRecordByData(schema.ACMEAccounts, "role", e.Request.PathValue("role"))
	if err != nil {
		return e.NotFoundError("There is no such ACME account.", err)
	}
	if err := e.App.DeleteWithContext(audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e)), record); err != nil {
		return e.BadRequestError("Failed to remove the ACME account.", err)
	}
	// certificates asked of it alone are asked of the accounts there are, in turn (#27)
	if pinned, err := e.App.FindRecordsByFilter(schema.Certificates, "account={:r}", "", 0, 0, dbx.Params{"r": record.GetString("role")}); err == nil {
		for _, cert := range pinned {
			p.setFields(cert, map[string]any{"account": ""})
		}
	}
	return e.NoContent(http.StatusNoContent)
}

// certificateNames reads the names of a certificate record.
func certificateNames(record *core.Record) []string {
	var names []string
	_ = json.Unmarshal([]byte(record.GetString("names")), &names)
	return names
}

// checkNames normalises the names of a requested certificate.
func checkNames(names []string) ([]string, error) {
	out := []string{}
	for _, name := range names {
		name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
		if name == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(name, "*."); ok && rest != "" {
			// Chrome refuses a wildcard of a public suffix or of a top-level
			// name it does not know: *.com, *.co.uk, *.home.arpa, *.lan; CAs
			// refuse them too
			if suffix, icann := publicsuffix.PublicSuffix(rest); suffix == rest && (icann || !strings.Contains(rest, ".")) {
				return nil, fmt.Errorf("browsers do not accept a wildcard right under a public suffix or a top-level name, such as %s: name each host, such as wiki.%s", name, rest)
			}
		}
		if err := edge.ValidHostname(name); err != nil {
			return nil, err
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("a certificate needs at least one name")
	}
	if len(out) > 100 {
		return nil, errors.New("a certificate holds at most 100 names")
	}
	return out, nil
}

// handleRequestCertificate asks for a certificate that no single domain asks
// for: a wildcard, or several names in one.
func (p *Panel) handleRequestCertificate(e *core.RequestEvent) error {
	var body struct {
		Names     []string `json:"names"`
		Challenge string   `json:"challenge"`
		// Issuer is acme, the default, or internal for LAN names (#26).
		Issuer string `json:"issuer"`
		// Account asks one ACME account only, primary or fallback (#27);
		// empty tries both.
		Account string `json:"account"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid request.", err)
	}
	names, err := checkNames(body.Names)
	if err != nil {
		return e.BadRequestError(capitalize(err.Error())+".", nil)
	}
	issuer := body.Issuer
	if issuer == "" {
		issuer = schema.IssuerACME
	}
	if issuer != schema.IssuerACME && issuer != schema.IssuerInternal {
		return e.BadRequestError("The issuer is acme or internal.", nil)
	}
	if body.Account != "" {
		if issuer != schema.IssuerACME || body.Account != schema.ACMEPrimary && body.Account != schema.ACMEFallback {
			return e.BadRequestError("The account is primary or fallback, for a certificate from ACME.", nil)
		}
		if account, err := p.acmeAccount(body.Account); err != nil || account == nil {
			return e.BadRequestError(fmt.Sprintf("There is no %s ACME account: set one in Settings.", body.Account), nil)
		}
	}
	challenge := body.Challenge
	if challenge == "" {
		challenge = schema.ChallengeAuto
	}
	if !slices.Contains([]string{schema.ChallengeAuto, schema.ChallengeHTTP01, schema.ChallengeDNS01}, challenge) {
		return e.BadRequestError("The challenge is auto, http-01 or dns-01.", nil)
	}
	if issuer == schema.IssuerInternal {
		// the internal CA checks nothing, so it signs names of the LAN only
		challenge = ""
		for _, name := range names {
			if err := p.checkInternalName(name); err != nil {
				return e.BadRequestError(capitalize(err.Error())+".", nil)
			}
		}
	}
	for _, name := range names {
		if strings.HasPrefix(name, "*.") && challenge == schema.ChallengeHTTP01 {
			return e.BadRequestError(fmt.Sprintf("A wildcard (%s) can only be checked with DNS-01.", name), nil)
		}
	}

	existing, err := e.App.FindRecordsByFilter(schema.Certificates, "issuer={:issuer}", "", 0, 0, dbx.Params{"issuer": issuer})
	if err != nil {
		return e.InternalServerError("", err)
	}
	sorted := slices.Sorted(slices.Values(names))
	for _, record := range existing {
		if slices.Equal(slices.Sorted(slices.Values(certificateNames(record))), sorted) {
			return e.Error(http.StatusConflict, "There is a certificate for these names already.", map[string]any{"id": record.Id})
		}
	}

	collection, err := e.App.FindCachedCollectionByNameOrId(schema.Certificates)
	if err != nil {
		return e.InternalServerError("", err)
	}
	record := core.NewRecord(collection)
	record.Set("names", names)
	record.Set("issuer", issuer)
	record.Set("status", schema.CertPending)
	record.Set("challenge", challenge)
	record.Set("account", body.Account)
	if err := e.App.SaveWithContext(audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e)), record); err != nil {
		return e.BadRequestError("Failed to store the certificate.", err)
	}

	p.startIssue(record.Id, requesterOf(e), false)
	return e.JSON(http.StatusCreated, map[string]any{"id": record.Id, "names": names})
}

// handleRetryCertificate starts an attempt now, past the backoff but not past
// the daily limit, which keeps the CA's rate limits.
func (p *Panel) handleRetryCertificate(e *core.RequestEvent) error {
	record, err := e.App.FindRecordById(schema.Certificates, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The certificate does not exist.", err)
	}
	if issuer := record.GetString("issuer"); issuer != schema.IssuerACME && issuer != schema.IssuerInternal {
		return e.BadRequestError("Only certificates from ACME or the internal CA can be tried again; upload a new one instead.", nil)
	}
	if !needsAttempt(record, time.Now()) {
		// a new certificate each time would use up the CA's rate limits:
		// Let's Encrypt issues the same names five times a week
		return e.Error(http.StatusConflict, fmt.Sprintf("This certificate is fine; it renews after %s UTC by itself.",
			renewTime(record).UTC().Format("2006-01-02 15:04")), nil)
	}
	if until, limited := dailyLimit(record, time.Now()); limited {
		return e.Error(http.StatusTooManyRequests, fmt.Sprintf("It failed %d times in the last day; the next attempt can start at %s UTC.",
			acmeDailyAttempts, until.UTC().Format("2006-01-02 15:04")), nil)
	}
	if _, busy := p.acme.issuing.Load(record.Id); busy {
		return e.JSON(http.StatusAccepted, map[string]any{"id": record.Id, "issuing": true})
	}
	p.startIssue(record.Id, requesterOf(e), false)
	return e.JSON(http.StatusAccepted, map[string]any{"id": record.Id, "issuing": true})
}

// acmeAttempt is one entry of a certificate's history.
type acmeAttempt struct {
	At        time.Time  `json:"at"`
	OK        bool       `json:"ok"`
	Renewal   bool       `json:"renewal,omitempty"`
	Challenge string     `json:"challenge,omitempty"`
	Account   string     `json:"account,omitempty"`
	Error     string     `json:"error,omitempty"`
	NotAfter  *time.Time `json:"not_after,omitempty"`
}

func historyOf(record *core.Record) []acmeAttempt {
	var history []acmeAttempt
	_ = json.Unmarshal([]byte(record.GetString("history")), &history)
	return history
}

func withAttempt(record *core.Record, attempt acmeAttempt) []acmeAttempt {
	history := append(historyOf(record), attempt)
	if len(history) > acmeHistoryLength {
		history = history[len(history)-acmeHistoryLength:]
	}
	return history
}

// dailyLimit tells whether a certificate failed acmeDailyAttempts times in
// the last day, and when the oldest of those leaves the window.
func dailyLimit(record *core.Record, now time.Time) (time.Time, bool) {
	var failures []time.Time
	for _, attempt := range historyOf(record) {
		if !attempt.OK && now.Sub(attempt.At) < 24*time.Hour {
			failures = append(failures, attempt.At)
		}
	}
	if len(failures) < acmeDailyAttempts {
		return time.Time{}, false
	}
	slices.SortFunc(failures, func(a, b time.Time) int { return a.Compare(b) })
	return failures[len(failures)-acmeDailyAttempts].Add(24 * time.Hour), true
}

// renewTime is when an issued certificate renews.
func renewTime(record *core.Record) time.Time {
	renew := record.GetDateTime("renew_after").Time()
	if renew.IsZero() {
		renew = renewAfter(record.GetDateTime("not_before").Time(), record.GetDateTime("not_after").Time())
	}
	return renew
}

// needsAttempt tells whether a certificate needs to be issued: it never
// was, it is due to renew, or its last attempt failed or waits.
func needsAttempt(record *core.Record, now time.Time) bool {
	return record.GetString("key_pem") == "" || !now.Before(renewTime(record)) ||
		record.GetInt("attempts") > 0 || record.GetString("last_error") != ""
}

// renewAfter is when a certificate renews: 30 days before it expires, or a
// third of its lifetime before for a short one.
func renewAfter(notBefore, notAfter time.Time) time.Time {
	before := acmeRenewBefore
	if lifetime := notAfter.Sub(notBefore); lifetime/3 < before {
		before = lifetime / 3
	}
	return notAfter.Add(-before)
}

// RunACME orders the certificates that domains need and starts the
// issuances and renewals that are due. The panel runs it every minute and
// soon after a domain changes.
func (p *Panel) RunACME() {
	p.acme.planning.Lock()
	defer p.acme.planning.Unlock()
	if p.acme.ctx.Err() != nil {
		return
	}

	domains, err := p.tlsDomains()
	if err != nil {
		p.Log.Error("failed to read the domains", "error", err)
		return
	}
	p.orderCertificates(domains)
	accounts, err := p.App.CountRecords(schema.ACMEAccounts)
	if err != nil {
		return
	}

	records, err := p.App.FindRecordsByFilter(schema.Certificates, "issuer={:acme} || issuer={:internal}", "created", 0, 0,
		dbx.Params{"acme": schema.IssuerACME, "internal": schema.IssuerInternal})
	if err != nil {
		p.Log.Error("failed to read the certificates", "error", err)
		return
	}
	now := time.Now()
	for _, record := range records {
		if _, busy := p.acme.issuing.Load(record.Id); busy {
			continue
		}

		// an attempt the panel did not finish, as before a restart
		if !record.GetDateTime("attempt_started").IsZero() {
			p.setFields(record, map[string]any{"attempt_started": nil})
		}

		if !neededBy(record, domains) {
			p.dropUnneeded(record)
			continue
		}
		if !isDue(record, now) {
			continue
		}
		if next := record.GetDateTime("next_attempt").Time(); !next.IsZero() && now.Before(next) {
			continue
		}
		if until, limited := dailyLimit(record, now); limited {
			p.setFields(record, map[string]any{"next_attempt": until.UTC()})
			continue
		}
		// the internal CA needs no account
		if accounts == 0 && record.GetString("issuer") == schema.IssuerACME {
			p.acmeWaiting(record, noAccount)
			continue
		}
		p.startIssue(record.Id, "", true)
	}
}

// tlsDomains are the domains with automatic TLS.
func (p *Panel) tlsDomains() ([]*core.Record, error) {
	return p.App.FindRecordsByFilter(schema.Domains, "tls!={:off}", "", 0, 0, dbx.Params{"off": schema.TLSOff})
}

// neededBy tells whether a certificate is wanted: an admin asked for it, or a
// domain with TLS of its kind needs it.
func neededBy(record *core.Record, domains []*core.Record) bool {
	if !record.GetBool("auto") {
		return true
	}
	names := certificateNames(record)
	return slices.ContainsFunc(domains, func(d *core.Record) bool {
		return suits(record, d) && edge.Covers(names, d.GetString("hostname"))
	})
}

// isDue tells whether a certificate is to be issued now: it never was, or
// it is due to renew.
func isDue(record *core.Record, now time.Time) bool {
	return record.GetString("key_pem") == "" || !now.Before(renewTime(record))
}

// dropUnneeded lets go of a certificate that was ordered for a domain that
// is gone: an order is removed, an issued one is not renewed and has
// nothing to alert about.
func (p *Panel) dropUnneeded(record *core.Record) {
	if record.GetString("key_pem") == "" {
		if err := p.App.Delete(record); err != nil {
			p.Log.Warn("failed to remove a certificate nothing needs", "certificate", record.Id, "error", err)
		}
		return
	}
	p.resolveCertificateAlerts(record.Id, alertCertificateFailed)
}

func (p *Panel) resolveCertificateAlerts(id string, rules ...string) {
	for _, rule := range rules {
		if err := events.ResolveAlert(p.App, rule, id); err != nil {
			p.Log.Warn("failed to resolve an alert", "rule", rule, "error", err)
		}
	}
}

// orderCertificates asks for a certificate for each domain that no
// certificate of its kind covers that is valid now, unless one from ACME, or
// the internal CA for internal TLS, covers it already: its renewal is under
// way.
func (p *Panel) orderCertificates(domains []*core.Record) {
	records, err := p.App.FindAllRecords(schema.Certificates)
	if err != nil {
		p.Log.Error("failed to read the certificates", "error", err)
		return
	}
	now := time.Now()
	collection, err := p.App.FindCachedCollectionByNameOrId(schema.Certificates)
	if err != nil {
		return
	}

	for _, domain := range domains {
		hostname := domain.GetString("hostname")
		covered := false
		internal := domain.GetString("tls") == schema.TLSInternal
		for _, record := range records {
			names := certificateNames(record)
			if !edge.Covers(names, hostname) || !suits(record, domain) {
				continue
			}
			valid := record.GetString("key_pem") != "" && now.Before(record.GetDateTime("not_after").Time())
			if valid || record.GetString("issuer") == schema.IssuerACME || internal {
				covered = true
				break
			}
		}
		if covered {
			continue
		}

		record := core.NewRecord(collection)
		record.Set("names", []string{hostname})
		record.Set("status", schema.CertPending)
		record.Set("auto", true)
		if internal {
			record.Set("issuer", schema.IssuerInternal)
		} else {
			record.Set("issuer", schema.IssuerACME)
			record.Set("challenge", schema.ChallengeAuto)
		}
		if err := p.App.Save(record); err != nil {
			p.Log.Error("failed to order a certificate", "domain", hostname, "error", err)
			continue
		}
		records = append(records, record)
	}
}

// startIssue issues or renews a certificate in the background, unless that
// is under way already. A scheduled attempt checks again that it is due; one
// that someone asked for, as Retry, does not wait for the backoff.
func (p *Panel) startIssue(id, requester string, scheduled bool) {
	if _, busy := p.acme.issuing.LoadOrStore(id, struct{}{}); busy {
		return
	}
	p.later.after(0, func() {
		defer p.acme.issuing.Delete(id)
		// the internal CA signs at once: it does not wait for the CA's slots
		if record, err := p.App.FindRecordById(schema.Certificates, id); err == nil && record.GetString("issuer") == schema.IssuerInternal {
			p.issue(id, requester, scheduled)
			return
		}
		select {
		case p.acme.slots <- struct{}{}:
		case <-p.acme.ctx.Done():
			return
		}
		defer func() { <-p.acme.slots }()
		p.issue(id, requester, scheduled)
	})
}

// issue makes one attempt: the primary account, then the fallback one when
// the primary one fails or cannot be used.
func (p *Panel) issue(id, requester string, scheduled bool) {
	record, err := p.App.FindRecordById(schema.Certificates, id)
	if err != nil {
		return
	}
	names := certificateNames(record)

	// what was read to start it may have changed since
	domains, err := p.tlsDomains()
	if err != nil {
		return
	}
	if !neededBy(record, domains) {
		p.dropUnneeded(record)
		return
	}
	now := time.Now()
	if scheduled {
		if next := record.GetDateTime("next_attempt").Time(); !isDue(record, now) || !next.IsZero() && now.Before(next) {
			return
		}
		if _, limited := dailyLimit(record, now); limited {
			return
		}
	}

	if record.GetString("issuer") == schema.IssuerInternal {
		p.issueInternal(record)
		return
	}

	accounts := []*acmeAccount{}
	unusable := []string{}
	roles := []string{schema.ACMEPrimary, schema.ACMEFallback}
	if chosen := record.GetString("account"); chosen != "" {
		// asked of one CA: the other is no fallback for it
		roles = []string{chosen}
	}
	for _, role := range roles {
		account, err := p.acmeAccount(role)
		if err != nil {
			unusable = append(unusable, capitalize(err.Error())+": set the account again in Settings.")
			continue
		}
		if account != nil {
			accounts = append(accounts, account)
		}
	}
	if len(accounts) == 0 {
		if chosen := record.GetString("account"); chosen != "" && len(unusable) == 0 {
			// asked of an account that is gone: say which, and fail, so that it is seen
			p.acmeFailed(record, "", fmt.Sprintf("This certificate is asked of the %s ACME account alone, and there is none: set it in Settings, or request the certificate again.", chosen), false)
			return
		}
		if len(unusable) > 0 {
			p.acmeFailed(record, "", strings.Join(unusable, "\n"), false)
		} else {
			p.acmeWaiting(record, noAccount)
		}
		return
	}

	plan, err := p.planChallenge(names, record.GetString("challenge"))
	var waiting errWaiting
	if errors.As(err, &waiting) {
		p.acmeWaiting(record, waiting.reason)
		return
	}
	if err != nil {
		p.acmeFailed(record, plan.kind, capitalize(err.Error())+".", false)
		return
	}
	p.setFields(record, map[string]any{"attempt_started": types.NowDateTime()})

	explanations := unusable
	limited := false
	for _, account := range accounts {
		cert, err := p.obtainWith(account, names, plan, requester)
		if err == nil {
			p.acmeIssued(record, cert, plan.kind, account.record.GetString("role"))
			return
		}
		limited = limited || acme.RateLimited(err)
		explanation := acme.Explain(err)
		if hint := plan.hint(explanation); hint != "" {
			explanation += "\n" + hint
		}
		if len(accounts) > 1 {
			explanation = caName(account.record.GetString("directory_url")) + ": " + explanation
		}
		explanations = append(explanations, explanation)
		if p.acme.ctx.Err() != nil {
			break
		}
	}
	p.acmeFailed(record, plan.kind, strings.Join(explanations, "\n"), limited)
}

// caName names a CA for a person.
func caName(directory string) string {
	switch directory {
	case acme.LetsEncrypt:
		return "Let's Encrypt"
	case acme.LetsEncryptStaging:
		return "Let's Encrypt (staging)"
	case acme.ZeroSSL:
		return "ZeroSSL"
	}
	if parsed, err := url.Parse(directory); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return directory
}

func (p *Panel) obtainWith(account *acmeAccount, names []string, plan acmePlan, requester string) (*acme.Certificate, error) {
	ctx := p.acme.ctx
	req := acme.Request{Account: account.account, Names: names, HTTPClient: p.acmeHTTPClient()}
	switch plan.kind {
	case schema.ChallengeHTTP01:
		req.HTTP01 = &edgeAnswers{p: p, ctx: ctx, hosts: plan.hosts, requester: requester}
	default:
		timing := p.acme.propagation.Load()
		answers := &zoneAnswers{p: p, ctx: ctx, zones: plan.zones, timeout: timing[0], interval: timing[1]}
		req.DNS01 = answers
		req.Propagated = answers.propagated
	}
	uri := account.account.URI
	cert, err := acme.Obtain(req)
	if account.account.URI != uri {
		// registered again: the CA had forgotten the account; unless an
		// admin set another one meanwhile, which keeps its own
		err := p.update(context.Background(), account.record, func(fresh *core.Record) error {
			if fresh.GetString("key") != account.record.GetString("key") || fresh.GetString("directory_url") != account.record.GetString("directory_url") {
				return errAccountChanged
			}
			fresh.Set("uri", account.account.URI)
			fresh.Set("registered_at", types.NowDateTime())
			return nil
		})
		if err != nil && !errors.Is(err, errAccountChanged) {
			p.Log.Warn("failed to store the account's new registration", "account", account.record.Id, "error", err)
		}
	}
	return cert, err
}

// acmeWaiting records why a certificate waits, which is no attempt.
func (p *Panel) acmeWaiting(record *core.Record, reason string) {
	if record.GetString("last_error") == reason && record.GetDateTime("attempt_started").IsZero() {
		return
	}
	p.setFields(record, map[string]any{"last_error": reason, "attempt_started": nil})
}

func (p *Panel) acmeIssued(record *core.Record, cert *acme.Certificate, challenge, role string) {
	parsed, err := parseCertificate(string(cert.ChainPEM), string(cert.KeyPEM))
	if err != nil {
		p.acmeFailed(record, challenge, "The CA's certificate cannot be used: "+err.Error(), false)
		return
	}
	renewal := record.GetString("key_pem") != ""
	if domains, err := p.tlsDomains(); err == nil && !renewal && !neededBy(record, domains) {
		// its domain went away while it was issued
		p.dropUnneeded(record)
		return
	}
	notAfter := parsed.notAfter.UTC()
	err = p.update(context.Background(), record, func(fresh *core.Record) error {
		fresh.Set("chain_pem", string(parsed.chain))
		fresh.Set("key_pem", string(parsed.key))
		fresh.Set("not_before", parsed.notBefore.UTC())
		fresh.Set("not_after", notAfter)
		fresh.Set("fingerprint", parsed.fingerprint)
		fresh.Set("subject", truncateText(parsed.subject, 512))
		fresh.Set("issuer_name", truncateText(parsed.issuer, 512))
		fresh.Set("status", schema.CertActive)
		fresh.Set("renew_after", renewAfter(parsed.notBefore, parsed.notAfter).UTC())
		fresh.Set("attempts", 0)
		fresh.Set("next_attempt", nil)
		fresh.Set("attempt_started", nil)
		fresh.Set("last_error", "")
		fresh.Set("history", withAttempt(fresh, acmeAttempt{
			At: time.Now().UTC(), OK: true, Renewal: renewal, Challenge: challenge, Account: role, NotAfter: &notAfter,
		}))
		return nil
	})
	if err != nil {
		p.Log.Error("failed to store a certificate from ACME", "certificate", record.Id, "error", err)
		return
	}

	names := certificateNames(record)
	kind, verb := "certificate.issued", "Got"
	if renewal {
		kind, verb = "certificate.renewed", "Renewed"
	}
	events.Log(p.App, "", kind, schema.SeverityInfo,
		fmt.Sprintf("%s the certificate for %s, valid until %s.", verb, strings.Join(names, ", "), notAfter.Format("2006-01-02")),
		map[string]any{"certificate": record.Id, "names": strings.Join(names, ", "), "date": notAfter.Format("2006-01-02")})
	p.resolveCertificateAlerts(record.Id, alertCertificateFailed, alertCertificateExpiring)
	p.applyEdgesCovering(names, "")
}

func (p *Panel) acmeFailed(record *core.Record, challenge, message string, rateLimited bool) {
	if p.acme.ctx.Err() != nil {
		// the panel stops: no attempt
		p.acmeWaiting(record, record.GetString("last_error"))
		return
	}
	message = truncateText(message, 2048)
	attempts := record.GetInt("attempts") + 1
	delay := acmeBackoff[min(attempts, len(acmeBackoff))-1]
	if rateLimited && delay < 3*time.Hour {
		delay = 3 * time.Hour
	}
	now := time.Now().UTC()
	fields := map[string]any{
		"attempts":        attempts,
		"next_attempt":    now.Add(delay),
		"attempt_started": nil,
		"last_error":      message,
		"history":         withAttempt(record, acmeAttempt{At: now, OK: false, Renewal: record.GetString("key_pem") != "", Challenge: challenge, Error: message}),
	}
	if record.GetString("key_pem") == "" {
		fields["status"] = schema.CertFailed
	}
	p.setFields(record, fields)

	names := truncateText(strings.Join(certificateNames(record), ", "), 200)
	events.Log(p.App, "", "certificate.failed", schema.SeverityWarning,
		truncateText(fmt.Sprintf("Could not get the certificate for %s: %s", names, message), 2048),
		map[string]any{"certificate": record.Id, "names": names, "error": message})
	severity := schema.SeverityWarning
	if notAfter := record.GetDateTime("not_after").Time(); record.GetString("key_pem") != "" && time.Until(notAfter) < acmeAlertCritical {
		severity = schema.SeverityCritical
	}
	if err := events.RaiseAlert(p.App, alertCertificateFailed, record.Id, "", severity,
		truncateText(fmt.Sprintf("PocketCloud could not get the certificate for %s: %s", names, message), 2048)); err != nil {
		p.Log.Warn("failed to raise an alert", "error", err)
	}
}

// checkCertificateExpiry raises an alert for each certificate in use that
// expires within 14 days, as one whose renewals keep failing does, and
// resolves it once there is a newer one.
func (p *Panel) checkCertificateExpiry() {
	domains, err := p.tlsDomains()
	if err != nil {
		return
	}
	certs, err := p.certificates(p.App)
	if err != nil {
		return
	}
	now := time.Now()
	for _, cert := range certs {
		// in use: the one the edge serves a domain with, as buildEdgeConfig
		// picks it; one that a newer one replaced is not
		inUse := slices.ContainsFunc(domains, func(d *core.Record) bool {
			chosen := coveringCertificate(certs, d)
			return chosen != nil && chosen.record.Id == cert.record.Id
		})
		left := cert.notAfter.Sub(now)
		if !inUse || left >= expiringWithin {
			if err := events.ResolveAlert(p.App, alertCertificateExpiring, cert.record.Id); err != nil {
				p.Log.Warn("failed to resolve an alert", "error", err)
			}
			continue
		}
		severity, message := schema.SeverityWarning, fmt.Sprintf("The certificate for %s expires on %s.", strings.Join(cert.names, ", "), cert.notAfter.UTC().Format("2006-01-02"))
		if left < acmeAlertCritical {
			severity = schema.SeverityCritical
		}
		if left <= 0 {
			message = fmt.Sprintf("The certificate for %s expired on %s.", strings.Join(cert.names, ", "), cert.notAfter.UTC().Format("2006-01-02"))
		}
		if err := events.RaiseAlert(p.App, alertCertificateExpiring, cert.record.Id, "", severity, message); err != nil {
			p.Log.Warn("failed to raise an alert", "error", err)
		}
	}
}

// acmePlan is how the names of a certificate are checked.
type acmePlan struct {
	kind string
	// hosts are the edges that serve each name, for HTTP-01
	hosts map[string][]*core.Record
	// zones hold the names, for DNS-01
	zones map[string]acmeZone
}

// hint says what to look at when the CA could not check an answer.
func (plan acmePlan) hint(explanation string) string {
	lower := strings.ToLower(explanation)
	switch {
	case plan.kind == schema.ChallengeHTTP01 && (strings.Contains(lower, "(connection)") || strings.Contains(lower, "(unauthorized)") || strings.Contains(lower, "(incorrectresponse)")):
		return "The CA fetches the answer from http://<name>/.well-known/acme-challenge/ on port 80: check that the name points at the edge's public address and that port 80 is open."
	case plan.kind == schema.ChallengeDNS01 && (strings.Contains(lower, "(unauthorized)") || strings.Contains(lower, "(dns)")):
		return "The CA looks for a TXT record at _acme-challenge.<name>: check that the zone's name servers are those of the connected provider."
	}
	return ""
}

type acmeZone struct {
	name     string
	provider dns.Provider
	driver   dns.Driver
	label    string
}

// planChallenge picks how a certificate's names are checked. Auto takes
// HTTP-01 when every name is served by an edge with a public address, and
// DNS-01 for wildcards and names an edge cannot answer for, when their zones
// are managed through a provider.
func (p *Panel) planChallenge(names []string, wanted string) (acmePlan, error) {
	domains, err := p.App.FindAllRecords(schema.Domains)
	if err != nil {
		return acmePlan{}, err
	}
	zoneRecords, err := p.App.FindRecordsByFilter(schema.DNSZones, "managed=true", "", 0, 0)
	if err != nil {
		return acmePlan{}, err
	}
	zoneNames := make([]string, 0, len(zoneRecords))
	byName := map[string]*core.Record{}
	for _, zone := range zoneRecords {
		// a DNS server of the LAN holds no TXT records, and the CA cannot ask
		// it: the zone around it answers DNS-01, as for split horizon
		if provider, err := p.App.FindRecordById(schema.DNSProviders, zone.GetString("provider")); err == nil && lanDriver(provider) {
			continue
		}
		zoneNames = append(zoneNames, zone.GetString("name"))
		byName[zone.GetString("name")] = zone
	}

	plan := acmePlan{hosts: map[string][]*core.Record{}, zones: map[string]acmeZone{}}
	zoneOf := map[string]*core.Record{}
	// how each name can be checked
	canHTTP, canDNS := map[string]bool{}, map[string]bool{}
	domainOf := map[string]*core.Record{}
	wildcard, allHTTP, allPublic, allDNS := false, true, true, true

	for _, name := range names {
		base := strings.TrimPrefix(name, "*.")
		if base != name {
			wildcard = true
		} else {
			for _, domain := range domains {
				if domain.GetString("hostname") != name {
					continue
				}
				host, err := p.App.FindRecordById(schema.Hosts, domain.GetString("host"))
				if err != nil || !hasEdgeRole(host) {
					continue
				}
				domainOf[name] = domain
				plan.hosts[name] = append(plan.hosts[name], host)
				if host.GetString("public_ip4") == "" && host.GetString("public_ip6") == "" {
					allPublic = false
				}
			}
			canHTTP[name] = len(plan.hosts[name]) > 0
		}
		allHTTP = allHTTP && canHTTP[name]

		zone, ok := dns.ZoneFor(base, zoneNames)
		if ok {
			provider, err := p.App.FindRecordById(schema.DNSProviders, byName[zone].GetString("provider"))
			if err != nil {
				ok = false
			} else if driver, known := dns.Lookup(provider.GetString("kind")); !known || driver.Manual {
				ok = false
			}
		}
		canDNS[name] = ok
		allDNS = allDNS && ok
		if ok {
			zoneOf[base] = byName[zone]
		}
	}

	first := func(test func(name string) bool) string {
		for _, name := range names {
			if test(name) {
				return name
			}
		}
		return ""
	}
	noWay := first(func(name string) bool { return !canHTTP[name] && !canDNS[name] })
	cannotHTTP := first(func(name string) bool { return !canHTTP[name] })
	cannotDNS := first(func(name string) bool { return !canDNS[name] })

	switch wanted {
	case schema.ChallengeHTTP01:
		plan.kind = schema.ChallengeHTTP01
	case schema.ChallengeDNS01:
		plan.kind = schema.ChallengeDNS01
	default:
		switch {
		case noWay != "" && strings.HasPrefix(noWay, "*."):
			return plan, fmt.Errorf("a wildcard (%s) can only be checked with DNS-01, and its zone is in no connected DNS provider that PocketCloud manages", noWay)
		case noWay != "":
			return plan, fmt.Errorf("no edge serves %s and its zone is in no connected DNS provider that PocketCloud manages, so the CA cannot check it", noWay)
		case !allHTTP && !allDNS:
			return plan, fmt.Errorf("%s can only be checked with DNS-01 and %s only with HTTP-01: ask for them in separate certificates", cannotHTTP, cannotDNS)
		case wildcard || allDNS && (!allHTTP || !allPublic):
			plan.kind = schema.ChallengeDNS01
		default:
			plan.kind = schema.ChallengeHTTP01
		}
	}

	switch plan.kind {
	case schema.ChallengeHTTP01:
		if cannotHTTP != "" {
			if strings.HasPrefix(cannotHTTP, "*.") {
				return plan, fmt.Errorf("a wildcard (%s) can only be checked with DNS-01", cannotHTTP)
			}
			return plan, fmt.Errorf("no edge serves %s, so there is nowhere to answer the CA's HTTP-01 challenge", cannotHTTP)
		}
		for _, name := range names {
			for _, host := range plan.hosts[name] {
				applied := host.GetString("edge_applied_digest")
				if !host.GetBool("stream_open") || applied == "" || applied != host.GetString("edge_digest") {
					return plan, errWaiting{fmt.Sprintf("Waiting for the edge of %s to serve %s.", host.GetString("name"), name)}
				}
			}
			// the CA would find no address, or the old one, and the failure
			// would count; the records PocketCloud just set are checked every
			// minute
			if domain := domainOf[name]; domain != nil && domain.GetString("dns_status") == schema.DNSPending {
				return plan, errWaiting{fmt.Sprintf("Waiting for the DNS records of %s to be visible.", name)}
			}
		}
	case schema.ChallengeDNS01:
		if cannotDNS != "" {
			return plan, fmt.Errorf("%s is in no zone of a connected DNS provider that PocketCloud manages, so there is nowhere to answer the CA's DNS-01 challenge", cannotDNS)
		}
		opened := map[string]acmeZone{}
		for base, zone := range zoneOf {
			if found, ok := opened[zone.Id]; ok {
				plan.zones[base] = found
				continue
			}
			provider, err := p.App.FindRecordById(schema.DNSProviders, zone.GetString("provider"))
			if err != nil {
				return plan, err
			}
			driver, client, err := p.openProvider(provider)
			if err != nil {
				return plan, fmt.Errorf("%s cannot be used: %s", provider.GetString("name"), driver.Explain(err))
			}
			opened[zone.Id] = acmeZone{name: zone.GetString("name"), provider: client, driver: driver, label: provider.GetString("name")}
			plan.zones[base] = opened[zone.Id]
		}
	}
	return plan, nil
}

// edgeAnswers answers HTTP-01 challenges with files on the edges that serve
// the names.
type edgeAnswers struct {
	p         *Panel
	ctx       context.Context
	hosts     map[string][]*core.Record
	requester string
}

func (a *edgeAnswers) Present(domain, token, keyAuth string) error {
	hosts := a.hosts[domain]
	if len(hosts) == 0 {
		return fmt.Errorf("no edge serves %s", domain)
	}
	queued := make([]*core.Record, 0, len(hosts))
	for _, host := range hosts {
		command, err := a.p.Commands.Create(a.ctx, commands.Options{
			Host: host.Id,
			Command: &agentv1.Command{Payload: &agentv1.Command_AcmeChallengePresent{AcmeChallengePresent: &agentv1.AcmeChallengePresent{
				Token: token, KeyAuthorization: keyAuth,
			}}},
			Label:     "Answer the ACME challenge for " + domain,
			Requester: a.requester,
		})
		if err != nil {
			_ = a.CleanUp(domain, token, keyAuth)
			return err
		}
		queued = append(queued, command)
	}
	for i, command := range queued {
		done := a.p.waitForCommand(a.ctx, command.Id, acmeChallengeWait)
		if done == nil || done.GetString("status") != schema.CommandSucceeded {
			reason := "it did not answer in time"
			if done != nil && done.GetString("error") != "" {
				reason = done.GetString("error")
			}
			// lego cleans up only after an answer that is in place
			_ = a.CleanUp(domain, token, keyAuth)
			return fmt.Errorf("the edge of %s could not put the answer in place: %s", hosts[i].GetString("name"), reason)
		}
	}
	return nil
}

func (a *edgeAnswers) CleanUp(domain, token, _ string) error {
	for _, host := range a.hosts[domain] {
		_, err := a.p.Commands.Create(context.Background(), commands.Options{
			Host:      host.Id,
			Command:   &agentv1.Command{Payload: &agentv1.Command_AcmeChallengeCleanup{AcmeChallengeCleanup: &agentv1.AcmeChallengeCleanup{Token: token}}},
			Label:     "Remove the ACME challenge answer for " + domain,
			Requester: a.requester,
		})
		if err != nil {
			a.p.Log.Warn("failed to queue the removal of a challenge answer", "host", host.Id, "error", err)
		}
	}
	return nil
}

// waitForCommand waits until a command finishes; nil when it did not in time.
func (p *Panel) waitForCommand(ctx context.Context, id string, limit time.Duration) *core.Record {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		record, err := p.App.FindRecordById(schema.Commands, id)
		if err == nil {
			switch record.GetString("status") {
			case schema.CommandSucceeded, schema.CommandFailed, schema.CommandTimedOut, schema.CommandCancelled:
				return record
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// zoneAnswers answers DNS-01 challenges with TXT records at the providers.
type zoneAnswers struct {
	p                 *Panel
	ctx               context.Context
	zones             map[string]acmeZone
	timeout, interval time.Duration
}

func (a *zoneAnswers) record(domain, keyAuth string) (acmeZone, dns.Record, error) {
	// lego names a wildcard's challenge after the name without "*."; its
	// propagation check, after the name with it
	domain = strings.TrimPrefix(domain, "*.")
	zone, ok := a.zones[domain]
	if !ok {
		return zone, dns.Record{}, fmt.Errorf("%s is in no managed zone", domain)
	}
	info := acme.ChallengeInfo(domain, keyAuth)
	return zone, dns.Record{Name: dns.RelativeName(info.FQDN, zone.name), Type: "TXT", Value: info.Value, TTL: 60}, nil
}

func (a *zoneAnswers) Present(domain, _, keyAuth string) error {
	zone, record, err := a.record(domain, keyAuth)
	if err != nil {
		return err
	}
	if err := dns.Append(a.ctx, zone.provider, zone.name, []dns.Record{record}); err != nil {
		return fmt.Errorf("%s did not take the TXT record: %s", zone.label, zone.driver.Explain(err))
	}
	return nil
}

func (a *zoneAnswers) CleanUp(domain, _, keyAuth string) error {
	zone, record, err := a.record(domain, keyAuth)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := dns.Delete(ctx, zone.provider, zone.name, []dns.Record{record}); err != nil {
		a.p.Log.Warn("failed to remove a DNS-01 answer", "zone", zone.name, "error", err)
	}
	return nil
}

// Timeout is how long lego waits for the answer to be visible.
func (a *zoneAnswers) Timeout() (time.Duration, time.Duration) { return a.timeout, a.interval }

// propagated asks the zone's name servers for the answer; a panel that stops
// ends the wait.
func (a *zoneAnswers) propagated(domain, fqdn, value string) (bool, error) {
	if err := a.ctx.Err(); err != nil {
		return true, err
	}
	zone, ok := a.zones[strings.TrimPrefix(domain, "*.")]
	if !ok {
		return true, fmt.Errorf("%s is in no managed zone", domain)
	}
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	return a.p.checker().Visible(ctx, zone.name, fqdn, "TXT", value)
}
