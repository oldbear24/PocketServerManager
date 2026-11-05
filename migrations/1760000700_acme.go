package migrations

import (
	"slices"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upACME, downACME)
}

// certificateACMEFields are what an ACME certificate needs on top of an
// uploaded one: how it is checked, when it renews and how its attempts went.
var certificateACMEFields = []core.Field{
	// the challenge asked for: auto picks one per certificate
	&core.SelectField{Name: "challenge", MaxSelect: 1, Values: []string{schema.ChallengeAuto, schema.ChallengeHTTP01, schema.ChallengeDNS01}},
	// auto: the panel asked for it for a domain no certificate covered, and
	// renews it while a domain needs it
	&core.BoolField{Name: "auto"},
	&core.DateField{Name: "renew_after"},
	// failures in a row, and when the next attempt may start
	&core.NumberField{Name: "attempts", OnlyInt: true},
	&core.DateField{Name: "next_attempt"},
	// set while an attempt runs
	&core.DateField{Name: "attempt_started"},
	// the last attempts: when, how, and what the CA or the provider said
	&core.JSONField{Name: "history", MaxSize: 64 << 10},
}

var acmeCertStates = []string{schema.CertPending, schema.CertFailed}

// upACME adds the ACME accounts of #23, and what certificates from ACME need.
func upACME(app core.App) error {
	// accounts are set through the panel's routes, which register them at
	// their CA and seal their keys
	accounts := newCollection(schema.ACMEAccounts, access{list: schema.RuleViewer, view: schema.RuleViewer})
	accounts.Fields.Add(
		&core.SelectField{Name: "role", Required: true, MaxSelect: 1, Values: []string{schema.ACMEPrimary, schema.ACMEFallback}},
		&core.TextField{Name: "directory_url", Required: true, Max: 2048},
		&core.EmailField{Name: "email"},
		&core.TextField{Name: "key", Max: maxSealed, Hidden: true},
		&core.TextField{Name: "eab_kid", Max: 255},
		&core.TextField{Name: "eab_hmac", Max: maxSealed, Hidden: true},
		// the account's URL at the CA
		&core.TextField{Name: "uri", Max: 2048},
		&core.SelectField{Name: "status", MaxSelect: 1, Values: []string{schema.ACMEOK, schema.ACMEError}},
		&core.TextField{Name: "last_error", Max: 2048},
		&core.DateField{Name: "registered_at"},
	)
	accounts.AddIndex("idx_acme_accounts_role", true, "role", "")
	if err := app.Save(accounts); err != nil {
		return err
	}

	certificates, err := app.FindCollectionByNameOrId(schema.Certificates)
	if err != nil {
		return err
	}
	certificates.Fields.Add(certificateACMEFields...)
	if status, ok := certificates.Fields.GetByName("status").(*core.SelectField); ok {
		for _, state := range acmeCertStates {
			if !slices.Contains(status.Values, state) {
				status.Values = append(status.Values, state)
			}
		}
	}
	return app.Save(certificates)
}

func downACME(app core.App) error {
	accounts, err := app.FindCollectionByNameOrId(schema.ACMEAccounts)
	if err != nil {
		return err
	}
	if err := app.Delete(accounts); err != nil {
		return err
	}

	certificates, err := app.FindCollectionByNameOrId(schema.Certificates)
	if err != nil {
		return err
	}
	for _, field := range certificateACMEFields {
		certificates.Fields.RemoveByName(field.GetName())
	}
	if status, ok := certificates.Fields.GetByName("status").(*core.SelectField); ok {
		status.Values = slices.DeleteFunc(status.Values, func(v string) bool { return slices.Contains(acmeCertStates, v) })
	}
	return app.Save(certificates)
}
