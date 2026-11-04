package fakedocker

import (
	"encoding/binary"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// logBuffer holds what a fake container has written, and wakes up its followers.
type logBuffer struct {
	mu        sync.Mutex
	lines     []logLine
	followers map[chan struct{}]struct{}
}

type logLine struct {
	stream byte
	at     time.Time
	text   string
}

func (b *logBuffer) add(stream byte, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.lines = append(b.lines, logLine{stream: stream, at: time.Now().UTC(), text: text})
	for follower := range b.followers {
		select {
		case follower <- struct{}{}:
		default:
		}
	}
}

func (b *logBuffer) from(index int) []logLine {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]logLine{}, b.lines[min(index, len(b.lines)):]...)
}

func (b *logBuffer) follow() (<-chan struct{}, func()) {
	wake := make(chan struct{}, 1)

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.followers == nil {
		b.followers = make(map[chan struct{}]struct{})
	}
	b.followers[wake] = struct{}{}

	return wake, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.followers, wake)
	}
}

// WriteLog adds a line to the log of a container, on stdout or stderr.
func (e *Engine) WriteLog(name, stream, text string) {
	number := byte(1)
	if stream == "stderr" {
		number = 2
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	for _, container := range e.containers {
		if container.Name == name {
			if container.logs == nil {
				container.logs = &logBuffer{}
			}
			container.logs.add(number, text)
		}
	}
}

// LogFollowers returns how many log streams of a container are open right now.
func (e *Engine) LogFollowers(name string) int {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, container := range e.containers {
		if container.Name == name && container.logs != nil {
			container.logs.mu.Lock()
			defer container.logs.mu.Unlock()
			return len(container.logs.followers)
		}
	}
	return 0
}

// streamLogs answers like the Engine API: Docker's multiplexed format, with the
// container's timestamps, following until the client leaves.
func (e *Engine) streamLogs(w http.ResponseWriter, r *http.Request, container *Container) {
	e.mu.Lock()
	if container.logs == nil {
		container.logs = &logBuffer{}
	}
	buffer := container.logs
	e.mu.Unlock()

	query := r.URL.Query()
	follow := query.Get("follow") == "true"
	timestamps := query.Get("timestamps") == "true"

	wake, stop := buffer.follow()
	defer stop()

	next := 0
	if tail, err := strconv.Atoi(query.Get("tail")); err == nil {
		next = max(len(buffer.from(0))-tail, 0)
	}

	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	for {
		lines := buffer.from(next)
		next += len(lines)

		for _, line := range lines {
			if err := writeFrame(w, line, timestamps); err != nil {
				return
			}
		}
		if flusher != nil && len(lines) > 0 {
			flusher.Flush()
		}

		if !follow {
			return
		}

		select {
		case <-r.Context().Done():
			return
		case <-wake:
		}
	}
}

func writeFrame(w http.ResponseWriter, line logLine, timestamps bool) error {
	text := line.text + "\n"
	if timestamps {
		text = fmt.Sprintf("%s %s", line.at.Format(time.RFC3339Nano), text)
	}

	header := make([]byte, 8)
	header[0] = line.stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(text))) //nolint:gosec // a test line

	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write([]byte(text))

	return err
}
