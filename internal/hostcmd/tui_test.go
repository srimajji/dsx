package hostcmd

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/tui"
)

type tuiRunnerStub struct {
	calls            int
	requests         []tui.RunRequest
	intent           tui.Intent
	found            bool
	err              error
	progressRequests []tui.ProgressRequest
	progressSteps    []string
}

func (runner *tuiRunnerStub) Run(_ context.Context, request tui.RunRequest) (tui.Intent, bool, error) {
	runner.calls++
	runner.requests = append(runner.requests, request)
	return runner.intent, runner.found, runner.err
}

func (runner *tuiRunnerStub) RunProgress(ctx context.Context, request tui.ProgressRequest, operation tui.ProgressOperation) error {
	runner.progressRequests = append(runner.progressRequests, request)
	return operation(ctx, func(step string) {
		runner.progressSteps = append(runner.progressSteps, step)
	})
}

func TestNonTTYBarePrintsHelpWithoutTUIOrService(t *testing.T) {
	runner := &tuiRunnerStub{}
	lifecycle := &lifecycleStub{}
	clones := &cloneManagerStub{}
	dispatcher := NewDispatcher(Dependencies{
		TUI:       runner,
		Lifecycle: lifecycle,
		Clones:    clones,
		Stdin:     strings.NewReader(""),
		IsTTY:     func(io.Reader, io.Writer) bool { return false },
	})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), nil, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage:") || stderr.Len() != 0 {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
	if runner.calls != 0 {
		t.Fatalf("TUI calls = %d, want 0", runner.calls)
	}
	if lifecycle.calls() != 0 || clones.calls() != 0 {
		t.Fatalf("service calls: lifecycle=%d clones=%d, want 0", lifecycle.calls(), clones.calls())
	}
}

func TestSetupBareAndInitUseSameTUIRunner(t *testing.T) {
	runner := &tuiRunnerStub{}
	dispatcher := NewDispatcher(Dependencies{
		TUI:        runner,
		Stdin:      strings.NewReader("input"),
		IsTTY:      func(io.Reader, io.Writer) bool { return true },
		Accessible: true,
	})
	for _, args := range [][]string{nil, {"init", "--root", "/tmp/project"}} {
		var stdout, stderr bytes.Buffer
		if exit := dispatcher.Execute(context.Background(), args, &stdout, &stderr); exit != 0 {
			t.Fatalf("args %v: exit = %d, stderr = %q", args, exit, stderr.String())
		}
	}
	if runner.calls != 2 {
		t.Fatalf("TUI calls = %d, want 2", runner.calls)
	}
	if runner.requests[0].ForceSetup || runner.requests[0].Root != "." {
		t.Fatalf("bare request = %#v", runner.requests[0])
	}
	if !runner.requests[1].ForceSetup || runner.requests[1].Root != "/tmp/project" {
		t.Fatalf("init request = %#v", runner.requests[1])
	}
	if !runner.requests[0].Accessible || !runner.requests[1].Accessible {
		t.Fatalf("accessible requests = %#v", runner.requests)
	}
}

func TestTUIGitStatusUsesSelectedCloneAndRendersSanitizedStatus(t *testing.T) {
	hostile := "repo\x1b]52;c;Y2xpcA==\a\u202espoof"
	runner := &tuiRunnerStub{
		intent: tui.Intent{
			Action: "git-status", Project: "/tmp/project", Sandbox: "task", Repository: "web",
		},
		found: true,
	}
	lifecycle := &lifecycleStub{listResult: app.ListResult{Sandboxes: []app.SandboxSummary{{Sandbox: "task", Mode: model.ModeClone}}}}
	clones := &cloneManagerStub{statusResult: app.GitStatusResult{
		ProjectID: model.ProjectID(hostile),
		Sandbox:   "task",
		Repositories: []gitx.Status{{
			Repository: hostile, Sandbox: hostile, SourceRef: hostile, ResultBranch: hostile,
		}},
	}}
	dispatcher := NewDispatcher(Dependencies{
		Lifecycle: lifecycle,
		Clones:    clones,
		TUI:       runner,
		Stdin:     strings.NewReader("g"),
		IsTTY:     func(io.Reader, io.Writer) bool { return true },
	})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), nil, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	want := app.GitStatusRequest{Root: "/tmp/project", Sandbox: "task", Repository: "web"}
	if len(clones.statusRequests) != 1 || clones.statusRequests[0] != want {
		t.Fatalf("git status requests = %#v, want %#v", clones.statusRequests, want)
	}
	if len(lifecycle.listRequests) != 1 || lifecycle.listRequests[0] != (app.ListRequest{Root: "."}) {
		t.Fatalf("list requests = %#v", lifecycle.listRequests)
	}
	if len(runner.requests) != 1 || len(runner.requests[0].Sandboxes) != 1 || runner.requests[0].Sandboxes[0].Sandbox != "task" {
		t.Fatalf("TUI requests = %#v", runner.requests)
	}
	if !strings.Contains(stdout.String(), "Repository") || !strings.Contains(stdout.String(), `\x1b`) {
		t.Fatalf("git status output = %q", stdout.String())
	}
	for _, forbidden := range []string{"\x1b", "\a", "\u202e"} {
		if strings.Contains(stdout.String(), forbidden) {
			t.Fatalf("git status output contained hostile control %q: %q", forbidden, stdout.String())
		}
	}
}

func TestTUIGitDiffAndFetchUseSelectedClone(t *testing.T) {
	for _, test := range []struct {
		action string
		check  func(*testing.T, *cloneManagerStub)
	}{
		{
			action: "git-diff",
			check: func(t *testing.T, clones *cloneManagerStub) {
				t.Helper()
				want := app.GitDiffRequest{Root: "/tmp/project", Sandbox: "task", Repository: "web", MaxBytes: maxGitDiffBytes}
				if len(clones.diffRequests) != 1 || clones.diffRequests[0] != want {
					t.Fatalf("git diff requests = %#v, want %#v", clones.diffRequests, want)
				}
			},
		},
		{
			action: "git-fetch",
			check: func(t *testing.T, clones *cloneManagerStub) {
				t.Helper()
				want := app.GitFetchRequest{Root: "/tmp/project", Sandbox: "task", Repository: "web"}
				if len(clones.fetchRequests) != 1 || clones.fetchRequests[0] != want {
					t.Fatalf("git fetch requests = %#v, want %#v", clones.fetchRequests, want)
				}
			},
		},
	} {
		t.Run(test.action, func(t *testing.T) {
			runner := &tuiRunnerStub{intent: tui.Intent{
				Action: test.action, Project: "/tmp/project", Sandbox: "task", Repository: "web",
			}, found: true}
			lifecycle := &lifecycleStub{listResult: app.ListResult{Sandboxes: []app.SandboxSummary{{Sandbox: "task", Mode: model.ModeClone}}}}
			clones := &cloneManagerStub{}
			dispatcher := NewDispatcher(Dependencies{
				Lifecycle: lifecycle, Clones: clones, TUI: runner, Stdin: strings.NewReader(test.action),
				IsTTY: func(io.Reader, io.Writer) bool { return true },
			})
			var stdout, stderr bytes.Buffer
			if exit := dispatcher.Execute(context.Background(), nil, &stdout, &stderr); exit != 0 {
				t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
			}
			test.check(t, clones)
		})
	}
}

func TestTUICloneRunUsesReviewedPlanAndSelectedInputs(t *testing.T) {
	code := 23
	approval := strings.Repeat("a", 64)
	runner := &tuiRunnerStub{intent: tui.Intent{
		Action: "clone-run", Project: "/tmp/project", Sandbox: "review", Agent: "codex",
		Profile: "work", Prompt: "review the change", Browser: true, ApproveConfig: approval,
	}, found: true}
	lifecycle := &lifecycleStub{listResult: app.ListResult{Sandboxes: []app.SandboxSummary{{Sandbox: "main", Mode: model.ModeLive}}}}
	clones := &cloneManagerStub{runResult: app.CloneRunResult{Exit: runtime.Exit{Code: &code}}}
	dispatcher := NewDispatcher(Dependencies{
		Lifecycle: lifecycle, Clones: clones, TUI: runner, Stdin: strings.NewReader(""),
		IsTTY: func(io.Reader, io.Writer) bool { return true },
	})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), nil, &stdout, &stderr); exit != code {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if len(clones.runRequests) != 1 {
		t.Fatalf("clone run requests = %#v", clones.runRequests)
	}
	request := clones.runRequests[0]
	if request.Root != "/tmp/project" || request.ApproveConfig != approval || request.Sandbox != "review" ||
		request.Agent != "codex" || request.Profile != "work" || request.Prompt != "review the change" || !request.Browser {
		t.Fatalf("clone run request = %#v", request)
	}
}

func TestTUIGitStatusRejectsMissingOrLiveSelectionBeforeCall(t *testing.T) {
	tests := []struct {
		name      string
		intent    tui.Intent
		exit      int
		errorText string
	}{
		{
			name: "missing", intent: tui.Intent{Action: "git-status", Project: "/tmp/project"},
			exit: 2, errorText: "select a valid named clone sandbox",
		},
		{
			name: "live", intent: tui.Intent{Action: "git-status", Project: "/tmp/project", Sandbox: "main"},
			exit: 4, errorText: "unavailable for the live sandbox",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clones := &cloneManagerStub{}
			runner := &tuiRunnerStub{intent: test.intent, found: true}
			dispatcher := NewDispatcher(Dependencies{
				Clones: clones, TUI: runner, Stdin: strings.NewReader("g"),
				IsTTY: func(io.Reader, io.Writer) bool { return true },
			})
			var stdout, stderr bytes.Buffer
			if exit := dispatcher.Execute(context.Background(), nil, &stdout, &stderr); exit != test.exit {
				t.Fatalf("exit = %d, want %d; stderr = %q", exit, test.exit, stderr.String())
			}
			if clones.calls() != 0 {
				t.Fatalf("clone manager calls = %d, want 0", clones.calls())
			}
			if !strings.Contains(stderr.String(), test.errorText) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), test.errorText)
			}
		})
	}
}

func TestTUIIntentsInvokeLifecycleServices(t *testing.T) {
	for _, test := range []struct {
		action string
		check  func(*testing.T, *lifecycleStub)
	}{
		{action: "create", check: checkTUIStartAndShell},
		{action: "start", check: checkTUIStartAndShell},
		{action: "attach", check: checkTUIShell},
		{action: "stop", check: func(t *testing.T, service *lifecycleStub) {
			t.Helper()
			if len(service.stopRequests) != 1 || service.stopRequests[0].Root != "/tmp/project" {
				t.Fatalf("stop requests = %#v", service.stopRequests)
			}
		}},
		{action: "clean", check: func(t *testing.T, service *lifecycleStub) {
			t.Helper()
			if len(service.cleanRequests) != 1 || service.cleanRequests[0] != (app.CleanRequest{Root: "/tmp/project", Confirmed: true}) {
				t.Fatalf("clean requests = %#v", service.cleanRequests)
			}
		}},
	} {
		t.Run(test.action, func(t *testing.T) {
			runner := &tuiRunnerStub{intent: tui.Intent{Action: test.action, Project: "/tmp/project"}, found: true}
			service := &lifecycleStub{shellResult: shellResult(0)}
			dispatcher := NewDispatcher(Dependencies{
				Lifecycle: service,
				TUI:       runner,
				Stdin:     strings.NewReader("input"),
				IsTTY:     func(io.Reader, io.Writer) bool { return true },
			})
			var stdout, stderr bytes.Buffer
			if exit := dispatcher.Execute(context.Background(), nil, &stdout, &stderr); exit != 0 {
				t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
			}
			test.check(t, service)
			if test.action == "create" || test.action == "start" {
				if len(runner.progressRequests) != 1 || runner.progressRequests[0].Title != "Creating workspace" {
					t.Fatalf("progress requests = %#v", runner.progressRequests)
				}
				wantSteps := []string{"validate", "image", "resources", "workspace", "services", "ready"}
				if strings.Join(runner.progressSteps, ",") != strings.Join(wantSteps, ",") {
					t.Fatalf("progress steps = %#v, want %#v", runner.progressSteps, wantSteps)
				}
			}
		})
	}
}

func TestTUIDashboardStopPassesSelectedClone(t *testing.T) {
	service := &lifecycleStub{}
	dispatcher := NewDispatcher(Dependencies{Lifecycle: service})
	var stdout, stderr bytes.Buffer
	intent := tui.Intent{Action: "stop", Project: "/tmp/project", Sandbox: "review"}
	if exit := dispatcher.executeIntent(context.Background(), intent, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	want := app.StopRequest{Root: "/tmp/project", Sandbox: "review"}
	if len(service.stopRequests) != 1 || service.stopRequests[0] != want {
		t.Fatalf("stop requests = %#v, want %#v", service.stopRequests, want)
	}
}

func TestTUIRecreatePortsInvokesLifecycleReplacement(t *testing.T) {
	service := &lifecycleStub{startResult: app.StartResult{State: model.StateRunning}, shellResult: shellResult(0)}
	dispatcher := NewDispatcher(Dependencies{Lifecycle: service})
	intent := tui.Intent{Action: "recreate-ports", Project: "/tmp/project", ApproveConfig: strings.Repeat("a", 64)}
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.executeIntent(context.Background(), intent, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if len(service.recreateRequests) != 1 || service.recreateRequests[0].Root != "/tmp/project" ||
		service.recreateRequests[0].ApproveConfig != strings.Repeat("a", 64) ||
		!service.recreateRequests[0].Interactive || !service.recreateRequests[0].FinalConfirmed {
		t.Fatalf("recreate requests = %#v", service.recreateRequests)
	}
}

func checkTUIShell(t *testing.T, service *lifecycleStub) {
	t.Helper()
	if len(service.shellRequests) != 1 || !service.shellRequests[0].Terminal || service.shellRequests[0].RunInteractive == nil {
		t.Fatalf("shell requests = %#v", service.shellRequests)
	}
	if len(service.startRequests) != 0 {
		t.Fatalf("start requests = %#v, want none", service.startRequests)
	}
}

func checkTUIStartAndShell(t *testing.T, service *lifecycleStub) {
	t.Helper()
	if len(service.startRequests) != 1 || service.startRequests[0] != (app.StartRequest{
		Root: "/tmp/project", Interactive: true, FinalConfirmed: true,
	}) {
		t.Fatalf("start requests = %#v", service.startRequests)
	}
	if len(service.shellRequests) != 1 || !service.shellRequests[0].Terminal || service.shellRequests[0].RunInteractive == nil {
		t.Fatalf("shell requests = %#v", service.shellRequests)
	}
}

func TestTUIAttachReturnsExactChildSignalStatus(t *testing.T) {
	runner := &tuiRunnerStub{intent: tui.Intent{Action: "attach", Project: "/tmp/project"}, found: true}
	service := &lifecycleStub{shellResult: app.ShellResult{Exit: runtime.Exit{Signal: "SIGTERM"}}}
	dispatcher := NewDispatcher(Dependencies{
		Lifecycle: service,
		TUI:       runner,
		Stdin:     strings.NewReader("input"),
		IsTTY:     func(io.Reader, io.Writer) bool { return true },
	})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), nil, &stdout, &stderr); exit != 143 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
}
