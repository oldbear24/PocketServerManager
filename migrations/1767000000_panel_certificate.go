package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upPanelCertificate, downPanelCertificate)
}

// upPanelCertificate marks the certificate the panel serves its own UI and API
// with (#63): one from ACME for its public name, which no edge gets.
func upPanelCertificate(app core.App) error {
	certificates, err := app.FindCollectionByNameOrId(schema.Certificates)
	if err != nil {
		return err
	}
	certificates.Fields.Add(&core.BoolField{Name: "panel"})
	return app.Save(certificates)
}

func downPanelCertificate(app core.App) error {
	certificates, err := app.FindCollectionByNameOrId(schema.Certificates)
	if err != nil {
		return err
	}
	certificates.Fields.RemoveByName("panel")
	return app.Save(certificates)
}
