// Package recoverykit is PCKIT1, the recovery kit of a panel (#32): the
// master key, and what reaches and opens the panel's backup, sealed with a
// passphrase into a text file an admin keeps off the panel's machine.
//
// A kit is UTF-8 text. A preamble, which the parser ignores, says what it
// is; then, between the BEGIN and END lines, eight header lines in a fixed
// order, a blank line, and the sealed payload as base64 in lines of 64:
//
//	-----BEGIN POCKETCLOUD RECOVERY KIT-----
//	Kit: KIT-7Q2M-9XHC
//	Created: 2026-11-17T10:02:00Z
//	Panel: http://192.0.2.2:8090
//	Key-Id: <32 hex>
//	CA-Pin: sha256:<64 hex>
//	Panel-Version: 0.32.0
//	Agent-Image: <the agent's image>
//	KDF: argon2id t=3 m=65536 p=4
//
//	<base64: salt[16] | nonce[12] | AES-256-GCM ciphertext and tag>
//	-----END POCKETCLOUD RECOVERY KIT-----
//
// The key is argon2id of the passphrase in Unicode NFC, with the salt and
// the KDF line's parameters; the additional data is the header lines, each
// ended by a line feed, exactly as written. The payload is JSON (Payload),
// which repeats the header's values: they are compared once it opened.
//
// See docs/architecture.md, "Panel backup and disaster recovery (#32)".
package recoverykit

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/text/unicode/norm"

	"github.com/oldbear24/PocketServerManager/internal/secrets"
)

// The form of a kit.
const (
	Begin = "-----BEGIN POCKETCLOUD RECOVERY KIT-----"
	End   = "-----END POCKETCLOUD RECOVERY KIT-----"
	// Preamble is what a kit says of itself before its BEGIN line.
	Preamble = "This file restores a PocketCloud panel: docs/disaster-recovery.md. It is sealed with a passphrase.\n" +
		"Keep it off the panel's machine, apart from its passphrase and from the backups.\n"
	// MaxSize is the largest kit, in bytes.
	MaxSize = 64 << 10
	// MinPassphrase is the fewest characters of a passphrase, in NFC.
	MinPassphrase = 12
	// MaxPassphrase is the most bytes of a passphrase, in NFC.
	MaxPassphrase = 1024
	// FileExtension is the extension of a kit's file.
	FileExtension = ".pckit"
	// PayloadFormat names the payload's format.
	PayloadFormat = "pocketcloud-recovery-kit"
	// Version is the version of the payload this package writes and reads.
	Version = 1

	saltSize  = 16
	nonceSize = 12
	keySize   = 32
	columns   = 64
)

// The header lines of a kit, in their order.
var headerKeys = []string{"Kit", "Created", "Panel", "Key-Id", "CA-Pin", "Panel-Version", "Agent-Image", "KDF"}

var (
	// ErrNotAKit is text that is no recovery kit, or one whose header or
	// sealed content is not as this package writes it.
	ErrNotAKit = errors.New("this is no PocketCloud recovery kit")
	// ErrPassphrase is a passphrase that does not open the kit, or a kit
	// that was changed: neither can be told from the other.
	ErrPassphrase = errors.New("the passphrase does not open this kit, or the kit was changed")
	// ErrShortPassphrase is a passphrase of fewer than MinPassphrase
	// characters.
	ErrShortPassphrase = fmt.Errorf("the passphrase needs at least %d characters", MinPassphrase)
	// ErrLongPassphrase is a passphrase of more than MaxPassphrase bytes.
	ErrLongPassphrase = fmt.Errorf("the passphrase is at most %d bytes", MaxPassphrase)
)

// notAKit is ErrNotAKit, saying why.
func notAKit(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotAKit, fmt.Sprintf(format, args...))
}

// KDF is the argon2id parameters of a kit: passes, memory in KiB, and
// threads.
type KDF struct {
	Time    uint32
	Memory  uint32
	Threads uint8
}

// DefaultKDF is RFC 9106's second recommendation: 3 passes over 64 MiB with
// 4 threads.
var DefaultKDF = KDF{Time: 3, Memory: 64 << 10, Threads: 4}

// The bounds a kit's KDF is checked against before argon2 runs.
const (
	MinTime    = 3
	MaxTime    = 10
	MinMemory  = 64 << 10
	MaxMemory  = 1 << 20
	MinThreads = 1
	MaxThreads = 16
)

func (k KDF) String() string {
	return fmt.Sprintf("argon2id t=%d m=%d p=%d", k.Time, k.Memory, k.Threads)
}

// check refuses parameters out of bounds: a kit cannot make its opener
// spend more, or protect itself less, than they allow.
func (k KDF) check() error {
	if k.Time < MinTime || k.Time > MaxTime || k.Memory < MinMemory || k.Memory > MaxMemory || k.Threads < MinThreads || k.Threads > MaxThreads {
		return notAKit("its KDF %s is outside t %d..%d, m %d..%d, p %d..%d", k, MinTime, MaxTime, MinMemory, MaxMemory, MinThreads, MaxThreads)
	}
	return nil
}

// Header is a kit's header lines, which anyone reads without the passphrase.
type Header struct {
	// Kit is the kit's id, KIT-XXXX-XXXX.
	Kit string
	// Created is when the panel sealed it, to the second, in UTC.
	Created time.Time
	// Panel is the panel's address, as people reach it.
	Panel string
	// KeyID is secrets.KeyID of the master key the kit holds.
	KeyID string
	// CAPin is the pin of the panel's internal CA, "sha256:…".
	CAPin        string
	PanelVersion string
	AgentImage   string
	KDF          KDF
}

var (
	kitIDForm = regexp.MustCompile(`^KIT-[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`)
	keyIDForm = regexp.MustCompile(`^[0-9a-f]{32}$`)
	pinForm   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	kdfForm   = regexp.MustCompile(`^argon2id t=([0-9]{1,10}) m=([0-9]{1,10}) p=([0-9]{1,3})$`)
)

// Crockford's alphabet, as enrollment tokens use it: no I, L, O or U.
var crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// random is where salts, nonces and kit ids come from; a test makes it
// repeatable.
var random io.Reader = rand.Reader

// argon2Key derives a kit's key; a test counts its calls.
var argon2Key = argon2.IDKey

// NewKitID returns a new kit id: 40 random bits in Crockford's base32.
func NewKitID() (string, error) {
	raw := make([]byte, 5)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", err
	}
	id := crockford.EncodeToString(raw)
	return "KIT-" + id[:4] + "-" + id[4:], nil
}

// lines is the header's lines, without their line feeds.
func (h Header) lines() []string {
	return []string{
		"Kit: " + h.Kit,
		"Created: " + h.Created.UTC().Format(time.RFC3339),
		"Panel: " + h.Panel,
		"Key-Id: " + h.KeyID,
		"CA-Pin: " + h.CAPin,
		"Panel-Version: " + h.PanelVersion,
		"Agent-Image: " + h.AgentImage,
		"KDF: " + h.KDF.String(),
	}
}

// aad is the additional data of the payload: the header lines, each ended by
// a line feed.
func (h Header) aad() []byte {
	return []byte(strings.Join(h.lines(), "\n") + "\n")
}

// check refuses a header this package would not write or read.
func (h Header) check() error {
	switch {
	case !kitIDForm.MatchString(h.Kit):
		return notAKit("its id %.40q is not KIT-XXXX-XXXX", h.Kit)
	case h.Created.Unix() <= 0 || h.Created.Nanosecond() != 0:
		return notAKit("it was made at %s", h.Created)
	case !keyIDForm.MatchString(h.KeyID):
		return notAKit("its key id %.40q is not 32 hex characters", h.KeyID)
	case !pinForm.MatchString(h.CAPin):
		return notAKit("its CA pin %.80q is not sha256: and 64 hex characters", h.CAPin)
	}
	for name, value := range map[string]string{"Panel": h.Panel, "Panel-Version": h.PanelVersion, "Agent-Image": h.AgentImage} {
		if value == "" || len(value) > 300 || !utf8.ValidString(value) || strings.TrimSpace(value) != value ||
			strings.ContainsFunc(value, unicode.IsControl) {
			return notAKit("its %s %.80q is empty, too long, or not one line", name, value)
		}
	}
	return h.KDF.check()
}

// Passphrase is a passphrase as a kit takes it: in NFC, at least
// MinPassphrase characters and at most MaxPassphrase bytes.
func Passphrase(passphrase string) (string, error) {
	normal := norm.NFC.String(passphrase)
	if len(normal) > MaxPassphrase {
		return "", ErrLongPassphrase
	}
	if utf8.RuneCountInString(normal) < MinPassphrase {
		return "", ErrShortPassphrase
	}
	return normal, nil
}

// Seal seals a payload under a header with a passphrase, and returns the
// kit's text. It sets the payload's format, version, and the values the
// header repeats; the header's KDF defaults to DefaultKDF.
func Seal(h Header, p Payload, passphrase string) (string, error) {
	pass, err := Passphrase(passphrase)
	if err != nil {
		return "", err
	}
	if h.KDF == (KDF{}) {
		h.KDF = DefaultKDF
	}
	h.Created = time.Unix(h.Created.Unix(), 0).UTC()
	if err := h.check(); err != nil {
		return "", err
	}
	key, err := p.Key()
	if err != nil {
		return "", err
	}
	if secrets.KeyID(key) != h.KeyID {
		return "", fmt.Errorf("the header names the key %s, the payload holds the key %s", h.KeyID, secrets.KeyID(key))
	}
	p.fill(h)
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return seal(h, raw, pass)
}

// seal seals raw, a payload's JSON, under a header with a passphrase in NFC.
func seal(h Header, raw []byte, pass string) (string, error) {
	salt := make([]byte, saltSize+nonceSize)
	if _, err := io.ReadFull(random, salt); err != nil {
		return "", err
	}
	aead, err := kitAEAD(pass, salt[:saltSize], h.KDF)
	if err != nil {
		return "", err
	}
	sealed := aead.Seal(salt, salt[saltSize:], raw, h.aad())

	var out strings.Builder
	out.WriteString(Preamble)
	out.WriteString(Begin + "\n")
	out.Write(h.aad())
	out.WriteString("\n")
	body := base64.StdEncoding.EncodeToString(sealed)
	for len(body) > columns {
		out.WriteString(body[:columns] + "\n")
		body = body[columns:]
	}
	out.WriteString(body + "\n")
	out.WriteString(End + "\n")
	if out.Len() > MaxSize {
		return "", fmt.Errorf("the kit takes %d bytes, more than %d", out.Len(), MaxSize)
	}
	return out.String(), nil
}

// kitAEAD is the cipher of a kit's payload.
func kitAEAD(pass string, salt []byte, kdf KDF) (cipher.AEAD, error) {
	block, err := aes.NewCipher(argon2Key([]byte(pass), salt, kdf.Time, kdf.Memory, kdf.Threads, keySize))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// parsed is a kit's text taken apart.
type parsed struct {
	header Header
	sealed []byte
}

// ParseHeader reads a kit's header, which needs no passphrase: it checks
// the kit's form, and the KDF's bounds.
func ParseHeader(text string) (Header, error) {
	p, err := parse(text)
	return p.header, err
}

// parse takes a kit's text apart, and refuses what this package would not
// write: unknown, doubled, missing or reordered header lines, values not of
// their form, a KDF out of bounds, a body that is not base64, anything but
// blank lines after the END line.
func parse(text string) (parsed, error) {
	if len(text) > MaxSize {
		return parsed{}, notAKit("it takes %d bytes, more than %d", len(text), MaxSize)
	}
	if !utf8.ValidString(text) {
		return parsed{}, notAKit("it is not UTF-8 text")
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}
	begin := -1
	for i, line := range lines {
		if line == Begin {
			begin = i
			break
		}
	}
	if begin < 0 {
		return parsed{}, notAKit("it has no line %s", Begin)
	}
	lines = lines[begin+1:]
	if len(lines) < len(headerKeys)+3 {
		return parsed{}, notAKit("it ends before its sealed part")
	}

	values := make([]string, len(headerKeys))
	for i, key := range headerKeys {
		value, ok := strings.CutPrefix(lines[i], key+": ")
		if !ok {
			return parsed{}, notAKit("its header line %d is %.60q, not %s", i+1, lines[i], key)
		}
		values[i] = value
	}
	if lines[len(headerKeys)] != "" {
		return parsed{}, notAKit("its header has a line more: %.60q", lines[len(headerKeys)])
	}

	var body strings.Builder
	end := -1
	for i, line := range lines[len(headerKeys)+1:] {
		if line == End {
			end = len(headerKeys) + 1 + i
			break
		}
		body.WriteString(line)
	}
	if end < 0 {
		return parsed{}, notAKit("it has no line %s", End)
	}
	for _, line := range lines[end+1:] {
		if strings.TrimSpace(line) != "" {
			return parsed{}, notAKit("something follows its END line")
		}
	}
	sealed, err := base64.StdEncoding.Strict().DecodeString(body.String())
	if err != nil || len(sealed) < saltSize+nonceSize+16 {
		return parsed{}, notAKit("its sealed part is not base64 of a sealed payload")
	}

	created, err := time.Parse(time.RFC3339, values[1])
	if err != nil || created.UTC().Format(time.RFC3339) != values[1] {
		return parsed{}, notAKit("its time %.40q is not RFC 3339 in UTC", values[1])
	}
	kdf, err := parseKDF(values[7])
	if err != nil {
		return parsed{}, err
	}
	h := Header{Kit: values[0], Created: created.UTC(), Panel: values[2], KeyID: values[3], CAPin: values[4],
		PanelVersion: values[5], AgentImage: values[6], KDF: kdf}
	if err := h.check(); err != nil {
		return parsed{}, err
	}
	return parsed{header: h, sealed: sealed}, nil
}

// parseKDF reads the KDF line, as String writes it and no other way.
func parseKDF(line string) (KDF, error) {
	match := kdfForm.FindStringSubmatch(line)
	if match == nil {
		return KDF{}, notAKit("its KDF %.60q is not argon2id t=… m=… p=…", line)
	}
	t, errT := strconv.ParseUint(match[1], 10, 32)
	m, errM := strconv.ParseUint(match[2], 10, 32)
	p, errP := strconv.ParseUint(match[3], 10, 8)
	kdf := KDF{Time: uint32(t), Memory: uint32(m), Threads: uint8(p)}
	if errT != nil || errM != nil || errP != nil || kdf.String() != line {
		return KDF{}, notAKit("its KDF %.60q is not argon2id t=… m=… p=…", line)
	}
	return kdf, nil
}

// Open opens a kit with its passphrase: it checks the kit's form and its
// KDF's bounds before argon2 runs, then the payload, which must agree with
// the header and hold the key the header names.
func Open(text, passphrase string) (Header, Payload, error) {
	p, err := parse(text)
	if err != nil {
		return Header{}, Payload{}, err
	}
	pass := norm.NFC.String(passphrase)
	if len(pass) > MaxPassphrase {
		return Header{}, Payload{}, ErrPassphrase
	}
	aead, err := kitAEAD(pass, p.sealed[:saltSize], p.header.KDF)
	if err != nil {
		return Header{}, Payload{}, err
	}
	raw, err := aead.Open(nil, p.sealed[saltSize:saltSize+nonceSize], p.sealed[saltSize+nonceSize:], p.header.aad())
	if err != nil {
		return Header{}, Payload{}, ErrPassphrase
	}
	var payload Payload
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil || decoder.More() {
		return Header{}, Payload{}, notAKit("its sealed content is not a kit's payload: %v", err)
	}
	if err := payload.agrees(p.header); err != nil {
		return Header{}, Payload{}, err
	}
	return p.header, payload, nil
}

// FileName is the name of a kit's file.
func FileName(h Header) string {
	return "pocketcloud-recovery-kit-" + h.Created.UTC().Format("20060102") + "-" + h.Kit + FileExtension
}
