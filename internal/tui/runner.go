package tui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/colorprofile"
	xterm "github.com/charmbracelet/x/term"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/terminal"
)

type RunRequest struct {
	Root          string
	ForceSetup    bool
	Accessible    bool
	Dashboard     DashboardData
	LoadDashboard func(context.Context, string) (DashboardData, error)
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
			if _, _, err := runner.runAccessibleSetup(ctx, setup); err != nil {
				return Intent{}, false, err
			}
			if setup.stage == setupDone {
				return runner.continueAfterSetup(ctx, request)
			}
			return Intent{}, false, nil
		}
		model = setup
	case app.BareDashboard:
		if request.LoadDashboard != nil {
			request.Dashboard, err = request.LoadDashboard(ctx, request.Root)
			if err != nil {
				return Intent{}, false, err
			}
		}
		if request.Dashboard.Root == "" {
			request.Dashboard.Root = request.Root
		}
		action := NewDashboardModel(request.Dashboard)
		action.accessible = request.Accessible
		model = action
	default:
		return Intent{}, false, fmt.Errorf("unknown bare-command screen %q", state.Screen)
	}
	var final tea.Model
	if action, ok := model.(*DashboardModel); ok && request.Accessible {
		final, err = runner.runAccessibleAction(action)
	} else {
		final, err = runner.runModel(model)
	}
	if err != nil {
		return Intent{}, false, err
	}
	if setup, ok := final.(*SetupModel); ok {
		if setup.stage == setupDone {
			return runner.continueAfterSetup(ctx, request)
		}
		return Intent{}, false, nil
	}
	action, ok := final.(*DashboardModel)
	if !ok {
		return Intent{}, false, nil
	}
	intent, found := action.Intent()
	if !found {
		return Intent{}, false, nil
	}
	return intent, true, nil
}

func (runner *Runner) continueAfterSetup(ctx context.Context, request RunRequest) (Intent, bool, error) {
	state, err := runner.Application.BareState(ctx, app.BareStateRequest{Root: request.Root})
	if err != nil {
		return Intent{}, false, err
	}
	if state.Screen == app.BareSetup {
		return Intent{}, false, fmt.Errorf("setup completed but the project is still unconfigured")
	}
	request.ForceSetup = false
	return runner.Run(ctx, request)
}

func (runner *Runner) runAccessibleAction(model *DashboardModel) (tea.Model, error) {
	reader := bufio.NewReader(&singleByteReader{reader: runner.Input})
	for {
		if _, err := io.WriteString(runner.Output, "\n"+model.View().Content+"\n"); err != nil {
			return model, fmt.Errorf("write accessible project screen: %w", err)
		}
		key, err := reader.ReadByte()
		if errors.Is(err, io.EOF) {
			return model, nil
		}
		if err != nil {
			return model, fmt.Errorf("read accessible project action: %w", err)
		}
		message := tea.KeyPressMsg(tea.Key{Text: string(key), Code: rune(key)})
		if key == '\n' || key == '\r' {
			message = tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter})
		} else if key == '\t' {
			message = tea.KeyPressMsg(tea.Key{Code: tea.KeyTab})
		} else if key == 27 {
			message = tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape})
		} else if key == 127 || key == 8 {
			message = tea.KeyPressMsg(tea.Key{Code: tea.KeyBackspace})
		}
		updated, _ := model.Update(message)
		action, ok := updated.(*DashboardModel)
		if !ok {
			return model, fmt.Errorf("accessible project action returned an unexpected model")
		}
		model = action
		if _, found := model.Intent(); found {
			return model, nil
		}
		if key == 3 || key == 'q' && model.screen == dashboardHome {
			return model, nil
		}
	}
}

func (runner *Runner) runModel(model tea.Model) (tea.Model, error) {
	options := []tea.ProgramOption{tea.WithInput(runner.Input), tea.WithOutput(runner.Output)}
	if !terminal.ColorEnabled() {
		options = append(options, tea.WithColorProfile(colorprofile.NoTTY))
	}
	return tea.NewProgram(model, options...).Run()
}

func (runner *Runner) runAccessibleSetup(ctx context.Context, model *SetupModel) (Intent, bool, error) {
	input := &singleByteReader{reader: runner.Input}
	title := "DSX project setup\n\n"
	if model.reviewOnly {
		title = "DSX execution approval\n\n"
	} else if model.approveOnly {
		title = "DSX configuration approval\n\n"
	} else if model.updateOnly {
		title = "DSX published-port update\n\n"
	}
	if _, err := io.WriteString(runner.Output, title); err != nil {
		return Intent{}, false, fmt.Errorf("write accessible setup prompt: %w", err)
	}
	if !model.approveOnly && !model.updateOnly && !model.reviewOnly {
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
		if model.updateOnly {
			confirmation = "replace the published-port configuration and persist this approval"
		}
		if model.preview.Plan.Image.Standard && !model.reviewOnly && !model.approveOnly && !model.updateOnly {
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
	if !model.reviewOnly && !model.approveOnly && !model.updateOnly {
		progress := "\nSetting up this project:\n- Verify Apple container system\n- Save configuration and approval\n"
		if model.preview.Plan.Image.Standard {
			progress += "- Build and verify DSX Standard\n"
		}
		progress += "- Open project workspace screen\n"
		if _, err := io.WriteString(runner.Output, progress); err != nil {
			return Intent{}, false, fmt.Errorf("write accessible setup progress: %w", err)
		}
	}
	request := model.approvalRequest()
	var result app.InitializeResult
	var err error
	if model.reviewOnly {
		result = app.InitializeResult{Hash: model.preview.Hash}
	} else if model.approveOnly {
		result, err = runner.Application.ApproveExisting(ctx, request)
	} else if model.updateOnly {
		result, err = runner.Application.UpdateExisting(ctx, request)
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
