package hostcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/terminal"
)

type lifecycleStub struct {
	startRequests    []app.StartRequest
	recreateRequests []app.StartRequest
	stopRequests     []app.StopRequest
	cleanRequests    []app.CleanRequest
	listRequests     []app.ListRequest
	shellRequests    []app.ShellRequest
	statusRequests   []app.ProcessStatusRequest
	logsRequests     []app.ProcessLogsRequest
	startResult      app.StartResult
	stopResult       app.StopResult
	cleanResult      app.CleanResult
	listResult       app.ListResult
	shellResult      app.ShellResult
	statusResult     app.ProcessStatusResult
	logsResult       app.ProcessLogsResult
	shellReady       app.ShellReady
	startErr         error
	stopErr          error
	cleanErr         error
	listErr          error
	shellErr         error
	statusErr        error
	logsErr          error
	shellFunc        func(context.Context, app.ShellRequest) (app.ShellResult, error)
}

func (stub *lifecycleStub) Start(_ context.Context, request app.StartRequest) (app.StartResult, error) {
	stub.startRequests = append(stub.startRequests, request)
	return stub.startResult, stub.startErr
}

func (stub *lifecycleStub) StartWithProgress(_ context.Context, request app.StartRequest, report app.StartProgressReporter) (app.StartResult, error) {
	stub.startRequests = append(stub.startRequests, request)
	for _, step := range []app.StartProgressStep{
		app.StartProgressValidate,
		app.StartProgressImage,
		app.StartProgressResources,
		app.StartProgressWorkspace,
		app.StartProgressServices,
		app.StartProgressReady,
	} {
		report(step)
	}
	return stub.startResult, stub.startErr
}

func (stub *lifecycleStub) RecreatePorts(_ context.Context, request app.StartRequest) (app.StartResult, error) {
	stub.recreateRequests = append(stub.recreateRequests, request)
	return stub.startResult, stub.startErr
}

func (stub *lifecycleStub) Stop(_ context.Context, request app.StopRequest) (app.StopResult, error) {
	stub.stopRequests = append(stub.stopRequests, request)
	return stub.stopResult, stub.stopErr
}

func (stub *lifecycleStub) Clean(_ context.Context, request app.CleanRequest) (app.CleanResult, error) {
	stub.cleanRequests = append(stub.cleanRequests, request)
	return stub.cleanResult, stub.cleanErr
}

func (stub *lifecycleStub) List(_ context.Context, request app.ListRequest) (app.ListResult, error) {
	stub.listRequests = append(stub.listRequests, request)
	return stub.listResult, stub.listErr
}

func (stub *lifecycleStub) Shell(ctx context.Context, request app.ShellRequest) (app.ShellResult, error) {
	stub.shellRequests = append(stub.shellRequests, request)
	if stub.shellFunc != nil {
		return stub.shellFunc(ctx, request)
	}
	if request.BeforeExec != nil {
		if err := request.BeforeExec(stub.shellReady); err != nil {
			return app.ShellResult{}, err
		}
	}
	return stub.shellResult, stub.shellErr
}

func (stub *lifecycleStub) ProcessStatus(_ context.Context, request app.ProcessStatusRequest) (app.ProcessStatusResult, error) {
	stub.statusRequests = append(stub.statusRequests, request)
	return stub.statusResult, stub.statusErr
}

func (stub *lifecycleStub) ProcessLogs(_ context.Context, request app.ProcessLogsRequest) (app.ProcessLogsResult, error) {
	stub.logsRequests = append(stub.logsRequests, request)
	return stub.logsResult, stub.logsErr
}

func (stub *lifecycleStub) calls() int {
	return len(stub.startRequests) + len(stub.recreateRequests) + len(stub.stopRequests) + len(stub.cleanRequests) + len(stub.listRequests) + len(stub.shellRequests) + len(stub.statusRequests) + len(stub.logsRequests)
}

type harnessStub struct {
	requests      []app.HarnessRunRequest
	result        app.HarnessRunResult
	err           error
	loginRequests []app.HarnessLoginRequest
	loginResult   app.HarnessLoginResult
	loginFunc     func(context.Context, app.HarnessLoginRequest) (app.HarnessLoginResult, error)
	runFunc       func(context.Context, app.HarnessRunRequest) (app.HarnessRunResult, error)
}

func (stub *harnessStub) Run(ctx context.Context, request app.HarnessRunRequest) (app.HarnessRunResult, error) {
	stub.requests = append(stub.requests, request)
	if stub.runFunc != nil {
		return stub.runFunc(ctx, request)
	}
	if request.BeforeExec != nil && (stub.result.Agent != "" || stub.result.Version != "") {
		if err := request.BeforeExec(stub.result); err != nil {
			return stub.result, err
		}
	}
	return stub.result, stub.err
}
func (stub *harnessStub) Login(ctx context.Context, request app.HarnessLoginRequest) (app.HarnessLoginResult, error) {
	stub.loginRequests = append(stub.loginRequests, request)
	if stub.loginFunc != nil {
		return stub.loginFunc(ctx, request)
	}
	if request.BeforeExec != nil && (stub.loginResult.Agent != "" || stub.loginResult.Version != "") {
		if err := request.BeforeExec(stub.loginResult); err != nil {
			return stub.loginResult, err
		}
	}
	return stub.loginResult, stub.err
}

func (stub *harnessStub) PurgeAuth(_ context.Context, request app.PurgeAuthRequest) error {
	stub.requests = append(stub.requests, app.HarnessRunRequest{Agent: request.Agent, Profile: request.Profile})
	return stub.err
}

func TestLifecycleUsageErrorsDoNotCallService(t *testing.T) {
	approval := strings.Repeat("a", 64)
	cases := []struct {
		name string
		args []string
	}{
		{name: "start missing approval", args: []string{"start", "--root", "/tmp/project"}},
		{name: "start short approval", args: []string{"start", "--approve-config", "abc"}},
		{name: "start uppercase approval", args: []string{"start", "--approve-config", strings.Repeat("A", 64)}},
		{name: "start nonhex approval", args: []string{"start", "--approve-config", strings.Repeat("g", 64)}},
		{name: "start positional", args: []string{"start", "--approve-config", approval, "extra"}},
		{name: "stop positional", args: []string{"stop", "extra"}},
		{name: "list invalid format", args: []string{"list", "--format", "yaml"}},
		{name: "list positional", args: []string{"list", "extra"}},
		{name: "clean positional", args: []string{"clean", "extra"}},
		{name: "shell missing separator", args: []string{"shell", "echo", "hello"}},
		{name: "shell command before separator", args: []string{"shell", "echo", "--", "hello"}},
		{name: "empty root", args: []string{"clean", "--root", ""}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			service := &lifecycleStub{}
			dispatcher := NewDispatcher(Dependencies{Lifecycle: service})
			var stdout, stderr bytes.Buffer
			if exit := dispatcher.Execute(context.Background(), test.args, &stdout, &stderr); exit != 2 {
				t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
			}
			if service.calls() != 0 {
				t.Fatalf("service calls = %d, want 0", service.calls())
			}
		})
	}
}

func TestShellAgentRoutesThroughHarnessService(t *testing.T) {
	code := 17
	stub := &harnessStub{result: app.HarnessRunResult{Agent: harness.Claude, Version: "2.1.226", Exit: runtime.Exit{Code: &code}}}
	dispatcher := NewDispatcher(Dependencies{
		Harness: stub,
		Stdin:   strings.NewReader(""),
		IsTTY:   func(io.Reader, io.Writer) bool { return true },
	})
	var stdout, stderr bytes.Buffer
	approval := strings.Repeat("a", 64)
	exit := dispatcher.Execute(context.Background(), []string{"shell", "--root", "/tmp/project", "--approve-config", approval, "--agent", "claude", "--profile", "work"}, &stdout, &stderr)
	if exit != 17 || len(stub.requests) != 1 {
		t.Fatalf("agent shell exit=%d stderr=%q requests=%#v", exit, stderr.String(), stub.requests)
	}
	request := stub.requests[0]
	if request.Agent != "claude" || request.Profile != "work" || !request.Interactive || request.RunInteractive == nil {
		t.Fatalf("harness request = %#v", request)
	}
	if stdout.String() != "Agent: \"claude\"\nVersion: \"2.1.226\"\n" || strings.Contains(stdout.String(), "provider") {
		t.Fatalf("harness status output = %q", stdout.String())
	}
	if stderr.String() != "Warning: concurrent editing harnesses in one live workspace may conflict or corrupt work.\n" {
		t.Fatalf("harness warning output = %q", stderr.String())
	}
}

func TestCleanRequiresConfirmationAndSupportsForceAll(t *testing.T) {

	t.Run("non-interactive refused", func(t *testing.T) {
		service := &lifecycleStub{}
		dispatcher := NewDispatcher(Dependencies{Lifecycle: service})
		var stdout, stderr bytes.Buffer
		if exit := dispatcher.Execute(context.Background(), []string{"clean", "--root", "/tmp/project"}, &stdout, &stderr); exit != 2 {
			t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
		}
		if len(service.cleanRequests) != 0 {
			t.Fatalf("cleanup called without confirmation: %#v", service.cleanRequests)
		}
	})
	t.Run("interactive cancellation", func(t *testing.T) {
		service := &lifecycleStub{}
		dispatcher := NewDispatcher(Dependencies{
			Lifecycle: service,
			Stdin:     strings.NewReader("n\n"),
			IsTTY:     func(io.Reader, io.Writer) bool { return true },
		})
		var stdout, stderr bytes.Buffer
		if exit := dispatcher.Execute(context.Background(), []string{"clean", "--root", "/tmp/project"}, &stdout, &stderr); exit != 0 {
			t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
		}
		if len(service.cleanRequests) != 0 || !strings.Contains(stdout.String(), "Cleanup cancelled.") {
			t.Fatalf("requests = %#v, stdout = %q", service.cleanRequests, stdout.String())

		}
	})
	t.Run("forced global cleanup", func(t *testing.T) {
		service := &lifecycleStub{cleanResult: app.CleanResult{Projects: 2, DeletedManifests: 2, DeletedResources: 5}}
		dispatcher := NewDispatcher(Dependencies{Lifecycle: service})
		var stdout, stderr bytes.Buffer
		if exit := dispatcher.Execute(context.Background(), []string{"clean", "--all", "--force"}, &stdout, &stderr); exit != 0 {
			t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
		}
		if len(service.cleanRequests) != 1 || service.cleanRequests[0] != (app.CleanRequest{Root: ".", All: true, Confirmed: true}) {
			t.Fatalf("clean requests = %#v", service.cleanRequests)
		}
		if !strings.Contains(stdout.String(), "Projects: 2") {
			t.Fatalf("stdout = %q", stdout.String())
		}
	})
}
func TestDiscardIntentDoesNotBypassCleanupConfirmation(t *testing.T) {
	service := &lifecycleStub{}
	dispatcher := NewDispatcher(Dependencies{Lifecycle: service})
	var stdout, stderr bytes.Buffer
	exit := dispatcher.Execute(context.Background(), []string{
		"clean", "--root", "/tmp/project", "--name", "protected", "--discard-unfetched",
	}, &stdout, &stderr)
	if exit != 2 || len(service.cleanRequests) != 0 {
		t.Fatalf("exit=%d requests=%#v stderr=%q", exit, service.cleanRequests, stderr.String())
	}
}

func TestNamedLifecycleFlagsAndDiscardIntentPlumbExactRequests(t *testing.T) {
	service := &lifecycleStub{
		stopResult:  app.StopResult{ProjectID: "project", Sandbox: "fix-test", RunID: "run", State: model.StateStopped},
		cleanResult: app.CleanResult{ProjectID: "project", Projects: 1, DeletedManifests: 1, DeletedResources: 2},
	}
	dispatcher := NewDispatcher(Dependencies{Lifecycle: service})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), []string{"stop", "--root", "/tmp/project", "--name", "fix-test"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("stop exit = %d, stderr = %q", exit, stderr.String())
	}
	if len(service.stopRequests) != 1 || service.stopRequests[0] != (app.StopRequest{Root: "/tmp/project", Sandbox: "fix-test"}) {
		t.Fatalf("stop requests = %#v", service.stopRequests)
	}
	stdout.Reset()
	stderr.Reset()
	if exit := dispatcher.Execute(context.Background(), []string{"clean", "--root", "/tmp/project", "--name", "fix-test", "--force", "--discard-unfetched"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("clean exit = %d, stderr = %q", exit, stderr.String())
	}
	want := app.CleanRequest{Root: "/tmp/project", Sandbox: "fix-test", Confirmed: true, DiscardUnfetched: true}
	if len(service.cleanRequests) != 1 || service.cleanRequests[0] != want {
		t.Fatalf("clean requests = %#v, want %#v", service.cleanRequests, want)
	}
}

func TestNamedLifecycleInvalidAndMainSelectorsFailBeforeService(t *testing.T) {
	commands := [][]string{
		{"stop", "--root", "/tmp/project", "--name", "main"},
		{"stop", "--root", "/tmp/project", "--name", "../bad"},
		{"clean", "--root", "/tmp/project", "--name", "main", "--force"},
		{"clean", "--root", "/tmp/project", "--name", "../bad", "--force"},
		{"clean", "--all", "--name", "fix-test", "--force"},
	}
	for _, command := range commands {
		service := &lifecycleStub{}
		dispatcher := NewDispatcher(Dependencies{Lifecycle: service})
		var stdout, stderr bytes.Buffer
		if exit := dispatcher.Execute(context.Background(), command, &stdout, &stderr); exit != 2 {
			t.Fatalf("%v exit = %d, stderr = %q", command, exit, stderr.String())
		}
		if service.calls() != 0 {
			t.Fatalf("%v reached lifecycle service: %#v", command, service)
		}
	}
}

func TestNamedCleanConfirmationNamesExactSandbox(t *testing.T) {
	service := &lifecycleStub{}
	dispatcher := NewDispatcher(Dependencies{
		Lifecycle: service,
		Stdin:     strings.NewReader("yes\n"),
		IsTTY:     func(io.Reader, io.Writer) bool { return true },
	})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), []string{"clean", "--root", "/tmp/project", "--name", "fix-test"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), `sandbox "fix-test"`) {
		t.Fatalf("confirmation did not identify exact sandbox: %q", stdout.String())
	}
	if len(service.cleanRequests) != 1 || service.cleanRequests[0].Sandbox != "fix-test" || service.cleanRequests[0].DiscardUnfetched {
		t.Fatalf("clean requests = %#v", service.cleanRequests)
	}
}

func TestLifecycleHelpDocumentsNamedSelectionAndExplicitDiscard(t *testing.T) {
	tests := []struct {
		command string
		wants   []string
	}{
		{command: "stop", wants: []string{"Usage: dsx stop", "--name NAME"}},
		{command: "clean", wants: []string{"Usage: dsx clean", "--name NAME", "--discard-unfetched", "dsx clean --all"}},
	}
	for _, test := range tests {
		service := &lifecycleStub{}
		dispatcher := NewDispatcher(Dependencies{Lifecycle: service})
		var stdout, stderr bytes.Buffer
		if exit := dispatcher.Execute(context.Background(), []string{test.command, "--help"}, &stdout, &stderr); exit != 0 {
			t.Fatalf("%s help exit = %d, stderr = %q", test.command, exit, stderr.String())
		}
		for _, want := range test.wants {
			if !strings.Contains(stdout.String(), want) {
				t.Fatalf("%s help = %q, missing %q", test.command, stdout.String(), want)
			}
		}
		if service.calls() != 0 {
			t.Fatalf("%s help reached service", test.command)
		}
	}
	dispatcher := NewDispatcher(Dependencies{})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), []string{"help"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("top-level help exit = %d, stderr = %q", exit, stderr.String())
	}
	for _, want := range []string{"dsx stop [--root PATH] [--name NAME]", "dsx clean [--root PATH] [--name NAME]", "--discard-unfetched", "--purge-auth"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("top-level help = %q, missing %q", stdout.String(), want)
		}
	}
}
func TestCleanPurgeAuthRemovesOnlySelectedProfileAfterCleanup(t *testing.T) {
	lifecycle := &lifecycleStub{}
	harnesses := &harnessStub{}
	dispatcher := NewDispatcher(Dependencies{Lifecycle: lifecycle, Harness: harnesses})
	var stdout, stderr bytes.Buffer
	exit := dispatcher.Execute(context.Background(), []string{"clean", "--root", "/tmp/project", "--force", "--purge-auth", "--agent", "codex", "--profile", "work"}, &stdout, &stderr)
	if exit != 0 || stderr.Len() != 0 || len(lifecycle.cleanRequests) != 1 || len(harnesses.requests) != 1 {
		t.Fatalf("purge exit=%d stderr=%q clean=%#v auth=%#v", exit, stderr.String(), lifecycle.cleanRequests, harnesses.requests)
	}
	if request := harnesses.requests[0]; request.Agent != "codex" || request.Profile != "work" {
		t.Fatalf("purged profile = %#v", request)
	}
}

func TestStartPassesExactApprovalAndRendersStructuredResult(t *testing.T) {
	approval := strings.Repeat("a1", 32)
	service := &lifecycleStub{startResult: app.StartResult{
		ProjectID: "project", Sandbox: "main", RunID: "run", State: model.StateRunning,
	}}
	dispatcher := NewDispatcher(Dependencies{Lifecycle: service})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), []string{"start", "--root", "/tmp/project", "--approve-config", approval}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if len(service.startRequests) != 1 || service.startRequests[0] != (app.StartRequest{Root: "/tmp/project", ApproveConfig: approval}) {
		t.Fatalf("start requests = %#v", service.startRequests)
	}
	if stdout.String() != "Project: \"project\"\nSandbox: \"main\"\nRun: \"run\"\nState: \"running\"\nExisting: false\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestListTextAndJSONAreDeterministicAndSafe(t *testing.T) {
	result := app.ListResult{Sandboxes: []app.SandboxSummary{
		{ProjectID: "p2", Sandbox: "z", RunID: "r2", Mode: model.ModeLive, State: model.StateStopped, Resources: 1},
		{ProjectID: "p1", Sandbox: model.SandboxName("a\x1b[2J"), RunID: "r1", Mode: model.ModeLive, State: model.StateRunning, Resources: 2, Warnings: []string{"z warning", "a\x1b]0;title\a"}},
	}}

	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			service := &lifecycleStub{listResult: result}
			dispatcher := NewDispatcher(Dependencies{Lifecycle: service})
			var stdout, stderr bytes.Buffer
			if exit := dispatcher.Execute(context.Background(), []string{"list", "--root", "/tmp/project", "--format", format}, &stdout, &stderr); exit != 0 {
				t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
			}
			if len(service.listRequests) != 1 || service.listRequests[0].Root != "/tmp/project" {
				t.Fatalf("list requests = %#v", service.listRequests)
			}
			if strings.Contains(stdout.String(), "\x1b[2J") || strings.Contains(stdout.String(), "\x1b]0") || strings.Contains(stdout.String(), "\a") {
				t.Fatalf("raw terminal control rendered: %q", stdout.String())
			}
			if format == "text" {
				if !strings.HasPrefix(stdout.String(), "Sandbox \"a\\\\x1b[2J\"") || strings.Index(stdout.String(), "a\\\\x1b]0;title\\\\a") > strings.Index(stdout.String(), "z warning") {
					t.Fatalf("text output is not sorted/sanitized: %q", stdout.String())
				}
				return
			}
			var decoded app.ListResult
			if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if len(decoded.Sandboxes) != 2 || decoded.Sandboxes[0].Sandbox != model.SandboxName("a\x1b[2J") {
				t.Fatalf("decoded list = %#v", decoded)
			}
		})
	}
}
func TestShellPassesApprovalIntoAtomicApplicationRequest(t *testing.T) {
	approval := strings.Repeat("ab", 32)
	service := &lifecycleStub{shellResult: shellResult(0)}
	dispatcher := NewDispatcher(Dependencies{Lifecycle: service})
	var stdout, stderr bytes.Buffer
	exit := dispatcher.Execute(context.Background(), []string{"shell", "--root", "/tmp/project", "--approve-config", approval, "--", "true"}, &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if len(service.startRequests) != 0 {
		t.Fatalf("shell performed a non-atomic Start call: %#v", service.startRequests)
	}
	if len(service.shellRequests) != 1 || service.shellRequests[0].ApproveConfig != approval || !reflect.DeepEqual(service.shellRequests[0].Argv, []string{"true"}) {
		t.Fatalf("shell requests = %#v", service.shellRequests)
	}
}

func TestLifecycleErrorsUseTypedExitMapping(t *testing.T) {
	service := &lifecycleStub{stopErr: model.NewError(model.CodeConflict, "already stopped", nil)}
	dispatcher := NewDispatcher(Dependencies{Lifecycle: service})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), []string{"stop", "--root", "/tmp/project"}, &stdout, &stderr); exit != 3 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "already stopped") || stdout.Len() != 0 {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func TestShellPreservesArgvAndExactChildStatus(t *testing.T) {
	command := []string{"printf", "%s", "$(not-a-shell)", "two words", "--literal"}
	for _, test := range []struct {
		name   string
		result app.ShellResult
		want   int
	}{
		{name: "exit", result: shellResult(23), want: 23},
		{name: "signal", result: app.ShellResult{Exit: runtime.Exit{Signal: "SIGTERM"}}, want: 143},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &lifecycleStub{shellResult: test.result}
			dispatcher := NewDispatcher(Dependencies{
				Lifecycle: service,
				Stdin:     strings.NewReader("input"),
				IsTTY:     func(io.Reader, io.Writer) bool { return true },
			})
			args := append([]string{"shell", "--root", "/tmp/project", "--"}, command...)
			var stdout, stderr bytes.Buffer
			if exit := dispatcher.Execute(context.Background(), args, &stdout, &stderr); exit != test.want {
				t.Fatalf("exit = %d, want %d, stderr = %q", exit, test.want, stderr.String())
			}
			if len(service.shellRequests) != 1 {
				t.Fatalf("shell requests = %#v", service.shellRequests)
			}
			request := service.shellRequests[0]
			if strings.Join(request.Argv, "\x00") != strings.Join(command, "\x00") || !request.Terminal || request.RunInteractive == nil || request.Stdin == nil || request.Stdout != &stdout || request.Stderr != &stderr {
				t.Fatalf("shell request = %#v", request)
			}
		})
	}
}

func TestShellDoesNotInventDefaultArgv(t *testing.T) {
	service := &lifecycleStub{shellResult: shellResult(0)}
	dispatcher := NewDispatcher(Dependencies{Lifecycle: service})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), []string{"shell", "--root", "/tmp/project"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}

	if len(service.shellRequests) != 1 || len(service.shellRequests[0].Argv) != 0 || service.shellRequests[0].RunInteractive != nil {
		t.Fatalf("shell requests = %#v", service.shellRequests)
	}
}

type signalReadyWriter struct {
	once  sync.Once
	ready chan struct{}
}

func (writer *signalReadyWriter) Write(data []byte) (int, error) {
	if bytes.Contains(data, []byte("ready")) {
		writer.once.Do(func() { close(writer.ready) })
	}
	return len(data), nil
}

func TestInteractiveShellClaimsCommandSignalsBeforeChildExecution(t *testing.T) {
	output := &signalReadyWriter{ready: make(chan struct{})}
	state := &terminalStateStub{}
	service := &lifecycleStub{}
	service.shellFunc = func(ctx context.Context, request app.ShellRequest) (app.ShellResult, error) {
		if request.RunInteractive == nil {
			return app.ShellResult{}, errors.New("interactive runner is missing")
		}
		exit, err := request.RunInteractive(ctx, app.InteractiveChild{
			Argv:   []string{"/bin/sh", "-c", "trap 'exit 42' INT; echo ready; while :; do :; done"},
			Stdout: output,
		})
		return app.ShellResult{Exit: exit}, err
	}
	dispatcher := NewDispatcher(Dependencies{
		Lifecycle:     service,
		Stdin:         strings.NewReader(""),
		IsTTY:         func(io.Reader, io.Writer) bool { return true },
		TerminalState: state,
	})
	ctx, stopSignals := terminal.CommandSignalContext(context.Background())
	defer stopSignals()
	result := make(chan int, 1)
	go func() {
		result <- dispatcher.Execute(ctx, []string{"shell", "--root", "/tmp/project"}, io.Discard, io.Discard)
	}()
	select {
	case <-output.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("interactive child did not become ready")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case exit := <-result:
		if exit != 42 {
			t.Fatalf("dispatcher exit = %d, want child exit 42", exit)
		}
		if state.releases != 1 || state.restores != 1 {
			t.Fatalf("release/restore = %d/%d, want 1/1", state.releases, state.restores)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("interactive child did not receive host SIGINT")
	}
}

func TestInteractiveShellRoutesSIGQUITThroughHandoffOnceAndRestores(t *testing.T) {
	output := &signalReadyWriter{ready: make(chan struct{})}
	state := &terminalStateStub{}
	service := &lifecycleStub{}
	service.shellFunc = func(ctx context.Context, request app.ShellRequest) (app.ShellResult, error) {
		exit, err := request.RunInteractive(ctx, app.InteractiveChild{
			Argv:   []string{"/bin/sh", "-c", "trap 'exit 43' QUIT; echo ready; while :; do :; done"},
			Stdout: output,
		})
		return app.ShellResult{Exit: exit}, err
	}
	dispatcher := NewDispatcher(Dependencies{
		Lifecycle: service, Stdin: strings.NewReader(""),
		IsTTY: func(io.Reader, io.Writer) bool { return true }, TerminalState: state,
	})
	ctx, stopSignals := terminal.CommandSignalContext(context.Background())
	defer stopSignals()
	result := make(chan int, 1)
	go func() {
		result <- dispatcher.Execute(ctx, []string{"shell", "--root", "/tmp/project"}, io.Discard, io.Discard)
	}()
	select {
	case <-output.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("interactive child did not become ready")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGQUIT); err != nil {
		t.Fatal(err)
	}
	select {
	case exit := <-result:
		if exit != 43 {
			t.Fatalf("dispatcher exit = %d, want child SIGQUIT handler exit 43", exit)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("interactive child did not receive SIGQUIT")
	}
	if state.releases != 1 || state.restores != 1 {
		t.Fatalf("release/restore = %d/%d, want 1/1", state.releases, state.restores)
	}
}

func TestInteractiveHarnessSetupSignalCancelsBeforeChildOwnership(t *testing.T) {
	entered := make(chan struct{})
	state := &terminalStateStub{}
	service := &harnessStub{
		runFunc: func(ctx context.Context, request app.HarnessRunRequest) (app.HarnessRunResult, error) {
			if request.RunInteractive == nil {
				return app.HarnessRunResult{}, errors.New("interactive runner is missing")
			}
			close(entered)
			<-ctx.Done()
			return app.HarnessRunResult{}, ctx.Err()
		},
	}
	dispatcher := NewDispatcher(Dependencies{Harness: service, TerminalState: state})
	ctx, stopSignals := terminal.CommandSignalContext(context.Background())
	defer stopSignals()
	result := make(chan int, 1)
	go func() {
		result <- dispatcher.runHarness(ctx, app.HarnessRunRequest{Interactive: true}, io.Discard)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("harness setup did not start")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case exit := <-result:
		if exit != 1 {
			t.Fatalf("dispatcher exit = %d, want cancellation failure", exit)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SIGINT did not cancel harness setup")
	}
	if state.releases != 0 || state.restores != 0 {
		t.Fatalf("terminal changed before child handoff: release/restore = %d/%d", state.releases, state.restores)
	}
}

type terminalStateStub struct {
	releases int
	restores int
	release  error
	restore  error
}

func (state *terminalStateStub) Release() error {
	state.releases++
	return state.release
}

func (state *terminalStateStub) Restore() error {
	state.restores++
	return state.restore
}

func TestInteractiveRunnerRestoresTerminalAfterSuccessFailureAndCancellation(t *testing.T) {
	cases := []struct {
		name  string
		ctx   context.Context
		child app.InteractiveChild
	}{
		{name: "success", ctx: context.Background(), child: app.InteractiveChild{Argv: []string{"/bin/sh", "-c", "exit 0"}}},
		{name: "failure", ctx: context.Background(), child: app.InteractiveChild{Argv: []string{"/definitely/missing/dsx-child"}}},
		{name: "cancel", ctx: cancelledContext(), child: app.InteractiveChild{Argv: []string{"/bin/sh", "-c", "sleep 10"}}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			state := &terminalStateStub{}
			dispatcher := NewDispatcher(Dependencies{TerminalState: state})
			_, _ = dispatcher.runInteractive(test.ctx, test.child)
			if state.releases != 1 || state.restores != 1 {
				t.Fatalf("release/restore = %d/%d", state.releases, state.restores)
			}
		})
	}
}

func TestInteractiveRunnerReleaseFailureDoesNotStartChildAndRestores(t *testing.T) {
	state := &terminalStateStub{release: errors.New("release failed")}
	dispatcher := NewDispatcher(Dependencies{TerminalState: state})
	exit, err := dispatcher.runInteractive(context.Background(), app.InteractiveChild{Argv: []string{"/bin/sh", "-c", "exit 0"}})
	if err == nil || exit.Code != nil || exit.Signal != "" || state.releases != 1 || state.restores != 1 {
		t.Fatalf("exit = %#v, err = %v, release/restore = %d/%d", exit, err, state.releases, state.restores)
	}
}

func shellResult(code int) app.ShellResult {
	return app.ShellResult{Exit: runtime.Exit{Code: &code}}
}

func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

var _ terminal.TerminalState = (*terminalStateStub)(nil)
