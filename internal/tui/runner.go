package tui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/colorprofile"
	xterm "github.com/charmbracelet/x/term"

	"github.com/srimajji/dsx/internal/app"
	modelpkg "github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/terminal"
)

type RunRequest struct {
	Root       string
	ForceSetup bool
	Accessible bool
	Sandboxes  []app.SandboxSummary
}

// Runner composes application state with Bubble Tea. It returns lifecycle
// intents to its caller; it never reports an intent as completed.
type Runner struct {
	Application Application
	Input       io.Reader
	Output      io.Writer
}

func (runner *Runner) Run(ctx context.Context, request RunRequest) (Intent, bool, error) {
	if runner == nil || runner.Application == nil {
		return Intent{}, false, fmt.Errorf("TUI application service is unavailable")
	}
	state := app.BareState{Screen: app.BareSetup}
	var err error
	if !request.ForceSetup {
		state, err = runner.Application.BareState(ctx, app.BareStateRequest{Root: request.Root})
		if err != nil {
			return Intent{}, false, err
		}
	}
	var model tea.Model
	switch state.Screen {
	case app.BareSetup:
		preview, previewErr := runner.Application.PreviewSetup(ctx, app.SetupPreviewRequest{Root: request.Root})
		if previewErr != nil {
			return Intent{}, false, previewErr
		}
		setup := NewSetupModel(ctx, runner.Application, request.Root, preview, request.Accessible)
		if request.Accessible {
			return runner.runAccessibleSetup(ctx, setup)
		}
		model = setup
	case app.BareLauncher:
		action := NewLauncherModel(state, request.Sandboxes...)
		action.accessible = request.Accessible
		model = action
	case app.BareDashboard:
		action := NewDashboardModel(state, request.Sandboxes...)
		action.accessible = request.Accessible
		model = action
	default:
		return Intent{}, false, fmt.Errorf("unknown bare-command screen %q", state.Screen)
	}
	final, err := runner.runModel(model)
	if err != nil {
		return Intent{}, false, err
	}
	action, ok := final.(*ActionModel)
	if !ok {
		return Intent{}, false, nil
	}
	intent, found := action.Intent()
	if !found {
		return Intent{}, false, nil
	}
	if intent.Action == "start-container-system" {
		if err := runner.Application.StartContainerSystem(ctx); err != nil {
			return Intent{}, false, err
		}
		return runner.Run(ctx, request)
	}
	if intent.Action == "new-clone" {
		return runner.runCloneCreate(ctx, request)
	}
	if state.Screen == app.BareLauncher && requiresConfiguredApproval(intent.Action) {
		return runner.approveConfiguredAction(ctx, request, intent)
	}
	return intent, true, nil
}

func (runner *Runner) runModel(model tea.Model) (tea.Model, error) {
	options := []tea.ProgramOption{tea.WithInput(runner.Input), tea.WithOutput(runner.Output)}
	if !terminal.ColorEnabled() {
		options = append(options, tea.WithColorProfile(colorprofile.NoTTY))
	}
	return tea.NewProgram(model, options...).Run()
}

func (runner *Runner) approveConfiguredAction(ctx context.Context, request RunRequest, intent Intent) (Intent, bool, error) {
	preview, err := runner.Application.PreviewExisting(ctx, app.BareStateRequest{Root: request.Root})
	if err != nil {
		return Intent{}, false, err
	}
	setup := NewExistingApprovalModel(ctx, runner.Application, request.Root, preview, request.Accessible)
	if request.Accessible {
		if _, _, err := runner.runAccessibleSetup(ctx, setup); err != nil {
			return Intent{}, false, err
		}
	} else {
		if _, err := runner.runModel(setup); err != nil {
			return Intent{}, false, err
		}
	}
	if setup.stage != setupDone {
		return Intent{}, false, nil
	}
	return intent, true, nil
}

func (runner *Runner) runCloneCreate(ctx context.Context, request RunRequest) (Intent, bool, error) {
	base, err := runner.Application.PreviewExisting(ctx, app.BareStateRequest{Root: request.Root})
	if err != nil {
		return Intent{}, false, err
	}
	sandbox := ""
	agent := base.Config.Agents.Default
	profile := "default"
	profiles := make([]string, 0, len(base.Config.AuthProfiles))
	for name, configured := range base.Config.AuthProfiles {
		if configured.Harness == agent {
			profiles = append(profiles, name)
		}
	}
	sort.Strings(profiles)
	if len(profiles) != 0 {
		profile = profiles[0]
	}
	prompt := ""
	browserEnabled := base.Config.Browser.Enabled
	input := &singleByteReader{reader: runner.Input}
	fields := []huh.Field{
		huh.NewInput().Title("Named clone sandbox").Description("Lowercase letters, digits, and hyphens.").Value(&sandbox),
		huh.NewInput().Title("Coding agent").Value(&agent),
		huh.NewInput().Title("Authentication profile").Value(&profile),
		huh.NewInput().Title("One-shot prompt").Value(&prompt),
	}
	if base.Config.Browser.Enabled {
		fields = append(fields, huh.NewNote().Title("Isolated browser").Description("Enabled by project configuration."))
	} else {
		fields = append(fields, huh.NewConfirm().Title("Enable isolated browser?").Affirmative("Enable").Negative("Disable").Value(&browserEnabled))
	}
	form := huh.NewForm(huh.NewGroup(fields...)).
		WithInput(input).
		WithOutput(runner.Output).
		WithAccessible(request.Accessible)
	if request.Accessible || !terminal.ColorEnabled() {
		form.WithTheme(huh.ThemeFunc(huh.ThemeBase))
	}
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return Intent{}, false, nil
		}
		return Intent{}, false, err
	}
	parsedSandbox, err := modelpkg.ParseSandboxName(strings.TrimSpace(sandbox))
	if err != nil {
		return Intent{}, false, fmt.Errorf("named clone sandbox is invalid: %w", err)
	}
	if parsedSandbox == "main" {
		return Intent{}, false, fmt.Errorf("named clone sandbox must not be main")
	}
	if strings.TrimSpace(agent) == "" || strings.TrimSpace(profile) == "" || strings.TrimSpace(prompt) == "" {
		return Intent{}, false, fmt.Errorf("agent, authentication profile, and prompt are required")
	}
	preview, err := runner.Application.PreviewClone(ctx, app.ClonePreviewRequest{
		Root: request.Root, Sandbox: string(parsedSandbox), Agent: strings.TrimSpace(agent), Browser: browserEnabled,
	})
	if err != nil {
		return Intent{}, false, err
	}
	review := NewPlanReviewModel(ctx, runner.Application, request.Root, preview, request.Accessible)
	if request.Accessible {
		if _, _, err := runner.runAccessibleSetup(ctx, review); err != nil {
			return Intent{}, false, err
		}
	} else if _, err := runner.runModel(review); err != nil {
		return Intent{}, false, err
	}
	if review.stage != setupDone {
		return Intent{}, false, nil
	}
	return Intent{
		Action: "clone-run", Project: preview.Facts.CanonicalRoot, Sandbox: string(parsedSandbox),
		Agent: strings.TrimSpace(agent), Profile: strings.TrimSpace(profile), Prompt: strings.TrimSpace(prompt),
		Browser: browserEnabled, ApproveConfig: preview.Hash,
	}, true, nil
}

func requiresConfiguredApproval(action string) bool {
	return action == "create" || action == "start" || action == "attach"
}

func (runner *Runner) runAccessibleSetup(ctx context.Context, model *SetupModel) (Intent, bool, error) {
	input := &singleByteReader{reader: runner.Input}
	title := "DSX project setup\n\n"
	if model.reviewOnly {
		title = "DSX execution approval\n\n"
	} else if model.approveOnly {
		title = "DSX configuration approval\n\n"
	}
	if _, err := io.WriteString(runner.Output, title); err != nil {
		return Intent{}, false, fmt.Errorf("write accessible setup prompt: %w", err)
	}
	if !model.approveOnly && !model.reviewOnly {
		model.form.
			WithInput(input).
			WithOutput(runner.Output).
			WithAccessible(true).
			WithTheme(huh.ThemeFunc(huh.ThemeBase))
		if err := model.form.RunWithContext(ctx); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return Intent{}, false, nil
			}
			return Intent{}, false, err
		}
		if model.imageChoice == "custom" {
			model.customForm.
				WithInput(input).
				WithOutput(runner.Output).
				WithAccessible(true).
				WithTheme(huh.ThemeFunc(huh.ThemeBase))
			if err := model.customForm.RunWithContext(ctx); err != nil {
				if errors.Is(err, huh.ErrUserAborted) {
					return Intent{}, false, nil
				}
				return Intent{}, false, err
			}
		}
		model.applyForm()
		preview, err := runner.Application.PreviewSetup(ctx, app.SetupPreviewRequest{Root: model.root, Config: model.document})
		if err != nil {
			return Intent{}, false, err
		}
		model.preview = preview
		model.resetReview(preview)
	}
	model.width = outputWidth(runner.Output)
	if model.reviewRefused != nil {
		if _, writeErr := io.WriteString(runner.Output, "\n"+model.renderReviewPage()); writeErr != nil {
			return Intent{}, false, fmt.Errorf("write accessible setup refusal: %w", writeErr)
		}
		return Intent{}, false, model.reviewRefused
	}
	pages := reviewPages(model.review, model.width, 24)
	reader := bufio.NewReader(input)
	for index, page := range pages {
		position := fmt.Sprintf("Review page %d/%d", index+1, len(pages))
		if index+1 < len(pages) {
			prompt := page + "\n\n" + position + "\nPress Enter to view the next page, or q to cancel: "
			if _, err := io.WriteString(runner.Output, "\n"+terminal.Wrap(prompt, model.width)); err != nil {
				return Intent{}, false, fmt.Errorf("write accessible setup review page: %w", err)
			}
			advance := false
			for !advance {
				answer, readErr := readAccessibleAnswer(reader)
				if readErr != nil {
					return Intent{}, false, readErr
				}
				switch answer {
				case "", "n", "next":
					advance = true
				case "q", "quit", "no":
					return Intent{}, false, nil
				case "y", "yes":
					if _, err := io.WriteString(runner.Output, "\nConfirmation is unavailable until the final review page is visible.\nPress Enter to continue, or q to cancel: "); err != nil {
						return Intent{}, false, fmt.Errorf("write accessible setup navigation warning: %w", err)
					}
				default:
					if _, err := io.WriteString(runner.Output, "\nEnter continues to the next page; q cancels: "); err != nil {
						return Intent{}, false, fmt.Errorf("write accessible setup navigation prompt: %w", err)
					}
				}
			}
			continue
		}
		confirmation := "write configuration and persist this approval"
		if model.preview.Plan.Image.Standard && !model.reviewOnly && !model.approveOnly {
			confirmation = "write configuration, persist this approval, and build DSX Standard"
		}
		prompt := page + "\n\n" + position + "\nFinal confirmation: " + confirmation + "? [y/N]\n"
		if _, err := io.WriteString(runner.Output, "\n"+terminal.Wrap(prompt, model.width)); err != nil {
			return Intent{}, false, fmt.Errorf("write accessible setup final review page: %w", err)
		}
		answer, readErr := readAccessibleAnswer(reader)
		if readErr != nil {
			return Intent{}, false, readErr
		}
		if answer != "y" && answer != "yes" {
			return Intent{}, false, nil
		}
	}
	if model.preview.Plan.Image.Standard && !model.reviewOnly && !model.approveOnly {
		if _, err := io.WriteString(runner.Output, "\nBuilding and verifying DSX Standard...\n"); err != nil {
			return Intent{}, false, fmt.Errorf("write accessible standard image progress: %w", err)
		}
	}
	request := model.approvalRequest()
	var result app.InitializeResult
	var err error
	if model.reviewOnly {
		result = app.InitializeResult{Hash: model.preview.Hash}
	} else if model.approveOnly {
		result, err = runner.Application.ApproveExisting(ctx, request)
	} else {
		result, err = runner.Application.Initialize(ctx, request)
	}
	if err != nil {
		return Intent{}, false, err
	}
	model.result = result
	model.stage = setupDone
	if model.reviewOnly {
		if _, err := fmt.Fprintf(runner.Output, "\nDSX execution approved\n\nApproved hash: %s\n", terminal.SanitizeLine(result.Hash)); err != nil {
			return Intent{}, false, fmt.Errorf("write accessible execution approval: %w", err)
		}
	} else if _, err := fmt.Fprintf(runner.Output, "\nDSX setup complete\n\nConfiguration: %s\nApproved hash: %s\n", terminal.SanitizeLine(result.ConfigPath), terminal.SanitizeLine(result.Hash)); err != nil {
		return Intent{}, false, fmt.Errorf("write accessible setup result: %w", err)
	}
	return Intent{}, false, nil
}

func readAccessibleAnswer(reader *bufio.Reader) (string, error) {
	answer, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read accessible setup review response: %w", err)
	}
	return strings.TrimSpace(strings.ToLower(answer)), nil
}

type singleByteReader struct {
	reader io.Reader
}

func (reader *singleByteReader) Read(buffer []byte) (int, error) {
	if len(buffer) > 1 {
		buffer = buffer[:1]
	}
	if reader.reader == nil {
		return 0, io.EOF
	}
	return reader.reader.Read(buffer)
}

func outputWidth(writer io.Writer) int {
	const fallback = 80
	file, ok := writer.(interface{ Fd() uintptr })
	if !ok {
		return fallback
	}
	width, _, err := xterm.GetSize(file.Fd())
	if err != nil || width <= 0 {
		return fallback
	}
	return width
}
