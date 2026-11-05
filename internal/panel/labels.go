package panel

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/pocketbase/pocketbase/tools/types"
	yaml "go.yaml.in/yaml/v4"

	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/edge"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/stacks"
)

// A service's pocketcloud.domain label puts it on domains (#25). After every
// deploy that succeeds, the panel makes the stack's label domains what the
// deployed revision's labels say: a domain like any other, of the stack's
// server, so its DNS records, its certificate and its route on the edge follow
// as they do for one added by hand. A label that is gone leaves its domain in
// place, marked, until someone removes it or keeps it as a domain of its own.

// labelProblem is what was wrong with a label at the last deploy.
type labelProblem struct {
	Line    int    `json:"line,omitempty"`
	Message string `json:"message"`
}

func (p *Panel) registerLabels() {
	p.App.OnRecordCreateRequest(schema.Domains).BindFunc(p.guardNewDomain)
	p.App.OnRecordUpdateRequest(schema.Domains).BindFunc(p.guardDomainUpdate)
	p.App.OnRecordDeleteRequest(schema.Domains).BindFunc(p.guardDomainDelete)

	// a stack that is gone leaves its label domains to be confirmed
	p.App.OnRecordAfterDeleteSuccess(schema.Stacks).BindFunc(func(e *core.RecordEvent) error {
		p.later.after(0, p.markOrphanedLabelDomains)
		return e.Next()
	})
}

func (p *Panel) bindLabelRoutes(group *router.RouterGroup[*core.RequestEvent]) {
	operators := requireRole(schema.RoleOperator, schema.RoleAdmin)
	group.POST("/domains/{id}/keep", p.handleKeepDomain).BindFunc(operators)
}

// fromLabel tells whether a domain is a label's, and whether its label is
// still there.
func fromLabel(domain *core.Record) (label, removed bool) {
	if domain.GetString("origin") != schema.OriginLabel {
		return false, false
	}
	return true, domain.GetBool("label_removed") || domain.GetString("stack") == ""
}

// labelFields are what only the panel sets on a domain.
var labelFields = []string{"origin", "stack", "service", "label_removed"}

func (p *Panel) guardNewDomain(e *core.RecordRequestEvent) error {
	for _, field := range labelFields {
		e.Record.Set(field, nil)
	}
	return e.Next()
}

func (p *Panel) labelStackName(domain *core.Record) string {
	if stack, err := p.App.FindRecordById(schema.Stacks, domain.GetString("stack")); err == nil {
		return stack.GetString("name")
	}
	return "a removed stack"
}

func (p *Panel) guardDomainUpdate(e *core.RecordRequestEvent) error {
	stored, err := e.App.FindRecordById(schema.Domains, e.Record.Id)
	if err != nil {
		return e.NotFoundError("The domain does not exist.", err)
	}
	if label, removed := fromLabel(stored); label {
		if removed {
			return e.BadRequestError("The label of this domain is gone: remove the domain, or keep it as a domain of its own first.", nil)
		}
		return e.BadRequestError(fmt.Sprintf("This domain comes from the %s label of %s (%s): change the label in its Compose file and deploy.",
			stacks.LabelDomain, p.labelStackName(stored), stored.GetString("service")), nil)
	}
	for _, field := range labelFields {
		e.Record.Set(field, stored.Get(field))
	}
	return e.Next()
}

func (p *Panel) guardDomainDelete(e *core.RecordRequestEvent) error {
	if label, removed := fromLabel(e.Record); label && !removed {
		return e.BadRequestError(fmt.Sprintf("This domain comes from the %s label of %s (%s): remove the label and deploy, then confirm the removal here.",
			stacks.LabelDomain, p.labelStackName(e.Record), e.Record.GetString("service")), nil)
	}
	return e.Next()
}

// handleKeepDomain keeps the domain of a label that is gone as a domain of
// its own, as if it had been added by hand.
func (p *Panel) handleKeepDomain(e *core.RequestEvent) error {
	domain, err := e.App.FindRecordById(schema.Domains, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The domain does not exist.", err)
	}
	label, removed := fromLabel(domain)
	if !label {
		return e.JSON(http.StatusOK, map[string]any{"id": domain.Id})
	}
	if !removed {
		return e.BadRequestError("The label of this domain is still there: remove it and deploy first.", nil)
	}
	// a deploy may have brought the label back since: the domain as it is now
	errBack := errors.New("the label is back")
	err = p.update(audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e)), domain, func(fresh *core.Record) error {
		if label, removed := fromLabel(fresh); !label {
			return errUnchanged
		} else if !removed {
			return errBack
		}
		for _, field := range labelFields {
			fresh.Set(field, nil)
		}
		return nil
	})
	if errors.Is(err, errBack) {
		return e.BadRequestError("The label of this domain is still there: remove it and deploy first.", nil)
	}
	if err != nil {
		return e.BadRequestError("Failed to keep the domain.", err)
	}
	return e.JSON(http.StatusOK, map[string]any{"id": domain.Id})
}

// markOrphanedLabelDomains marks the label domains of stacks that are gone.
func (p *Panel) markOrphanedLabelDomains() {
	records, err := p.App.FindRecordsByFilter(schema.Domains, "origin={:label} && stack='' && label_removed=false", "", 0, 0,
		dbx.Params{"label": schema.OriginLabel})
	if err != nil {
		return
	}
	for _, record := range records {
		err := p.update(context.Background(), record, func(fresh *core.Record) error {
			if fresh.GetString("origin") != schema.OriginLabel || fresh.GetString("stack") != "" || fresh.GetBool("label_removed") {
				return errUnchanged
			}
			fresh.Set("label_removed", true)
			return nil
		})
		if err != nil {
			p.Log.Error("failed to mark a label domain", "domain", record.Id, "error", err)
		}
	}
}

// stackExposures reads the labels of a deployed revision, with the values its
// deploy had: the stack's variables and secrets.
func (p *Panel) stackExposures(stack, revision *core.Record) ([]stacks.Exposure, []labelProblem, error) {
	sealed, err := p.openStackSecrets(stack)
	if err != nil {
		return nil, nil, err
	}
	plain, referenced, err := stacks.ResolveEnv(revision.GetString("env"), p.secretResolver(stack))
	if err != nil {
		return nil, nil, err
	}
	maps.Copy(sealed, referenced)
	environment, err := stacks.Environment(plain, sealed)
	if err != nil {
		return nil, nil, err
	}
	compose := revision.GetString("compose_yaml")
	project, err := stacks.LoadResolved(context.Background(), stack.GetString("name"), compose, "", environment)
	if err != nil {
		return nil, nil, err
	}
	var tree yaml.Node
	if err := yaml.Unmarshal([]byte(compose), &tree); err != nil {
		return nil, nil, err
	}
	exposures, found := stacks.Exposures(project, &tree, nil)
	problems := make([]labelProblem, 0, len(found))
	for _, problem := range found {
		problems = append(problems, labelProblem{Line: problem.Line, Message: problem.Message})
	}
	return exposures, problems, nil
}

// syncStackLabels makes the label domains of a stack what its deployed
// revision says.
func (p *Panel) syncStackLabels(stack, revision *core.Record) {
	lock := p.edgeLocks.of("labels:" + stack.Id)
	lock.Lock()
	defer lock.Unlock()

	exposures, problems, err := p.stackExposures(stack, revision)
	if err != nil {
		p.setFields(stack, map[string]any{"label_problems": []labelProblem{{Message: "The labels could not be read: " + err.Error()}}, "labels_at": types.NowDateTime()})
		return
	}

	existing, err := p.App.FindRecordsByFilter(schema.Domains, "stack={:s} && origin={:label}", "", 0, 0,
		dbx.Params{"s": stack.Id, "label": schema.OriginLabel})
	if err != nil {
		p.Log.Error("failed to read the label domains", "stack", stack.Id, "error", err)
		return
	}
	byHostname := map[string]*core.Record{}
	for _, domain := range existing {
		byHostname[domain.GetString("hostname")] = domain
	}

	wanted := map[string]bool{}
	// a service whose labels are wrong keeps its domains as they are
	held := map[string]bool{}
	for _, exposure := range exposures {
		if exposure.Broken {
			held[exposure.Service] = true
			for _, hostname := range exposure.Hostnames {
				wanted[hostname] = true
			}
			continue
		}
		for _, hostname := range exposure.Hostnames {
			wanted[hostname] = true
			if problem := p.applyExposure(stack, exposure, hostname, byHostname[hostname]); problem != "" {
				problems = append(problems, labelProblem{Message: exposure.Service + ": " + problem})
			}
		}
	}

	for hostname, domain := range byHostname {
		if wanted[hostname] || held[domain.GetString("service")] || domain.GetBool("label_removed") {
			continue
		}
		err := p.update(context.Background(), domain, func(fresh *core.Record) error {
			if !ownedBy(fresh, stack) || fresh.GetBool("label_removed") {
				return errUnchanged
			}
			fresh.Set("label_removed", true)
			return nil
		})
		if err != nil {
			p.Log.Error("failed to mark a label domain", "domain", domain.Id, "error", err)
		}
	}

	slices.SortFunc(problems, func(a, b labelProblem) int { return strings.Compare(a.Message, b.Message) })
	p.setFields(stack, map[string]any{"label_problems": problems, "labels_at": types.NowDateTime()})
}

// applyExposure creates or changes the domain of one hostname of a label. It
// returns what went wrong, for the stack's page.
func (p *Panel) applyExposure(stack *core.Record, exposure stacks.Exposure, hostname string, domain *core.Record) string {
	if domain == nil {
		other, err := p.App.FindFirstRecordByData(schema.Domains, "hostname", hostname)
		if err == nil {
			where := "of another stack"
			if label, _ := fromLabel(other); !label {
				where = "added by hand"
			}
			return fmt.Sprintf("%s is a domain %s already: remove it there, or change the label.", hostname, where)
		}
		collection, err := p.App.FindCachedCollectionByNameOrId(schema.Domains)
		if err != nil {
			return err.Error()
		}
		domain = core.NewRecord(collection)
		domain.Set("hostname", hostname)
		domain.Set("origin", schema.OriginLabel)
		domain.Set("stack", stack.Id)
		domain.Set("target_kind", schema.TargetContainer)
		domain.Set("hsts", false)
		domain.Set("compress", false)
		p.setExposure(domain, stack, exposure)
		if err := p.App.Save(domain); err != nil {
			return describeSaveError(hostname, err)
		}
		return ""
	}

	// the domain as it is stored now: it may have been kept or removed since
	// the sync read it, and nothing else it has is written back
	err := p.update(context.Background(), domain, func(fresh *core.Record) error {
		if !ownedBy(fresh, stack) || !p.setExposure(fresh, stack, exposure) {
			return errUnchanged
		}
		return nil
	})
	if err != nil {
		return describeSaveError(hostname, err)
	}
	return ""
}

// ownedBy says whether a domain is still a label domain of a stack.
func ownedBy(domain, stack *core.Record) bool {
	return domain.GetString("origin") == schema.OriginLabel && domain.GetString("stack") == stack.Id
}

// setExposure sets what a label says on its domain, and says whether that
// changed anything.
func (p *Panel) setExposure(domain, stack *core.Record, exposure stacks.Exposure) bool {
	tls := schema.TLSAuto
	if exposure.TLS == stacks.TLSOff {
		tls = schema.TLSOff
	}
	auth := basicAuthText(exposure.BasicAuth)
	stored := ""
	if sealed := domain.GetString("basic_auth"); sealed != "" {
		stored, _ = p.Box.OpenString(sealed)
	}

	changed := domain.IsNew() || domain.GetString("host") != stack.GetString("host") ||
		domain.GetString("target_kind") != schema.TargetContainer ||
		domain.GetString("container") != exposure.Container || domain.GetInt("port") != exposure.Port ||
		domain.GetString("tls") != tls || domain.GetString("service") != exposure.Service ||
		domain.GetBool("label_removed") || stored != auth
	if !changed {
		return false
	}
	domain.Set("host", stack.GetString("host"))
	domain.Set("target_kind", schema.TargetContainer)
	domain.Set("container", exposure.Container)
	domain.Set("port", exposure.Port)
	domain.Set("tls", tls)
	// HTTPS once there is a certificate; the edge sends plain HTTP there then
	domain.Set("https_redirect", tls == schema.TLSAuto)
	domain.Set("service", exposure.Service)
	domain.Set("label_removed", false)
	domain.Set("basic_auth", auth)
	return true
}

// describeSaveError says in words why a domain could not be saved.
func describeSaveError(hostname string, err error) string {
	return hostname + ": " + saveErrorText(err)
}

// saveErrorText is why a record could not be saved, in words.
func saveErrorText(err error) string {
	var errs validation.Errors
	if errors.As(err, &errs) {
		messages := []string{}
		for _, field := range slices.Sorted(maps.Keys(errs)) {
			messages = append(messages, errs[field].Error())
		}
		return strings.Join(messages, " ")
	}
	return err.Error()
}

// basicAuthText is how the users of a route are stored: user:hash lines.
func basicAuthText(users []edge.BasicAuthUser) string {
	lines := make([]string, 0, len(users))
	for _, user := range users {
		lines = append(lines, user.Username+":"+user.Hash)
	}
	return strings.Join(lines, "\n")
}

func parseBasicAuth(text string) []edge.BasicAuthUser {
	users := []edge.BasicAuthUser{}
	for _, line := range strings.Split(text, "\n") {
		if name, hash, ok := strings.Cut(strings.TrimSpace(line), ":"); ok {
			users = append(users, edge.BasicAuthUser{Username: name, Hash: hash})
		}
	}
	return users
}

// domainBasicAuth opens the users of a domain's route.
func (p *Panel) domainBasicAuth(domain *core.Record) ([]edge.BasicAuthUser, error) {
	sealed := domain.GetString("basic_auth")
	if sealed == "" {
		return nil, nil
	}
	plain, err := p.Box.OpenString(sealed)
	if err != nil {
		return nil, fmt.Errorf("the passwords of %s cannot be opened: %w", domain.GetString("hostname"), err)
	}
	return parseBasicAuth(plain), nil
}

// validateBasicAuth checks the users of a domain as they are saved, before
// they are sealed, and that a domain with a password stays one the edge can
// ask it for: a container behind HTTPS. It names the field that is wrong.
func validateBasicAuth(record *core.Record) (string, error) {
	text := record.GetString("basic_auth")
	if text == "" {
		return "", nil
	}
	// only the panel sets passwords, so a sealed one is the label's, or a kept
	// label domain's: removing the domain is the way to drop it
	const kept = " remove the domain and add it again to drop its password"
	if record.GetString("target_kind") != schema.TargetContainer {
		if secrets.IsSealed(text) {
			return "target_kind", errors.New("this domain asks for a password, so it stays the domain of a container:" + kept)
		}
		return "basic_auth", errors.New("only a domain of a container can ask for a password")
	}
	if record.GetString("tls") == schema.TLSOff {
		if secrets.IsSealed(text) {
			return "tls", errors.New("this domain asks for a password, so it needs HTTPS:" + kept)
		}
		return "basic_auth", errors.New("a password needs HTTPS")
	}
	if secrets.IsSealed(text) {
		return "", nil
	}
	return "basic_auth", checkBasicAuthText(text)
}

// checkBasicAuthText checks user:hash lines as they are typed: a line without
// a user, such as a hash alone, would leave the route without a password.
func checkBasicAuthText(text string) error {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.Contains(line, ":") {
			return errors.New("each line is user:bcrypt-hash; a line has no user")
		}
	}
	users := parseBasicAuth(text)
	if len(users) == 0 {
		return errors.New("name at least one user:bcrypt-hash")
	}
	return edge.ValidBasicAuth(users)
}
