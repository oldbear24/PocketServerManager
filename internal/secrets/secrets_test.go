package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newBox(t *testing.T) *Box {
	t.Helper()

	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	box, err := New(key)
	if err != nil {
		t.Fatal(err)
	}

	return box
}

func TestRoundTrip(t *testing.T) {
	box := newBox(t)

	for _, plaintext := range []string{"", "x", "hunter2", strings.Repeat("long value ", 10000), "příliš žluťoučký kůň"} {
		sealed, err := box.SealString(plaintext)
		if err != nil {
			t.Fatal(err)
		}
		if !IsSealed(sealed) {
			t.Fatalf("sealed value has no prefix: %q", sealed)
		}
		if len(plaintext) > 8 && strings.Contains(sealed, plaintext) {
			t.Fatalf("sealed value contains the plaintext")
		}

		opened, err := box.OpenString(sealed)
		if err != nil {
			t.Fatal(err)
		}
		if opened != plaintext {
			t.Fatalf("got %q, want %q", opened, plaintext)
		}
	}
}

func TestSealIsRandomised(t *testing.T) {
	box := newBox(t)

	a, _ := box.SealString("same")
	b, _ := box.SealString("same")
	if a == b {
		t.Fatal("two seals of the same value must differ")
	}
}

func TestWrongKeyFails(t *testing.T) {
	sealed, err := newBox(t).SealString("secret")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := newBox(t).OpenString(sealed); err == nil {
		t.Fatal("opening with another key must fail")
	}
}

func TestTamperedValueFails(t *testing.T) {
	box := newBox(t)

	sealed, err := box.SealString("secret")
	if err != nil {
		t.Fatal(err)
	}

	// flip a character in the middle of the ciphertext
	raw := []byte(sealed)
	i := len(prefix) + (len(raw)-len(prefix))/2
	if raw[i] == 'A' {
		raw[i] = 'B'
	} else {
		raw[i] = 'A'
	}

	if _, err := box.OpenString(string(raw)); err == nil {
		t.Fatal("opening a tampered value must fail")
	}
}

func TestOpenRejectsMalformedValues(t *testing.T) {
	box := newBox(t)

	for _, value := range []string{"", "plain", "pcs1:", "pcs1:###", "pcs1:QUJD"} {
		if _, err := box.Open(value); err == nil {
			t.Fatalf("expected an error for %q", value)
		}
	}
}

func TestNewRejectsBadKeySizes(t *testing.T) {
	for _, size := range []int{0, 16, 31, 33} {
		if _, err := New(make([]byte, size)); err == nil {
			t.Fatalf("expected an error for a key of %d bytes", size)
		}
	}
}

func TestKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")

	if _, err := LoadKeyFile(path); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("expected ErrKeyMissing, got %v", err)
	}

	if err := WriteKeyFile(path); err != nil {
		t.Fatal(err)
	}

	first, err := LoadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(first); err != nil {
		t.Fatal(err)
	}

	if err := WriteKeyFile(path); err == nil {
		t.Fatal("WriteKeyFile must not overwrite an existing key")
	}

	second, err := LoadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("the key changed")
	}

	if err := os.WriteFile(path, []byte("too short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyFile(path); err == nil {
		t.Fatal("expected an error for a malformed key file")
	}
}

func TestDeriveKeyIsPerPurpose(t *testing.T) {
	master := bytes.Repeat([]byte{7}, KeySize)

	a, err := DeriveKey(master, "one")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := DeriveKey(master, "one")
	b, _ := DeriveKey(master, "two")

	if len(a) != KeySize || !bytes.Equal(a, again) {
		t.Error("the same purpose gives a different key")
	}
	if bytes.Equal(a, b) || bytes.Equal(a, master) {
		t.Error("purposes share a key, or a key is the master key")
	}
	if _, err := DeriveKey([]byte("short"), "one"); err == nil {
		t.Error("a short master key is accepted")
	}
}
