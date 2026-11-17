package panelbackup

import (
	"bytes"
	"compress/flate"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	mrand "math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/secrets"
)

// keyOf is a master key whose bytes start at b and count up.
func keyOf(b byte) []byte {
	key := make([]byte, secrets.KeySize)
	for i := range key {
		key[i] = b + byte(i)
	}
	return key
}

var (
	vectorKey = keyOf(0)
	otherKey  = keyOf(100)
	madeAt    = time.Date(2026, 11, 17, 4, 0, 0, 0, time.UTC)
)

// seeded is a random stream that is the same for the same seed.
func seeded(seed string) io.Reader {
	var key [32]byte
	copy(key[:], seed)
	return mrand.NewChaCha8(key)
}

// stored is a file of the panel's storage.
type stored struct {
	name string
	data []byte
}

// noise is n bytes that do not compress.
func noise(seed string, n int) []byte {
	out := make([]byte, n)
	_, _ = io.ReadFull(seeded(seed), out)
	return out
}

// baseManifest is what the panel says of itself, besides what the writer
// sets and the counts.
func baseManifest() Manifest {
	return Manifest{
		PanelVersion: "0.32.0", Migration: "1772000000_panel_recovery.go", CAPin: "sha256:" + strings.Repeat("5d", 32),
		Audit: AuditHead{Seq: 3812, Hash: strings.Repeat("ab", 32)}, AgentsDial: []Dial{{Address: "192.0.2.2:9443", Servers: 2}},
		Hosts: 2, Job: "abc123def456ghi", Run: "run123run456run", Storage: StorageLocal, LeftOut: LeftOut,
	}
}

// countsOf sets a manifest's counts for this database and these files.
func countsOf(m Manifest, db []byte, files []stored) Manifest {
	m.Files, m.Bytes = len(files), int64(len(db))
	for _, f := range files {
		m.Bytes += int64(len(f.data))
	}
	return m
}

// build writes an export with the Writer.
func build(t testing.TB, master []byte, random io.Reader, db []byte, files []stored) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := newWriter(&out, master, madeAt, random)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Manifest(countsOf(baseManifest(), db, files)); err != nil {
		t.Fatal(err)
	}
	if err := w.Database(bytes.NewReader(db), int64(len(db))); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := w.File(f.name, bytes.NewReader(f.data), int64(len(f.data))); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// span is where a segment is in an export.
type span struct {
	kind       byte
	name       string
	start, end int
	// chunks are where its chunks start, and the end
	chunks []int
}

// spansOf walks an export's segments, opening nothing.
func spansOf(t testing.TB, export []byte) []span {
	t.Helper()
	var spans []span
	at := HeaderSize
	for at < len(export) {
		s := span{kind: export[at], start: at}
		nameLen := int(binary.BigEndian.Uint16(export[at+2:]))
		s.name = string(export[at+4 : at+4+nameLen])
		storedAt := at + 4 + nameLen + SaltSize
		stored := int64(binary.BigEndian.Uint64(export[storedAt:]))
		at = storedAt + 8
		for i := range chunks(stored) {
			s.chunks = append(s.chunks, at)
			at += int(min(ChunkSize, stored-i*ChunkSize)) + TagSize
		}
		s.chunks = append(s.chunks, at)
		s.end = at
		spans = append(spans, s)
	}
	if at != len(export) {
		t.Fatalf("the export does not end at a segment's end: %d of %d", at, len(export))
	}
	return spans
}

// crafter puts an export together segment by segment, as the format says,
// so that a test can make one the Writer refuses to make.
type crafter struct {
	t       testing.TB
	keys    keys
	header  Header
	raw     []byte
	out     bytes.Buffer
	entries []TrailerEntry
}

func newCrafter(t testing.TB, master []byte) *crafter {
	t.Helper()
	k, err := deriveKeys(master)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	c := &crafter{t: t, keys: k, header: Header{KeyID: secrets.KeyID(master), ExportID: hex.EncodeToString(id), Created: madeAt}}
	c.raw, err = c.header.marshal()
	if err != nil {
		t.Fatal(err)
	}
	c.out.Write(c.raw)
	return c
}

// lastChunk is the STREAM flag a chunk is sealed with: the truth.
func lastChunk(i, n int64) bool { return i == n-1 }

// seal is a segment's bytes, its chunks sealed with the flags last says,
// under the additional data of header.
func (c *crafter) seal(s segmentHeader, plain []byte, header []byte, last func(i, n int64) bool) []byte {
	aead, err := segmentAEAD(c.keys, s.salt)
	if err != nil {
		c.t.Fatal(err)
	}
	out := s.marshal()
	n := chunks(int64(len(plain)))
	for i := range n {
		part := plain[min(i*ChunkSize, int64(len(plain))):min((i+1)*ChunkSize, int64(len(plain)))]
		out = aead.Seal(out, nonce(i, last(i, n)), part, s.aad(header))
	}
	return out
}

// segment appends a segment and lists it for the trailer.
func (c *crafter) segment(s segmentHeader, plain []byte) {
	c.segmentWith(s, plain, c.raw, lastChunk)
}

func (c *crafter) segmentWith(s segmentHeader, plain []byte, header []byte, last func(i, n int64) bool) {
	if s.salt == nil {
		s.salt = make([]byte, SaltSize)
		_, _ = rand.Read(s.salt)
	}
	if s.stored == 0 {
		s.stored = int64(len(plain))
	}
	sealed := c.seal(s, plain, header, last)
	c.out.Write(sealed)
	c.entries = append(c.entries, entryOfBytes(s, sealed))
}

func entryOfBytes(s segmentHeader, sealed []byte) TrailerEntry {
	digest := sha256Hex(sealed)
	return TrailerEntry{Kind: s.kind, Name: s.name, StoredLen: s.stored, SHA256: digest}
}

// manifest appends a manifest that agrees with the header, or as change
// makes it.
func (c *crafter) manifest(m Manifest, change func(*Manifest)) {
	m.Format, m.Version, m.ExportID, m.KeyID = ManifestFormat, Version, c.header.ExportID, c.header.KeyID
	m.CreatedAt = c.header.Created.Format(time.RFC3339)
	if change != nil {
		change(&m)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		c.t.Fatal(err)
	}
	c.segment(segmentHeader{kind: KindManifest, name: ManifestName}, raw)
}

// deflate is what the Writer stores of a database.
func deflate(t testing.TB, plain []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	fw, _ := flate.NewWriter(&out, flate.BestSpeed)
	_, _ = fw.Write(plain)
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// database appends a database as the Writer stores it.
func (c *crafter) database(plain []byte) {
	c.segment(segmentHeader{kind: KindDatabase, flags: FlagFlate, name: DatabaseName}, deflate(c.t, plain))
}

// file appends a stored file with its convergent salt.
func (c *crafter) file(name string, data []byte) {
	c.segment(segmentHeader{kind: KindFile, name: name, salt: c.keys.fileSalt(name, sha256Sum(data))}, data)
}

// trailer appends the trailer of what came before.
func (c *crafter) trailer() {
	raw, err := json.Marshal(trailer{Segments: c.entries})
	if err != nil {
		c.t.Fatal(err)
	}
	entries := c.entries
	c.segment(segmentHeader{kind: KindTrailer, name: TrailerName}, raw)
	c.entries = entries
}

// whole is a valid crafted export of this database and these files.
func (c *crafter) whole(db []byte, files []stored) []byte {
	c.manifest(countsOf(baseManifest(), db, files), nil)
	c.database(db)
	for _, f := range files {
		c.file(f.name, f.data)
	}
	c.trailer()
	return c.out.Bytes()
}

// mustDamage checks that err is a *DamagedError of this segment whose
// reason holds want.
func mustDamage(t testing.TB, err error, segment int, want string) {
	t.Helper()
	var damaged *DamagedError
	if !errors.As(err, &damaged) || !errors.Is(err, ErrDamaged) {
		t.Fatalf("got %v, not a damaged backup", err)
	}
	if segment > 0 && damaged.Segment != segment {
		t.Errorf("damaged at segment %d, not %d: %v", damaged.Segment, segment, err)
	}
	if !strings.Contains(damaged.Reason, want) {
		t.Errorf("the reason %q does not say %q", damaged.Reason, want)
	}
}

// readAllOf reads every entry, and gives what it gave out of each before an
// error, by name.
func readAllOf(export, master []byte) (map[string][]byte, Summary, error) {
	got := map[string][]byte{}
	rd, err := NewReader(bytes.NewReader(export), master)
	if err != nil {
		return got, Summary{}, err
	}
	for {
		entry, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return got, rd.Summary(), nil
		}
		if err != nil {
			return got, Summary{}, err
		}
		var buf bytes.Buffer
		_, err = io.Copy(&buf, entry)
		got[entry.Name] = buf.Bytes()
		if err != nil {
			return got, Summary{}, err
		}
	}
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

func sha256Hex(data []byte) string { return hex.EncodeToString(sha256Sum(data)) }
