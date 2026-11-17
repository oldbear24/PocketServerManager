package secrets

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
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

// The id of a key 00 01 … 1f, as HKDF-SHA256 with no salt and the info
// "pocketcloud key id v1" gives it outside Go too: databases, panel backups
// and recovery kits keep it, so it can never change.
const keyIDVector = "17ac82d879a43a9e72720c907de8857d"

func TestKeyIDIsStableAndNotADerivedKey(t *testing.T) {
	master := make([]byte, KeySize)
	for i := range master {
		master[i] = byte(i)
	}

	id := KeyID(master)
	if id != keyIDVector {
		t.Fatalf("the key id is %s, not %s", id, keyIDVector)
	}
	if again := KeyID(bytes.Clone(master)); again != id {
		t.Error("the key id is not stable")
	}

	other := bytes.Clone(master)
	other[31] ^= 1
	if KeyID(other) == id || len(KeyID(other)) != 32 {
		t.Error("another key has the same id, or an id of another size")
	}

	// the id gives away neither the key nor a key derived for any purpose
	for _, purpose := range []string{"pocketcloud panel backup v1", "pocketcloud recovery kit digest v1", "pocketcloud audit chain head", ""} {
		derived, err := DeriveKey(master, purpose)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(hex.EncodeToString(derived), id) {
			t.Errorf("the id is part of the key derived for %q", purpose)
		}
	}
	if strings.Contains(hex.EncodeToString(master), id) {
		t.Error("the id is part of the key")
	}

	for _, size := range []int{0, 16, 31, 33} {
		if KeyID(make([]byte, size)) != "" {
			t.Errorf("a key of %d bytes has an id", size)
		}
	}
}

func TestWriteKeyNeverOverwritesAnotherKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	key, _ := GenerateKey()

	if err := WriteKey(path, key); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the key file has mode %o", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != hex.EncodeToString(key)+"\n" {
		t.Errorf("the key file holds %q", raw)
	}
	if loaded, err := LoadKeyFile(path); err != nil || !bytes.Equal(loaded, key) {
		t.Errorf("the key reads back as %x, %v", loaded, err)
	}

	// the same key again is fine, as hex or as base64
	if err := WriteKey(path, key); err != nil {
		t.Errorf("writing the same key again: %v", err)
	}
	b64 := filepath.Join(dir, "base64.key")
	if err := os.WriteFile(b64, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteKey(b64, key); err != nil {
		t.Errorf("a base64 file of the same key: %v", err)
	}

	// another key, or no key, is never overwritten
	other, _ := GenerateKey()
	if err := WriteKey(path, other); !errors.Is(err, ErrKeyExists) {
		t.Errorf("writing another key: %v", err)
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(again, raw) {
		t.Error("another key changed the file")
	}
	junk := filepath.Join(dir, "junk.key")
	if err := os.WriteFile(junk, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteKey(junk, key); !errors.Is(err, ErrKeyExists) {
		t.Errorf("a file of no key: %v", err)
	}
	if again, _ := os.ReadFile(junk); string(again) != "not a key\n" {
		t.Error("a file of no key was overwritten")
	}
	empty := filepath.Join(dir, "empty.key")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteKey(empty, key); !errors.Is(err, ErrKeyExists) {
		t.Errorf("an empty file: %v", err)
	}

	// a key of another size is no key, and no file is left
	short := filepath.Join(dir, "short.key")
	if err := WriteKey(short, key[:31]); err == nil {
		t.Error("a key of 31 bytes was written")
	}
	if _, err := os.Stat(short); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused key left a file: %v", err)
	}
	if err := WriteKey(filepath.Join(dir, "missing", "master.key"), key); err == nil || errors.Is(err, ErrKeyExists) {
		t.Errorf("a directory that does not exist: %v", err)
	}
}
