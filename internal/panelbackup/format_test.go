package panelbackup

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/secrets"
)

var update = flag.Bool("update", false, "write the frozen vector anew")

// someFiles are stored files of every size that matters: empty, small, two
// whole chunks, a chunk and a byte.
func someFiles() []stored {
	return []stored{
		{"storage/_pb_users_auth_/abcdefghijklmno/avatar_x1y2z3.png", noise("avatar", 3000)},
		{"storage/pbc_3142635823/abcdefghijklmno/empty_0a1b2c3d4e.txt", nil},
		{"storage/pbc_3142635823/abcdefghijklmno/vaultwarden_dump_k3j9x2m4q8.sql.gz", noise("dump", 2*ChunkSize)},
		{"storage/pbc_3142635823/zzzzzzzzzzzzzzz/zpráva 1_q8w1z7v2k3.txt", noise("note", ChunkSize+1)},
	}
}

// someDatabase is a database that compresses into more than one chunk.
func someDatabase() []byte {
	return append([]byte("SQLite format 3\x00"), append(bytes.Repeat([]byte("panel "), 40000), noise("db", 150_000)...)...)
}

// TestARoundTripGivesBackEveryFile: an export opens with its key into the
// database and the stored files as they were, and says what it held.
func TestARoundTripGivesBackEveryFile(t *testing.T) {
	db, files := someDatabase(), someFiles()
	export := build(t, vectorKey, seeded("round trip"), db, files)
	if !bytes.HasPrefix(export, []byte("PCPB\x01\x00\x00\x00")) {
		t.Fatalf("the export starts %q", export[:8])
	}
	for _, plain := range [][]byte{[]byte("SQLite format 3"), files[0].data[:64], files[2].data[ChunkSize : ChunkSize+64]} {
		if bytes.Contains(export, plain) {
			t.Fatalf("the export holds %q in plaintext", plain[:15])
		}
	}

	header, err := ReadHeader(bytes.NewReader(export))
	if err != nil || header.KeyID != secrets.KeyID(vectorKey) || !header.Created.Equal(madeAt) || len(header.ExportID) != 32 {
		t.Fatalf("the header: %+v, %v", header, err)
	}

	dir := filepath.Join(t.TempDir(), "pb_data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	summary, err := Extract(bytes.NewReader(export), vectorKey, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := countsOf(baseManifest(), db, files)
	if summary.Files != 4 || summary.Bytes != want.Bytes || summary.Size != int64(len(export)) || summary.Header != header {
		t.Errorf("the summary: %+v", summary)
	}
	m := summary.Manifest
	if m.Format != ManifestFormat || m.Version != 1 || m.ExportID != header.ExportID || m.KeyID != header.KeyID ||
		m.CreatedAt != "2026-11-17T04:00:00Z" || m.CAPin != want.CAPin || m.Audit != want.Audit || !slices.Equal(m.AgentsDial, want.AgentsDial) ||
		m.Files != 4 || m.Bytes != want.Bytes || !slices.Equal(m.LeftOut, LeftOut) || m.Missing == nil {
		t.Errorf("the manifest: %+v", m)
	}

	check := func(name string, data []byte) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("%s: %d bytes, %v", name, len(got), err)
		}
		if info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v, %v", name, info.Mode(), err)
		}
	}
	check(DatabaseName, db)
	for _, f := range files {
		check(f.name, f.data)
	}
	if info, err := os.Stat(filepath.Join(dir, "storage", "pbc_3142635823")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("a storage directory: %v, %v", info.Mode(), err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("pb_data holds %v: only data.db and storage belong", entries)
	}

	// the Reader gives the entries in their order
	rd, err := NewReader(bytes.NewReader(export), vectorKey)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for {
		entry, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		names = append(names, entry.Name)
		if entry.Kind == KindFile && entry.Size != int64(len(files[len(names)-2].data)) {
			t.Errorf("%s has the size %d", entry.Name, entry.Size)
		}
	}
	if !slices.Equal(names, []string{DatabaseName, files[0].name, files[1].name, files[2].name, files[3].name}) {
		t.Errorf("the entries: %v", names)
	}
	if _, err := rd.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("after the end: %v", err)
	}

	// the crafted export, made as the format says, opens the same
	crafted := newCrafter(t, vectorKey).whole(db, files)
	if got, _, err := readAllOf(crafted, vectorKey); err != nil || !bytes.Equal(got[DatabaseName], db) || !bytes.Equal(got[files[3].name], files[3].data) {
		t.Errorf("a crafted export: %v", err)
	}
}

// TestAnotherKeyIsRefusedByItsID: an export of another master key is
// refused by the key id of its header, before anything is decrypted.
func TestAnotherKeyIsRefusedByItsID(t *testing.T) {
	export := build(t, vectorKey, seeded("key"), someDatabase(), nil)
	_, err := Verify(bytes.NewReader(export), otherKey)
	var keyErr *KeyError
	if !errors.Is(err, ErrOtherKey) || !errors.As(err, &keyErr) || keyErr.Export != secrets.KeyID(vectorKey) || keyErr.Key != secrets.KeyID(otherKey) {
		t.Fatalf("got %v", err)
	}
	if _, err := Extract(bytes.NewReader(export), otherKey, t.TempDir()); !errors.Is(err, ErrOtherKey) {
		t.Errorf("extract: %v", err)
	}
	if _, err := Verify(bytes.NewReader(export), vectorKey[:31]); err == nil {
		t.Error("a key of 31 bytes")
	}
}

// TestTheHeaderIsChecked: what is no export, or one of another version, is
// refused by its header.
func TestTheHeaderIsChecked(t *testing.T) {
	export := build(t, vectorKey, seeded("header"), someDatabase(), nil)
	for name, c := range map[string]struct {
		at   int
		b    byte
		want error
	}{
		"magic":    {0, 'X', ErrNotAnExport},
		"version":  {4, 2, ErrVersion},
		"reserved": {6, 1, ErrVersion},
	} {
		changed := bytes.Clone(export)
		changed[c.at] = c.b
		if _, err := ReadHeader(bytes.NewReader(changed)); !errors.Is(err, c.want) {
			t.Errorf("%s: ReadHeader gives %v", name, err)
		}
		if _, err := Verify(bytes.NewReader(changed), vectorKey); !errors.Is(err, c.want) {
			t.Errorf("%s: Verify gives %v", name, err)
		}
	}
	zero := bytes.Clone(export)
	binary.BigEndian.PutUint64(zero[40:48], 0)
	if _, err := ReadHeader(bytes.NewReader(zero)); !errors.Is(err, ErrNotAnExport) {
		t.Errorf("an export made at 0: %v", err)
	}
	for _, short := range []int{0, 4, 47} {
		if _, err := ReadHeader(bytes.NewReader(export[:short])); !errors.Is(err, ErrNotAnExport) {
			t.Errorf("%d bytes: %v", short, err)
		}
	}
	if _, err := ReadHeader(bytes.NewReader([]byte("SQLite format 3\x00" + strings.Repeat("x", 64)))); !errors.Is(err, ErrNotAnExport) {
		t.Errorf("a database: %v", err)
	}
}

// TestEveryByteIsVouchedFor: flipping any bit of any byte of an export,
// header included, makes it fail.
func TestEveryByteIsVouchedFor(t *testing.T) {
	small := build(t, vectorKey, seeded("small"), []byte("SQLite format 3\x00tiny"), []stored{
		{"storage/pbc_1/abcdefghijklmno/a.txt", []byte("alpha")},
		{"storage/pbc_1/abcdefghijklmno/b.txt", nil},
	})
	for i := range small {
		for _, bit := range []byte{0x01, 0x80} {
			changed := bytes.Clone(small)
			changed[i] ^= bit
			if _, err := Verify(bytes.NewReader(changed), vectorKey); err == nil {
				t.Fatalf("flipping %#x of byte %d of %d went unnoticed", bit, i, len(small))
			}
		}
	}

	// a big one: some bytes, and each segment's and chunk's first and last
	big := build(t, vectorKey, seeded("big"), someDatabase(), someFiles())
	var at []int
	for i := 0; i < len(big); i += 4999 {
		at = append(at, i)
	}
	for _, s := range spansOf(t, big) {
		at = append(at, s.start, s.end-1)
		for _, c := range s.chunks[:len(s.chunks)-1] {
			at = append(at, c, c+1, c-1)
		}
	}
	for _, i := range at {
		changed := bytes.Clone(big)
		changed[i] ^= 0x10
		if _, err := Verify(bytes.NewReader(changed), vectorKey); err == nil {
			t.Fatalf("flipping byte %d of %d went unnoticed", i, len(big))
		}
	}
}

// TestNoChangedChunkGivesOutAByte: a reader gives out only bytes of the
// export as it was made, whatever was done to a chunk: changed, moved,
// taken from elsewhere, or sealed as the last when it is not.
func TestNoChangedChunkGivesOutAByte(t *testing.T) {
	db, files := someDatabase(), someFiles()
	export := build(t, vectorKey, seeded("chunks"), db, files)
	spans := spansOf(t, export)
	file := spans[4] // the file of two whole chunks
	if file.name != files[2].name || len(file.chunks) != 3 {
		t.Fatalf("the spans: %+v", file)
	}

	cases := map[string][]byte{}
	changed := bytes.Clone(export)
	changed[file.chunks[1]+100] ^= 1
	cases["changed"] = changed

	swapped := bytes.Clone(export)
	first := slices.Clone(export[file.chunks[0]:file.chunks[1]])
	second := slices.Clone(export[file.chunks[1]:file.chunks[2]])
	copy(swapped[file.chunks[0]:], second)
	copy(swapped[file.chunks[0]+len(second):], first)
	cases["swapped"] = swapped

	// the same file of another export: its chunks open there, not here
	other := build(t, vectorKey, seeded("other"), db, []stored{files[0], files[1], {files[2].name, noise("other", 2*ChunkSize)}, files[3]})
	otherFile := spansOf(t, other)[4]
	taken := bytes.Clone(export)
	copy(taken[file.chunks[1]:file.chunks[2]], other[otherFile.chunks[1]:otherFile.chunks[2]])
	cases["taken"] = taken

	for name, export := range cases {
		got, _, err := readAllOf(export, vectorKey)
		mustDamage(t, err, 5, "does not open")
		if !bytes.HasPrefix(files[2].data, got[files[2].name]) || len(got[files[2].name]) > ChunkSize {
			t.Errorf("%s: %d bytes given out that are not the file's first chunk", name, len(got[files[2].name]))
		}
	}

	// the first chunk of the file sealed as its last, or its last as not
	for name, last := range map[string]func(i, n int64) bool{
		"first as last": func(i, n int64) bool { return i == 0 || i == n-1 },
		"last as not":   func(int64, int64) bool { return false },
	} {
		c := newCrafter(t, vectorKey)
		c.manifest(countsOf(baseManifest(), db, files[2:3]), nil)
		c.database(db)
		c.segmentWith(segmentHeader{kind: KindFile, name: files[2].name, salt: c.keys.fileSalt(files[2].name, sha256Sum(files[2].data))},
			files[2].data, c.raw, last)
		c.trailer()
		got, _, err := readAllOf(c.out.Bytes(), vectorKey)
		mustDamage(t, err, 3, "does not open")
		if len(got[files[2].name]) > 0 && name == "first as last" {
			t.Errorf("%s: %d bytes given out", name, len(got[files[2].name]))
		}
		if len(got[files[2].name]) > ChunkSize {
			t.Errorf("%s: the last chunk was given out", name)
		}
	}
}

// TestACutExportIsIncomplete: an export cut anywhere, at a segment's end or
// inside a chunk, or one that goes on after its trailer, is incomplete.
func TestACutExportIsIncomplete(t *testing.T) {
	small := build(t, vectorKey, seeded("cut"), []byte("SQLite format 3\x00"), []stored{{"storage/pbc_1/abcdefghijklmno/a.txt", []byte("alpha")}})
	for n := range len(small) {
		_, err := Verify(bytes.NewReader(small[:n]), vectorKey)
		if n < HeaderSize {
			if !errors.Is(err, ErrNotAnExport) {
				t.Fatalf("cut at %d: %v", n, err)
			}
			continue
		}
		if !errors.Is(err, ErrIncomplete) || !errors.Is(err, ErrDamaged) {
			t.Fatalf("cut at %d of %d: %v", n, len(small), err)
		}
	}
	spans := spansOf(t, small)
	_, err := Verify(bytes.NewReader(small[:spans[len(spans)-1].start]), vectorKey)
	mustDamage(t, err, len(spans), "ends before its trailer")

	for name, more := range map[string][]byte{"a byte": {0}, "the trailer again": small[spans[len(spans)-1].start:], "an export": small} {
		_, err := Verify(bytes.NewReader(append(bytes.Clone(small), more...)), vectorKey)
		if !errors.Is(err, ErrIncomplete) {
			t.Errorf("%s after the trailer: %v", name, err)
		}
		mustDamage(t, err, 0, "bytes follow the trailer")
	}

	big := build(t, vectorKey, seeded("cut big"), someDatabase(), someFiles())
	for _, s := range spansOf(t, big) {
		for _, n := range append([]int{s.start, s.start + 3, s.end - 1}, s.chunks[1:len(s.chunks)-1]...) {
			if _, err := Verify(bytes.NewReader(big[:n]), vectorKey); !errors.Is(err, ErrIncomplete) {
				t.Fatalf("cut at %d in %s: %v", n, s.name, err)
			}
		}
	}
}

// TestSegmentsKeepTheirPlace: segments moved, dropped, doubled, or taken
// from another export of the same key, fail.
func TestSegmentsKeepTheirPlace(t *testing.T) {
	db, files := []byte("SQLite format 3\x00db"), []stored{
		{"storage/pbc_1/abcdefghijklmno/a.txt", []byte("alpha")},
		{"storage/pbc_1/abcdefghijklmno/b.txt", []byte("bravo")},
	}
	export := build(t, vectorKey, seeded("place"), db, files)
	spans := spansOf(t, export)
	seg := func(i int) []byte { return export[spans[i].start:spans[i].end] }
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	head := export[:HeaderSize]

	for name, c := range map[string]struct {
		export  []byte
		segment int
		want    string
	}{
		"files swapped":       {join(head, seg(0), seg(1), seg(3), seg(2), seg(4)), 4, "not in path order"},
		"a file twice":        {join(head, seg(0), seg(1), seg(2), seg(2), seg(3), seg(4)), 4, "not in path order"},
		"a file dropped":      {join(head, seg(0), seg(1), seg(3), seg(4)), 4, "the trailer lists 4 segments, the backup holds 3"},
		"no database":         {join(head, seg(0), seg(2), seg(3), seg(4)), 2, "where the database belongs"},
		"database first":      {join(head, seg(1), seg(0), seg(2), seg(3), seg(4)), 1, "where the manifest belongs"},
		"manifest again":      {join(head, seg(0), seg(1), seg(0), seg(4)), 3, "where a stored file or the trailer belongs"},
		"database again":      {join(head, seg(0), seg(1), seg(2), seg(1), seg(4)), 4, "where a stored file or the trailer belongs"},
		"no manifest":         {join(head, seg(1), seg(2), seg(3), seg(4)), 1, "where the manifest belongs"},
		"trailer for nothing": {join(head, seg(0), seg(4)), 2, "where the database belongs"},
	} {
		_, err := Verify(bytes.NewReader(c.export), vectorKey)
		t.Run(name, func(t *testing.T) { mustDamage(t, err, c.segment, c.want) })
	}

	// another export of the same key: a file of the same name and size,
	// whose own key and chunks hold, is not the one the trailer vouches for
	other := build(t, vectorKey, seeded("other place"), db, []stored{files[0], {files[1].name, []byte("BRAVO")}})
	otherSpans := spansOf(t, other)
	otherSeg := func(i int) []byte { return other[otherSpans[i].start:otherSpans[i].end] }
	_, err := Verify(bytes.NewReader(join(head, seg(0), seg(1), seg(2), otherSeg(3), seg(4))), vectorKey)
	mustDamage(t, err, 5, "the trailer lists segment 4")
	// its trailer is sealed to its own header
	_, err = Verify(bytes.NewReader(join(head, seg(0), seg(1), seg(2), seg(3), otherSeg(4))), vectorKey)
	mustDamage(t, err, 5, "does not open")
	// its manifest names its own export
	_, err = Verify(bytes.NewReader(join(head, otherSeg(0), seg(1), seg(2), seg(3), seg(4))), vectorKey)
	mustDamage(t, err, 1, "names the export")
}

// TestTheTrailerBindsTheHeader: a trailer sealed to another header does not
// open, though every segment before it is this export's.
func TestTheTrailerBindsTheHeader(t *testing.T) {
	db := []byte("SQLite format 3\x00db")
	c := newCrafter(t, vectorKey)
	c.manifest(countsOf(baseManifest(), db, nil), nil)
	c.database(db)
	raw, _ := json.Marshal(trailer{Segments: c.entries})
	elsewhere := bytes.Clone(c.raw)
	elsewhere[30] ^= 1 // another export id
	c.segmentWith(segmentHeader{kind: KindTrailer, name: TrailerName}, raw, elsewhere, lastChunk)
	_, err := Verify(bytes.NewReader(c.out.Bytes()), vectorKey)
	mustDamage(t, err, 3, "does not open")
}

// TestTheTrailerListsEverySegment: a trailer that is no list, or lists other
// segments, fails.
func TestTheTrailerListsEverySegment(t *testing.T) {
	db := []byte("SQLite format 3\x00db")
	for name, c := range map[string]struct {
		trailer func([]TrailerEntry) []byte
		want    string
	}{
		"no JSON":       {func([]TrailerEntry) []byte { return []byte("segments") }, "no list of segments"},
		"another field": {func(e []TrailerEntry) []byte { return []byte(`{"segments":[],"more":1}`) }, "no list of segments"},
		"two lists": {func(e []TrailerEntry) []byte {
			raw, _ := json.Marshal(trailer{Segments: e})
			return append(raw, raw...)
		}, "no list of segments"},
		"another size": {func(e []TrailerEntry) []byte {
			e = slices.Clone(e)
			e[1].StoredLen++
			raw, _ := json.Marshal(trailer{Segments: e})
			return raw
		}, "the trailer lists segment 2"},
		"one fewer": {func(e []TrailerEntry) []byte {
			raw, _ := json.Marshal(trailer{Segments: e[:1]})
			return raw
		}, "the trailer lists 1 segments, the backup holds 2"},
		"one more": {func(e []TrailerEntry) []byte {
			raw, _ := json.Marshal(trailer{Segments: append(slices.Clone(e), e[1])})
			return raw
		}, "the trailer lists 3 segments"},
	} {
		cr := newCrafter(t, vectorKey)
		cr.manifest(countsOf(baseManifest(), db, nil), nil)
		cr.database(db)
		cr.segment(segmentHeader{kind: KindTrailer, name: TrailerName}, c.trailer(cr.entries))
		_, err := Verify(bytes.NewReader(cr.out.Bytes()), vectorKey)
		t.Run(name, func(t *testing.T) { mustDamage(t, err, 3, c.want) })
	}
}

// TestTheManifestAgreesWithTheHeaderAndTheFiles: a manifest of another
// format, version, export, key or time, or that counts other files or bytes,
// fails.
func TestTheManifestAgreesWithTheHeaderAndTheFiles(t *testing.T) {
	db, files := []byte("SQLite format 3\x00db"), []stored{{"storage/pbc_1/abcdefghijklmno/a.txt", []byte("alpha")}}
	for name, c := range map[string]struct {
		change  func(*Manifest)
		segment int
		want    string
	}{
		"format":    {func(m *Manifest) { m.Format = "pocketbase-backup" }, 1, "of the format"},
		"version":   {func(m *Manifest) { m.Version = 2 }, 1, "version 2"},
		"export id": {func(m *Manifest) { m.ExportID = strings.Repeat("0", 32) }, 1, "names the export"},
		"key id":    {func(m *Manifest) { m.KeyID = secrets.KeyID(otherKey) }, 1, "names the key"},
		"created":   {func(m *Manifest) { m.CreatedAt = "2026-11-17T04:00:01Z" }, 1, "was made 2026-11-17T04:00:01Z"},
		"files":     {func(m *Manifest) { m.Files = 2 }, 4, "counts 2 files"},
		"bytes":     {func(m *Manifest) { m.Bytes-- }, 4, "counts 1 files and 22 bytes"},
	} {
		cr := newCrafter(t, vectorKey)
		cr.manifest(countsOf(baseManifest(), db, files), c.change)
		cr.database(db)
		cr.file(files[0].name, files[0].data)
		cr.trailer()
		_, err := Verify(bytes.NewReader(cr.out.Bytes()), vectorKey)
		t.Run(name, func(t *testing.T) { mustDamage(t, err, c.segment, c.want) })
	}
	cr := newCrafter(t, vectorKey)
	cr.segment(segmentHeader{kind: KindManifest, name: ManifestName}, []byte("{not json"))
	_, err := NewReader(bytes.NewReader(cr.out.Bytes()), vectorKey)
	mustDamage(t, err, 1, "no JSON")
}

// TestAFileIsTheOneItsKeyWasMadeFor: a stored file sealed with a key not
// made from its name and content fails, though it opens.
func TestAFileIsTheOneItsKeyWasMadeFor(t *testing.T) {
	db := []byte("SQLite format 3\x00db")
	name := "storage/pbc_1/abcdefghijklmno/a.txt"
	for what, salt := range map[string]func(k keys) []byte{
		"a random salt":          func(keys) []byte { return nil },
		"another file's salt":    func(k keys) []byte { return k.fileSalt(name, sha256Sum([]byte("bravo"))) },
		"another name's salt":    func(k keys) []byte { return k.fileSalt(name+"x", sha256Sum([]byte("alpha"))) },
		"a salt of another key":  func(keys) []byte { k, _ := deriveKeys(otherKey); return k.fileSalt(name, sha256Sum([]byte("alpha"))) },
		"the content's own hash": func(keys) []byte { return sha256Sum([]byte("alpha")) },
	} {
		cr := newCrafter(t, vectorKey)
		cr.manifest(countsOf(baseManifest(), db, []stored{{name, []byte("alpha")}}), nil)
		cr.database(db)
		cr.segment(segmentHeader{kind: KindFile, name: name, salt: salt(cr.keys)}, []byte("alpha"))
		cr.trailer()
		_, err := Verify(bytes.NewReader(cr.out.Bytes()), vectorKey)
		t.Run(what, func(t *testing.T) { mustDamage(t, err, 3, "is not the file its key was made for") })
	}
}

// TestTheDatabaseDecompressesExactly: a database that is no flate stream, or
// one followed by more bytes, fails; one stored as it is opens.
func TestTheDatabaseDecompressesExactly(t *testing.T) {
	db := []byte("SQLite format 3\x00db")
	for name, c := range map[string]struct {
		stored []byte
		flags  byte
		want   string
	}{
		"no flate":        {[]byte("SQLite format 3\x00 not flate"), FlagFlate, "does not decompress"},
		"bytes after":     {append(deflate(t, db), 0, 1, 2), FlagFlate, "bytes follow the compressed database"},
		"a stream cut":    {deflate(t, db)[:5], FlagFlate, "does not decompress"},
		"stored as it is": {db, 0, ""},
	} {
		cr := newCrafter(t, vectorKey)
		cr.manifest(countsOf(baseManifest(), db, nil), nil)
		cr.segment(segmentHeader{kind: KindDatabase, flags: c.flags, name: DatabaseName}, c.stored)
		cr.trailer()
		got, _, err := readAllOf(cr.out.Bytes(), vectorKey)
		t.Run(name, func(t *testing.T) {
			if c.want == "" {
				if err != nil || !bytes.Equal(got[DatabaseName], db) {
					t.Errorf("%v, %q", err, got[DatabaseName])
				}
				return
			}
			mustDamage(t, err, 2, c.want)
		})
	}

	// a database larger than the limit
	defer func(was int64) { maxDatabase = was }(maxDatabase)
	maxDatabase = 1000
	_, err := Verify(bytes.NewReader(build(t, vectorKey, seeded("large"), make([]byte, 5000), nil)), vectorKey)
	mustDamage(t, err, 2, "larger than 1000 bytes")
}

// TestFlagsAreChecked: only the database may be compressed, and no other
// flag exists.
func TestFlagsAreChecked(t *testing.T) {
	db := []byte("SQLite format 3\x00db")
	name := "storage/pbc_1/abcdefghijklmno/a.txt"
	for what, c := range map[string]struct {
		build   func(*crafter)
		segment int
	}{
		"a compressed file": {func(cr *crafter) {
			cr.database(db)
			cr.segment(segmentHeader{kind: KindFile, flags: FlagFlate, name: name, salt: cr.keys.fileSalt(name, sha256Sum([]byte("alpha")))}, []byte("alpha"))
		}, 3},
		"another flag": {func(cr *crafter) {
			cr.segment(segmentHeader{kind: KindDatabase, flags: FlagFlate | 2, name: DatabaseName}, deflate(t, db))
		}, 2},
	} {
		cr := newCrafter(t, vectorKey)
		cr.manifest(countsOf(baseManifest(), db, []stored{{name, []byte("alpha")}}), nil)
		c.build(cr)
		cr.trailer()
		_, err := Verify(bytes.NewReader(cr.out.Bytes()), vectorKey)
		t.Run(what, func(t *testing.T) { mustDamage(t, err, c.segment, "with the flags") })
	}
	cr := newCrafter(t, vectorKey)
	cr.segment(segmentHeader{kind: KindManifest, flags: FlagFlate, name: ManifestName}, []byte("{}"))
	_, err := NewReader(bytes.NewReader(cr.out.Bytes()), vectorKey)
	mustDamage(t, err, 1, "with the flags")
}

// fields is a segment's fields with any lengths.
func fields(kind, flags byte, nameLen int, name string, stored uint64) []byte {
	out := []byte{kind, flags}
	out = binary.BigEndian.AppendUint16(out, uint16(nameLen))
	out = append(out, name...)
	out = append(out, make([]byte, SaltSize)...)
	return binary.BigEndian.AppendUint64(out, stored)
}

// endless is zeros without end, counting what was read.
type endless struct{ n int64 }

func (e *endless) Read(p []byte) (int, error) {
	clear(p)
	e.n += int64(len(p))
	return len(p), nil
}

// TestOversizedLengthsAreRefusedBeforeAllocating: a name or a size beyond
// the limits is refused by the fields that announce it, before anything of
// that size is read or allocated.
func TestOversizedLengthsAreRefusedBeforeAllocating(t *testing.T) {
	db := []byte("SQLite format 3\x00db")
	export := build(t, vectorKey, seeded("limits"), db, nil)
	spans := spansOf(t, export)
	head, withManifest, withDatabase := export[:HeaderSize], export[:spans[0].end], export[:spans[1].end]

	for name, c := range map[string]struct {
		prefix  []byte
		fields  []byte
		segment int
		want    string
	}{
		"a manifest's name of 65535 bytes":  {head, fields(KindManifest, 0, 0xFFFF, "", 10), 1, "name of 65535 bytes"},
		"a name of no bytes":                {head, fields(KindManifest, 0, 0, "", 10), 1, "name of 0 bytes"},
		"a name of 1025 bytes":              {withDatabase, fields(KindFile, 0, MaxName+1, "", 10), 3, "name of 1025 bytes"},
		"a manifest of 1 MiB and a byte":    {head, fields(KindManifest, 0, 13, ManifestName, MaxManifest+1), 1, "more than 1048576"},
		"a database of 64 GiB and a byte":   {withManifest, fields(KindDatabase, FlagFlate, 7, DatabaseName, MaxStored+1), 2, "more than 68719476736"},
		"a database of 2^64-1 bytes":        {withManifest, fields(KindDatabase, FlagFlate, 7, DatabaseName, 1<<64-1), 2, "more than 68719476736"},
		"a file of 2^63 bytes":              {withDatabase, fields(KindFile, 0, 35, "storage/pbc_1/abcdefghijklmno/a.txt", 1<<63), 3, "more than 68719476736"},
		"a trailer of 256 MiB and a byte":   {withDatabase, fields(KindTrailer, 0, 7, TrailerName, MaxTrailer+1), 3, "more than 268435456"},
		"a file named ../../etc/passwd":     {withDatabase, fields(KindFile, 0, 16, "../../etc/passwd", 10), 3, "cannot be named"},
		"a segment of an unknown kind":      {withDatabase, fields(7, 0, 7, "trailer", 10), 3, "where a stored file or the trailer belongs"},
		"a trailer where the database goes": {withManifest, fields(KindTrailer, 0, 7, TrailerName, 10), 2, "where the database belongs"},
	} {
		t.Run(name, func(t *testing.T) {
			rest := &endless{}
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			_, err := Verify(io.MultiReader(bytes.NewReader(c.prefix), bytes.NewReader(c.fields), rest), vectorKey)
			runtime.ReadMemStats(&after)
			mustDamage(t, err, c.segment, c.want)
			if grew := after.TotalAlloc - before.TotalAlloc; grew > 2<<20 {
				t.Errorf("refusing it allocated %d bytes", grew)
			}
		})
	}

	// at most so many segments
	defer func(was int) { maxSegments = was }(maxSegments)
	maxSegments = 5
	files := []stored{{"storage/pbc_1/abcdefghijklmno/a", nil}, {"storage/pbc_1/abcdefghijklmno/b", nil}, {"storage/pbc_1/abcdefghijklmno/c", nil}}
	_, err := Verify(bytes.NewReader(build(t, vectorKey, seeded("many"), db, files)), vectorKey)
	mustDamage(t, err, 6, "5 segments at most")
	if _, err := Verify(bytes.NewReader(build(t, vectorKey, seeded("enough"), db, files[:2])), vectorKey); err != nil {
		t.Errorf("five segments: %v", err)
	}
}

// badNames are names no stored file of an export may have.
var badNames = []string{
	"storage/../x", "/etc/passwd", "storage\\pbc_1\\abcdefghijklmno\\a", "storage/pbc_1/abcdefghijklmno/a\x00b",
	"storage/pbc_1/abcdefghijklmno/..", "storage/pbc_1/abcdefghijklmno/.", "storage/pbc_1/abcdefghijklmno/a/b",
	"storage/pbc_1/abcdefghijklmno/a\\..\\..\\b", "storage/pbc_1/ABCDEFGHIJKLMNO/a", "storage/pbc 1/abcdefghijklmno/a",
	"storage/pbc_1/abcdefghijklmn/a", "storage/pbc_1/abcdefghijklmno/", "storage/pbc_1/abcdefghijklmno/" + strings.Repeat("x", 256),
	"storage/" + strings.Repeat("c", 101) + "/abcdefghijklmno/a", "storage/pbc_1/abcdefghijklmno/a\nb", "storage/pbc_1/abcdefghijklmno/a\x7fb",
	"storage/pbc_1/abcdefghijklmno/\xff", "storage/pbc_1/abcdefghijklmno/thumbs_a.png/100x100_a.png", "data.db", "manifest.json", "trailer",
	" storage/pbc_1/abcdefghijklmno/a.txt", "pb_data/storage/pbc_1/abcdefghijklmno/a", "storage",
}

// TestTraversalNamesAreRefused: a name that would leave pb_data's storage,
// or is no file of it, is refused by the writer and by the reader, before
// anything is written.
func TestTraversalNamesAreRefused(t *testing.T) {
	for _, good := range []string{
		"storage/pbc_3142635823/abcdefghijklmno/backup_k3j9x2m4q8.sql.gz", "storage/_pb_users_auth_/abcdefghijklmno/avatar_x1.png",
		"storage/pbc_1/abcdefghijklmno/zpráva 1.txt", "storage/pbc_1/abcdefghijklmno/...", "storage/pbc_1/abcdefghijklmno/" + strings.Repeat("x", 255),
	} {
		if !ValidFileName(good) {
			t.Errorf("%q is refused", good)
		}
	}
	db := []byte("SQLite format 3\x00db")
	for _, name := range badNames {
		var out bytes.Buffer
		w, _ := NewWriter(&out, vectorKey, madeAt)
		_ = w.Manifest(countsOf(baseManifest(), db, []stored{{name, []byte("x")}}))
		_ = w.Database(bytes.NewReader(db), int64(len(db)))
		if err := w.File(name, bytes.NewReader([]byte("x")), 1); err == nil {
			t.Errorf("the writer took %q", name)
		}

		cr := newCrafter(t, vectorKey)
		cr.manifest(countsOf(baseManifest(), db, []stored{{name, []byte("x")}}), nil)
		cr.database(db)
		cr.file(name, []byte("x"))
		cr.trailer()
		dir := t.TempDir()
		_, err := Extract(bytes.NewReader(cr.out.Bytes()), vectorKey, filepath.Join(dir))
		mustDamage(t, err, 3, "cannot be named")
		var left []string
		_ = filepath.WalkDir(filepath.Dir(dir), func(p string, d os.DirEntry, _ error) error {
			if !d.IsDir() && filepath.Base(p) != DatabaseName {
				left = append(left, p)
			}
			return nil
		})
		if len(left) > 0 {
			t.Errorf("%q left %v", name, left)
		}
	}
}

// TestFileSegmentsConverge: a stored file encrypts to the same bytes in two
// exports of one key, so that restic deduplicates it, and to others under
// another key, or with another content or name; the other segments differ
// every time.
func TestFileSegmentsConverge(t *testing.T) {
	db, files := someDatabase(), someFiles()
	segments := func(export []byte) map[string][]byte {
		out := map[string][]byte{}
		for _, s := range spansOf(t, export) {
			out[s.name] = export[s.start:s.end]
		}
		return out
	}
	a := segments(build(t, vectorKey, seeded("a"), db, files))
	b := segments(build(t, vectorKey, seeded("b"), db, files))
	for _, f := range files {
		if !bytes.Equal(a[f.name], b[f.name]) {
			t.Errorf("%s differs between two exports of one key", f.name)
		}
	}
	for _, name := range []string{ManifestName, DatabaseName, TrailerName} {
		if bytes.Equal(a[name][SegmentFixedSize:], b[name][SegmentFixedSize:]) || bytes.Equal(a[name][4+len(name):4+len(name)+SaltSize], b[name][4+len(name):4+len(name)+SaltSize]) {
			t.Errorf("%s is the same in two exports", name)
		}
	}
	other := segments(build(t, otherKey, seeded("a"), db, files))
	for _, f := range files {
		if bytes.Equal(a[f.name], other[f.name]) {
			t.Errorf("%s is the same under another key", f.name)
		}
	}
	changed := slices.Clone(files)
	changed[0].data = append(bytes.Clone(changed[0].data[:len(changed[0].data)-1]), 0)
	if bytes.Equal(a[files[0].name], segments(build(t, vectorKey, seeded("a"), db, changed))[files[0].name]) {
		t.Error("another content encrypts the same")
	}
	renamed := []stored{{"storage/pbc_3142635823/abcdefghijklmno/a_renamed.png", files[0].data}}
	if bytes.Equal(a[files[0].name][SegmentFixedSize+len(files[0].name):], segments(build(t, vectorKey, seeded("a"), db, renamed))[renamed[0].name][SegmentFixedSize+len(renamed[0].name):]) {
		t.Error("another name encrypts the same")
	}
}

// TestTheWriterKeepsTheFormat: the writer refuses to put a segment out of
// its place, to count wrong, or to write a source that changed.
func TestTheWriterKeepsTheFormat(t *testing.T) {
	db := []byte("SQLite format 3\x00db")
	a := stored{"storage/pbc_1/abcdefghijklmno/a.txt", []byte("alpha")}
	b := stored{"storage/pbc_1/abcdefghijklmno/b.txt", []byte("bravo")}
	start := func(m Manifest) *Writer {
		w, err := NewWriter(io.Discard, vectorKey, madeAt)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Manifest(m); err != nil {
			t.Fatal(err)
		}
		return w
	}
	counted := countsOf(baseManifest(), db, []stored{a, b})

	w, _ := NewWriter(io.Discard, vectorKey, madeAt)
	if err := w.Database(bytes.NewReader(db), 2); err == nil {
		t.Error("a database before the manifest")
	}
	if err := w.Manifest(counted); err == nil {
		t.Error("the writer did not stay failed")
	}
	w = start(counted)
	if err := w.File(a.name, bytes.NewReader(a.data), 5); err == nil {
		t.Error("a file before the database")
	}
	if err := start(counted).Close(); err == nil {
		t.Error("a trailer before the database")
	}
	w = start(counted)
	_ = w.Database(bytes.NewReader(db), int64(len(db)))
	_ = w.File(b.name, bytes.NewReader(b.data), 5)
	if err := w.File(a.name, bytes.NewReader(a.data), 5); err == nil {
		t.Error("a file out of path order")
	}
	w = start(counted)
	_ = w.Database(bytes.NewReader(db), int64(len(db)))
	_ = w.File(a.name, bytes.NewReader(a.data), 5)
	if err := w.File(a.name, bytes.NewReader(a.data), 5); err == nil {
		t.Error("a file twice")
	}
	for name, m := range map[string]Manifest{"files": countsOf(baseManifest(), db, []stored{a}), "bytes": countsOf(baseManifest(), db, []stored{a, {b.name, []byte("bravo bravo")}})} {
		w := start(m)
		_ = w.Database(bytes.NewReader(db), int64(len(db)))
		_ = w.File(a.name, bytes.NewReader(a.data), 5)
		_ = w.File(b.name, bytes.NewReader(b.data), 5)
		if err := w.Close(); err == nil || !strings.Contains(err.Error(), "the manifest counts") {
			t.Errorf("a manifest of other %s: %v", name, err)
		}
	}
	w = start(counted)
	if err := w.Database(bytes.NewReader(db), int64(len(db))+1); err == nil {
		t.Error("a database shorter than its size")
	}
	w = start(counted)
	_ = w.Database(bytes.NewReader(db), int64(len(db)))
	if err := w.File(a.name, bytes.NewReader(a.data), 6); err == nil {
		t.Error("a file shorter than its size")
	}
	w = start(counted)
	if err := w.Database(&changing{data: db}, int64(len(db))); err == nil || !strings.Contains(err.Error(), "changed while it was written") {
		t.Errorf("a database that changed: %v", err)
	}
	w = start(counted)
	_ = w.Database(bytes.NewReader(db), int64(len(db)))
	if err := w.File(a.name, &changing{data: a.data}, 5); err == nil || !strings.Contains(err.Error(), "changed while it was written") {
		t.Errorf("a file that changed: %v", err)
	}
	if _, err := NewWriter(io.Discard, vectorKey, time.Unix(0, 0)); err == nil {
		t.Error("an export made at 0")
	}
}

// changing is a source whose bytes change after the first read through.
type changing struct {
	data  []byte
	reads int
}

func (c *changing) ReadAt(p []byte, off int64) (int, error) {
	if off == 0 {
		c.reads++
	}
	data := c.data
	if c.reads > 1 {
		data = bytes.Repeat([]byte{0xA5}, len(c.data))
	}
	if off >= int64(len(data)) {
		return 0, io.EOF
	}
	n := copy(p, data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// The frozen vector: the master key 00 01 … 1f, the export made 2026-11-17
// 04:00 UTC with random bytes from ChaCha8 seeded "pcpb1 frozen vector", of
// vectorDatabase and vectorFiles. A reader must open it as long as PCPB1
// exists; the writer must write it byte for byte.
const vectorPath = "testdata/pcpb1.vector"

var (
	vectorDatabase = []byte("SQLite format 3\x00the panel's database, as a test sees it")
	vectorFiles    = []stored{
		{"storage/pbc_3142635823/abcdefghijklmno/vaultwarden_k3j9x2m4q8.sql", []byte("-- a dump\n")},
		{"storage/pbc_3142635823/abcdefghijklmno/zero_q8w1z7v2k3.bin", nil},
	}
)

func TestAFrozenVectorPinsTheFormat(t *testing.T) {
	built := build(t, vectorKey, seeded("pcpb1 frozen vector"), vectorDatabase, vectorFiles)
	if *update {
		if err := os.WriteFile(vectorPath, built, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	frozen, err := os.ReadFile(vectorPath)
	if err != nil {
		t.Fatal(err)
	}

	got, summary, err := readAllOf(frozen, vectorKey)
	if err != nil {
		t.Fatalf("the frozen vector does not open: %v", err)
	}
	if !bytes.Equal(got[DatabaseName], vectorDatabase) || !bytes.Equal(got[vectorFiles[0].name], vectorFiles[0].data) || len(got) != 3 {
		t.Errorf("the frozen vector holds %q", got)
	}
	if summary.Header.ExportID != "b9eb0758c25d53b5d98b9fa6a7c3c286" || summary.Header.KeyID != "17ac82d879a43a9e72720c907de8857d" ||
		!summary.Header.Created.Equal(madeAt) || summary.Manifest.Audit.Seq != 3812 || summary.Files != 2 {
		t.Errorf("the frozen vector's summary: %+v", summary)
	}
	if sha256Hex(frozen) != vectorSHA256 {
		t.Errorf("the frozen vector's sha256 is %s", sha256Hex(frozen))
	}
	if !bytes.Equal(built, frozen) {
		t.Error("the writer no longer writes the frozen vector: the format changed, or compress/flate's output did")
	}
}

// vectorSHA256 is the SHA-256 of the frozen vector.
const vectorSHA256 = "1f052501c22005e1e4aeba030734906f4bcd1be6850914bb67eccf2ad5e27998"
