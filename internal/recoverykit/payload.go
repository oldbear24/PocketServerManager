package recoverykit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/gowebpki/jcs"

	"github.com/oldbear24/PocketServerManager/internal/secrets"
)

// Payload is what a kit seals: the master key, the panel's start
// environment and dial addresses, and for the panel's backup job each
// target's repository exactly as the panel's enricher delivers it to an
// agent. A kit holds every secret a recovery needs: nothing of it may reach
// a log, an event, a result or the audit.
type Payload struct {
	Format    string `json:"format"`
	Version   int    `json:"version"`
	KitID     string `json:"kit_id"`
	CreatedAt string `json:"created_at"`
	// CreatedBy is the admin who downloaded it.
	CreatedBy string `json:"created_by"`
	Panel     string `json:"panel"`
	KeyID     string `json:"key_id"`
	// MasterKey is the master key, 64 hex characters.
	MasterKey    string `json:"master_key"`
	CAPin        string `json:"ca_pin"`
	PanelVersion string `json:"panel_version"`
	AgentImage   string `json:"agent_image"`
	// Env is the panel's effective start environment, POCKETCLOUD_* names
	// and values; never POCKETCLOUD_METRICS_TOKEN.
	Env map[string]string `json:"env"`
	// AgentsDial lists the addresses the servers' agents dial.
	AgentsDial []Dial `json:"agents_dial"`
	// Job is the panel's backup job, or nil when it has none.
	Job *Job `json:"job"`
	// Targets are the panel job's targets.
	Targets []Target `json:"targets"`
}

// Dial is an address servers dial, how many dial it, and whether it is an IP
// address, which a new machine has to take over.
type Dial struct {
	Address string `json:"address"`
	Servers int    `json:"servers"`
	IP      bool   `json:"ip"`
}

// Job is the panel's backup job.
type Job struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Target is a target of the panel's backup job.
type Target struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Server is the name of a local target's server: only there its
	// directory can be read.
	Server     string     `json:"server"`
	Repository Repository `json:"repository"`
}

// Repository is a target's repository as the enricher delivers it: the
// fields of agentv1.BackupRepository but its target id.
type Repository struct {
	URL        string            `json:"url"`
	Password   string            `json:"password"`
	Env        map[string]string `json:"env"`
	Options    map[string]string `json:"options"`
	SSHKey     string            `json:"ssh_key"`
	KnownHosts string            `json:"known_hosts"`
	Mount      string            `json:"mount"`
}

// Key is the master key the payload holds.
func (p Payload) Key() ([]byte, error) {
	key, err := hex.DecodeString(p.MasterKey)
	if err != nil || len(key) != secrets.KeySize {
		return nil, fmt.Errorf("a kit's master key is %d hex characters", 2*secrets.KeySize)
	}
	return key, nil
}

// fill sets what the payload repeats of the header, and empties for nils.
func (p *Payload) fill(h Header) {
	p.Format, p.Version = PayloadFormat, Version
	p.KitID, p.CreatedAt, p.Panel = h.Kit, h.Created.UTC().Format(time.RFC3339), h.Panel
	p.KeyID, p.CAPin, p.PanelVersion, p.AgentImage = h.KeyID, h.CAPin, h.PanelVersion, h.AgentImage
	if p.Env == nil {
		p.Env = map[string]string{}
	}
	if p.AgentsDial == nil {
		p.AgentsDial = []Dial{}
	}
	if p.Targets == nil {
		p.Targets = []Target{}
	}
	for i := range p.Targets {
		if p.Targets[i].Repository.Env == nil {
			p.Targets[i].Repository.Env = map[string]string{}
		}
		if p.Targets[i].Repository.Options == nil {
			p.Targets[i].Repository.Options = map[string]string{}
		}
	}
}

// agrees checks an opened payload against its header: the format, the
// values the header repeats, and the key the header names.
func (p Payload) agrees(h Header) error {
	if p.Format != PayloadFormat || p.Version != Version {
		return notAKit("its payload is of the format %q, version %d", p.Format, p.Version)
	}
	header := []string{h.Kit, h.Created.UTC().Format(time.RFC3339), h.Panel, h.KeyID, h.CAPin, h.PanelVersion, h.AgentImage}
	payload := []string{p.KitID, p.CreatedAt, p.Panel, p.KeyID, p.CAPin, p.PanelVersion, p.AgentImage}
	for i, value := range header {
		if payload[i] != value {
			return notAKit("its header says %s %.80q, its sealed content %.80q", headerKeys[i], value, payload[i])
		}
	}
	key, err := p.Key()
	if err != nil {
		return notAKit("%v", err)
	}
	if secrets.KeyID(key) != h.KeyID {
		return notAKit("its master key is not the key %s its header names", h.KeyID)
	}
	return nil
}

// The sections of a kit whose digests say whether it is current, in the
// order the panel names them.
const (
	SectionKey     = "key"
	SectionAddress = "address"
	SectionJob     = "job"
	SectionTargets = "targets"
)

// Sections lists the sections in their order.
var Sections = []string{SectionKey, SectionAddress, SectionJob, SectionTargets}

// DigestPurpose names the master key's key for the digests.
const DigestPurpose = "pocketcloud recovery kit digest v1"

// Digests are the digests of what a payload holds, one per section: the
// first 32 hex characters of HMAC-SHA256 under the master key's key
// DigestPurpose of the section's name, a zero byte and the section's
// canonical JSON (RFC 8785):
//
//	key      the key id
//	address  {"agents_dial": the addresses sorted, "env": env}
//	job      {"id": the job's id, "targets": the target ids sorted}, or null
//	targets  {target id: {"kind", "repository"}, …}
//
// A payload of the panel as it is now gives its current digests; a kit's own
// payload gives the kit's, so a kit made before a restore still checks. Dial
// counts, names and the job's name are left out: they change nothing a
// recovery needs.
func Digests(p Payload) (map[string]string, error) {
	master, err := p.Key()
	if err != nil {
		return nil, err
	}
	key, err := secrets.DeriveKey(master, DigestPurpose)
	if err != nil {
		return nil, err
	}

	addresses := make([]string, 0, len(p.AgentsDial))
	for _, dial := range p.AgentsDial {
		addresses = append(addresses, dial.Address)
	}
	slices.Sort(addresses)
	env := p.Env
	if env == nil {
		env = map[string]string{}
	}
	var job any
	ids := make([]string, 0, len(p.Targets))
	targets := map[string]any{}
	for _, target := range p.Targets {
		ids = append(ids, target.ID)
		repo := target.Repository
		if repo.Env == nil {
			repo.Env = map[string]string{}
		}
		if repo.Options == nil {
			repo.Options = map[string]string{}
		}
		targets[target.ID] = map[string]any{"kind": target.Kind, "repository": repo}
	}
	slices.Sort(ids)
	if p.Job != nil {
		job = map[string]any{"id": p.Job.ID, "targets": ids}
	}

	sections := map[string]any{
		SectionKey:     p.KeyID,
		SectionAddress: map[string]any{"agents_dial": addresses, "env": env},
		SectionJob:     job,
		SectionTargets: targets,
	}
	out := make(map[string]string, len(sections))
	for name, value := range sections {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		canonical, err := jcs.Transform(raw)
		if err != nil {
			return nil, err
		}
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(name))
		mac.Write([]byte{0})
		mac.Write(canonical)
		out[name] = hex.EncodeToString(mac.Sum(nil))[:32]
	}
	return out, nil
}

// Changed lists, in their order, the sections whose digests differ between
// a kit's and the panel's current ones: why the kit is out of date.
func Changed(kit, current map[string]string) []string {
	var changed []string
	for _, section := range Sections {
		if kit[section] == "" || !hmac.Equal([]byte(kit[section]), []byte(current[section])) {
			changed = append(changed, section)
		}
	}
	return changed
}
