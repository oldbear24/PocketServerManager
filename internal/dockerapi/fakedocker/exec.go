package fakedocker

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// ExecCall is a process started inside a fake container.
type ExecCall struct {
	ID        string
	Container string
	Cmd       []string
	Env       []string
	User      string
	// WorkingDir is where the process was asked to start.
	WorkingDir string
	TTY        bool
	Stdin      bool

	mu      sync.Mutex
	running bool
	code    int
	cols    int
	rows    int
}

// Size returns the terminal size the process was last given.
func (c *ExecCall) Size() (cols, rows int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cols, c.rows
}

// Running tells whether the process is still running.
func (c *ExecCall) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

// ExecResult is what a process without a terminal did.
type ExecResult struct {
	Stdout string
	Stderr string
	Code   int
}

// Execs returns the processes started so far, oldest first.
func (e *Engine) Execs() []*ExecCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*ExecCall{}, e.execList...)
}

// LastExec returns the newest process started in a container with a name, or nil.
func (e *Engine) LastExec(name string) *ExecCall {
	e.mu.Lock()
	defer e.mu.Unlock()

	var id string
	for _, container := range e.containers {
		if container.Name == name {
			id = container.ID
		}
	}

	for i := len(e.execList) - 1; i >= 0; i-- {
		if e.execList[i].Container == id {
			return e.execList[i]
		}
	}
	return nil
}

func (e *Engine) createExec(w http.ResponseWriter, r *http.Request, container *Container) {
	var body struct {
		Cmd         []string `json:"Cmd"`
		Env         []string `json:"Env"`
		User        string   `json:"User"`
		WorkingDir  string   `json:"WorkingDir"`
		Tty         bool     `json:"Tty"`
		AttachStdin bool     `json:"AttachStdin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if container.Status != "running" {
		writeError(w, http.StatusConflict, "container "+container.ID+" is not running")
		return
	}

	call := &ExecCall{
		ID:         newID(),
		Container:  container.ID,
		Cmd:        body.Cmd,
		Env:        body.Env,
		User:       body.User,
		WorkingDir: body.WorkingDir,
		TTY:        body.Tty,
		Stdin:      body.AttachStdin,
		running:    true,
	}

	e.execs[call.ID] = call
	e.execList = append(e.execList, call)

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]string{"Id": call.ID})
}

func (e *Engine) execRoute(w http.ResponseWriter, r *http.Request, rest string) {
	id, action, _ := strings.Cut(rest, "/")

	e.mu.Lock()
	call := e.execs[id]
	handler := e.OnExec
	e.mu.Unlock()

	if call == nil {
		writeError(w, http.StatusNotFound, "no such exec instance: "+id)
		return
	}

	switch action {
	case "start":
		e.startExec(w, r, call, handler)

	case "resize":
		cols, _ := strconv.Atoi(r.URL.Query().Get("w"))
		rows, _ := strconv.Atoi(r.URL.Query().Get("h"))
		call.mu.Lock()
		call.cols, call.rows = cols, rows
		call.mu.Unlock()
		w.WriteHeader(http.StatusOK)

	case "json":
		call.mu.Lock()
		defer call.mu.Unlock()
		writeJSON(w, map[string]any{"Running": call.running, "ExitCode": call.code})

	default:
		writeError(w, http.StatusNotFound, "unknown exec action "+action)
	}
}

// startExec answers like the Engine API: it upgrades the connection and hands it to
// the process.
func (e *Engine) startExec(w http.ResponseWriter, r *http.Request, call *ExecCall, handler func(*ExecCall, []byte) ExecResult) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		writeError(w, http.StatusInternalServerError, "this connection cannot be taken over")
		return
	}

	// what follows the request on this connection is the input of the process
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		return
	}

	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	if _, err := fmt.Fprint(buffered, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}

	code := 0
	if call.TTY {
		code = fakeShell(conn, buffered.Reader, call)
	} else {
		code = runProcess(conn, buffered.Reader, call, handler)
	}

	call.mu.Lock()
	call.running = false
	call.code = code
	call.mu.Unlock()
}

// fakeShell is an interactive program behind a fake terminal. It echoes what is
// typed, turns Ctrl+C into ^C and knows two commands: `size` and `exit <code>`.
func fakeShell(conn net.Conn, input *bufio.Reader, call *ExecCall) int {
	write := func(text string) bool {
		_, err := io.WriteString(conn, text)
		return err == nil
	}

	if !write("$ ") {
		return 0
	}

	line := []byte{}
	for {
		char, err := input.ReadByte()
		if err != nil {
			return 0
		}

		switch char {
		case 0x03:
			line = line[:0]
			if !write("^C\r\n$ ") {
				return 0
			}

		case '\r', '\n':
			command := strings.TrimSpace(string(line))
			line = line[:0]

			if !write("\r\n") {
				return 0
			}

			switch {
			case command == "size":
				cols, rows := call.Size()
				if !write(fmt.Sprintf("%dx%d\r\n", cols, rows)) {
					return 0
				}
			case command == "exit":
				return 0
			case strings.HasPrefix(command, "exit "):
				code, _ := strconv.Atoi(strings.TrimPrefix(command, "exit "))
				return code
			case command != "":
				if !write("ran: " + command + "\r\n") {
					return 0
				}
			}

			if !write("$ ") {
				return 0
			}

		default:
			line = append(line, char)
			if !write(string(char)) {
				return 0
			}
		}
	}
}

// runProcess is a program without a terminal: it gets what was sent to its standard
// input, and its output goes out in Docker's multiplexed format.
func runProcess(conn net.Conn, input *bufio.Reader, call *ExecCall, handler func(*ExecCall, []byte) ExecResult) int {
	var stdin []byte
	if call.Stdin {
		stdin, _ = io.ReadAll(input)
	}

	result := ExecResult{}
	if handler != nil {
		result = handler(call, stdin)
	}

	frame := func(stream byte, text string) {
		if text == "" {
			return
		}
		header := make([]byte, 8)
		header[0] = stream
		binary.BigEndian.PutUint32(header[4:], uint32(len(text))) //nolint:gosec // test output
		if _, err := conn.Write(append(header, text...)); err != nil {
			return
		}
	}

	frame(1, result.Stdout)
	frame(2, result.Stderr)

	return result.Code
}
