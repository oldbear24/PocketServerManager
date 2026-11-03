package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// restoreStarted rewrites the agent's command store to what a kill -9 leaves
// behind: the command started and never finished.
func restoreStarted(t *testing.T, stateDir, commandID string) {
	t.Helper()

	path := filepath.Join(stateDir, "commands.json")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if entry["id"] == commandID {
			entry["state"] = "started"
			delete(entry, "finished_at")
			delete(entry, "error")
		}
	}

	raw, err = json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// recordID reads the id from a record in a JSON response.
func recordID(t *testing.T, body string) string {
	t.Helper()

	response := struct {
		ID string `json:"id"`
	}{}
	if err := json.Unmarshal([]byte(body), &response); err != nil || response.ID == "" {
		t.Fatalf("no id in %q (%v)", body, err)
	}

	return response.ID
}
