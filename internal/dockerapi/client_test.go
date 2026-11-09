package dockerapi_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
)

func TestHosts(t *testing.T) {
	for _, good := range []string{"", "unix:///var/run/docker.sock", "tcp://127.0.0.1:2375", "http://docker:2375"} {
		if _, err := dockerapi.New(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	if _, err := dockerapi.New("ftp://nope"); err == nil {
		t.Error("an unknown scheme must be refused")
	}
}

func client(t *testing.T) (*dockerapi.Client, *fakedocker.Engine) {
	t.Helper()

	engine := fakedocker.New(t)

	docker, err := dockerapi.New(engine.Host())
	if err != nil {
		t.Fatal(err)
	}

	return docker, engine
}

func TestErrorsCarryTheMessage(t *testing.T) {
	docker, _ := client(t)

	_, err := docker.Inspect(context.Background(), "missing")
	if err == nil || !strings.Contains(err.Error(), "no such container") {
		t.Fatalf("%v", err)
	}
	if !dockerapi.IsNotFound(err) {
		t.Errorf("%v must count as not found", err)
	}
}

func TestContainerLifecycle(t *testing.T) {
	docker, engine := client(t)
	ctx := context.Background()

	id := engine.AddContainer("web", "nginx:1", map[string]string{"a": "b"})

	list, err := docker.Containers(ctx)
	if err != nil || len(list) != 1 || list[0].ID != id || list[0].Names[0] != "/web" {
		t.Fatalf("%v, %v", list, err)
	}

	if err := docker.Stop(ctx, id, 5); err != nil {
		t.Fatal(err)
	}
	if !engine.Called("/stop?t=5") {
		t.Errorf("the timeout was not sent: %v", engine.Calls())
	}

	info, err := docker.Inspect(ctx, id)
	if err != nil || info.State.Status != "exited" || info.State.Running {
		t.Fatalf("%+v, %v", info, err)
	}

	if err := docker.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := docker.Rename(ctx, id, "web2"); err != nil {
		t.Fatal(err)
	}
	if engine.Find("web2") == nil {
		t.Error("not renamed")
	}

	if err := docker.Remove(ctx, id, true, false); err == nil {
		t.Error("removing a running container without force must fail")
	}
	if err := docker.Remove(ctx, id, true, true); err != nil {
		t.Fatal(err)
	}
	if engine.Find("web2") != nil {
		t.Error("not removed")
	}
}

func TestPullSplitsTheReference(t *testing.T) {
	for _, tc := range []struct {
		image string
		want  string
	}{
		{"nginx", "fromImage=nginx"},
		{"nginx:1.27", "fromImage=nginx&tag=1.27"},
		{"registry:5000/app", "fromImage=registry%3A5000%2Fapp"},
		{"registry:5000/app:v2", "fromImage=registry%3A5000%2Fapp&tag=v2"},
		{"ghcr.io/a/b@sha256:abc", "fromImage=ghcr.io%2Fa%2Fb&tag=sha256%3Aabc"},
	} {
		t.Run(tc.image, func(t *testing.T) {
			docker, engine := client(t)

			if err := docker.Pull(context.Background(), tc.image, nil, nil); err != nil {
				t.Fatal(err)
			}
			if !engine.Called(tc.want) {
				t.Fatalf("%v", engine.Calls())
			}
		})
	}
}

// TestPullFailsOnAnErrorInTheStream: the Engine answers 200 and reports a failure
// later, in the middle of the progress lines.
func TestPullFailsOnAnErrorInTheStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"Pulling"}` + "\n" + `{"error":"manifest unknown"}` + "\n"))
	}))
	t.Cleanup(server.Close)

	docker, err := dockerapi.New("tcp://" + strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	lines := []string{}
	err = docker.Pull(context.Background(), "nginx", nil, func(status string) { lines = append(lines, status) })

	if err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Fatalf("%v", err)
	}
	if len(lines) != 1 || lines[0] != "Pulling" {
		t.Errorf("progress: %v", lines)
	}
}

func TestRegistryAuthIsSentInTheHeader(t *testing.T) {
	var header string
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("X-Registry-Auth")
	}))
	t.Cleanup(server.Close)

	docker, err := dockerapi.New("tcp://" + strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	auth := &dockerapi.PullAuth{Username: "me", Password: "secret", ServerAddress: "ghcr.io"}
	if err := docker.Pull(context.Background(), "ghcr.io/me/app", auth, nil); err != nil {
		t.Fatal(err)
	}

	if header == "" || strings.Contains(header, "secret") {
		t.Errorf("the header must hold the encoded credentials: %q", header)
	}
}

func TestEvents(t *testing.T) {
	docker, engine := client(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan dockerapi.Event, 4)
	go func() {
		_ = docker.Events(ctx, func(event dockerapi.Event) { received <- event })
	}()

	// wait for the stream to be open
	deadline := time.Now().Add(5 * time.Second)
	for !engine.Called("GET /events") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	id := engine.AddContainer("web", "nginx", nil)

	select {
	case event := <-received:
		if event.Type != "container" || event.Action != "start" || event.Actor.ID != id {
			t.Errorf("%+v", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event arrived")
	}
}

func frame(stream byte, text string) []byte {
	header := make([]byte, 8)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(text)))
	return append(header, text...)
}

func TestReadMultiplexed(t *testing.T) {
	var input bytes.Buffer
	input.Write(frame(1, "out line\n"))
	input.Write(frame(2, "err line\n"))
	input.Write(frame(1, ""))

	type got struct {
		stream byte
		text   string
	}
	frames := []got{}

	err := dockerapi.ReadMultiplexed(&input, func(stream byte, data []byte) {
		frames = append(frames, got{stream, string(data)})
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(frames) != 3 || frames[0] != (got{1, "out line\n"}) || frames[1] != (got{2, "err line\n"}) {
		t.Errorf("%+v", frames)
	}

	// a cut stream ends quietly at a frame boundary, and reports a broken frame
	if err := dockerapi.ReadMultiplexed(bytes.NewReader(nil), func(byte, []byte) {}); err != nil {
		t.Errorf("empty: %v", err)
	}
	broken := frame(1, "complete")[:10]
	if err := dockerapi.ReadMultiplexed(bytes.NewReader(broken), func(byte, []byte) {}); err == nil {
		t.Error("a frame that was cut off must be an error")
	}
}

// TestWaitReturnsTheExitCode: a job container's end is how its job went.
func TestWaitReturnsTheExitCode(t *testing.T) {
	var asked string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery
		if strings.Contains(r.URL.Path, "broken") {
			_, _ = w.Write([]byte(`{"StatusCode":125,"Error":{"Message":"the container could not start"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"StatusCode":3}`))
	}))
	t.Cleanup(server.Close)

	docker, err := dockerapi.New("tcp://" + strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	code, err := docker.Wait(context.Background(), "job")
	if err != nil || code != 3 {
		t.Errorf("code %d, %v", code, err)
	}
	if !strings.HasSuffix(asked, "/containers/job/wait?condition=not-running") || !strings.HasPrefix(asked, "POST ") {
		t.Errorf("asked %s", asked)
	}
	if _, err := docker.Wait(context.Background(), "broken"); err == nil || !strings.Contains(err.Error(), "could not start") {
		t.Errorf("an error: %v", err)
	}
}

// TestAttachWritesAJobsInput (#29): what is written to the attached
// connection before and after the job starts is its standard input, which
// ends when the connection closes; once the job has exited, writes fail.
func TestAttachWritesAJobsInput(t *testing.T) {
	docker, engine := client(t)
	ctx := context.Background()
	engine.AddImage("agent", nil)
	engine.OnJob = func(job fakedocker.Job) fakedocker.JobResult {
		if job.Stdin == nil {
			return fakedocker.JobResult{Code: 1, Stderr: "no input"}
		}
		if job.Cmd[0] == "ignore" {
			return fakedocker.JobResult{}
		}
		read, err := io.ReadAll(job.Stdin)
		if err != nil {
			return fakedocker.JobResult{Code: 1, Stderr: err.Error()}
		}
		return fakedocker.JobResult{Stdout: string(read)}
	}
	job := func(cmd string) string {
		t.Helper()
		id, err := docker.Create(ctx, "", map[string]any{
			"Image": "agent", "Cmd": []string{cmd}, "OpenStdin": true, "StdinOnce": true, "AttachStdin": true,
			"Labels": map[string]string{"pocketcloud.role": "job"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	id := job("read")
	stdin, err := docker.Attach(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !engine.Called("POST /containers/" + id + "/attach?stdin=1&stream=1") {
		t.Errorf("calls: %v", engine.Calls())
	}
	if _, err := io.WriteString(stdin, "before the start\n"); err != nil {
		t.Fatal(err)
	}
	if err := docker.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stdin, "after it\n"); err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if code, err := docker.Wait(ctx, id); err != nil || code != 0 {
		t.Fatalf("the job: %d %v", code, err)
	}
	logs, err := docker.Logs(ctx, id, dockerapi.LogOptions{Tail: -1})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_ = dockerapi.ReadMultiplexed(logs, func(_ byte, data []byte) { out.Write(data) })
	logs.Close()
	if out.String() != "before the start\nafter it\n" {
		t.Errorf("the job read %q", out.String())
	}

	// a job that exited reads no more
	id = job("ignore")
	stdin, err = docker.Attach(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := docker.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := docker.Wait(ctx, id); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := stdin.Write(make([]byte, 64<<10)); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("writes to a job that exited went on")
		}
	}

	// a container without input refuses
	plain, err := docker.Create(ctx, "", map[string]any{"Image": "agent", "Labels": map[string]string{"pocketcloud.role": "job"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := docker.Attach(ctx, plain); err == nil {
		t.Error("a container without OpenStdin was attached")
	}
}
