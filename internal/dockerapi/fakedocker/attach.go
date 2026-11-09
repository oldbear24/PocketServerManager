package fakedocker

import (
	"fmt"
	"io"
	"net/http"
)

// attach answers like the Engine API for a client that writes a container's
// standard input: it upgrades the connection and passes what the client sends
// to the container, until the client leaves, which ends the input of a
// container created with StdinOnce. The fake attaches standard input only.
func attach(w http.ResponseWriter, r *http.Request, stdin *io.PipeWriter, once bool) {
	query := r.URL.Query()
	if query.Get("stream") != "1" || query.Get("stdin") != "1" || query.Get("stdout") == "1" || query.Get("stderr") == "1" {
		writeError(w, http.StatusBadRequest, "the fake engine attaches a stream of standard input only")
		return
	}
	if stdin == nil {
		writeError(w, http.StatusConflict, "the container was not created with OpenStdin: it reads no input")
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
	if _, err := fmt.Fprint(buffered, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}
	// a container that exited takes no more: the client's writes fail
	if _, err := io.Copy(stdin, buffered.Reader); err == nil && once {
		_ = stdin.Close()
	}
}

// closeStdin ends the standard input of a container that goes.
func closeStdin(c *Container) {
	if c.stdinR != nil {
		_ = c.stdinR.Close()
		_ = c.stdinW.Close()
	}
}
