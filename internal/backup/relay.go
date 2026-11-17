package backup

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
)

// A database dump reaches restic through the backup tool's relay (#29).
// restic runs the command of --stdin-from-command with /dev/null for its
// input, so the dump, which the agent reads from the database's container
// with docker exec, comes in on the job container's attached standard input
// as frames: the backup tool hands them to the relay, the command restic
// runs, through a unix socket, and the relay writes their data to its output,
// which restic saves. The last frame says how the dump ended: the relay ends
// with the dump's exit code, and restic saves no snapshot of a dump that
// failed.
//
// A frame is its type, one byte, the length of its payload, four bytes big
// endian, and the payload:
//
//	'd'  data: the next bytes of the dump
//	'x'  the end: the dump's exit code, four bytes big endian, and what it
//	     said, if anything

// The types of frame.
const (
	FrameData = 'd'
	FrameExit = 'x'
)

// maxFrame is the largest payload of a frame.
const maxFrame = 1 << 20

// ErrDumpCut is a stream of frames that ended before its exit frame.
var ErrDumpCut = errors.New("the dump ended before it said how it went")

// FrameWriter writes a dump as frames.
type FrameWriter struct{ w io.Writer }

// NewFrameWriter writes frames to w.
func NewFrameWriter(w io.Writer) *FrameWriter { return &FrameWriter{w: w} }

// Write writes data as data frames.
func (f *FrameWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), maxFrame)
		if err := f.frame(FrameData, p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// End writes the exit frame: the dump's exit code and what it said.
func (f *FrameWriter) End(code int, message string) error {
	if len(message) > maxFrame-4 {
		message = message[len(message)-(maxFrame-4):]
	}
	payload := make([]byte, 4, 4+len(message))
	binary.BigEndian.PutUint32(payload, uint32(int32(code))) //nolint:gosec // an exit code
	return f.frame(FrameExit, append(payload, message...))
}

func (f *FrameWriter) frame(kind byte, payload []byte) error {
	frame := make([]byte, 5, 5+len(payload))
	frame[0] = kind
	binary.BigEndian.PutUint32(frame[1:], uint32(len(payload))) //nolint:gosec // at most maxFrame
	_, err := f.w.Write(append(frame, payload...))
	return err
}

// DumpEnd is how a dump ended, as its exit frame says.
type DumpEnd struct {
	Code    int
	Message string
}

// ReadFrames reads a dump's frames from r and writes their data to data, up
// to the exit frame, which it returns. A stream that ends before it is
// ErrDumpCut.
func ReadFrames(r io.Reader, data io.Writer) (DumpEnd, error) {
	header := make([]byte, 5)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return DumpEnd{}, ErrDumpCut
			}
			return DumpEnd{}, err
		}
		size := binary.BigEndian.Uint32(header[1:])
		if size > maxFrame {
			return DumpEnd{}, fmt.Errorf("the dump's stream is broken: a frame of %d bytes", size)
		}
		switch header[0] {
		case FrameData:
			copied, err := io.CopyN(data, r, int64(size))
			if err != nil {
				if copied < int64(size) && errors.Is(err, io.EOF) {
					return DumpEnd{}, ErrDumpCut
				}
				return DumpEnd{}, err
			}
		case FrameExit:
			payload := make([]byte, size)
			if _, err := io.ReadFull(r, payload); err != nil {
				return DumpEnd{}, ErrDumpCut
			}
			if size < 4 {
				return DumpEnd{}, errors.New("the dump's stream is broken: an exit frame without a code")
			}
			return DumpEnd{Code: int(int32(binary.BigEndian.Uint32(payload))), Message: string(payload[4:])}, nil //nolint:gosec // an exit code
		default:
			return DumpEnd{}, fmt.Errorf("the dump's stream is broken: a frame of type %q", header[0])
		}
	}
}

// Relay writes the data of the dump that r brings to stdout, and returns the
// exit code the relay ends with: the dump's, after it wrote what the dump
// said to stderr, or 1 for a stream that broke off.
func Relay(r io.Reader, stdout, stderr io.Writer) int {
	end, err := ReadFrames(r, stdout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if message := strings.TrimRight(end.Message, "\n"); message != "" {
		fmt.Fprintln(stderr, message)
	}
	if end.Code < 0 || end.Code > 255 {
		// a code the process cannot end with: it still failed
		return 1
	}
	return end.Code
}

// RelaySocket relays the dump that comes through the backup tool's socket.
func RelaySocket(socket string, stdout, stderr io.Writer) int {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		fmt.Fprintln(stderr, "the relay cannot reach the backup tool:", err)
		return 1
	}
	defer conn.Close()
	return Relay(conn, stdout, stderr)
}
