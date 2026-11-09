package stacks

import (
	"context"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v4"
)

func backupLabels(t *testing.T, compose string) ([]BackupLabel, []Problem) {
	t.Helper()
	project, err := LoadResolved(context.Background(), "shop", compose, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var tree yaml.Node
	if err := yaml.Unmarshal([]byte(compose), &tree); err != nil {
		t.Fatal(err)
	}
	return BackupLabels(project, &tree, nil)
}

// TestLabelsBackUpServices (#29): pocketcloud.backup names what a service's
// job backs up; the other labels are optional and say only what they set.
func TestLabelsBackUpServices(t *testing.T) {
	compose := `services:
  app:
    image: app:1
    volumes:
      - uploads:/srv/uploads
      - cache:/srv/cache
      - ./config:/etc/app
      - /tmp
    labels:
      pocketcloud.backup: volumes
      pocketcloud.backup.schedule: "15  */6 * * *"
      pocketcloud.backup.target: b2, disk ,b2
      pocketcloud.backup.retention: 24h 7d 4w 6m 2y
  db:
    image: postgres:17
    labels:
      - pocketcloud.backup=postgres
  maria:
    image: mariadb:11
    labels:
      pocketcloud.backup: MariaDB
      pocketcloud.backup.retention: "20"
  notes:
    image: notes:1
    labels:
      pocketcloud.backup: sqlite:/data/notes.db
  plain:
    image: plain:1
    volumes: [data:/data]
    labels:
      pocketcloud.backup: "true"
  off:
    image: off:1
    labels:
      pocketcloud.backup: "false"
  web:
    image: nginx:1
volumes:
  uploads:
  cache:
    name: shared-cache
  data:
    external: true
`
	got, problems := backupLabels(t, compose)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if len(got) != 5 {
		t.Fatalf("%d labels: %+v", len(got), got)
	}
	app, db, maria, notes, plain := got[0], got[1], got[2], got[3], got[4]
	if app.Kind != BackupVolumes || strings.Join(app.Volumes, ",") != "shared-cache,shop_uploads" || app.Schedule != "15 */6 * * *" ||
		!app.TargetsSet || strings.Join(app.Targets, ",") != "b2,disk" || app.Retention == nil ||
		*app.Retention != (Retention{Hourly: 24, Daily: 7, Weekly: 4, Monthly: 6, Yearly: 2}) || app.Broken {
		t.Errorf("app: %+v", app)
	}
	if db.Kind != BackupPostgres || db.Schedule != "" || db.TargetsSet || db.Retention != nil {
		t.Errorf("db: %+v", db)
	}
	if maria.Kind != BackupMySQL || maria.Retention == nil || *maria.Retention != (Retention{Last: 20}) {
		t.Errorf("maria: %+v", maria)
	}
	if notes.Kind != BackupSQLite || notes.File != "/data/notes.db" {
		t.Errorf("notes: %+v", notes)
	}
	if plain.Kind != BackupVolumes || strings.Join(plain.Volumes, ",") != "data" {
		t.Errorf("plain: %+v", plain)
	}
}

// TestWrongBackupLabelsSayWhatIsWrongAndWhere (#29): a label that cannot be
// used is a problem on its line, and holds the service's job as it is.
func TestWrongBackupLabelsSayWhatIsWrongAndWhere(t *testing.T) {
	compose := `services:
  novolume:
    image: a:1
    volumes: ["/srv/a:/data"]
    labels:
      pocketcloud.backup: volumes
  what:
    image: b:1
    labels:
      pocketcloud.backup: everything
  relative:
    image: c:1
    labels:
      pocketcloud.backup: sqlite:data/app.db
  schedule:
    image: d:1
    labels:
      pocketcloud.backup: postgres
      pocketcloud.backup.schedule: every night
  notarget:
    image: e:1
    labels:
      pocketcloud.backup: postgres
      pocketcloud.backup.target: " , "
  retention:
    image: f:1
    labels:
      pocketcloud.backup: postgres
      pocketcloud.backup.retention: 7d 3x
  lonely:
    image: g:1
    labels:
      pocketcloud.backup.schedule: "@daily"
`
	got, problems := backupLabels(t, compose)
	if len(got) != 6 {
		t.Errorf("%d labels: %+v", len(got), got)
	}
	for _, label := range got {
		if !label.Broken {
			t.Errorf("used although wrong: %+v", label)
		}
	}
	want := map[string]string{
		"novolume:":  "the service has no named volume",
		"what:":      `pocketcloud.backup is volumes, postgres, mysql, mariadb or sqlite:/path/in/the/container, not "everything"`,
		"relative:":  `absolute path in the container, such as sqlite:/data/app.db, not "data/app.db"`,
		"schedule:":  `not "every night"`,
		"notarget:":  "pocketcloud.backup.target names no target",
		"retention:": `not "3x"`,
		"lonely:":    "pocketcloud.backup.schedule does nothing without pocketcloud.backup",
	}
	for prefix, text := range want {
		found := false
		for _, problem := range problems {
			if strings.HasPrefix(problem.Message, prefix) && strings.Contains(problem.Message, text) {
				found = true
				if problem.Line == 0 {
					t.Errorf("%s has no line", problem.Message)
				}
			}
		}
		if !found {
			t.Errorf("no problem %q %q in %v", prefix, text, problems)
		}
	}
	for _, problem := range problems {
		if strings.HasPrefix(problem.Message, "schedule:") && problem.Line != 19 {
			t.Errorf("the schedule at line %d", problem.Line)
		}
	}
	// the editor warns of them too
	if result := validate(compose, ""); len(result.Warnings) != len(problems) {
		t.Errorf("the editor's warnings: %v", result.Warnings)
	}
}

// TestRetentionShorthand (#29): counts with a unit, and a number alone for
// the last snapshots, in any order; String writes them back.
func TestRetentionShorthand(t *testing.T) {
	for _, test := range []struct {
		text, want string
		retention  Retention
	}{
		{"7d 4w 6m", "7d 4w 6m", Retention{Daily: 7, Weekly: 4, Monthly: 6}},
		{"2Y,24h  10", "24h 2y 10", Retention{Hourly: 24, Yearly: 2, Last: 10}},
		{"30", "30", Retention{Last: 30}},
	} {
		got, err := ParseRetention(test.text)
		if err != nil || got != test.retention || got.String() != test.want {
			t.Errorf("%q: %+v %q %v", test.text, got, got.String(), err)
		}
	}
	for _, text := range []string{"", "7d 7d", "0d", "10001", "7x", "d", "-1"} {
		if got, err := ParseRetention(text); err == nil {
			t.Errorf("%q: %+v", text, got)
		}
	}
}
