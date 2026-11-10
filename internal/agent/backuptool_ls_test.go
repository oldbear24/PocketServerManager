package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLsKeepsTheFoldersOwnEntriesOfADeeperListing (#30): restic 0.17.3
// lists a folder without going deeper, but the tool keeps only the folder's
// own entries whatever restic prints, so that a page never holds what is in
// its folders.
func TestLsKeepsTheFoldersOwnEntriesOfADeeperListing(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		`{"message_type":"snapshot","struct_type":"snapshot","id":"x"}`,
		`{"name":"app","type":"dir","path":"/volumes/app","mode":2147484141,"message_type":"node","struct_type":"node"}`,
		`{"name":"conf","type":"dir","path":"/volumes/app/conf","mode":2147484141,"message_type":"node","struct_type":"node"}`,
		`{"name":"app.ini","type":"file","path":"/volumes/app/conf/app.ini","size":3,"mode":420,"message_type":"node","struct_type":"node"}`,
		`{"name":"a.txt","type":"file","path":"/volumes/app/a.txt","size":1,"mode":420,"message_type":"node","struct_type":"node"}`,
		`{"name":"other","type":"dir","path":"/volumes/other","mode":2147484141,"message_type":"node","struct_type":"node"}`,
	}
	listing := filepath.Join(dir, "listing")
	if err := os.WriteFile(listing, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \" $* \" in *\" version \"*) echo 'restic 0.17.3 compiled with go1.23.9 on linux/amd64'; exit 0;; esac\ncat '" + listing + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "restic"), []byte(script), 0o755); err != nil { //nolint:gosec // a script
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	result, err := restic{log: io.Discard}.ls(context.Background(), strings.Repeat("a", 64), "/volumes/app", 0, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 || result.Entries[0].Name != "conf" || result.Entries[1].Name != "a.txt" {
		t.Errorf("the folder: %+v", result)
	}
}

// TestCountEntriesStopsPastTenThousand (#30): a destination's entries are
// counted to 10,000, and one that holds more says so, without reading all
// of it; one that is not there holds nothing.
func TestCountEntriesStopsPastTenThousand(t *testing.T) {
	dir := t.TempDir()
	if n, capped := countEntries(filepath.Join(dir, "missing")); n != 0 || capped {
		t.Errorf("a missing destination: %d %v", n, capped)
	}
	writeFiles(t, dir, map[string]string{"sub/a": "a", "b": "b"})
	if n, capped := countEntries(dir); n != 3 || capped {
		t.Errorf("a small destination: %d %v", n, capped)
	}
	for i := range maxEntriesCounted {
		if err := os.WriteFile(filepath.Join(dir, "sub", "f"+itoa(i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if n, capped := countEntries(dir); n != maxEntriesCounted || !capped {
		t.Errorf("a large destination: %d %v", n, capped)
	}
}
