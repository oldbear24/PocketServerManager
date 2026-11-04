package fakecaddy

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func client(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
}

func post(t *testing.T, socket, body string) int {
	t.Helper()
	response, err := client(socket).Post("http://edge/load", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

func TestRefusedConfigKeepsTheOldOne(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "admin.sock")
	autosave := filepath.Join(dir, "autosave.json")

	server, err := Start(socket, autosave)
	if err != nil {
		t.Fatal(err)
	}

	good := `{"apps":{"http":{"servers":{"s":{"routes":[{"handle":[{"handler":"static_response"}]}]}}}}}`
	if code := post(t, socket, good); code != http.StatusOK {
		t.Fatalf("a valid config got %d", code)
	}
	for _, bad := range []string{
		`{"apps":`,
		`{"apps":{"layer4":{}}}`,
		`{"apps":{"http":{"servers":{"s":{"routes":[{"handle":[{"handler":"nope"}]}]}}}}}`,
	} {
		if code := post(t, socket, bad); code != http.StatusBadRequest {
			t.Errorf("%s got %d, want 400", bad, code)
		}
	}
	if !bytes.Equal(server.Config(), []byte(good)) {
		t.Errorf("the active config is %s", server.Config())
	}
	if len(server.Rejected()) != 3 || len(server.Loads()) != 1 {
		t.Errorf("%d rejected, %d loaded", len(server.Rejected()), len(server.Loads()))
	}

	// a restart resumes the last good config
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Error("the socket is left behind")
	}
	again, err := Start(socket, autosave)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if !bytes.Equal(again.Config(), []byte(good)) {
		t.Errorf("after a restart the config is %s", again.Config())
	}
}
