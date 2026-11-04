package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/fcgi"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// serveFCGI runs Go's own FastCGI server with the handler, as a stand-in for FPM.
func serveFCGI(t *testing.T, network string, handler http.Handler) string {
	t.Helper()

	address := "127.0.0.1:0"
	if network == "unix" {
		address = filepath.Join(t.TempDir(), "fpm.sock")
	}

	listener, err := net.Listen(network, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() { _ = fcgi.Serve(listener, handler) }()

	return listener.Addr().String()
}

// echo answers with what the request carried, so a test can see what arrived.
func echo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Seen", "yes")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"method": r.Method,
		"path":   r.URL.Path,
		"query":  r.URL.RawQuery,
		"env":    fcgi.ProcessEnv(r),
	})
}

func TestRequestReachesGoFastCGIServer(t *testing.T) {
	for _, network := range []string{"tcp", "unix"} {
		t.Run(network, func(t *testing.T) {
			address := serveFCGI(t, network, http.HandlerFunc(echo))

			long := strings.Repeat("x", 300)
			params := requestParams("/status", "/srv/status", "json&full")
			params["PHP_ADMIN_VALUE"] = "open_basedir=/usr/local/lib\nauto_prepend_file="
			params["LONG_NAME_"+strings.Repeat("N", 200)] = long

			response, err := fcgiGet(context.Background(), network, address, params)
			if err != nil {
				t.Fatal(err)
			}

			if response.Status != http.StatusAccepted || response.Header.Get("X-Seen") != "yes" {
				t.Fatalf("status %d, header %v", response.Status, response.Header)
			}

			var seen struct {
				Method string            `json:"method"`
				Path   string            `json:"path"`
				Query  string            `json:"query"`
				Env    map[string]string `json:"env"`
			}
			if err := json.Unmarshal(response.Body, &seen); err != nil {
				t.Fatalf("%v: %q", err, response.Body)
			}

			if seen.Method != "GET" || seen.Path != "/status" || seen.Query != "json&full" {
				t.Errorf("the server saw %+v", seen)
			}
			if seen.Env["SCRIPT_FILENAME"] != "/srv/status" || seen.Env["PHP_ADMIN_VALUE"] != params["PHP_ADMIN_VALUE"] {
				t.Errorf("the parameters arrived as %v", seen.Env)
			}
			if seen.Env["LONG_NAME_"+strings.Repeat("N", 200)] != long {
				t.Error("a parameter longer than 127 bytes did not arrive whole")
			}
		})
	}
}

func TestParametersLargerThanOneRecord(t *testing.T) {
	address := serveFCGI(t, "tcp", http.HandlerFunc(echo))

	// 120 kB in three parameters: more than one record holds
	params := requestParams("/x", "/x", "")
	for _, name := range []string{"HUGE_A", "HUGE_B", "HUGE_C"} {
		params[name] = strings.Repeat(name, 10_000/len(name)*4)
	}

	response, err := fcgiGet(context.Background(), "tcp", address, params)
	if err != nil {
		t.Fatal(err)
	}

	var seen struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(response.Body, &seen); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"HUGE_A", "HUGE_B", "HUGE_C"} {
		if seen.Env[name] != params[name] {
			t.Fatalf("%s arrived with %d bytes instead of %d", name, len(seen.Env[name]), len(params[name]))
		}
	}
}

// FPM parses every record of parameters on its own, and refuses one whose
// content and padding exceed 65535 bytes, which Go's FastCGI server accepts.
func TestParameterRecordsAreWholeForFPM(t *testing.T) {
	params := map[string]string{"A": strings.Repeat("a", 40_000), "B": strings.Repeat("b", 40_000), "C": strings.Repeat("c", 65_529)}

	var buffer bytes.Buffer
	if err := writeRequest(&buffer, params); err != nil {
		t.Fatal(err)
	}

	stream := buffer.Bytes()
	records := 0
	got := map[string]string{}
	for len(stream) > 0 {
		kind := stream[1]
		length := int(stream[4])<<8 | int(stream[5])
		padding := int(stream[6])
		if length+padding > 65535 {
			t.Fatalf("a record of %d bytes with %d of padding", length, padding)
		}
		content := stream[8 : 8+length]
		stream = stream[8+length+padding:]
		if kind != fcgiParams || length == 0 {
			continue
		}
		records++

		// every record holds whole pairs
		for len(content) > 0 {
			nameLength, n := readLength(t, content)
			valueLength, m := readLength(t, content[n:])
			start := n + m
			if start+nameLength+valueLength > len(content) {
				t.Fatalf("a pair runs past the end of record %d", records)
			}
			got[string(content[start:start+nameLength])] = string(content[start+nameLength : start+nameLength+valueLength])
			content = content[start+nameLength+valueLength:]
		}
	}

	if records != 3 {
		t.Errorf("%d records of parameters, want 3", records)
	}
	for name, value := range params {
		if got[name] != value {
			t.Errorf("%s: %d bytes, want %d", name, len(got[name]), len(value))
		}
	}
}

func readLength(t *testing.T, b []byte) (int, int) {
	t.Helper()
	if len(b) == 0 {
		t.Fatal("a pair is cut off")
	}
	if b[0] < 128 {
		return int(b[0]), 1
	}
	if len(b) < 4 {
		t.Fatal("a length is cut off")
	}
	return int(b[0]&0x7f)<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3]), 4
}

func TestParameterLargerThanARecordIsRefused(t *testing.T) {
	err := writeRequest(io.Discard, map[string]string{"HUGE": strings.Repeat("x", 70_000)})
	if err == nil || !strings.Contains(err.Error(), "does not fit in a record") {
		t.Fatalf("%v", err)
	}
}

func TestResponseLongerThanOneRecord(t *testing.T) {
	body := bytes.Repeat([]byte("abcdefgh"), 40_000) // 320 kB
	address := serveFCGI(t, "tcp", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))

	response, err := fcgiGet(context.Background(), "tcp", address, requestParams("/x", "/x", ""))
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != http.StatusOK || !bytes.Equal(response.Body, body) {
		t.Fatalf("status %d and %d bytes of %d", response.Status, len(response.Body), len(body))
	}
}

func TestStatusFromTheHeader(t *testing.T) {
	address := serveFCGI(t, "tcp", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "File not found.", http.StatusNotFound)
	}))

	response, err := fcgiGet(context.Background(), "tcp", address, requestParams("/x", "/x", ""))
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != http.StatusNotFound || !strings.Contains(string(response.Body), "File not found.") {
		t.Fatalf("status %d, body %q", response.Status, response.Body)
	}
}

// fakeServer accepts one connection, reads the request and answers with the
// records that answer writes, for what Go's server never does.
func fakeServer(t *testing.T, answer func(conn net.Conn)) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		if err := readRequest(conn); err != nil {
			t.Error(err)
			return
		}
		answer(conn)
	}()

	return listener.Addr().String()
}

// readRequest reads records up to the end of the request's input.
func readRequest(r io.Reader) error {
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			return err
		}
		length := int(binary.BigEndian.Uint16(header[4:6]))
		if _, err := io.CopyN(io.Discard, r, int64(length+int(header[6]))); err != nil {
			return err
		}
		if header[1] == fcgiStdin && length == 0 {
			return nil
		}
	}
}

func record(kind byte, id uint16, content []byte) []byte {
	var b bytes.Buffer
	if err := writeRecordID(&b, kind, id, content); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func writeRecordID(w io.Writer, kind byte, id uint16, content []byte) error {
	padding := (8 - len(content)%8) % 8
	header := []byte{fcgiVersion, kind, byte(id >> 8), byte(id), byte(len(content) >> 8), byte(len(content)), byte(padding), 0}
	_, err := w.Write(append(append(header, content...), make([]byte, padding)...))
	return err
}

func endRequest(protocolStatus byte) []byte {
	return record(fcgiEndRequest, fcgiRequestID, []byte{0, 0, 0, 0, protocolStatus, 0, 0, 0})
}

func TestStderrAndRecordsOfOthers(t *testing.T) {
	address := fakeServer(t, func(conn net.Conn) {
		_, _ = conn.Write(record(fcgiStdout, 0, []byte("not ours")))
		_, _ = conn.Write(record(fcgiStderr, fcgiRequestID, []byte("PHP message: PHP Warning: x")))
		_, _ = conn.Write(record(fcgiStdout, fcgiRequestID, []byte("Status: 500 Internal Server Error\nContent-type: text/html\n\n")))
		_, _ = conn.Write(record(fcgiStdout, fcgiRequestID, []byte("oops")))
		_, _ = conn.Write(endRequest(fcgiRequestComplete))
	})

	response, err := fcgiGet(context.Background(), "tcp", address, requestParams("/x", "/x", ""))
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != 500 || string(response.Body) != "oops" || string(response.Stderr) != "PHP message: PHP Warning: x" {
		t.Fatalf("status %d, body %q, stderr %q", response.Status, response.Body, response.Stderr)
	}
}

func TestRefusedOrBrokenResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer []byte
		want   string
	}{
		{"overloaded", endRequest(fcgiOverloaded), "overloaded"},
		{"closed early", record(fcgiStdout, fcgiRequestID, []byte("Status: 200\r\n\r\n")), "closed the connection before it ended"},
		{"cut record", record(fcgiStdout, fcgiRequestID, []byte("Status: 200 OK\r\n\r\nbody"))[:12], "middle of a record"},
		{"wrong version", append([]byte{2}, endRequest(fcgiRequestComplete)[1:]...), "does not speak FastCGI 1"},
		{"no header end", append(record(fcgiStdout, fcgiRequestID, []byte("Status: 200 OK\r\n")), endRequest(fcgiRequestComplete)...), "no end of its header"},
		{"bad status", append(record(fcgiStdout, fcgiRequestID, []byte("Status: abc\r\n\r\n")), endRequest(fcgiRequestComplete)...), "bad status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			address := fakeServer(t, func(conn net.Conn) { _, _ = conn.Write(tc.answer) })

			_, err := fcgiGet(context.Background(), "tcp", address, requestParams("/x", "/x", ""))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one saying %q", err, tc.want)
			}
		})
	}
}

func TestSilentServerTimesOut(t *testing.T) {
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	address := fakeServer(t, func(net.Conn) { <-hold })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := fcgiGet(ctx, "tcp", address, requestParams("/x", "/x", ""))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v, want the deadline", err)
	}
	if waited := time.Since(started); waited > 2*time.Second {
		t.Fatalf("waited %v", waited)
	}
}

func TestParseCGIResponse(t *testing.T) {
	for _, tc := range []struct {
		output string
		status int
		body   string
	}{
		{"Content-Type: text/plain\r\n\r\npong", 200, "pong"},
		{"Status: 404 Not Found\r\nContent-Type: text/html\r\n\r\nFile not found.\n", 404, "File not found.\n"},
		{"Status: 403\nContent-type: text/html\n\nAccess denied.", 403, "Access denied."},
		{"Content-Type: application/json\r\n\r\n", 200, ""},
		{"X-A: 1\r\n\r\nline\n\nwith a blank line", 200, "line\n\nwith a blank line"},
	} {
		response, err := parseCGIResponse([]byte(tc.output))
		if err != nil {
			t.Errorf("%q: %v", tc.output, err)
			continue
		}
		if response.Status != tc.status || string(response.Body) != tc.body {
			t.Errorf("%q: status %d, body %q", tc.output, response.Status, response.Body)
		}
	}
}

func TestEncodeParamsIsStable(t *testing.T) {
	params := map[string]string{"B": "2", "A": "1", strings.Repeat("L", 130): ""}

	first := bytes.Join(encodeParams(params), nil)
	for range 20 {
		if !bytes.Equal(bytes.Join(encodeParams(params), nil), first) {
			t.Fatal("the same parameters encoded differently")
		}
	}

	// the 130-byte name takes a four-byte length with the top bit set
	want := fmt.Sprintf("\x01\x01A1\x01\x01B2\x80\x00\x00\x82\x00%s", strings.Repeat("L", 130))
	if string(first) != want {
		t.Fatalf("encoded as %q", first)
	}
}
