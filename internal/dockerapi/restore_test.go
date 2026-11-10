package dockerapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
)

// TestContainersDecodeMounts (#30): the container list says what each
// container mounts, as the Engine reports it, so that a restore finds the
// users of a volume or a directory without inspecting every container.
func TestContainersDecodeMounts(t *testing.T) {
	// an Engine's own answer, cut to one container
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"Id":"c1","Names":["/vaultwarden"],"State":"running","Labels":{},
			"Mounts":[{"Type":"volume","Name":"vw_data","Source":"/var/lib/docker/volumes/vw_data/_data","Destination":"/data","Driver":"local","Mode":"z","RW":true,"Propagation":""},
			{"Type":"bind","Source":"/srv/vw","Destination":"/etc/vw","Mode":"ro","RW":false,"Propagation":"rprivate"}]}]`))
	}))
	t.Cleanup(server.Close)
	docker, err := dockerapi.New("tcp://" + strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	list, err := docker.Containers(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("%v %v", list, err)
	}
	want := []dockerapi.Mount{
		{Type: "volume", Name: "vw_data", Source: "/var/lib/docker/volumes/vw_data/_data", Destination: "/data", RW: true},
		{Type: "bind", Source: "/srv/vw", Destination: "/etc/vw"},
	}
	if len(list[0].Mounts) != 2 || list[0].Mounts[0] != want[0] || list[0].Mounts[1] != want[1] {
		t.Errorf("mounts: %+v", list[0].Mounts)
	}

	// the fake reports what a container's HostConfig mounts, as the Engine
	fake, engine := client(t)
	engine.AddContainer("app", "app:1", nil)
	engine.SetHostConfig("app", map[string]any{
		"Mounts": []any{map[string]any{"Type": "volume", "Source": "app_data", "Target": "/data"}},
		"Binds":  []any{"/srv/app:/app:ro", "app_cache:/cache"},
	})
	list, err = fake.Containers(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("%v %v", list, err)
	}
	got := list[0].Mounts
	if len(got) != 3 ||
		got[0] != (dockerapi.Mount{Type: "volume", Name: "app_data", Source: engine.VolumeDir("app_data"), Destination: "/data", RW: true}) ||
		got[1] != (dockerapi.Mount{Type: "bind", Source: "/srv/app", Destination: "/app"}) ||
		got[2] != (dockerapi.Mount{Type: "volume", Name: "app_cache", Source: engine.VolumeDir("app_cache"), Destination: "/cache", RW: true}) {
		t.Errorf("the fake's mounts: %+v", got)
	}
}

// TestUpdateRestartPolicyKeepsMaxRetry (#30): a policy put back after a
// restore keeps its retries, which SetRestartPolicy would drop.
func TestUpdateRestartPolicyKeepsMaxRetry(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Method == http.MethodPost && r.URL.Path == "/containers/c1/update" {
			bodies = append(bodies, body)
		}
		_, _ = w.Write([]byte(`{"Warnings":[]}`))
	}))
	t.Cleanup(server.Close)
	docker, err := dockerapi.New("tcp://" + strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if err := docker.UpdateRestartPolicy(context.Background(), "c1", "on-failure", 5); err != nil {
		t.Fatal(err)
	}
	if err := docker.UpdateRestartPolicy(context.Background(), "c1", "no", 0); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("bodies: %v", bodies)
	}
	first, _ := bodies[0]["RestartPolicy"].(map[string]any)
	second, _ := bodies[1]["RestartPolicy"].(map[string]any)
	if first["Name"] != "on-failure" || first["MaximumRetryCount"] != float64(5) || second["Name"] != "no" || second["MaximumRetryCount"] != float64(0) {
		t.Errorf("bodies: %v", bodies)
	}

	// the fake keeps it, as inspect shows
	fake, engine := client(t)
	id := engine.AddContainer("app", "app:1", nil)
	if err := fake.UpdateRestartPolicy(context.Background(), id, "on-failure", 3); err != nil {
		t.Fatal(err)
	}
	info, err := fake.Inspect(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := info.HostConfig["RestartPolicy"].(map[string]any)
	if policy["Name"] != "on-failure" || policy["MaximumRetryCount"] != float64(3) {
		t.Errorf("the policy: %v", policy)
	}
}

// TestAttachOutputDemultiplexes (#30): a job whose log driver is none is
// read through its attached output, byte for byte as it writes it, binary
// data and lines without an end alike; the process waits while the reader
// does not read, and its log cannot be read.
func TestAttachOutputDemultiplexes(t *testing.T) {
	docker, engine := client(t)
	ctx := context.Background()
	engine.AddImage("agent", nil)
	const chunk = 1 << 20
	const chunks = 64
	var written atomic.Int64
	engine.OnJob = func(job fakedocker.Job) fakedocker.JobResult {
		if job.Cmd[0] == "slow" {
			data := bytes.Repeat([]byte{0xff}, chunk)
			for range chunks {
				if _, err := job.Stdout.Write(data); err != nil {
					return fakedocker.JobResult{Code: 1}
				}
				written.Add(chunk)
			}
			return fakedocker.JobResult{}
		}
		_, _ = job.Stdout.Write([]byte{0, 1, 2, '\n', 0xfe})
		_, _ = io.WriteString(job.Stderr, "restic says\n")
		_, _ = job.Stdout.Write([]byte("no end"))
		return fakedocker.JobResult{Stderr: "error: the last words\n"}
	}
	job := func(cmd string) string {
		t.Helper()
		id, err := docker.Create(ctx, "", map[string]any{
			"Image": "agent", "Cmd": []string{cmd}, "Labels": map[string]string{"pocketcloud.role": "job"},
			"HostConfig": map[string]any{"LogConfig": map[string]any{"Type": "none"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	id := job("talk")
	out, err := docker.AttachOutput(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !engine.Called("POST /containers/" + id + "/attach?stderr=1&stdout=1&stream=1") {
		t.Errorf("calls: %v", engine.Calls())
	}
	if err := docker.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := dockerapi.ReadMultiplexed(out, func(stream byte, data []byte) {
		if stream == 2 {
			stderr.Write(data)
			return
		}
		stdout.Write(data)
	}); err != nil {
		t.Fatal(err)
	}
	out.Close()
	if !bytes.Equal(stdout.Bytes(), append([]byte{0, 1, 2, '\n', 0xfe}, "no end"...)) || stderr.String() != "restic says\nerror: the last words\n" {
		t.Errorf("stdout %q, stderr %q", stdout.Bytes(), stderr.String())
	}
	if code, err := docker.Wait(ctx, id); err != nil || code != 0 {
		t.Errorf("the job: %d %v", code, err)
	}
	if _, err := docker.Logs(ctx, id, dockerapi.LogOptions{Tail: -1}); err == nil || !strings.Contains(err.Error(), "does not support reading") {
		t.Errorf("the log of a job that keeps none: %v", err)
	}

	// a reader that does not read holds the process up
	id = job("slow")
	out, err = docker.AttachOutput(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err := docker.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if held := written.Load(); held >= chunk*chunks {
		t.Errorf("the job wrote all of %d bytes while nobody read", held)
	}
	read := int64(0)
	if err := dockerapi.ReadMultiplexed(out, func(_ byte, data []byte) { read += int64(len(data)) }); err != nil {
		t.Fatal(err)
	}
	if read != chunk*chunks {
		t.Errorf("read %d bytes", read)
	}
}
