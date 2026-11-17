package fakedocker

import (
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// attachOutput answers like the Engine API for a client that reads what a
// container prints (#30): standard output and error, in the multiplexed
// format, as the process writes them, until it ends. A write of the process
// waits for the client to take it, as the Engine stops reading the process's
// pipe while the connection is full. A client that leaves gets no more.
func attachOutput(w http.ResponseWriter, r *http.Request, out *output) {
	if r.URL.Query().Get("stream") != "1" || r.URL.Query().Get("stdin") == "1" {
		writeError(w, http.StatusBadRequest, "the fake engine attaches a stream of standard input, or of the output, alone")
		return
	}
	if out == nil {
		writeError(w, http.StatusConflict, "the fake engine attaches the output of a container it created")
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		writeError(w, http.StatusInternalServerError, "this connection cannot be taken over")
		return
	}
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		return
	}
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	client := &outputClient{w: conn, gone: make(chan struct{})}
	// the client is attached as the upgrade is answered, under the output's
	// lock: a client that starts the container once it has the answer gets
	// all it prints, and no frame goes out before the answer
	attached, err := out.attachAnswering(client, func() error {
		if _, err := fmt.Fprint(buffered, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"); err != nil {
			return err
		}
		return buffered.Flush()
	})
	if err != nil || !attached {
		// the connection failed, or the process has ended already
		return
	}
	defer out.detach(client)
	left := make(chan struct{})
	go func() {
		// the client sends nothing: a read that ends is the client leaving
		_, _ = io.Copy(io.Discard, buffered.Reader)
		close(left)
	}()
	select {
	case <-out.ended:
	case <-left:
	case <-client.gone:
	}
}

// output passes what a job prints to the clients attached to its output.
type output struct {
	mu      sync.Mutex
	clients []*outputClient
	ended   chan struct{}
	done    bool
}

// outputClient is one attached client's connection.
type outputClient struct {
	w    io.Writer
	gone chan struct{}
}

func newOutput() *output { return &output{ended: make(chan struct{})} }

// attachAnswering adds a client once answer, which answers it, succeeded,
// unless the output has ended; nothing is written to the clients meanwhile.
func (o *output) attachAnswering(client *outputClient, answer func() error) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return false, nil
	}
	if err := answer(); err != nil {
		return false, err
	}
	o.clients = append(o.clients, client)
	return true, nil
}

func (o *output) detach(client *outputClient) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i, other := range o.clients {
		if other == client {
			o.clients = append(o.clients[:i], o.clients[i+1:]...)
			return
		}
	}
}

// write sends data of a stream, 1 for stdout and 2 for stderr, to every
// client as a frame, and returns once they took it; a client whose
// connection fails is gone.
func (o *output) write(stream byte, data []byte) {
	if o == nil || len(data) == 0 {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	frame := make([]byte, 8, 8+len(data))
	frame[0] = stream
	binary.BigEndian.PutUint32(frame[4:], uint32(len(data))) //nolint:gosec // a test's output
	frame = append(frame, data...)
	kept := o.clients[:0]
	for _, client := range o.clients {
		if _, err := client.w.Write(frame); err != nil {
			close(client.gone)
			continue
		}
		kept = append(kept, client)
	}
	o.clients = kept
}

// end closes the output: the process ended, or the container went.
func (o *output) end() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.done {
		o.done = true
		close(o.ended)
	}
}

// jobOutput is where a job writes one of its streams: its log, line by line,
// unless the job keeps none, and its attached clients as it comes.
type jobOutput struct {
	log    *liveLog
	out    *output
	stream byte
}

func (j *jobOutput) Write(data []byte) (int, error) {
	if j.log != nil {
		if _, err := j.log.Write(data); err != nil {
			return 0, err
		}
	}
	j.out.write(j.stream, data)
	return len(data), nil
}

// logDriver is the log driver a container was created with: json-file when
// it names none.
func logDriver(host map[string]any) string {
	config, _ := host["LogConfig"].(map[string]any)
	if driver, _ := config["Type"].(string); driver != "" {
		return driver
	}
	return "json-file"
}
