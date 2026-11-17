package panelbackup

import (
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/secrets"
)

// Limits a test lowers.
var (
	maxSegments = MaxSegments
	maxDatabase = int64(MaxStored)
)

// Reader reads an export as it streams: every chunk is opened before its
// bytes are given out, and the trailer, which vouches for the whole, is read
// last. What a reader gave out of an export that then fails is not to be
// trusted: Verify and Extract say so by their error.
type Reader struct {
	src      *bufio.Reader
	in       *tee
	keys     keys
	header   Header
	raw      []byte
	manifest Manifest
	seen     []TrailerEntry
	entry    *Entry
	last     string
	files    int
	bytes    int64
	done     bool
	err      error
}

// tee hashes and counts what is read through it.
type tee struct {
	r *bufio.Reader
	h hash.Hash
	n int64
}

func (t *tee) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	t.h.Write(p[:n])
	t.n += int64(n)
	return n, err
}

// NewReader reads an export's header and manifest. An export whose header
// names another key than master's is refused before anything is decrypted.
func NewReader(r io.Reader, master []byte) (*Reader, error) {
	keys, err := deriveKeys(master)
	if err != nil {
		return nil, err
	}
	src := bufio.NewReaderSize(r, ChunkSize+TagSize)
	header, err := ReadHeader(src)
	if err != nil {
		return nil, err
	}
	if want := secrets.KeyID(master); header.KeyID != want {
		return nil, &KeyError{Export: header.KeyID, Key: want}
	}
	raw, _ := header.marshal()
	rd := &Reader{src: src, in: &tee{r: src, h: sha256.New(), n: HeaderSize}, keys: keys, header: header, raw: raw}

	s, err := rd.segmentHeader()
	if err != nil {
		return nil, err
	}
	body, err := rd.readAll(s)
	if err != nil {
		return nil, err
	}
	// the manifest is listed once it is checked: until then it is segment 1
	if err := json.Unmarshal(body, &rd.manifest); err != nil {
		return nil, rd.damaged(false, "the manifest is no JSON: %v", err)
	}
	m := rd.manifest
	switch {
	case m.Format != ManifestFormat || m.Version != Version:
		return nil, rd.damaged(false, "the manifest is of the format %q, version %d", m.Format, m.Version)
	case m.ExportID != header.ExportID:
		return nil, rd.damaged(false, "the manifest names the export %s, the header %s", m.ExportID, header.ExportID)
	case m.KeyID != header.KeyID:
		return nil, rd.damaged(false, "the manifest names the key %s, the header %s", m.KeyID, header.KeyID)
	case m.CreatedAt != header.Created.Format(time.RFC3339):
		return nil, rd.damaged(false, "the manifest was made %s, the header says %s", m.CreatedAt, header.Created.Format(time.RFC3339))
	}
	rd.seen = append(rd.seen, entryOf(s, rd.in.h))
	return rd, nil
}

// Header is the export's header.
func (r *Reader) Header() Header { return r.header }

// Manifest is the export's manifest.
func (r *Reader) Manifest() Manifest { return r.manifest }

// damaged is an error at the segment being read.
func (r *Reader) damaged(incomplete bool, format string, args ...any) error {
	return r.fail(&DamagedError{Segment: len(r.seen) + 1, Reason: fmt.Sprintf(format, args...), Incomplete: incomplete})
}

// fail keeps the first error.
func (r *Reader) fail(err error) error {
	if r.err == nil {
		r.err = err
	}
	return r.err
}

// cut is a read that ended inside the export: it is incomplete.
func (r *Reader) cut(err error, what string) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return r.damaged(true, "the backup ends inside %s", what)
	}
	return r.fail(err)
}

// segmentHeader reads the fields of the next segment, and checks them before
// it reads or allocates anything they announce.
func (r *Reader) segmentHeader() (segmentHeader, error) {
	number := len(r.seen) + 1
	if number > maxSegments {
		return segmentHeader{}, r.damaged(false, "an export holds %d segments at most", maxSegments)
	}
	r.in.h = sha256.New()
	fixed := make([]byte, 4)
	if n, err := io.ReadFull(r.in, fixed); err != nil {
		if n == 0 && errors.Is(err, io.EOF) {
			return segmentHeader{}, r.damaged(true, "the backup ends before its trailer")
		}
		return segmentHeader{}, r.cut(err, "a segment's fields")
	}
	s := segmentHeader{kind: fixed[0], flags: fixed[1]}
	nameLen := int(binary.BigEndian.Uint16(fixed[2:4]))
	if nameLen == 0 || nameLen > MaxName {
		return s, r.damaged(false, "a segment's name of %d bytes", nameLen)
	}
	var want string
	switch {
	case number == 1 && s.kind != KindManifest:
		want = "the manifest"
	case number == 2 && s.kind != KindDatabase:
		want = "the database"
	case number > 2 && s.kind != KindFile && s.kind != KindTrailer:
		want = "a stored file or the trailer"
	}
	if want != "" {
		return s, r.damaged(false, "a segment of kind %d where %s belongs", s.kind, want)
	}
	name := make([]byte, nameLen)
	if _, err := io.ReadFull(r.in, name); err != nil {
		return s, r.cut(err, "a segment's name")
	}
	s.name = string(name)
	if !validName(s.kind, s.name) {
		return s, r.damaged(false, "a segment of kind %d cannot be named %.80q", s.kind, s.name)
	}
	if s.kind == KindFile && r.last != "" && s.name <= r.last {
		return s, r.damaged(false, "%.80q comes after %.80q: the files are not in path order, once each", s.name, r.last)
	}
	if s.flags&^FlagFlate != 0 || (s.flags&FlagFlate != 0 && s.kind != KindDatabase) {
		return s, r.damaged(false, "a segment of kind %d with the flags %#x", s.kind, s.flags)
	}
	rest := make([]byte, SaltSize+8)
	if _, err := io.ReadFull(r.in, rest); err != nil {
		return s, r.cut(err, "a segment's fields")
	}
	s.salt = rest[:SaltSize]
	stored := binary.BigEndian.Uint64(rest[SaltSize:])
	limit := uint64(MaxStored)
	switch s.kind {
	case KindManifest:
		limit = MaxManifest
	case KindTrailer:
		limit = MaxTrailer
	}
	if stored > limit {
		return s, r.damaged(false, "%s takes %d bytes, more than %d", s.name, stored, limit)
	}
	s.stored = int64(stored) //nolint:gosec // at most MaxStored, checked above
	return s, nil
}

// open is the reader of a segment's stored bytes.
func (r *Reader) open(s segmentHeader) (*segmentReader, error) {
	aead, err := segmentAEAD(r.keys, s.salt)
	if err != nil {
		return nil, r.fail(err)
	}
	return &segmentReader{r: r, aead: aead, aad: s.aad(r.raw), stored: s.stored, chunks: chunks(s.stored),
		sealed: make([]byte, 0, ChunkSize+TagSize)}, nil
}

// readAll reads the stored bytes of the manifest or the trailer, which are
// small.
func (r *Reader) readAll(s segmentHeader) ([]byte, error) {
	seg, err := r.open(s)
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	if _, err := io.Copy(&body, seg); err != nil {
		return nil, err
	}
	return body.Bytes(), nil
}

// segmentReader opens a segment's chunks one by one.
type segmentReader struct {
	r      *Reader
	aead   cipher.AEAD
	aad    []byte
	stored int64
	chunks int64
	next   int64
	plain  []byte
	off    int
	sealed []byte
}

func (s *segmentReader) Read(p []byte) (int, error) {
	for s.off == len(s.plain) {
		if s.next == s.chunks {
			return 0, io.EOF
		}
		if err := s.load(); err != nil {
			return 0, err
		}
	}
	n := copy(p, s.plain[s.off:])
	s.off += n
	return n, nil
}

// load opens the next chunk.
func (s *segmentReader) load() error {
	size := min(int64(ChunkSize), s.stored-s.next*ChunkSize)
	s.sealed = s.sealed[:size+TagSize]
	if _, err := io.ReadFull(s.r.in, s.sealed); err != nil {
		return s.r.cut(err, "a segment's chunks")
	}
	plain, err := s.aead.Open(s.plain[:0], nonce(s.next, s.next == s.chunks-1), s.sealed, s.aad)
	if err != nil {
		return s.r.damaged(false, "its chunk %d does not open: it was changed, cut or moved", s.next+1)
	}
	s.plain, s.off = plain, 0
	s.next++
	return nil
}

// Entry is the database or a stored file of an export, as Next gives it.
// Reading it gives its bytes, opened; its last read checks what the segment
// vouches for, and fails when it does not hold.
type Entry struct {
	// Kind is KindDatabase or KindFile.
	Kind byte
	// Name is data.db or the stored file's path.
	Name string
	// Size is a stored file's size; for the database, its compressed size.
	Size int64
	r    io.Reader
	end  func() error
	done bool
	err  error
}

func (e *Entry) Read(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	if e.done {
		return 0, io.EOF
	}
	n, err := e.r.Read(p)
	if errors.Is(err, io.EOF) {
		if endErr := e.end(); endErr != nil {
			e.err = endErr
			return n, endErr
		}
		e.done = true
		return n, io.EOF
	}
	if err != nil {
		e.err = err
	}
	return n, err
}

// Next gives the next entry of the export: the database, then each stored
// file. After the last one it reads the trailer and gives io.EOF once the
// trailer vouched for every segment and nothing followed it. An entry not
// read to its end is read, and checked, by the next call.
func (r *Reader) Next() (*Entry, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.done {
		return nil, io.EOF
	}
	if r.entry != nil {
		if _, err := io.Copy(io.Discard, r.entry); err != nil {
			return nil, r.fail(err)
		}
		r.entry = nil
	}
	s, err := r.segmentHeader()
	if err != nil {
		return nil, err
	}
	if s.kind == KindTrailer {
		return nil, r.trailer(s)
	}
	seg, err := r.open(s)
	if err != nil {
		return nil, err
	}
	entry := &Entry{Kind: s.kind, Name: s.name, Size: s.stored}
	switch s.kind {
	case KindDatabase:
		entry.r, entry.end = r.database(s, seg)
	default:
		entry.r, entry.end = r.file(s, seg)
	}
	r.entry = entry
	return entry, nil
}

// database is the reader of the database's bytes, decompressed when the
// segment says so, and the check of its end.
func (r *Reader) database(s segmentHeader, seg *segmentReader) (io.Reader, func() error) {
	done := func(size int64) error {
		r.seen = append(r.seen, entryOf(s, r.in.h))
		r.bytes += size
		return nil
	}
	if s.flags&FlagFlate == 0 {
		counted := &countingReader{r: seg}
		return counted, func() error { return done(counted.n) }
	}
	// a bufio.Reader is a flate.Reader: flate reads no further than its end
	buffered := bufio.NewReader(seg)
	inflated := &countingReader{r: flate.NewReader(buffered), limit: maxDatabase}
	reader := readerFunc(func(p []byte) (int, error) {
		n, err := inflated.Read(p)
		if err != nil && !errors.Is(err, io.EOF) {
			var damaged *DamagedError
			if !errors.As(err, &damaged) {
				err = r.damaged(false, "the database does not decompress: %v", err)
			}
		}
		return n, err
	})
	return reader, func() error {
		if _, err := buffered.ReadByte(); !errors.Is(err, io.EOF) {
			if err != nil {
				return err
			}
			return r.damaged(false, "bytes follow the compressed database")
		}
		return done(inflated.n)
	}
}

// file is the reader of a stored file's bytes, and the check that they are
// the file its key was made for.
func (r *Reader) file(s segmentHeader, seg *segmentReader) (io.Reader, func() error) {
	digest := sha256.New()
	return io.TeeReader(seg, digest), func() error {
		if !hmac.Equal(r.keys.fileSalt(s.name, digest.Sum(nil)), s.salt) {
			return r.damaged(false, "the content of %.80q is not the file its key was made for", s.name)
		}
		r.seen = append(r.seen, entryOf(s, r.in.h))
		r.last = s.name
		r.files++
		r.bytes += s.stored
		return nil
	}
}

// trailer reads the trailer and checks that it lists every segment read,
// that nothing follows it, and that the manifest counted what it holds.
func (r *Reader) trailer(s segmentHeader) error {
	// the trailer's own entry is not in the list
	seen := r.seen
	body, err := r.readAll(s)
	if err != nil {
		return err
	}
	var t trailer
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&t); err != nil || decoder.More() {
		return r.damaged(false, "the trailer is no list of segments: %v", err)
	}
	if len(t.Segments) != len(seen) {
		return r.damaged(false, "the trailer lists %d segments, the backup holds %d", len(t.Segments), len(seen))
	}
	for i, entry := range t.Segments {
		if entry != seen[i] {
			return r.damaged(false, "the trailer lists segment %d as %.80q, %d bytes, sha256 %.16s…, the backup holds %.80q, %d bytes, sha256 %.16s…",
				i+1, entry.Name, entry.StoredLen, entry.SHA256, seen[i].Name, seen[i].StoredLen, seen[i].SHA256)
		}
	}
	if _, err := r.src.ReadByte(); !errors.Is(err, io.EOF) {
		if err != nil {
			return r.fail(err)
		}
		return r.damaged(true, "bytes follow the trailer")
	}
	if r.manifest.Files != r.files || r.manifest.Bytes != r.bytes {
		return r.damaged(false, "the manifest counts %d files and %d bytes, the backup holds %d and %d",
			r.manifest.Files, r.manifest.Bytes, r.files, r.bytes)
	}
	r.done = true
	return io.EOF
}

// Summary is what an export held, once read to its end.
type Summary struct {
	Header   Header
	Manifest Manifest
	// Files counts its stored files.
	Files int
	// Bytes counts the bytes of its database and stored files.
	Bytes int64
	// Size is the size of the export.
	Size int64
}

// Summary is what the export held. It is complete once Next gave io.EOF.
func (r *Reader) Summary() Summary {
	return Summary{Header: r.header, Manifest: r.manifest, Files: r.files, Bytes: r.bytes, Size: r.in.n}
}

// Verify reads a whole export and checks every step: the key id, every
// chunk, the order, the manifest, each file's key and the trailer.
func Verify(r io.Reader, master []byte) (Summary, error) {
	rd, err := NewReader(r, master)
	if err != nil {
		return Summary{}, err
	}
	for {
		if _, err := rd.Next(); errors.Is(err, io.EOF) {
			return rd.Summary(), nil
		} else if err != nil {
			return Summary{}, err
		}
	}
}

// Extract reads a whole export, as Verify does, into dir, a directory of
// pb_data: dir/data.db and dir/storage/…, files of mode 0600 in directories
// of mode 0700, created and never overwritten. On an error, what it wrote
// stays in dir, which the caller removes: nothing of it is to be trusted.
func Extract(r io.Reader, master []byte, dir string) (Summary, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Summary{}, err
	}
	defer root.Close()
	rd, err := NewReader(r, master)
	if err != nil {
		return Summary{}, err
	}
	for {
		entry, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return rd.Summary(), nil
		} else if err != nil {
			return Summary{}, err
		}
		if err := extract(root, entry); err != nil {
			return Summary{}, rd.fail(err)
		}
	}
}

// extract writes one entry under root.
func extract(root *os.Root, entry *Entry) error {
	if dir := path.Dir(entry.Name); dir != "." {
		if err := root.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	f, err := root.OpenFile(entry.Name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, entry); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// countingReader counts what it reads, up to a limit when it has one.
type countingReader struct {
	r     io.Reader
	n     int64
	limit int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.limit > 0 && c.n > c.limit {
		return n, fmt.Errorf("the database is larger than %d bytes", c.limit)
	}
	return n, err
}

// readerFunc is a function as an io.Reader.
type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
