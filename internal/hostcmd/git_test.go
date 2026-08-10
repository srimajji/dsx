package hostcmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
)

type cloneManagerStub struct {
	runRequests    []app.CloneRunRequest
	statusRequests []app.GitStatusRequest
	diffRequests   []app.GitDiffRequest
	fetchRequests  []app.GitFetchRequest
	applyRequests  []app.GitApplyRequest
	runResult      app.CloneRunResult
	statusResult   app.GitStatusResult
	diffResult     app.GitDiffResult
	fetchResult    app.GitFetchResult
	applyResult    app.GitApplyResult
	runErr         error
	statusErr      error
	diffErr        error
	fetchErr       error
	applyErr       error
}

func (stub *cloneManagerStub) RunClone(_ context.Context, request app.CloneRunRequest) (app.CloneRunResult, error) {
	stub.runRequests = append(stub.runRequests, request)
	return stub.runResult, stub.runErr
}

func (stub *cloneManagerStub) GitStatus(_ context.Context, request app.GitStatusRequest) (app.GitStatusResult, error) {
	stub.statusRequests = append(stub.statusRequests, request)
	return stub.statusResult, stub.statusErr
}

func (stub *cloneManagerStub) GitDiff(_ context.Context, request app.GitDiffRequest) (app.GitDiffResult, error) {
	stub.diffRequests = append(stub.diffRequests, request)
	return stub.diffResult, stub.diffErr
}

func (stub *cloneManagerStub) GitFetch(_ context.Context, request app.GitFetchRequest) (app.GitFetchResult, error) {
	stub.fetchRequests = append(stub.fetchRequests, request)
	return stub.fetchResult, stub.fetchErr
}

func (stub *cloneManagerStub) GitApply(_ context.Context, request app.GitApplyRequest) (app.GitApplyResult, error) {
	stub.applyRequests = append(stub.applyRequests, request)
	return stub.applyResult, stub.applyErr
}

func (stub *cloneManagerStub) calls() int {
	return len(stub.runRequests) + len(stub.statusRequests) + len(stub.diffRequests) + len(stub.fetchRequests) + len(stub.applyRequests)
}

func TestRunPassesExactOneShotRequestAndExit(t *testing.T) {
	code := 37
	manager := &cloneManagerStub{runResult: app.CloneRunResult{Exit: runtime.Exit{Code: &code}}}
	stdin := strings.NewReader("not consumed by the command layer")
	dispatcher := NewDispatcher(Dependencies{Clones: manager, Stdin: stdin, IsTTY: func(io.Reader, io.Writer) bool {
		t.Fatal("one-shot run must not query terminal interactivity")
		return true
	}})
	var stdout, stderr bytes.Buffer
	approval := strings.Repeat("a", 64)
	exit := dispatcher.Execute(context.Background(), []string{"run", "--name", "fix-test", "--agent", "codex", "--profile", "work", "--browser", "--approve-config", approval, "--", "fix --all exactly"}, &stdout, &stderr)
	if exit != code || stderr.Len() != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if len(manager.runRequests) != 1 {
		t.Fatalf("run requests = %d", len(manager.runRequests))
	}
	request := manager.runRequests[0]
	if request.Root != "." || request.Sandbox != "fix-test" || request.Agent != "codex" || request.Profile != "work" || request.ApproveConfig != approval || request.Prompt != "fix --all exactly" || !request.Browser {
		t.Fatalf("request = %#v", request)
	}
	if request.Stdin != stdin || request.Stdout != &stdout || request.Stderr != &stderr || request.RunInteractive != nil || request.MCPServers != nil || request.Environment != nil {
		t.Fatalf("one-shot request plumbing = %#v", request)
	}
}

func TestRunOmittedBrowserFlagPreservesFalseAndPromptSeparator(t *testing.T) {
	code := 0
	manager := &cloneManagerStub{runResult: app.CloneRunResult{Exit: runtime.Exit{Code: &code}}}
	dispatcher := NewDispatcher(Dependencies{Clones: manager, IsTTY: func(io.Reader, io.Writer) bool {
		t.Fatal("one-shot run must not query terminal interactivity")
		return true
	}})
	var stdout, stderr bytes.Buffer
	exit := dispatcher.Execute(context.Background(), []string{
		"run", "--name", "task", "--agent", "codex", "--profile", "default",
		"--approve-config", strings.Repeat("b", 64), "--", "--browser",
	}, &stdout, &stderr)
	if exit != 0 || stderr.Len() != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if len(manager.runRequests) != 1 {
		t.Fatalf("run requests = %d", len(manager.runRequests))
	}
	request := manager.runRequests[0]
	if request.Browser || request.Prompt != "--browser" {
		t.Fatalf("request = %#v", request)
	}
}

func TestRunBrowserWithoutNamedCloneFailsBeforeManagerMutation(t *testing.T) {
	manager := &cloneManagerStub{}
	dispatcher := NewDispatcher(Dependencies{Clones: manager})
	var stdout, stderr bytes.Buffer
	exit := dispatcher.Execute(context.Background(), []string{
		"run", "--browser", "--agent", "codex", "--profile", "default",
		"--approve-config", strings.Repeat("c", 64), "--", "prompt",
	}, &stdout, &stderr)
	if exit != 2 || stderr.Len() == 0 {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", exit, stdout.String(), stderr.String())
	}
	if manager.calls() != 0 {
		t.Fatalf("manager calls = %d", manager.calls())
	}
}

func TestRunBrowserFlagAppearsInCommandAndTopLevelHelp(t *testing.T) {
	for _, args := range [][]string{{"run", "--help"}, {"help"}} {
		var stdout, stderr bytes.Buffer
		if exit := Execute(context.Background(), args, &stdout, &stderr); exit != 0 || stderr.Len() != 0 {
			t.Fatalf("args = %#v, exit = %d, stderr = %q", args, exit, stderr.String())
		}
		if !strings.Contains(stdout.String(), "--browser") {
			t.Fatalf("args = %#v, help = %q", args, stdout.String())
		}
	}
}

func TestRunPreservesSignalExitAndRejectsInvalidExit(t *testing.T) {
	manager := &cloneManagerStub{runResult: app.CloneRunResult{Exit: runtime.Exit{Signal: "SIGTERM"}}}
	dispatcher := NewDispatcher(Dependencies{Clones: manager})
	args := []string{"run", "--name", "task", "--agent", "codex", "--profile", "default", "--approve-config", strings.Repeat("b", 64), "--", "prompt"}
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), args, &stdout, &stderr); exit != 143 {
		t.Fatalf("signal exit = %d, stderr = %q", exit, stderr.String())
	}
	code := 1
	manager.runResult.Exit = runtime.Exit{Code: &code, Signal: "SIGTERM"}
	stderr.Reset()
	if exit := dispatcher.Execute(context.Background(), args, &stdout, &stderr); exit != 1 || !strings.Contains(stderr.String(), "both an exit code and a signal") {
		t.Fatalf("invalid exit = %d, stderr = %q", exit, stderr.String())
	}
}

func TestCloneUsageFailuresNeverCallManager(t *testing.T) {
	approval := strings.Repeat("a", 64)
	tests := [][]string{
		{"run", "--name", "Upper", "--agent", "codex", "--profile", "default", "--approve-config", approval, "--", "prompt"},
		{"run", "--name", "task", "--agent", "unknown", "--profile", "default", "--approve-config", approval, "--", "prompt"},
		{"run", "--name", "task", "--agent", "codex", "--profile", "", "--approve-config", approval, "--", "prompt"},
		{"run", "--name", "task", "--agent", "codex", "--profile", "default", "--approve-config", "ABC", "--", "prompt"},
		{"run", "--name", "task", "--agent", "codex", "--profile", "default", "--approve-config", approval, "prompt"},
		{"run", "--name", "task", "--agent", "codex", "--profile", "default", "--approve-config", approval, "--", "one", "two"},
		{"run", "--name", "task", "--agent", "codex", "--profile", "default", "--approve-config", approval, "--", "   "},
		{"git"},
		{"git", "unknown", "task"},
		{"git", "status"},
		{"git", "status", "Upper"},
		{"git", "status", "task", "--format", "yaml"},
		{"git", "diff", "task", "--root", ""},
		{"git", "fetch", "task", "extra"},
		{"git", "apply", "--root", ".", "task"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			manager := &cloneManagerStub{}
			dispatcher := NewDispatcher(Dependencies{Clones: manager})
			var stdout, stderr bytes.Buffer
			if exit := dispatcher.Execute(context.Background(), args, &stdout, &stderr); exit != 2 {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q", exit, stdout.String(), stderr.String())
			}
			if manager.calls() != 0 {
				t.Fatalf("manager calls = %d", manager.calls())
			}
		})
	}
}

func TestCloneCommandsReportUnavailableOnlyAfterValidParsing(t *testing.T) {
	approval := strings.Repeat("c", 64)
	commands := [][]string{
		{"run", "--name", "task", "--agent", "codex", "--profile", "default", "--approve-config", approval, "--", "prompt"},
		{"git", "status", "task"},
		{"git", "diff", "task"},
		{"git", "fetch", "task"},
		{"git", "apply", "task"},
	}
	for _, args := range commands {
		var stdout, stderr bytes.Buffer
		if exit := NewDispatcher(Dependencies{}).Execute(context.Background(), args, &stdout, &stderr); exit != 4 || !strings.Contains(stderr.String(), "clone service is unavailable") {
			t.Fatalf("args = %#v, exit = %d, stderr = %q", args, exit, stderr.String())
		}
	}
}

func TestGitCommandsPassExactRequests(t *testing.T) {
	manager := &cloneManagerStub{}
	dispatcher := NewDispatcher(Dependencies{Clones: manager})
	commands := [][]string{
		{"git", "status", "task", "--repo", "member", "--root", "/project", "--format", "json"},
		{"git", "diff", "task", "--repo", "member", "--root", "/project", "--format", "json"},
		{"git", "fetch", "task", "--repo", "member", "--root", "/project", "--format", "json"},
		{"git", "apply", "task", "--repo", "member", "--root", "/project", "--format", "json"},
	}
	for _, args := range commands {
		var stdout, stderr bytes.Buffer
		if exit := dispatcher.Execute(context.Background(), args, &stdout, &stderr); exit != 0 {
			t.Fatalf("args = %#v, exit = %d, stderr = %q", args, exit, stderr.String())
		}
	}
	if !reflect.DeepEqual(manager.statusRequests, []app.GitStatusRequest{{Root: "/project", Sandbox: "task", Repository: "member"}}) {
		t.Fatalf("status requests = %#v", manager.statusRequests)
	}
	if !reflect.DeepEqual(manager.diffRequests, []app.GitDiffRequest{{Root: "/project", Sandbox: "task", Repository: "member", MaxBytes: maxGitDiffBytes}}) {
		t.Fatalf("diff requests = %#v", manager.diffRequests)
	}
	if !reflect.DeepEqual(manager.fetchRequests, []app.GitFetchRequest{{Root: "/project", Sandbox: "task", Repository: "member"}}) {
		t.Fatalf("fetch requests = %#v", manager.fetchRequests)
	}
	if !reflect.DeepEqual(manager.applyRequests, []app.GitApplyRequest{{Root: "/project", Sandbox: "task", Repository: "member"}}) {
		t.Fatalf("apply requests = %#v", manager.applyRequests)
	}
}

func TestGitJSONIsStableAndPreservesRawPatch(t *testing.T) {
	patch := []byte{0, 1, 2, 0x1b, 0xff, '\n'}
	manager := &cloneManagerStub{
		statusResult: app.GitStatusResult{ProjectID: "project", Sandbox: "task", Repositories: []gitx.Status{{Repository: "z"}, {Repository: "a"}}},
		diffResult:   app.GitDiffResult{ProjectID: "project", Sandbox: "task", Diffs: []app.RepositoryDiff{{Repository: "z", Patch: []byte("z")}, {Repository: "a", Patch: patch, Truncated: true}}},
		fetchResult:  app.GitFetchResult{ProjectID: "project", Sandbox: "task", Repositories: []gitx.FetchResult{{HostRef: "z", Commit: "2"}, {HostRef: "a", Commit: "1"}}},
		applyResult:  app.GitApplyResult{ProjectID: "project", Sandbox: "task", Repositories: []gitx.ApplyResult{{AppliedCommit: "z", Paths: []string{"z", "a"}}, {AppliedCommit: "a", Paths: []string{"b"}}}},
	}
	dispatcher := NewDispatcher(Dependencies{Clones: manager})
	for _, operation := range []string{"status", "diff", "fetch", "apply"} {
		var first, second, stderr bytes.Buffer
		args := []string{"git", operation, "task", "--format", "json"}
		if exit := dispatcher.Execute(context.Background(), args, &first, &stderr); exit != 0 {
			t.Fatalf("%s first exit = %d, stderr = %q", operation, exit, stderr.String())
		}
		if exit := dispatcher.Execute(context.Background(), args, &second, &stderr); exit != 0 {
			t.Fatalf("%s second exit = %d, stderr = %q", operation, exit, stderr.String())
		}
		if first.String() != second.String() || !json.Valid(first.Bytes()) {
			t.Fatalf("%s unstable or invalid JSON: %q != %q", operation, first.String(), second.String())
		}
	}
	var output bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), []string{"git", "diff", "task", "--format", "json"}, &output, io.Discard); exit != 0 {
		t.Fatalf("diff exit = %d", exit)
	}
	var decoded struct {
		Diffs []struct {
			Repository string `json:"repository"`
			Patch      string `json:"patch"`
			Truncated  bool   `json:"truncated"`
		} `json:"diffs"`
	}
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Diffs) != 2 || decoded.Diffs[0].Repository != "a" || decoded.Diffs[0].Patch != base64.StdEncoding.EncodeToString(patch) || !decoded.Diffs[0].Truncated {
		t.Fatalf("decoded diffs = %#v", decoded.Diffs)
	}
}

func TestGitTextRenderingSanitizesControlsAndShowsBinaryTruncation(t *testing.T) {
	hostile := "repo\x1b[2J\x1b]0;owned\a\r\t"
	manager := &cloneManagerStub{
		statusResult: app.GitStatusResult{ProjectID: model.ProjectID(hostile), Sandbox: model.SandboxName(hostile), Repositories: []gitx.Status{{Repository: "z"}, {Repository: hostile, Sandbox: hostile, SourceRef: hostile, SourceCommit: hostile, ResultBranch: hostile, ResultCommit: hostile, HostCommit: hostile, HostTrackedFingerprint: hostile, FetchedCommit: hostile}}},
		diffResult:   app.GitDiffResult{ProjectID: "project", Sandbox: "task", Diffs: []app.RepositoryDiff{{Repository: "text", Patch: []byte("+line\x1b]8;;https://bad\aevil\x1b]8;;\a\n")}, {Repository: hostile, Patch: []byte{0, 0xff, 1}, Truncated: true}}},
		fetchResult:  app.GitFetchResult{ProjectID: "project", Sandbox: "task", Repositories: []gitx.FetchResult{{Repository: "z", HostRef: "z", Commit: hostile}, {Repository: hostile, HostRef: hostile, Commit: "a"}}},
		applyResult:  app.GitApplyResult{ProjectID: "project", Sandbox: "task", Repositories: []gitx.ApplyResult{{Repository: hostile, AppliedCommit: hostile, Paths: []string{"z", hostile}}}},
	}
	dispatcher := NewDispatcher(Dependencies{Clones: manager})
	for _, operation := range []string{"status", "diff", "fetch", "apply"} {
		var stdout, stderr bytes.Buffer
		if exit := dispatcher.Execute(context.Background(), []string{"git", operation, "task"}, &stdout, &stderr); exit != 0 {
			t.Fatalf("%s exit = %d, stderr = %q", operation, exit, stderr.String())
		}
		assertNoTerminalControls(t, stdout.Bytes())
		if !strings.Contains(stdout.String(), `\x1b`) {
			t.Fatalf("%s output did not visibly escape hostile input: %q", operation, stdout.String())
		}
		if operation == "diff" && (!strings.Contains(stdout.String(), "[binary diff omitted: 3 bytes]") || !strings.Contains(stdout.String(), "[diff truncated]")) {
			t.Fatalf("diff markers missing: %q", stdout.String())
		}
	}
}

func TestGitStatusTextNamesHostFieldsAndManifestRefs(t *testing.T) {
	manager := &cloneManagerStub{statusResult: app.GitStatusResult{Repositories: []gitx.Status{{
		Repository: "workspace", SourceRef: "refs/heads/main", ResultBranch: "dsx/task",
		HostCommit: "host", HostTrackedFingerprint: "fingerprint", HostTrackedClean: true,
	}}}}
	var stdout, stderr bytes.Buffer
	if exit := NewDispatcher(Dependencies{Clones: manager}).Execute(context.Background(), []string{"git", "status", "task"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	for _, token := range []string{`source_ref="refs/heads/main"`, `result_branch="dsx/task"`, `host_commit="host"`, `host_tracked_fingerprint="fingerprint"`, `host_tracked_clean=true`} {
		if !strings.Contains(stdout.String(), token) {
			t.Fatalf("status output missing %q: %q", token, stdout.String())
		}
	}
}

func TestGitTextRenderingOrdersMultipleRepositories(t *testing.T) {
	manager := &cloneManagerStub{
		statusResult: app.GitStatusResult{Repositories: []gitx.Status{{Repository: "z"}, {Repository: "a"}}},
		fetchResult:  app.GitFetchResult{Repositories: []gitx.FetchResult{{Repository: "z", HostRef: "refs/z"}, {Repository: "a", HostRef: "refs/a"}}},
		applyResult:  app.GitApplyResult{Repositories: []gitx.ApplyResult{{Repository: "z", AppliedCommit: "z", Paths: []string{"z", "a"}}, {Repository: "a", AppliedCommit: "a", Paths: []string{"b"}}}},
	}
	dispatcher := NewDispatcher(Dependencies{Clones: manager})
	tests := []struct {
		op          string
		first, last string
	}{{"status", `Repository "a"`, `Repository "z"`}, {"fetch", `Fetched repository "a"`, `Fetched repository "z"`}, {"apply", `Applied repository "a"`, `Applied repository "z"`}}
	for _, test := range tests {
		var stdout bytes.Buffer
		if exit := dispatcher.Execute(context.Background(), []string{"git", test.op, "task"}, &stdout, io.Discard); exit != 0 {
			t.Fatalf("%s exit = %d", test.op, exit)
		}
		if strings.Index(stdout.String(), test.first) < 0 || strings.Index(stdout.String(), test.first) >= strings.Index(stdout.String(), test.last) {
			t.Fatalf("%s output order = %q", test.op, stdout.String())
		}
		if test.op == "apply" && strings.Index(stdout.String(), `Path: "a"`) >= strings.Index(stdout.String(), `Path: "z"`) {
			t.Fatalf("apply path order = %q", stdout.String())
		}
	}
}

func TestCloneServiceErrorsUseTypedExitCodes(t *testing.T) {
	manager := &cloneManagerStub{
		runErr:    model.NewError(model.CodeConflict, "run conflict", nil),
		statusErr: model.NewError(model.CodeUnavailable, "status unavailable", nil),
		diffErr:   errors.New("diff failed"),
		fetchErr:  model.NewError(model.CodeDataLoss, "fetch blocked", nil),
		applyErr:  model.NewError(model.CodeUnapproved, "apply blocked", nil),
	}
	dispatcher := NewDispatcher(Dependencies{Clones: manager})
	approval := strings.Repeat("d", 64)
	tests := []struct {
		args []string
		exit int
	}{{[]string{"run", "--name", "task", "--agent", "codex", "--profile", "default", "--approve-config", approval, "--", "prompt"}, 3}, {[]string{"git", "status", "task"}, 4}, {[]string{"git", "diff", "task"}, 1}, {[]string{"git", "fetch", "task"}, 3}, {[]string{"git", "apply", "task"}, 3}}
	for _, test := range tests {
		var stdout, stderr bytes.Buffer
		if exit := dispatcher.Execute(context.Background(), test.args, &stdout, &stderr); exit != test.exit || stderr.Len() == 0 {
			t.Fatalf("args = %#v, exit = %d, stderr = %q", test.args, exit, stderr.String())
		}
	}
}

// TestFakeManagerCloneCommandSmoke documents the command-layer Slice 6 smoke:
// run a named clone, then inspect, diff, fetch, and apply that exact sandbox.
func TestFakeManagerCloneCommandSmoke(t *testing.T) {
	code := 0
	manager := &cloneManagerStub{
		runResult:    app.CloneRunResult{Exit: runtime.Exit{Code: &code}},
		statusResult: app.GitStatusResult{Repositories: []gitx.Status{{Repository: "root", HostTrackedClean: false}}},
		diffResult:   app.GitDiffResult{Diffs: []app.RepositoryDiff{{Repository: "root", Patch: []byte("+smoke\n")}}},
		fetchResult:  app.GitFetchResult{Repositories: []gitx.FetchResult{{Repository: "root", HostRef: "refs/remotes/dsx/smoke", Commit: "abc"}}},
		applyResult:  app.GitApplyResult{Repositories: []gitx.ApplyResult{{Repository: "root", AppliedCommit: "abc", Paths: []string{"smoke.txt"}}}},
	}
	dispatcher := NewDispatcher(Dependencies{Clones: manager})
	approval := strings.Repeat("e", 64)
	commands := [][]string{
		{"run", "--name", "smoke", "--agent", "codex", "--approve-config", approval, "--", "change all file classes"},
		{"git", "status", "smoke"},
		{"git", "diff", "smoke"},
		{"git", "fetch", "smoke"},
		{"git", "apply", "smoke"},
	}
	for _, args := range commands {
		var stdout, stderr bytes.Buffer
		if exit := dispatcher.Execute(context.Background(), args, &stdout, &stderr); exit != 0 {
			t.Fatalf("args = %#v, exit = %d, stderr = %q", args, exit, stderr.String())
		}
	}
	if manager.calls() != len(commands) || manager.runRequests[0].Sandbox != "smoke" || manager.runRequests[0].Profile != "default" || manager.statusRequests[0].Sandbox != "smoke" || manager.diffRequests[0].Sandbox != "smoke" || manager.fetchRequests[0].Sandbox != "smoke" || manager.applyRequests[0].Sandbox != "smoke" {
		t.Fatalf("smoke requests were not routed exactly: %#v", manager)
	}
}

func assertNoTerminalControls(t *testing.T, output []byte) {
	t.Helper()
	for _, value := range output {
		if value == '\n' {
			continue
		}
		if value < 0x20 || value == 0x7f {
			t.Fatalf("terminal control %#x in %q", value, output)
		}
	}
}
