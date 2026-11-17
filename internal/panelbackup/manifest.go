package panelbackup

// ManifestFormat names the manifest's format.
const ManifestFormat = "pocketcloud-panel-backup"

// Manifest is the first segment of an export: what the panel was when it
// made it. The writer sets Format, Version, ExportID, KeyID and CreatedAt
// from the header; a reader checks that they agree with it, and that Files
// and Bytes count what follows.
type Manifest struct {
	Format    string `json:"format"`
	Version   int    `json:"version"`
	ExportID  string `json:"export_id"`
	CreatedAt string `json:"created_at"`
	// PanelVersion is the version of the panel that made it.
	PanelVersion string `json:"panel_version"`
	// Migration is the newest file of the database's _migrations.
	Migration string `json:"migration"`
	KeyID     string `json:"key_id"`
	// CAPin is pki.PinOf the internal CA's root, "sha256:…".
	CAPin string `json:"ca_pin"`
	// Audit is the head of the audit log in the copy.
	Audit AuditHead `json:"audit"`
	// AgentsDial lists the addresses the servers' agents dial.
	AgentsDial []Dial `json:"agents_dial"`
	// Hosts counts the servers.
	Hosts int `json:"hosts"`
	// Job and Run are the backup job and run the export was made for.
	Job string `json:"job"`
	Run string `json:"run"`
	// Storage is where PocketBase keeps stored files, local or s3: with s3
	// the export holds none.
	Storage string `json:"storage"`
	// Files counts the stored files of the export.
	Files int `json:"files"`
	// Bytes counts the bytes of the database and the stored files, before
	// compression.
	Bytes int64 `json:"bytes"`
	// Missing lists the files the database names that were gone when the
	// export was made.
	Missing []string `json:"missing"`
	// LeftOut lists what a panel backup never holds.
	LeftOut []string `json:"left_out"`
}

// AuditHead is the head of the audit log: the newest entry's seq and hash.
type AuditHead struct {
	Seq  int64  `json:"seq"`
	Hash string `json:"hash"`
}

// Dial is an address servers' agents dial, and how many dial it.
type Dial struct {
	Address string `json:"address"`
	Servers int    `json:"servers"`
}

// The storage of the stored files.
const (
	StorageLocal = "local"
	StorageS3    = "s3"
)

// LeftOut is what a panel backup never holds.
var LeftOut = []string{"auxiliary.db", "metrics.db", "diagnostic_bundles"}
