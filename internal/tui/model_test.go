package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/config"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/terminal"
)

type setupApplicationStub struct {
	initializes    int
	previews       int
	approvals      int
	systemStarts   int
	request        app.InitializeRequest
	cloneRequest   app.ClonePreviewRequest
	setupRequest   app.SetupPreviewRequest
	preview        app.SetupPreview
	bareState      app.BareState
	previewErr     error
	initializeErr  error
	systemStartErr error
}

func (stub *setupApplicationStub) BareState(context.Context, app.BareStateRequest) (app.BareState, error) {
	if stub.bareState.Screen == "" {
		return app.BareState{Screen: app.BareSetup}, nil
	}
	return stub.bareState, nil
}

func (stub *setupApplicationStub) StartContainerSystem(context.Context) error {
	stub.systemStarts++
	if stub.systemStartErr == nil {
		stub.bareState.ContainerSystem.State = runtime.SystemStateRunning
	}
	return stub.systemStartErr
}

func (stub *setupApplicationStub) PreviewSetup(_ context.Context, request app.SetupPreviewRequest) (app.SetupPreview, error) {
	stub.previews++
	stub.setupRequest = request
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

func TestSetupRejectsBlankCustomImage(t *testing.T) {
	if err := validateCustomImage(" \t"); err == nil || !strings.Contains(err.Error(), "digest-pinned") {
		t.Fatalf("blank custom image validation = %v", err)
	}
}

func TestSetupOmitsUnavailableImageOptions(t *testing.T) {
	preview := app.SetupPreview{
		Config: config.ConfigDocument{SchemaVersion: 1},
		ImageOptions: []app.SetupImageOption{
			{ID: "standard", Name: "DSX Standard", Available: false},
			{ID: "custom", Name: "Use another image", Available: true},
		},
		SelectedImageOption: "custom",
	}
	model := NewSetupModel(context.Background(), &setupApplicationStub{}, "/tmp/project", preview, false)
	if len(model.imageOptions) != 1 || model.imageOptions[0].ID != "custom" {
		t.Fatalf("image options = %#v, want only custom", model.imageOptions)
	}
	if model.imageChoice != "custom" {
		t.Fatalf("selected image option = %q, want custom", model.imageChoice)
	}
}

func TestSetupFormShowsConciseImageAndSupportedAgentChoices(t *testing.T) {
	preview := app.SetupPreview{
		Config: config.ConfigDocument{
			SchemaVersion: 1,
			Agents:        config.AgentConfig{Default: "codex", Allowed: []string{"codex"}},
		},
		ImageOptions: []app.SetupImageOption{
			{
				ID:          "standard",
				Name:        "DSX Standard — Ubuntu (Recommended)",
				Description: "Built locally on first use with Codex, Claude, OMP, and OpenCode",
				Available:   true,
				Image:       config.ImageConfig{Standard: true},
			},
			{ID: "custom", Name: "Use another image — Advanced", Available: true},
		},
		SelectedImageOption: "standard",
	}
	model := NewSetupModel(context.Background(), &setupApplicationStub{}, "/tmp/project", preview, false)
	model.form.Init()
	imageView := ansi.Strip(model.form.View())
	for _, expected := range []string{"DSX Standard — Ubuntu (Recommended)", "Use another image — Advanced"} {
		if !strings.Contains(imageView, expected) {
			t.Fatalf("image choices omitted %q:\n%s", expected, imageView)
		}
	}
	for _, unwanted := range []string{"Choose a starting environment", "Built locally on first use"} {
		if strings.Contains(imageView, unwanted) {
			t.Fatalf("image choices retained redundant copy %q:\n%s", unwanted, imageView)
		}
	}

	model.form.NextGroup()
	agentView := ansi.Strip(model.form.View())
	for _, expected := range []string{"Codex", "Claude Code", "OMP", "OpenCode"} {
		if !strings.Contains(agentView, expected) {
			t.Fatalf("coding assistant choices omitted %q:\n%s", expected, agentView)
		}
	}
	if strings.Contains(agentView, "OpenCode  Let this workspace") {
		t.Fatalf("internet question ran into the final coding assistant option:\n%s", agentView)
	}
	for _, line := range strings.Split(agentView, "\n") {
		if strings.Contains(line, "Allow") && strings.Contains(line, "Keep offline") {
			if strings.Index(line, "Allow") > 4 {
				t.Fatalf("internet choices are centered instead of form-aligned: %q", line)
			}
			return
		}
	}
	t.Fatalf("internet choices were not rendered together:\n%s", agentView)
}

func TestSetupShowsManagedStandardBuildProgressAfterConfirmation(t *testing.T) {
	model := NewSetupModel(context.Background(), &setupApplicationStub{}, "/tmp/project", app.SetupPreview{
		Plan: plan.ExecutionPlan{Image: plan.ResolvedImage{Standard: true}},
	}, false)
	model.stage = setupSaving
	model.confirming = true
	view := ansi.Strip(model.View().Content)
	if !strings.Contains(view, "Building DSX Standard") || !strings.Contains(view, "building and verifying") {
		t.Fatalf("managed standard progress view:\n%s", view)
	}
	model.stage = setupPreview
	model.reviewPage = model.reviewPageCount() - 1
	confirmation := ansi.Strip(model.renderReviewPage())
	if normalized := strings.Join(strings.Fields(confirmation), " "); !strings.Contains(normalized, "build DSX Standard") {
		t.Fatalf("managed standard confirmation view: %q", confirmation)
	}
}

func TestSetupAppliesSelectedDetectedImage(t *testing.T) {
	detected := config.ImageConfig{Build: &config.ImageBuild{Context: ".", File: "Dockerfile"}}
	preview := app.SetupPreview{
		Config: config.ConfigDocument{SchemaVersion: 1},
		ImageOptions: []app.SetupImageOption{
			{ID: "standard", Name: "DSX Standard", Available: true, Image: config.ImageConfig{Ref: "example@sha256:" + strings.Repeat("a", 64)}},
			{ID: "dockerfile:Dockerfile", Name: "Project build", Available: true, Image: detected},
			{ID: "custom", Name: "Custom", Available: true},
		},
		SelectedImageOption: "standard",
	}
	model := NewSetupModel(context.Background(), &setupApplicationStub{}, "/tmp/project", preview, false)
	model.imageChoice = "dockerfile:Dockerfile"
	model.applyForm()
	if model.document.Image.Build == nil ||
		model.document.Image.Build.Context != detected.Build.Context ||
		model.document.Image.Build.File != detected.Build.File ||
		model.document.Image.Ref != "" {
		t.Fatalf("selected image = %#v", model.document.Image)
	}
}

func TestSetupAllowsSelectedSupportedAgent(t *testing.T) {
	preview := app.SetupPreview{
		Config: config.ConfigDocument{
			SchemaVersion: 1,
			Agents:        config.AgentConfig{Default: "codex", Allowed: []string{"codex"}},
		},
	}
	model := NewSetupModel(context.Background(), &setupApplicationStub{}, "/tmp/project", preview, false)
	model.agent = "claude"
	model.applyForm()
	if model.document.Agents.Default != "claude" || !slices.Contains(model.document.Agents.Allowed, "claude") {
		t.Fatalf("selected agent configuration = %#v", model.document.Agents)
	}
}

func TestSetupResourceScreenDefaultsAndAppliesSelections(t *testing.T) {
	application := &setupApplicationStub{}
	model := NewSetupModel(context.Background(), application, "/tmp/project", app.SetupPreview{
		Config: config.ConfigDocument{SchemaVersion: 1},
	}, false)
	if model.cpus != 4 || model.memory != "6GiB" {
		t.Fatalf("resource defaults = %d CPU, %q memory", model.cpus, model.memory)
	}
	model.form.Init()
	model.form.NextGroup()
	model.form.NextGroup()
	resourceView := ansi.Strip(model.form.View())
	for _, expected := range []string{
		"CPU allocation", "4 CPUs (Recommended)", "Memory allocation", "6GiB (Recommended)",
	} {
		if !strings.Contains(resourceView, expected) {
			t.Fatalf("resource screen omitted %q:\n%s", expected, resourceView)
		}
	}

	model.cpus = 8
	model.memory = "12GiB"
	model.applyForm()
	if model.document.Resources.CPUs != 8 || model.document.Resources.Memory != "12GiB" {
		t.Fatalf("selected resources = %#v", model.document.Resources)
	}
	message := model.previewCommand()()
	if _, ok := message.(previewMessage); !ok ||
		application.setupRequest.Config.Resources.CPUs != 8 ||
		application.setupRequest.Config.Resources.Memory != "12GiB" {
		t.Fatalf("resource preview request = %#v, message = %#v", application.setupRequest, message)
	}
}

func TestSetupReviewBackReturnsToEnvironmentWithSelections(t *testing.T) {
	preview := app.SetupPreview{
		Config: config.ConfigDocument{
			SchemaVersion: 1,
			Agents:        config.AgentConfig{Default: "omp", Allowed: []string{"omp"}},
			Resources:     config.ResourceLimits{CPUs: 8, Memory: "12GiB"},
		},
		ImageOptions: []app.SetupImageOption{
			{ID: "standard", Name: "DSX Standard — Ubuntu (Recommended)", Available: true, Image: config.ImageConfig{Standard: true}},
			{ID: "custom", Name: "Use another image", Available: true},
		},
		SelectedImageOption: "standard",
	}
	model := NewSetupModel(context.Background(), &setupApplicationStub{}, "/tmp/project", preview, false)
	model.stage = setupPreview
	model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	review := ansi.Strip(model.View().Content)
	if !strings.Contains(review, "[b] back to environment") {
		t.Fatalf("review omitted environment back action:\n%s", review)
	}

	updated, command := model.Update(tea.KeyPressMsg(tea.Key{Text: "b", Code: 'b'}))
	model = updated.(*SetupModel)
	if command == nil || model.stage != setupForm {
		t.Fatalf("back result: stage=%d command=%v", model.stage, command)
	}
	environment := ansi.Strip(model.form.View())
	if !strings.Contains(environment, "DSX Standard — Ubuntu (Recommended)") {
		t.Fatalf("back did not return to environment screen:\n%s", environment)
	}
	if model.agent != "omp" || model.cpus != 8 || model.memory != "12GiB" {
		t.Fatalf("back lost selections: agent=%q resources=%d/%q", model.agent, model.cpus, model.memory)
	}
}
func TestCompleteReviewUsesReadableCardsWithoutRawJSON(t *testing.T) {
	internet := true
	hostPort := uint16(8080)
	preview := app.SetupPreview{
		Facts: app.ProjectFacts{
			CanonicalRoot: "/tmp/project",
			GitRoots:      []app.DetectedPath{{Path: ".", Kind: "git"}},
			Lockfiles:     []app.DetectedPath{{Path: "package-lock.json", Kind: "javascript"}},
			Dockerfiles:   []app.DetectedPath{{Path: "Dockerfile", Kind: "container"}},
		},
		Config:               config.ConfigDocument{Network: config.NetworkConfig{Internet: &internet}},
		SelectedCapabilities: []string{"workspace", "commands", "credentials", "network", "ports"},
		RenderedConfig:       []byte(`{"raw-json-must-not-render":true}`),
		ConfigContentDigest:  "config-digest",
		ProjectState:         "project-state",
		Hash:                 "approved-hash",
		Plan: plan.ExecutionPlan{
			ContractVersion: plan.ContractVersion,
			Project:         plan.ProjectIdentity{ID: model.ProjectID("project-id"), CanonicalRoot: "/tmp/project"},
			Sandbox:         plan.SandboxIdentity{Name: model.SandboxName("main"), RunID: model.RunID("run-id")},
			Mode:            model.ModeLive,
			Agent:           "codex",
			Image: plan.ResolvedImage{
				Context: ".", File: "Dockerfile", Target: "development", InputDigest: "image-input",
				BuildArgs: []plan.KeyValue{{Key: "FEATURE", Value: "enabled"}},
			},
			Repositories: []plan.RepositoryPlan{{Name: "main", HostPath: "/tmp/project", GuestPath: "/workspace/main", SourceRef: "refs/heads/main", SourceCommit: "commit", TrackedDigest: "tracked"}},
			Setup: []plan.ResolvedCommand{{
				Argv: []string{"npm", "install"}, Cwd: "/workspace/main",
				Env: []plan.EnvGrant{{Name: "NPM_TOKEN", Reference: "secret://npm", Secret: true}},
			}},
			Processes: []plan.ResolvedProcess{{
				Name: "web", Command: plan.ResolvedCommand{Argv: []string{"npm", "run", "dev"}, Cwd: "/workspace/main"},
				Required: true, Health: &plan.ResolvedHealth{
					Kind: "command", Command: &plan.ResolvedCommand{Argv: []string{"healthcheck"}},
					IntervalMS: 1000, TimeoutMS: 500, Retries: 3,
				},
			}},
			Mounts:  []plan.ResolvedMount{{SourceType: "host", Source: "/tmp/project", SourceIdentity: "project-id", Target: "/workspace/main", ReadOnly: false}},
			Volumes: []plan.ResolvedVolume{{Name: "cache", Target: "/cache", Scope: "project", Persistent: true}},
			Auth:    []plan.ResolvedAuthGrant{{Harness: "codex", Profile: "default", Persistence: "global"}},
			Ports: []plan.PortRequest{{
				Name: "web", GuestPort: 3000, Protocol: "tcp", HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: &hostPort,
			}},
			Browser: &plan.BrowserPlan{Enabled: true, ImageReference: "browser@example", ImageDigest: "browser-digest"},
			Bridges: []plan.BridgeGrant{{Kind: "tcp", Name: "database", Destination: "db.internal", Port: 5432, ReadOnly: true}},
			Limits:  plan.ResourceLimits{CPUs: 2, MemoryBytes: 2 << 30, MaxConcurrentClones: 1},
			Ownership: plan.OwnershipPlan{
				ResourceName: "owned-resource",
				Labels:       []plan.KeyValue{{Key: "project", Value: "project-id"}},
			},
			Provenance: map[string]config.SourceRef{"agent": {Kind: "config", Path: ".dsx/config.jsonc", Line: 12}},
		},
	}
	review, err := buildCompleteReview(preview)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"WORKSPACE", "Where DSX will run", "DETECTED PROJECT", "package-lock.json",
		"ACCESS & ISOLATION", "Credentials codex/default", "db.internal:5432", "127.0.0.1:8080",
		"COMMANDS & SERVICES", `"npm" "install"`, "NPM_TOKEN = secret reference secret://npm", "healthcheck",
		"FILES & PERSISTENCE", "refs/heads/main", "cache → /cache", "APPROVAL",
		"owned-resource", "Source agent ← config .dsx/config.jsonc:12", "approved-hash",
		"development", "FEATURE=enabled", "browser@example", "browser-digest",
	} {
		if !strings.Contains(review, expected) {
			t.Fatalf("readable review omitted %q:\n%s", expected, review)
		}
	}
	for _, raw := range []string{`"raw-json-must-not-render"`, `"canonical_root"`, `"contract_version"`} {
		if strings.Contains(review, raw) {
			t.Fatalf("readable review exposed raw JSON field %q:\n%s", raw, review)
		}
	}
}

func TestReviewSectionsUseSeparateColoredViews(t *testing.T) {
	review := strings.Join([]string{
		"WORKSPACE\nWhere DSX will run.\n  Project: /tmp/project\n  Internet: Allowed",
		"DETECTED PROJECT\nWhat DSX found without running code.\n  Diagnostics: 1\n  • warning — unsupported field",
	}, "\n\n")
	pages := reviewSectionPages(review, 100, 40)
	if len(pages) != 2 || pages[0].title != "WORKSPACE" || pages[1].title != "DETECTED PROJECT" {
		t.Fatalf("section pages = %#v", pages)
	}

	theme := newVisualTheme(true)
	workspace := renderReviewSectionPage(pages[0], theme)
	detected := renderReviewSectionPage(pages[1], theme)
	if strings.Contains(ansi.Strip(workspace), "DETECTED PROJECT") || strings.Contains(ansi.Strip(detected), "Where DSX will run") {
		t.Fatalf("review sections shared a view:\nworkspace=%q\ndetected=%q", workspace, detected)
	}
	for _, styled := range []string{
		theme.section.Render("WORKSPACE"),
		theme.label.Render("Project:"),
		theme.value.Render("/tmp/project"),
		theme.success.Render("Allowed"),
		theme.warning.Render("warning — unsupported field"),
	} {
		if !strings.Contains(workspace+detected, styled) {
			t.Fatalf("colored review omitted styled element %q", styled)
		}
	}
	if !strings.Contains(workspace+detected, "\x1b[") {
		t.Fatal("colored review emitted no ANSI styling")
	}
}

func TestLongReviewSectionContinuesWithoutMixingNextSection(t *testing.T) {
	details := make([]string, 18)
	for index := range details {
		details[index] = fmt.Sprintf("  • detected-%02d", index)
	}
	review := "DETECTED PROJECT\nWhat DSX found.\n" + strings.Join(details, "\n") +
		"\n\nAPPROVAL\nExact identity and hashes.\n  Executable hash: approved"
	pages := reviewSectionPages(review, 80, 25)
	if len(pages) < 3 {
		t.Fatalf("long section pages = %d, want continuation pages", len(pages))
	}
	approvalSeen := false
	for _, page := range pages {
		if page.title == "APPROVAL" {
			approvalSeen = true
			continue
		}
		if approvalSeen || page.title != "DETECTED PROJECT" || page.parts < 2 {
			t.Fatalf("mixed or unordered section page: %#v", page)
		}
		for _, line := range page.lines {
			if strings.Contains(line, "APPROVAL") {
				t.Fatalf("approval leaked into detected-project view: %#v", page)
			}
		}
	}
	if !approvalSeen {
		t.Fatal("approval section was lost after continuation pages")
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

func TestProjectScreenExplainsRuntimeAndWorkspaceState(t *testing.T) {
	tests := []struct {
		name       string
		state      app.BareState
		sandboxes  []app.SandboxSummary
		expected   []string
		unexpected []string
	}{
		{
			name: "stopped runtime without workspace",
			state: app.BareState{
				Facts: app.ProjectFacts{CanonicalRoot: "/tmp/project"}, ConfigExists: true,
				ContainerSystem: runtime.SystemStatus{State: runtime.SystemStateStopped},
			},
			expected:   []string{"Container system", "Stopped", "Workspace", "Not created", "Start container system"},
			unexpected: []string{"attach", "git status", "managed resources"},
		},
		{
			name: "running runtime without workspace",
			state: app.BareState{
				Facts: app.ProjectFacts{CanonicalRoot: "/tmp/project"}, ConfigExists: true,
				ContainerSystem: runtime.SystemStatus{State: runtime.SystemStateRunning},
			},
			expected:   []string{"Container system", "Running", "No workspace exists for this project yet", "Create & open", "More options"},
			unexpected: []string{"attach", "stop", "git status", "managed resources"},
		},
		{
			name: "runtime not installed",
			state: app.BareState{
				Facts: app.ProjectFacts{CanonicalRoot: "/tmp/project"}, ConfigExists: true,
				ContainerSystem: runtime.SystemStatus{
					State: runtime.SystemStateNotInstalled, Remediation: "Install Apple Container 1.2.2 to continue.",
				},
			},
			expected:   []string{"Not installed", "Install Apple Container 1.2.2"},
			unexpected: []string{"Create & open", "More options", "attach"},
		},
		{
			name: "running live workspace",
			state: app.BareState{
				Facts: app.ProjectFacts{CanonicalRoot: "/tmp/project"}, ConfigExists: true, OwnedResources: 1,
				ContainerSystem: runtime.SystemStatus{State: runtime.SystemStateRunning},
			},
			sandboxes: []app.SandboxSummary{{Sandbox: "main", Mode: model.ModeLive, State: model.StateRunning}},
			expected:  []string{"Workspace", "main — Running", "workspace is ready", "Attach"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ansi.Strip(NewLauncherModel(test.state, test.sandboxes...).View().Content)
			for _, expected := range test.expected {
				if !strings.Contains(got, expected) {
					t.Fatalf("view missing %q: %q", expected, got)
				}
			}
			for _, unexpected := range test.unexpected {
				if strings.Contains(strings.ToLower(got), strings.ToLower(unexpected)) {
					t.Fatalf("view unexpectedly contains %q: %q", unexpected, got)
				}
			}
		})
	}
}

func TestProjectPrimaryActionMatchesCurrentState(t *testing.T) {
	tests := []struct {
		name    string
		state   runtime.SystemState
		sandbox *app.SandboxSummary
		action  string
		label   string
	}{
		{name: "start container", state: runtime.SystemStateStopped, action: "start-container-system", label: "Start container system"},
		{name: "create workspace", state: runtime.SystemStateRunning, action: "create", label: "Create & open"},
		{name: "attach running", state: runtime.SystemStateRunning, sandbox: &app.SandboxSummary{Sandbox: "main", Mode: model.ModeLive, State: model.StateRunning}, action: "attach", label: "Attach"},
		{name: "start stopped", state: runtime.SystemStateRunning, sandbox: &app.SandboxSummary{Sandbox: "main", Mode: model.ModeLive, State: model.StateStopped}, action: "start", label: "Start & open"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := app.BareState{
				Facts: app.ProjectFacts{CanonicalRoot: "/tmp/project"}, ConfigExists: true,
				ContainerSystem: runtime.SystemStatus{State: test.state},
			}
			var sandboxes []app.SandboxSummary
			if test.sandbox != nil {
				state.OwnedResources = 1
				sandboxes = append(sandboxes, *test.sandbox)
			}
			action := NewLauncherModel(state, sandboxes...)
			if gotAction, gotLabel := action.primaryAction(); gotAction != test.action || gotLabel != test.label {
				t.Fatalf("primaryAction() = %q, %q; want %q, %q", gotAction, gotLabel, test.action, test.label)
			}
			updated, command := action.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
			if command == nil {
				t.Fatal("primary action did not exit with an intent")
			}
			intent, found := updated.(*ActionModel).Intent()
			if !found || intent.Action != test.action || intent.Project != "/tmp/project" {
				t.Fatalf("intent = %#v, found = %t, want action %q", intent, found, test.action)
			}
		})
	}
}

func TestProjectScreenIgnoresHiddenActionShortcuts(t *testing.T) {
	state := app.BareState{
		Facts: app.ProjectFacts{CanonicalRoot: "/tmp/project"}, ConfigExists: true,
		ContainerSystem: runtime.SystemStatus{State: runtime.SystemStateRunning},
	}
	for _, key := range []string{"a", "s", "x", "d", "g", "v", "f", "n"} {
		action := NewLauncherModel(state)
		updated, command := action.Update(tea.KeyPressMsg(tea.Key{Text: key, Code: rune(key[0])}))
		if command != nil {
			t.Fatalf("hidden key %q exited", key)
		}
		if _, found := updated.(*ActionModel).Intent(); found {
			t.Fatalf("hidden key %q created an intent", key)
		}
	}
}

func TestProjectMoreOptionsShowsOnlyApplicableActions(t *testing.T) {
	state := app.BareState{
		Facts: app.ProjectFacts{CanonicalRoot: "/tmp/project"}, ConfigExists: true,
		ContainerSystem: runtime.SystemStatus{State: runtime.SystemStateRunning},
	}
	action := NewLauncherModel(state)
	updated, _ := action.Update(tea.KeyPressMsg(tea.Key{Text: "m", Code: 'm'}))
	action = updated.(*ActionModel)
	view := ansi.Strip(action.View().Content)
	if !strings.Contains(view, "Create isolated clone") || strings.Contains(view, "Stop selected workspace") || strings.Contains(view, "Git status") || strings.Contains(view, "Clean DSX resources") {
		t.Fatalf("empty-project options = %q", view)
	}
	updated, command := action.Update(tea.KeyPressMsg(tea.Key{Text: "n", Code: 'n'}))
	intent, found := updated.(*ActionModel).Intent()
	if command == nil || !found || intent.Action != "new-clone" {
		t.Fatalf("new clone intent = %#v, found=%t, command=%v", intent, found, command)
	}
}

func TestProjectManageActionsTargetSelectedWorkspace(t *testing.T) {
	state := app.BareState{
		Facts: app.ProjectFacts{CanonicalRoot: "/tmp/project"}, ConfigExists: true, OwnedResources: 2,
		ContainerSystem: runtime.SystemStatus{State: runtime.SystemStateRunning},
	}
	action := NewDashboardModel(state,
		app.SandboxSummary{Sandbox: "z-task", Mode: model.ModeClone, State: model.StateRunning},
		app.SandboxSummary{Sandbox: "api-task", Mode: model.ModeClone, State: model.StateRunning},
	)
	updated, _ := action.Update(tea.KeyPressMsg(tea.Key{Text: "m", Code: 'm'}))
	action = updated.(*ActionModel)
	updated, command := action.Update(tea.KeyPressMsg(tea.Key{Text: "g", Code: 'g'}))
	intent, found := updated.(*ActionModel).Intent()
	if command == nil || !found || intent != (Intent{Action: "git-status", Project: "/tmp/project", Sandbox: "api-task"}) {
		t.Fatalf("git intent = %#v, found=%t", intent, found)
	}

	action = NewDashboardModel(state, app.SandboxSummary{Sandbox: "api-task", Mode: model.ModeClone, State: model.StateRunning})
	updated, _ = action.Update(tea.KeyPressMsg(tea.Key{Text: "m", Code: 'm'}))
	updated, command = updated.(*ActionModel).Update(tea.KeyPressMsg(tea.Key{Text: "x", Code: 'x'}))
	intent, found = updated.(*ActionModel).Intent()
	if command == nil || !found || intent != (Intent{Action: "stop", Project: "/tmp/project", Sandbox: "api-task"}) {
		t.Fatalf("stop intent = %#v, found=%t", intent, found)
	}
}

func TestProjectCleanRequiresManageViewAndConfirmation(t *testing.T) {
	state := app.BareState{
		Facts: app.ProjectFacts{CanonicalRoot: "/tmp/project"}, ConfigExists: true, OwnedResources: 2,
		ContainerSystem: runtime.SystemStatus{State: runtime.SystemStateRunning},
	}
	action := NewDashboardModel(state)
	updated, _ := action.Update(tea.KeyPressMsg(tea.Key{Text: "m", Code: 'm'}))
	action = updated.(*ActionModel)
	updated, command := action.Update(tea.KeyPressMsg(tea.Key{Text: "d", Code: 'd'}))
	action = updated.(*ActionModel)
	if command != nil || !strings.Contains(ansi.Strip(action.View().Content), "Confirm cleanup") {
		t.Fatalf("cleanup confirmation view = %q", action.View().Content)
	}
	updated, command = action.Update(tea.KeyPressMsg(tea.Key{Text: "y", Code: 'y'}))
	intent, found := updated.(*ActionModel).Intent()
	if command == nil || !found || intent.Action != "clean" {
		t.Fatalf("clean intent = %#v, found=%t", intent, found)
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
	preview.Config.Agents.Default = "codex"
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

	action := NewLauncherModel(app.BareState{
		Facts: app.ProjectFacts{CanonicalRoot: hostile}, ConfigExists: true,
		ContainerSystem: runtime.SystemStatus{State: runtime.SystemStateRunning},
	})
	assertTerminalSafe(t, action.View().Content)
	intentModel, _ := action.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	intent, found := intentModel.(*ActionModel).Intent()
	if !found || intent.Project != hostile {
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
			content := ansi.Strip(model.View().Content)
			assertLinesFit(t, content, width)
			normalized := strings.Join(strings.Fields(content), " ")
			if !strings.Contains(content, "REVIEW") || !strings.Contains(content, "More") || !strings.Contains(content, "below") {
				t.Fatalf("width %d lost paginator direction: %q", width, content)
			}
			if width >= 40 && (!strings.Contains(normalized, "REVIEW 1 /") || !strings.Contains(normalized, "Approval locked")) {
				t.Fatalf("width %d lost paginator position or approval status: %q", width, content)
			}
			if width >= 80 && strings.Count(content, "\n")+1 > 24 {
				t.Fatalf("width %d review exceeds terminal height: %d lines\n%s", width, strings.Count(content, "\n")+1, content)
			}
			for model.reviewPage+1 < model.reviewPageCount() {
				updated, _ = model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyPgDown}))
				model = updated.(*SetupModel)
			}
			content = ansi.Strip(model.View().Content)
			assertLinesFit(t, content, width)
			if !strings.Contains(content, "Final confirmation:") || !strings.Contains(content, "[y/N]") {
				t.Fatalf("width %d lost final confirmation: %q", width, content)
			}

			action := NewDashboardModel(app.BareState{
				Facts: preview.Facts, ConfigExists: true,
				ContainerSystem: runtime.SystemStatus{State: runtime.SystemStateRunning},
			})
			updatedAction, _ := action.Update(tea.WindowSizeMsg{Width: width, Height: 24})
			actionContent := ansi.Strip(updatedAction.(*ActionModel).View().Content)
			assertLinesFit(t, actionContent, width)
			controls := []string{"[Enter]", "[m]", "[q]"}
			if width >= 40 {
				controls = []string{"[Enter] Create & open", "[m] More options", "[q] Quit"}
			}
			for _, control := range controls {
				if !strings.Contains(actionContent, control) {
					t.Fatalf("width %d lost %q: %q", width, control, actionContent)
				}
			}
		})
	}
}

func TestWideLayoutsCenterSharedChromeAndFooters(t *testing.T) {
	const width = 120
	leading := func(t *testing.T, content, fragment string) int {
		t.Helper()
		for _, line := range strings.Split(content, "\n") {
			if strings.Contains(line, fragment) {
				return len(line) - len(strings.TrimLeft(line, " "))
			}
		}
		t.Fatalf("missing %q in layout:\n%s", fragment, content)
		return -1
	}

	setup := NewSetupModel(context.Background(), &setupApplicationStub{}, "/tmp/project", app.SetupPreview{
		Facts: app.ProjectFacts{CanonicalRoot: "/tmp/project"},
		Hash:  strings.Repeat("a", 64),
	}, false)
	setup.color = false
	setup.stage = setupPreview
	updated, _ := setup.Update(tea.WindowSizeMsg{Width: width, Height: 40})
	setup = updated.(*SetupModel)
	review := ansi.Strip(setup.View().Content)
	assertLinesFit(t, review, width)
	if got := leading(t, review, "╭"); got != 4 {
		t.Fatalf("review panel left padding = %d, want 4:\n%s", got, review)
	}
	if got := leading(t, review, "DSX"); got < 4 {
		t.Fatalf("review header was outside centered column: %d", got)
	}
	if got := leading(t, review, "Environment"); got <= 4 {
		t.Fatalf("stepper was not centered over the panel: %d", got)
	}
	if got := leading(t, review, "Approval locked"); got <= 4 {
		t.Fatalf("review footer was not centered under the panel: %d", got)
	}

	dashboard := NewDashboardModel(app.BareState{
		Facts: app.ProjectFacts{CanonicalRoot: "/tmp/project"}, ConfigExists: true,
		ContainerSystem: runtime.SystemStatus{State: runtime.SystemStateRunning},
	})
	updatedDashboard, _ := dashboard.Update(tea.WindowSizeMsg{Width: width, Height: 40})
	dashboardView := ansi.Strip(updatedDashboard.(*ActionModel).View().Content)
	assertLinesFit(t, dashboardView, width)
	if got := leading(t, dashboardView, "╭"); got != 4 {
		t.Fatalf("dashboard panel left padding = %d, want 4:\n%s", got, dashboardView)
	}
	if got := leading(t, dashboardView, "[Enter] Create & open"); got != 4 {
		t.Fatalf("project controls left padding = %d, want shared panel padding 4", got)
	}
}

func TestAccessibleSetupUsesPlainPromptsAndSanitizedReview(t *testing.T) {
	hostile := "/tmp/project\x1b]52;c;Y2xpcA==\a\u202e"
	preview := app.SetupPreview{
		Facts: app.ProjectFacts{CanonicalRoot: hostile},
		Config: config.ConfigDocument{
			SchemaVersion: 1,
			Image:         config.ImageConfig{Build: &config.ImageBuild{Context: ".", File: "Dockerfile"}},
		},
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
		Input:       strings.NewReader("1\n\n\n\n\n" + strings.Repeat("\n", pageCount-1) + "y\n"),
		Output:      &output,
	}
	_, found, err := runner.Run(context.Background(), RunRequest{Root: hostile, ForceSetup: true, Accessible: true})
	if err != nil {
		t.Fatalf("accessible Run() error = %v", err)
	}
	if found || application.initializes != 1 {
		t.Fatalf("found = %t, initialize calls = %d, output = %q", found, application.initializes, output.String())
	}
	got := output.String()
	assertTerminalSafe(t, got)
	for _, expected := range []string{"Configured image", "Coding assistant", "Codex", "Claude Code", "OMP", "OpenCode", "Let this workspace access the internet?", "Final confirmation:", "DSX setup complete", `\x1b]52`, `\a`, `\u202e`} {
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
	if !strings.Contains(output.String(), "ACCESS & ISOLATION") || !strings.Contains(output.String(), "Executable hash") || !strings.Contains(output.String(), "Final confirmation:") {
		t.Fatalf("approval output omitted readable complete review: %q", output.String())
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
	if !strings.Contains(output.String(), "ACCESS & ISOLATION") || !strings.Contains(output.String(), "Executable hash") || !strings.Contains(output.String(), "DSX execution approved") {
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
		pages := reviewSectionPages(model.review, model.width, model.height)
		sectionView := renderReviewSectionPage(pages[model.reviewPage], newVisualTheme(model.color))
		tailVisible = tailVisible || strings.Contains(sectionView, tailGrant)
		if model.reviewPage+1 == model.reviewPageCount() {
			if !tailVisible {
				t.Fatal("reached final confirmation without displaying the complete tail trust grant")
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
	tooLarge.Plan.Bridges = append(tooLarge.Plan.Bridges, plan.BridgeGrant{Name: "oversized", Destination: strings.Repeat("z", maxSetupReviewBytes+1)})
	refused := NewSetupModel(context.Background(), application, "/tmp/project", tooLarge, false)
	refused.stage = setupPreview
	content := ansi.Strip(refused.View().Content)
	normalized := strings.Join(strings.Fields(content), " ")
	hasNothing := strings.Contains(normalized, "Nothing") && strings.Contains(normalized, "was truncated")
	hasDisabled := strings.Contains(normalized, "Approval is disabled")
	hasConfirmation := strings.Contains(content, "[y/N]")
	if !hasNothing || !hasDisabled || hasConfirmation {
		t.Fatalf("over-bound review refusal: nothing=%t disabled=%t confirmation=%t normalized=%q", hasNothing, hasDisabled, hasConfirmation, normalized)
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
	value = ansi.Strip(value)
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
	escapedOSC := strings.Contains(raw, `\u001b]52;c;Y2xpcA==`) || strings.Contains(raw, `\x1b]52;c;Y2xpcA==`)
	if !escapedOSC || !strings.Contains(raw, `\u202e`) {
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
