package backup

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestADumpsFramesCarryItsDataAndItsEnd (#29): the data of every data frame,
// in order, and the exit frame's code and message.
func TestADumpsFramesCarryItsDataAndItsEnd(t *testing.T) {
	var stream bytes.Buffer
	frames := NewFrameWriter(&stream)
	big := strings.Repeat("x", maxFrame+10)
	for _, part := range []string{"CREATE TABLE a;\n", "", big} {
		if n, err := frames.Write([]byte(part)); err != nil || n != len(part) {
			t.Fatalf("%d %v", n, err)
		}
	}
	if err := frames.End(0, "pg_dump: warning: nothing\n"); err != nil {
		t.Fatal(err)
	}
	// a data frame is never larger than the limit
	if stream.Bytes()[0] != FrameData || stream.Bytes()[5+16] != FrameData {
		t.Errorf("frames: %q", stream.Bytes()[:30])
	}
	var data bytes.Buffer
	end, err := ReadFrames(bytes.NewReader(stream.Bytes()), &data)
	if err != nil || end.Code != 0 || end.Message != "pg_dump: warning: nothing\n" || data.String() != "CREATE TABLE a;\n"+big {
		t.Errorf("read %+v %v, %d bytes", end, err, data.Len())
	}

	// the relay passes the data on, says what the dump said and ends with its code
	stream.Reset()
	_, _ = frames.Write([]byte("partial"))
	_ = frames.End(3, "pg_dump: error: connection refused")
	var stdout, stderr bytes.Buffer
	if code := Relay(&stream, &stdout, &stderr); code != 3 || stdout.String() != "partial" || stderr.String() != "pg_dump: error: connection refused\n" {
		t.Errorf("relay: %d %q %q", code, stdout.String(), stderr.String())
	}
	// a code no process can end with is still a failure
	stream.Reset()
	_ = frames.End(256, "")
	if code := Relay(&stream, &stdout, &stderr); code != 1 {
		t.Errorf("code 256: %d", code)
	}
	stream.Reset()
	_ = frames.End(-1, "")
	if code := Relay(&stream, &stdout, &stderr); code != 1 {
		t.Errorf("code -1: %d", code)
	}
}

// TestADumpThatBreaksOffFails (#29): a stream that ends before its exit frame,
// even in the middle of a frame, fails, as does one that is not frames.
func TestADumpThatBreaksOffFails(t *testing.T) {
	var stream bytes.Buffer
	frames := NewFrameWriter(&stream)
	_, _ = frames.Write([]byte("some of the dump"))
	whole := stream.Bytes()
	for _, cut := range [][]byte{nil, whole, whole[:3], whole[:10]} {
		var stdout, stderr bytes.Buffer
		if code := Relay(bytes.NewReader(cut), &stdout, &stderr); code != 1 || stderr.String() != "the dump ended before it said how it went\n" {
			t.Errorf("%q: %d %q", cut, code, stderr.String())
		}
		if _, err := ReadFrames(bytes.NewReader(cut), &stdout); !errors.Is(err, ErrDumpCut) {
			t.Errorf("%q: %v", cut, err)
		}
	}
	for _, broken := range []string{"z\x00\x00\x00\x00", "d\xff\xff\xff\xff", "x\x00\x00\x00\x02ab"} {
		var stdout, stderr bytes.Buffer
		if code := Relay(strings.NewReader(broken), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "broken") {
			t.Errorf("%q: %d %q", broken, code, stderr.String())
		}
	}
	// a relay whose socket is not there
	var stderr bytes.Buffer
	if code := RelaySocket(t.TempDir()+"/none.sock", &bytes.Buffer{}, &stderr); code != 1 || !strings.Contains(stderr.String(), "cannot reach the backup tool") {
		t.Errorf("no socket: %d %q", code, stderr.String())
	}
}
