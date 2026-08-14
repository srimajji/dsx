package tui

import (
	"bytes"
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/terminal"
)

func dashboardFixture(state string) DashboardData {
	return DashboardData{
		Root: "/tmp/tracking-chrome-extension", Branch: "feat/branch-1", Revision: "abc123", Clean: true,
		AllowedAgents: []string{"omp", "codex"}, DefaultAgent: "omp",
		Workspaces: []DashboardWorkspace{{Name: "feature-a", State: state}},
	}
}

func dashboardPress(t *testing.T, model *DashboardModel, key tea.KeyPressMsg) (*DashboardModel, tea.Cmd) {
	t.Helper()
	updated, command := model.Update(key)
	result, ok := updated.(*DashboardModel)
	if !ok {
		t.Fatalf("Update() returned %T", updated)
	}
	return result, command
}

func textKey(value string) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Text: value, Code: []rune(value)[0]})
}

func specialKey(code rune) tea.KeyPressMsg { return tea.KeyPressMsg(tea.Key{Code: code}) }

func TestDashboardRendersPeerWorkspaceStatesAndAgents(t *testing.T) {
	data := dashboardFixture("running")
	data.Workspaces = []DashboardWorkspace{
		{Name: "tests", State: "needs_resolution", DefaultAgent: "codex"},
		{Name: "feature-b", State: "stopped", DefaultAgent: "codex"},
		{Name: "feature-a", State: "running"},
	}
	view := ansi.Strip(NewDashboardModel(data).View().Content)
	for _, expected := range []string{
		"Local checkout", "feat/branch-1 @ abc123", "Clean", "feature-a", "Running",
		"feature-b", "Stopped", "tests", "Needs resolution", "Default: OMP (project default)",
		"Default: Codex", "Approved: Codex · OMP",
	} {
		if !strings.Contains(view, expected) {
			t.Fatalf("dashboard omitted %q:\n%s", expected, view)
		}
	}
	if strings.Index(view, "feature-a") > strings.Index(view, "feature-b") || strings.Index(view, "feature-b") > strings.Index(view, "tests") {
		t.Fatalf("workspaces are not deterministically sorted:\n%s", view)
	}
}

func TestDashboardActionsAreStateAware(t *testing.T) {
	tests := []struct {
		state   string
		present []string
		absent  []string
	}{
		{state: "running", present: []string{"[Enter] Open", "[a] Open agent", "[u] Update", "[s] Stop", "[r] Restart", "[g] Review Git", "[d] Remove"}},
		{state: "stopped", present: []string{"[Enter] Open", "[u] Update", "[s] Start", "[r] Restart", "[g] Review Git", "[d] Remove"}, absent: []string{"[a] Open agent"}},
		{state: "needs_resolution", present: []string{"[Enter] Open", "[s] Stop", "[g] Review Git", "[d] Remove"}, absent: []string{"[a] Open agent", "[u] Update", "[r] Restart"}},
		{state: "failed", present: []string{"[g] Review Git", "[d] Remove"}, absent: []string{"[Enter] Open", "[a] Open agent", "[u] Update", "[s] Start", "[r] Restart"}},
		{state: "planned", present: []string{"Update unavailable", "Restart unavailable"}, absent: []string{"[Enter] Open", "[a] Open agent", "[s] Start", "[g] Review Git", "[d] Remove"}},
		{state: "creating", present: []string{"Update unavailable", "Restart unavailable"}, absent: []string{"[Enter] Open", "[a] Open agent", "[s] Start", "[g] Review Git", "[d] Remove"}},
		{state: "cleaning", present: []string{"Update unavailable", "Restart unavailable"}, absent: []string{"[Enter] Open", "[a] Open agent", "[s] Start", "[g] Review Git", "[d] Remove"}},
	}
	for _, test := range tests {
		t.Run(test.state, func(t *testing.T) {
			view := ansi.Strip(NewDashboardModel(dashboardFixture(test.state)).View().Content)
			for _, expected := range test.present {
				if !strings.Contains(view, expected) {
					t.Fatalf("%s view omitted %q:\n%s", test.state, expected, view)
				}
			}
			for _, unexpected := range test.absent {
				if strings.Contains(view, unexpected) {
					t.Fatalf("%s view included %q:\n%s", test.state, unexpected, view)
				}
			}
		})
	}
}

func TestDashboardOmitsDeletedWorkspace(t *testing.T) {
	data := dashboardFixture("deleted")
	model := NewDashboardModel(data)
	view := ansi.Strip(model.View().Content)
	if len(model.data.Workspaces) != 0 || strings.Contains(view, "feature-a") {
		t.Fatalf("deleted workspace remained visible: %#v\n%s", model.data.Workspaces, view)
	}
	if !strings.Contains(view, "No workspaces yet") {
		t.Fatalf("deleted-only dashboard lacked empty guidance:\n%s", view)
	}
}

func TestDashboardDisablesUpdateAndRestartDuringMutation(t *testing.T) {
	data := dashboardFixture("running")
	data.Workspaces[0].MutationActive = true
	model := NewDashboardModel(data)
	view := ansi.Strip(model.View().Content)
	for _, expected := range []string{"Update unavailable while lifecycle change runs", "Restart unavailable while lifecycle change runs"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("mutating view omitted %q:\n%s", expected, view)
		}
	}
	for _, key := range []string{"u", "r"} {
		updated, command := dashboardPress(t, NewDashboardModel(data), textKey(key))
		if command != nil {
			t.Fatalf("disabled %q action exited", key)
		}
		if intent, found := updated.Intent(); found {
			t.Fatalf("disabled %q emitted %#v", key, intent)
		}
	}
}

func TestDashboardAllowsReviewedCreateButBlocksOrdinaryUpdateForDirtyCheckout(t *testing.T) {
	data := dashboardFixture("running")
	data.Clean = false
	model := NewDashboardModel(data)
	view := ansi.Strip(model.View().Content)
	normalizedView := strings.Join(strings.Fields(strings.ReplaceAll(view, "│", " ")), " ")
	for _, expected := range []string{
		"Not clean — ordinary create/update require a commit; reviewed snapshots are available.",
		"[c] Create workspace",
		"Update unavailable — use dsx workspace update NAME --snapshot",
	} {
		if !strings.Contains(normalizedView, expected) {
			t.Fatalf("dirty checkout view omitted %q:\nnormalized=%q\n%s", expected, normalizedView, view)
		}
	}
	if strings.Contains(normalizedView, "Create unavailable") {
		t.Fatalf("dirty checkout incorrectly disabled reviewed create:\n%s", view)
	}
	updated, command := dashboardPress(t, model, textKey("c"))
	if command != nil || updated.screen != dashboardCreate {
		t.Fatalf("dirty checkout create did not open reviewed form: screen=%d command=%v", updated.screen, command)
	}
	updated, command = dashboardPress(t, NewDashboardModel(data), textKey("u"))
	if command != nil {
		t.Fatal("dirty ordinary update exited")
	}
	if intent, found := updated.Intent(); found {
		t.Fatalf("dirty ordinary update emitted %#v", intent)
	}
}

func TestDashboardFailsClosedWhenCheckoutIdentityIsUnavailable(t *testing.T) {
	data := dashboardFixture("running")
	data.Branch, data.Revision = "", ""
	model := NewDashboardModel(data)
	view := ansi.Strip(model.View().Content)
	for _, expected := range []string{"Local checkout", "Unavailable", "Source branch or revision unavailable", "Create unavailable", "Update unavailable"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("missing checkout view omitted %q:\n%s", expected, view)
		}
	}
	for _, key := range []string{"c", "u"} {
		updated, command := dashboardPress(t, model, textKey(key))
		if command != nil || updated.screen != dashboardHome {
			t.Fatalf("missing checkout %q action escaped fail-closed state", key)
		}
		if intent, found := updated.Intent(); found {
			t.Fatalf("missing checkout %q emitted %#v", key, intent)
		}
	}
}

func TestCreateFormContainsOnlyWorkspaceInputsAndEmitsBothActions(t *testing.T) {
	for _, open := range []bool{true, false} {
		model := NewDashboardModel(dashboardFixture("running"))
		model, _ = dashboardPress(t, model, textKey("c"))
		view := ansi.Strip(model.View().Content)
		for _, expected := range []string{"Create workspace", "Name", "Starting point", "feat/branch-1 @ abc123", "Default agent", "OMP — inherited from project", "Snapshot local changes", "Create and open", "Create in background"} {
			if !strings.Contains(view, expected) {
				t.Fatalf("create form omitted %q:\n%s", expected, view)
			}
		}

		for _, forbidden := range []string{"Authentication", "Prompt", "Browser", "Workspace mode", "Live", "Clone"} {
			if strings.Contains(strings.ToLower(view), strings.ToLower(forbidden)) {
				t.Fatalf("create form included forbidden %q:\n%s", forbidden, view)
			}
		}
		for _, r := range "feature-new" {
			model, _ = dashboardPress(t, model, textKey(string(r)))
		}
		model, _ = dashboardPress(t, model, specialKey(tea.KeyTab))
		model, _ = dashboardPress(t, model, specialKey(tea.KeyTab))
		model, _ = dashboardPress(t, model, specialKey(tea.KeyTab))
		if !open {
			model, _ = dashboardPress(t, model, specialKey(tea.KeyTab))
		}
		model, command := dashboardPress(t, model, specialKey(tea.KeyEnter))
		intent, found := model.Intent()
		want := Intent{
			Action: "workspace-create", Root: "/tmp/tracking-chrome-extension", Workspace: "feature-new",
			SourceBranch: "feat/branch-1", SourceRevision: "abc123", Agent: "omp", Open: open,
		}
		if command == nil || !found || intent != want {
			t.Fatalf("create intent = %#v, found=%t, command=%v; want %#v", intent, found, command, want)
		}
	}
}

func TestSnapshotCreateRequiresExplicitReviewAndPreservesFormOnBack(t *testing.T) {
	data := dashboardFixture("running")
	data.Clean = false
	model := NewDashboardModel(data)
	model, _ = dashboardPress(t, model, textKey("c"))
	for _, r := range "dirty-work" {
		model, _ = dashboardPress(t, model, textKey(string(r)))
	}
	for range 2 {
		model, _ = dashboardPress(t, model, specialKey(tea.KeyTab))
	}
	model, _ = dashboardPress(t, model, textKey(" "))
	model, _ = dashboardPress(t, model, specialKey(tea.KeyTab))
	model, command := dashboardPress(t, model, specialKey(tea.KeyEnter))
	if command != nil || model.screen != dashboardCreateSnapshotReview {
		t.Fatalf("snapshot create skipped review: screen=%d command=%v", model.screen, command)
	}
	if _, found := model.Intent(); found {
		t.Fatal("snapshot review emitted an intent before confirmation")
	}
	view := ansi.Strip(model.View().Content)
	normalizedReview := strings.Join(strings.Fields(strings.ReplaceAll(view, "│", " ")), " ")
	for _, expected := range []string{
		"Review source snapshot",
		"dirty-work",
		"feat/branch-1 @ abc123",
		"Includes final tracked file content and nonignored untracked files.",
		"Ignored untracked files stay on the host; tracked files remain included.",
		"Unmerged paths and Git submodules are rejected.",
		"Your branch, HEAD, index, worktree, and durable refs are not changed.",
	} {
		if !strings.Contains(normalizedReview, expected) {
			t.Fatalf("snapshot review omitted %q:\nnormalized=%q\n%s", expected, normalizedReview, view)
		}
	}
	model, _ = dashboardPress(t, model, specialKey(tea.KeyEscape))
	if model.screen != dashboardCreate || model.name != "dirty-work" || !model.snapshot || !model.pendingCreateOpen {
		t.Fatalf("review back lost form state: %#v", model)
	}
	model, _ = dashboardPress(t, model, specialKey(tea.KeyTab))
	model, _ = dashboardPress(t, model, specialKey(tea.KeyEnter))
	model, command = dashboardPress(t, model, textKey("y"))
	want := Intent{
		Action: "workspace-create", Root: data.Root, Workspace: "dirty-work",
		SourceBranch: data.Branch, SourceRevision: data.Revision,
		Snapshot: true, Agent: "omp", Open: true,
	}
	intent, found := model.Intent()
	if command == nil || !found || intent != want {
		t.Fatalf("snapshot intent = %#v, found=%t, command=%v; want %#v", intent, found, command, want)
	}
}

func TestDirtyCreateWithoutSnapshotStaysInForm(t *testing.T) {
	data := dashboardFixture("running")
	data.Clean = false
	model := NewDashboardModel(data)
	model, _ = dashboardPress(t, model, textKey("c"))
	for _, r := range "dirty-work" {
		model, _ = dashboardPress(t, model, textKey(string(r)))
	}
	model.focus = 3
	model, command := dashboardPress(t, model, specialKey(tea.KeyEnter))
	if command != nil || model.screen != dashboardCreate {
		t.Fatalf("unchecked dirty create left form: screen=%d command=%v", model.screen, command)
	}
	if _, found := model.Intent(); found {
		t.Fatal("unchecked dirty create emitted an intent")
	}
	if view := ansi.Strip(model.View().Content); !strings.Contains(view, "Select Snapshot local changes to create from a dirty checkout.") {
		t.Fatalf("unchecked dirty create omitted guidance:\n%s", view)
	}
}
func TestDashboardVSCodeAttachOnlyForRunningWorkspace(t *testing.T) {
	data := dashboardFixture("running")
	data.Workspaces = []DashboardWorkspace{{Name: "running", State: "running"}, {Name: "stopped", State: "stopped"}}
	model := NewDashboardModel(data)
	if view := ansi.Strip(model.View().Content); !strings.Contains(view, "[v] Attach with VS Code (experimental)") {
		t.Fatalf("running actions omit VS Code attach: %s", view)
	}
	updated, cmd := model.Update(tea.KeyPressMsg{Code: 'v', Text: "v"})
	if updated.(*DashboardModel).intent == nil || updated.(*DashboardModel).intent.Action != "vscode-attach" || cmd == nil {
		t.Fatalf("running v intent = %#v", updated.(*DashboardModel).intent)
	}
	model = NewDashboardModel(data)
	model.selected = 1
	if view := ansi.Strip(model.View().Content); strings.Contains(view, "[v] Attach with VS Code (experimental)") {
		t.Fatalf("stopped actions expose VS Code attach: %s", view)
	}
}

func TestCreateFormSelectsOnlyApprovedDefaultAgents(t *testing.T) {
	model := NewDashboardModel(dashboardFixture("running"))
	model, _ = dashboardPress(t, model, textKey("c"))
	for _, r := range "selected-agent" {
		model, _ = dashboardPress(t, model, textKey(string(r)))
	}
	model, _ = dashboardPress(t, model, specialKey(tea.KeyTab))
	model, _ = dashboardPress(t, model, specialKey(tea.KeyRight))
	if view := ansi.Strip(model.View().Content); !strings.Contains(view, "Codex") {
		t.Fatalf("approved agent selector did not move to Codex:\n%s", view)
	}
	model, _ = dashboardPress(t, model, specialKey(tea.KeyTab))
	model, _ = dashboardPress(t, model, specialKey(tea.KeyTab))
	model, command := dashboardPress(t, model, specialKey(tea.KeyEnter))
	intent, found := model.Intent()
	if command == nil || !found || intent.Agent != "codex" {
		t.Fatalf("selected default agent intent = %#v, found=%t", intent, found)
	}
}

func TestCreateFormValidatesNameAndCancellationIsSideEffectFree(t *testing.T) {
	model := NewDashboardModel(dashboardFixture("running"))
	model, _ = dashboardPress(t, model, textKey("c"))
	for _, r := range "Bad-" {
		model, _ = dashboardPress(t, model, textKey(string(r)))
	}
	model.focus = 3
	model, command := dashboardPress(t, model, specialKey(tea.KeyEnter))
	if command != nil {
		t.Fatal("invalid name exited")
	}
	if _, found := model.Intent(); found {
		t.Fatal("invalid name emitted intent")
	}
	if !strings.Contains(ansi.Strip(model.View().Content), "1–24 lowercase") {
		t.Fatalf("validation guidance missing: %s", model.View().Content)
	}
	model, _ = dashboardPress(t, model, specialKey(tea.KeyEscape))
	if model.screen != dashboardHome {
		t.Fatalf("cancel left screen %d", model.screen)
	}
	if _, found := model.Intent(); found {
		t.Fatal("cancel emitted intent")
	}
}

func TestAgentFormContainsOnlyAgentAndSessionBrowser(t *testing.T) {
	data := dashboardFixture("running")
	data.Workspaces[0].DefaultAgent = "codex"
	model := NewDashboardModel(data)
	model, _ = dashboardPress(t, model, textKey("a"))
	view := ansi.Strip(model.View().Content)
	for _, expected := range []string{"Open agent", "Agent", "Codex", "Enable isolated browser for this session only"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("agent form omitted %q:\n%s", expected, view)
		}
	}
	for _, forbidden := range []string{"Authentication", "Profile", "Prompt", "Workspace mode"} {
		if strings.Contains(strings.ToLower(view), strings.ToLower(forbidden)) {
			t.Fatalf("agent form included forbidden %q:\n%s", forbidden, view)
		}
	}
	model, _ = dashboardPress(t, model, specialKey(tea.KeyTab))
	model, _ = dashboardPress(t, model, textKey(" "))
	model, _ = dashboardPress(t, model, specialKey(tea.KeyTab))
	model, command := dashboardPress(t, model, specialKey(tea.KeyEnter))
	want := Intent{Action: "agent-run", Root: data.Root, Workspace: "feature-a", Agent: "codex", Browser: true}
	intent, found := model.Intent()
	if command == nil || !found || intent != want {
		t.Fatalf("agent intent = %#v, found=%t, command=%v; want %#v", intent, found, command, want)
	}
}

func TestDashboardEmitsEveryStateAwareIntent(t *testing.T) {
	tests := []struct{ state, key, action string }{
		{"running", "enter", "workspace-open"}, {"running", "u", "workspace-update"},
		{"running", "s", "workspace-stop"}, {"stopped", "s", "workspace-start"},
		{"running", "r", "workspace-restart"},
	}
	for _, test := range tests {
		t.Run(test.action, func(t *testing.T) {
			model := NewDashboardModel(dashboardFixture(test.state))
			var key tea.KeyPressMsg
			if test.key == "enter" {
				key = specialKey(tea.KeyEnter)
			} else {
				key = textKey(test.key)
			}
			model, command := dashboardPress(t, model, key)
			intent, found := model.Intent()
			if command == nil || !found || intent.Action != test.action || intent.Workspace != "feature-a" {
				t.Fatalf("intent = %#v, found=%t", intent, found)
			}
		})
	}
	for key, action := range map[string]string{"s": "git-status", "d": "git-diff", "f": "git-fetch", "a": "git-apply"} {
		model := NewDashboardModel(dashboardFixture("needs_resolution"))
		model, _ = dashboardPress(t, model, textKey("g"))
		model, command := dashboardPress(t, model, textKey(key))
		intent, found := model.Intent()
		if command == nil || !found || intent.Action != action {
			t.Fatalf("git %s intent = %#v, found=%t", key, intent, found)
		}
	}
	cancelled := NewDashboardModel(dashboardFixture("stopped"))
	cancelled, _ = dashboardPress(t, cancelled, textKey("d"))
	cancelled, command := dashboardPress(t, cancelled, textKey("n"))
	if command != nil || cancelled.screen != dashboardHome {
		t.Fatalf("remove cancellation = screen %d, command %v", cancelled.screen, command)
	}
	if intent, found := cancelled.Intent(); found {
		t.Fatalf("remove cancellation emitted %#v", intent)
	}

	model := NewDashboardModel(dashboardFixture("stopped"))
	model, _ = dashboardPress(t, model, textKey("d"))
	model, command = dashboardPress(t, model, textKey("y"))
	intent, found := model.Intent()
	if command == nil || !found || intent.Action != "workspace-remove" {
		t.Fatalf("remove intent = %#v, found=%t", intent, found)
	}
}

func TestDashboardAWSActionsRequireExplicitConfirmationAndEmitOnlyIntent(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
		action  string
		label   string
	}{
		{name: "enable", action: intentAWSEnable, label: "Enable AWS"},
		{name: "disable", enabled: true, action: intentAWSDisable, label: "Disable AWS"},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := dashboardFixture("running")
			data.AWSCapability = "host-default"
			data.Workspaces[0].AWSEnabled = test.enabled
			data.Workspaces[0].AWSHostAvailability = "available"
			data.Workspaces[0].AWSMirrorHealth = "current"
			model := NewDashboardModel(data)
			model.accessible = true
			model.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
			if view := ansi.Strip(model.View().Content); !strings.Contains(view, test.label) || !strings.Contains(view, "Host: Available") {
				t.Fatalf("AWS dashboard omitted state/action:\n%s", view)
			}
			model, command := dashboardPress(t, model, textKey("w"))
			if command != nil {
				t.Fatal("opening AWS confirmation exited")
			}
			if intent, found := model.Intent(); found {
				t.Fatalf("opening AWS confirmation emitted %#v", intent)
			}
			confirmation := ansi.Strip(model.View().Content)
			confirmationCompact := strings.Join(strings.Fields(confirmation), "")
			if test.enabled {
				if !strings.Contains(confirmationCompact, "revokedimmediately") || !strings.Contains(confirmationCompact, "otherworkspacesareunchanged") {
					t.Fatalf("disable confirmation incomplete:\n%s", confirmation)
				}
			} else {
				for _, expected := range []string{"continuously", "approval", "restart", "Named", "unavailable", "Other", "workspaces"} {
					if !strings.Contains(confirmationCompact, strings.Join(strings.Fields(expected), "")) {
						t.Fatalf("enable confirmation omitted %q:\n%s", expected, confirmation)
					}
				}
			}
			model, command = dashboardPress(t, model, textKey("y"))
			intent, found := model.Intent()
			if command == nil || !found || intent.Action != test.action || intent.Workspace != "feature-a" {
				t.Fatalf("AWS intent = %#v, found=%t, command=%v", intent, found, command)
			}
		})
	}
}

func TestDashboardAWSUnavailableGuidanceIsSafeAndCancellationHasNoIntent(t *testing.T) {
	data := dashboardFixture("stopped")
	data.AWSCapability = "host-default"
	data.Workspaces[0].AWSHostAvailability = "credential-secret-must-not-render"
	data.Workspaces[0].AWSMirrorHealth = "raw-host-file-content"
	data.Workspaces[0].AWSFailureCode = "another-secret"
	model := NewDashboardModel(data)
	model.accessible = true
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	model, _ = dashboardPress(t, model, textKey("w"))
	view := ansi.Strip(model.View().Content)
	viewCompact := strings.Join(strings.Fields(view), "")
	for _, expected := range []string{"Unavailable", "Leapp", "provider", "[default]"} {
		if !strings.Contains(viewCompact, strings.Join(strings.Fields(expected), "")) {
			t.Fatalf("unavailable AWS confirmation omitted %q:\n%s", expected, view)
		}
	}
	for _, forbidden := range []string{"credential-secret-must-not-render", "raw-host-file-content", "another-secret"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("AWS confirmation exposed %q:\n%s", forbidden, view)
		}
	}
	model, command := dashboardPress(t, model, textKey("n"))
	if command != nil || model.screen != dashboardHome {
		t.Fatalf("AWS cancellation = screen %d, command %v", model.screen, command)
	}
	if intent, found := model.Intent(); found {
		t.Fatalf("AWS cancellation emitted %#v", intent)
	}
}

func TestDashboardSanitizesBoundsNoColorAndResizes(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	hostile := "branch\x1b]52;c;secret\a\u202espoof"
	data := dashboardFixture("running")
	data.Root, data.Branch, data.Revision = "/tmp/"+hostile, hostile+strings.Repeat("x", 1000), hostile
	data.AllowedAgents = []string{"omp", hostile}
	model := NewDashboardModel(data)
	for _, width := range []int{20, 40, 80, 120} {
		updated, _ := dashboardPress(t, model, tea.KeyPressMsg{})
		updated.Update(tea.WindowSizeMsg{Width: width, Height: 16})
		view := updated.View().Content
		if strings.Contains(view, "\x1b[") {
			t.Fatalf("NO_COLOR emitted SGR: %q", view)
		}
		assertTerminalSafe(t, view)
		for _, line := range strings.Split(ansi.Strip(view), "\n") {
			if terminal.Width(line) > width {
				t.Fatalf("width %d line overflow: %q", width, line)
			}
		}
		if len(view) > 16*1024 {
			t.Fatalf("dashboard view is unbounded: %d bytes", len(view))
		}
	}
	model, command := dashboardPress(t, model, specialKey(tea.KeyEnter))
	intent, found := model.Intent()
	if command == nil || !found || intent.Root != data.Root {
		t.Fatalf("display sanitization mutated intent root: %#v, found=%t", intent, found)
	}
}

func TestRunnerLoadsFreshDashboardAndAccessibleModeRestoresWithoutIntent(t *testing.T) {
	application := &setupApplicationStub{bareState: app.BareState{Screen: app.BareDashboard}}
	var output bytes.Buffer
	loads := 0
	runner := &Runner{Application: application, Input: strings.NewReader("q"), Output: &output}
	intent, found, err := runner.Run(context.Background(), RunRequest{
		Root: "/tmp/project", Accessible: true,
		LoadDashboard: func(context.Context, string) (DashboardData, error) {
			loads++
			return dashboardFixture("running"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if found || intent != (Intent{}) {
		t.Fatalf("cancel returned intent %#v, found=%t", intent, found)
	}
	if loads != 1 {
		t.Fatalf("dashboard loads = %d, want 1", loads)
	}
	if !strings.Contains(output.String(), "Local checkout") {
		t.Fatalf("accessible dashboard output = %q", output.String())
	}
	assertTerminalSafe(t, output.String())
}

func TestRunnerForceSetupApprovesExistingConfiguration(t *testing.T) {
	preview := app.SetupPreview{
		Hash:                strings.Repeat("a", 64),
		ConfigContentDigest: strings.Repeat("b", 64),
		ProjectState:        strings.Repeat("c", 64),
	}
	application := &setupApplicationStub{
		bareState: app.BareState{Screen: app.BareDashboard, ConfigExists: true},
		preview:   preview,
	}
	var output bytes.Buffer
	probe := NewExistingApprovalModel(context.Background(), application, "/tmp/project", preview, true)
	pageCount := len(reviewPages(probe.review, outputWidth(&output), 24))
	input := strings.NewReader(strings.Repeat("\n", pageCount-1) + "y\nq\n")
	runner := &Runner{Application: application, Input: input, Output: &output}

	intent, found, err := runner.Run(context.Background(), RunRequest{
		Root: "/tmp/project", ForceSetup: true, Accessible: true,
		LoadDashboard: func(context.Context, string) (DashboardData, error) {
			return dashboardFixture("running"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if found || intent != (Intent{}) {
		t.Fatalf("reapproval returned intent %#v, found=%t", intent, found)
	}
	if application.existingPreviews != 1 || application.previews != 0 {
		t.Fatalf("preview calls = existing %d, setup %d", application.existingPreviews, application.previews)
	}
	if application.approvals != 1 || application.initializes != 0 {
		t.Fatalf("approval calls = %d, setup calls = %d", application.approvals, application.initializes)
	}
	if !application.request.Confirmed || application.request.ExpectedHash != preview.Hash {
		t.Fatalf("approval request = %#v", application.request)
	}
	if !strings.Contains(output.String(), "DSX configuration approval") || !strings.Contains(output.String(), "Local checkout") {
		t.Fatalf("reapproval output = %q", output.String())
	}
}

func TestAccessibleCreateFormEmitsReviewedWorkspaceIntent(t *testing.T) {
	var output bytes.Buffer
	runner := &Runner{Input: strings.NewReader("cfeature-new\t\t\t\n"), Output: &output}
	final, err := runner.runAccessibleAction(NewDashboardModel(dashboardFixture("running")))
	if err != nil {
		t.Fatal(err)
	}
	model := final.(*DashboardModel)
	intent, found := model.Intent()
	want := Intent{
		Action: "workspace-create", Root: "/tmp/tracking-chrome-extension",
		Workspace: "feature-new", SourceBranch: "feat/branch-1", SourceRevision: "abc123",
		Agent: "omp", Open: true,
	}
	if !found || intent != want {
		t.Fatalf("accessible create intent = %#v, found=%t; want %#v; output=%q", intent, found, want, output.String())
	}
	for _, forbidden := range []string{"Authentication profile", "One-shot prompt", "Enable isolated browser?"} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("accessible create output contained forbidden field %q: %q", forbidden, output.String())
		}
	}
	assertTerminalSafe(t, output.String())
}

func TestAccessibleDirtySnapshotCreateRequiresReview(t *testing.T) {
	data := dashboardFixture("running")
	data.Clean = false
	var output bytes.Buffer
	runner := &Runner{Input: strings.NewReader("cdirty-work\t\t \t\ny"), Output: &output}
	final, err := runner.runAccessibleAction(NewDashboardModel(data))
	if err != nil {
		t.Fatal(err)
	}
	intent, found := final.(*DashboardModel).Intent()
	want := Intent{
		Action: "workspace-create", Root: data.Root, Workspace: "dirty-work",
		SourceBranch: data.Branch, SourceRevision: data.Revision,
		Snapshot: true, Agent: "omp", Open: true,
	}
	if !found || intent != want {
		t.Fatalf("accessible snapshot intent = %#v, found=%t; want %#v", intent, found, want)
	}
	normalized := strings.Join(strings.Fields(strings.ReplaceAll(ansi.Strip(output.String()), "│", " ")), " ")
	for _, expected := range []string{
		"Review source snapshot",
		"Includes final tracked file content and nonignored untracked files.",
		"Your branch, HEAD, index, worktree, and durable refs are not changed.",
	} {
		if !strings.Contains(normalized, expected) {
			t.Fatalf("accessible snapshot output omitted %q: %q", expected, normalized)
		}
	}
	assertTerminalSafe(t, output.String())
}

func TestSnapshotReviewRespectsSmallTerminalWidths(t *testing.T) {
	for _, width := range []int{20, 40} {
		model := NewDashboardModel(dashboardFixture("running"))
		model.screen = dashboardCreateSnapshotReview
		model.name = "dirty-work"
		model.snapshot = true
		model.Update(tea.WindowSizeMsg{Width: width, Height: 16})
		view := ansi.Strip(model.View().Content)
		for _, line := range strings.Split(view, "\n") {
			if terminal.Width(line) > width {
				t.Fatalf("width %d snapshot review overflow: %q", width, line)
			}
		}
		if len(view) > 16*1024 {
			t.Fatalf("width %d snapshot review exceeded output bound: %d", width, len(view))
		}
	}
}
