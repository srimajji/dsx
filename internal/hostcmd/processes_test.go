package hostcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/guestproto"
	"github.com/srimajji/dsx/internal/runtime"
)

func TestProcessStatusTextIsSortedAndSanitized(t *testing.T) {
	exitCode := 7
	service := &lifecycleStub{statusResult: app.ProcessStatusResult{
		Generation: 4,
		Processes: []guestproto.ProcessStatus{
			{ID: "web", State: guestproto.StateRunning, Ready: true, Required: true},
			{ID: "api", State: guestproto.StateFailed, Required: true, Failure: "bad\x1b[2J\nvalue", Exit: &guestproto.ExitStatus{Code: &exitCode}},
		},
	}}
	dispatcher := NewDispatcher(Dependencies{Lifecycle: service})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), []string{"status", "--root", "/project"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	if len(service.statusRequests) != 1 || service.statusRequests[0].Root != "/project" {
		t.Fatalf("requests=%#v", service.statusRequests)
	}
	output := stdout.String()
	if strings.Index(output, "api state=") > strings.Index(output, "web state=") || strings.Contains(output, "\x1b") || !strings.Contains(output, `exit_code=7 failure=bad\x1b[2J\nvalue`) {
		t.Fatalf("unsafe or unsorted output %q", output)
	}
}

func TestProcessStatusJSONAndLogsText(t *testing.T) {
	service := &lifecycleStub{
		statusResult: app.ProcessStatusResult{Generation: 2, Processes: []guestproto.ProcessStatus{{ID: "web", State: guestproto.StateRunning}}},
		logsResult:   app.ProcessLogsResult{Target: "web", Log: "line\x1b[31m\n", DroppedBytes: 8},
	}
	dispatcher := NewDispatcher(Dependencies{Lifecycle: service})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), []string{"status", "--format", "json"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("status exit=%d stderr=%q", exit, stderr.String())
	}
	var decoded app.ProcessStatusResult
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil || decoded.Generation != 2 {
		t.Fatalf("status JSON=%q err=%v", stdout.String(), err)
	}
	stdout.Reset()
	stderr.Reset()
	if exit := dispatcher.Execute(context.Background(), []string{"logs", "--root", "/project", "web"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("logs exit=%d stderr=%q", exit, stderr.String())
	}
	if len(service.logsRequests) != 1 || service.logsRequests[0] != (app.ProcessLogsRequest{Root: "/project", Target: "web"}) {
		t.Fatalf("logs requests=%#v", service.logsRequests)
	}
	if strings.Contains(stdout.String(), "\x1b") || !strings.Contains(stdout.String(), `\x1b[31m`) {
		t.Fatalf("unsafe logs output %q", stdout.String())
	}
}

func TestShellRendersReadyProcessesAndURLsBeforeExecution(t *testing.T) {
	exitCode := 0
	service := &lifecycleStub{
		shellResult: app.ShellResult{Exit: runtime.Exit{Code: &exitCode}},
		shellReady: app.ShellReady{
			URLs:      []string{"http://127.0.0.1:8080"},
			Processes: []guestproto.ProcessStatus{{ID: "web", State: guestproto.StateReady, Ready: true, Required: true}},
		},
	}
	dispatcher := NewDispatcher(Dependencies{Lifecycle: service, Stdin: strings.NewReader("")})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), []string{"shell", "--", "/bin/true"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "URL http://127.0.0.1:8080\n") || !strings.Contains(stdout.String(), "web state=ready ready=true required=true\n") {
		t.Fatalf("shell readiness output = %q", stdout.String())
	}
}
