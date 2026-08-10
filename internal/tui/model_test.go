package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/terminal"
)

type setupApplicationStub struct {
	initializes   int
	previews      int
	approvals     int
	request       app.InitializeRequest
	cloneRequest  app.ClonePreviewRequest
	preview       app.SetupPreview
	previewErr    error
	initializeErr error
}

func (stub *setupApplicationStub) BareState(context.Context, app.BareStateRequest) (app.BareState, error) {
	return app.BareState{Screen: app.BareSetup}, nil
}

func (stub *setupApplicationStub) PreviewSetup(context.Context, app.SetupPreviewRequest) (app.SetupPreview, error) {
	stub.previews++
	return stub.preview, stub.previewErr
}

func (stub *setupApplicationStub) PreviewExisting(context.Context, app.BareStateRequest) (app.SetupPreview, error) {
	stub.previews++
	return stub.preview, stub.previewErr
}

func (stub *setupApplicationStub) PreviewClone(_ context.Context, request app.ClonePreviewRequest) (app.SetupPreview, error) {
	stub.previews++
	stub.cloneRequest = request
	preview := stub.preview
	preview.Facts.CanonicalRoot = request.Root
	return preview, stub.previewErr
}

func (stub *setupApplicationStub) Initialize(_ context.Context, request app.InitializeRequest) (app.InitializeResult, error) {
	stub.initializes++
	stub.request = request
	if stub.initializeErr != nil {
		return app.InitializeResult{}, stub.initializeErr
	}
	return app.InitializeResult{ConfigPath: "/tmp/project/.dsx/config.jsonc", Hash: request.ExpectedHash, Created: true}, nil
}

func (stub *setupApplicationStub) ApproveExisting(_ context.Context, request app.InitializeRequest) (app.InitializeResult, error) {
	stub.approvals++
	stub.request = request
	if stub.initializeErr != nil {
		return app.InitializeResult{}, stub.initializeErr
	}
	return app.InitializeResult{ConfigPath: "/tmp/project/.dsx/config.jsonc", Hash: request.ExpectedHash}, nil
}

func TestSetupCancelDoesNotCallApplicationCommand(t *testing.T) {
	application := &setupApplicationStub{}
	model := NewSetupModel(context.Background(), application, "/tmp/project", app.SetupPreview{}, false)
	model.stage = setupPreview
	_, command := model.Update(tea.KeyPressMsg(tea.Key{Code: 'c', Mod: tea.ModCtrl}))
	if command == nil {
		t.Fatal("Ctrl-C did not return a quit command")
	}
	if application.initializes != 0 {
		t.Fatalf("Initialize calls = %d, want 0", application.initializes)
	}
}

func TestSetupFinalConfirmationCallsApplicationCommandOnce(t *testing.T) {
	application := &setupApplicationStub{}
	preview := app.SetupPreview{Hash: strings.Repeat("a", 64), ConfigContentDigest: strings.Repeat("b", 64), ProjectState: strings.Repeat("c", 64), RenderedConfig: []byte("{}\n")}
	model := NewSetupModel(context.Background(), application, "/tmp/project", preview, false)
	model.stage = setupPreview
	for model.reviewPage+1 < model.reviewPageCount() {
		updated, _ := model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyPgDown}))
		model = updated.(*SetupModel)
	}
	updated, command := model.Update(tea.KeyPressMsg(tea.Key{Text: "y", Code: 'y'}))
	model = updated.(*SetupModel)
	if command == nil {
		t.Fatal("final confirmation did not schedule Initialize")
	}
	message := command()
	updated, _ = model.Update(message)
	model = updated.(*SetupModel)
	if application.initializes != 1 {
		t.Fatalf("Initialize calls = %d, want 1", application.initializes)
	}
	if !application.request.Confirmed || application.request.ExpectedHash != preview.Hash || string(application.request.RenderedConfig) != string(preview.RenderedConfig) {
		t.Fatalf("Initialize request = %#v", application.request)
	}
	_, second := model.Update(tea.KeyPressMsg(tea.Key{Text: "y", Code: 'y'}))
	if second != nil || application.initializes != 1 {
		t.Fatalf("completed model scheduled a second Initialize; calls = %d", application.initializes)
	}
}

func TestLauncherAndDashboardExposeIntentsWithoutSuccess(t *testing.T) {
	state := app.BareState{Facts: app.ProjectFacts{CanonicalRoot: "/tmp/project"}, ConfigExists: true, OwnedResources: 1}
	for name, model := range map[string]*ActionModel{"launcher": NewLauncherModel(state), "dashboard": NewDashboardModel(state)} {
		t.Run(name, func(t *testing.T) {
			updated, command := model.Update(tea.KeyPressMsg(tea.Key{Text: "a", Code: 'a'}))
			if command == nil {
				t.Fatal("attach did not exit with an intent")
			}
			intent, ok := updated.(*ActionModel).Intent()
			if !ok || intent.Action != "attach" || intent.Project != "/tmp/project" {
				t.Fatalf("intent = %#v, found = %t", intent, ok)
			}
			if strings.Contains(updated.(*ActionModel).View().Content, "success") {
				t.Fatal("action model rendered fake success")
			}
		})
	}
}

func TestConfiguredLauncherCanSelectNamedCloneCreation(t *testing.T) {
	state := app.BareState{Facts: app.ProjectFacts{CanonicalRoot: "/tmp/project"}, ConfigExists: true}
	updated, command := NewLauncherModel(state).Update(tea.KeyPressMsg(tea.Key{Text: "n", Code: 'n'}))
	if command == nil {
		t.Fatal("named clone creation did not exit with an intent")
	}
	intent, found := updated.(*ActionModel).Intent()
	want := Intent{Action: "new-clone", Project: "/tmp/project"}
	if !found || intent != want {
		t.Fatalf("intent = %#v, found = %t, want %#v", intent, found, want)
	}
	if !strings.Contains(NewLauncherModel(state).View().Content, "[n] new clone") {
		t.Fatal("launcher omitted named clone action")
	}
}

func TestDashboardGitStatusSelectsExactNamedClone(t *testing.T) {
	state := app.BareState{Facts: app.ProjectFacts{CanonicalRoot: "/tmp/project"}, ConfigExists: true, OwnedResources: 3}
	action := NewDashboardModel(state,
		app.SandboxSummary{Sandbox: "z-task", Mode: model.ModeClone, State: model.StateStopped},
		app.SandboxSummary{Sandbox: "main", Mode: model.ModeLive, State: model.StateRunning},
		app.SandboxSummary{Sandbox: "api-task", Mode: model.ModeClone, State: model.StateRunning},
	)
	updated, command := action.Update(tea.KeyPressMsg(tea.Key{Text: "g", Code: 'g'}))
	if command == nil {
		t.Fatal("git status did not quit with an intent")
	}
	intent, found := updated.(*ActionModel).Intent()
	want := Intent{Action: "git-status", Project: "/tmp/project", Sandbox: "api-task"}
	if !found || intent != want {
		t.Fatalf("git status intent = %#v, found = %t, want %#v", intent, found, want)
	}

	action = NewDashboardModel(state,
		app.SandboxSummary{Sandbox: "z-task", Mode: model.ModeClone},
		app.SandboxSummary{Sandbox: "api-task", Mode: model.ModeClone},
	)
	updated, _ = action.Update(tea.KeyPressMsg(tea.Key{Text: "j", Code: 'j'}))
	updated, _ = updated.(*ActionModel).Update(tea.KeyPressMsg(tea.Key{Text: "g", Code: 'g'}))
	intent, found = updated.(*ActionModel).Intent()
	if !found || intent.Sandbox != "z-task" {
		t.Fatalf("selected git status intent = %#v, found = %t", intent, found)
	}
}

func TestDashboardGitDiffAndFetchSelectExactNamedClone(t *testing.T) {
	state := app.BareState{Facts: app.ProjectFacts{CanonicalRoot: "/tmp/project"}, ConfigExists: true, OwnedResources: 2}
	for _, test := range []struct {
		key string

		action string
	}{
		{key: "v", action: "git-diff"},
		{key: "f", action: "git-fetch"},
	} {
		t.Run(test.action, func(t *testing.T) {
			action := NewDashboardModel(state, app.SandboxSummary{Sandbox: "review", Mode: model.ModeClone})
			updated, command := action.Update(tea.KeyPressMsg(tea.Key{Text: test.key, Code: rune(test.key[0])}))
			if command == nil {
				t.Fatalf("%s did not quit with an intent", test.action)
			}
			intent, found := updated.(*ActionModel).Intent()
			want := Intent{Action: test.action, Project: "/tmp/project", Sandbox: "review"}
			if !found || intent != want {
				t.Fatalf("intent = %#v, found = %t, want %#v", intent, found, want)
			}
		})
	}
}
func TestDashboardStopSelectsExactNamedClone(t *testing.T) {
	state := app.BareState{Facts: app.ProjectFacts{CanonicalRoot: "/tmp/project"}, ConfigExists: true, OwnedResources: 2}
	action := NewDashboardModel(state,
		app.SandboxSummary{Sandbox: "z-task", Mode: model.ModeClone, State: model.StateRunning},
		app.SandboxSummary{Sandbox: "api-task", Mode: model.ModeClone, State: model.StateRunning},
	)
	updated, command := action.Update(tea.KeyPressMsg(tea.Key{Text: "x", Code: 'x'}))
	if command == nil {
		t.Fatal("stop did not quit with an intent")
	}
	intent, found := updated.(*ActionModel).Intent()
	want := Intent{Action: "stop", Project: "/tmp/project", Sandbox: "api-task"}
	if !found || intent != want {
		t.Fatalf("intent = %#v, found = %t, want %#v", intent, found, want)
	}
}

func TestDashboardGitStatusRejectsLiveOrMissingSelection(t *testing.T) {
	hostile := model.SandboxName("task\x1b]52;c;Y2xpcA==\a\u202e")
	action := NewDashboardModel(
		app.BareState{Facts: app.ProjectFacts{CanonicalRoot: "/tmp/project"}},
		app.SandboxSummary{Sandbox: "main", Mode: model.ModeLive},
		app.SandboxSummary{Sandbox: hostile, Mode: model.ModeClone},
	)
	updated, command := action.Update(tea.KeyPressMsg(tea.Key{Text: "g", Code: 'g'}))
	action = updated.(*ActionModel)
	if command != nil {
		t.Fatal("git status quit without a named clone selection")
	}
	if _, found := action.Intent(); found {
		t.Fatal("git status intent existed without a named clone selection")
	}
	if !strings.Contains(action.View().Content, "Git status unavailable") {
		t.Fatalf("missing-selection view = %q", action.View().Content)
	}
	assertTerminalSafe(t, action.View().Content)
}

func TestDashboardCleanRequiresExplicitConfirmation(t *testing.T) {
	model := NewDashboardModel(app.BareState{Facts: app.ProjectFacts{CanonicalRoot: "/tmp/project"}, ConfigExists: true, OwnedResources: 2})
	updated, command := model.Update(tea.KeyPressMsg(tea.Key{Text: "d", Code: 'd'}))
	model = updated.(*ActionModel)
	if command != nil {
		t.Fatal("clean key quit before confirmation")
	}
	if _, found := model.Intent(); found {
		t.Fatal("clean intent existed before confirmation")
	}
	if !strings.Contains(model.View().Content, "confirm cleanup") {
		t.Fatalf("confirmation view = %q", model.View().Content)
	}
	updated, command = model.Update(tea.KeyPressMsg(tea.Key{Text: "y", Code: 'y'}))
	if command == nil {
		t.Fatal("confirmed clean did not quit with an intent")
	}
	intent, found := updated.(*ActionModel).Intent()
	if !found || intent.Action != "clean" || intent.Project != "/tmp/project" {
		t.Fatalf("clean intent = %#v, found = %t", intent, found)
	}
}

func TestSanitizeAllDynamicTUIFields(t *testing.T) {
	hostile := "project\x1b[2J\x1b]0;title\a\u202espoof"
	preview := app.SetupPreview{
		Facts:                app.ProjectFacts{CanonicalRoot: "/tmp/" + hostile, ConfigPath: hostile},
		SelectedCapabilities: []string{hostile},
		RenderedConfig:       []byte("{\"name\":\"" + hostile + "\"}\n"),
		Hash:                 hostile,
	}
	preview.Config.Image.Ref = hostile
	preview.Config.Agents.Default = hostile
	model := NewSetupModel(context.Background(), &setupApplicationStub{}, hostile, preview, false)
	for _, forbidden := range []string{"\x1b[2J", "\x1b]0", "\a", "\u202e"} {
		if strings.Contains(model.form.View(), forbidden) {
			t.Fatalf("setup form rendered raw hostile control %q: %q", forbidden, model.form.View())
		}
	}
	model.applyForm()
	if model.document.Image.Ref != preview.Config.Image.Ref || model.document.Agents.Default != preview.Config.Agents.Default {
		t.Fatal("sanitized form display mutated unchanged configuration values")
	}
	model.stage = setupPreview
	assertTerminalSafe(t, model.View().Content)
	if !strings.Contains(model.View().Content, `\x1b[2J`) || !strings.Contains(model.View().Content, `\u202e`) {
		t.Fatalf("preview did not visibly escape hostile content: %q", model.View().Content)
	}

	model.err = errors.New(hostile)
	assertTerminalSafe(t, model.View().Content)
	model.err = nil
	model.stage = setupDone
	model.result = app.InitializeResult{ConfigPath: hostile, Hash: hostile}
	assertTerminalSafe(t, model.View().Content)

	action := NewLauncherModel(app.BareState{Facts: app.ProjectFacts{CanonicalRoot: hostile}})
	assertTerminalSafe(t, action.View().Content)
	if intentModel, _ := action.Update(tea.KeyPressMsg(tea.Key{Text: "a", Code: 'a'})); intentModel.(*ActionModel).intent.Project != hostile {
		t.Fatal("display sanitization corrupted the functional project intent")
	}
}

func TestResizeNarrowLayoutsRetainConfirmationAndControls(t *testing.T) {
	preview := app.SetupPreview{
		Facts:                app.ProjectFacts{CanonicalRoot: "/tmp/" + strings.Repeat("wide-project-", 20)},
		SelectedCapabilities: []string{strings.Repeat("capability-", 20)},
		RenderedConfig:       []byte(strings.Repeat("configuration ", 30)),
		Hash:                 strings.Repeat("a", 64),
	}
	for _, width := range []int{20, 40, 80, 120} {
		t.Run(fmt.Sprintf("width-%d", width), func(t *testing.T) {
			model := NewSetupModel(context.Background(), &setupApplicationStub{}, "/tmp/project", preview, false)
			model.stage = setupPreview
			updated, _ := model.Update(tea.WindowSizeMsg{Width: width, Height: 24})
			model = updated.(*SetupModel)
			content := model.View().Content
			assertLinesFit(t, content, width)
			if !strings.Contains(content, "Review page 1/") || !strings.Contains(content, "Approval is locked") {
				t.Fatalf("width %d lost paged review position and controls: %q", width, content)
			}
			for model.reviewPage+1 < model.reviewPageCount() {
				updated, _ = model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyPgDown}))
				model = updated.(*SetupModel)
			}
			content = model.View().Content
			assertLinesFit(t, content, width)
			if !strings.Contains(content, "Final confirmation:") || !strings.Contains(content, "[y/N]") {
				t.Fatalf("width %d lost final confirmation: %q", width, content)
			}

			action := NewDashboardModel(app.BareState{Facts: preview.Facts})
			updatedAction, _ := action.Update(tea.WindowSizeMsg{Width: width, Height: 24})
			actionContent := updatedAction.(*ActionModel).View().Content
			assertLinesFit(t, actionContent, width)
			for _, control := range []string{"[c] new clone", "[d] clean", "[q] quit"} {
				if !strings.Contains(actionContent, control) {
					t.Fatalf("width %d lost %q: %q", width, control, actionContent)
				}
			}
		})
	}
}

func TestAccessibleSetupUsesPlainPromptsAndSanitizedReview(t *testing.T) {
	hostile := "/tmp/project\x1b]52;c;Y2xpcA==\a\u202e"
	preview := app.SetupPreview{
		Facts:               app.ProjectFacts{CanonicalRoot: hostile},
		RenderedConfig:      []byte("{}\n"),
		Hash:                strings.Repeat("a", 64),
		ConfigContentDigest: strings.Repeat("b", 64),
		ProjectState:        strings.Repeat("c", 64),
	}
	application := &setupApplicationStub{preview: preview}
	review, reviewErr := buildCompleteReview(preview)
	if reviewErr != nil {
		t.Fatalf("build accessible review: %v", reviewErr)
	}
	pageCount := len(reviewPages(review, 80, 24))
	var output bytes.Buffer
	runner := &Runner{
		Application: application,
		Input:       strings.NewReader("\n\n\n" + strings.Repeat("\n", pageCount-1) + "y\n"),
		Output:      &output,
	}
	_, found, err := runner.Run(context.Background(), RunRequest{Root: hostile, ForceSetup: true, Accessible: true})
	if err != nil {
		t.Fatalf("accessible Run() error = %v", err)
	}
	if found || application.initializes != 1 {
		t.Fatalf("found = %t, initialize calls = %d", found, application.initializes)
	}
	got := output.String()
	assertTerminalSafe(t, got)
	for _, expected := range []string{"Pinned image reference", "Default coding agent", "Allow internet access?", "Final confirmation:", "DSX setup complete", `\u001b]52`, `\u0007`, `\u202e`} {
		if !strings.Contains(got, expected) {
			t.Fatalf("accessible output missing %q: %q", expected, got)
		}
	}
}

func TestConfiguredLauncherActionRequiresCompleteAccessibleApproval(t *testing.T) {
	preview := app.SetupPreview{
		Facts:               app.ProjectFacts{CanonicalRoot: "/tmp/project"},
		RenderedConfig:      []byte("{}\n"),
		Hash:                strings.Repeat("a", 64),
		ConfigContentDigest: strings.Repeat("b", 64),
		ProjectState:        strings.Repeat("c", 64),
	}
	review, err := buildCompleteReview(preview)
	if err != nil {
		t.Fatal(err)
	}
	application := &setupApplicationStub{preview: preview}
	var output bytes.Buffer
	runner := &Runner{
		Application: application,
		Input: strings.NewReader(
			strings.Repeat("\n", len(reviewPages(review, 80, 24))-1) + "y\n",
		),
		Output: &output,
	}
	intent := Intent{Action: "start", Project: "/tmp/project"}
	got, found, err := runner.approveConfiguredAction(context.Background(), RunRequest{
		Root: "/tmp/project", Accessible: true,
	}, intent)
	if err != nil {
		t.Fatal(err)
	}
	if !found || got != intent || application.approvals != 1 || application.initializes != 0 {
		t.Fatalf("intent = %#v, found = %t, approvals = %d, initializes = %d", got, found, application.approvals, application.initializes)
	}
	if !strings.Contains(output.String(), "Complete effective plan") || !strings.Contains(output.String(), "Final confirmation:") {
		t.Fatalf("approval output omitted complete review: %q", output.String())
	}
}

func TestAccessibleDashboardCloneCreateReturnsReviewedRunIntent(t *testing.T) {
	preview := app.SetupPreview{
		Facts:               app.ProjectFacts{CanonicalRoot: "/tmp/project"},
		RenderedConfig:      []byte("{}\n"),
		Hash:                strings.Repeat("d", 64),
		ConfigContentDigest: strings.Repeat("e", 64),
		ProjectState:        strings.Repeat("f", 64),
	}
	review, err := buildCompleteReview(preview)
	if err != nil {
		t.Fatal(err)
	}
	application := &setupApplicationStub{preview: preview}
	var output bytes.Buffer
	runner := &Runner{
		Application: application,
		Input: strings.NewReader(
			"task\nopencode\nsmoke\nCreate result.txt then exit.\nn\n" +
				strings.Repeat("\n", len(reviewPages(review, 80, 24))-1) + "y\n",
		),
		Output: &output,
	}
	intent, found, err := runner.runCloneCreate(context.Background(), RunRequest{Root: "/tmp/project", Accessible: true})
	if err != nil {
		t.Fatal(err)
	}
	want := Intent{
		Action: "clone-run", Project: "/tmp/project", Sandbox: "task", Agent: "opencode",
		Profile: "smoke", Prompt: "Create result.txt then exit.", ApproveConfig: preview.Hash,
	}
	if !found || intent != want {
		t.Fatalf("intent = %#v, found = %t, want %#v, output = %q", intent, found, want, output.String())
	}
	if !strings.Contains(output.String(), "Complete effective plan") || !strings.Contains(output.String(), "DSX execution approved") {
		t.Fatalf("clone approval output = %q", output.String())
	}
}

func TestAccessibleDashboardCloneCannotDisableConfiguredBrowser(t *testing.T) {
	preview := app.SetupPreview{
		Facts:               app.ProjectFacts{CanonicalRoot: "/tmp/project"},
		RenderedConfig:      []byte("{}\n"),
		Hash:                strings.Repeat("a", 64),
		ConfigContentDigest: strings.Repeat("b", 64),
		ProjectState:        strings.Repeat("c", 64),
	}
	preview.Config.Browser.Enabled = true
	review, err := buildCompleteReview(preview)
	if err != nil {
		t.Fatal(err)
	}
	application := &setupApplicationStub{preview: preview}
	var output bytes.Buffer
	runner := &Runner{
		Application: application,
		Input: strings.NewReader(
			"task\nopencode\nsmoke\nCreate result.txt then exit.\n" +
				strings.Repeat("\n", len(reviewPages(review, 80, 24))-1) + "y\n",
		),
		Output: &output,
	}
	intent, found, err := runner.runCloneCreate(context.Background(), RunRequest{Root: "/tmp/project", Accessible: true})
	if err != nil {
		t.Fatal(err)
	}
	if !found || !intent.Browser || !application.cloneRequest.Browser {
		t.Fatalf("intent = %#v, found = %t, preview request = %#v", intent, found, application.cloneRequest)
	}
	if !strings.Contains(output.String(), "Enabled by project configuration") || strings.Contains(output.String(), "Enable isolated browser?") {
		t.Fatalf("configured browser form offered an invalid disable choice: %q", output.String())
	}
}

func TestCompleteReviewRequiresTailGrantAndRejectsOverBound(t *testing.T) {
	const tailGrant = "tail-trust-grant.example.internal"
	bridges := make([]plan.BridgeGrant, 0, 401)
	for index := range 400 {
		bridges = append(bridges, plan.BridgeGrant{
			Kind:        "tcp",
			Name:        fmt.Sprintf("grant-%03d-%s", index, strings.Repeat("x", 40)),
			Destination: fmt.Sprintf("service-%03d.example.internal", index),
			Port:        443,
		})
	}
	bridges = append(bridges, plan.BridgeGrant{Kind: "tcp", Name: "tail", Destination: tailGrant, Port: 8443})
	preview := app.SetupPreview{
		Plan:                plan.ExecutionPlan{ContractVersion: plan.ContractVersion, Bridges: bridges},
		RenderedConfig:      []byte("{}\n"),
		Hash:                strings.Repeat("a", 64),
		ConfigContentDigest: strings.Repeat("b", 64),
		ProjectState:        strings.Repeat("c", 64),
	}
	application := &setupApplicationStub{}
	model := NewSetupModel(context.Background(), application, "/tmp/project", preview, false)
	model.stage = setupPreview
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	if len(model.review) <= terminal.DefaultSanitizedLimit {
		t.Fatalf("review length = %d, want greater than generic sanitizer limit", len(model.review))
	}
	if strings.Contains(model.View().Content, tailGrant) || strings.Contains(model.View().Content, "Final confirmation:") {
		t.Fatal("first page exposed tail grant or enabled confirmation")
	}
	if _, command := model.Update(tea.KeyPressMsg(tea.Key{Text: "y", Code: 'y'})); command != nil || application.initializes != 0 {
		t.Fatal("approval was available before navigating the complete review")
	}

	tailVisible := false
	for {
		content := model.View().Content
		tailVisible = tailVisible || strings.Contains(content, tailGrant)
		if model.reviewPage+1 == model.reviewPageCount() {
			if !tailVisible {
				t.Fatal("reached final confirmation without displaying the tail trust grant")
			}
			if !strings.Contains(content, "Final confirmation:") {
				t.Fatal("final page did not expose confirmation")
			}
			break
		}
		updated, _ := model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyPgDown}))
		model = updated.(*SetupModel)
	}
	_, command := model.Update(tea.KeyPressMsg(tea.Key{Text: "y", Code: 'y'}))
	if command == nil {
		t.Fatal("final confirmation did not schedule initialization")
	}
	_ = command()
	if application.initializes != 1 {
		t.Fatalf("Initialize calls = %d, want 1", application.initializes)
	}

	tooLarge := preview
	tooLarge.RenderedConfig = []byte(strings.Repeat("z", maxSetupReviewBytes+1))
	refused := NewSetupModel(context.Background(), application, "/tmp/project", tooLarge, false)
	refused.stage = setupPreview
	content := refused.View().Content
	if !strings.Contains(content, "Nothing was truncated") || !strings.Contains(content, "Approval is disabled") || strings.Contains(content, "[y/N]") {
		t.Fatalf("over-bound review refusal = %q", content)
	}
	if _, command := refused.Update(tea.KeyPressMsg(tea.Key{Text: "y", Code: 'y'})); command != nil {
		t.Fatal("over-bound review accepted confirmation")
	}
}

func TestNoColorSetupViewHasNoSGR(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	model := NewSetupModel(context.Background(), &setupApplicationStub{}, "/tmp/project", app.SetupPreview{}, false)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
	if strings.Contains(model.View().Content, "\x1b[") {
		t.Fatalf("NO_COLOR view emitted SGR: %q", model.View().Content)
	}
}

func assertTerminalSafe(t *testing.T, value string) {
	t.Helper()
	for _, forbidden := range []string{"\x1b", "\a", "\r", "\u202e", "\u2066", "\u2069"} {
		if strings.Contains(value, forbidden) {
			t.Fatalf("terminal output contains raw control %q: %q", forbidden, value)
		}
	}
}

func assertLinesFit(t *testing.T, value string, width int) {
	t.Helper()
	for _, line := range strings.Split(value, "\n") {
		if got := terminal.Width(line); got > width {
			t.Fatalf("line width %d exceeds %d: %q", got, width, line)
		}
	}
}

func TestHandoffHostileSetupPTYSmoke(t *testing.T) {
	const helperEnvironment = "DSX_HOSTILE_SETUP_PTY_HELPER"
	if os.Getenv(helperEnvironment) == "1" {
		hostileRoot := os.Getenv("DSX_HOSTILE_PROJECT_ROOT")
		preview := app.SetupPreview{Facts: app.ProjectFacts{CanonicalRoot: hostileRoot}, RenderedConfig: []byte("{}\n"), Hash: strings.Repeat("a", 64)}
		model := NewSetupModel(context.Background(), &setupApplicationStub{}, hostileRoot, preview, false)
		model.stage = setupPreview
		if _, err := tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run(); err != nil {
			t.Fatalf("run setup PTY helper: %v", err)
		}
		return
	}

	hostileRoot := filepath.Join(t.TempDir(), "project\x1b]52;c;Y2xpcA==\a\u202e")
	if err := os.Mkdir(hostileRoot, 0o700); err != nil {
		t.Fatalf("create hostile temporary project: %v", err)
	}
	command := exec.Command(os.Args[0], "-test.run", "^TestHandoffHostileSetupPTYSmoke$")
	command.Env = append(os.Environ(), helperEnvironment+"=1", "DSX_HOSTILE_PROJECT_ROOT="+hostileRoot)
	output := &synchronizedBuffer{}
	var eventMu sync.Mutex
	var events []string
	record := func(event string) {
		eventMu.Lock()
		events = append(events, event)
		eventMu.Unlock()
	}
	exit, err := (terminal.Handoff{
		Input:  strings.NewReader("q"),
		Output: output,
		State: terminal.StateFuncs{
			ReleaseFunc: func() error { record("release"); return nil },
			RestoreFunc: func() error { record("restore"); return nil },
		},
	}).Run(context.Background(), command)
	if err != nil || exit.ExitCode != 0 {
		t.Fatalf("hostile setup PTY exit = %#v, error = %v, output = %q", exit, err, output.String())
	}
	raw := output.String()
	if strings.Contains(raw, "\x1b]52;c;Y2xpcA==") || strings.Contains(raw, "\u202e") {
		t.Fatalf("hostile setup executed raw OSC/bidi content: %q", raw)
	}
	if !strings.Contains(raw, `\u001b]52;c;Y2xpcA==`) || !strings.Contains(raw, `\u202e`) {
		t.Fatalf("hostile setup did not visibly escape project name: %q", raw)
	}
	enter, leave := strings.Index(raw, "\x1b[?1049h"), strings.LastIndex(raw, "\x1b[?1049l")
	if enter < 0 || leave <= enter {
		t.Fatalf("alternate-screen restoration ordering missing: enter=%d leave=%d output=%q", enter, leave, raw)
	}
	eventMu.Lock()
	gotEvents := strings.Join(events, ",")
	eventMu.Unlock()
	if gotEvents != "release,restore" {
		t.Fatalf("handoff terminal restoration events = %q", gotEvents)
	}
}

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *synchronizedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}

func (buffer *synchronizedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}
