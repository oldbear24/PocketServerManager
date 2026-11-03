package dockerapi

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

// NetworkOptions describes a new network.
type NetworkOptions struct {
	// Internal cuts the network off from everything outside it.
	Internal bool
	Labels   map[string]string
}

// CreateNetwork creates a bridge network and returns its id.
func (c *Client) CreateNetwork(ctx context.Context, name string, options NetworkOptions) (string, error) {
	var created struct {
		ID string `json:"Id"`
	}
	err := c.do(ctx, http.MethodPost, "/networks/create", nil, map[string]any{
		"Name":           name,
		"Driver":         "bridge",
		"Internal":       options.Internal,
		"Labels":         options.Labels,
		"CheckDuplicate": true,
	}, &created)
	return created.ID, err
}

// DisconnectNetwork detaches a container from a network.
func (c *Client) DisconnectNetwork(ctx context.Context, network, container string, force bool) error {
	return c.do(ctx, http.MethodPost, "/networks/"+network+"/disconnect", nil, map[string]any{
		"Container": container,
		"Force":     force,
	}, nil)
}

// RemoveNetwork removes a network that no container uses any more.
func (c *Client) RemoveNetwork(ctx context.Context, network string) error {
	return c.do(ctx, http.MethodDelete, "/networks/"+network, nil, nil, nil)
}

// CopyFile writes one file into a container, which may be created but not yet
// started, through the archive endpoint.
func (c *Client) CopyFile(ctx context.Context, container, dir, name string, content []byte, mode int64) error {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(content)), ModTime: time.Now()}); err != nil {
		return err
	}
	if _, err := writer.Write(content); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	query := url.Values{"path": {dir}}
	res, err := c.requestRaw(ctx, http.MethodPut, "/containers/"+container+"/archive", query, &archive, "application/x-tar")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)

	return nil
}

// ExecResult is what a process without a terminal did.
type ExecResult struct {
	ExitCode int
	// Stderr is what the process wrote to its error output, up to 64 KiB.
	Stderr string
}

const maxExecStderr = 64 << 10

// Run runs a process inside a running container without a terminal. Its output
// goes to stdout while it runs; the result says how it ended.
func (c *Client) Run(ctx context.Context, container string, options ExecOptions, stdout io.Writer) (ExecResult, error) {
	options.TTY = false
	options.Stdin = false

	id, err := c.ExecCreate(ctx, container, options)
	if err != nil {
		return ExecResult{}, err
	}

	connection, err := c.ExecStart(ctx, id, false)
	if err != nil {
		return ExecResult{}, err
	}

	stderr := &limitedBuffer{limit: maxExecStderr}
	var writeErr error
	readErr := ReadMultiplexed(connection, func(stream byte, data []byte) {
		if writeErr != nil {
			return
		}
		if stream == 2 {
			stderr.keep(data)
			return
		}
		if stdout != nil {
			_, writeErr = stdout.Write(data)
		}
	})
	connection.Close()

	if writeErr != nil {
		return ExecResult{}, writeErr
	}
	if readErr != nil {
		return ExecResult{}, readErr
	}

	// the engine may report the process as running for a moment after its output ended
	for attempt := 0; ; attempt++ {
		running, code, err := c.ExecInspect(ctx, id)
		if err != nil {
			return ExecResult{}, err
		}
		if !running {
			return ExecResult{ExitCode: code, Stderr: stderr.String()}, nil
		}
		if attempt == 50 {
			return ExecResult{}, errors.New("the process did not end after its output did")
		}
		select {
		case <-ctx.Done():
			return ExecResult{}, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// limitedBuffer keeps the first bytes written to it.
type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) keep(data []byte) {
	if room := b.limit - b.Len(); room > 0 {
		b.Write(data[:min(len(data), room)])
	}
}
