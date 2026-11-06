package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upDomainsPage, downDomainsPage)
}

// upDomainsPage lets a certificate be asked of one of the ACME accounts, as
// the issue dialog of Domains & TLS offers (#27): empty tries the primary
// account and then the fallback one.
func upDomainsPage(app core.App) error {
	certificates, err := app.FindCollectionByNameOrId(schema.Certificates)
	if err != nil {
		return err
	}
	certificates.Fields.Add(&core.SelectField{Name: "account", MaxSelect: 1, Values: []string{schema.ACMEPrimary, schema.ACMEFallback}})
	return app.Save(certificates)
}

func downDomainsPage(app core.App) error {
	certificates, err := app.FindCollectionByNameOrId(schema.Certificates)
	if err != nil {
		return err
	}
	certificates.Fields.RemoveByName("account")
	return app.Save(certificates)
}
