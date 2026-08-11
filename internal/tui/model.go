package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/config"
	"github.com/srimajji/dsx/internal/harness"
	modelpkg "github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/terminal"
)

const maxSetupReviewBytes = 512 * 1024

var errSetupReviewTooLarge = errors.New("complete setup review exceeds the safe display bound")

// Application is the setup/bare-state boundary. Models never write files or
// perform lifecycle transitions themselves.
type Application interface {
	BareState(context.Context, app.BareStateRequest) (app.BareState, error)
	StartContainerSystem(context.Context) error
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
	customForm    *huh.Form
	stage         setupStage
	preview       app.SetupPreview
	document      config.ConfigDocument
	image         string
	imageChoice   string
	imageOptions  []app.SetupImageOption
	agent         string
	initialImage  string
	initialAgent  string
	internet      bool
	cpus          int
	memory        string
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
		imageChoice:  initial.SelectedImageOption,
		imageOptions: append([]app.SetupImageOption(nil), initial.ImageOptions...),
		agent:        terminal.SanitizeLine(initial.Config.Agents.Default),
		initialImage: initial.Config.Image.Ref,
		initialAgent: initial.Config.Agents.Default,
		internet:     true,
		cpus:         initial.Config.Resources.CPUs,
		memory:       initial.Config.Resources.Memory,
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
	model.prepareImageOptions()
	model.buildSetupForms()
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

func (model *SetupModel) buildSetupForms() {
	imageOptions := make([]huh.Option[string], 0, len(model.imageOptions))
	for _, option := range model.imageOptions {
		imageOptions = append(imageOptions, huh.NewOption(option.Name, option.ID))
	}
	imageSelect := huh.NewSelect[string]().
		Options(imageOptions...).
		Value(&model.imageChoice).
		Validate(model.validateImageChoice)
	imageInput := huh.NewInput().
		Title("Container image reference (advanced)").
		Description("Use a complete OCI reference ending in @sha256:<64-hex-digit-digest>.").
		Value(&model.image).
		Validate(validateCustomImage)
	model.form = huh.NewForm(
		huh.NewGroup(imageSelect),
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Coding assistant").
				Description("Choose the assistant DSX will open inside your workspace.").
				Options(
					huh.NewOption("Codex", string(harness.Codex)),
					huh.NewOption("Claude Code", string(harness.Claude)),
					huh.NewOption("OMP", string(harness.OMP)),
					huh.NewOption("OpenCode", string(harness.OpenCode)),
				).
				Value(&model.agent),
			huh.NewConfirm().
				Title("Let this workspace access the internet?").
				Description("Needed for package downloads, documentation, and most coding assistants.").
				Affirmative("Allow").
				Negative("Keep offline").
				Value(&model.internet).
				WithButtonAlignment(lipgloss.Left),
		),
		huh.NewGroup(
			huh.NewSelect[int]().
				Title("CPU allocation").
				Description("Compute available to each workspace sandbox.").
				Options(setupCPUOptions(model.cpus)...).
				Value(&model.cpus),
			huh.NewSelect[string]().
				Title("Memory allocation").
				Description("RAM available to each workspace sandbox.").
				Options(setupMemoryOptions(model.memory)...).
				Value(&model.memory),
		),
	).WithAccessible(model.accessible)
	model.customForm = huh.NewForm(huh.NewGroup(imageInput)).WithAccessible(model.accessible)
	if model.accessible || !model.color {
		theme := huh.ThemeFunc(huh.ThemeBase)
		model.form.WithTheme(theme)
		model.customForm.WithTheme(theme)
	}
}

func validateCustomImage(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("Enter a digest-pinned image reference")
	}
	return nil
}

func (model *SetupModel) prepareImageOptions() {
	available := model.imageOptions[:0]
	selectedAvailable := model.imageChoice == ""
	for _, option := range model.imageOptions {
		if !option.Available {
			continue
		}
		available = append(available, option)
		selectedAvailable = selectedAvailable || option.ID == model.imageChoice
	}
	model.imageOptions = available
	if !selectedAvailable {
		model.imageChoice = ""
	}

	if len(model.imageOptions) == 0 {
		option := app.SetupImageOption{ID: "custom", Name: "Custom OCI image", Available: true}
		if model.document.Image.Build != nil || model.document.Image.Ref != "" {
			option = app.SetupImageOption{
				ID: "configured", Name: "Configured image", Description: "Use the current configuration",
				Available: true, Image: model.document.Image,
			}
		}
		model.imageOptions = append(model.imageOptions, option)
	}
	if model.imageChoice == "" {
		if model.document.Image.Build != nil || model.document.Image.Ref != "" {
			for _, option := range model.imageOptions {
				if option.Available && option.ID != "custom" {
					model.imageChoice = option.ID
					break
				}
			}
		}
		if model.imageChoice == "" {
			model.imageChoice = "custom"
		}
	}
	hasCustom := false
	for _, option := range model.imageOptions {
		hasCustom = hasCustom || option.ID == "custom"
	}
	if !hasCustom {
		model.imageOptions = append(model.imageOptions, app.SetupImageOption{
			ID: "custom", Name: "Custom OCI image", Description: "Advanced digest-pinned reference", Available: true,
		})
	}
}

func (model *SetupModel) validateImageChoice(choice string) error {
	for _, option := range model.imageOptions {
		if option.ID != choice {
			continue
		}
		if !option.Available {
			return errors.New("This image option is unavailable in the current DSX build")
		}
		return nil
	}
	return errors.New("Choose a workspace image")
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
			case "b":
				if model.approveOnly || model.reviewOnly {
					return model, nil
				}
				model.stage = setupForm
				model.reviewPage = 0
				model.buildSetupForms()
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
			if model.imageChoice == "custom" && model.form != model.customForm {
				model.form = model.customForm
				return model, tea.Batch(command, model.form.Init())
			}
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
	header := theme.header("Workspace setup", friendlyProjectName(model.root), model.width)
	step := 0
	var content string
	if model.err != nil {
		content = header + "\n\n" + theme.panel(
			"Something needs attention",
			theme.danger.Render("Could not continue")+"\n\n"+terminal.Sanitize(model.err.Error())+"\n\nPress Ctrl-C to exit.",
			model.width,
			true,
		)
	} else {
		switch model.stage {
		case setupForm:
			formView := model.form.View()
			if !model.color {
				formView = ansi.Strip(formView)
			}
			content = header + "\n\n" + theme.stepper(step, model.width) + "\n\n" +
				theme.panel("Choose how your workspace should start", formView, model.width, true)
		case setupSaving:
			step = 1
			title := "Checking your choices"
			body := "DSX is preparing a complete, reviewable plan. Nothing has been created yet."
			if model.confirming && model.preview.Plan.Image.Standard {
				title = "Building DSX Standard"
				body = "DSX is building and verifying the approved Ubuntu agent image. No workspace has been created."
			}
			content = header + "\n\n" + theme.stepper(step, model.width) + "\n\n" +
				theme.panel(title, body, model.width, true)
		case setupDone:
			step = 2
			body := theme.success.Render("✓ Workspace configuration saved") +
				"\n\nConfiguration\n" + terminal.SanitizeLine(model.result.ConfigPath) +
				"\n\nApproval\n" + terminal.SanitizeLine(model.result.Hash)
			title := "Ready to build"
			if model.preview.Plan.Image.Standard {
				title = "Ready to use"
			}
			content = header + "\n\n" + theme.stepper(step, model.width) + "\n\n" +
				theme.panel(title, body, model.width, true)
		default:
			content = model.renderReviewPage()
		}
	}
	rendered := terminal.Wrap(content, tuiContentWidth(model.width))
	if !model.accessible {
		rendered = theme.layout(rendered, model.width)
	}
	view := tea.NewView(rendered)
	view.AltScreen = !model.accessible
	return view
}

func (model *SetupModel) applyForm() {
	if model.document.SchemaVersion == 0 {
		model.document = model.preview.Config
	}
	if model.imageChoice == "custom" {
		image := strings.TrimSpace(model.image)
		if image == terminal.SanitizeLine(model.initialImage) {
			image = model.initialImage
		}
		model.document.Image = config.ImageConfig{Ref: image}
	} else {
		for _, option := range model.imageOptions {
			if option.ID == model.imageChoice {
				model.document.Image = option.Image
				break
			}
		}
	}
	agent := strings.TrimSpace(model.agent)
	if agent == terminal.SanitizeLine(model.initialAgent) {
		agent = model.initialAgent
	}
	model.document.Agents.Default = agent
	if model.document.Agents.Default != "" && !slices.Contains(model.document.Agents.Allowed, model.document.Agents.Default) {
		model.document.Agents.Allowed = append(model.document.Agents.Allowed, model.document.Agents.Default)
	}
	internet := model.internet
	model.document.Network.Internet = &internet
	model.document.Resources.CPUs = model.cpus
	model.document.Resources.Memory = model.memory
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
	return len(reviewSectionPages(model.review, model.width, model.height))
}

func (model *SetupModel) renderReviewPage() string {
	theme := newVisualTheme(model.color && !model.accessible)
	header := theme.header("Workspace setup", friendlyProjectName(model.root), model.width)
	if model.reviewRefused != nil {
		body := fmt.Sprintf("The complete review exceeds the %d-byte safe display limit. Nothing was truncated. Approval is disabled.\n\nReduce the configuration and retry.", maxSetupReviewBytes)
		footer := theme.center(theme.help("[q] quit"), model.width)
		return header + "\n\n" + theme.stepper(1, model.width) + "\n\n" +
			theme.panel("Review unavailable", body, model.width, true) + "\n\n" + footer
	}
	pages := reviewSectionPages(model.review, model.width, model.height)
	if model.reviewPage >= len(pages) {
		model.reviewPage = len(pages) - 1
	}
	position := theme.accent.Render(reviewPageIndicator(model.reviewPage, len(pages)))
	controls := theme.help("[PgDn/↓] next section", "[PgUp/↑] previous", "[q] quit")
	if !model.approveOnly && !model.reviewOnly {
		controls = theme.help("[b] back to environment", "[PgDn/↓] next section", "[PgUp/↑] previous", "[q] quit")
	}
	status := theme.warning.Render("Approval locked • review every section to continue")
	if model.reviewPage+1 == len(pages) {
		controls = theme.help("[y] approve", "[PgUp/↑] previous", "[q] quit")
		if !model.approveOnly && !model.reviewOnly {
			controls = theme.help("[y] approve", "[b] back to environment", "[PgUp/↑] previous", "[q] quit")
		}
		confirmation := "write configuration and persist this approval"
		if model.preview.Plan.Image.Standard && !model.reviewOnly && !model.approveOnly {
			confirmation = "write configuration, persist this approval, and build DSX Standard"
		}
		status = theme.success.Render("Final confirmation: " + confirmation + "? [y/N]")
	}
	page := position + "\n" + theme.muted.Render(reviewNavigationHint(model.reviewPage, len(pages))) + "\n\n" +
		renderReviewSectionPage(pages[model.reviewPage], theme)
	footer := theme.center(status+"\n"+controls, model.width)
	return header + "\n\n" + theme.stepper(1, model.width) + "\n\n" +
		theme.panel("Review what DSX will do", page, model.width, true) +
		"\n\n" + footer
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
	buildWorkspaceReview(builder, preview)
	buildDetectedProjectReview(builder, preview)
	buildAccessReview(builder, preview)
	buildAutomationReview(builder, preview)
	buildStorageReview(builder, preview)
	buildApprovalReview(builder, preview)
	if !builder.output.Complete() {
		return "", errSetupReviewTooLarge
	}
	return builder.output.String(), nil
}

func buildWorkspaceReview(builder *setupReviewBuilder, preview app.SetupPreview) {
	execution := preview.Plan
	builder.section("Workspace", "Where DSX will run and which environment it will start.")
	builder.item("Project", preview.Facts.CanonicalRoot)
	builder.item("Mode", string(execution.Mode))
	builder.item("Sandbox", string(execution.Sandbox.Name))
	builder.item("Environment", reviewImageSummary(preview))
	if execution.Image.Context != "" {
		builder.bullet("Build context " + execution.Image.Context)
	}
	if execution.Image.File != "" {
		builder.bullet("Build file " + execution.Image.File)
	}
	if execution.Image.Target != "" {
		builder.bullet("Build target " + execution.Image.Target)
	}
	for _, argument := range execution.Image.BuildArgs {
		builder.bullet("Build argument " + argument.Key + "=" + argument.Value)
	}
	if execution.Image.InputDigest != "" && !strings.Contains(reviewImageSummary(preview), execution.Image.InputDigest) {
		builder.bullet("Image input digest " + execution.Image.InputDigest)
	}
	builder.item("Coding assistant", execution.Agent)
	builder.item("Internet", reviewInternetSummary(preview.Config))
	builder.item("Browser", reviewBrowserSummary(execution))
	builder.item("Resources", fmt.Sprintf("%d CPU • %s • %d concurrent clone(s)", execution.Limits.CPUs, reviewBytes(execution.Limits.MemoryBytes), execution.Limits.MaxConcurrentClones))
}

func buildDetectedProjectReview(builder *setupReviewBuilder, preview app.SetupPreview) {
	builder.section("Detected project", "Used to suggest defaults only. Detection never runs project code.")
	if preview.Facts.ConfigExists {
		builder.item("Existing DSX config", preview.Facts.ConfigPath)
	} else {
		builder.item("Existing DSX config", "Not found — a new configuration will be written after approval")
	}
	buildDetectedPaths(builder, "Git repositories", preview.Facts.GitRoots)
	buildDetectedPaths(builder, "Dependency files", preview.Facts.Lockfiles)
	buildDetectedPaths(builder, "Container builds", preview.Facts.Dockerfiles)
	buildDetectedPaths(builder, "Dev Containers", preview.Facts.Devcontainers)
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
	for _, grant := range execution.Auth {
		builder.bullet("Credentials " + grant.Harness + "/" + grant.Profile + " • " + grant.Persistence)
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
	if len(execution.Mounts) == 0 && len(execution.Auth) == 0 && len(execution.Bridges) == 0 && len(execution.Ports) == 0 {
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
		if repository.SourceRef != "" {
			detail += " • ref " + repository.SourceRef
		}
		if repository.SourceCommit != "" {
			detail += " • commit " + repository.SourceCommit
		}
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
	builder.item("Run ID", string(execution.Sandbox.RunID))
	builder.item("Owned resource", execution.Ownership.ResourceName)
	for _, label := range execution.Ownership.Labels {
		builder.bullet("Ownership label " + label.Key + " = " + label.Value)
	}
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

func reviewBrowserSummary(execution plan.ExecutionPlan) string {
	if execution.Browser == nil || !execution.Browser.Enabled {
		return "Disabled"
	}
	parts := []string{"Enabled • isolated browser"}
	if execution.Browser.ImageReference != "" {
		parts = append(parts, execution.Browser.ImageReference)
	}
	if execution.Browser.ImageDigest != "" {
		parts = append(parts, execution.Browser.ImageDigest)
	}
	return strings.Join(parts, " • ")
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
		return "DSX Standard — Ubuntu (local build, input sha256:" + preview.Plan.Image.InputDigest + ")"
	}
	if preview.Plan.Image.Reference != "" {
		return preview.Plan.Image.Reference
	}
	if preview.Plan.Image.File != "" {
		return "project build " + preview.Plan.Image.File
	}
	return "not resolved"
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
	bodyWidth := max(1, tuiContentWidth(width)-6)
	pageBudget := max(1, height-20)
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
			details = strings.Split(terminal.Wrap(strings.Join(rawLines[2:], "\n"), bodyWidth), "\n")
		}
		headerLines := len(strings.Split(terminal.Wrap(title, bodyWidth), "\n"))
		if description != "" {
			headerLines += len(strings.Split(terminal.Wrap(description, bodyWidth), "\n")) + 1
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

type ActionModel struct {
	state        app.BareState
	intent       *Intent
	width        int
	accessible   bool
	manage       bool
	confirmClean bool
	sandboxes    []app.SandboxSummary
	selected     int
	notice       string
}

func NewLauncherModel(state app.BareState, sandboxes ...app.SandboxSummary) *ActionModel {
	return newActionModel(state, sandboxes)
}

func NewDashboardModel(state app.BareState, sandboxes ...app.SandboxSummary) *ActionModel {
	return newActionModel(state, sandboxes)
}

func newActionModel(state app.BareState, sandboxes []app.SandboxSummary) *ActionModel {
	available := make([]app.SandboxSummary, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		if _, err := modelpkg.ParseSandboxName(string(sandbox.Sandbox)); err == nil && sandbox.State != modelpkg.StateDeleted {
			available = append(available, sandbox)
		}
	}
	slices.SortStableFunc(available, func(left, right app.SandboxSummary) int {
		if left.Sandbox == "main" && right.Sandbox != "main" {
			return -1
		}
		if left.Sandbox != "main" && right.Sandbox == "main" {
			return 1
		}
		return strings.Compare(string(left.Sandbox), string(right.Sandbox))
	})
	return &ActionModel{state: state, width: 80, sandboxes: available}
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
	pressed := strings.ToLower(key.String())
	if model.confirmClean {
		switch pressed {
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
	if !model.manage {
		switch pressed {
		case "ctrl+c", "q", "esc":
			return model, tea.Quit
		case "m":
			model.manage = true
			model.notice = ""
			return model, nil
		case "enter":
			action, _ := model.primaryAction()
			if action == "" {
				return model, nil
			}
			model.intent = &Intent{Action: action, Project: model.state.Facts.CanonicalRoot}
			return model, tea.Quit
		default:
			return model, nil
		}
	}

	switch pressed {
	case "ctrl+c", "q":
		return model, tea.Quit
	case "esc", "m":
		model.manage = false
		model.notice = ""
		return model, nil
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
	case "n":
		if model.runtimeReady() && model.state.ConfigExists {
			model.intent = &Intent{Action: "new-clone", Project: model.state.Facts.CanonicalRoot}
			return model, tea.Quit
		}
	case "x":
		if selected := model.selectedSandbox(); selected != nil && selected.State == modelpkg.StateRunning {
			model.intent = &Intent{
				Action: "stop", Project: model.state.Facts.CanonicalRoot, Sandbox: string(selected.Sandbox),
			}
			return model, tea.Quit
		}
	case "d":
		if model.workspaceExists() {
			model.confirmClean = true
			return model, nil
		}
	case "g":
		return model.selectGitAction("status")
	case "v":
		return model.selectGitAction("diff")
	case "f":
		return model.selectGitAction("fetch")
	}
	return model, nil
}

func (model *ActionModel) runtimeReady() bool {
	return model.state.ContainerSystem.State == runtime.SystemStateRunning
}

func (model *ActionModel) workspaceExists() bool {
	return model.state.OwnedResources > 0 || len(model.sandboxes) > 0
}

func (model *ActionModel) mainSandbox() *app.SandboxSummary {
	for index := range model.sandboxes {
		if model.sandboxes[index].Sandbox == "main" {
			return &model.sandboxes[index]
		}
	}
	return nil
}

func (model *ActionModel) selectedSandbox() *app.SandboxSummary {
	if model.selected < 0 || model.selected >= len(model.sandboxes) {
		return nil
	}
	return &model.sandboxes[model.selected]
}

func (model *ActionModel) primaryAction() (string, string) {
	switch model.state.ContainerSystem.State {
	case runtime.SystemStateStopped:
		return "start-container-system", "Start container system"
	case runtime.SystemStateNotInstalled, runtime.SystemStateUnavailable, runtime.SystemStateUnknown:
		return "", ""
	case runtime.SystemStateRunning:
	}
	main := model.mainSandbox()
	if main == nil {
		if model.state.OwnedResources > 0 && len(model.sandboxes) == 0 {
			return "", ""
		}
		return "create", "Create & open"
	}
	switch main.State {
	case modelpkg.StateRunning:
		return "attach", "Attach"
	case modelpkg.StateStopped:
		return "start", "Start & open"
	default:
		return "", ""
	}
}

func (model *ActionModel) selectGitAction(operation string) (tea.Model, tea.Cmd) {
	selected := model.selectedSandbox()
	if !model.manage || selected == nil || selected.Mode != modelpkg.ModeClone {
		model.notice = fmt.Sprintf("Git %s is available only for a selected isolated clone.", operation)
		return model, nil
	}
	model.intent = &Intent{
		Action:  "git-" + operation,
		Project: model.state.Facts.CanonicalRoot,
		Sandbox: string(selected.Sandbox),
	}
	return model, tea.Quit
}

func (model *ActionModel) View() tea.View {
	theme := newVisualTheme(terminal.ColorEnabled() && !model.accessible)
	header := theme.header("Project", friendlyProjectName(model.state.Facts.CanonicalRoot), model.width)
	if model.confirmClean {
		body := theme.danger.Render("This removes DSX-owned workspace resources.") +
			"\n\nYour project files and global agent sign-ins are preserved." +
			"\n\nProject\n" + terminal.SanitizeLine(model.state.Facts.CanonicalRoot) +
			"\n\n" + theme.help("[y] confirm cleanup", "[n] cancel")
		return model.actionView(header + "\n\n" + theme.panel("Confirm cleanup", body, model.width, true))
	}

	projectWidth := max(8, tuiContentWidth(model.width)-10)
	overview := theme.title.Render(terminal.Truncate(friendlyProjectName(model.state.Facts.CanonicalRoot), projectWidth)) +
		"\n" + theme.muted.Render(terminal.Truncate(terminal.SanitizeLine(model.state.Facts.CanonicalRoot), projectWidth)) +
		"\n\n" + model.renderStatus(theme)
	content := header + "\n\n" + theme.panel("Status", overview, model.width, false)

	if model.manage {
		content += "\n\n" + theme.panel("Workspaces", model.renderSandboxList(theme), model.width, len(model.sandboxes) != 0)
		content += "\n\n" + theme.panel("More options", model.renderManageActions(theme), model.width, false)
	} else {
		content += "\n\n" + theme.panel("Next", model.nextStep(theme), model.width, false)
	}
	if model.notice != "" {
		content += "\n\n" + theme.panel("Heads up", terminal.SanitizeLine(model.notice), model.width, true)
	}
	return model.actionView(content)
}

func (model *ActionModel) actionView(content string) tea.View {
	theme := newVisualTheme(terminal.ColorEnabled() && !model.accessible)
	rendered := terminal.Wrap(content, tuiContentWidth(model.width))
	if !model.accessible {
		rendered = theme.layout(rendered, model.width)
	}
	view := tea.NewView(rendered)
	view.AltScreen = !model.accessible
	return view
}

func (model *ActionModel) renderStatus(theme visualTheme) string {
	setup := "Not configured"
	if model.state.ConfigExists {
		setup = "Complete"
	}
	return strings.Join([]string{
		statusRow(theme, "Container system", containerSystemLabel(model.state.ContainerSystem.State)),
		statusRow(theme, "Project setup", setup),
		statusRow(theme, "Workspace", model.workspaceLabel()),
	}, "\n")
}

func statusRow(theme visualTheme, label, value string) string {
	style := theme.value
	lower := strings.ToLower(value)
	switch {
	case strings.Contains(lower, "running"), lower == "complete":
		style = theme.success
	case strings.Contains(lower, "not created"), strings.Contains(lower, "stopped"):
		style = theme.warning
	case strings.Contains(lower, "not installed"), strings.Contains(lower, "unavailable"):
		style = theme.danger
	}
	return theme.label.Render(label) + "    " + style.Render(value)
}

func containerSystemLabel(state runtime.SystemState) string {
	switch state {
	case runtime.SystemStateRunning:
		return "Running"
	case runtime.SystemStateStopped:
		return "Stopped"
	case runtime.SystemStateNotInstalled:
		return "Not installed"
	default:
		return "Unavailable"
	}
}

func (model *ActionModel) workspaceLabel() string {
	if main := model.mainSandbox(); main != nil {
		state := string(main.State)
		if state == "" {
			state = "unknown"
		} else {
			state = strings.ToUpper(state[:1]) + state[1:]
		}
		return "main — " + state
	}
	named := 0
	for _, sandbox := range model.sandboxes {
		if sandbox.Mode == modelpkg.ModeClone {
			named++
		}
	}
	if named > 0 {
		return fmt.Sprintf("Not created • %d isolated", named)
	}
	if model.state.OwnedResources > 0 {
		return "State unavailable"
	}
	return "Not created"
}

func (model *ActionModel) nextStep(theme visualTheme) string {
	var message string
	switch model.state.ContainerSystem.State {
	case runtime.SystemStateStopped:
		message = "DSX needs Apple Container running before it can use a workspace."
	case runtime.SystemStateNotInstalled:
		message = "DSX requires Apple Container 1.2.2.\n\n" + model.state.ContainerSystem.Remediation
	case runtime.SystemStateUnavailable, runtime.SystemStateUnknown:
		message = "DSX could not determine the Apple Container status.\n\n" + model.state.ContainerSystem.Remediation
	case runtime.SystemStateRunning:
		switch main := model.mainSandbox(); {
		case main == nil && model.state.OwnedResources == 0:
			message = "No workspace exists for this project yet."
		case main == nil:
			message = "DSX found project resources but could not verify a live workspace."
		case main.State == modelpkg.StateRunning:
			message = "Your project workspace is ready."
		case main.State == modelpkg.StateStopped:
			message = "Your project workspace is stopped."
		default:
			message = "Your project workspace needs attention before it can be used."
		}
	}
	_, label := model.primaryAction()
	actions := []string{}
	if label != "" {
		actions = append(actions, theme.help("[Enter] "+label))
	}
	if model.runtimeReady() {
		actions = append(actions, theme.help("[m] More options"))
	}
	actions = append(actions, theme.help("[q] Quit"))
	return message + "\n\n" + strings.Join(actions, "\n")
}

func (model *ActionModel) renderManageActions(theme visualTheme) string {
	actions := make([]string, 0, 6)
	if model.runtimeReady() && model.state.ConfigExists {
		actions = append(actions, theme.help("[n] Create isolated clone"))
	}
	selected := model.selectedSandbox()
	if selected != nil && selected.State == modelpkg.StateRunning {
		actions = append(actions, theme.help("[x] Stop selected workspace"))
	}
	if selected != nil && selected.Mode == modelpkg.ModeClone {
		actions = append(actions, theme.help("[g] Git status", "[v] Git diff", "[f] Git fetch"))
	}
	if model.workspaceExists() {
		actions = append(actions, theme.help("[d] Clean DSX resources"))
	}
	actions = append(actions, theme.help("[m/Esc] Back", "[q] Quit"))
	return strings.Join(actions, "\n")
}

func (model *ActionModel) renderSandboxList(theme visualTheme) string {
	if len(model.sandboxes) == 0 {
		return theme.muted.Render("No workspace exists for this project.")
	}
	var entries strings.Builder
	entries.WriteString(theme.muted.Render("Use j/k or ↑/↓ to select a workspace.") + "\n\n")
	for index, sandbox := range model.sandboxes {
		marker := "  "
		nameStyle := theme.title
		if index == model.selected {
			marker = "› "
			nameStyle = theme.accent
		}
		state := terminal.SanitizeLine(string(sandbox.State))
		tone := "warning"
		if state == "running" {
			tone = "success"
		} else if state == "failed" {
			tone = "danger"
		}
		fmt.Fprintf(
			&entries,
			"%s%s  %s\n",
			marker,
			nameStyle.Render(terminal.SanitizeLine(string(sandbox.Sandbox))),
			theme.badge(state, tone),
		)
	}
	return strings.TrimRight(entries.String(), "\n")
}

func (model *ActionModel) Intent() (Intent, bool) {
	if model.intent == nil {
		return Intent{}, false
	}
	return *model.intent, true
}
