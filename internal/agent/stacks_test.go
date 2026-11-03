package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/dotenv"
)

// The env file must say exactly what was typed, as Compose reads it: passwords hold
// all kinds of characters. compose-go is what Compose itself parses it with.
func TestSecretsSurviveTheEnvFile(t *testing.T) {
	secrets := map[string]string{
		"PLAIN":      "hunter2",
		"DOLLAR":     "a$b${c}$$d",
		"HASH":       "a #b",
		"SINGLE":     `it's`,
		"DOUBLE":     `say "hi"`,
		"BACKSLASH":  `C:\dir\`,
		"ENDQUOTE":   `abc'`,
		"SPACES":     "  around  ",
		"NEWLINE":    "two\nlines",
		"UNICODE":    "příliš žluťoučký",
		"EMPTY":      "",
		"EVERYTHING": `s3cr3t'value$with"everything #\`,
	}

	path := filepath.Join(t.TempDir(), ".env")
	if err := writeEnvFile(path, "TZ=Europe/Prague\n# a comment\nA=1", secrets); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := dotenv.UnmarshalWithLookup(string(raw), nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}

	for name, value := range secrets {
		if parsed[name] != value {
			t.Errorf("%s: got %q, want %q", name, parsed[name], value)
		}
	}
	if parsed["TZ"] != "Europe/Prague" || parsed["A"] != "1" {
		t.Errorf("the plain part: %v", parsed)
	}
}

func TestMaskingHidesSecretsInOutput(t *testing.T) {
	var output bytes.Buffer
	writer := newMaskingWriter(&output, map[string]string{"A": "hunter2", "SHORT": "ab"})

	line := []byte("connecting with hunter2 to ab\n")
	count, err := writer.Write(line)
	if err != nil {
		t.Fatal(err)
	}
	if count != len(line) {
		t.Errorf("wrote %d of %d", count, len(line))
	}

	if text := output.String(); strings.Contains(text, "hunter2") || !strings.Contains(text, "*** to ab") {
		t.Errorf("output: %q", text)
	}

	// without anything to hide nothing stands in between
	if plain := newMaskingWriter(&output, nil); plain != &output {
		t.Error("a masking writer without secrets")
	}
}
