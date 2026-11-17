package panelbackup

import (
	"bytes"
	"compress/flate"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/secrets"
)

// Writer writes an export: Manifest, then Database, then File for each
// stored file in path order, then Close. It reads the database and each file
// twice, through an io.ReaderAt: once to know its stored size, or the digest
// its key comes from, and once to encrypt it; a source that changes in
// between fails the export. After an error the Writer stays failed.
type Writer struct {
	out      io.Writer
	random   io.Reader
	keys     keys
	header   Header
	raw      []byte
	manifest Manifest
	step     int
	last     string
	entries  []TrailerEntry
	files    int
	bytes    int64
	err      error
}

// The steps of a Writer.
const (
	stepManifest = iota
	stepDatabase
	stepFiles
	stepClosed
)

// NewWriter starts an export sealed with master, made at created, and writes
// its header to w.
func NewWriter(w io.Writer, master []byte, created time.Time) (*Writer, error) {
	return newWriter(w, master, created, rand.Reader)
}

func newWriter(w io.Writer, master []byte, created time.Time, random io.Reader) (*Writer, error) {
	keys, err := deriveKeys(master)
	if err != nil {
		return nil, err
	}
	id := make([]byte, 16)
	if _, err := io.ReadFull(random, id); err != nil {
		return nil, err
	}
	header := Header{KeyID: secrets.KeyID(master), ExportID: hex.EncodeToString(id), Created: time.Unix(created.Unix(), 0).UTC()}
	raw, err := header.marshal()
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(raw); err != nil {
		return nil, err
	}
	return &Writer{out: w, random: random, keys: keys, header: header, raw: raw}, nil
}

// Header is the export's header.
func (w *Writer) Header() Header { return w.header }

// fail keeps the first error.
func (w *Writer) fail(err error) error {
	if w.err == nil {
		w.err = err
	}
	return w.err
}

// at checks that the export is at this step.
func (w *Writer) at(step int, what string) error {
	if w.err != nil {
		return w.err
	}
	if w.step != step {
		return w.fail(fmt.Errorf("the %s does not belong at this place of the export", what))
	}
	return nil
}

// Manifest writes the manifest, first. The writer sets its format, version,
// export id, key id and time from the header.
func (w *Writer) Manifest(m Manifest) error {
	if err := w.at(stepManifest, "manifest"); err != nil {
		return err
	}
	m.Format, m.Version = ManifestFormat, Version
	m.ExportID, m.KeyID = w.header.ExportID, w.header.KeyID
	m.CreatedAt = w.header.Created.Format(time.RFC3339)
	if m.AgentsDial == nil {
		m.AgentsDial = []Dial{}
	}
	if m.Missing == nil {
		m.Missing = []string{}
	}
	if m.LeftOut == nil {
		m.LeftOut = []string{}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return w.fail(err)
	}
	if len(raw) > MaxManifest {
		return w.fail(fmt.Errorf("the manifest takes %d bytes, more than %d", len(raw), MaxManifest))
	}
	if err := w.randomSegment(KindManifest, 0, ManifestName, int64(len(raw)), func(sw io.Writer) error {
		_, err := sw.Write(raw)
		return err
	}); err != nil {
		return err
	}
	w.manifest = m
	w.step = stepDatabase
	return nil
}

// Database writes the copy of data.db, of size bytes, flate compressed,
// second.
func (w *Writer) Database(src io.ReaderAt, size int64) error {
	if err := w.at(stepDatabase, "database"); err != nil {
		return err
	}
	if size < 0 || size > MaxStored {
		return w.fail(fmt.Errorf("a database of %d bytes", size))
	}
	counted, first := &counter{}, sha256.New()
	if err := compress(counted, first, src, size); err != nil {
		return w.fail(err)
	}
	if counted.n > MaxStored {
		return w.fail(fmt.Errorf("the compressed database takes %d bytes", counted.n))
	}
	if err := w.randomSegment(KindDatabase, FlagFlate, DatabaseName, counted.n, func(sw io.Writer) error {
		again := sha256.New()
		if err := compress(sw, again, src, size); err != nil {
			return err
		}
		if !bytes.Equal(again.Sum(nil), first.Sum(nil)) {
			return errors.New("the database changed while it was written")
		}
		return nil
	}); err != nil {
		return err
	}
	w.bytes += size
	w.step = stepFiles
	return nil
}

// compress writes the size bytes of src flate compressed to dst, and hashes
// them into digest.
func compress(dst io.Writer, digest hash.Hash, src io.ReaderAt, size int64) error {
	fw, err := flate.NewWriter(dst, flate.BestSpeed)
	if err != nil {
		return err
	}
	n, err := io.Copy(io.MultiWriter(fw, digest), io.NewSectionReader(src, 0, size))
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("the database has %d bytes, not %d", n, size)
	}
	return fw.Close()
}

// File writes a stored file, of size bytes, after the database, in path
// order: each name after the one before.
func (w *Writer) File(name string, src io.ReaderAt, size int64) error {
	if err := w.at(stepFiles, "file "+name); err != nil {
		return err
	}
	if !ValidFileName(name) {
		return w.fail(fmt.Errorf("an export cannot hold a file %q", name))
	}
	if w.last != "" && name <= w.last {
		return w.fail(fmt.Errorf("the file %q comes after %q: the files go in path order, once each", name, w.last))
	}
	if size < 0 || size > MaxStored {
		return w.fail(fmt.Errorf("a file of %d bytes", size))
	}
	digest, err := digestOf(src, size)
	if err != nil {
		return w.fail(fmt.Errorf("%s: %w", name, err))
	}
	if err := w.segment(segmentHeader{kind: KindFile, name: name, salt: w.keys.fileSalt(name, digest), stored: size}, func(sw io.Writer) error {
		again := sha256.New()
		n, err := io.Copy(io.MultiWriter(sw, again), io.NewSectionReader(src, 0, size))
		if err != nil {
			return err
		}
		if n != size || !bytes.Equal(again.Sum(nil), digest) {
			return fmt.Errorf("the file %s changed while it was written", name)
		}
		return nil
	}); err != nil {
		return err
	}
	w.last = name
	w.files++
	w.bytes += size
	return nil
}

// digestOf is the SHA-256 of the size bytes of src.
func digestOf(src io.ReaderAt, size int64) ([]byte, error) {
	h := sha256.New()
	n, err := io.Copy(h, io.NewSectionReader(src, 0, size))
	if err != nil {
		return nil, err
	}
	if n != size {
		return nil, fmt.Errorf("it has %d bytes, not %d", n, size)
	}
	return h.Sum(nil), nil
}

// fileSalt is the salt of a stored file's segment: keyed, and the same for
// the same name and content.
func (k keys) fileSalt(name string, digest []byte) []byte {
	mac := hmac.New(sha256.New, k.fileID)
	mac.Write([]byte(name))
	mac.Write([]byte{0})
	mac.Write(digest)
	return mac.Sum(nil)
}

// Close writes the trailer, once the manifest and the database are written,
// after checking that the manifest counted the files and their bytes. It
// does not close the writer the export goes to.
func (w *Writer) Close() error {
	if w.err != nil {
		return w.err
	}
	if w.step != stepFiles {
		return w.fail(errors.New("an export needs its manifest and its database before its trailer"))
	}
	if w.manifest.Files != w.files || w.manifest.Bytes != w.bytes {
		return w.fail(fmt.Errorf("the manifest counts %d files and %d bytes, the export holds %d and %d",
			w.manifest.Files, w.manifest.Bytes, w.files, w.bytes))
	}
	raw, err := json.Marshal(trailer{Segments: w.entries})
	if err != nil {
		return w.fail(err)
	}
	if len(raw) > MaxTrailer {
		return w.fail(fmt.Errorf("the trailer takes %d bytes", len(raw)))
	}
	if err := w.randomSegment(KindTrailer, 0, TrailerName, int64(len(raw)), func(sw io.Writer) error {
		_, err := sw.Write(raw)
		return err
	}); err != nil {
		return err
	}
	w.step = stepClosed
	return nil
}

// randomSegment writes a segment with a random salt.
func (w *Writer) randomSegment(kind, flags byte, name string, stored int64, fill func(io.Writer) error) error {
	salt := make([]byte, SaltSize)
	if _, err := io.ReadFull(w.random, salt); err != nil {
		return w.fail(err)
	}
	return w.segment(segmentHeader{kind: kind, flags: flags, name: name, salt: salt, stored: stored}, fill)
}

// segment writes a segment whose fill writes exactly its stored bytes.
func (w *Writer) segment(s segmentHeader, fill func(io.Writer) error) error {
	if len(w.entries)+1 >= MaxSegments && s.kind != KindTrailer {
		return w.fail(fmt.Errorf("an export holds %d segments at most", MaxSegments))
	}
	aead, err := segmentAEAD(w.keys, s.salt)
	if err != nil {
		return w.fail(err)
	}
	digest := sha256.New()
	out := io.MultiWriter(w.out, digest)
	if _, err := out.Write(s.marshal()); err != nil {
		return w.fail(err)
	}
	cw := &chunkWriter{out: out, aead: aead, aad: s.aad(w.raw), stored: s.stored, buf: make([]byte, 0, ChunkSize)}
	if err := fill(cw); err != nil {
		return w.fail(err)
	}
	if err := cw.finish(); err != nil {
		return w.fail(err)
	}
	w.entries = append(w.entries, entryOf(s, digest))
	return nil
}

// entryOf is a segment as the trailer lists it.
func entryOf(s segmentHeader, digest hash.Hash) TrailerEntry {
	return TrailerEntry{Kind: s.kind, Name: s.name, StoredLen: s.stored, SHA256: hex.EncodeToString(digest.Sum(nil))}
}

// segmentAEAD is the cipher of the segment with this salt.
func segmentAEAD(k keys, salt []byte) (cipher.AEAD, error) {
	key, err := k.segmentKey(salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// chunkWriter seals the stored bytes of a segment in chunks.
type chunkWriter struct {
	out      io.Writer
	aead     cipher.AEAD
	aad      []byte
	stored   int64
	consumed int64
	index    int64
	buf      []byte
	sealed   []byte
}

func (c *chunkWriter) Write(p []byte) (int, error) {
	if c.consumed+int64(len(p)) > c.stored {
		return 0, fmt.Errorf("a segment of %d bytes takes more", c.stored)
	}
	written := 0
	for len(p) > 0 {
		// a full chunk with more to come is not the last
		if len(c.buf) == ChunkSize {
			if err := c.seal(false); err != nil {
				return written, err
			}
		}
		n := copy(c.buf[len(c.buf):ChunkSize], p)
		c.buf = c.buf[:len(c.buf)+n]
		p = p[n:]
		written += n
		c.consumed += int64(n)
	}
	return written, nil
}

func (c *chunkWriter) seal(last bool) error {
	c.sealed = c.aead.Seal(c.sealed[:0], nonce(c.index, last), c.buf, c.aad)
	c.index++
	c.buf = c.buf[:0]
	_, err := c.out.Write(c.sealed)
	return err
}

// finish seals the last chunk.
func (c *chunkWriter) finish() error {
	if c.consumed != c.stored {
		return fmt.Errorf("a segment of %d bytes got %d", c.stored, c.consumed)
	}
	if err := c.seal(true); err != nil {
		return err
	}
	if c.index != chunks(c.stored) {
		return fmt.Errorf("a segment of %d bytes took %d chunks", c.stored, c.index)
	}
	return nil
}

// counter counts what is written to it.
type counter struct{ n int64 }

func (c *counter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}
