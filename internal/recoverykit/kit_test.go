package recoverykit

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	mrand "math/rand/v2"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/oldbear24/PocketServerManager/internal/secrets"
)

var update = flag.Bool("update", false, "write the frozen vector anew")

// keyOf is a master key whose bytes start at b and count up.
func keyOf(b byte) []byte {
	key := make([]byte, secrets.KeySize)
	for i := range key {
		key[i] = b + byte(i)
	}
	return key
}

var (
	vectorKey  = keyOf(0)
	otherKey   = keyOf(100)
	sealedAt   = time.Date(2026, 11, 17, 10, 2, 0, 0, time.UTC)
	passphrase = "correct horse battery staple"
	// the secrets a kit holds, which its text must not show
	targetPassword = "rest-pass-0b1c2d3e4f"
	minioSecret    = "minio-secret-9f8e7d6c5b"
	sshKey         = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----\n"
)

// seeded is a random stream that is the same for the same seed.
func seeded(seed string) io.Reader {
	var key [32]byte
	copy(key[:], seed)
	return mrand.NewChaCha8(key)
}

// repeatable makes salts, nonces and kit ids come from seed for the test.
func repeatable(t *testing.T, seed string) {
	t.Helper()
	was := random
	random = seeded(seed)
	t.Cleanup(func() { random = was })
}

// headerOf is the header of a kit of master.
func headerOf(master []byte, kit string) Header {
	return Header{Kit: kit, Created: sealedAt, Panel: "http://192.0.2.2:8090", KeyID: secrets.KeyID(master),
		CAPin: "sha256:" + strings.Repeat("5d", 32), PanelVersion: "0.32.0", AgentImage: "ghcr.io/oldbear24/pocketcloud-agent:0.32.0"}
}

// payloadOf is what a kit of master holds: two targets, one remote, one on a
// server's disk.
func payloadOf(master []byte) Payload {
	return Payload{
		CreatedBy: "admin@example.com",
		MasterKey: hex.EncodeToString(master),
		KeyID:     secrets.KeyID(master),
		Env: map[string]string{
			"POCKETCLOUD_PUBLIC_URL": "http://192.0.2.2:8090", "POCKETCLOUD_GRPC_ADDR": ":9443",
			"POCKETCLOUD_GRPC_PUBLIC_ADDR": "", "POCKETCLOUD_PHP_IMAGE": "php:8.3-fpm",
			"POCKETCLOUD_HTTPS_ADDR": "", "POCKETCLOUD_HTTPS_CHALLENGE": "",
		},
		AgentsDial: []Dial{{Address: "192.0.2.2:9443", Servers: 2, IP: true}, {Address: "panel.example.com:9443", Servers: 1}},
		Job:        &Job{ID: "abc123def456ghi", Name: "panel (pb_data)"},
		Targets: []Target{
			{ID: "tgtminio0000001", Name: "panel-minio", Kind: "s3", Repository: Repository{
				URL: "s3:http://192.0.2.2:9000/backups/panel32", Password: targetPassword,
				Env: map[string]string{"AWS_ACCESS_KEY_ID": "minio", "AWS_SECRET_ACCESS_KEY": minioSecret}, Options: map[string]string{"s3.bucket-lookup": "path"},
			}},
			{ID: "tgtdisk00000002", Name: "panel-disk", Kind: "local", Server: "vm", Repository: Repository{
				URL: "/repo", Password: targetPassword + "-disk", Mount: "/srv/pc-panel32", SSHKey: sshKey, KnownHosts: "[192.0.2.2]:2222 ssh-ed25519 AAAA",
			}},
		},
	}
}

// sealOf seals a kit of master with the passphrase.
func sealOf(t *testing.T, master []byte) string {
	t.Helper()
	text, err := Seal(headerOf(master, "KIT-7Q2M-9XHC"), payloadOf(master), passphrase)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// countArgon2 counts argon2's runs in the test.
func countArgon2(t *testing.T) *atomic.Int32 {
	t.Helper()
	var runs atomic.Int32
	was := argon2Key
	argon2Key = func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
		runs.Add(1)
		return was(password, salt, time, memory, threads, keyLen)
	}
	t.Cleanup(func() { argon2Key = was })
	return &runs
}

// TestARoundTripOpensWhatWasSealed: a kit opens with its passphrase into
// what was sealed, and its header reads without it; another passphrase
// does not open it.
func TestARoundTripOpensWhatWasSealed(t *testing.T) {
	text := sealOf(t, vectorKey)
	if !strings.HasPrefix(text, Preamble+Begin+"\nKit: KIT-7Q2M-9XHC\nCreated: 2026-11-17T10:02:00Z\nPanel: http://192.0.2.2:8090\n") ||
		!strings.Contains(text, "\nKDF: argon2id t=3 m=65536 p=4\n\n") || !strings.HasSuffix(text, "\n"+End+"\n") {
		t.Fatalf("the kit reads:\n%s", text)
	}
	for _, line := range strings.Split(strings.TrimSpace(text[strings.Index(text, "p=4\n\n")+5:strings.Index(text, End)]), "\n") {
		if len(line) > 64 {
			t.Errorf("a line of %d columns", len(line))
		}
	}

	header, err := ParseHeader(text)
	if err != nil {
		t.Fatal(err)
	}
	want := headerOf(vectorKey, "KIT-7Q2M-9XHC")
	want.KDF = DefaultKDF
	if header != want {
		t.Errorf("the header: %+v", header)
	}

	opened, payload, err := Open(text, passphrase)
	if err != nil {
		t.Fatal(err)
	}
	if opened != want {
		t.Errorf("the opened header: %+v", opened)
	}
	sealed := payloadOf(vectorKey)
	sealed.fill(want)
	if !reflect.DeepEqual(payload, sealed) {
		t.Errorf("the payload:\n%+v\nnot\n%+v", payload, sealed)
	}
	if key, err := payload.Key(); err != nil || !bytes.Equal(key, vectorKey) {
		t.Errorf("the key: %x, %v", key, err)
	}
	if payload.Format != PayloadFormat || payload.Version != 1 || payload.KitID != "KIT-7Q2M-9XHC" || payload.CreatedAt != "2026-11-17T10:02:00Z" {
		t.Errorf("the payload's own fields: %+v", payload)
	}
	if FileName(header) != "pocketcloud-recovery-kit-20261117-KIT-7Q2M-9XHC.pckit" {
		t.Errorf("the file name %s", FileName(header))
	}

	for _, wrong := range []string{"correct horse battery stapl", "Correct horse battery staple", passphrase + " ", ""} {
		if _, _, err := Open(text, wrong); !errors.Is(err, ErrPassphrase) {
			t.Errorf("%q: %v", wrong, err)
		}
	}
	// a kit opens whatever its line ends became
	if _, _, err := Open(strings.ReplaceAll(text, "\n", "\r\n"), passphrase); err != nil {
		t.Errorf("with CRLF: %v", err)
	}
	if _, _, err := Open("a note before it\n\n"+text+"\n\n", passphrase); err != nil {
		t.Errorf("with more around it: %v", err)
	}

	// no payload without its master key, or of another key than the header's
	noKey := payloadOf(vectorKey)
	noKey.MasterKey = "xyz"
	if _, err := Seal(headerOf(vectorKey, "KIT-7Q2M-9XHC"), noKey, passphrase); err == nil {
		t.Error("a payload without a key was sealed")
	}
	if _, err := Seal(headerOf(otherKey, "KIT-7Q2M-9XHC"), payloadOf(vectorKey), passphrase); err == nil {
		t.Error("a header of another key was sealed")
	}
}

// TestEveryHeaderLineIsSealed: changing any header line, to a value of its
// form, keeps the kit from opening: the lines are its additional data.
func TestEveryHeaderLineIsSealed(t *testing.T) {
	text := sealOf(t, vectorKey)
	for line, changed := range map[string]string{
		"Kit: KIT-7Q2M-9XHC":                                      "Kit: KIT-7Q2M-9XHD",
		"Created: 2026-11-17T10:02:00Z":                           "Created: 2026-11-17T10:02:01Z",
		"Panel: http://192.0.2.2:8090":                            "Panel: http://192.0.2.3:8090",
		"Key-Id: " + secrets.KeyID(vectorKey):                     "Key-Id: " + secrets.KeyID(otherKey),
		"CA-Pin: sha256:" + strings.Repeat("5d", 32):              "CA-Pin: sha256:" + strings.Repeat("5e", 32),
		"Panel-Version: 0.32.0":                                   "Panel-Version: 0.32.1",
		"Agent-Image: ghcr.io/oldbear24/pocketcloud-agent:0.32.0": "Agent-Image: ghcr.io/oldbear24/pocketcloud-agent:0.31.0",
		"KDF: argon2id t=3 m=65536 p=4":                           "KDF: argon2id t=3 m=65536 p=2",
	} {
		if !strings.Contains(text, "\n"+line+"\n") {
			t.Fatalf("the kit has no line %q", line)
		}
		edited := strings.Replace(text, "\n"+line+"\n", "\n"+changed+"\n", 1)
		if _, err := ParseHeader(edited); err != nil {
			t.Fatalf("%q is not of its form: %v", changed, err)
		}
		if _, _, err := Open(edited, passphrase); !errors.Is(err, ErrPassphrase) {
			t.Errorf("with %q: %v", changed, err)
		}
	}
	// the sealed part, changed anywhere
	body := strings.Index(text, "p=4\n\n") + 5
	for _, at := range []int{body, body + 30, body + 63, strings.Index(text, End) - 3} {
		edited := []byte(text)
		if edited[at] == 'A' {
			edited[at] = 'B'
		} else {
			edited[at] = 'A'
		}
		if _, _, err := Open(string(edited), passphrase); err == nil {
			t.Errorf("a change at %d went unnoticed", at)
		}
	}
}

// TestTheKitsFormIsStrict: what this package would not write is no kit,
// before any passphrase is tried.
func TestTheKitsFormIsStrict(t *testing.T) {
	runs := countArgon2(t)
	text := sealOf(t, vectorKey)
	runs.Store(0)
	lines := strings.Split(text, "\n")
	begin := slices.Index(lines, Begin)
	edit := func(f func(lines []string) []string) string {
		return strings.Join(f(slices.Clone(lines)), "\n")
	}
	for name, kit := range map[string]string{
		"an unknown line":      edit(func(l []string) []string { return slices.Insert(l, begin+3, "Comment: hello") }),
		"a line twice":         edit(func(l []string) []string { return slices.Insert(l, begin+3, l[begin+3]) }),
		"two lines swapped":    edit(func(l []string) []string { l[begin+1], l[begin+2] = l[begin+2], l[begin+1]; return l }),
		"a line missing":       edit(func(l []string) []string { return slices.Delete(l, begin+4, begin+5) }),
		"a key in lower case":  edit(func(l []string) []string { l[begin+1] = strings.ToLower(l[begin+1][:3]) + l[begin+1][3:]; return l }),
		"a space more":         edit(func(l []string) []string { l[begin+3] = strings.Replace(l[begin+3], ": ", ":  ", 1); return l }),
		"a space at the end":   edit(func(l []string) []string { l[begin+6] += " "; return l }),
		"no BEGIN":             strings.Replace(text, Begin, "-----BEGIN PGP MESSAGE-----", 1),
		"no END":               strings.Replace(text, End, "", 1),
		"more after END":       text + "Kit: KIT-0000-0000\n",
		"another kit after it": text + text,
		"no blank line":        edit(func(l []string) []string { return slices.Delete(l, begin+9, begin+10) }),
		"no sealed part":       edit(func(l []string) []string { return append(l[:begin+10], End, "") }),
		"no base64":            edit(func(l []string) []string { l[begin+10] = "!!" + l[begin+10][2:]; return l }),
		"too short":            edit(func(l []string) []string { return append(l[:begin+10], "QUJD", End, "") }),
		"a kit id of 0 and 1":  strings.Replace(text, "KIT-7Q2M-9XHC", "KIT-7Q2M-9XHI", 1),
		"a kit id in lower":    strings.Replace(text, "KIT-7Q2M-9XHC", "KIT-7q2m-9xhc", 1),
		"a time with a zone":   strings.Replace(text, "2026-11-17T10:02:00Z", "2026-11-17T11:02:00+01:00", 1),
		"a time with nanos":    strings.Replace(text, "2026-11-17T10:02:00Z", "2026-11-17T10:02:00.5Z", 1),
		"a key id in upper":    strings.Replace(text, secrets.KeyID(vectorKey), strings.ToUpper(secrets.KeyID(vectorKey)), 1),
		"a short key id":       strings.Replace(text, secrets.KeyID(vectorKey), secrets.KeyID(vectorKey)[:30], 1),
		"a pin of md5":         strings.Replace(text, "sha256:"+strings.Repeat("5d", 32), "md5:"+strings.Repeat("5d", 16), 1),
		"an empty version":     strings.Replace(text, "Panel-Version: 0.32.0", "Panel-Version: ", 1),
		"a KDF of scrypt":      strings.Replace(text, "argon2id t=3 m=65536 p=4", "scrypt N=32768 r=8 p=1", 1),
		"a KDF with a zero":    strings.Replace(text, "argon2id t=3 m=65536 p=4", "argon2id t=03 m=65536 p=4", 1),
		"too large":            text + strings.Repeat("\n", MaxSize),
		"not UTF-8":            strings.Replace(text, "Panel: http", "Panel: \xffhttp", 1),
		"nothing":              "",
		"a PocketCloud backup": "PCPB\x01\x00\x00\x00",
	} {
		if _, err := ParseHeader(kit); !errors.Is(err, ErrNotAKit) {
			t.Errorf("%s: ParseHeader gives %v", name, err)
		}
		if _, _, err := Open(kit, passphrase); !errors.Is(err, ErrNotAKit) {
			t.Errorf("%s: Open gives %v", name, err)
		}
	}
	if runs.Load() != 0 {
		t.Errorf("argon2 ran %d times for kits that are none", runs.Load())
	}
}

// TestKDFBoundsAreCheckedBeforeArgon2: a kit whose KDF would cost its opener
// too much, or protect it too little, is refused before argon2 runs.
func TestKDFBoundsAreCheckedBeforeArgon2(t *testing.T) {
	text := sealOf(t, vectorKey)
	runs := countArgon2(t)
	for _, kdf := range []string{
		"argon2id t=2 m=65536 p=4", "argon2id t=11 m=65536 p=4", "argon2id t=3 m=65535 p=4", "argon2id t=3 m=1048577 p=4",
		"argon2id t=3 m=65536 p=0", "argon2id t=3 m=65536 p=17", "argon2id t=4294967295 m=4294967295 p=255", "argon2id t=99999999999 m=65536 p=4",
	} {
		kit := strings.Replace(text, "KDF: argon2id t=3 m=65536 p=4", "KDF: "+kdf, 1)
		if _, _, err := Open(kit, passphrase); !errors.Is(err, ErrNotAKit) {
			t.Errorf("%s: %v", kdf, err)
		}
		if _, err := ParseHeader(kit); !errors.Is(err, ErrNotAKit) {
			t.Errorf("%s: ParseHeader gives %v", kdf, err)
		}
	}
	if runs.Load() != 0 {
		t.Fatalf("argon2 ran %d times", runs.Load())
	}
	h := headerOf(vectorKey, "KIT-7Q2M-9XHC")
	h.KDF = KDF{Time: 2, Memory: 65536, Threads: 4}
	if _, err := Seal(h, payloadOf(vectorKey), passphrase); !errors.Is(err, ErrNotAKit) || runs.Load() != 0 {
		t.Errorf("a kit sealed with t=2: %v", err)
	}

	// the bounds themselves are taken: argon2 runs, here without its cost
	argon2Key = func(_, _ []byte, _, _ uint32, _ uint8, keyLen uint32) []byte {
		runs.Add(1)
		return make([]byte, keyLen)
	}
	for _, kdf := range []string{"argon2id t=3 m=65536 p=1", "argon2id t=10 m=1048576 p=16"} {
		kit := strings.Replace(text, "KDF: argon2id t=3 m=65536 p=4", "KDF: "+kdf, 1)
		if _, _, err := Open(kit, passphrase); !errors.Is(err, ErrPassphrase) {
			t.Errorf("%s: %v", kdf, err)
		}
	}
	if runs.Load() != 2 {
		t.Errorf("argon2 ran %d times for the bounds", runs.Load())
	}
}

// TestThePassphraseIsNFC: a passphrase opens the kit in NFC and NFD alike;
// it needs 12 characters, counted in NFC, and 1024 bytes at most.
func TestThePassphraseIsNFC(t *testing.T) {
	horse := "Příliš žluťoučký kůň"
	nfc, nfd := norm.NFC.String(horse), norm.NFD.String(horse)
	if nfc == nfd {
		t.Fatal("the forms are the same")
	}
	for name, c := range map[string]struct{ seal, open string }{"NFC then NFD": {nfc, nfd}, "NFD then NFC": {nfd, nfc}} {
		text, err := Seal(headerOf(vectorKey, "KIT-7Q2M-9XHC"), payloadOf(vectorKey), c.seal)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := Open(text, c.open); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	for passphrase, want := range map[string]error{
		"abcdefghijk":             ErrShortPassphrase,
		"abcdefghijkl":            nil,
		"žžžžžžžžžžž":             ErrShortPassphrase, // 22 bytes, 11 characters
		strings.Repeat("é", 11):  ErrShortPassphrase, // 22 runes in NFD, 11 characters in NFC
		strings.Repeat("é", 12):  nil,
		strings.Repeat("x", 1024): nil,
		strings.Repeat("x", 1025): ErrLongPassphrase,
		strings.Repeat("ž", 513):  ErrLongPassphrase, // 1026 bytes
		"":                        ErrShortPassphrase,
		"            ":            nil,
	} {
		if _, err := Passphrase(passphrase); !errors.Is(err, want) {
			t.Errorf("%.20q: %v, not %v", passphrase, err, want)
		}
	}
	if _, err := Seal(headerOf(vectorKey, "KIT-7Q2M-9XHC"), payloadOf(vectorKey), "short"); !errors.Is(err, ErrShortPassphrase) {
		t.Errorf("a short passphrase: %v", err)
	}

	// a passphrase over the limit opens nothing, without argon2
	text := sealOf(t, vectorKey)
	runs := countArgon2(t)
	if _, _, err := Open(text, strings.Repeat("x", 1025)); !errors.Is(err, ErrPassphrase) || runs.Load() != 0 {
		t.Errorf("a passphrase of 1025 bytes: %v, %d runs", err, runs.Load())
	}
}

// TestTheSealedContentIsStrict: a payload with a field this package does not
// know, a second value, another format, or values the header does not
// repeat, is no kit, though it opens.
func TestTheSealedContentIsStrict(t *testing.T) {
	h := headerOf(vectorKey, "KIT-7Q2M-9XHC")
	h.KDF = DefaultKDF
	good := payloadOf(vectorKey)
	good.fill(h)
	raw, _ := jsonOf(good)
	pass, _ := Passphrase(passphrase)
	for name, c := range map[string]struct {
		raw  []byte
		want string
	}{
		"an unknown field":       {[]byte(strings.Replace(string(raw), `{"format"`, `{"note":"x","format"`, 1)), "unknown field"},
		"a second value":         {append(bytes.Clone(raw), raw...), "not a kit's payload"},
		"no JSON":                {[]byte("master key"), "not a kit's payload"},
		"another format":         {edited(t, good, func(p *Payload) { p.Format = "pocketbase" }), "of the format"},
		"another version":        {edited(t, good, func(p *Payload) { p.Version = 2 }), "version 2"},
		"another kit id":         {edited(t, good, func(p *Payload) { p.KitID = "KIT-0000-0000" }), "says Kit"},
		"another time":           {edited(t, good, func(p *Payload) { p.CreatedAt = "2026-11-17T10:02:01Z" }), "says Created"},
		"another panel":          {edited(t, good, func(p *Payload) { p.Panel = "http://elsewhere" }), "says Panel"},
		"another key id":         {edited(t, good, func(p *Payload) { p.KeyID = secrets.KeyID(otherKey) }), "says Key-Id"},
		"another pin":            {edited(t, good, func(p *Payload) { p.CAPin = "sha256:" + strings.Repeat("00", 32) }), "says CA-Pin"},
		"another version of it":  {edited(t, good, func(p *Payload) { p.PanelVersion = "0.31.0" }), "says Panel-Version"},
		"another agent image":    {edited(t, good, func(p *Payload) { p.AgentImage = "agent:old" }), "says Agent-Image"},
		"another master key":     {edited(t, good, func(p *Payload) { p.MasterKey = hex.EncodeToString(otherKey) }), "not the key"},
		"a master key of no hex": {edited(t, good, func(p *Payload) { p.MasterKey = "zz" }), "hex characters"},
	} {
		text, err := seal(h, c.raw, pass)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = Open(text, passphrase)
		if !errors.Is(err, ErrNotAKit) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// jsonOf is a payload's JSON.
var jsonOf = json.Marshal

// edited is a payload's JSON, changed.
func edited(t *testing.T, p Payload, change func(*Payload)) []byte {
	t.Helper()
	change(&p)
	raw, err := jsonOf(p)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestTheKitsTextHoldsNoSecret: a kit's text shows none of what it seals.
func TestTheKitsTextHoldsNoSecret(t *testing.T) {
	text := sealOf(t, vectorKey)
	for _, secret := range []string{hex.EncodeToString(vectorKey), targetPassword, minioSecret, "b3BlbnNzaC1rZXktdjEAAAAA", passphrase,
		base64.StdEncoding.EncodeToString(vectorKey)[:20], "AWS_SECRET_ACCESS_KEY", "s3:http", "admin@example.com"} {
		if strings.Contains(text, secret) || strings.Contains(strings.ReplaceAll(text, "\n", ""), secret) {
			t.Errorf("the kit shows %q", secret)
		}
	}
}

// TestDigestsNameWhatChanged: the digests are the same for the same payload
// sealed twice, change with each section's input alone, and differ under
// another master key; what they leave out changes none.
func TestDigestsNameWhatChanged(t *testing.T) {
	base := payloadOf(vectorKey)
	first, err := Digests(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range Sections {
		if len(first[section]) != 32 {
			t.Errorf("%s: %q", section, first[section])
		}
	}
	if len(first) != 4 {
		t.Errorf("the digests: %v", first)
	}

	// two kits of the same panel
	_, a, err := Open(sealOf(t, vectorKey), passphrase)
	if err != nil {
		t.Fatal(err)
	}
	_, b, _ := Open(sealOf(t, vectorKey), passphrase)
	da, _ := Digests(a)
	db, _ := Digests(b)
	if !reflect.DeepEqual(da, db) || !reflect.DeepEqual(da, first) {
		t.Errorf("two seals: %v, %v, %v", da, db, first)
	}

	for name, c := range map[string]struct {
		change func(*Payload)
		want   []string
	}{
		"the start environment":  {func(p *Payload) { p.Env["POCKETCLOUD_GRPC_PUBLIC_ADDR"] = "panel.example.com:9443" }, []string{SectionAddress}},
		"an address more":        {func(p *Payload) { p.AgentsDial = append(p.AgentsDial, Dial{Address: "10.0.0.2:9443"}) }, []string{SectionAddress}},
		"another job":            {func(p *Payload) { p.Job = &Job{ID: "zzz123def456ghi", Name: p.Job.Name} }, []string{SectionJob}},
		"no job":                 {func(p *Payload) { p.Job = nil }, []string{SectionJob}},
		"a target's password":    {func(p *Payload) { p.Targets[0].Repository.Password = "rotated" }, []string{SectionTargets}},
		"a target's credentials": {func(p *Payload) { p.Targets[0].Repository.Env["AWS_SECRET_ACCESS_KEY"] = "rotated" }, []string{SectionTargets}},
		"a target's kind":        {func(p *Payload) { p.Targets[1].Kind = "sftp" }, []string{SectionTargets}},
		"a target less":          {func(p *Payload) { p.Targets = p.Targets[:1] }, []string{SectionJob, SectionTargets}},
		"another key's kit":      {func(p *Payload) { p.MasterKey, p.KeyID = hex.EncodeToString(otherKey), secrets.KeyID(otherKey) }, Sections},
		"the dial counts":        {func(p *Payload) { p.AgentsDial[0].Servers = 9; p.AgentsDial[1].IP = true }, nil},
		"the addresses' order":   {func(p *Payload) { slices.Reverse(p.AgentsDial) }, nil},
		"the targets' order":     {func(p *Payload) { slices.Reverse(p.Targets) }, nil},
		"the names":              {func(p *Payload) { p.Job.Name, p.Targets[0].Name, p.Targets[1].Server = "renamed", "renamed", "node-b" }, nil},
		"who and when":           {func(p *Payload) { p.CreatedBy, p.CreatedAt, p.KitID = "other@example.com", "x", "KIT-0000-0000" }, nil},
		"empty maps for nil ones": {func(p *Payload) {
			p.Targets[1].Repository.Env, p.Targets[1].Repository.Options = nil, map[string]string{}
		}, nil},
	} {
		p := payloadOf(vectorKey)
		c.change(&p)
		now, err := Digests(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := Changed(first, now); !slices.Equal(got, c.want) {
			t.Errorf("%s changes %v, not %v", name, got, c.want)
		}
	}
	if got := Changed(nil, first); !slices.Equal(got, Sections) {
		t.Errorf("a kit of no digests: %v", got)
	}
	if _, err := Digests(Payload{MasterKey: "short"}); err == nil {
		t.Error("digests without a key")
	}
}

// TestKitIDsAreCrockford: a kit id is 40 random bits in Crockford's base32.
func TestKitIDsAreCrockford(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		id, err := NewKitID()
		if err != nil || !kitIDForm.MatchString(id) || seen[id] {
			t.Fatalf("%q, %v", id, err)
		}
		seen[id] = true
	}
}

// The frozen vector: the master key 00 01 … 1f, the passphrase
// "Příliš žluťoučký kůň" in NFC, salts and nonce from ChaCha8 seeded
// "pckit1 frozen vector", sealed 2026-11-17 10:02 UTC. A kit must open as
// long as PCKIT1 exists; Seal must write it byte for byte.
const vectorPath = "testdata/pckit1.vector"

// vectorDigests are the frozen vector's digests.
var vectorDigests = map[string]string{
	SectionKey: "4de86bd4948abae0736cb3007293d3e0", SectionAddress: "71761357874ae8860774959468cc7a8e",
	SectionJob: "4f50dcd4548608c37ce262f54e88c51e", SectionTargets: "89c18c5b8581984f6cd9d2be6cdbb08e",
}

func TestAFrozenVectorPinsTheKit(t *testing.T) {
	repeatable(t, "pckit1 frozen vector")
	id, err := NewKitID()
	if err != nil {
		t.Fatal(err)
	}
	built, err := Seal(headerOf(vectorKey, id), payloadOf(vectorKey), "Příliš žluťoučký kůň")
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile(vectorPath, []byte(built), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	frozen, err := os.ReadFile(vectorPath)
	if err != nil {
		t.Fatal(err)
	}
	header, payload, err := Open(string(frozen), norm.NFD.String("Příliš žluťoučký kůň"))
	if err != nil {
		t.Fatalf("the frozen vector does not open: %v", err)
	}
	if header.Kit != "KIT-FMH6-NQDX" || header.KeyID != "17ac82d879a43a9e72720c907de8857d" || header.KDF != DefaultKDF {
		t.Errorf("the frozen vector's header: %+v", header)
	}
	want := payloadOf(vectorKey)
	want.fill(header)
	if !reflect.DeepEqual(payload, want) {
		t.Errorf("the frozen vector holds %+v", payload)
	}
	digests, _ := Digests(payload)
	if !reflect.DeepEqual(digests, vectorDigests) {
		t.Errorf("the frozen vector's digests are %v", digests)
	}
	if built != string(frozen) {
		t.Error("Seal no longer writes the frozen vector: the format changed")
	}
}
