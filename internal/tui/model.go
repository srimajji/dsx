package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/config"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/terminal"
)

const (
	maxSetupReviewBytes = 512 * 1024
	maxSetupFormHeight  = 10
)

var errSetupReviewTooLarge = errors.New("complete setup review exceeds the safe display bound")

// Application is the setup/bare-state boundary. Models never write files or
// perform lifecycle transitions themselves.
type Application interface {
	BareState(context.Context, app.BareStateRequest) (app.BareState, error)
	PreviewSetup(context.Context, app.SetupPreviewRequest) (app.SetupPreview, error)
	PreviewExisting(context.Context, app.BareStateRequest) (app.SetupPreview, error)
	Initialize(context.Context, app.InitializeRequest) (app.InitializeResult, error)
	ApproveExisting(context.Context, app.InitializeRequest) (app.InitializeResult, error)
	UpdateExisting(context.Context, app.InitializeRequest) (app.InitializeResult, error)
}

type setupStage int

const (
	setupForm setupStage = iota
	setupPreview
	setupSaving
	setupDone
)

type SetupModel struct {
	ctx                 context.Context
	application         Application
	root                string
	form                *huh.Form
	stage               setupStage
	spinner             spinner.Model
	preview             app.SetupPreview
	document            config.ConfigDocument
	setupChoice         string
	agent               string
	initialAgent        string
	awsMode             string
	awsDirectory        string
	initialConfigDigest string
	internet            bool
	cpus                int
	memory              string
	portInput           string
	confirming          bool
	accessible          bool
	width               int
	color               bool
	height              int
	result              app.InitializeResult
	err                 error
	review              string
	reviewPage          int
	reviewRefused       error
	approveOnly         bool
	updateOnly          bool
	reviewOnly          bool
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
		spinner:      spinner.New(spinner.WithSpinner(spinner.Line)),
		preview:      initial,
		document:     initial.Config,
		setupChoice:  "ubuntu-default",
		agent:        terminal.SanitizeLine(initial.Config.Agents.Default),
		initialAgent: initial.Config.Agents.Default,
		internet:     true,
		cpus:         initial.Config.Resources.CPUs,
		memory:       initial.Config.Resources.Memory,
		portInput:    formatGuestPorts(initial.Config.Ports),
		awsMode:      initial.Config.AWS.Mode,
		awsDirectory: initial.Config.AWS.Directory,
		accessible:   accessible,
		width:        80,
		color:        terminal.ColorEnabled(),
		height:       24,
	}
	if initial.Config.Network.Internet != nil {
		model.internet = *initial.Config.Network.Internet
	}
	if model.cpus == 0 {
		model.cpus = initial.Plan.Limits.CPUs
	}
	if model.cpus == 0 {
		model.cpus = app.DefaultWorkspaceCPUs
	}
	if model.memory == "" {
		model.memory = setupMemoryValue(initial.Plan.Limits.MemoryBytes)
	}
	if model.awsMode == "" {
		model.awsMode = plan.AWSModeNone
	}
	if model.awsDirectory == "" {
		model.awsDirectory = defaultHostAWSDirectory()
	}
	model.buildSetupForm()
	model.resetReview(initial)
	return model
}

func setupMemoryValue(bytes int64) string {
	if bytes <= 0 {
		return app.DefaultWorkspaceMemory
	}
	const (
		mib = int64(1 << 20)
		gib = int64(1 << 30)
	)
	if bytes%gib == 0 {
		return fmt.Sprintf("%dGiB", bytes/gib)
	}
	if bytes%mib == 0 {
		return fmt.Sprintf("%dMiB", bytes/mib)
	}
	return app.DefaultWorkspaceMemory
}

func setupCPUOptions(selected int) []huh.Option[int] {
	values := []int{2, app.DefaultWorkspaceCPUs, 8, 16}
	if !slices.Contains(values, selected) {
		values = append(values, selected)
		slices.Sort(values)
	}
	options := make([]huh.Option[int], 0, len(values))
	for _, value := range values {
		label := fmt.Sprintf("%d CPUs", value)
		if value == app.DefaultWorkspaceCPUs {
			label += " (Recommended)"
		}
		options = append(options, huh.NewOption(label, value))
	}
	return options
}

func setupMemoryOptions(selected string) []huh.Option[string] {
	values := []string{"2GiB", "4GiB", app.DefaultWorkspaceMemory, "8GiB", "12GiB", "16GiB"}
	if !slices.Contains(values, selected) {
		values = append(values, selected)
	}
	options := make([]huh.Option[string], 0, len(values))
	for _, value := range values {
		label := value
		if value == app.DefaultWorkspaceMemory {
			label += " (Recommended)"
		}
		options = append(options, huh.NewOption(label, value))
	}
	return options
}

func formatGuestPorts(ports []config.PortConfig) string {
	values := make([]int, 0, len(ports))
	for _, port := range ports {
		if port.Guest != 0 && !slices.Contains(values, int(port.Guest)) {
			values = append(values, int(port.Guest))
		}
	}
	slices.Sort(values)
	parts := make([]string, len(values))
	for index, value := range values {
		parts[index] = strconv.Itoa(value)
	}
	return strings.Join(parts, ", ")
}

func parseGuestPorts(value string) ([]config.PortConfig, error) {
	fields := strings.FieldsFunc(value, func(character rune) bool {
		return character == ',' || character == ' ' || character == '\t' || character == '\n'
	})
	if len(fields) > 128 {
		return nil, fmt.Errorf("at most 128 guest ports may be published")
	}
	seen := make(map[uint16]struct{}, len(fields))
	ports := make([]config.PortConfig, 0, len(fields))
	for _, field := range fields {
		parsed, err := strconv.ParseUint(field, 10, 16)
		if err != nil || parsed == 0 {
			return nil, fmt.Errorf("guest port %q must be an integer from 1 to 65535", field)
		}
		guest := uint16(parsed)
		if _, duplicate := seen[guest]; duplicate {
			return nil, fmt.Errorf("guest port %d is listed more than once", guest)
		}
		seen[guest] = struct{}{}
		ports = append(ports, config.PortConfig{
			Name: "port-" + strconv.Itoa(int(guest)), Guest: guest,
			Host: config.HostPort{Dynamic: true}, Bind: "127.0.0.1", Protocol: "tcp",
		})
	}
	slices.SortFunc(ports, func(left, right config.PortConfig) int {
		return int(left.Guest) - int(right.Guest)
	})
	return ports, nil
}

func validateGuestPorts(value string) error {
	_, err := parseGuestPorts(value)
	return err
}

func defaultHostAWSDirectory() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	directory := filepath.Join(home, ".aws")
	if physical, resolveErr := filepath.EvalSymlinks(directory); resolveErr == nil {
		return filepath.Clean(physical)
	}
	return filepath.Clean(directory)
}

func validateHostAWSDirectory(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("enter the canonical host directory that contains the standard AWS files")
	}
	if !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return errors.New("AWS directory must be a canonical absolute path")
	}
	return nil
}

func (model *SetupModel) buildSetupForm() {
	ubuntuChoice := huh.NewSelect[string]().
		Title("How should this workspace start?").
		Description("Recommended uses Codex, 6 CPUs, 6 GiB memory, internet access, no published ports, and no browser session.").
		Options(
			huh.NewOption("Ubuntu — Default settings", "ubuntu-default"),
			huh.NewOption("Ubuntu — Custom", "ubuntu-custom"),
		).
		Value(&model.setupChoice)
	awsCapability := huh.NewSelect[string]().
		Title("Will this project use AWS?").
		Description("Optional. Choosing yes only makes AWS available; each new workspace still starts disconnected.").
		Options(
			huh.NewOption("None — no host AWS access", plan.AWSModeNone),
			huh.NewOption("Follow host default — selected workspaces only", plan.AWSModeHostDefault),
		).
		Value(&model.awsMode)
	awsDirectory := huh.NewGroup(
		huh.NewInput().
			Title("Where are the host AWS files?").
			Description("Use the standard AWS directory. Leapp Desktop or a compatible provider must keep one complete temporary [default] session active. Named profiles are not shared.").
			Value(&model.awsDirectory).
			Validate(validateHostAWSDirectory),
	).WithHideFunc(func() bool { return model.awsMode != plan.AWSModeHostDefault })
	custom := huh.NewGroup(
		huh.NewSelect[string]().
			Title("Which coding assistant should open by default?").
			Description("You can choose another approved assistant when opening a session.").
			Options(
				huh.NewOption("Codex", string(harness.Codex)),
				huh.NewOption("Claude Code", string(harness.Claude)),
				huh.NewOption("OMP", string(harness.OMP)),
				huh.NewOption("OpenCode", string(harness.OpenCode)),
			).
			Value(&model.agent),
		huh.NewConfirm().
			Title("Allow internet access?").
			Description("Recommended for package downloads, documentation, and most coding assistants.").
			Affirmative("Allow internet").
			Negative("Keep offline").
			Value(&model.internet).
			WithButtonAlignment(lipgloss.Left),
		huh.NewInput().
			Title("Which app ports should be reachable from this Mac?").
			Description("Optional. Enter guest ports separated by commas, for example 3000, 8080. DSX assigns safe local-only host ports.").
			Value(&model.portInput).
			Validate(validateGuestPorts),
		huh.NewSelect[int]().
			Title("How much CPU should this workspace use?").
			Options(setupCPUOptions(model.cpus)...).
			Value(&model.cpus),
		huh.NewSelect[string]().
			Title("How much memory should this workspace use?").
			Options(setupMemoryOptions(model.memory)...).
			Value(&model.memory),
	).WithHideFunc(func() bool { return model.setupChoice != "ubuntu-custom" })
	model.form = huh.NewForm(
		huh.NewGroup(ubuntuChoice),
		huh.NewGroup(awsCapability),
		awsDirectory,
		custom,
	).WithAccessible(model.accessible)
	if model.accessible || !model.color {
		model.form.WithTheme(huh.ThemeFunc(huh.ThemeBase))
	}
	model.resizeSetupForm()
}

func (model *SetupModel) resizeSetupForm() {
	if model.form == nil {
		return
	}
	theme := newVisualTheme(model.color && !model.accessible)
	width := tuiSetupWidth(model.width)
	formWidth := theme.panelBodyWidth(width, model.height)
	headerHeight := lipgloss.Height(theme.header("Workspace setup", friendlyProjectName(model.root), width))
	stepHeight := lipgloss.Height(theme.stepper(0, width))
	titleHeight := lipgloss.Height(wrapTUIText("Create a Linux workspace for this project", formWidth))
	chromeHeight := headerHeight + stepHeight + titleHeight
	if !compactTUILayout(width, model.height) {
		chromeHeight += theme.border.GetVerticalFrameSize() + 2
	}
	model.form.WithWidth(formWidth).WithHeight(max(1, min(maxSetupFormHeight, model.height-chromeHeight-2)))
}

func NewPortUpdateReviewModel(ctx context.Context, application Application, root string, current, candidate app.SetupPreview, accessible bool) *SetupModel {
	model := NewSetupModel(ctx, application, root, candidate, accessible)
	model.stage = setupPreview
	model.document = candidate.Config
	model.updateOnly = true
	model.initialConfigDigest = current.ConfigContentDigest
	return model
}

func (model *SetupModel) validateImageChoice(string) error {
	return nil
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
		return model.spinner.Tick
	}
	return tea.Batch(model.form.Init(), model.spinner.Tick)
}

func (model *SetupModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case spinner.TickMsg:
		var command tea.Cmd
		model.spinner, command = model.spinner.Update(message)
		return model, command
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
			model.resizeSetupForm()
		}
	case tea.KeyPressMsg:
		if message.String() == "ctrl+c" || message.String() == "esc" && model.stage != setupForm {
			return model, tea.Quit
		}
		if model.stage == setupPreview && !model.confirming {
			switch strings.ToLower(message.String()) {
			case "b":
				if model.approveOnly || model.reviewOnly {
					return model, nil
				}
				model.stage = setupForm
				model.reviewPage = 0
				model.buildSetupForm()
				updated, _ := model.form.Update(tea.WindowSizeMsg{Width: model.width, Height: model.height})
				if form, ok := updated.(*huh.Form); ok {
					model.form = form
				}
				return model, model.form.Init()
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
	theme := newVisualTheme(model.color && !model.accessible)
	width := tuiSetupWidth(model.width)
	header := theme.header("Workspace setup", friendlyProjectName(model.root), width)
	gap := tuiGap(model.height)
	step := 0
	var content string
	if model.err != nil {
		content = header + gap + theme.panel(
			"Something needs attention",
			theme.danger.Render("DSX could not continue")+"\n\n"+terminal.Sanitize(model.err.Error())+"\n\nPress Ctrl-C to exit.",
			width,
			model.height,
			true,
		)
	} else {
		switch model.stage {
		case setupForm:
			formView := model.form.View()
			if !model.color {
				formView = ansi.Strip(formView)
			}
			content = header + gap + theme.stepper(step, width) + gap +
				theme.panel("Create a Linux workspace for this project", formView, width, model.height, true)
		case setupSaving:
			step = 1
			title := "Checking your choices"
			body := model.spinner.View() + " Preparing a complete, safe review\n\n" +
				"[ ] Inspect this project\n[ ] Resolve workspace access\n[ ] Calculate approval identity"
			if model.confirming {
				title = "Applying approved setup"
				body = model.spinner.View() + " Setting up this project\n\n" +
					"[ ] Verify Apple Container\n[ ] Save configuration and approval"
				if model.approveOnly {
					title = "Saving configuration approval"
					body = model.spinner.View() + " Verifying and saving the reviewed approval"
				} else if model.updateOnly {
					title = "Updating published ports"
					body = model.spinner.View() + " Verifying and saving the reviewed configuration"
				} else if model.preview.Plan.Image.Standard {
					title = "Preparing the DSX workspace image"
					body = model.spinner.View() + " Building and verifying DSX Standard\n\n" +
						"[ ] Verify Apple Container\n[ ] Save configuration and approval\n[ ] Build and verify DSX Standard"
				}
				if !model.approveOnly && !model.updateOnly {
					body += "\n[ ] Open the project workspace screen"
				}
			}
			content = header + gap + theme.stepper(step, width) + gap +
				theme.panel(title, body, width, model.height, true)
		case setupDone:
			step = 2
			status := "✓ Workspace configuration saved"
			title := "Ready to create a workspace"
			if model.approveOnly {
				status = "✓ Existing workspace configuration approved"
				title = "Approval saved"
			} else if model.updateOnly {
				status = "✓ Published-port configuration saved"
				title = "Ports updated"
			} else if model.preview.Plan.Image.Standard {
				title = "Ready to use"
			}
			body := theme.success.Render(status) +
				"\n\nConfiguration\n" + terminal.SanitizeLine(model.result.ConfigPath) +
				"\n\nApproval\n" + terminal.SanitizeLine(model.result.Hash)
			content = header + gap + theme.stepper(step, width) + gap +
				theme.panel(title, body, width, model.height, true)
		default:
			content = model.renderReviewPage()
		}
	}
	rendered := content
	if !model.accessible {
		rendered = theme.layoutAt(rendered, model.width, width)
	}
	view := tea.NewView(rendered)
	view.AltScreen = !model.accessible
	return view
}

func (model *SetupModel) applyForm() {
	if model.document.SchemaVersion == 0 {
		model.document = model.preview.Config
	}
	for _, option := range model.preview.ImageOptions {
		if option.ID == "standard" && option.Available {
			model.document.Image = option.Image
			break
		}
	}
	if model.setupChoice == "ubuntu-default" {
		model.agent = string(harness.Codex)
		model.internet = true
		model.cpus = app.DefaultWorkspaceCPUs
		model.memory = app.DefaultWorkspaceMemory
		model.portInput = ""
	}
	agent := strings.TrimSpace(model.agent)
	if agent == terminal.SanitizeLine(model.initialAgent) {
		agent = model.initialAgent
	}
	model.document.Agents.Default = agent
	if model.document.Agents.Default != "" && !slices.Contains(model.document.Agents.Allowed, model.document.Agents.Default) {
		model.document.Agents.Allowed = append(model.document.Agents.Allowed, model.document.Agents.Default)
	}
	switch model.awsMode {
	case plan.AWSModeHostDefault:
		model.document.AWS = config.AWSConfig{
			Mode:      plan.AWSModeHostDefault,
			Directory: strings.TrimSpace(model.awsDirectory),
		}
	default:
		model.document.AWS = config.AWSConfig{Mode: plan.AWSModeNone}
	}
	internet := model.internet
	model.document.Network.Internet = &internet
	model.document.Resources.CPUs = model.cpus
	model.document.Resources.Memory = model.memory
	if ports, err := parseGuestPorts(model.portInput); err == nil {
		model.document.Ports = ports
	}
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
		} else if model.updateOnly {
			result, err = model.application.UpdateExisting(model.ctx, request)
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
		ReplacesConfigDigest:           model.initialConfigDigest,
		RenderedConfig:                 append([]byte(nil), model.preview.RenderedConfig...),
	}
}

func (model *SetupModel) resetReview(preview app.SetupPreview) {
	model.review, model.reviewRefused = buildCompleteReview(preview)
	model.reviewPage = 0
}

func (model *SetupModel) reviewPageCount() int {
	return len(reviewSectionPages(model.review, model.width, model.height))
}

func (model *SetupModel) renderReviewPage() string {
	theme := newVisualTheme(model.color && !model.accessible)
	width := tuiSetupWidth(model.width)
	header := theme.header("Workspace setup", friendlyProjectName(model.root), width)
	gap := tuiGap(model.height)
	helpWidth := theme.panelBodyWidth(width, model.height)
	if model.reviewRefused != nil {
		body := fmt.Sprintf("The complete review exceeds the %d-byte safe display limit. Nothing was truncated. Approval is disabled.\n\nReduce the configuration and retry.", maxSetupReviewBytes)
		footer := theme.center(theme.help(helpWidth, "[q] quit"), width)
		return header + gap + theme.stepper(1, width) + gap +
			theme.panel("Review unavailable", body, width, model.height, true) + gap + footer
	}
	pages := reviewSectionPages(model.review, model.width, model.height)
	if model.reviewPage >= len(pages) {
		model.reviewPage = len(pages) - 1
	}
	position := theme.accent.Render(reviewPageIndicator(model.reviewPage, len(pages)))
	controls := theme.help(helpWidth, "[PgDn/↓] next section", "[PgUp/↑] previous", "[q] quit")
	if !model.approveOnly && !model.reviewOnly {
		controls = theme.help(helpWidth, "[b] change choices", "[PgDn/↓] next section", "[PgUp/↑] previous", "[q] quit")
	}
	status := theme.warning.Render("Approval locked • review every section to continue")
	if model.reviewPage+1 == len(pages) {
		controls = theme.help(helpWidth, "[y] approve", "[PgUp/↑] previous", "[q] quit")
		if !model.approveOnly && !model.reviewOnly {
			controls = theme.help(helpWidth, "[y] approve", "[b] change choices", "[PgUp/↑] previous", "[q] quit")
		}
		confirmation := "save this configuration and approval"
		if model.preview.Plan.Image.Standard && !model.reviewOnly && !model.approveOnly {
			confirmation = "save this configuration and approval, then build DSX Standard"
		}
		status = theme.success.Render("Ready to continue: " + confirmation + "? [y/N]")
	}
	page := position + "\n" + theme.muted.Render(reviewNavigationHint(model.reviewPage, len(pages))) + "\n\n" +
		renderReviewSectionPage(pages[model.reviewPage], theme)
	footer := theme.center(status+"\n"+controls, width)
	return header + gap + theme.stepper(1, width) + gap +
		theme.panel("Review before DSX makes changes", page, width, model.height, true) +
		gap + footer
}

type setupReviewBuilder struct {
	output *terminal.SanitizedBuilder
}

func newSetupReviewBuilder() *setupReviewBuilder {
	return &setupReviewBuilder{output: terminal.NewSanitizedBuilder(maxSetupReviewBytes)}
}

func (builder *setupReviewBuilder) write(parts ...string) {
	for _, part := range parts {
		builder.output.WriteString(part)
	}
}

func (builder *setupReviewBuilder) section(title, description string) {
	if builder.output.String() != "" {
		builder.write("\n")
	}
	builder.write(strings.ToUpper(title), "\n", description, "\n")
}

func (builder *setupReviewBuilder) item(label, value string) {
	if strings.TrimSpace(value) == "" {
		value = "None"
	}
	builder.write("  ", label, ": ", value, "\n")
}

func (builder *setupReviewBuilder) bullet(value string) {
	builder.write("  • ", value, "\n")
}

func buildCompleteReview(preview app.SetupPreview) (string, error) {
	builder := newSetupReviewBuilder()
	buildConciseReview(builder, preview)
	if !builder.output.Complete() {
		return "", errSetupReviewTooLarge
	}
	return builder.output.String(), nil
}

func buildConciseReview(builder *setupReviewBuilder, preview app.SetupPreview) {
	execution := preview.Plan
	builder.section("Ubuntu workspace", "Review the settings and any additional access DSX will approve.")
	builder.item("Environment", reviewImageSummary(preview))
	builder.item("Resources", fmt.Sprintf("%d CPUs • %s", execution.Limits.CPUs, reviewBytes(execution.Limits.MemoryBytes)))
	builder.item("Network", reviewInternetSummary(preview.Config))
	builder.item("Browser", "Disabled")
	builder.item("Agent", execution.Agents.Default)
	if len(execution.Ports) == 0 {
		builder.item("Ports", "None")
	} else {
		for _, port := range execution.Ports {
			hostPort := "dynamic"
			if port.HostPort != nil {
				hostPort = strconv.Itoa(int(*port.HostPort))
			}
			builder.bullet(fmt.Sprintf("Port %s • %s %d → %s:%s", port.Name, port.Protocol, port.GuestPort, port.HostIP, hostPort))
		}
	}
	additional := len(execution.Setup) + len(execution.Processes) + len(execution.Mounts) + len(execution.Auth.Imports) + nonInternetBridgeCount(execution.Bridges) + len(execution.Volumes)
	if additional == 0 {
		builder.item("Additional access or commands", "None")
	} else {
		for _, command := range execution.Setup {
			builder.bullet("Setup command: " + reviewCommand(command))
			buildCommandEnvironment(builder, command.Env)
		}
		for _, process := range execution.Processes {
			builder.bullet("Service " + process.Name + ": " + reviewCommand(process.Command))
			buildCommandEnvironment(builder, process.Command.Env)
		}
		for _, mount := range execution.Mounts {
			access := "read/write"
			if mount.ReadOnly {
				access = "read-only"
			}
			builder.bullet("Mount " + mount.Source + " → " + mount.Target + " • " + access)
		}
		for _, harness := range execution.Auth.Imports {
			builder.bullet("Authentication import: " + harness)
		}
		for _, bridge := range execution.Bridges {
			if bridge.Kind == "internet" {
				continue
			}
			target := bridge.Destination
			if bridge.Port != 0 {
				target += ":" + strconv.Itoa(int(bridge.Port))
			}
			builder.bullet("Host grant " + bridge.Name + " → " + target)
		}
		for _, volume := range execution.Volumes {
			builder.bullet("Volume " + volume.Name + " → " + volume.Target)
		}
	}
	buildAWSCapabilityReview(builder, execution.AWS)
	builder.section("Approval", "The executable identity for this complete project authority.")
	builder.item("Executable hash", preview.Hash)
}

func buildAWSCapabilityReview(builder *setupReviewBuilder, capability plan.AWSCapability) {
	builder.section("AWS capability", "Optional default-only host AWS authority; credential values are never part of this review.")
	if capability.Mode == "" || capability.Mode == plan.AWSModeNone {
		builder.item("Mode", "Disabled — no host AWS credential authority is approved")
		return
	}
	builder.item("Mode", capability.Mode)
	builder.item("Source", capability.SourceDirectory)
	builder.item("Source identity", capability.SourceIdentity)
	builder.item("Destination", capability.Destination)
	access := "read/write"
	if capability.ReadOnly {
		access = "read-only"
	}
	builder.item("Access", access)
	builder.item("Eligible profile", capability.EligibleProfile+" only")
	workspaceDefault := "Enabled"
	if !capability.WorkspaceDefaultEnabled {
		workspaceDefault = "Disabled"
	}
	builder.item("Default for new workspaces", workspaceDefault)
	builder.item("Authority model", capability.AuthorityModel)
	builder.item("Host availability", "Status only, not approval authority — enablement requires a valid temporary host default; if unavailable, start the host default session")
	builder.bullet("This capability is for selected workspaces only; new workspaces start with AWS access disabled.")
	builder.bullet("Leapp Desktop or a compatible provider must keep a temporary default session active for enablement and rotation.")
	builder.bullet("Only AWS-enabled running workspaces follow the host default.")
	builder.bullet("Switching the host default changes every AWS-enabled running workspace without another approval or workspace restart.")
	builder.bullet("Named host profiles are unavailable.")
}

func nonInternetBridgeCount(bridges []plan.BridgeGrant) int {
	count := 0
	for _, bridge := range bridges {
		if bridge.Kind != "internet" {
			count++
		}
	}
	return count
}
func buildDetectedProjectReview(builder *setupReviewBuilder, preview app.SetupPreview) {
	if preview.Facts.ConfigExists {
		builder.item("Existing DSX config", preview.Facts.ConfigPath)
	} else {
		builder.item("Existing DSX config", "Not found — a new configuration will be written after approval")
	}
	buildDetectedPaths(builder, "Git repositories", preview.Facts.GitRoots)
	buildDetectedPaths(builder, "Dependency files", preview.Facts.Lockfiles)
	buildDetectedPaths(builder, "Container builds", preview.Facts.Dockerfiles)
	buildDetectedPaths(builder, "devenv files", preview.Facts.DevenvFiles)
	if len(preview.Diagnostics) > 0 {
		builder.item("Diagnostics", strconv.Itoa(len(preview.Diagnostics)))
		for _, diagnostic := range preview.Diagnostics {
			location := diagnostic.Path
			if diagnostic.Line > 0 {
				location += ":" + strconv.Itoa(diagnostic.Line)
			}
			message := diagnostic.Severity + " — " + diagnostic.Message
			if location != "" {
				message += " (" + location + ")"
			}
			builder.bullet(message)
		}
	}
}

func buildDetectedPaths(builder *setupReviewBuilder, label string, paths []app.DetectedPath) {
	builder.item(label, strconv.Itoa(len(paths)))
	for _, detected := range paths {
		value := detected.Path
		if detected.Kind != "" {
			value += " (" + detected.Kind + ")"
		}
		builder.bullet(value)
	}
}

func buildAccessReview(builder *setupReviewBuilder, preview app.SetupPreview) {
	execution := preview.Plan
	builder.section("Access & isolation", "Exact project, credential, network, and localhost access granted to the workspace.")
	builder.item("Capabilities", strings.Join(preview.SelectedCapabilities, ", "))
	for _, mount := range execution.Mounts {
		access := "read/write"
		if mount.ReadOnly {
			access = "read-only"
		}
		source := mount.SourceType + " " + mount.Source
		if mount.SourceIdentity != "" {
			source += " [" + mount.SourceIdentity + "]"
		}
		builder.bullet("Mount " + source + " → " + mount.Target + " • " + access)
	}
	for _, harness := range execution.Auth.Imports {
		builder.bullet("Approved authentication import " + harness)
	}
	for _, bridge := range execution.Bridges {
		access := "read/write"
		if bridge.ReadOnly {
			access = "read-only"
		}
		target := bridge.Destination
		if bridge.Port != 0 {
			target += ":" + strconv.Itoa(int(bridge.Port))
		}
		builder.bullet("Host grant " + bridge.Name + " • " + bridge.Kind + " • " + access)
		builder.item("Destination", target)
		if bridge.SourceIdentity != "" {
			builder.item("Source identity", bridge.SourceIdentity)
		}
	}
	for _, port := range execution.Ports {
		hostPort := "dynamic"
		if port.HostPort != nil {
			hostPort = strconv.Itoa(int(*port.HostPort))
		}
		grant := ""
		if port.ExplicitNonLoopbackGrant {
			grant = " • explicit non-loopback grant"
		}
		builder.bullet(fmt.Sprintf("Port %s • %s %d → %s:%s%s", port.Name, port.Protocol, port.GuestPort, port.HostIP, hostPort, grant))
	}
	if len(execution.Mounts) == 0 && len(execution.Auth.Imports) == 0 && len(execution.Bridges) == 0 && len(execution.Ports) == 0 {
		builder.bullet("No additional mounts, credentials, host grants, or published ports")
	}
}

func buildAutomationReview(builder *setupReviewBuilder, preview app.SetupPreview) {
	execution := preview.Plan
	builder.section("Commands & services", "Exact commands DSX may run inside the workspace. Nothing runs during this review.")
	builder.item("Setup commands", strconv.Itoa(len(execution.Setup)))
	for index, command := range execution.Setup {
		builder.bullet(fmt.Sprintf("%d. %s", index+1, reviewCommand(command)))
		buildCommandEnvironment(builder, command.Env)
	}
	builder.item("Project services", strconv.Itoa(len(execution.Processes)))
	for _, process := range execution.Processes {
		flags := []string{}
		if process.Required {
			flags = append(flags, "required")
		}
		if process.Terminal {
			flags = append(flags, "terminal")
		}
		if len(process.DependsOn) > 0 {
			flags = append(flags, "after "+strings.Join(process.DependsOn, ", "))
		}
		detail := reviewCommand(process.Command)
		if len(flags) > 0 {
			detail += " • " + strings.Join(flags, " • ")
		}
		builder.bullet(process.Name + " — " + detail)
		buildCommandEnvironment(builder, process.Command.Env)
		if process.Health != nil {
			target := process.Health.Target
			if process.Health.Command != nil {
				target = reviewCommand(*process.Health.Command)
			}
			builder.bullet(fmt.Sprintf("  Health: %s %s • every %dms • timeout %dms • %d retries", process.Health.Kind, target, process.Health.IntervalMS, process.Health.TimeoutMS, process.Health.Retries))
			if process.Health.Command != nil {
				buildCommandEnvironment(builder, process.Health.Command.Env)
			}
		}
	}
}

func buildCommandEnvironment(builder *setupReviewBuilder, environment []plan.EnvGrant) {
	for _, grant := range environment {
		value := strconv.Quote(grant.Value)
		if grant.Reference != "" {
			value = "reference " + grant.Reference
		}
		if grant.Secret {
			value = "secret " + value
		}
		builder.bullet("  Environment " + grant.Name + " = " + value)
	}
}

func reviewCommand(command plan.ResolvedCommand) string {
	var rendered string
	if len(command.Argv) > 0 {
		quoted := make([]string, len(command.Argv))
		for index, argument := range command.Argv {
			quoted[index] = strconv.Quote(argument)
		}
		rendered = strings.Join(quoted, " ")
	} else if command.Shell != "" {
		rendered = "shell " + strconv.Quote(command.Shell)
		if command.ShellPath != "" {
			rendered += " via " + command.ShellPath
		}
	} else {
		rendered = "No command"
	}
	if command.Cwd != "" {
		rendered += " • cwd " + command.Cwd
	}
	return rendered
}

func buildStorageReview(builder *setupReviewBuilder, preview app.SetupPreview) {
	execution := preview.Plan
	builder.section("Files & persistence", "Repository mappings and guest-owned storage that survive according to the declared scope.")
	builder.item("Repositories", strconv.Itoa(len(execution.Repositories)))
	for _, repository := range execution.Repositories {
		detail := repository.Name + " • " + repository.HostPath + " → " + repository.GuestPath
		if repository.TrackedDigest != "" {
			detail += " • tracked " + repository.TrackedDigest
		}
		builder.bullet(detail)
	}
	builder.item("Volumes", strconv.Itoa(len(execution.Volumes)))
	for _, volume := range execution.Volumes {
		persistence := "temporary"
		if volume.Persistent {
			persistence = "persistent"
		}
		builder.bullet(volume.Name + " → " + volume.Target + " • " + volume.Scope + " • " + persistence)
	}
}

func buildApprovalReview(builder *setupReviewBuilder, preview app.SetupPreview) {
	execution := preview.Plan
	builder.section("Approval", "Identity and hashes bind this review to the exact configuration and executable plan.")
	builder.item("Plan contract", execution.ContractVersion)
	builder.item("Project ID", string(execution.Project.ID))
	keys := make([]string, 0, len(execution.Provenance))
	for key := range execution.Provenance {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		source := execution.Provenance[key]
		location := source.Kind
		if source.Path != "" {
			location += " " + source.Path
		}
		if source.Line > 0 {
			location += ":" + strconv.Itoa(source.Line)
		}
		if source.Column > 0 {
			location += ":" + strconv.Itoa(source.Column)
		}
		location += " • priority " + strconv.Itoa(source.Priority)
		builder.bullet("Source " + key + " ← " + location)
	}
	builder.item("Configuration digest", preview.ConfigContentDigest)
	builder.item("Project state", preview.ProjectState)
	builder.item("Executable hash", preview.Hash)
	if execution.ExecutableHash != "" && execution.ExecutableHash != preview.Hash {
		builder.item("Plan executable hash", execution.ExecutableHash)
	}
}

func reviewInternetSummary(document config.ConfigDocument) string {
	if document.Network.Internet != nil && !*document.Network.Internet {
		return "Keep offline"
	}
	return "Allowed"
}

func reviewBytes(bytes int64) string {
	const (
		kib = int64(1 << 10)
		mib = int64(1 << 20)
		gib = int64(1 << 30)
	)
	switch {
	case bytes >= gib && bytes%gib == 0:
		return fmt.Sprintf("%d GiB", bytes/gib)
	case bytes >= mib && bytes%mib == 0:
		return fmt.Sprintf("%d MiB", bytes/mib)
	case bytes >= kib && bytes%kib == 0:
		return fmt.Sprintf("%d KiB", bytes/kib)
	default:
		return fmt.Sprintf("%d bytes", bytes)
	}
}

func reviewImageSummary(preview app.SetupPreview) string {
	if preview.Plan.Image.Standard {
		return "Ubuntu (DSX Standard)"
	}
	if preview.Plan.Image.Reference != "" {
		return preview.Plan.Image.Reference
	}
	if preview.Plan.Image.File != "" {
		return "project build " + preview.Plan.Image.File
	}
	return "Ubuntu"
}

type setupReviewPage struct {
	title       string
	description string
	lines       []string
	part        int
	parts       int
}

func reviewSectionPages(review string, width, height int) []setupReviewPage {
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	contentWidth := tuiSetupWidth(width)
	bodyWidth := max(1, contentWidth-4)
	if compactTUILayout(contentWidth, height) {
		bodyWidth = contentWidth
	}
	pageBudget := max(1, height-16)
	if compactTUILayout(contentWidth, height) {
		pageBudget = max(1, height-11)
	}
	sections := strings.Split(review, "\n\n")
	pages := make([]setupReviewPage, 0, len(sections))
	for _, section := range sections {
		rawLines := strings.Split(section, "\n")
		if len(rawLines) == 0 || strings.TrimSpace(rawLines[0]) == "" {
			continue
		}
		title := rawLines[0]
		description := ""
		if len(rawLines) > 1 {
			description = rawLines[1]
		}
		details := []string{}
		if len(rawLines) > 2 {
			details = strings.Split(wrapTUIText(strings.Join(rawLines[2:], "\n"), bodyWidth), "\n")
		}
		headerLines := len(strings.Split(wrapTUIText(title, bodyWidth), "\n"))
		if description != "" {
			headerLines += len(strings.Split(wrapTUIText(description, bodyWidth), "\n")) + 1
		}
		detailsPerPage := max(1, pageBudget-headerLines)
		partCount := max(1, (len(details)+detailsPerPage-1)/detailsPerPage)
		if len(details) == 0 {
			pages = append(pages, setupReviewPage{title: title, description: description, part: 1, parts: 1})
			continue
		}
		for start := 0; start < len(details); start += detailsPerPage {
			end := min(start+detailsPerPage, len(details))
			pages = append(pages, setupReviewPage{
				title: title, description: description,
				lines: details[start:end], part: start/detailsPerPage + 1, parts: partCount,
			})
		}
	}
	if len(pages) == 0 {
		return []setupReviewPage{{title: "Review", description: "No effective configuration was produced.", part: 1, parts: 1}}
	}
	return pages
}

func renderReviewSectionPage(page setupReviewPage, theme visualTheme) string {
	title := page.title
	if page.parts > 1 {
		title += fmt.Sprintf("  •  %d/%d", page.part, page.parts)
	}
	lines := []string{theme.section.Render(title)}
	if page.description != "" {
		lines = append(lines, theme.muted.Render(page.description), "")
	}
	for _, line := range page.lines {
		lines = append(lines, renderReviewDetailLine(line, theme))
	}
	return strings.Join(lines, "\n")
}

func renderReviewDetailLine(line string, theme visualTheme) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "• ") {
		value := strings.TrimPrefix(trimmed, "• ")
		style := theme.value
		lower := strings.ToLower(value)
		if strings.HasPrefix(lower, "warning ") || strings.Contains(lower, "explicit non-loopback grant") {
			style = theme.warning
		}
		return "  " + theme.bullet.Render("•") + " " + style.Render(value)
	}
	if label, value, found := strings.Cut(trimmed, ": "); found {
		return "  " + theme.label.Render(label+":") + " " + reviewValueStyle(value, theme).Render(value)
	}
	return "  " + theme.value.Render(trimmed)
}

func reviewValueStyle(value string, theme visualTheme) lipgloss.Style {
	lower := strings.ToLower(value)
	switch {
	case lower == "allowed", strings.HasPrefix(lower, "enabled"):
		return theme.success
	case lower == "disabled", lower == "keep offline", lower == "none", strings.HasPrefix(lower, "not found"):
		return theme.muted
	default:
		return theme.value
	}
}

func reviewPageIndicator(index, total int) string {
	if total < 1 {
		total = 1
	}
	index = max(0, min(index, total-1))
	const segments = 10
	filled := max(1, (index+1)*segments/total)
	return fmt.Sprintf("REVIEW  %d / %d   [%s%s]", index+1, total, strings.Repeat("█", filled), strings.Repeat("░", segments-filled))
}

func reviewNavigationHint(index, total int) string {
	switch {
	case total <= 1:
		return "Complete review fits on this page"
	case index <= 0:
		return "More below • PgDn / ↓ / j"
	case index+1 >= total:
		return "End of review • PgUp / ↑ / k to go back"
	default:
		return "More above and below • PgUp / ↑ / k • PgDn / ↓ / j"
	}
}

func reviewPages(review string, width, height int) []string {
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	bodyWidth := max(1, tuiContentWidth(width)-6)
	lines := strings.Split(terminal.Wrap(review, bodyWidth), "\n")
	linesPerPage := max(1, height-20)
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
