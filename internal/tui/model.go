package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/config"
	modelpkg "github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/terminal"
)

const maxSetupReviewBytes = 512 * 1024

var errSetupReviewTooLarge = errors.New("complete setup review exceeds the safe display bound")

// Application is the setup/bare-state boundary. Models never write files or
// perform lifecycle transitions themselves.
type Application interface {
	BareState(context.Context, app.BareStateRequest) (app.BareState, error)
	PreviewSetup(context.Context, app.SetupPreviewRequest) (app.SetupPreview, error)
	PreviewExisting(context.Context, app.BareStateRequest) (app.SetupPreview, error)
	PreviewClone(context.Context, app.ClonePreviewRequest) (app.SetupPreview, error)
	Initialize(context.Context, app.InitializeRequest) (app.InitializeResult, error)
	ApproveExisting(context.Context, app.InitializeRequest) (app.InitializeResult, error)
}

type Intent struct {
	Action        string
	Project       string
	Sandbox       string
	Repository    string
	Agent         string
	Profile       string
	Prompt        string
	Browser       bool
	ApproveConfig string
}

type setupStage int

const (
	setupForm setupStage = iota
	setupPreview
	setupSaving
	setupDone
)

type SetupModel struct {
	ctx           context.Context
	application   Application
	root          string
	form          *huh.Form
	stage         setupStage
	preview       app.SetupPreview
	document      config.ConfigDocument
	image         string
	agent         string
	initialImage  string
	initialAgent  string
	internet      bool
	confirming    bool
	accessible    bool
	width         int
	color         bool
	height        int
	result        app.InitializeResult
	err           error
	review        string
	reviewPage    int
	reviewRefused error
	approveOnly   bool
	reviewOnly    bool
}

type previewMessage struct {
	preview app.SetupPreview
	err     error
}

type initializeMessage struct {
	result app.InitializeResult
	err    error
}

func NewSetupModel(ctx context.Context, application Application, root string, initial app.SetupPreview, accessible bool) *SetupModel {
	if ctx == nil {
		ctx = context.Background()
	}
	model := &SetupModel{
		ctx:          ctx,
		application:  application,
		root:         root,
		stage:        setupForm,
		preview:      initial,
		document:     initial.Config,
		image:        terminal.SanitizeLine(initial.Config.Image.Ref),
		agent:        terminal.SanitizeLine(initial.Config.Agents.Default),
		initialImage: initial.Config.Image.Ref,
		initialAgent: initial.Config.Agents.Default,
		internet:     true,
		accessible:   accessible,
		width:        80,
		color:        terminal.ColorEnabled(),
		height:       24,
	}
	if initial.Config.Network.Internet != nil {
		model.internet = *initial.Config.Network.Internet
	}
	model.form = huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("Pinned image reference").Description("Use a full @sha256 digest; leave unchanged when using the detected Dockerfile build.").Value(&model.image),
		huh.NewInput().Title("Default coding agent").Value(&model.agent),
		huh.NewConfirm().Title("Allow internet access?").Affirmative("Allow").Negative("Deny").Value(&model.internet),
	)).WithAccessible(accessible)
	if accessible || !model.color {
		model.form.WithTheme(huh.ThemeFunc(huh.ThemeBase))
	}
	model.resetReview(initial)
	return model
}

func NewExistingApprovalModel(ctx context.Context, application Application, root string, initial app.SetupPreview, accessible bool) *SetupModel {
	model := NewSetupModel(ctx, application, root, initial, accessible)
	model.approveOnly = true
	model.stage = setupPreview
	return model
}

func NewPlanReviewModel(ctx context.Context, application Application, root string, initial app.SetupPreview, accessible bool) *SetupModel {
	model := NewSetupModel(ctx, application, root, initial, accessible)
	model.reviewOnly = true
	model.stage = setupPreview
	return model
}

func (model *SetupModel) Init() tea.Cmd {
	if model.form == nil {
		return nil
	}
	return model.form.Init()
}

func (model *SetupModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		resized := false
		if message.Width > 0 && message.Width != model.width {
			model.width = message.Width
			resized = true
		}
		if message.Height > 0 && message.Height != model.height {
			model.height = message.Height
			resized = true
		}
		if resized {
			model.reviewPage = 0
		}
	case tea.KeyPressMsg:
		if message.String() == "ctrl+c" || message.String() == "esc" && model.stage != setupForm {
			return model, tea.Quit
		}
		if model.stage == setupPreview && !model.confirming {
			switch strings.ToLower(message.String()) {
			case "pgdown", "down", "j", "right", " ":
				if model.reviewRefused == nil && model.reviewPage+1 < model.reviewPageCount() {
					model.reviewPage++
				}
			case "pgup", "up", "k", "left":
				if model.reviewPage > 0 {
					model.reviewPage--
				}
			case "home":
				model.reviewPage = 0
			case "y":
				if model.reviewRefused != nil || model.reviewPage+1 != model.reviewPageCount() {
					return model, nil
				}
				model.confirming = true
				model.stage = setupSaving
				return model, model.initializeCommand()
			case "n", "q":
				return model, tea.Quit
			}
		}
	case previewMessage:
		model.err = message.err
		if message.err == nil {
			model.preview = message.preview
			model.resetReview(message.preview)
			model.stage = setupPreview
		}
		return model, nil
	case initializeMessage:
		model.err = message.err
		if message.err == nil {
			model.result = message.result
			model.stage = setupDone
			return model, tea.Quit
		}
		model.stage = setupPreview
		model.confirming = false
		return model, nil
	}

	if model.stage == setupForm && model.form != nil {
		updated, command := model.form.Update(message)
		if form, ok := updated.(*huh.Form); ok {
			model.form = form
		}
		switch model.form.State {
		case huh.StateAborted:
			return model, tea.Quit
		case huh.StateCompleted:
			model.applyForm()
			model.stage = setupSaving
			return model, tea.Batch(command, model.previewCommand())
		}
		return model, command
	}
	return model, nil
}

func (model *SetupModel) View() tea.View {
	var content string
	if model.err != nil {
		content = fmt.Sprintf("DSX setup\n\nError: %s\n\nPress Ctrl-C to exit.\n", terminal.Sanitize(model.err.Error()))
	} else {
		switch model.stage {
		case setupForm:
			formView := model.form.View()
			if !model.color {
				formView = ansi.Strip(formView)
			}
			content = "DSX project setup\n\n" + formView
		case setupSaving:
			content = "DSX setup\n\nValidating the complete effective plan…\n"
		case setupDone:
			content = fmt.Sprintf("DSX setup complete\n\nConfiguration: %s\nApproved hash: %s\n", terminal.SanitizeLine(model.result.ConfigPath), terminal.SanitizeLine(model.result.Hash))
		default:
			content = model.renderReviewPage()
		}
	}
	view := tea.NewView(terminal.Wrap(content, model.width))
	view.AltScreen = !model.accessible
	return view
}

func (model *SetupModel) applyForm() {
	if model.document.SchemaVersion == 0 {
		model.document = model.preview.Config
	}
	if model.document.Image.Build == nil {
		image := strings.TrimSpace(model.image)
		if image == terminal.SanitizeLine(model.initialImage) {
			image = model.initialImage
		}
		model.document.Image.Ref = image
	}
	agent := strings.TrimSpace(model.agent)
	if agent == terminal.SanitizeLine(model.initialAgent) {
		agent = model.initialAgent
	}
	model.document.Agents.Default = agent
	if model.document.Agents.Default != "" && len(model.document.Agents.Allowed) == 0 {
		model.document.Agents.Allowed = []string{model.document.Agents.Default}
	}
	internet := model.internet
	model.document.Network.Internet = &internet
}

func (model *SetupModel) previewCommand() tea.Cmd {
	return func() tea.Msg {
		preview, err := model.application.PreviewSetup(model.ctx, app.SetupPreviewRequest{Root: model.root, Config: model.document})
		return previewMessage{preview: preview, err: err}
	}
}

func (model *SetupModel) initializeCommand() tea.Cmd {
	request := model.approvalRequest()
	return func() tea.Msg {
		var result app.InitializeResult
		var err error
		if model.reviewOnly {
			result = app.InitializeResult{Hash: model.preview.Hash}
		} else if model.approveOnly {
			result, err = model.application.ApproveExisting(model.ctx, request)
		} else {
			result, err = model.application.Initialize(model.ctx, request)
		}
		return initializeMessage{result: result, err: err}
	}
}

func (model *SetupModel) approvalRequest() app.InitializeRequest {
	return app.InitializeRequest{
		Root:                           model.root,
		ExpectedHash:                   model.preview.Hash,
		ExpectedConfigDigest:           model.preview.ConfigContentDigest,
		ExpectedImportedContentDigests: slices.Clone(model.preview.ImportedContentDigests),
		ExpectedProjectState:           model.preview.ProjectState,
		Confirmed:                      true,
		RenderedConfig:                 append([]byte(nil), model.preview.RenderedConfig...),
	}
}

func (model *SetupModel) resetReview(preview app.SetupPreview) {
	model.review, model.reviewRefused = buildCompleteReview(preview)
	model.reviewPage = 0
}

func (model *SetupModel) reviewPageCount() int {
	return len(reviewPages(model.review, model.width, model.height))
}

func (model *SetupModel) renderReviewPage() string {
	if model.reviewRefused != nil {
		return fmt.Sprintf("DSX setup review unavailable\n\nThe complete rendered configuration and plan exceed the %d-byte sanitized review bound. Nothing was truncated. Approval is disabled; reduce the configuration or plan and retry.\n\n[q] quit\n", maxSetupReviewBytes)
	}
	pages := reviewPages(model.review, model.width, model.height)
	if model.reviewPage >= len(pages) {
		model.reviewPage = len(pages) - 1
	}
	position := fmt.Sprintf("Review page %d/%d", model.reviewPage+1, len(pages))
	if model.reviewPage+1 == len(pages) {
		return pages[model.reviewPage] + "\n\n" + position + "\nFinal confirmation: write configuration and persist this approval? [y/N]\n[PgUp] back  [q] quit\n"
	}
	return pages[model.reviewPage] + "\n\n" + position + "\n[PgDn] next  [PgUp] back  [q] quit\nApproval is locked until the final page is visible.\n"
}

func buildCompleteReview(preview app.SetupPreview) (string, error) {
	facts, err := json.MarshalIndent(preview.Facts, "", "  ")
	if err != nil {
		return "", fmt.Errorf("render detected facts for review: %w", err)
	}
	plan, err := json.MarshalIndent(preview.Plan, "", "  ")
	if err != nil {
		return "", fmt.Errorf("render effective plan for review: %w", err)
	}
	capabilities := strings.Join(preview.SelectedCapabilities, ", ")
	if capabilities == "" {
		capabilities = "none"
	}
	builder := terminal.NewSanitizedBuilder(maxSetupReviewBytes)
	for _, section := range []string{
		"DSX setup review\n\nDetected facts:\n",
		string(facts),
		"\n\nSelected capabilities: ",
		capabilities,
		"\n\nRendered configuration:\n",
		string(preview.RenderedConfig),
		"\nComplete effective plan (commands, mounts, credentials, network grants, and ports):\n",
		string(plan),
		"\n\nExecutable hash: ",
		preview.Hash,
		"\n",
	} {
		if !builder.WriteString(section) {
			return "", errSetupReviewTooLarge
		}
	}
	if !builder.Complete() {
		return "", errSetupReviewTooLarge
	}
	return builder.String(), nil
}

func reviewPages(review string, width, height int) []string {
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	lines := strings.Split(terminal.Wrap(review, width), "\n")
	linesPerPage := max(1, height-8)
	pages := make([]string, 0, (len(lines)+linesPerPage-1)/linesPerPage)
	for start := 0; start < len(lines); start += linesPerPage {
		end := min(start+linesPerPage, len(lines))
		pages = append(pages, strings.Join(lines[start:end], "\n"))
	}
	if len(pages) == 0 {
		return []string{""}
	}
	return pages
}

type ActionModel struct {
	state        app.BareState
	title        string
	intent       *Intent
	width        int
	accessible   bool
	dashboard    bool
	confirmClean bool
	sandboxes    []app.SandboxSummary
	selected     int
	notice       string
}

func NewLauncherModel(state app.BareState) *ActionModel {
	return &ActionModel{state: state, title: "DSX project launcher", width: 80}
}

func NewDashboardModel(state app.BareState, sandboxes ...app.SandboxSummary) *ActionModel {
	available := make([]app.SandboxSummary, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		name, err := modelpkg.ParseSandboxName(string(sandbox.Sandbox))
		if err != nil || name == "main" || sandbox.Mode != modelpkg.ModeClone {
			continue
		}
		available = append(available, sandbox)
	}
	slices.SortStableFunc(available, func(left, right app.SandboxSummary) int {
		return strings.Compare(string(left.Sandbox), string(right.Sandbox))
	})
	return &ActionModel{state: state, title: "DSX project dashboard", width: 80, dashboard: true, sandboxes: available}
}

func (model *ActionModel) Init() tea.Cmd { return nil }

func (model *ActionModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := message.(tea.WindowSizeMsg); ok {
		if size.Width > 0 {
			model.width = size.Width
		}
		return model, nil
	}
	key, ok := message.(tea.KeyPressMsg)
	if !ok {
		return model, nil
	}
	if model.confirmClean {
		switch strings.ToLower(key.String()) {
		case "ctrl+c", "q":
			return model, tea.Quit
		case "y", "enter":
			model.intent = &Intent{Action: "clean", Project: model.state.Facts.CanonicalRoot}
			return model, tea.Quit
		case "n", "esc":
			model.confirmClean = false
		}
		return model, nil
	}
	switch strings.ToLower(key.String()) {
	case "up", "k":
		if len(model.sandboxes) != 0 {
			model.selected = (model.selected - 1 + len(model.sandboxes)) % len(model.sandboxes)
			model.notice = ""
		}
		return model, nil
	case "down", "j":
		if len(model.sandboxes) != 0 {
			model.selected = (model.selected + 1) % len(model.sandboxes)
			model.notice = ""
		}
		return model, nil
	}
	action := ""
	switch strings.ToLower(key.String()) {
	case "ctrl+c", "q", "esc":
		return model, tea.Quit
	case "c":
		if model.dashboard {
			action = "new-clone"
		} else {
			action = "create"
		}
	case "n":
		action = "new-clone"
	case "a":
		action = "attach"
	case "s":
		action = "start"
	case "x":
		action = "stop"
	case "d":
		model.confirmClean = true
		return model, nil
	case "g":
		return model.selectGitAction("status")
	case "v":
		return model.selectGitAction("diff")
	case "f":
		return model.selectGitAction("fetch")
	}
	if action != "" {
		intent := Intent{Action: action, Project: model.state.Facts.CanonicalRoot}
		if action == "stop" && model.dashboard && len(model.sandboxes) != 0 {
			intent.Sandbox = string(model.sandboxes[model.selected].Sandbox)
		}
		model.intent = &intent
		return model, tea.Quit
	}
	return model, nil
}

func (model *ActionModel) selectGitAction(operation string) (tea.Model, tea.Cmd) {
	if !model.dashboard || len(model.sandboxes) == 0 {
		model.notice = fmt.Sprintf("Git %s unavailable: select a named clone sandbox.", operation)
		return model, nil
	}
	model.intent = &Intent{
		Action:  "git-" + operation,
		Project: model.state.Facts.CanonicalRoot,
		Sandbox: string(model.sandboxes[model.selected].Sandbox),
	}
	return model, tea.Quit
}

func (model *ActionModel) View() tea.View {
	if model.confirmClean {
		content := fmt.Sprintf("%s\n\nRemove all DSX-owned resources for project %s?\n\n[y] confirm cleanup  [n] cancel\n",
			terminal.SanitizeLine(model.title),
			terminal.SanitizeLine(model.state.Facts.CanonicalRoot),
		)
		view := tea.NewView(terminal.Wrap(content, model.width))
		view.AltScreen = !model.accessible
		return view
	}
	var selected string
	if model.dashboard {
		if len(model.sandboxes) == 0 {
			selected = "\nNamed clone selection: unavailable\n"
		} else {
			var entries strings.Builder
			entries.WriteString("\nNamed clone sandboxes (j/k selects):\n")
			for index, sandbox := range model.sandboxes {
				marker := " "
				if index == model.selected {
					marker = ">"
				}
				fmt.Fprintf(&entries, "%s %s (%s)\n", marker, terminal.SanitizeLine(string(sandbox.Sandbox)), terminal.SanitizeLine(string(sandbox.State)))
			}
			selected = entries.String()
		}
	}
	notice := ""
	if model.notice != "" {
		notice = "\n" + terminal.SanitizeLine(model.notice) + "\n"
	}
	createActions := "[c] create live  [n] new clone"
	if model.dashboard {
		createActions = "[c] new clone"
	}
	content := fmt.Sprintf("%s\n\nProject: %s\nConfiguration: %t\nOwned resources: %d\n%s%s\n%s  [a] attach  [s] start  [x] stop  [d] clean  [g] git status  [v] git diff  [f] git fetch  [q] quit\n",
		terminal.SanitizeLine(model.title),
		terminal.SanitizeLine(model.state.Facts.CanonicalRoot),
		model.state.ConfigExists,
		model.state.OwnedResources,
		selected,
		notice,
		createActions,
	)
	view := tea.NewView(terminal.Wrap(content, model.width))
	view.AltScreen = !model.accessible
	return view
}

func (model *ActionModel) Intent() (Intent, bool) {
	if model.intent == nil {
		return Intent{}, false
	}
	return *model.intent, true
}
