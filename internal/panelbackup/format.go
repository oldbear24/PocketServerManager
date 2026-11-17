// Package panelbackup is PCPB1, the format of the panel's encrypted copy of
// itself (#32): its database and stored files, encrypted on the panel with
// keys derived from the master key, so that the server that uploads the copy
// and the repository that keeps it never see them unencrypted.
//
// An export is a plaintext header of 48 bytes and a series of segments: the
// manifest, the database, the stored files in path order, and the trailer.
//
//	header   "PCPB" | version 0x01 | 3 zero bytes | key_id[16] | export_id[16] | created_unix int64 BE
//	segment  kind u8 | flags u8 | name_len u16 BE | name | salt[32] | stored_len u64 BE | chunks
//
// A segment's fixed fields take 44 bytes besides its name. Its stored bytes,
// the data.db flate-compressed (flags bit 0) and anything else as it is, are
// cut into chunks of 64 KiB, at least one, the last one shorter or empty, and
// each is sealed with AES-256-GCM: chunk i takes the nonce i as 11 bytes big
// endian and then 0x01 for the last chunk, 0x00 for the others (the STREAM
// construction), so a cut, a chunk moved or a chunk taken from elsewhere
// fails; its additional data is "PCPB1 segment", kind, flags, name_len, name
// and stored_len, and for the trailer the header too. A segment's key is
// HKDF-SHA256(K_pb, salt, "pcpb1 segment key"), with K_pb the master key's
// key "pocketcloud panel backup v1": every key seals one plaintext. The salt
// is random, except for a stored file's, which is HMAC-SHA256 under the key
// HKDF-SHA256(K_pb, nil, "pcpb1 file id") of the file's name, a zero byte and
// the SHA-256 of its content: a file that did not change encrypts to the same
// bytes in every export, which restic deduplicates, and nobody without the
// master key can confirm a guess of a file. The trailer lists every segment
// before it with the SHA-256 of its bytes, and binds them to the header.
//
// See docs/architecture.md, "Panel backup and disaster recovery (#32)".
package panelbackup

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/oldbear24/PocketServerManager/internal/secrets"
)

// The form of an export.
const (
	// Magic starts every export.
	Magic = "PCPB"
	// Version is the version of the format this package writes and reads.
	Version = 1
	// HeaderSize is the size of the plaintext header: the magic (4), the
	// version (1), 3 zero bytes, the key id (16), the export id (16) and the
	// time it was made (8).
	HeaderSize = 48
	// SegmentFixedSize is the size of a segment's fixed fields: kind (1),
	// flags (1), name_len (2), salt (32) and stored_len (8).
	SegmentFixedSize = 44
	// ChunkSize is the plaintext size of a chunk but the last.
	ChunkSize = 64 << 10
	// TagSize is the size of a chunk's GCM tag.
	TagSize = 16
	// SaltSize is the size of a segment's salt.
	SaltSize = 32
)

// Kinds of segment, in the order an export has them.
const (
	KindManifest byte = 1
	KindDatabase byte = 2
	KindFile     byte = 3
	KindTrailer  byte = 0xFF
)

// FlagFlate marks a database segment whose stored bytes are flate
// compressed. No other kind may have it, and no other flag exists.
const FlagFlate byte = 1

// The names of the segments that are not stored files.
const (
	ManifestName = "manifest.json"
	DatabaseName = "data.db"
	TrailerName  = "trailer"
)

// The limits a reader checks before it allocates anything.
const (
	// MaxName is the longest name of a segment, in bytes.
	MaxName = 1024
	// MaxStored is the most stored bytes of a segment, and the most bytes
	// of the database once decompressed.
	MaxStored = 64 << 30
	// MaxSegments is the most segments of an export, the manifest, the
	// database and the trailer included.
	MaxSegments = 1_000_000
	// MaxManifest is the most stored bytes of the manifest.
	MaxManifest = 1 << 20
	// MaxTrailer is the most stored bytes of the trailer.
	MaxTrailer = 256 << 20
)

// The purposes of the keys of an export.
const (
	// Purpose names K_pb, the master key's key for panel backups.
	Purpose     = "pocketcloud panel backup v1"
	segmentInfo = "pcpb1 segment key"
	fileIDInfo  = "pcpb1 file id"
	aadPrefix   = "PCPB1 segment"
)

var (
	// ErrNotAnExport is a stream that does not start as an export.
	ErrNotAnExport = errors.New("this is no PocketCloud panel backup")
	// ErrVersion is an export of a version of the format this package
	// does not read.
	ErrVersion = errors.New("this panel backup is of a format this panel does not read")
	// ErrOtherKey is an export sealed with another master key, as its key id
	// says: nothing of it is decrypted. Its error is a *KeyError.
	ErrOtherKey = errors.New("this panel backup was sealed with another master key")
	// ErrDamaged is an export that does not open as it was written: a chunk
	// that does not open, a segment out of its place or beyond a limit, a
	// manifest or a trailer that disagrees. Its error is a *DamagedError.
	ErrDamaged = errors.New("the panel backup is damaged")
	// ErrIncomplete is an export that ends before its trailer, or goes on
	// after it. Its error is a *DamagedError that is ErrDamaged too.
	ErrIncomplete = errors.New("the panel backup is incomplete")
)

// KeyError is an export sealed with another master key than the one given.
type KeyError struct {
	// Export is the key id of the export's header; Key that of the key given.
	Export, Key string
}

func (e *KeyError) Error() string {
	return fmt.Sprintf("this panel backup was sealed with another master key (id %s) than the one given (id %s)", e.Export, e.Key)
}

// Is makes a KeyError ErrOtherKey.
func (e *KeyError) Is(target error) bool { return target == ErrOtherKey }

// DamagedError says where an export stopped opening: segments are numbered
// from 1, the manifest's.
type DamagedError struct {
	Segment    int
	Reason     string
	Incomplete bool
}

func (e *DamagedError) Error() string {
	what := "damaged"
	if e.Incomplete {
		what = "incomplete"
	}
	return fmt.Sprintf("the panel backup is %s at segment %d: %s", what, e.Segment, e.Reason)
}

// Is makes a DamagedError ErrDamaged, and ErrIncomplete when it is one.
func (e *DamagedError) Is(target error) bool {
	return target == ErrDamaged || (e.Incomplete && target == ErrIncomplete)
}

// Header is an export's plaintext header. Nothing vouches for it until the
// trailer opened: Reader and Verify check it, ReadHeader only reads it.
type Header struct {
	// KeyID is secrets.KeyID of the master key that sealed the export.
	KeyID string
	// ExportID names the export: 32 hex characters, random.
	ExportID string
	// Created is when the panel made it, to the second, in UTC.
	Created time.Time
}

// marshal is the header's 48 bytes.
func (h Header) marshal() ([]byte, error) {
	keyID, err := hex.DecodeString(h.KeyID)
	if err != nil || len(keyID) != 16 {
		return nil, fmt.Errorf("a key id is 32 hex characters, not %q", h.KeyID)
	}
	exportID, err := hex.DecodeString(h.ExportID)
	if err != nil || len(exportID) != 16 {
		return nil, fmt.Errorf("an export id is 32 hex characters, not %q", h.ExportID)
	}
	if h.Created.Unix() <= 0 {
		return nil, fmt.Errorf("an export cannot be made at %s", h.Created)
	}
	out := make([]byte, 0, HeaderSize)
	out = append(out, Magic...)
	out = append(out, Version, 0, 0, 0)
	out = append(out, keyID...)
	out = append(out, exportID...)
	return binary.BigEndian.AppendUint64(out, uint64(h.Created.Unix())), nil //nolint:gosec // a time before 1970 is refused above
}

// parseHeader reads the header's 48 bytes.
func parseHeader(raw []byte) (Header, error) {
	if len(raw) != HeaderSize || string(raw[:4]) != Magic {
		return Header{}, ErrNotAnExport
	}
	if raw[4] != Version || raw[5] != 0 || raw[6] != 0 || raw[7] != 0 {
		return Header{}, fmt.Errorf("%w (version %d.%d.%d.%d)", ErrVersion, raw[4], raw[5], raw[6], raw[7])
	}
	created := int64(binary.BigEndian.Uint64(raw[40:48])) //nolint:gosec // a time before 1970 is refused below
	if created <= 0 {
		return Header{}, ErrNotAnExport
	}
	return Header{
		KeyID:    hex.EncodeToString(raw[8:24]),
		ExportID: hex.EncodeToString(raw[24:40]),
		Created:  time.Unix(created, 0).UTC(),
	}, nil
}

// ReadHeader reads an export's header, which needs no key: the key id it
// names tells which master key opens it, before anything is decrypted.
func ReadHeader(r io.Reader) (Header, error) {
	raw := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, raw); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return Header{}, ErrNotAnExport
		}
		return Header{}, err
	}
	return parseHeader(raw)
}

// keys are the keys of an export, derived from the master key.
type keys struct {
	pb     []byte
	fileID []byte
}

func deriveKeys(master []byte) (keys, error) {
	pb, err := secrets.DeriveKey(master, Purpose)
	if err != nil {
		return keys{}, err
	}
	fileID, err := hkdf.Key(sha256.New, pb, nil, fileIDInfo, 32)
	if err != nil {
		return keys{}, err
	}
	return keys{pb: pb, fileID: fileID}, nil
}

// segmentKey is the key of the segment with this salt.
func (k keys) segmentKey(salt []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, k.pb, salt, segmentInfo, 32)
}

// storagePath is a stored file of PocketBase's local storage:
// storage/<collection id>/<record id>/<file name>. A collection's id is
// PocketBase's \w{1,100} ("pbc_" and digits for the panel's), a record's the
// default [a-z0-9]{15}.
var storagePath = regexp.MustCompile(`^storage/[A-Za-z0-9_]{1,100}/[a-z0-9]{15}/[^/\\\x00]{1,255}$`)

// validName says whether a segment of this kind may have this name.
func validName(kind byte, name string) bool {
	switch kind {
	case KindManifest:
		return name == ManifestName
	case KindDatabase:
		return name == DatabaseName
	case KindTrailer:
		return name == TrailerName
	case KindFile:
		return ValidFileName(name)
	}
	return false
}

// ValidFileName says whether name is a stored file an export may hold: a
// path of PocketBase's local storage, in UTF-8, with no control character,
// whose file is neither "." nor "..".
func ValidFileName(name string) bool {
	if len(name) > MaxName || !storagePath.MatchString(name) || !utf8.ValidString(name) {
		return false
	}
	if base := name[strings.LastIndexByte(name, '/')+1:]; base == "." || base == ".." {
		return false
	}
	return !strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// segmentHeader is a segment's fields before its chunks.
type segmentHeader struct {
	kind, flags byte
	name        string
	salt        []byte
	stored      int64
}

// marshal is the segment's fields as an export has them.
func (s segmentHeader) marshal() []byte {
	out := make([]byte, 0, SegmentFixedSize+len(s.name))
	out = append(out, s.kind, s.flags)
	out = binary.BigEndian.AppendUint16(out, uint16(len(s.name))) //nolint:gosec // names are checked to be at most MaxName
	out = append(out, s.name...)
	out = append(out, s.salt...)
	return binary.BigEndian.AppendUint64(out, uint64(s.stored)) //nolint:gosec // stored sizes are checked to be at most MaxStored
}

// aad is the additional data of the segment's chunks; the trailer's binds
// the export's header too.
func (s segmentHeader) aad(header []byte) []byte {
	var out bytes.Buffer
	out.WriteString(aadPrefix)
	out.WriteByte(s.kind)
	out.WriteByte(s.flags)
	_ = binary.Write(&out, binary.BigEndian, uint16(len(s.name))) //nolint:gosec // names are checked to be at most MaxName
	out.WriteString(s.name)
	_ = binary.Write(&out, binary.BigEndian, uint64(s.stored)) //nolint:gosec // stored sizes are checked to be at most MaxStored
	if s.kind == KindTrailer {
		out.Write(header)
	}
	return out.Bytes()
}

// chunks is how many chunks hold stored bytes: at least one.
func chunks(stored int64) int64 {
	if stored == 0 {
		return 1
	}
	return (stored + ChunkSize - 1) / ChunkSize
}

// nonce is chunk i's nonce: i as 11 bytes big endian, then whether it is the
// last.
func nonce(i int64, last bool) []byte {
	out := make([]byte, 12)
	binary.BigEndian.PutUint64(out[3:11], uint64(i)) //nolint:gosec // chunk indexes are never negative
	if last {
		out[11] = 1
	}
	return out
}

// TrailerEntry is a segment as the trailer lists it.
type TrailerEntry struct {
	Kind      byte   `json:"kind"`
	Name      string `json:"name"`
	StoredLen int64  `json:"stored_len"`
	// SHA256 is the SHA-256 of the segment's bytes in the export, its
	// fields and its sealed chunks, as hex.
	SHA256 string `json:"sha256"`
}

// trailer is the trailer's JSON.
type trailer struct {
	Segments []TrailerEntry `json:"segments"`
}
