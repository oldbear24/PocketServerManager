package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/version"
)

const (
	defaultBundleLogLines = 1000
	uploadChunkSize       = 1 << 20
	reportTimeoutTotal    = 10 * time.Second
)

// Report is the agent's account of its own state. It holds no secrets: neither
// the token nor keys are ever part of it.
type Report struct {
	GeneratedAt  time.Time        `json:"generated_at"`
	Version      string           `json:"version"`
	Commit       string           `json:"commit,omitempty"`
	GoVersion    string           `json:"go_version"`
	Platform     string           `json:"platform"`
	Host         string           `json:"host,omitempty"`
	HostID       string           `json:"host_id,omitempty"`
	Roles        []string         `json:"roles,omitempty"`
	Panel        string           `json:"panel,omitempty"`
	Connectivity string           `json:"connectivity"`
	Certificate  *CertificateInfo `json:"certificate,omitempty"`
	Docker       string           `json:"docker"`
	SelfChecks   []SelfCheckInfo  `json:"self_checks"`
	Commands     []commandEntry   `json:"recent_commands"`
	DockerInfo   any              `json:"docker_info,omitempty"`
	LogLevel     string           `json:"log_level"`
}

// CertificateInfo describes the client certificate.
type CertificateInfo struct {
	Serial    string    `json:"serial"`
	NotAfter  time.Time `json:"not_after"`
	ExpiresIn string    `json:"expires_in"`
}

// SelfCheckInfo is one self-check result.
type SelfCheckInfo struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	Detail  string `json:"detail"`
}

// BuildReport gathers the report. It works without a panel connection.
func (a *Agent) BuildReport(ctx context.Context) Report {
	report := Report{
		GeneratedAt:  time.Now().UTC(),
		Version:      version.Version,
		Commit:       version.Commit,
		GoVersion:    runtime.Version(),
		Platform:     runtime.GOOS + "/" + runtime.GOARCH,
		Connectivity: "not enrolled",
		Docker:       "not checked",
		LogLevel:     strings.ToLower(a.level.Level().String()),
		Commands:     a.store.recent(50),
	}

	identity := a.Identity
	if identity == nil {
		if loaded, err := LoadIdentity(a.Config.StateDir); err == nil {
			identity = loaded
			a.Identity = loaded
		}
	}

	if identity != nil {
		report.Host = identity.Name
		report.HostID = identity.HostID
		report.Roles = identity.Roles
		report.Panel = identity.Panel
		report.Connectivity = a.testConnectivity(ctx)

		if leaf := identity.Certificate(); leaf != nil {
			report.Certificate = &CertificateInfo{
				Serial:    fmt.Sprintf("%x", leaf.SerialNumber),
				NotAfter:  leaf.NotAfter,
				ExpiresIn: time.Until(leaf.NotAfter).Round(time.Hour).String(),
			}
		}
	}

	if info, err := a.Docker.Info(ctx); err != nil {
		report.Docker = "not available: " + err.Error()
	} else {
		report.Docker = "Docker " + info.ServerVersion
		report.DockerInfo = info
	}

	a.checks.run(ctx)
	for _, check := range a.checks.results() {
		report.SelfChecks = append(report.SelfChecks, SelfCheckInfo{
			Name:    check.GetName(),
			OK:      check.GetOk(),
			Skipped: check.GetSkipped(),
			Detail:  check.GetDetail(),
		})
	}

	return report
}

// testConnectivity makes a real round trip to the panel with the stored identity.
func (a *Agent) testConnectivity(parent context.Context) string {
	ctx, cancel := context.WithTimeout(parent, reportTimeoutTotal)
	defer cancel()

	conn, err := a.dial()
	if err != nil {
		return "cannot connect: " + err.Error()
	}
	defer conn.Close()

	start := time.Now()
	_, err = agentv1.NewAgentServiceClient(conn).Heartbeat(ctx, &agentv1.HeartbeatRequest{})
	if err != nil {
		code := status.Code(err)
		if code == codes.Unauthenticated || code == codes.PermissionDenied {
			return "the panel refuses this agent: " + status.Convert(err).Message()
		}
		return "cannot reach the panel: " + err.Error()
	}

	return "ok, round trip " + time.Since(start).Round(time.Millisecond).String()
}

// WriteReportText prints a report for `pocketcloud-agent diagnostics`.
func WriteReportText(out io.Writer, report Report) {
	fmt.Fprintf(out, "PocketCloud agent %s (%s, %s)\n\n", report.Version, report.Platform, report.GoVersion)
	fmt.Fprintf(out, "Host:          %s\n", valueOr(report.Host, "not enrolled"))
	fmt.Fprintf(out, "Roles:         %s\n", valueOr(strings.Join(report.Roles, ", "), "-"))
	fmt.Fprintf(out, "Panel:         %s\n", valueOr(report.Panel, "-"))
	fmt.Fprintf(out, "Connectivity:  %s\n", report.Connectivity)
	if report.Certificate != nil {
		fmt.Fprintf(out, "Certificate:   expires %s (in %s), serial %s\n", report.Certificate.NotAfter.Format(time.DateOnly), report.Certificate.ExpiresIn, report.Certificate.Serial)
	} else {
		fmt.Fprintf(out, "Certificate:   none\n")
	}
	fmt.Fprintf(out, "Docker:        %s\n", report.Docker)
	fmt.Fprintf(out, "Log level:     %s\n", report.LogLevel)

	fmt.Fprintf(out, "\nSelf-checks:\n")
	for _, check := range report.SelfChecks {
		state := "FAIL"
		switch {
		case check.Skipped:
			state = "skip"
		case check.OK:
			state = " ok "
		}
		fmt.Fprintf(out, "  [%s] %-11s %s\n", state, check.Name, check.Detail)
	}

	fmt.Fprintf(out, "\nRecent commands: %d\n", len(report.Commands))
	for _, command := range report.Commands[:min(10, len(report.Commands))] {
		fmt.Fprintf(out, "  %s  %-26s %-10s %s\n", command.StartedAt.Format(time.DateTime), command.Type, command.State, command.Error)
	}
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// buildBundle creates the zip of a debug bundle.
func (a *Agent) buildBundle(ctx context.Context, logLines int) ([]byte, error) {
	if logLines <= 0 {
		logLines = defaultBundleLogLines
	}

	report := a.BuildReport(ctx)

	reportJSON, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, err
	}

	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)

	var text bytes.Buffer
	WriteReportText(&text, report)

	for _, file := range []struct {
		name string
		data []byte
	}{
		{"report.txt", text.Bytes()},
		{"report.json", reportJSON},
		{"agent.log", []byte(strings.Join(a.ring.last(logLines), ""))},
		{"environment.txt", []byte(strings.Join(safeEnvironment(), "\n") + "\n")},
	} {
		writer, err := archive.Create(file.name)
		if err != nil {
			return nil, err
		}
		if _, err := writer.Write(file.data); err != nil {
			return nil, err
		}
	}

	if err := archive.Close(); err != nil {
		return nil, err
	}

	return buffer.Bytes(), nil
}

// safeEnvironment lists the environment of the agent. Only known variables are
// included, so that a value someone added to the container cannot leak.
func safeEnvironment() []string {
	allowed := []string{"PANEL", "NAME", "ROLES", "STATE_DIR", "HOST_ROOT", "DOCKER_HOST", "AGENT_CONTAINER", "HOSTNAME", "TZ"}

	result := make([]string, 0, len(allowed))
	for _, name := range allowed {
		if value, ok := os.LookupEnv(name); ok {
			result = append(result, name+"="+value)
		}
	}
	slices.Sort(result)

	return result
}

func (a *Agent) handleDiagnostics(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	reporter.Progress("collect", "Collecting diagnostics", 20)

	bundle, err := a.buildBundle(ctx, int(command.GetAgentDiagnostics().GetLogLines()))
	if err != nil {
		return nil, fmt.Errorf("failed to build the bundle: %w", err)
	}

	reporter.Progress("upload", "Uploading the bundle", 60)

	host := "agent"
	if a.Identity != nil {
		host = a.Identity.Name
	}

	name := "pocketcloud-" + host + "-" + time.Now().UTC().Format("20060102-150405") + ".zip"

	id, err := a.uploadArtifact(ctx, command.GetId(), "diagnostics", name, bundle)
	if err != nil {
		return nil, err
	}

	return map[string]any{"bundle": id, "size_bytes": float64(len(bundle))}, nil
}

// uploadArtifact sends a file that belongs to a command to the panel.
func (a *Agent) uploadArtifact(ctx context.Context, commandID, kind, name string, content []byte) (string, error) {
	client := a.client.Load()
	if client == nil {
		return "", errors.New("not connected")
	}

	stream, err := (*client).UploadArtifact(ctx)
	if err != nil {
		return "", err
	}

	err = stream.Send(&agentv1.UploadArtifactRequest{Part: &agentv1.UploadArtifactRequest_Header{Header: &agentv1.UploadArtifactHeader{
		CommandId: commandID,
		Kind:      kind,
		FileName:  name,
		SizeBytes: uint64(len(content)),
	}}})
	if err != nil {
		return "", err
	}

	for offset := 0; offset < len(content); offset += uploadChunkSize {
		end := min(offset+uploadChunkSize, len(content))
		err := stream.Send(&agentv1.UploadArtifactRequest{Part: &agentv1.UploadArtifactRequest_Chunk{Chunk: content[offset:end]}})
		if err != nil {
			return "", err
		}
	}

	response, err := stream.CloseAndRecv()
	if err != nil {
		return "", err
	}

	return response.GetArtifactId(), nil
}
