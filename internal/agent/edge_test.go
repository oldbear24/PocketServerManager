package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestCertificateFilesBelongToOneCertificate: removing certificate "a" leaves the
// files of certificate "a-b" alone.
func TestCertificateFilesBelongToOneCertificate(t *testing.T) {
	a := &Agent{Config: Config{StateDir: t.TempDir()}}
	dir := a.edgeDir("certs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := []string{"a-0123456789abcdef.crt", "a-0123456789abcdef.key", "a-b-0123456789abcdef.crt", "a-b-0123456789abcdef.key"}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	removed, _, err := a.removeCertFiles("a", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Errorf("removed %v", removed)
	}
	for _, name := range files[2:] {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s of another certificate was removed", name)
		}
	}
}

// TestFilesInUseAreFoundHoweverTheConfigWritesThem: JSON may escape the slashes.
func TestFilesInUseAreFoundHoweverTheConfigWritesThem(t *testing.T) {
	a := &Agent{Config: Config{StateDir: t.TempDir()}}
	config := json.RawMessage(`{"apps":{"tls":{"certificates":{"load_files":[{"certificate":"\/pocketcloud\/certs\/c-0123456789abcdef.crt","key":"\/pocketcloud\/certs\/c-0123456789abcdef.key"}]}}}}`)
	if err := os.MkdirAll(a.edgeDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := a.saveEdgeState(&edgeState{Config: config}); err != nil {
		t.Fatal(err)
	}
	used, err := a.usedCertFiles()
	if err != nil || !used["c-0123456789abcdef.crt"] || !used["c-0123456789abcdef.key"] {
		t.Errorf("files in use: %v, %v", used, err)
	}
}
