package tui

import (
	"context"
	"fmt"
	"io"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/srimajji/dsx/internal/terminal"
)

// ProgressStep is one bounded, user-facing lifecycle milestone.
type ProgressStep struct {
	ID    string
	Label string
}

// ProgressRequest describes a lifecycle operation without exposing raw logs.
type ProgressRequest struct {
	Title      string
	Project    string
	Detail     string
	Steps      []ProgressStep
	Accessible bool
}

// ProgressOperation performs lifecycle work and reports ordered step IDs.
type ProgressOperation func(context.Context, func(string)) error

type progressUpdateMessage struct{ id string }
type progressResultMessage struct{ err error }

type progressModel struct {
	ctx       context.Context
	cancel    context.CancelFunc
	request   ProgressRequest
	operation ProgressOperation
	updates   chan string
	spinner   spinner.Model
	current   int
	width     int
	color     bool
	err       error
	cancelled bool
}

func newProgressModel(ctx context.Context, cancel context.CancelFunc, request ProgressRequest, operation ProgressOperation) *progressModel {
	return &progressModel{
		ctx: ctx, cancel: cancel, request: request, operation: operation,
		updates: make(chan string), spinner: spinner.New(spinner.WithSpinner(spinner.Dot)),
		width: 80, color: terminal.ColorEnabled(),
	}
}

func (model *progressModel) Init() tea.Cmd {
	return tea.Batch(model.spinner.Tick, model.waitForUpdate(), model.runOperation())
}

func (model *progressModel) waitForUpdate() tea.Cmd {
	return func() tea.Msg {
		select {
		case id := <-model.updates:
			return progressUpdateMessage{id: id}
		case <-model.ctx.Done():
			return progressResultMessage{err: model.ctx.Err()}
		}
	}
}

func (model *progressModel) runOperation() tea.Cmd {
	return func() tea.Msg {
		err := model.operation(model.ctx, func(id string) {
			select {
			case model.updates <- id:
			case <-model.ctx.Done():
			}
		})
		return progressResultMessage{err: err}
	}
}

func (model *progressModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case spinner.TickMsg:
		var command tea.Cmd
		model.spinner, command = model.spinner.Update(message)
		return model, command
	case tea.WindowSizeMsg:
		if message.Width > 0 {
			model.width = message.Width
		}
	case tea.KeyPressMsg:
		if message.String() == "ctrl+c" || message.String() == "esc" || message.String() == "q" {
			model.cancelled = true
			model.cancel()
			return model, tea.Quit
		}
	case progressUpdateMessage:
		for index, step := range model.request.Steps {
			if step.ID == message.id {
				model.current = index
				break
			}
		}
		return model, model.waitForUpdate()
	case progressResultMessage:
		model.err = message.err
		if message.err == nil {
			model.current = len(model.request.Steps)
		}
		return model, tea.Quit
	}
	return model, nil
}

func (model *progressModel) View() tea.View {
	theme := newVisualTheme(model.color)
	header := theme.header(model.request.Title, friendlyProjectName(model.request.Project), model.width)
	body := terminal.SanitizeLine(model.request.Detail)
	for index, step := range model.request.Steps {
		marker := "[ ]"
		label := terminal.SanitizeLine(step.Label)
		switch {
		case index < model.current:
			marker = theme.success.Render("✓")
		case index == model.current && model.err == nil:
			marker = model.spinner.View()
		case index == model.current && model.err != nil:
			marker = theme.danger.Render("!")
		}
		body += "\n\n" + marker + " " + label
	}
	body += "\n\n" + theme.muted.Render("DSX shows milestones here; command output stays hidden unless an error occurs.")
	content := header + "\n\n" + theme.panel("Workspace progress", body, model.width, true)
	content = terminal.Wrap(content, tuiContentWidth(model.width))
	view := tea.NewView(theme.layout(content, model.width))
	view.AltScreen = true
	return view
}

// RunProgress keeps a bounded progress screen active while lifecycle work runs.
func (runner *Runner) RunProgress(ctx context.Context, request ProgressRequest, operation ProgressOperation) error {
	if runner == nil || operation == nil || len(request.Steps) == 0 {
		return fmt.Errorf("progress operation is not configured")
	}
	if request.Accessible {
		return runner.runAccessibleProgress(ctx, request, operation)
	}
	operationCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	model := newProgressModel(operationCtx, cancel, request, operation)
	final, err := runner.runModel(model)
	if err != nil {
		return err
	}
	completed, ok := final.(*progressModel)
	if !ok {
		return fmt.Errorf("progress operation returned an unexpected model")
	}
	if completed.cancelled && completed.err == nil {
		return context.Canceled
	}
	return completed.err
}

func (runner *Runner) runAccessibleProgress(ctx context.Context, request ProgressRequest, operation ProgressOperation) error {
	if runner.Output == nil {
		return fmt.Errorf("progress output is unavailable")
	}
	if _, err := fmt.Fprintf(runner.Output, "\n%s\n\n%s\n", terminal.SanitizeLine(request.Title), terminal.SanitizeLine(request.Detail)); err != nil {
		return fmt.Errorf("write accessible progress heading: %w", err)
	}
	labels := make(map[string]string, len(request.Steps))
	for _, step := range request.Steps {
		labels[step.ID] = step.Label
	}
	return operation(ctx, func(id string) {
		label, found := labels[id]
		if !found {
			return
		}
		_, _ = io.WriteString(runner.Output, "- "+terminal.SanitizeLine(label)+"\n")
	})
}
