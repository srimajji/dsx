package tui

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	"github.com/srimajji/dsx/internal/terminal"
)

type setupApplicationStub struct {
	initializes      int
	previews         int
	existingPreviews int
	approvals        int
	request          app.InitializeRequest
	setupRequest     app.SetupPreviewRequest
	preview          app.SetupPreview
	bareState        app.BareState
	previewErr       error
	initializeErr    error
}

func (stub *setupApplicationStub) BareState(context.Context, app.BareStateRequest) (app.BareState, error) {
	if stub.bareState.Screen == "" {
		return app.BareState{Screen: app.BareSetup}, nil
	}
	return stub.bareState, nil
}

func (stub *setupApplicationStub) PreviewSetup(_ context.Context, request app.SetupPreviewRequest) (app.SetupPreview, error) {
	stub.previews++
	stub.setupRequest = request
	return stub.preview, stub.previewErr
}

func (stub *setupApplicationStub) PreviewExisting(context.Context, app.BareStateRequest) (app.SetupPreview, error) {
	stub.existingPreviews++
	return stub.preview, stub.previewErr
}

func (stub *setupApplicationStub) Initialize(_ context.Context, request app.InitializeRequest) (app.InitializeResult, error) {
	stub.initializes++
	stub.request = request
	if stub.initializeErr != nil {
		return app.InitializeResult{}, stub.initializeErr
	}
	stub.bareState = app.BareState{
		Screen:       app.BareDashboard,
		ConfigExists: true,
		Facts:        app.ProjectFacts{CanonicalRoot: request.Root},
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

func (stub *setupApplicationStub) UpdateExisting(_ context.Context, request app.InitializeRequest) (app.InitializeResult, error) {
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

func TestSetupFormOffersOnlyDefaultAndCustomUbuntu(t *testing.T) {
	preview := app.SetupPreview{
		Config: config.ConfigDocument{
			SchemaVersion: 1,
			Agents:        config.AgentConfig{Default: "codex", Allowed: []string{"codex"}},
		},
		ImageOptions: []app.SetupImageOption{
			{ID: "standard", Name: "DSX Standard", Available: true, Image: config.ImageConfig{Standard: true}},
			{ID: "dockerfile:Dockerfile", Name: "Project Dockerfile", Available: true},
			{ID: "custom", Name: "Use another image", Available: true},
		},
	}
	model := NewSetupModel(context.Background(), &setupApplicationStub{}, "/tmp/project", preview, false)
	model.form.Init()
	view := ansi.Strip(model.form.View())
	for _, expected := range []string{"Ubuntu — Default settings", "Ubuntu — Custom", "6 CPUs", "6 GiB", "network allowed", "no browser", "AWS capability", "None — no host AWS access", "Follow host default — selected workspaces only"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("setup choice omitted %q:\n%s", expected, view)
		}
	}
	for _, forbidden := range []string{"Use another image", "Project Dockerfile", "Container image reference", "Coding assistant"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("default setup exposed %q:\n%s", forbidden, view)
		}
	}

	model.setupChoice = "ubuntu-custom"
	model.form.Init()
	model.form.NextGroup()
	customView := ansi.Strip(model.form.View())
	for _, expected := range []string{"Coding assistant", "Codex", "Claude Code", "OMP", "OpenCode", "Published guest ports", "CPU allocation", "Memory allocation"} {
		if !strings.Contains(customView, expected) {
			t.Fatalf("custom setup omitted %q:\n%s", expected, customView)
		}
	}
}

func TestSetupAWSCapabilityDefaultsOffAndAppliesHostDefaultDraft(t *testing.T) {
	t.Setenv("HOME", "/Users/example")
	model := NewSetupModel(context.Background(), &setupApplicationStub{}, "/tmp/project", app.SetupPreview{
		Config: config.ConfigDocument{SchemaVersion: 1},
	}, false)
	if model.awsMode != plan.AWSModeNone || model.awsDirectory != "/Users/example/.aws" {
		t.Fatalf("default AWS choice = mode %q, directory %q", model.awsMode, model.awsDirectory)
	}
	model.applyForm()
	if model.document.AWS.Mode != plan.AWSModeNone || model.document.AWS.Directory != "" {
		t.Fatalf("default AWS draft = %#v", model.document.AWS)
	}

	model.awsMode = plan.AWSModeHostDefault
	model.awsDirectory = "/Users/example/.aws"
	model.applyForm()
	model.buildSetupForm()
	model.form.Init()
	model.form.NextGroup()
	directoryView := ansi.Strip(model.form.View())
	for _, expected := range []string{"Host AWS directory", "Leapp Desktop or a compatible provider", "Named profiles are unavailable"} {
		if !strings.Contains(directoryView, expected) {
			t.Fatalf("host-default directory form omitted %q:\n%s", expected, directoryView)
		}
	}
	if model.document.AWS != (config.AWSConfig{Mode: plan.AWSModeHostDefault, Directory: "/Users/example/.aws"}) {
		t.Fatalf("host-default AWS draft = %#v", model.document.AWS)
	}
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

func TestWorkspaceProgressShowsBoundedMilestones(t *testing.T) {
	request := ProgressRequest{
		Title:   "Creating workspace",
		Project: "/tmp/project",
		Detail:  "DSX is creating the approved workspace and waiting until it is ready.",
		Steps: []ProgressStep{
			{ID: "validate", Label: "Validate approved project plan"},
			{ID: "workspace", Label: "Create and start workspace"},
			{ID: "ready", Label: "Workspace ready"},
		},
	}
	model := newProgressModel(context.Background(), func() {}, request, func(context.Context, func(string)) error {
		return nil
	})
	model.current = 1
	view := strings.ToLower(ansi.Strip(model.View().Content))
	for _, expected := range []string{
		"creating workspace",
		"validate approved project plan",
		"create and start workspace",
		"workspace ready",
		"command output stays hidden",
	} {
		if !strings.Contains(view, expected) {
			t.Fatalf("progress view missing %q:\n%s", expected, view)
		}
	}
}

func TestSetupAlwaysAppliesStandardUbuntuImage(t *testing.T) {
	standard := config.ImageConfig{Ref: "example@sha256:" + strings.Repeat("a", 64)}
	preview := app.SetupPreview{
		Config: config.ConfigDocument{SchemaVersion: 1},
		ImageOptions: []app.SetupImageOption{
			{ID: "standard", Name: "DSX Standard", Available: true, Image: standard},
			{ID: "dockerfile:Dockerfile", Name: "Project build", Available: true, Image: config.ImageConfig{Build: &config.ImageBuild{Context: ".", File: "Dockerfile"}}},
		},
	}
	model := NewSetupModel(context.Background(), &setupApplicationStub{}, "/tmp/project", preview, false)
	model.applyForm()
	if model.document.Image.Ref != standard.Ref || model.document.Image.Build != nil {
		t.Fatalf("selected image = %#v, want standard Ubuntu", model.document.Image)
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
	model.setupChoice = "ubuntu-custom"
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
	if model.cpus != 6 || model.memory != "6GiB" {
		t.Fatalf("resource defaults = %d CPU, %q memory", model.cpus, model.memory)
	}
	model.setupChoice = "ubuntu-custom"
	model.buildSetupForm()
	model.form.Init()
	model.form.NextGroup()
	resourceView := ansi.Strip(model.form.View())
	for _, expected := range []string{
		"Published guest ports", "CPU allocation", "6 CPUs (Recommended)", "Memory allocation", "6GiB (Recommended)",
	} {
		if !strings.Contains(resourceView, expected) {
			t.Fatalf("custom resource screen omitted %q:\n%s", expected, resourceView)
		}
	}

	model.setupChoice = "ubuntu-custom"
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

func TestSetupParsesGuestPortsIntoDynamicLoopbackMappings(t *testing.T) {
	ports, err := parseGuestPorts(" 8080,3001 ")
	if err != nil {
		t.Fatal(err)
	}
	want := []config.PortConfig{
		{Name: "port-3001", Guest: 3001, Host: config.HostPort{Dynamic: true}, Bind: "127.0.0.1", Protocol: "tcp"},
		{Name: "port-8080", Guest: 8080, Host: config.HostPort{Dynamic: true}, Bind: "127.0.0.1", Protocol: "tcp"},
	}
	if !reflect.DeepEqual(ports, want) {
		t.Fatalf("ports = %#v, want %#v", ports, want)
	}
	for _, input := range []string{"0", "65536", "8080,not-a-port"} {
		if _, err := parseGuestPorts(input); err == nil {
			t.Fatalf("parseGuestPorts(%q) succeeded", input)
		}
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
	if !strings.Contains(environment, "Ubuntu — Default settings") {
		t.Fatalf("back did not return to Ubuntu choice:\n%s", environment)
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
			Agents:          plan.AgentPlan{Allowed: []string{"codex", "omp"}, Default: "codex"},
			Image: plan.ResolvedImage{
				Context: ".", File: "Dockerfile", Target: "development", InputDigest: "image-input",
				BuildArgs: []plan.KeyValue{{Key: "FEATURE", Value: "enabled"}},
			},
			Repositories: []plan.RepositoryPlan{{Name: "main", HostPath: "/tmp/project", GuestPath: "/workspace/main", TrackedDigest: "tracked"}},
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
			Mounts:  []plan.ResolvedMount{{SourceType: "project-config", Source: "/tmp/config", SourceIdentity: "config-digest", Target: "/etc/dsx/project", ReadOnly: true}},
			Volumes: []plan.ResolvedVolume{{Name: "cache", Target: "/cache", Scope: "project", Persistent: true}},
			Auth:    plan.AuthPlan{Imports: []string{"codex"}},
			Ports: []plan.PortRequest{{
				Name: "web", GuestPort: 3000, Protocol: "tcp", HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: &hostPort,
			}},
			Bridges: []plan.BridgeGrant{{Kind: "tcp", Name: "database", Destination: "db.internal", Port: 5432, ReadOnly: true}},
			AWS: plan.AWSCapability{
				Mode: "host-default", SourceDirectory: "/Users/sri/.aws", SourceIdentity: "dev=1;ino=2",
				Destination: "/run/dsx/aws", ReadOnly: true, EligibleProfile: "default",
				WorkspaceDefaultEnabled: false, AuthorityModel: "dynamic-host-default",
			},
			Limits:     plan.ResourceLimits{CPUs: 2, MemoryBytes: 2 << 30, MaxConcurrentWorkspaces: 2},
			Provenance: map[string]config.SourceRef{"agent": {Kind: "config", Path: ".dsx/config.jsonc", Line: 12}},
		},
	}
	review, err := buildCompleteReview(preview)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"UBUNTU WORKSPACE", "Environment", "Resources", "2 CPUs", "2 GiB", "Network", "Allowed",
		"Browser", "Disabled", "Agent", "codex", "Port web", "127.0.0.1:8080",
		`Setup command: "npm" "install"`, "NPM_TOKEN = secret reference secret://npm",
		`Service web: "npm" "run" "dev"`, "Mount /tmp/config → /etc/dsx/project • read-only",
		"Authentication import: codex", "Host grant database → db.internal:5432", "Volume cache → /cache",
		"AWS CAPABILITY", "host-default", "/Users/sri/.aws", "dev=1;ino=2", "/run/dsx/aws", "read-only",
		"default only", "Default for new workspaces: Disabled", "dynamic-host-default",
		"Status only, not approval authority", "selected workspaces only", "new workspaces start with AWS access disabled",
		"temporary default session active for enablement and rotation", "Only AWS-enabled running workspaces",
		"without another approval or workspace restart", "Named host profiles are unavailable",
		"APPROVAL", "Executable hash", "approved-hash",
	} {
		if !strings.Contains(review, expected) {
			t.Fatalf("concise review omitted %q:\n%s", expected, review)
		}
	}
	for _, omitted := range []string{
		`"raw-json-must-not-render"`, `"canonical_root"`, `"contract_version"`,
		"PROJECT DEFAULTS", "DETECTED PROJECT", "package-lock.json", "Configuration digest",
		"Project state", "Source agent", "tracked", "FEATURE=enabled",
	} {
		if strings.Contains(review, omitted) {
			t.Fatalf("concise review exposed unnecessary detail %q:\n%s", omitted, review)
		}
	}
}

func TestReviewSectionsUseSeparateColoredViews(t *testing.T) {
	review := strings.Join([]string{
		"PROJECT DEFAULTS\nReusable settings for named workspaces.\n  Project: /tmp/project\n  Internet: Allowed",
		"DETECTED PROJECT\nWhat DSX found without running code.\n  Diagnostics: 1\n  • warning — unsupported field",
	}, "\n\n")
	pages := reviewSectionPages(review, 100, 40)
	if len(pages) != 2 || pages[0].title != "PROJECT DEFAULTS" || pages[1].title != "DETECTED PROJECT" {
		t.Fatalf("section pages = %#v", pages)
	}

	theme := newVisualTheme(true)
	workspace := renderReviewSectionPage(pages[0], theme)
	detected := renderReviewSectionPage(pages[1], theme)
	if strings.Contains(ansi.Strip(workspace), "DETECTED PROJECT") || strings.Contains(ansi.Strip(detected), "Reusable settings") {
		t.Fatalf("review sections shared a view:\nworkspace=%q\ndetected=%q", workspace, detected)
	}
	for _, styled := range []string{
		theme.section.Render("PROJECT DEFAULTS"),
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
		Plan: plan.ExecutionPlan{
			ContractVersion: plan.ContractVersion,
			Bridges:         bridges,
			AWS: plan.AWSCapability{
				Mode: "host-default", SourceDirectory: "/Users/sri/.aws", SourceIdentity: "dev=1;ino=2",
				Destination: "/run/dsx/aws", ReadOnly: true, EligibleProfile: "default",
				AuthorityModel: "dynamic-host-default",
			},
		},
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
	awsWarningVisible := false
	for {
		content := model.View().Content
		pages := reviewSectionPages(model.review, model.width, model.height)
		sectionView := renderReviewSectionPage(pages[model.reviewPage], newVisualTheme(model.color))
		tailVisible = tailVisible || strings.Contains(sectionView, tailGrant)
		awsWarningVisible = awsWarningVisible || strings.Contains(sectionView, "Named host profiles are unavailable")
		if model.reviewPage+1 == model.reviewPageCount() {
			if !tailVisible || !awsWarningVisible {
				t.Fatalf("reached final confirmation without displaying complete authority: tail=%t aws-warning=%t", tailVisible, awsWarningVisible)
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
