package hostcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/config"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
)

type fakeInspector struct {
	calls  int
	result app.InspectResult
	err    error
}

func (fake *fakeInspector) Inspect(_ context.Context, _ app.InspectRequest) (app.InspectResult, error) {
	fake.calls++
	return fake.result, fake.err
}

type fakeDoctor struct {
	calls  int
	result app.DoctorResult
	err    error
}

func (fake *fakeDoctor) Doctor(_ context.Context, _ app.DoctorRequest) (app.DoctorResult, error) {
	fake.calls++
	return fake.result, fake.err
}

func TestInspectJSONCallsApplicationOnce(t *testing.T) {
	inspector := &fakeInspector{result: app.InspectResult{
		Facts: app.ProjectFacts{CanonicalRoot: "/tmp/project", ConfigExists: true, ConfigPath: ".dsx/config.jsonc"},
		Plan: plan.ExecutionPlan{
			ContractVersion: plan.ContractVersion,
			Agent:           "codex",
			ExecutableHash:  strings.Repeat("a", 64),
			Provenance: config.Provenance{
				"/agent": {Kind: "project", Path: ".dsx/config.jsonc", Line: 3, Column: 5, Priority: plan.PriorityProject},
			},
		},
	}}
	dispatcher := NewDispatcher(Dependencies{Inspector: inspector})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), []string{"inspect", "--format", "json", "--root", "/tmp/project"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if inspector.calls != 1 {
		t.Fatalf("inspect calls = %d", inspector.calls)
	}
	var result app.InspectResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode inspect JSON: %v", err)
	}
	if result.Plan.ExecutableHash != strings.Repeat("a", 64) || result.Plan.Provenance["/agent"].Kind != "project" {
		t.Fatalf("incomplete inspect output: %#v", result.Plan)
	}
}

func TestInspectErrorUsesTypedExit(t *testing.T) {
	inspector := &fakeInspector{
		result: app.InspectResult{Diagnostics: []config.Diagnostic{{Severity: "error", Code: "unsupported", Message: "unsafe field"}}},
		err:    model.NewError(model.CodeInvalidInput, "invalid project", nil),
	}
	dispatcher := NewDispatcher(Dependencies{Inspector: inspector})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), []string{"inspect"}, &stdout, &stderr); exit != 2 {
		t.Fatalf("exit = %d, want 2", exit)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), `code="unsupported"`) || !strings.Contains(stderr.String(), "invalid project") {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func TestDoctorJSONCallsApplicationOnce(t *testing.T) {
	doctor := &fakeDoctor{result: app.DoctorResult{Capabilities: runtime.Capabilities{
		HostOS: "Darwin", HostVersion: "27.0", HostArch: "arm64",
		CLIVersion: "1.2.2", ServerVersion: "1.2.2", CompatibilityID: "apple-container/cli-1.2.2/server-1.2.2", ServiceHealthy: true,
	}}}
	dispatcher := NewDispatcher(Dependencies{Doctor: doctor})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), []string{"doctor", "--format=json"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if doctor.calls != 1 || !strings.Contains(stdout.String(), `"compatibility_id":"apple-container/cli-1.2.2/server-1.2.2"`) {
		t.Fatalf("calls = %d, stdout = %q", doctor.calls, stdout.String())
	}
}

func TestInspectRejectsUnknownFormatWithoutCallingApplication(t *testing.T) {
	inspector := &fakeInspector{}
	dispatcher := NewDispatcher(Dependencies{Inspector: inspector})
	var stdout, stderr bytes.Buffer
	exit := dispatcher.Execute(context.Background(), []string{"inspect", "--format", "yaml"}, &stdout, &stderr)
	if exit != 2 || inspector.calls != 0 {
		t.Fatalf("exit = %d, calls = %d", exit, inspector.calls)
	}
}

func TestDoctorPropagatesUnavailable(t *testing.T) {
	doctor := &fakeDoctor{err: model.NewError(model.CodeUnavailable, "runtime unavailable", errors.New("missing"))}
	dispatcher := NewDispatcher(Dependencies{Doctor: doctor})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), []string{"doctor"}, &stdout, &stderr); exit != 4 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
}
