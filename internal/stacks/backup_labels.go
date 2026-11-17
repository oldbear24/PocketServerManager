package stacks

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/pocketbase/pocketbase/tools/cron"
	yaml "go.yaml.in/yaml/v4"
)

// Labels of a service that back it up (#29). The panel makes a backup job of
// each labelled service, of the stack's server, after every deploy.
const (
	// LabelBackup says what is backed up: volumes (or true) for the
	// service's named volumes, postgres, mysql or mariadb for a dump of its
	// database, sqlite:/path for a dump of a SQLite file in its container.
	LabelBackup = "pocketcloud.backup"
	// LabelBackupSchedule is when, as cron in UTC; 0 3 * * * by default.
	LabelBackupSchedule = "pocketcloud.backup.schedule"
	// LabelBackupTarget names the backup targets, separated by commas.
	LabelBackupTarget = "pocketcloud.backup.target"
	// LabelBackupRetention is how many snapshots are kept, as 24h 7d 4w 6m
	// 2y, or a number for the last ones; 7d 4w 6m by default.
	LabelBackupRetention = "pocketcloud.backup.retention"
)

// What a pocketcloud.backup label backs up.
const (
	// BackupVolumes is the service's named volumes.
	BackupVolumes = "volumes"
	// BackupPostgres, BackupMySQL and BackupSQLite are dumps of the
	// service's database; mariadb is mysql.
	BackupPostgres = "postgres"
	BackupMySQL    = "mysql"
	BackupSQLite   = "sqlite"
)

// Defaults of a label's job.
const (
	DefaultBackupSchedule  = "0 3 * * *"
	DefaultBackupRetention = "7d 4w 6m"
)

// maxBackupTargets is how many targets a job backs up to.
const maxBackupTargets = 10

// maxKeep is the most snapshots a retention count keeps.
const maxKeep = 10000

// ScheduleProblem says what a schedule is, for one that is not.
const ScheduleProblem = "A schedule has five fields, minute hour day month weekday, such as */5 * * * *, or is one of @hourly, @daily, @weekly, @monthly and @yearly."

// BackupLabel is a service that its labels back up.
type BackupLabel struct {
	Service string `json:"service"`
	// Kind is BackupVolumes, or the engine of a dump.
	Kind string `json:"kind"`
	// Volumes are the real names of the service's named volumes.
	Volumes []string `json:"volumes,omitempty"`
	// File is the SQLite file in the service's container.
	File string `json:"file,omitempty"`
	// Schedule is the label's schedule; empty when the label is not there.
	Schedule string `json:"schedule,omitempty"`
	// Targets are the names of the targets, when the label is there.
	Targets []string `json:"targets,omitempty"`
	// TargetsSet says the label naming the targets is there.
	TargetsSet bool `json:"targets_set,omitempty"`
	// Retention is the label's retention, when the label is there.
	Retention *Retention `json:"retention,omitempty"`
	// Broken is set when the labels are there but wrong: the service's job
	// stays as it is until they are fixed.
	Broken bool `json:"broken,omitempty"`
}

// Retention is how many snapshots of a job restic keeps: the newest of each
// hour, day, week, month and year, and the last ones. Zero keeps no count of
// that kind; all zero keeps every snapshot.
type Retention struct {
	Last    int `json:"last"`
	Hourly  int `json:"hourly"`
	Daily   int `json:"daily"`
	Weekly  int `json:"weekly"`
	Monthly int `json:"monthly"`
	Yearly  int `json:"yearly"`
}

// retentionUnits are the units of the shorthand, in its order.
var retentionUnits = []struct {
	unit string
	keep func(*Retention) *int
}{
	{"h", func(r *Retention) *int { return &r.Hourly }},
	{"d", func(r *Retention) *int { return &r.Daily }},
	{"w", func(r *Retention) *int { return &r.Weekly }},
	{"m", func(r *Retention) *int { return &r.Monthly }},
	{"y", func(r *Retention) *int { return &r.Yearly }},
}

// ParseRetention reads the shorthand of a retention: counts with a unit, h
// for hours, d days, w weeks, m months and y years, such as 24h 7d 4w 6m 2y,
// and a number alone for the last snapshots, separated by spaces or commas.
func ParseRetention(text string) (Retention, error) {
	var retention Retention
	seen := map[string]bool{}
	fields := strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	if len(fields) == 0 {
		return retention, fmt.Errorf("%s names no count", LabelBackupRetention)
	}
	for _, field := range fields {
		field = strings.ToLower(field)
		number, unit := field, ""
		if last := field[len(field)-1]; last < '0' || last > '9' {
			number, unit = field[:len(field)-1], field[len(field)-1:]
		}
		count, err := strconv.Atoi(number)
		if err != nil || count < 1 || count > maxKeep {
			return Retention{}, fmt.Errorf("%s is counts such as 24h 7d 4w 6m 2y, or a number for the last snapshots, each from 1 to %d, not %q", LabelBackupRetention, maxKeep, field)
		}
		if seen[unit] {
			return Retention{}, fmt.Errorf("%s names %q twice", LabelBackupRetention, unitName(unit))
		}
		seen[unit] = true
		if unit == "" {
			retention.Last = count
			continue
		}
		known := false
		for _, kind := range retentionUnits {
			if kind.unit == unit {
				*kind.keep(&retention) = count
				known = true
			}
		}
		if !known {
			return Retention{}, fmt.Errorf("%s counts hours (h), days (d), weeks (w), months (m) and years (y), not %q", LabelBackupRetention, field)
		}
	}
	return retention, nil
}

func unitName(unit string) string {
	if unit == "" {
		return "the last snapshots"
	}
	return unit
}

// String is the shorthand of a retention, as ParseRetention reads it.
func (r Retention) String() string {
	parts := []string{}
	for _, kind := range retentionUnits {
		if count := *kind.keep(&r); count > 0 {
			parts = append(parts, strconv.Itoa(count)+kind.unit)
		}
	}
	if r.Last > 0 {
		parts = append(parts, strconv.Itoa(r.Last))
	}
	return strings.Join(parts, " ")
}

// Valid says whether each count is one restic takes.
func (r Retention) Valid() bool {
	for _, count := range []int{r.Last, r.Hourly, r.Daily, r.Weekly, r.Monthly, r.Yearly} {
		if count < 0 || count > maxKeep {
			return false
		}
	}
	return true
}

// NormalSchedule is a schedule as PocketBase's cron reads it: its fields
// separated by single spaces.
func NormalSchedule(schedule string) string {
	return strings.Join(strings.Fields(schedule), " ")
}

// ValidSchedule says whether a schedule is one cron runs.
func ValidSchedule(schedule string) bool {
	_, err := cron.NewSchedule(NormalSchedule(schedule))
	return err == nil && schedule != ""
}

// BackupLabels reads the backup labels of a project's services. A service
// whose labels are wrong is Broken, and the problems say why. placeholder,
// which may be nil, names the values that cannot be checked.
func BackupLabels(project *types.Project, tree *yaml.Node, placeholder Placeholder) ([]BackupLabel, []Problem) {
	labels := []BackupLabel{}
	problems := []Problem{}

	for _, name := range slices.Sorted(maps.Keys(project.Services)) {
		service := project.Services[name]
		at := func(label, message string) Problem {
			line, column := lineOf(tree, []string{"services", name, "labels", label})
			return Problem{File: FileCompose, Line: line, Column: column, Message: name + ": " + message}
		}
		unknown := func(label string) bool { return placeholder != nil && placeholder(name, label) }

		what, ok := service.Labels[LabelBackup]
		if ok && !unknown(LabelBackup) && strings.EqualFold(strings.TrimSpace(what), "false") {
			ok = false
		}
		if !ok {
			for _, label := range []string{LabelBackupSchedule, LabelBackupTarget, LabelBackupRetention} {
				if _, set := service.Labels[label]; set {
					problems = append(problems, at(label, fmt.Sprintf("%s does nothing without %s.", label, LabelBackup)))
				}
			}
			continue
		}

		backup := BackupLabel{Service: name}
		valid := true
		fail := func(label, message string) {
			problems = append(problems, at(label, message))
			valid = false
		}

		if unknown(LabelBackup) {
			// checked at the deploy
			backup.Kind = BackupVolumes
		} else {
			value := strings.TrimSpace(what)
			switch lower := strings.ToLower(value); {
			case lower == "volumes" || lower == "true":
				backup.Kind = BackupVolumes
				backup.Volumes = namedVolumes(project, service)
				if len(backup.Volumes) == 0 {
					fail(LabelBackup, "the service has no named volume to back up: name one under volumes, or back up its database.")
				}
			case lower == BackupPostgres:
				backup.Kind = BackupPostgres
			case lower == BackupMySQL || lower == "mariadb":
				backup.Kind = BackupMySQL
			case strings.HasPrefix(lower, BackupSQLite+":"):
				backup.Kind = BackupSQLite
				backup.File = strings.TrimSpace(value[len(BackupSQLite)+1:])
				if !path.IsAbs(backup.File) || path.Clean(backup.File) != backup.File || backup.File == "/" {
					fail(LabelBackup, fmt.Sprintf("%s=sqlite: names the database file by its absolute path in the container, such as sqlite:/data/app.db, not %q.", LabelBackup, backup.File))
				}
			default:
				fail(LabelBackup, fmt.Sprintf("%s is volumes, postgres, mysql, mariadb or sqlite:/path/in/the/container, not %q.", LabelBackup, value))
			}
		}

		if text, set := service.Labels[LabelBackupSchedule]; set && !unknown(LabelBackupSchedule) {
			schedule := NormalSchedule(text)
			if !ValidSchedule(schedule) {
				fail(LabelBackupSchedule, fmt.Sprintf("%s has five fields, minute hour day month weekday, in UTC, such as 0 3 * * *, or is one of @hourly, @daily, @weekly, @monthly and @yearly, not %q.", LabelBackupSchedule, text))
			}
			backup.Schedule = schedule
		}

		if text, set := service.Labels[LabelBackupTarget]; set && !unknown(LabelBackupTarget) {
			backup.TargetsSet = true
			for _, target := range strings.Split(text, ",") {
				if target = strings.TrimSpace(target); target != "" && !slices.Contains(backup.Targets, target) {
					backup.Targets = append(backup.Targets, target)
				}
			}
			switch {
			case len(backup.Targets) == 0:
				fail(LabelBackupTarget, LabelBackupTarget+" names no target.")
			case len(backup.Targets) > maxBackupTargets:
				fail(LabelBackupTarget, fmt.Sprintf("a job backs up to at most %d targets.", maxBackupTargets))
			}
		}

		if text, set := service.Labels[LabelBackupRetention]; set && !unknown(LabelBackupRetention) {
			retention, err := ParseRetention(text)
			if err != nil {
				fail(LabelBackupRetention, err.Error()+".")
			} else {
				backup.Retention = &retention
			}
		}

		backup.Broken = !valid
		labels = append(labels, backup)
	}
	return labels, problems
}

// namedVolumes are the real names of the named volumes a service mounts.
func namedVolumes(project *types.Project, service types.ServiceConfig) []string {
	names := []string{}
	for _, mount := range service.Volumes {
		if mount.Type != types.VolumeTypeVolume || mount.Source == "" {
			continue
		}
		name := mount.Source
		if volume, ok := project.Volumes[mount.Source]; ok && volume.Name != "" {
			name = volume.Name
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}
