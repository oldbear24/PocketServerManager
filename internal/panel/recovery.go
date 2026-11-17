package panel

import (
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/pocketbase/pocketbase/tools/types"
	"golang.org/x/text/unicode/norm"

	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/recoverykit"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/version"
)

// The recovery kit (#32): the master key, and what reaches and opens the
// panel's backup, sealed with a passphrase (internal/recoverykit). An admin
// downloads it, keeps it off the panel's machine, and checks it by uploading
// it back: a kit counts as saved only once a check opened it. The panel
// keeps no kit, only its id and the digests of what it holds, which say
// whether a kit is still current. See docs/architecture.md, "Panel backup
// and disaster recovery (#32)".

// The kits' statuses.
const (
	kitNever      = "never"
	kitUnverified = "unverified"
	kitStale      = "stale"
	kitSaved      = "saved"
)

const (
	// kitAttemptLimit attempts to download or check a kit, per user, in
	// kitAttemptWindow.
	kitAttemptLimit  = 5
	kitAttemptWindow = 15 * time.Minute
	// keptKits is how many kits the panel lists.
	keptKits = 20
	// kitBodyLimit is the most a check's body takes: a kit and a passphrase
	// as JSON.
	kitBodyLimit = recoverykit.MaxSize + 8<<10
)

// kitAttempts limits the attempts at the kit's routes, per user: each
// download or check takes an argon2 of 64 MiB, and a download needs the
// sign-in password.
type kitAttempts struct {
	mu sync.Mutex
	at map[string][]time.Time
}

// take counts an attempt of a user, or says how long until one is allowed.
func (k *kitAttempts) take(user string, now time.Time) (time.Duration, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.at == nil {
		k.at = map[string][]time.Time{}
	}
	recent := slices.DeleteFunc(k.at[user], func(at time.Time) bool { return now.Sub(at) >= kitAttemptWindow })
	if len(recent) >= kitAttemptLimit {
		k.at[user] = recent
		return recent[0].Add(kitAttemptWindow).Sub(now), false
	}
	k.at[user] = append(recent, now)
	return 0, true
}

func (p *Panel) bindRecoveryRoutes(group *router.RouterGroup[*core.RequestEvent]) {
	admins := requireRole(schema.RoleAdmin)
	group.GET("/recovery", p.handleRecovery).BindFunc(admins)
	group.POST("/recovery/kit", p.handleExportKit).BindFunc(admins)
	group.POST("/recovery/kit/check", p.handleCheckKit).BindFunc(admins)
}

// kitTarget is a target of the panel's job, opened.
type kitTarget struct {
	record *core.Record
	kit    recoverykit.Target
	err    error
}

// panelJob is the panel's backup job, or nil.
func (p *Panel) panelJob() (*core.Record, error) {
	jobs, err := p.App.FindAllRecords(schema.BackupJobs, dbx.HashExp{"source_kind": schema.SourcePanel})
	if err != nil || len(jobs) == 0 {
		return nil, err
	}
	return jobs[0], nil
}

// panelJobTargets opens the targets of the panel's job, in the job's order:
// each repository as the enricher delivers it to the carrier.
func (p *Panel) panelJobTargets(job *core.Record) ([]kitTarget, error) {
	var targets []kitTarget
	for _, id := range job.GetStringSlice("targets") {
		record, err := p.App.FindRecordById(schema.BackupTargets, id)
		if err != nil {
			continue
		}
		target := kitTarget{record: record, kit: recoverykit.Target{ID: record.Id, Name: record.GetString("name"), Kind: record.GetString("kind")}}
		if target.kit.Kind == backup.KindLocal {
			if host, err := p.App.FindRecordById(schema.Hosts, record.GetString("host")); err == nil {
				target.kit.Server = host.GetString("name")
			}
		}
		opened, err := p.targetSecretsOf(record)
		if err == nil {
			var located backup.Repository
			located, err = backup.Locate(opened.kind, opened.config, opened.creds)
			target.kit.Repository = recoverykit.Repository{URL: located.URL, Password: opened.password, Env: located.Env,
				Options: located.Options, SSHKey: located.SSHKey, KnownHosts: located.KnownHosts, Mount: located.Mount}
		}
		target.err = err
		targets = append(targets, target)
	}
	return targets, nil
}

// startEnv is the panel's effective start environment, as a restored panel
// is started with: never a secret.
func (p *Panel) startEnv() map[string]string {
	env := map[string]string{}
	for name, value := range map[string]string{
		EnvPublicURL: p.Config.PublicURL, EnvGRPCAddr: p.Config.GRPCAddr, EnvGRPCPublic: p.Config.GRPCPublicAddr,
		EnvPHPImage: p.Config.PHPImage, EnvHTTPSAddr: p.Config.HTTPSAddr, EnvHTTPSChallenge: p.Config.HTTPSChallenge,
	} {
		if value != "" {
			env[name] = value
		}
	}
	return env
}

// dialAddresses are the addresses the servers' agents dial, as their streams
// said, with how many dial each; and how many servers have not said yet.
func (p *Panel) dialAddresses() ([]recoverykit.Dial, int, error) {
	hosts, err := p.App.FindAllRecords(schema.Hosts)
	if err != nil {
		return nil, 0, err
	}
	counts := map[string]int{}
	unreported := 0
	for _, host := range hosts {
		if address := host.GetString("panel_address"); address != "" {
			counts[address]++
		} else {
			unreported++
		}
	}
	dials := []recoverykit.Dial{}
	for address, servers := range counts {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			host = address
		}
		dials = append(dials, recoverykit.Dial{Address: address, Servers: servers, IP: net.ParseIP(host) != nil})
	}
	slices.SortFunc(dials, func(a, b recoverykit.Dial) int { return strings.Compare(a.Address, b.Address) })
	return dials, unreported, nil
}

// currentKit is the panel now as a kit's payload, and the names of its
// job's targets. Strict, a target whose secrets do not open fails it;
// otherwise that target's repository is left empty, which no kit holds.
func (p *Panel) currentKit(strict bool) (recoverykit.Payload, []string, error) {
	payload := recoverykit.Payload{
		KeyID: secrets.KeyID(p.masterKey), MasterKey: hex.EncodeToString(p.masterKey),
		PanelVersion: version.Version, AgentImage: version.AgentRef(), Env: p.startEnv(),
	}
	if authority := p.authority.Load(); authority != nil {
		payload.CAPin = authority.Pin()
	}
	dials, _, err := p.dialAddresses()
	if err != nil {
		return payload, nil, err
	}
	payload.AgentsDial = dials
	job, err := p.panelJob()
	if err != nil || job == nil {
		return payload, []string{}, err
	}
	payload.Job = &recoverykit.Job{ID: job.Id, Name: job.GetString("name")}
	targets, err := p.panelJobTargets(job)
	if err != nil {
		return payload, nil, err
	}
	names := []string{}
	for _, target := range targets {
		if target.err != nil && strict {
			return payload, nil, &targetSecretsError{name: target.kit.Name, err: target.err}
		}
		payload.Targets = append(payload.Targets, target.kit)
		names = append(names, target.kit.Name)
	}
	return payload, names, nil
}

// targetSecretsError is a target of the panel's job whose secrets do not
// open: a kit made now would not reach it.
type targetSecretsError struct {
	name string
	err  error
}

func (e *targetSecretsError) Error() string {
	return fmt.Sprintf("The secrets of the target %s cannot be opened: %v.", e.name, e.err)
}

// currentDigests are the digests of the panel as it is now.
func (p *Panel) currentDigests() (map[string]string, error) {
	payload, _, err := p.currentKit(false)
	if err != nil {
		return nil, err
	}
	return recoverykit.Digests(payload)
}

// kitState is a kit's status as the API says it.
type kitState struct {
	Status     string   `json:"status"`
	Reasons    []string `json:"reasons"`
	KitID      string   `json:"kit_id"`
	Created    string   `json:"created"`
	CreatedBy  string   `json:"created_by"`
	VerifiedAt string   `json:"verified_at"`
	VerifiedBy string   `json:"verified_by"`
}

func digestsOf(record *core.Record) map[string]string {
	digests := map[string]string{}
	_ = record.UnmarshalJSONField("digests", &digests)
	return digests
}

// kitStatus says whether a kit is saved: the newest checked kit's digests
// are current. Otherwise the newest kit is unverified while it is current,
// and stale, naming the sections that changed, when it is not.
func (p *Panel) kitStatus(current map[string]string) (kitState, error) {
	kits, err := p.App.FindRecordsByFilter(schema.RecoveryKits, "", "-created", keptKits, 0)
	if err != nil {
		return kitState{}, err
	}
	state := func(record *core.Record, status string, reasons []string) kitState {
		if reasons == nil {
			reasons = []string{}
		}
		return kitState{Status: status, Reasons: reasons, KitID: record.GetString("kit_id"),
			Created: record.GetString("created"), CreatedBy: record.GetString("created_by"),
			VerifiedAt: record.GetString("verified_at"), VerifiedBy: record.GetString("verified_by")}
	}
	if len(kits) == 0 {
		return kitState{Status: kitNever, Reasons: []string{}}, nil
	}
	for _, kit := range kits {
		if !kit.GetDateTime("verified_at").IsZero() && len(recoverykit.Changed(digestsOf(kit), current)) == 0 {
			return state(kit, kitSaved, nil), nil
		}
	}
	newest := kits[0]
	if reasons := recoverykit.Changed(digestsOf(newest), current); len(reasons) > 0 {
		return state(newest, kitStale, reasons), nil
	}
	return state(newest, kitUnverified, nil), nil
}

// keepNewestKits removes all but the newest kits from the list.
func (p *Panel) keepNewestKits() {
	old, err := p.App.FindRecordsByFilter(schema.RecoveryKits, "", "-created", 0, keptKits)
	if err != nil {
		p.Log.Error("failed to list old recovery kits", "error", err)
		return
	}
	for _, kit := range old {
		if err := p.App.Delete(kit); err != nil {
			p.Log.Error("failed to remove an old recovery kit", "kit", kit.GetString("kit_id"), "error", err)
		}
	}
}

// panelJobInfo is the panel's job as GET /api/pc/recovery shows it.
func (p *Panel) panelJobInfo() (map[string]any, error) {
	job, err := p.panelJob()
	if err != nil || job == nil {
		return nil, err
	}
	carrier := job.GetString("host")
	carrierName := ""
	if host, err := p.App.FindRecordById(schema.Hosts, carrier); err == nil {
		carrierName = host.GetString("name")
	}
	names := []string{}
	for _, id := range job.GetStringSlice("targets") {
		if target, err := p.App.FindRecordById(schema.BackupTargets, id); err == nil {
			names = append(names, target.GetString("name"))
		}
	}
	last, err := p.lastPanelSuccess(job)
	if err != nil {
		return nil, err
	}
	lastSuccess := ""
	if !last.IsZero() {
		lastSuccess = last.String()
	}
	return map[string]any{"id": job.Id, "name": job.GetString("name"), "enabled": job.GetBool("enabled"), "carrier": carrier,
		"carrier_name": carrierName, "targets": names, "last_success_at": lastSuccess}, nil
}

// lastPanelSuccess is when a run of the panel's job last succeeded.
func (p *Panel) lastPanelSuccess(job *core.Record) (types.DateTime, error) {
	runs, err := p.App.FindRecordsByFilter(schema.BackupRuns, "job = {:job} && status = {:succeeded}", "-finished_at", 1, 0,
		dbx.Params{"job": job.Id, "succeeded": schema.RunSucceeded})
	if err != nil || len(runs) == 0 {
		return types.DateTime{}, err
	}
	return runs[0].GetDateTime("finished_at"), nil
}

// restoredInfo is the restore GET /api/pc/recovery shows; nil when the
// panel was never restored.
func (p *Panel) restoredInfo() (map[string]any, error) {
	state, err := readPanelState(p.App.DB())
	if err != nil || state.RestoredAt == "" {
		return nil, err
	}
	var from restoreMarker
	_ = json.Unmarshal([]byte(state.RestoredFrom), &from)
	back, total, err := p.serversBack(state.RestoredAt)
	if err != nil {
		return nil, err
	}
	return map[string]any{"at": state.RestoredAt, "from": from.ExportCreated, "snapshot": from.Snapshot, "back": back, "total": total}, nil
}

func (p *Panel) handleRecovery(e *core.RequestEvent) error {
	current, err := p.currentDigests()
	if err != nil {
		return e.InternalServerError("Failed to read the panel's state.", err)
	}
	kit, err := p.kitStatus(current)
	if err != nil {
		return e.InternalServerError("Failed to read the recovery kits.", err)
	}
	dials, unreported, err := p.dialAddresses()
	if err != nil {
		return e.InternalServerError("Failed to read the servers.", err)
	}
	job, err := p.panelJobInfo()
	if err != nil {
		return e.InternalServerError("Failed to read the panel's backup job.", err)
	}
	restored, err := p.restoredInfo()
	if err != nil {
		return e.InternalServerError("Failed to read the panel's restore.", err)
	}
	env := p.startEnv()
	pin := ""
	if authority := p.authority.Load(); authority != nil {
		pin = authority.Pin()
		env[EnvExpectCAPin] = pin
	}
	answer := map[string]any{
		"key_id": secrets.KeyID(p.masterKey), "ca_pin": pin, "kit": kit, "addresses": dials, "unreported": unreported,
		"start_env": env, "panel_job": nil, "restored": nil,
	}
	// null, not an empty object, when there is none
	if job != nil {
		answer["panel_job"] = job
	}
	if restored != nil {
		answer["restored"] = restored
	}
	return e.JSON(http.StatusOK, answer)
}

// attempt counts an attempt at the kit's routes, or answers 429.
func (p *Panel) attempt(e *core.RequestEvent) error {
	user := e.Auth.Collection().Name + ":" + e.Auth.Id
	if wait, ok := p.kitAttempts.take(user, time.Now()); !ok {
		minutes := int((wait + time.Minute - 1) / time.Minute)
		return e.Error(http.StatusTooManyRequests, fmt.Sprintf("Too many attempts: try again in %d minutes.", minutes), nil)
	}
	return nil
}

// panelAddressOf is the panel's address a kit names: its public URL, or
// the one the request came to.
func (p *Panel) panelAddressOf(e *core.RequestEvent) string {
	if p.Config.PublicURL != "" {
		return p.Config.PublicURL
	}
	scheme := "http"
	if e.Request.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + e.Request.Host
}

func (p *Panel) handleExportKit(e *core.RequestEvent) error {
	e.Request.Body = http.MaxBytesReader(e.Response, e.Request.Body, 8<<10)
	var body struct {
		Passphrase string `json:"passphrase"`
		Password   string `json:"password"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", nil)
	}
	if err := p.attempt(e); err != nil {
		return err
	}
	if !e.Auth.ValidatePassword(body.Password) {
		return e.BadRequestError("Your password is not right.", nil)
	}
	passphrase, err := recoverykit.Passphrase(body.Passphrase)
	switch {
	case errors.Is(err, recoverykit.ErrShortPassphrase):
		return e.BadRequestError(fmt.Sprintf("The passphrase needs at least %d characters.", recoverykit.MinPassphrase), nil)
	case errors.Is(err, recoverykit.ErrLongPassphrase):
		return e.BadRequestError(fmt.Sprintf("The passphrase is at most %d bytes.", recoverykit.MaxPassphrase), nil)
	case err != nil:
		return e.BadRequestError("Invalid passphrase.", nil)
	}
	if passphrase == norm.NFC.String(body.Password) {
		return e.BadRequestError("The passphrase cannot be your sign-in password.", nil)
	}

	// one argon2 at a time
	select {
	case p.kitSealing <- struct{}{}:
		defer func() { <-p.kitSealing }()
	default:
		return e.Error(http.StatusServiceUnavailable, "Another recovery kit is being sealed: try again in a moment.", nil)
	}

	payload, names, err := p.currentKit(true)
	if refused := (*targetSecretsError)(nil); errors.As(err, &refused) {
		return e.Error(http.StatusConflict, refused.Error(), nil)
	}
	if err != nil {
		return e.InternalServerError("Failed to read the panel's state.", err)
	}
	kitID, err := recoverykit.NewKitID()
	if err != nil {
		return e.InternalServerError("Failed to make the kit.", err)
	}
	header := recoverykit.Header{Kit: kitID, Created: time.Now().UTC(), Panel: p.panelAddressOf(e), KeyID: payload.KeyID,
		CAPin: payload.CAPin, PanelVersion: payload.PanelVersion, AgentImage: payload.AgentImage}
	payload.CreatedBy = e.Auth.Email()
	text, err := recoverykit.Seal(header, payload, body.Passphrase)
	if err != nil {
		// never the payload or the passphrase in the log
		return e.InternalServerError("Failed to seal the kit.", errors.New("sealing the recovery kit failed"))
	}
	digests, err := recoverykit.Digests(payload)
	if err != nil {
		return e.InternalServerError("Failed to seal the kit.", nil)
	}

	collection, err := p.App.FindCollectionByNameOrId(schema.RecoveryKits)
	if err != nil {
		return e.InternalServerError("Failed to record the kit.", err)
	}
	row := core.NewRecord(collection)
	row.Load(map[string]any{"kit_id": kitID, "key_id": payload.KeyID, "digests": digests, "created_by": payload.CreatedBy})
	if err := p.App.Save(row); err != nil {
		return e.InternalServerError("Failed to record the kit.", err)
	}
	p.keepNewestKits()

	addresses := []string{}
	for _, dial := range payload.AgentsDial {
		addresses = append(addresses, dial.Address)
	}
	job := ""
	if payload.Job != nil {
		job = payload.Job.ID
	}
	p.Audit.Security(audit.ActionRecoveryKitExported, audit.ActorFromRequest(e), kitID,
		map[string]any{"key_id": payload.KeyID, "job": job, "targets": names, "addresses": addresses})
	p.evaluateRecoveryAlerts()

	e.Response.Header().Set("Cache-Control", "no-store")
	return e.JSON(http.StatusOK, map[string]any{"kit_id": kitID, "file_name": recoverykit.FileName(header), "content": text})
}

func (p *Panel) handleCheckKit(e *core.RequestEvent) error {
	e.Request.Body = http.MaxBytesReader(e.Response, e.Request.Body, kitBodyLimit)
	var body struct {
		Kit        string `json:"kit"`
		Passphrase string `json:"passphrase"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("This is no PocketCloud recovery kit.", nil)
	}
	if err := p.attempt(e); err != nil {
		return err
	}

	select {
	case p.kitSealing <- struct{}{}:
		defer func() { <-p.kitSealing }()
	case <-e.Request.Context().Done():
		return e.Request.Context().Err()
	}
	_, payload, err := recoverykit.Open(body.Kit, body.Passphrase)
	switch {
	case errors.Is(err, recoverykit.ErrPassphrase):
		return e.BadRequestError("The passphrase does not open this kit, or the kit was changed.", nil)
	case err != nil:
		return e.BadRequestError("This is no PocketCloud recovery kit.", nil)
	}
	key, err := payload.Key()
	if err != nil || subtle.ConstantTimeCompare(key, p.masterKey) != 1 {
		return e.Error(http.StatusConflict, fmt.Sprintf("This kit holds another master key (id %s): it is not this panel's.", payload.KeyID), nil)
	}

	kitDigests, err := recoverykit.Digests(payload)
	if err != nil {
		return e.InternalServerError("Failed to check the kit.", nil)
	}
	current, err := p.currentDigests()
	if err != nil {
		return e.InternalServerError("Failed to read the panel's state.", err)
	}
	reasons := recoverykit.Changed(kitDigests, current)
	if reasons == nil {
		reasons = []string{}
	}
	if len(reasons) == 0 {
		if err := p.kitChecked(e, payload, kitDigests); err != nil {
			return e.InternalServerError("Failed to record the check.", err)
		}
	}
	status, err := p.kitStatus(current)
	if err != nil {
		return e.InternalServerError("Failed to read the recovery kits.", err)
	}
	return e.JSON(http.StatusOK, map[string]any{"kit_id": payload.KitID, "current": len(reasons) == 0, "reasons": reasons, "status": status.Status})
}

// kitChecked records that a current kit opened: its row, made if the panel
// has none, as one restored from before the kit, is verified.
func (p *Panel) kitChecked(e *core.RequestEvent, payload recoverykit.Payload, digests map[string]string) error {
	row, err := p.App.FindFirstRecordByData(schema.RecoveryKits, "kit_id", payload.KitID)
	if err != nil {
		collection, err := p.App.FindCollectionByNameOrId(schema.RecoveryKits)
		if err != nil {
			return err
		}
		row = core.NewRecord(collection)
		row.Load(map[string]any{"kit_id": payload.KitID, "key_id": payload.KeyID, "digests": digests, "created_by": payload.CreatedBy})
	}
	row.Set("verified_at", types.NowDateTime())
	row.Set("verified_by", e.Auth.Email())
	if err := p.App.Save(row); err != nil {
		return err
	}
	p.keepNewestKits()
	p.Audit.Security(audit.ActionRecoveryKitChecked, audit.ActorFromRequest(e), payload.KitID, map[string]any{"key_id": payload.KeyID})
	p.evaluateRecoveryAlerts()
	return nil
}
