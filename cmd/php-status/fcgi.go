package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The parts of FastCGI 1.0 that a client of one responder needs.
const (
	fcgiVersion      = 1
	fcgiBeginRequest = 1
	fcgiEndRequest   = 3
	fcgiParams       = 4
	fcgiStdin        = 5
	fcgiStdout       = 6
	fcgiStderr       = 7

	fcgiResponder = 1
	// one request per connection, so its id can be fixed
	fcgiRequestID  = 1
	fcgiMaxContent = 65535

	fcgiRequestComplete = 0
	fcgiCantMultiplex   = 1
	fcgiOverloaded      = 2
	fcgiUnknownRole     = 3
)

// A response may hold this much output, and this much of what the server wrote
// to its error stream is kept, so a confused server cannot fill the memory.
const (
	maxOutput = 16 << 20
	maxStderr = 64 << 10
)

// errClosedEarly is a connection that ended before the response did, as when
// the worker that answered it crashed.
var errClosedEarly = errors.New("the server closed the connection before it ended the response")

// fcgiResponse is a responder's answer: a CGI response and its error stream.
type fcgiResponse struct {
	Status int
	Header textproto.MIMEHeader
	Body   []byte
	Stderr []byte
}

// fcgiGet sends one request without a body to a FastCGI responder at the
// address and reads the answer. The connection closes with the request.
func fcgiGet(ctx context.Context, network, address string, params map[string]string) (*fcgiResponse, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	// a cancelled context without a deadline ends a read or write too
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	if err := writeRequest(conn, params); err != nil {
		return nil, fmt.Errorf("sending the request: %w", contextError(ctx, err))
	}

	stdout, stderr, err := readResponse(conn)
	if err != nil {
		return nil, contextError(ctx, err)
	}

	response, err := parseCGIResponse(stdout)
	if err != nil {
		return nil, err
	}
	response.Stderr = stderr

	return response, nil
}

// contextError names the context's reason when it is why a read or write
// failed. The connection's deadline is the context's, and may pass a moment
// before the context says so.
func contextError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
	}

	return err
}

// writeRequest writes a request of the responder role: the parameters, and an
// empty body.
func writeRequest(w io.Writer, params map[string]string) error {
	buffered := bufio.NewWriter(w)

	// the role, then flags: 0 asks the server to close the connection afterwards
	begin := []byte{0, fcgiResponder, 0, 0, 0, 0, 0, 0}
	if err := writeRecord(buffered, fcgiBeginRequest, begin); err != nil {
		return err
	}

	// FPM reads every record of parameters on its own, so a pair never spans two
	records := [][]byte{nil}
	for _, pair := range encodeParams(params) {
		if len(pair) > fcgiMaxContent {
			return fmt.Errorf("a parameter of %d bytes does not fit in a record", len(pair))
		}
		last := len(records) - 1
		if len(records[last])+len(pair) > fcgiMaxContent {
			records = append(records, nil)
			last++
		}
		records[last] = append(records[last], pair...)
	}
	for _, record := range records {
		if len(record) == 0 {
			continue
		}
		if err := writeRecord(buffered, fcgiParams, record); err != nil {
			return err
		}
	}

	// an empty record ends a stream
	if err := writeRecord(buffered, fcgiParams, nil); err != nil {
		return err
	}
	if err := writeRecord(buffered, fcgiStdin, nil); err != nil {
		return err
	}

	return buffered.Flush()
}

func writeRecord(w io.Writer, kind byte, content []byte) error {
	if len(content) > fcgiMaxContent {
		return fmt.Errorf("a record cannot hold %d bytes", len(content))
	}

	// padding is optional, and FPM refuses a record whose content and padding
	// add up to more than the largest content
	padding := (8 - len(content)%8) % 8
	if len(content)+padding > fcgiMaxContent {
		padding = 0
	}
	header := []byte{fcgiVersion, kind}
	header = binary.BigEndian.AppendUint16(header, fcgiRequestID)
	header = binary.BigEndian.AppendUint16(header, uint16(len(content))) //nolint:gosec // checked above
	header = append(header, byte(padding), 0)                            //nolint:gosec // below 8

	if _, err := w.Write(header); err != nil {
		return err
	}
	if _, err := w.Write(content); err != nil {
		return err
	}
	_, err := w.Write(make([]byte, padding))
	return err
}

// encodeParams encodes name-value pairs, one slice each, in the order of their
// names so that a request is always the same bytes.
func encodeParams(params map[string]string) [][]byte {
	var pairs [][]byte
	for _, name := range slices.Sorted(maps.Keys(params)) {
		value := params[name]
		var pair []byte
		pair = appendLength(pair, len(name))
		pair = appendLength(pair, len(value))
		pair = append(pair, name...)
		pair = append(pair, value...)
		pairs = append(pairs, pair)
	}
	return pairs
}

// appendLength writes a length in one byte below 128, otherwise in four with
// the top bit set.
func appendLength(b []byte, n int) []byte {
	if n < 128 {
		return append(b, byte(n)) //nolint:gosec // below 128
	}
	return binary.BigEndian.AppendUint32(b, uint32(n)|1<<31) //nolint:gosec // a parameter is far below 2 GiB
}

// readResponse reads records until the end of the request and returns what the
// server wrote to the output and to the error stream.
func readResponse(r io.Reader) (stdout, stderr []byte, err error) {
	reader := bufio.NewReader(r)
	header := make([]byte, 8)

	for {
		if _, err := io.ReadFull(reader, header); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, nil, errClosedEarly
			}
			return nil, nil, err
		}
		if header[0] != fcgiVersion {
			return nil, nil, fmt.Errorf("the server does not speak FastCGI 1: a record has version %d", header[0])
		}

		kind := header[1]
		id := int(header[2])<<8 | int(header[3])
		length := int(header[4])<<8 | int(header[5])
		padding := int(header[6])

		content := make([]byte, length+padding)
		if _, err := io.ReadFull(reader, content); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, nil, errors.New("the server closed the connection in the middle of a record")
			}
			return nil, nil, err
		}
		content = content[:length]

		// management records have id 0; nothing else shares the connection
		if id != fcgiRequestID {
			continue
		}

		switch kind {
		case fcgiStdout:
			if len(stdout)+len(content) > maxOutput {
				return nil, nil, fmt.Errorf("the response is longer than %d bytes", maxOutput)
			}
			stdout = append(stdout, content...)
		case fcgiStderr:
			if room := maxStderr - len(stderr); room > 0 {
				stderr = append(stderr, content[:min(room, len(content))]...)
			}
		case fcgiEndRequest:
			if len(content) < 8 {
				return nil, nil, errors.New("the server ended the request with a short record")
			}
			if status := content[4]; status != fcgiRequestComplete {
				return nil, nil, fmt.Errorf("the server did not take the request: %s", protocolStatusText(status))
			}
			return stdout, stderr, nil
		}
	}
}

func protocolStatusText(status byte) string {
	switch status {
	case fcgiCantMultiplex:
		return "it cannot take more requests on this connection"
	case fcgiOverloaded:
		return "it is overloaded"
	case fcgiUnknownRole:
		return "it does not know the responder role"
	default:
		return fmt.Sprintf("protocol status %d", status)
	}
}

// parseCGIResponse splits a CGI response into its status, header and body. A
// response without a Status header has the status 200.
func parseCGIResponse(output []byte) (*fcgiResponse, error) {
	end, separator := bytes.Index(output, []byte("\r\n\r\n")), 4
	if lf := bytes.Index(output, []byte("\n\n")); lf >= 0 && (end < 0 || lf < end) {
		end, separator = lf, 2
	}
	if end < 0 {
		return nil, errors.New("the response has no end of its header")
	}

	headerBlock := append(output[:end:end], "\r\n\r\n"...)
	header, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(headerBlock))).ReadMIMEHeader()
	if err != nil {
		return nil, fmt.Errorf("the response has a bad header: %w", err)
	}

	status := 200
	if value := header.Get("Status"); value != "" {
		code, _, _ := strings.Cut(value, " ")
		status, err = strconv.Atoi(code)
		if err != nil || status < 100 || status > 999 {
			return nil, fmt.Errorf("the response has a bad status %q", value)
		}
	}

	return &fcgiResponse{Status: status, Header: header, Body: output[end+separator:]}, nil
}
