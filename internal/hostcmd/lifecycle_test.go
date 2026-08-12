package hostcmd

import (
	"bytes"
	tea "charm.land/bubbletea/v2"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
	"github.com/srimajji/dsx/internal/tui"
)

type workspaceStub struct {
	creates      []app.WorkspaceCreateRequest
	opens        []app.WorkspaceOpenRequest
	starts       []app.WorkspaceStartRequest
	stops        []app.WorkspaceStopRequest
	restarts     []app.WorkspaceRestartRequest
	removes      []app.WorkspaceRemoveRequest
	lists        []app.WorkspaceListRequest
	result       app.WorkspaceResult
	openResult   app.WorkspaceOpenResult
	listResult   app.WorkspaceListResult
	removeResult app.WorkspaceRemoveResult
	err          error
	createErr    error
	openErr      error
	events       *[]string
}

func (stub *workspaceStub) Create(_ context.Context, request app.WorkspaceCreateRequest) (app.WorkspaceResult, error) {
	stub.creates = append(stub.creates, request)
	if stub.events != nil {
		*stub.events = append(*stub.events, "create")
	}
	if stub.createErr != nil {
		return stub.result, stub.createErr
	}
	return stub.result, stub.err
}
func (stub *workspaceStub) Open(_ context.Context, request app.WorkspaceOpenRequest) (app.WorkspaceOpenResult, error) {
	stub.opens = append(stub.opens, request)
	if stub.events != nil {
		*stub.events = append(*stub.events, "open")
	}
	if stub.openErr != nil {
		return stub.openResult, stub.openErr
	}
	return stub.openResult, stub.err
}
func (stub *workspaceStub) Start(_ context.Context, request app.WorkspaceStartRequest) (app.WorkspaceResult, error) {
	stub.starts = append(stub.starts, request)
	return stub.result, stub.err
}
func (stub *workspaceStub) Stop(_ context.Context, request app.WorkspaceStopRequest) (app.WorkspaceResult, error) {
	stub.stops = append(stub.stops, request)
	return stub.result, stub.err
}
func (stub *workspaceStub) Restart(_ context.Context, request app.WorkspaceRestartRequest) (app.WorkspaceResult, error) {
	stub.restarts = append(stub.restarts, request)
	return stub.result, stub.err
}
func (stub *workspaceStub) Remove(_ context.Context, request app.WorkspaceRemoveRequest) (app.WorkspaceRemoveResult, error) {
	stub.removes = append(stub.removes, request)
	return stub.removeResult, stub.err
}
func (stub *workspaceStub) List(_ context.Context, request app.WorkspaceListRequest) (app.WorkspaceListResult, error) {
	stub.lists = append(stub.lists, request)
	return stub.listResult, stub.err
}
func (stub *workspaceStub) calls() int {
	return len(stub.creates) + len(stub.opens) + len(stub.starts) + len(stub.stops) + len(stub.restarts) + len(stub.removes) + len(stub.lists)
}

type awsWorkspaceStub struct {
	enables       []app.AWSWorkspaceRequest
	disables      []app.AWSWorkspaceRequest
	statuses      []app.AWSWorkspaceRequest
	enableResult  app.AWSWorkspaceResult
	disableResult app.AWSWorkspaceResult
	statusResult  app.AWSWorkspaceResult
	enableErr     error
	disableErr    error
	statusErr     error
}

func (stub *awsWorkspaceStub) Enable(_ context.Context, request app.AWSWorkspaceRequest) (app.AWSWorkspaceResult, error) {
	stub.enables = append(stub.enables, request)
	return stub.enableResult, stub.enableErr
}

func (stub *awsWorkspaceStub) Disable(_ context.Context, request app.AWSWorkspaceRequest) (app.AWSWorkspaceResult, error) {
	stub.disables = append(stub.disables, request)
	return stub.disableResult, stub.disableErr
}

func (stub *awsWorkspaceStub) Status(_ context.Context, request app.AWSWorkspaceRequest) (app.AWSWorkspaceResult, error) {
	stub.statuses = append(stub.statuses, request)
	return stub.statusResult, stub.statusErr
}

type gitStub struct {
	updates  []app.WorkspaceUpdateRequest
	statuses []app.GitStatusRequest
	diffs    []app.GitDiffRequest
	fetches  []app.GitFetchRequest
	applies  []app.GitApplyRequest
	result   app.WorkspaceResult
}

func (stub *gitStub) Update(_ context.Context, request app.WorkspaceUpdateRequest) (app.WorkspaceResult, error) {
	stub.updates = append(stub.updates, request)
	return stub.result, nil
}
func (stub *gitStub) GitStatus(_ context.Context, request app.GitStatusRequest) (app.GitStatusResult, error) {
	stub.statuses = append(stub.statuses, request)
	return app.GitStatusResult{}, nil
}
func (stub *gitStub) GitDiff(_ context.Context, request app.GitDiffRequest) (app.GitDiffResult, error) {
	stub.diffs = append(stub.diffs, request)
	return app.GitDiffResult{}, nil
}
func (stub *gitStub) GitFetch(_ context.Context, request app.GitFetchRequest) (app.GitFetchResult, error) {
	stub.fetches = append(stub.fetches, request)
	return app.GitFetchResult{}, nil
}
func (stub *gitStub) GitApply(_ context.Context, request app.GitApplyRequest) (app.GitApplyResult, error) {
	stub.applies = append(stub.applies, request)
	return app.GitApplyResult{}, nil
}
func (stub *gitStub) calls() int {
	return len(stub.updates) + len(stub.statuses) + len(stub.diffs) + len(stub.fetches) + len(stub.applies)
}

type agentStub struct {
	requests []app.AgentRunRequest
	result   app.AgentRunResult
	err      error
}

func (stub *agentStub) Run(_ context.Context, request app.AgentRunRequest) (app.AgentRunResult, error) {
	stub.requests = append(stub.requests, request)
	return stub.result, stub.err
}

type authStub struct {
	statuses  []app.AuthStatusRequest
	imports   []app.AuthImportRequest
	logins    []app.AuthLoginRequest
	refreshes []app.AuthRefreshRequest
	purges    []app.AuthPurgeRequest
}

func (stub *authStub) Status(_ context.Context, r app.AuthStatusRequest) (app.AuthStatusResult, error) {
	stub.statuses = append(stub.statuses, r)
	return app.AuthStatusResult{}, nil
}
func (stub *authStub) Import(_ context.Context, r app.AuthImportRequest) (app.AuthImportResult, error) {
	stub.imports = append(stub.imports, r)
	return app.AuthImportResult{Agent: harness.Name(r.Agent), Digest: "digest"}, nil
}
func (stub *authStub) Login(_ context.Context, r app.AuthLoginRequest) (app.AuthLoginResult, error) {
	stub.logins = append(stub.logins, r)
	code := 0
	return app.AuthLoginResult{Agent: harness.Name(r.Agent), Exit: runtime.Exit{Code: &code}}, nil
}
func (stub *authStub) Refresh(_ context.Context, r app.AuthRefreshRequest) (app.AuthImportResult, error) {
	stub.refreshes = append(stub.refreshes, r)
	return app.AuthImportResult{Agent: harness.Name(r.Agent), Digest: "digest"}, nil
}
func (stub *authStub) Purge(_ context.Context, r app.AuthPurgeRequest) error {
	stub.purges = append(stub.purges, r)
	return nil
}

type tuiRunnerStub struct {
	intents        []tui.Intent
	progress       []tui.ProgressRequest
	progressEvents []string
	events         *[]string
	progressErr    error
	runs           int
}

func (stub *tuiRunnerStub) Run(context.Context, tui.RunRequest) (tui.Intent, bool, error) {
	stub.runs++
	if stub.events != nil {
		*stub.events = append(*stub.events, "run")
	}
	if len(stub.intents) == 0 {
		return tui.Intent{}, false, nil
	}
	intent := stub.intents[0]
	stub.intents = stub.intents[1:]
	return intent, true, nil
}

func (stub *tuiRunnerStub) RunProgress(ctx context.Context, request tui.ProgressRequest, operation tui.ProgressOperation) error {
	stub.progress = append(stub.progress, request)
	if stub.events != nil {
		*stub.events = append(*stub.events, "progress-start")
	}
	if stub.progressErr != nil {
		return stub.progressErr
	}
	err := operation(ctx, func(id string) {
		stub.progressEvents = append(stub.progressEvents, id)
		if stub.events != nil {
			*stub.events = append(*stub.events, id)
		}
	})
	if stub.events != nil {
		*stub.events = append(*stub.events, "progress-end")
	}
	return err
}

func TestWorkspaceCommandsTargetExactNamedWorkspace(t *testing.T) {
	code := 0
	workspaces := &workspaceStub{result: app.WorkspaceResult{Workspace: "feature-a", State: model.StateRunning}, openResult: app.WorkspaceOpenResult{WorkspaceResult: app.WorkspaceResult{Workspace: "feature-a"}, Exit: runtime.Exit{Code: &code}}}
	git := &gitStub{result: app.WorkspaceResult{Workspace: "feature-a"}}
	dispatcher := NewDispatcher(Dependencies{Workspaces: workspaces, Git: git, Stdin: strings.NewReader(""), IsTTY: func(io.Reader, io.Writer) bool { return true }})
	commands := [][]string{
		{"workspace", "create", "feature-a", "--root", "/project", "--default-agent", "codex"},
		{"workspace", "open", "feature-a", "--root", "/project"},
		{"workspace", "start", "feature-a", "--root", "/project"},
		{"workspace", "stop", "feature-a", "--root", "/project"},
		{"workspace", "restart", "feature-a", "--root", "/project"},
		{"workspace", "update", "feature-a", "--root", "/project"},
		{"workspace", "remove", "feature-a", "--root", "/project", "--force"},
	}
	for _, command := range commands {
		var stdout, stderr bytes.Buffer
		if exit := dispatcher.Execute(context.Background(), command, &stdout, &stderr); exit != 0 {
			t.Fatalf("%q exit=%d stderr=%q", command, exit, stderr.String())
		}
	}
	if len(workspaces.creates) != 1 || workspaces.creates[0].Workspace != "feature-a" || workspaces.creates[0].Root != "/project" || workspaces.creates[0].DefaultAgent != "codex" {
		t.Fatalf("create requests=%#v", workspaces.creates)
	}
	if len(workspaces.opens) != 1 || workspaces.opens[0].Workspace != "feature-a" || len(workspaces.starts) != 1 || workspaces.starts[0].Workspace != "feature-a" || len(workspaces.stops) != 1 || workspaces.stops[0].Workspace != "feature-a" || len(workspaces.restarts) != 1 || workspaces.restarts[0].Workspace != "feature-a" {
		t.Fatalf("named lifecycle requests not exact: %#v %#v %#v %#v", workspaces.opens, workspaces.starts, workspaces.stops, workspaces.restarts)
	}
	if len(git.updates) != 1 || git.updates[0].Workspace != "feature-a" || len(workspaces.removes) != 1 || workspaces.removes[0].Workspace != "feature-a" || !workspaces.removes[0].Confirmed || !workspaces.removes[0].DiscardUnfetched {
		t.Fatalf("update=%#v remove=%#v", git.updates, workspaces.removes)
	}
}

func TestTUICreateAndOpenKeepsProgressUntilInteractiveHandoff(t *testing.T) {
	code := 0
	events := []string{}
	runner := &tuiRunnerStub{
		intents: []tui.Intent{{
			Action: "workspace-create", Root: "/project", Workspace: "feature-a",
			SourceBranch: "main", SourceRevision: strings.Repeat("a", 40), Agent: "codex", Open: true,
		}},
		events: &events,
	}
	workspaces := &workspaceStub{
		result:     app.WorkspaceResult{Workspace: "feature-a", State: model.StateRunning},
		openResult: app.WorkspaceOpenResult{WorkspaceResult: app.WorkspaceResult{Workspace: "feature-a"}, Exit: runtime.Exit{Code: &code}},
		events:     &events,
	}
	dispatcher := NewDispatcher(Dependencies{
		TUI: runner, Workspaces: workspaces, Stdin: strings.NewReader(""),
		IsTTY: func(io.Reader, io.Writer) bool { return true },
	})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.executeTUI(context.Background(), tui.RunRequest{Root: "/project"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	wantEvents := "run,progress-start,validate,workspace,create,ready,progress-end,open"
	if got := strings.Join(events, ","); got != wantEvents {
		t.Fatalf("events = %q, want %q", got, wantEvents)
	}
	if len(workspaces.creates) != 1 || workspaces.creates[0].Open || workspaces.creates[0].RunInteractive != nil {
		t.Fatalf("create request = %#v", workspaces.creates)
	}
	if len(workspaces.opens) != 1 || !workspaces.opens[0].Terminal || workspaces.opens[0].RunInteractive == nil {
		t.Fatalf("open request = %#v", workspaces.opens)
	}
	if stdout.Len() != 0 {
		t.Fatalf("TUI create-and-open rendered lifecycle summary before handoff: %q", stdout.String())
	}
	if len(runner.progress) != 1 || strings.Join(runner.progressEvents, ",") != "validate,workspace,ready" {
		t.Fatalf("progress requests = %#v events = %#v", runner.progress, runner.progressEvents)
	}
}

func TestTUICreateFailureNeverOpensWorkspace(t *testing.T) {
	events := []string{}
	runner := &tuiRunnerStub{
		intents: []tui.Intent{{Action: "workspace-create", Root: "/project", Workspace: "broken", Open: true}},
		events:  &events,
	}
	workspaces := &workspaceStub{createErr: errors.New("injected create failure"), events: &events}
	dispatcher := NewDispatcher(Dependencies{
		TUI: runner, Workspaces: workspaces, Stdin: strings.NewReader(""),
		IsTTY: func(io.Reader, io.Writer) bool { return true },
	})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.executeTUI(context.Background(), tui.RunRequest{Root: "/project"}, &stdout, &stderr); exit == 0 {
		t.Fatalf("failure exit = 0, stderr = %q", stderr.String())
	}
	if len(workspaces.opens) != 0 || strings.Contains(strings.Join(events, ","), "ready") {
		t.Fatalf("failed create events = %#v opens = %#v", events, workspaces.opens)
	}
}

func TestTUICreateCancellationNeverCreatesOrOpensWorkspace(t *testing.T) {
	runner := &tuiRunnerStub{
		intents:     []tui.Intent{{Action: "workspace-create", Root: "/project", Workspace: "cancelled", Open: true}},
		progressErr: context.Canceled,
	}
	workspaces := &workspaceStub{}
	dispatcher := NewDispatcher(Dependencies{
		TUI: runner, Workspaces: workspaces, Stdin: strings.NewReader(""),
		IsTTY: func(io.Reader, io.Writer) bool { return true },
	})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.executeTUI(context.Background(), tui.RunRequest{Root: "/project"}, &stdout, &stderr); exit == 0 {
		t.Fatalf("cancellation exit = 0, stderr = %q", stderr.String())
	}
	if len(workspaces.creates) != 0 || len(workspaces.opens) != 0 {
		t.Fatalf("cancelled create requests = %#v, opens = %#v", workspaces.creates, workspaces.opens)
	}
}

func TestTUIBackgroundCreateCompletesWithoutOpeningWorkspace(t *testing.T) {
	events := []string{}
	runner := &tuiRunnerStub{
		intents: []tui.Intent{{Action: "workspace-create", Root: "/project", Workspace: "background"}},
		events:  &events,
	}
	workspaces := &workspaceStub{events: &events}
	dispatcher := NewDispatcher(Dependencies{
		TUI: runner, Workspaces: workspaces, Stdin: strings.NewReader(""),
		IsTTY: func(io.Reader, io.Writer) bool { return true },
	})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.executeTUI(context.Background(), tui.RunRequest{Root: "/project"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if runner.runs != 1 || len(workspaces.opens) != 0 {
		t.Fatalf("dashboard runs = %d, opens = %#v", runner.runs, workspaces.opens)
	}
	wantEvents := "run,progress-start,validate,workspace,create,ready,progress-end"
	if got := strings.Join(events, ","); got != wantEvents {
		t.Fatalf("events = %q, want %q", got, wantEvents)
	}
}

func TestAllNamedMutationRoutesRejectMissingOrInvalidWorkspaceBeforeService(t *testing.T) {
	for _, args := range [][]string{
		{"workspace", "create"}, {"workspace", "open"}, {"workspace", "start"}, {"workspace", "stop"}, {"workspace", "restart"}, {"workspace", "update"}, {"workspace", "remove"},
		{"agent"}, {"git", "status"}, {"git", "diff"}, {"git", "fetch"}, {"git", "apply"},
		{"workspace", "start", "Upper"}, {"agent", "../bad"}, {"git", "fetch", "-bad"},
	} {
		workspaces, git, agents := &workspaceStub{}, &gitStub{}, &agentStub{}
		dispatcher := NewDispatcher(Dependencies{Workspaces: workspaces, Git: git, Agents: agents})
		var stdout, stderr bytes.Buffer
		if exit := dispatcher.Execute(context.Background(), args, &stdout, &stderr); exit != 2 {
			t.Fatalf("%q exit=%d stderr=%q", args, exit, stderr.String())
		}
		if workspaces.calls() != 0 || git.calls() != 0 || len(agents.requests) != 0 {
			t.Fatalf("%q mutated service", args)
		}
		if stdout.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("%q stdout=%q stderr=%q", args, stdout.String(), stderr.String())
		}
	}
}

func TestAgentRequiresPromptSeparatorAndPassesExactRequest(t *testing.T) {
	code := 37
	agents := &agentStub{result: app.AgentRunResult{Agent: harness.Codex, Exit: runtime.Exit{Code: &code}}}
	dispatcher := NewDispatcher(Dependencies{Agents: agents, Stdin: strings.NewReader("input")})
	var stdout, stderr bytes.Buffer
	args := []string{"agent", "feature-a", "--root", "/project", "--agent", "codex", "--browser", "--", "--literal prompt $(not-shell)"}
	if exit := dispatcher.Execute(context.Background(), args, &stdout, &stderr); exit != 37 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	if len(agents.requests) != 1 {
		t.Fatalf("requests=%d", len(agents.requests))
	}
	request := agents.requests[0]
	if request.Root != "/project" || request.Workspace != "feature-a" || request.Agent != "codex" || !request.Browser || request.Prompt != "--literal prompt $(not-shell)" {
		t.Fatalf("request=%#v", request)
	}
	for _, bad := range [][]string{{"agent", "feature-a", "prompt"}, {"agent", "feature-a", "--"}, {"agent", "feature-a", "--", "one", "two"}} {
		var out, errout bytes.Buffer
		if exit := dispatcher.Execute(context.Background(), bad, &out, &errout); exit != 2 {
			t.Fatalf("%q exit=%d", bad, exit)
		}
	}
	if len(agents.requests) != 1 {
		t.Fatalf("invalid prompts called service: %d", len(agents.requests))
	}
}

func TestAuthCommandsAreExplicitAndPurgeRequiresConfirmation(t *testing.T) {
	authentication := &authStub{}
	dispatcher := NewDispatcher(Dependencies{Auth: authentication, Stdin: strings.NewReader("no\n"), IsTTY: func(io.Reader, io.Writer) bool { return true }})
	for _, args := range [][]string{{"auth", "import", "--agent", "omp", "--root", "/project"}, {"auth", "refresh", "--agent", "codex", "--root", "/project"}} {
		var stdout, stderr bytes.Buffer
		if exit := dispatcher.Execute(context.Background(), args, &stdout, &stderr); exit != 0 {
			t.Fatalf("%q exit=%d stderr=%q", args, exit, stderr.String())
		}
	}
	if len(authentication.imports) != 1 || !authentication.imports[0].Approved || len(authentication.refreshes) != 1 || !authentication.refreshes[0].Approved {
		t.Fatalf("import=%#v refresh=%#v", authentication.imports, authentication.refreshes)
	}
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), []string{"auth", "purge", "--agent", "omp"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("purge cancel exit=%d", exit)
	}
	if len(authentication.purges) != 0 {
		t.Fatal("cancelled purge called service")
	}
	stdout.Reset()
	stderr.Reset()
	if exit := dispatcher.Execute(context.Background(), []string{"auth", "purge", "--agent", "omp", "--force"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("forced purge exit=%d stderr=%q", exit, stderr.String())
	}
	if len(authentication.purges) != 1 || !authentication.purges[0].Approved {
		t.Fatalf("purges=%#v", authentication.purges)
	}
}

func TestGitCommandsPassExactWorkspaceAndRepository(t *testing.T) {
	git := &gitStub{}
	dispatcher := NewDispatcher(Dependencies{Git: git})
	for _, operation := range []string{"status", "diff", "fetch", "apply"} {
		var stdout, stderr bytes.Buffer
		args := []string{"git", operation, "feature-a", "--root", "/project", "--repo", "api", "--format", "json"}
		if exit := dispatcher.Execute(context.Background(), args, &stdout, &stderr); exit != 0 {
			t.Fatalf("%s exit=%d stderr=%q", operation, exit, stderr.String())
		}
	}
	if len(git.statuses) != 1 || git.statuses[0].Workspace != "feature-a" || git.statuses[0].Repository != "api" || len(git.diffs) != 1 || git.diffs[0].MaxBytes != maxGitDiffBytes || len(git.fetches) != 1 || git.fetches[0].Root != "/project" || len(git.applies) != 1 {
		t.Fatalf("git requests: %#v %#v %#v %#v", git.statuses, git.diffs, git.fetches, git.applies)
	}
}

type inventoryStub struct{ manifests []state.Manifest }

func (stub inventoryStub) ListAllManifests(context.Context) ([]state.Manifest, error) {
	return append([]state.Manifest(nil), stub.manifests...), nil
}

func TestAllProjectsCleanupDispatchesExactManifestRootsAndNames(t *testing.T) {
	workspaces := &workspaceStub{}
	inventory := inventoryStub{manifests: []state.Manifest{
		{CanonicalRoot: "/project-b", Workspace: "tests", RunID: "run-b", State: model.StateStopped},
		{CanonicalRoot: "/project-a", Workspace: "feature-a", RunID: "run-a", State: model.StateRunning},
	}}
	dispatcher := NewDispatcher(Dependencies{Workspaces: workspaces, Inventory: inventory})
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.Execute(context.Background(), []string{"workspace", "remove", "--all-projects", "--force"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, stderr = %q", exit, stderr.String())
	}
	if len(workspaces.removes) != 2 ||
		workspaces.removes[0].Root != "/project-a" || workspaces.removes[0].Workspace != "feature-a" ||
		workspaces.removes[1].Root != "/project-b" || workspaces.removes[1].Workspace != "tests" {
		t.Fatalf("remove requests = %#v", workspaces.removes)
	}
}

func TestDashboardLoadsCleanAndDirtyGitCheckoutSummary(t *testing.T) {
	root := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", root}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	runGit("init", "-b", "main")
	runGit("config", "user.email", "test@example.invalid")
	runGit("config", "user.name", "DSX Test")
	tracked := filepath.Join(root, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("clean\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "tracked.txt")
	workspaces := &workspaceStub{listResult: app.WorkspaceListResult{Workspaces: []app.WorkspaceSummary{{Workspace: "feature-a", State: model.StateStopped}}}}
	dispatcher := NewDispatcher(Dependencies{Inspector: app.NewInspectionService(plan.NewResolver()), Workspaces: workspaces})
	runGit("commit", "-m", "initial")

	clean, err := dispatcher.loadDashboard(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if clean.Branch != "main" || clean.Revision == "" || !clean.Clean {
		t.Fatalf("clean dashboard = %#v", clean)
	}
	cleanModel := tui.NewDashboardModel(clean)
	updated, _ := cleanModel.Update(tea.KeyPressMsg(tea.Key{Text: "c", Code: 'c'}))
	if !strings.Contains(updated.(*tui.DashboardModel).View().Content, "Starting point") {
		t.Fatal("clean checkout did not open create form")
	}
	updateModel := tui.NewDashboardModel(clean)
	updateModel.Update(tea.KeyPressMsg(tea.Key{Text: "u", Code: 'u'}))
	if intent, found := updateModel.Intent(); !found || intent.Action != "workspace-update" || intent.Workspace != "feature-a" {
		t.Fatalf("clean update intent = %#v, found = %t", intent, found)
	}

	if err := os.WriteFile(tracked, []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirty, err := dispatcher.loadDashboard(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if dirty.Branch != "main" || dirty.Revision != clean.Revision || dirty.Clean {
		t.Fatalf("dirty dashboard = %#v", dirty)
	}
	dirtyModel := tui.NewDashboardModel(dirty)
	updated, _ = dirtyModel.Update(tea.KeyPressMsg(tea.Key{Text: "c", Code: 'c'}))
	if strings.Contains(updated.(*tui.DashboardModel).View().Content, "Starting point") || !strings.Contains(updated.(*tui.DashboardModel).View().Content, "Create unavailable") {
		t.Fatal("dirty checkout did not gate create")
	}
	dirtyModel.Update(tea.KeyPressMsg(tea.Key{Text: "u", Code: 'u'}))
	if intent, found := dirtyModel.Intent(); found {
		t.Fatalf("dirty checkout emitted intent %#v", intent)
	}

	runGit("add", "tracked.txt")
	runGit("commit", "-m", "advance after dashboard load")
	var stdout, stderr bytes.Buffer
	exit := dispatcher.executeIntent(context.Background(), tui.Intent{
		Action: "workspace-create", Root: root, Workspace: "feature-b",
		SourceBranch: clean.Branch, SourceRevision: clean.Revision,
	}, &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("create exit = %d, stderr = %q", exit, stderr.String())
	}
	if len(workspaces.creates) != 1 || workspaces.creates[0].SourceBranch != clean.Branch || workspaces.creates[0].SourceRevision != clean.Revision {
		t.Fatalf("create request after HEAD advance = %#v", workspaces.creates)
	}
}

func TestDashboardLoadsNonSecretAWSWorkspaceStatus(t *testing.T) {
	inspector := &fakeInspector{result: app.InspectResult{
		Facts: app.ProjectFacts{CanonicalRoot: "/tmp/project", Branch: "main", Revision: "abc123", Clean: true},
		Plan: plan.ExecutionPlan{
			AWS:    plan.AWSCapability{Mode: plan.AWSModeHostDefault},
			Agents: plan.AgentPlan{Default: "codex", Allowed: []string{"codex"}},
		},
	}}
	workspaces := &workspaceStub{listResult: app.WorkspaceListResult{Workspaces: []app.WorkspaceSummary{{
		Workspace: "feature-a", State: model.StateRunning, DefaultAgent: "codex",
	}}}}
	aws := &awsWorkspaceStub{statusResult: app.AWSWorkspaceResult{
		Workspace: "feature-a", Enabled: true, HostAvailability: app.AWSHostAvailable, MirrorHealth: app.AWSMirrorCurrent,
	}}
	dispatcher := NewDispatcher(Dependencies{Inspector: inspector, Workspaces: workspaces, AWS: aws})
	data, err := dispatcher.loadDashboard(context.Background(), "/tmp/project")
	if err != nil {
		t.Fatal(err)
	}
	if data.AWSCapability != plan.AWSModeHostDefault || len(data.Workspaces) != 1 {
		t.Fatalf("AWS dashboard data = %#v", data)
	}
	entry := data.Workspaces[0]
	if !entry.AWSEnabled || entry.AWSHostAvailability != app.AWSHostAvailable || entry.AWSMirrorHealth != app.AWSMirrorCurrent || len(aws.statuses) != 1 {
		t.Fatalf("AWS workspace status = %#v, requests = %#v", entry, aws.statuses)
	}

	aws.statusErr = errors.New("raw-host-file-secret")
	data, err = dispatcher.loadDashboard(context.Background(), "/tmp/project")
	if err != nil {
		t.Fatal(err)
	}
	entry = data.Workspaces[0]
	if entry.AWSEnabled || entry.AWSHostAvailability != app.AWSHostUnavailable || entry.AWSMirrorHealth != app.AWSMirrorDegraded || entry.AWSFailureCode != "status-unavailable" {
		t.Fatalf("safe AWS status failure = %#v", entry)
	}
	if strings.Contains(tui.NewDashboardModel(data).View().Content, "raw-host-file-secret") {
		t.Fatal("dashboard exposed raw AWS status error")
	}
}

func TestExecuteAWSDashboardIntentsUseApplicationServiceAndRenderStatus(t *testing.T) {
	aws := &awsWorkspaceStub{
		enableResult:  app.AWSWorkspaceResult{Workspace: "feature-a", Enabled: true, HostAvailability: app.AWSHostAvailable, MirrorHealth: app.AWSMirrorCurrent},
		disableResult: app.AWSWorkspaceResult{Workspace: "feature-a", HostAvailability: app.AWSHostUnavailable, MirrorHealth: app.AWSMirrorDisabled},
	}
	dispatcher := NewDispatcher(Dependencies{AWS: aws})
	for _, action := range []string{"aws-enable", "aws-disable"} {
		var stdout, stderr bytes.Buffer
		exit := dispatcher.executeIntent(context.Background(), tui.Intent{Action: action, Root: "/tmp/project", Workspace: "feature-a"}, &stdout, &stderr)
		if exit != 0 || stderr.Len() != 0 {
			t.Fatalf("%s exit = %d, stdout = %q, stderr = %q", action, exit, stdout.String(), stderr.String())
		}
		for _, expected := range []string{`Workspace: "feature-a"`, "Host availability:", "Mirror health:"} {
			if !strings.Contains(stdout.String(), expected) {
				t.Fatalf("%s output omitted %q: %q", action, expected, stdout.String())
			}
		}
	}
	if len(aws.enables) != 1 || len(aws.disables) != 1 || aws.enables[0].Workspace != "feature-a" || aws.disables[0].Workspace != "feature-a" {
		t.Fatalf("AWS application requests = enable %#v, disable %#v", aws.enables, aws.disables)
	}

	aws.enableErr = errors.New("safe unavailable")
	var stdout, stderr bytes.Buffer
	if exit := dispatcher.executeIntent(context.Background(), tui.Intent{Action: "aws-enable", Root: "/tmp/project", Workspace: "feature-a"}, &stdout, &stderr); exit == 0 || !strings.Contains(stderr.String(), "safe unavailable") {
		t.Fatalf("AWS application error exit/output = %d, %q", exit, stderr.String())
	}
}
