package hostcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/buildinfo"
	"github.com/srimajji/dsx/internal/config"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
)

func TestVersion(t *testing.T) {
	previousVersion, previousCommit, previousBuiltAt := buildinfo.Version, buildinfo.Commit, buildinfo.BuiltAt
	buildinfo.Version, buildinfo.Commit, buildinfo.BuiltAt = "1.2.3", "abc123", "2026-08-09T00:00:00Z"
	t.Cleanup(func() {
		buildinfo.Version, buildinfo.Commit, buildinfo.BuiltAt = previousVersion, previousCommit, previousBuiltAt
	})

	var stdout, stderr bytes.Buffer
	if exit := Execute(context.Background(), []string{"--version"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "dsx 1.2.3 (commit abc123") {
		t.Fatalf("unexpected version: %q", stdout.String())
	}
}

func TestVersionJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exit := Execute(context.Background(), []string{"--version", "--json"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	var value map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &value); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	for _, key := range []string{"version", "commit", "built_at", "go_version"} {
		if value[key] == "" {
			t.Errorf("missing %s", key)
		}
	}
}
func TestHelpFlagSucceeds(t *testing.T) {
	for _, argument := range []string{"--help", "-h", "help"} {
		var stdout, stderr bytes.Buffer
		if exit := Execute(context.Background(), []string{argument}, &stdout, &stderr); exit != 0 {
			t.Errorf("%s exit = %d, stderr = %q", argument, exit, stderr.String())
		}
		if !strings.Contains(stdout.String(), "Usage:") || stderr.Len() != 0 {
			t.Errorf("%s stdout = %q, stderr = %q", argument, stdout.String(), stderr.String())
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exit := Execute(context.Background(), []string{"missing"}, &stdout, &stderr); exit != 2 {
		t.Fatalf("exit = %d, want 2", exit)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

type inspectorStub struct {
	request app.InspectRequest
	result  app.InspectResult
	err     error
	calls   int
}

func (stub *inspectorStub) Inspect(_ context.Context, request app.InspectRequest) (app.InspectResult, error) {
	stub.calls++
	stub.request = request
	return stub.result, stub.err
}

type doctorStub struct {
	request app.DoctorRequest
	result  app.DoctorResult
	err     error
}

func (stub *doctorStub) Doctor(_ context.Context, request app.DoctorRequest) (app.DoctorResult, error) {
	stub.request = request
	return stub.result, stub.err
}

func TestInspectJSON(t *testing.T) {
	inspector := &inspectorStub{result: app.InspectResult{
		Facts: app.ProjectFacts{
			CanonicalRoot: "/tmp/project",
			ConfigPath:    ".dsx/config.jsonc",
			ConfigExists:  true,
			GitRoots:      []app.DetectedPath{},
			Lockfiles:     []app.DetectedPath{},
			Dockerfiles:   []app.DetectedPath{},
			DevenvFiles:   []app.DetectedPath{},
		},
		Diagnostics: []config.Diagnostic{{Severity: "warning", Code: "notice", Message: "included in JSON only"}},
		Plan: plan.ExecutionPlan{
			ContractVersion: plan.ContractVersion,
			Mode:            model.ModeLive,
			Agent:           "omp",
			Image:           plan.ResolvedImage{Reference: "example.invalid/dsx@sha256:abc"},
			ExecutableHash:  "sha256:test",
		},
	}}
	dispatcher := NewDispatcher(Dependencies{Inspector: inspector})
	var stdout, stderr bytes.Buffer

	if exit := dispatcher.Execute(context.Background(), []string{"inspect", "--format", "json", "--root", "/tmp/project", "--mode", "clone", "--sandbox", "smoke", "--agent", "omp", "--browser"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if inspector.request.Root != "/tmp/project" || inspector.request.Mode != "clone" || inspector.request.SandboxName != "smoke" || inspector.request.CLIOverrides.Agent != "omp" ||
		inspector.request.CLIOverrides.Browser == nil || !*inspector.request.CLIOverrides.Browser {
		t.Fatalf("request = %#v", inspector.request)
	}
	var result app.InspectResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode inspect JSON: %v", err)
	}
	if result.Plan.ExecutableHash != "sha256:test" || result.Facts.CanonicalRoot != "/tmp/project" {
		t.Fatalf("unexpected inspect result: %#v", result)
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "notice" {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
func TestInspectLeavesBrowserOverrideUnsetByDefault(t *testing.T) {
	inspector := &inspectorStub{result: app.InspectResult{Plan: plan.ExecutionPlan{ExecutableHash: "sha256:test"}}}
	dispatcher := NewDispatcher(Dependencies{Inspector: inspector})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), []string{"inspect", "--format", "json"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if inspector.request.CLIOverrides.Browser != nil {
		t.Fatalf("browser override = %v, want nil", *inspector.request.CLIOverrides.Browser)
	}
}

func TestInspectRejectsInvalidCloneSelectionBeforeServiceCall(t *testing.T) {
	for name, arguments := range map[string][]string{
		"clone missing sandbox": {"inspect", "--mode", "clone"},
		"clone main sandbox":    {"inspect", "--mode", "clone", "--sandbox", "main"},
		"live named sandbox":    {"inspect", "--sandbox", "smoke"},
		"invalid agent":         {"inspect", "--agent", "missing"},
	} {
		t.Run(name, func(t *testing.T) {
			inspector := &inspectorStub{}
			dispatcher := NewDispatcher(Dependencies{Inspector: inspector})
			var stdout, stderr bytes.Buffer
			if exit := dispatcher.Execute(context.Background(), arguments, &stdout, &stderr); exit != 2 {
				t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
			}
			if inspector.calls != 0 {
				t.Fatalf("inspection service called %d times", inspector.calls)
			}
		})
	}
}

func TestInspectErrorRendersDiagnostics(t *testing.T) {
	inspector := &inspectorStub{
		result: app.InspectResult{Diagnostics: []config.Diagnostic{{
			Severity: "error",
			Code:     "invalid_config",
			Path:     ".dsx/config.jsonc",
			Line:     4,
			Column:   7,
			Message:  "invalid field",
		}}},
		err: model.NewError(model.CodeInvalidInput, "configuration is invalid", nil),
	}
	dispatcher := NewDispatcher(Dependencies{Inspector: inspector})
	var stdout, stderr bytes.Buffer

	if exit := dispatcher.Execute(context.Background(), []string{"inspect"}, &stdout, &stderr); exit != 2 {
		t.Fatalf("exit = %d, want 2", exit)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
	for _, expected := range []string{`code="invalid_config"`, `line=4 column=7`, "configuration is invalid"} {
		if !strings.Contains(stderr.String(), expected) {
			t.Fatalf("stderr = %q, missing %q", stderr.String(), expected)
		}
	}
}

func TestDoctorTextRequiresBuilder(t *testing.T) {
	doctor := &doctorStub{result: app.DoctorResult{
		Capabilities: runtime.Capabilities{
			HostOS:          "darwin",
			HostVersion:     "27.0",
			HostArch:        "arm64",
			CLIVersion:      "1.2.2",
			ServerVersion:   "1.2.2",
			CompatibilityID: "apple-container-1.2.2",
			ServiceHealthy:  true,
			BuilderHealthy:  true,
		},
		Diagnostics: []config.Diagnostic{},
	}}
	dispatcher := NewDispatcher(Dependencies{Doctor: doctor})
	var stdout, stderr bytes.Buffer

	if exit := dispatcher.Execute(context.Background(), []string{"doctor", "--require-builder"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if !doctor.request.RequireBuilder {
		t.Fatal("RequireBuilder = false, want true")
	}
	for _, expected := range []string{`Host: "darwin" "27.0" "arm64"`, `Compatibility: "apple-container-1.2.2"`, "Builder healthy: true"} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("stdout = %q, missing %q", stdout.String(), expected)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestInspectAndDoctorRejectInvalidFormatBeforeServiceCall(t *testing.T) {
	for _, command := range []string{"inspect", "doctor"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			dispatcher := NewDispatcher(Dependencies{})
			if exit := dispatcher.Execute(context.Background(), []string{command, "--format", "yaml"}, &stdout, &stderr); exit != 2 {
				t.Fatalf("exit = %d, want 2", exit)
			}
			if !strings.Contains(stderr.String(), `format must be "text" or "json"`) {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}
