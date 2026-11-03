package panel

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

var errSetupDone = errors.New("an account already exists: sign in instead")

// prepareFirstRun creates the token that allows creating the first admin account,
// if there is no account yet. It is only ever printed to the panel's log, so only
// whoever runs the panel can use it.
func (p *Panel) prepareFirstRun() error {
	p.hintAboutSuperuser()

	needed, err := p.setupNeeded()
	if err != nil || !needed {
		return err
	}

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	p.setupToken = hex.EncodeToString(raw)

	base := p.Config.PublicURL
	if base == "" {
		base = "http://<this panel>"
	}
	p.Log.Info("no user account exists yet: create the first admin at " + base + "/setup?token=" + p.setupToken)

	return nil
}

// hintAboutSuperuser says how to get into PocketBase's dashboard. It is optional:
// the panel itself is used with normal accounts.
func (p *Panel) hintAboutSuperuser() {
	total, err := p.App.CountRecords(schema.Superusers)
	if err != nil || total > 0 {
		return
	}
	p.Log.Info("no superuser exists yet: for the PocketBase dashboard at /_/, create one with: panel superuser upsert EMAIL PASSWORD")
}

func (p *Panel) setupNeeded() (bool, error) {
	total, err := p.App.CountRecords(schema.Users)
	if err != nil {
		return false, err
	}
	return total == 0, nil
}

func (p *Panel) bindSetupRoutes(se *core.ServeEvent) {
	se.Router.GET("/api/pc/setup", func(e *core.RequestEvent) error {
		needed, err := p.setupNeeded()
		if err != nil {
			return e.InternalServerError("", err)
		}
		return e.JSON(http.StatusOK, map[string]bool{"needed": needed})
	})

	se.Router.POST("/api/pc/setup", p.handleSetup)
}

// handleSetup creates the first admin account. It stops working as soon as any
// account exists; after that, admins create the others.
func (p *Panel) handleSetup(e *core.RequestEvent) error {
	var body struct {
		Token    string `json:"token"`
		Email    string `json:"email"`
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}

	if p.setupToken == "" || subtle.ConstantTimeCompare([]byte(body.Token), []byte(p.setupToken)) != 1 {
		return e.ForbiddenError("The setup token is not valid. It is printed in the panel's log when it starts without any user account.", nil)
	}

	collection, err := p.App.FindCachedCollectionByNameOrId(schema.Users)
	if err != nil {
		return e.InternalServerError("", err)
	}

	admin := core.NewRecord(collection)
	admin.SetEmail(body.Email)
	admin.SetVerified(true)
	admin.SetPassword(body.Password)
	admin.Set("name", body.Name)
	admin.Set("role", schema.RoleAdmin)

	actor := audit.ActorFromRequest(e)
	actor.Label = "first-run setup"

	err = p.App.RunInTransaction(func(tx core.App) error {
		total, err := tx.CountRecords(schema.Users)
		if err != nil {
			return err
		}
		if total > 0 {
			return errSetupDone
		}
		return tx.SaveWithContext(audit.WithActor(e.Request.Context(), actor), admin)
	})
	if err != nil {
		if errors.Is(err, errSetupDone) {
			return e.ForbiddenError("An account already exists: sign in instead.", nil)
		}
		return e.BadRequestError("The account could not be created.", err)
	}

	p.setupToken = ""

	return e.NoContent(http.StatusNoContent)
}
