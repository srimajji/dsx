package tui

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"

	modelpkg "github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/terminal"
)

const (
	maxDashboardFieldBytes    = 256
	maxWorkspaceNameBytes     = 24
	defaultDashboardWidth     = 80
	defaultDashboardHeight    = 24
	maxVisibleWorkspaceRows   = 12
	dashboardNonWorkspaceRows = 14
	workspaceDisplayRowHeight = 2
	createFormFocusCount      = 5
	agentFormFocusCount       = 3
	intentAWSEnable           = "aws-enable"
	intentAWSDisable          = "aws-disable"
)

type DashboardData struct {
	Root          string
	Branch        string
	Revision      string
	Clean         bool
	AllowedAgents []string
	DefaultAgent  string
	AWSCapability string
	Workspaces    []DashboardWorkspace
}

type DashboardWorkspace struct {
	Name                string
	State               string
	DefaultAgent        string
	MutationActive      bool
	AWSEnabled          bool
	AWSHostAvailability string
	AWSMirrorHealth     string
	AWSFailureCode      string
}

type Intent struct {
	Action         string
	Root           string
	Workspace      string
	SourceBranch   string
	SourceRevision string
	Snapshot       bool
	Agent          string
	Browser        bool
	Open           bool
}

type dashboardScreen uint8

const (
	dashboardHome dashboardScreen = iota
	dashboardCreate
	dashboardCreateSnapshotReview
	dashboardAgent
	dashboardGit
	dashboardRemove
	dashboardAWS
)

type DashboardModel struct {
	data              DashboardData
	intent            *Intent
	width             int
	height            int
	accessible        bool
	selected          int
	screen            dashboardScreen
	focus             int
	name              string
	agentIndex        int
	browser           bool
	snapshot          bool
	pendingCreateOpen bool
	notice            string
}

func NewDashboardModel(data DashboardData) *DashboardModel {
	data.Branch = boundedLine(data.Branch)
	data.Revision = boundedLine(data.Revision)
	data.DefaultAgent = boundedLine(data.DefaultAgent)
	if data.AWSCapability != "host-default" {
		data.AWSCapability = "none"
	}
	data.AllowedAgents = normalizedAgents(data.AllowedAgents)
	if !slices.Contains(data.AllowedAgents, data.DefaultAgent) {
		data.DefaultAgent = ""
	}
	available := make([]DashboardWorkspace, 0, len(data.Workspaces))
	for _, workspace := range data.Workspaces {
		name := workspace.Name
		if _, err := modelpkg.ParseWorkspaceName(name); err != nil || workspace.State == "deleted" {
			continue
		}
		workspace.State = boundedLine(workspace.State)
		workspace.AWSHostAvailability = boundedAWSState(workspace.AWSHostAvailability, "unknown")
		workspace.AWSMirrorHealth = boundedAWSState(workspace.AWSMirrorHealth, "unknown")
		workspace.AWSFailureCode = boundedAWSState(workspace.AWSFailureCode, "")
		workspace.MutationActive = workspace.MutationActive || workspace.State == "planned" || workspace.State == "creating" || workspace.State == "cleaning"
		workspace.DefaultAgent = boundedLine(workspace.DefaultAgent)
		if !slices.Contains(data.AllowedAgents, workspace.DefaultAgent) {
			workspace.DefaultAgent = ""
		}
		available = append(available, workspace)
	}
	slices.SortStableFunc(available, func(left, right DashboardWorkspace) int {
		return strings.Compare(left.Name, right.Name)
	})
	data.Workspaces = available
	model := &DashboardModel{data: data, width: defaultDashboardWidth, height: defaultDashboardHeight}
	model.agentIndex = model.defaultAgentIndex("")
	return model
}

func (model *DashboardModel) Init() tea.Cmd { return nil }

func (model *DashboardModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := message.(tea.WindowSizeMsg); ok {
		if size.Width > 0 {
			model.width = size.Width
		}
		if size.Height > 0 {
			model.height = size.Height
		}
		return model, nil
	}
	key, ok := message.(tea.KeyPressMsg)
	if !ok {
		return model, nil
	}
	pressed := strings.ToLower(key.String())
	if pressed == "ctrl+c" || pressed == "q" && model.screen == dashboardHome {
		return model, tea.Quit
	}
	switch model.screen {
	case dashboardCreate:
		return model.updateCreate(key, pressed)
	case dashboardCreateSnapshotReview:
		return model.updateCreateSnapshotReview(pressed)
	case dashboardAgent:
		return model.updateAgent(pressed)
	case dashboardGit:
		return model.updateGit(pressed)
	case dashboardRemove:
		return model.updateRemove(pressed)
	case dashboardAWS:
		return model.updateAWS(pressed)
	default:
		return model.updateHome(pressed)
	}
}

func (model *DashboardModel) updateHome(pressed string) (tea.Model, tea.Cmd) {
	model.notice = ""
	switch pressed {
	case "q", "esc":
		return model, tea.Quit
	case "up", "k":
		model.moveSelection(-1)
	case "down", "j":
		model.moveSelection(1)
	case "c":
		if model.sourceIdentityReady() {
			model.screen, model.focus, model.name = dashboardCreate, 0, ""
			model.agentIndex = model.defaultAgentIndex("")
			model.snapshot, model.pendingCreateOpen = false, false
		}
	case "enter":
		if workspace := model.selectedWorkspace(); workspace != nil && canOpen(*workspace) {
			return model.emit("workspace-open", *workspace)
		}
	case "v":
		if workspace := model.selectedWorkspace(); workspace != nil && workspace.State == "running" && !workspace.MutationActive {
			return model.emit("vscode-attach", *workspace)
		}
	case "a":
		if workspace := model.selectedWorkspace(); workspace != nil && canAgent(*workspace) && len(model.data.AllowedAgents) != 0 {
			model.screen, model.focus, model.browser = dashboardAgent, 0, false
			model.agentIndex = model.defaultAgentIndex(workspace.DefaultAgent)
		}
	case "u":
		if workspace := model.selectedWorkspace(); model.sourceReady() && workspace != nil && canUpdate(*workspace) {
			return model.emit("workspace-update", *workspace)
		}
	case "s":
		if workspace := model.selectedWorkspace(); workspace != nil && !workspace.MutationActive {
			switch workspace.State {
			case "running", "needs_resolution":
				return model.emit("workspace-stop", *workspace)
			case "stopped":
				return model.emit("workspace-start", *workspace)
			}
		}
	case "r":
		if workspace := model.selectedWorkspace(); workspace != nil && canRestart(*workspace) {
			return model.emit("workspace-restart", *workspace)
		}
	case "g":
		if workspace := model.selectedWorkspace(); workspace != nil && canGit(*workspace) {
			model.screen, model.focus = dashboardGit, 0
		}
	case "w":
		if workspace := model.selectedWorkspace(); workspace != nil && canAWS(*workspace) && model.data.AWSCapability == "host-default" {
			model.screen = dashboardAWS
		}
	case "d":
		if workspace := model.selectedWorkspace(); workspace != nil && canRemove(*workspace) {
			model.screen = dashboardRemove
		}
	}
	return model, nil
}

func (model *DashboardModel) updateCreate(key tea.KeyPressMsg, pressed string) (tea.Model, tea.Cmd) {
	switch pressed {
	case "esc":
		model.cancelForm()
	case "tab", "down":
		model.focus = (model.focus + 1) % createFormFocusCount
	case "shift+tab", "up":
		model.focus = (model.focus + createFormFocusCount - 1) % createFormFocusCount
	case "left", "k":
		if model.focus == 1 {
			model.moveAgent(-1)
		} else if model.focus == 0 && pressed == "k" && len(model.name)+len(key.Text) <= maxWorkspaceNameBytes {
			model.name += key.Text
		}
	case "right", "j":
		if model.focus == 1 {
			model.moveAgent(1)
		} else if model.focus == 0 && pressed == "j" && len(model.name)+len(key.Text) <= maxWorkspaceNameBytes {
			model.name += key.Text
		}
	case " ", "space":
		if model.focus == 2 {
			model.snapshot = !model.snapshot
			model.notice = ""
		}
	case "backspace":
		if model.focus == 0 && model.name != "" {
			_, size := utf8.DecodeLastRuneInString(model.name)
			model.name = model.name[:len(model.name)-size]
		}
	case "enter":
		switch model.focus {
		case 2:
			model.snapshot = !model.snapshot
			model.notice = ""
		case 3, 4:
			return model.submitCreate(model.focus == 3)
		}
	default:
		if model.focus == 0 && key.Text != "" && len(model.name)+len(key.Text) <= maxWorkspaceNameBytes {
			model.name += key.Text
		}
	}
	return model, nil
}

func (model *DashboardModel) submitCreate(open bool) (tea.Model, tea.Cmd) {
	name, err := modelpkg.ParseWorkspaceName(model.name)
	if err != nil {
		model.notice = "Use 1–24 lowercase letters, digits, or hyphens; a hyphen cannot be first or last."
		return model, nil
	}
	for _, workspace := range model.data.Workspaces {
		if workspace.Name == string(name) {
			model.notice = "A workspace with this name already exists."
			return model, nil
		}
	}
	if !model.data.Clean && !model.snapshot {
		model.notice = "Enable Include local changes to create from a dirty checkout."
		return model, nil
	}
	if model.snapshot {
		model.pendingCreateOpen = open
		model.screen = dashboardCreateSnapshotReview
		model.notice = ""
		return model, nil
	}
	model.intent = &Intent{
		Action: "workspace-create", Root: model.data.Root, Workspace: string(name),
		SourceBranch: model.data.Branch, SourceRevision: model.data.Revision,
		Agent: model.selectedAgent(), Open: open,
	}
	return model, tea.Quit
}

func (model *DashboardModel) updateCreateSnapshotReview(pressed string) (tea.Model, tea.Cmd) {
	switch pressed {
	case "esc", "n":
		model.screen = dashboardCreate
		model.focus = 2
	case "enter", "y":
		model.intent = &Intent{
			Action: "workspace-create", Root: model.data.Root, Workspace: model.name,
			SourceBranch: model.data.Branch, SourceRevision: model.data.Revision,
			Snapshot: true, Agent: model.selectedAgent(), Open: model.pendingCreateOpen,
		}
		return model, tea.Quit
	}
	return model, nil
}

func (model *DashboardModel) updateAgent(pressed string) (tea.Model, tea.Cmd) {
	switch pressed {
	case "esc":
		model.cancelForm()
	case "tab", "down":
		model.focus = (model.focus + 1) % agentFormFocusCount
	case "shift+tab", "up":
		model.focus = (model.focus + agentFormFocusCount - 1) % agentFormFocusCount
	case "left", "k":
		if model.focus == 0 {
			model.moveAgent(-1)
		}
	case "right", "j":
		if model.focus == 0 {
			model.moveAgent(1)
		}
	case " ", "space":
		if model.focus == 1 {
			model.browser = !model.browser
		}
	case "enter":
		if model.focus == 1 {
			model.browser = !model.browser
		} else if model.focus == 2 {
			workspace := model.selectedWorkspace()
			if workspace != nil && canAgent(*workspace) {
				model.intent = &Intent{Action: "agent-run", Root: model.data.Root, Workspace: workspace.Name, Agent: model.selectedAgent(), Browser: model.browser}
				return model, tea.Quit
			}
		}
	}
	return model, nil
}

func (model *DashboardModel) updateGit(pressed string) (tea.Model, tea.Cmd) {
	if pressed == "esc" {
		model.cancelForm()
		return model, nil
	}
	actions := map[string]string{"s": "git-status", "d": "git-diff", "f": "git-fetch", "a": "git-apply"}
	action, found := actions[pressed]
	workspace := model.selectedWorkspace()
	if !found || workspace == nil || !canGit(*workspace) {
		return model, nil
	}
	model.intent = &Intent{Action: action, Root: model.data.Root, Workspace: workspace.Name}
	return model, tea.Quit
}

func (model *DashboardModel) updateRemove(pressed string) (tea.Model, tea.Cmd) {
	switch pressed {
	case "esc", "n":
		model.cancelForm()
	case "y", "enter":
		if workspace := model.selectedWorkspace(); workspace != nil && canRemove(*workspace) {
			return model.emit("workspace-remove", *workspace)
		}
	}
	return model, nil
}

func (model *DashboardModel) updateAWS(pressed string) (tea.Model, tea.Cmd) {
	switch pressed {
	case "esc", "n":
		model.cancelForm()
	case "y", "enter":
		workspace := model.selectedWorkspace()
		if workspace == nil || !canAWS(*workspace) || model.data.AWSCapability != "host-default" {
			return model, nil
		}
		action := intentAWSEnable
		if workspace.AWSEnabled {
			action = intentAWSDisable
		}
		return model.emit(action, *workspace)
	}
	return model, nil
}

func (model *DashboardModel) emit(action string, workspace DashboardWorkspace) (tea.Model, tea.Cmd) {
	model.intent = &Intent{Action: action, Root: model.data.Root, Workspace: workspace.Name}
	return model, tea.Quit
}

func (model *DashboardModel) cancelForm() {
	model.screen, model.focus, model.notice, model.browser = dashboardHome, 0, "", false
	model.snapshot, model.pendingCreateOpen = false, false
}
func (model *DashboardModel) moveSelection(delta int) {
	if len(model.data.Workspaces) != 0 {
		model.selected = (model.selected + delta + len(model.data.Workspaces)) % len(model.data.Workspaces)
	}
}
func (model *DashboardModel) selectedWorkspace() *DashboardWorkspace {
	if model.selected < 0 || model.selected >= len(model.data.Workspaces) {
		return nil
	}
	return &model.data.Workspaces[model.selected]
}
func (model *DashboardModel) defaultAgentIndex(workspaceDefault string) int {
	selected := workspaceDefault
	if selected == "" {
		selected = model.data.DefaultAgent
	}
	return max(0, slices.Index(model.data.AllowedAgents, selected))
}
func (model *DashboardModel) moveAgent(delta int) {
	if len(model.data.AllowedAgents) != 0 {
		model.agentIndex = (model.agentIndex + delta + len(model.data.AllowedAgents)) % len(model.data.AllowedAgents)
	}
}
func (model *DashboardModel) selectedAgent() string {
	if model.agentIndex < 0 || model.agentIndex >= len(model.data.AllowedAgents) {
		return ""
	}
	return model.data.AllowedAgents[model.agentIndex]
}
func (model *DashboardModel) Intent() (Intent, bool) {
	if model.intent == nil {
		return Intent{}, false
	}
	return *model.intent, true
}

func (model *DashboardModel) View() tea.View {
	theme := newVisualTheme(terminal.ColorEnabled() && !model.accessible)
	width := tuiContentWidth(model.width)
	header := theme.header("Workspaces", friendlyProjectName(model.data.Root), width)
	var content string
	switch model.screen {
	case dashboardCreate:
		content = model.renderCreate(theme)
	case dashboardCreateSnapshotReview:
		content = model.renderCreateSnapshotReview(theme)
	case dashboardAgent:
		content = model.renderAgent(theme)
	case dashboardGit:
		content = model.renderGit(theme)
	case dashboardAWS:
		content = model.renderAWS(theme)
	case dashboardRemove:
		content = model.renderRemove(theme)
	default:
		content = model.renderHome(theme)
	}
	rendered := header + tuiGap(model.height) + content
	if !model.accessible {
		rendered = theme.layoutAt(rendered, model.width, width)
	}
	view := tea.NewView(rendered)
	view.AltScreen = !model.accessible
	return view
}

func (model *DashboardModel) renderHome(theme visualTheme) string {
	width := tuiContentWidth(model.width)
	source := model.renderProjectSource(theme)
	if compactTUILayout(width, model.height) {
		return model.renderCompactHome(theme, width)
	}
	if width >= 96 && model.height >= 24 {
		leftWidth := max(30, min(38, (width-2)/3))
		rightWidth := width - leftWidth - 2
		leftBodyWidth := theme.panelBodyWidth(leftWidth, model.height)
		rightBodyWidth := theme.panelBodyWidth(rightWidth, model.height)
		left := theme.panel("Workspaces", model.renderWorkspaceList(theme, leftBodyWidth), leftWidth, model.height, len(model.data.Workspaces) != 0)
		right := theme.panel("Selected workspace", model.renderSelectedWorkspace(theme, rightBodyWidth), rightWidth, model.height, true)
		columns := lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", right)
		return theme.panel("Local checkout", source, width, model.height, false) + tuiGap(model.height) + columns
	}
	workspaceWidth := theme.panelBodyWidth(width, model.height)
	return theme.panel("Local checkout", source, width, model.height, false) + tuiGap(model.height) +
		theme.panel("Workspaces", model.renderWorkspaceList(theme, workspaceWidth), width, model.height, len(model.data.Workspaces) != 0) + tuiGap(model.height) +
		theme.panel("Selected workspace", model.renderSelectedWorkspace(theme, workspaceWidth), width, model.height, true)
}

func (model *DashboardModel) renderCompactHome(theme visualTheme, width int) string {
	sourceState := theme.success.Render("Ready")
	if !model.sourceIdentityReady() {
		sourceState = theme.warning.Render("Source unavailable")
	} else if !model.data.Clean {
		sourceState = theme.warning.Render("Local files changed")
	}
	body := theme.section.Render("Local checkout") + "\n" +
		wrapTUIText(model.checkoutLabel()+" · "+sourceState, width)
	workspace := model.selectedWorkspace()
	if width >= 60 && len(model.data.Workspaces) != 0 {
		body += "\n" + theme.section.Render("Workspaces") + "\n" + model.renderCompactWorkspaceList(theme, width)
	}
	if workspace == nil {
		return body + "\n" + theme.section.Render("Workspaces") + "\n" +
			theme.muted.Render("No workspaces yet.") + "\n" +
			theme.section.Render("Actions") + "\n" + model.renderActions(theme, width)
	}
	state, tone := workspaceStateLabel(workspace.State)
	defaultAgent := workspace.DefaultAgent
	if defaultAgent == "" {
		defaultAgent = model.data.DefaultAgent
	}
	body += "\n" + theme.section.Render(fmt.Sprintf("Workspace %d of %d", model.selected+1, len(model.data.Workspaces))) +
		"\n" + theme.title.Render(workspace.Name) + "  " + theme.badge(state, tone) +
		"\n" + wrapTUIText(theme.muted.Render("Assistant: "+agentDisplayName(defaultAgent)), width)
	if workspace.MutationActive {
		body += "\n" + wrapTUIText(theme.warning.Render(workspaceStateDescription(*workspace)), width)
	}
	if model.data.AWSCapability == "host-default" {
		body += "\n" + wrapTUIText(theme.muted.Render("AWS: "+awsGrantLabel(workspace.AWSEnabled)+" · host "+strings.ToLower(awsAvailabilityShort(workspace.AWSHostAvailability))+" · mirror "+strings.ToLower(awsMirrorLabel(workspace.AWSMirrorHealth))), width)
	}
	if guidance := model.actionGuidance(*workspace); guidance != "" {
		body += "\n" + wrapTUIText(theme.warning.Render(guidance), width)
	}
	return body + "\n" + theme.section.Render("Actions") + "\n" + model.renderActions(theme, width)
}

func (model *DashboardModel) renderCompactWorkspaceList(theme visualTheme, width int) string {
	maxVisible := max(1, min(len(model.data.Workspaces), max(1, model.height-14)))
	start := max(0, model.selected-maxVisible+1)
	end := min(len(model.data.Workspaces), start+maxVisible)
	var output strings.Builder
	for index := start; index < end; index++ {
		workspace := model.data.Workspaces[index]
		marker, nameStyle := "  ", theme.title
		if index == model.selected {
			marker, nameStyle = "> ", theme.accent
		}
		state, tone := workspaceStateLabel(workspace.State)
		nameWidth := max(1, width-terminal.Width(state)-4)
		fmt.Fprintf(&output, "%s%s  %s", marker, nameStyle.Render(terminal.Truncate(workspace.Name, nameWidth)), theme.badge(state, tone))
		if index+1 < end {
			output.WriteByte('\n')
		}
	}
	return output.String()
}

func (model *DashboardModel) renderProjectSource(theme visualTheme) string {
	source := theme.value.Render(model.checkoutLabel())
	switch {
	case !model.sourceIdentityReady():
		return source + "\n" + theme.warning.Render("Source unavailable — DSX cannot create or update a workspace.")
	case model.data.Clean:
		return source + "\n" + theme.success.Render("Ready — new workspaces use this committed source.")
	default:
		return source + "\n" + theme.warning.Render("Local files changed — commit them for normal create/update, or review a snapshot when creating.")
	}
}

func (model *DashboardModel) renderWorkspaceList(theme visualTheme, width int) string {
	if len(model.data.Workspaces) == 0 {
		return theme.muted.Render("No workspaces yet.\n\nPress c to create an isolated Linux workspace for this project.")
	}
	rowHeight := workspaceDisplayRowHeight
	if model.data.AWSCapability == "host-default" {
		rowHeight++
	}
	maxVisible := max(1, min(maxVisibleWorkspaceRows, (model.height-dashboardNonWorkspaceRows)/rowHeight))
	start := max(0, model.selected-maxVisible+1)
	end := min(len(model.data.Workspaces), start+maxVisible)
	if end-start < maxVisible {
		start = max(0, end-maxVisible)
	}
	var output strings.Builder
	if start > 0 {
		fmt.Fprintf(&output, "↑ %d more\n", start)
	}
	for index := start; index < end; index++ {
		workspace := model.data.Workspaces[index]
		marker, nameStyle := "  ", theme.title
		if index == model.selected {
			marker, nameStyle = "> ", theme.accent
		}
		state, tone := workspaceStateLabel(workspace.State)
		nameWidth := max(1, width-terminal.Width(state)-4)
		name := terminal.Truncate(boundedLine(workspace.Name), nameWidth)
		fmt.Fprintf(&output, "%s%s  %s\n", marker, nameStyle.Render(name), theme.badge(state, tone))
		defaultAgent := workspace.DefaultAgent
		if defaultAgent == "" {
			defaultAgent = model.data.DefaultAgent
		}
		detail := "  Opens with " + agentDisplayName(defaultAgent)
		if model.data.AWSCapability == "host-default" {
			detail += " · AWS " + strings.ToLower(awsGrantLabel(workspace.AWSEnabled))
		}
		fmt.Fprintln(&output, terminal.Truncate(detail, width))
	}
	if remaining := len(model.data.Workspaces) - end; remaining > 0 {
		fmt.Fprintf(&output, "↓ %d more\n", remaining)
	}
	return strings.TrimRight(output.String(), "\n")
}

func (model *DashboardModel) renderSelectedWorkspace(theme visualTheme, width int) string {
	workspace := model.selectedWorkspace()
	if workspace == nil {
		return theme.title.Render("Create your first workspace") +
			"\n" + theme.muted.Render("A workspace is a private Linux environment for this project. Your Mac checkout stays separate.") +
			"\n\n" + theme.section.Render("Actions") + "\n" + model.renderActions(theme, width)
	}
	state, tone := workspaceStateLabel(workspace.State)
	defaultAgent, inherited := workspace.DefaultAgent, ""
	if defaultAgent == "" {
		defaultAgent, inherited = model.data.DefaultAgent, " (project default)"
	}
	body := theme.title.Render(workspace.Name) + "  " + theme.badge(state, tone) +
		"\n" + theme.muted.Render(workspaceStateDescription(*workspace)) +
		"\n\n" + theme.label.Render("Coding assistant") + "\n" + agentDisplayName(defaultAgent) + inherited +
		"\n" + theme.muted.Render("Available: "+agentListDisplay(model.data.AllowedAgents))
	if model.data.AWSCapability == "host-default" {
		body += "\n\n" + theme.label.Render("AWS") +
			"\nGrant: " + awsGrantLabel(workspace.AWSEnabled) + " · Host: " + awsAvailabilityShort(workspace.AWSHostAvailability) +
			" · Mirror: " + awsMirrorLabel(workspace.AWSMirrorHealth)
	}
	if guidance := model.actionGuidance(*workspace); guidance != "" {
		body += "\n\n" + theme.warning.Render(guidance)
	}
	return body + "\n\n" + theme.section.Render("Actions") + "\n" + model.renderActions(theme, width)
}

func workspaceStateDescription(workspace DashboardWorkspace) string {
	if workspace.MutationActive {
		return "A lifecycle change is running. Conflicting actions are temporarily unavailable."
	}
	switch workspace.State {
	case "running":
		return "Ready. Open a shell or start a coding-assistant session."
	case "stopped":
		return "Saved but powered off. Opening it starts the workspace first."
	case "needs_resolution":
		return "Git needs conflict resolution. Open the workspace to resolve it safely."
	case "failed":
		return "The last lifecycle operation failed. Review Git state or remove the workspace when safe."
	default:
		return "DSX is tracking this workspace."
	}
}

func (model *DashboardModel) actionGuidance(workspace DashboardWorkspace) string {
	switch {
	case workspace.MutationActive:
		return "Wait for the current lifecycle change before updating or restarting."
	case canUpdate(workspace) && !model.sourceIdentityReady():
		return "Update is unavailable because the local source branch or revision could not be identified."
	case canUpdate(workspace) && !model.data.Clean:
		return "Update needs a clean checkout. Commit local work, or use dsx workspace update " + workspace.Name + " --snapshot."
	default:
		return ""
	}
}

func (model *DashboardModel) renderActions(theme visualTheme, width int) string {
	actions := []string{"[c] New workspace"}
	if !model.sourceIdentityReady() {
		actions[0] = "[c] New workspace unavailable"
	}
	workspace := model.selectedWorkspace()
	if workspace != nil {
		if canOpen(*workspace) {
			actions = append(actions, "[Enter] Open shell")
		}
		if workspace.State == "running" && !workspace.MutationActive {
			actions = append(actions, "[v] Open in VS Code (experimental)")
		}
		if canAgent(*workspace) && len(model.data.AllowedAgents) != 0 {
			actions = append(actions, "[a] Open coding assistant")
		}
		if model.sourceReady() && canUpdate(*workspace) {
			actions = append(actions, "[u] Update from this Mac")
		}
		if !workspace.MutationActive && (workspace.State == "running" || workspace.State == "needs_resolution") {
			actions = append(actions, "[s] Stop")
		} else if !workspace.MutationActive && workspace.State == "stopped" {
			actions = append(actions, "[s] Start")
		}
		if canRestart(*workspace) {
			actions = append(actions, "[r] Restart")
		}
		if canGit(*workspace) {
			actions = append(actions, "[g] Review Git changes")
		}
		if model.data.AWSCapability == "host-default" && canAWS(*workspace) {
			if workspace.AWSEnabled {
				actions = append(actions, "[w] Disable AWS")
			} else {
				actions = append(actions, "[w] Enable AWS")
			}
		}
		if canRemove(*workspace) {
			actions = append(actions, "[d] Remove")
		}
	}
	actions = append(actions, "[↑/↓] Select workspace", "[q] Quit")
	return theme.help(width, actions...)
}

func (model *DashboardModel) sourceReady() bool {
	return model.data.Clean && model.sourceIdentityReady()
}

func (model *DashboardModel) sourceIdentityReady() bool {
	return model.data.Branch != "" && model.data.Revision != ""
}

func (model *DashboardModel) sourceBlockedReason() string {
	return "source branch or revision is unavailable"
}

func (model *DashboardModel) checkoutLabel() string {
	if model.data.Branch == "" || model.data.Revision == "" {
		return "Unavailable"
	}
	return boundedLine(model.data.Branch) + " @ " + boundedLine(model.data.Revision)
}

func (model *DashboardModel) renderCreate(theme visualTheme) string {
	width := tuiContentWidth(model.width)
	bodyWidth := theme.panelBodyWidth(width, model.height)
	name := model.name
	if name == "" {
		name = "feature-a"
	}
	agent := model.selectedAgent()
	displayAgent := agentDisplayName(agent)
	if agent == "" {
		displayAgent = "No approved assistants"
	}
	inherited := ""
	if agent == model.data.DefaultAgent {
		inherited = " — project default"
	}
	check := "[ ] Off"
	if model.snapshot {
		check = "[x] On"
	}
	if compactTUILayout(width, model.height) {
		fields := []string{
			formRow(theme, true, "Workspace name", name, bodyWidth) + "\n" + theme.muted.Render("Use a short label: lowercase letters, numbers, and hyphens."),
			formRow(theme, true, "Coding assistant", displayAgent+inherited, bodyWidth),
			formRow(theme, true, "Include local changes", check, bodyWidth) + "\n" + theme.muted.Render("Off uses committed source. On requires a separate safety review."),
			formChoice(theme, true, "Create and open a shell"),
			formChoice(theme, true, "Create in background"),
		}
		body := theme.muted.Render(fmt.Sprintf("Step %d of %d", model.focus+1, createFormFocusCount)) + "\n\n" + fields[model.focus]
		if model.focus == 0 {
			body += "\n" + theme.muted.Render("Starting from "+boundedLine(model.data.Branch)+" @ "+boundedLine(model.data.Revision))
		}
		if model.notice != "" {
			body += "\n\n" + theme.warning.Render(boundedLine(model.notice))
		}
		body += "\n\n" + theme.help(bodyWidth, "[Tab/↑/↓] Move", "[←/→] Change", "[Space] Toggle", "[Enter] Choose", "[Esc] Cancel")
		return theme.panel("Create workspace", body, width, model.height, true)
	}
	body := theme.muted.Render("Create a private Linux environment from "+boundedLine(model.data.Branch)+" @ "+boundedLine(model.data.Revision)+".") + "\n\n" +
		formRow(theme, model.focus == 0, "Workspace name", name, bodyWidth) + "\n" +
		formRow(theme, model.focus == 1, "Coding assistant", displayAgent+inherited, bodyWidth) + "\n" +
		formRow(theme, model.focus == 2, "Include local changes", check+" — reviewed final working-tree content", bodyWidth) + "\n\n" +
		formChoice(theme, model.focus == 3, "Create and open a shell") + "\n" +
		formChoice(theme, model.focus == 4, "Create in background")
	if model.notice != "" {
		body += "\n\n" + theme.warning.Render(boundedLine(model.notice))
	}
	body += "\n\n" + theme.help(bodyWidth, "[Tab/↑/↓] Move", "[←/→] Change assistant", "[Space] Toggle", "[Enter] Choose", "[Esc] Cancel")
	return theme.panel("Create workspace", body, width, model.height, true)
}

func (model *DashboardModel) renderCreateSnapshotReview(theme visualTheme) string {
	width := tuiContentWidth(model.width)
	bodyWidth := theme.panelBodyWidth(width, model.height)
	body := theme.warning.Render("Create "+boundedLine(model.name)+" from local changes?") +
		"\n\nParent source\n  " + boundedLine(model.data.Branch) + " @ " + boundedLine(model.data.Revision) +
		"\n\nIncludes final tracked file content and nonignored untracked files." +
		"\nIgnored untracked files stay on the host; tracked files remain included." +
		"\nUnmerged paths and Git submodules are rejected." +
		"\nYour branch, HEAD, index, worktree, and durable refs are not changed." +
		"\n\n" + theme.help(bodyWidth, "[y/Enter] Create and continue", "[n/Esc] Go back")
	return theme.panel("Review source snapshot", body, width, model.height, true)
}

func (model *DashboardModel) renderAgent(theme visualTheme) string {
	width := tuiContentWidth(model.width)
	bodyWidth := theme.panelBodyWidth(width, model.height)
	workspace := model.selectedWorkspace()
	name := ""
	if workspace != nil {
		name = workspace.Name
	}
	check := "[ ] Off"
	if model.browser {
		check = "[x] On"
	}
	if compactTUILayout(width, model.height) {
		fields := []string{
			formRow(theme, true, "Coding assistant", agentDisplayName(model.selectedAgent()), bodyWidth),
			formRow(theme, true, "Isolated browser", check, bodyWidth) + "\n" + theme.muted.Render("Available only to this assistant session."),
			formChoice(theme, true, "Open coding assistant"),
		}
		body := theme.muted.Render("Workspace "+boundedLine(name)+" · Step "+fmt.Sprintf("%d of %d", model.focus+1, agentFormFocusCount)) +
			"\n\n" + fields[model.focus] +
			"\n\n" + theme.help(bodyWidth, "[Tab/↑/↓] Move", "[←/→] Change", "[Space] Toggle", "[Enter] Choose", "[Esc] Cancel")
		return theme.panel("Open coding assistant", body, width, model.height, true)
	}
	body := theme.muted.Render("Workspace  "+boundedLine(name)) + "\n\n" +
		formRow(theme, model.focus == 0, "Coding assistant", agentDisplayName(model.selectedAgent()), bodyWidth) + "\n" +
		formRow(theme, model.focus == 1, "Isolated browser", check+" — this session only", bodyWidth) + "\n\n" +
		formChoice(theme, model.focus == 2, "Open coding assistant") + "\n\n" +
		theme.help(bodyWidth, "[Tab/↑/↓] Move", "[←/→] Change assistant", "[Space] Toggle browser", "[Enter] Choose", "[Esc] Cancel")
	return theme.panel("Open coding assistant", body, width, model.height, true)
}

func (model *DashboardModel) renderGit(theme visualTheme) string {
	width := tuiContentWidth(model.width)
	bodyWidth := theme.panelBodyWidth(width, model.height)
	workspace := model.selectedWorkspace()
	name := ""
	if workspace != nil {
		name = workspace.Name
	}
	body := "Choose what to review for " + boundedLine(name) + ". Fetch and apply use DSX's guarded result-transfer flow.\n\n" +
		theme.help(bodyWidth, "[s] Status summary", "[d] View diff", "[f] Fetch results", "[a] Apply results", "[Esc] Go back")
	return theme.panel("Review workspace Git changes", body, width, model.height, true)
}

func (model *DashboardModel) renderRemove(theme visualTheme) string {
	width := tuiContentWidth(model.width)
	bodyWidth := theme.panelBodyWidth(width, model.height)
	workspace := model.selectedWorkspace()
	name := ""
	if workspace != nil {
		name = workspace.Name
	}
	body := theme.danger.Render("Remove workspace "+boundedLine(name)+"?") +
		"\n\nDSX preserves unfetched or uncertain work. Destructive loss confirmation remains unavailable in this dashboard." +
		"\n\n" + theme.help(bodyWidth, "[y/Enter] Remove safely", "[n/Esc] Keep workspace")
	return theme.panel("Confirm workspace removal", body, width, model.height, true)
}

func (model *DashboardModel) renderAWS(theme visualTheme) string {
	width := tuiContentWidth(model.width)
	bodyWidth := theme.panelBodyWidth(width, model.height)
	workspace := model.selectedWorkspace()
	if workspace == nil {
		return theme.panel("AWS access", "No workspace selected.\n\n"+theme.help(bodyWidth, "[Esc] Go back"), width, model.height, true)
	}
	host := awsAvailabilityLabel(workspace.AWSHostAvailability)
	if workspace.AWSHostAvailability != "available" {
		host += "\nStart or renew one complete temporary [default] session in Leapp Desktop or a compatible provider, then try again."
	}
	if compactTUILayout(width, model.height) {
		if workspace.AWSEnabled {
			body := theme.danger.Render("Disable AWS for "+boundedLine(workspace.Name)+"?") +
				"\nHost: " + host +
				"\nAccess is revoked immediately for this workspace only. Its mirror and helper are removed; other workspaces are unchanged." +
				"\n" + theme.help(bodyWidth, "[y/Enter] Disable AWS", "[n/Esc] Keep enabled")
			return theme.panel("Confirm AWS disable", body, width, model.height, true)
		}
		body := theme.warning.Render("Enable AWS for "+boundedLine(workspace.Name)+"?") +
			"\nHost: " + host +
			"\nThis workspace continuously follows the host [default]. Switching it changes the AWS account or role without another approval or restart. Named profiles are unavailable. Other workspaces are unchanged." +
			"\n" + theme.help(bodyWidth, "[y/Enter] Enable AWS", "[n/Esc] Keep disabled")
		return theme.panel("Review dynamic AWS access", body, width, model.height, true)
	}
	if workspace.AWSEnabled {
		body := theme.danger.Render("Disconnect AWS from "+boundedLine(workspace.Name)+"?") +
			"\n\nHost default\n" + host +
			"\n\nAccess is revoked immediately for this workspace only. Its AWS mirror and helper are removed; other workspaces are unchanged." +
			"\n\n" + theme.help(bodyWidth, "[y/Enter] Disable AWS", "[n/Esc] Keep enabled")
		return theme.panel("Confirm AWS disable", body, width, model.height, true)
	}
	body := theme.warning.Render("Connect "+boundedLine(workspace.Name)+" to the current host AWS default?") +
		"\n\nHost default\n" + host +
		"\n\nThis workspace and its coding assistants will continuously follow whichever AWS account and role the host provider assigns to [default]. Switching [default] changes authority without another approval or restart. Named profiles are unavailable. Other workspaces are unchanged." +
		"\n\n" + theme.help(bodyWidth, "[y/Enter] Enable AWS", "[n/Esc] Keep disabled")
	return theme.panel("Review dynamic AWS access", body, width, model.height, true)
}

func formRow(theme visualTheme, active bool, label, value string, width int) string {
	marker, style := "  ", theme.value
	if active {
		marker, style = "> ", theme.accent
	}
	indent := "    "
	wrapped := wrapTUIText(style.Render(boundedLine(value)), max(1, width-terminal.Width(indent)))
	wrapped = strings.ReplaceAll(wrapped, "\n", "\n"+indent)
	return marker + theme.label.Render(label) + "\n" + indent + wrapped
}

func formChoice(theme visualTheme, active bool, label string) string {
	marker, style := "  ", theme.value
	if active {
		marker, style = "> ", theme.accent
	}
	return marker + style.Render("[ "+boundedLine(label)+" ]")
}

func canOpen(workspace DashboardWorkspace) bool {
	return !workspace.MutationActive && (workspace.State == "running" || workspace.State == "stopped" || workspace.State == "needs_resolution")
}
func canAgent(workspace DashboardWorkspace) bool {
	return !workspace.MutationActive && workspace.State == "running"
}
func canUpdate(workspace DashboardWorkspace) bool {
	return !workspace.MutationActive && (workspace.State == "running" || workspace.State == "stopped")
}
func canRestart(workspace DashboardWorkspace) bool {
	return !workspace.MutationActive && (workspace.State == "running" || workspace.State == "stopped")
}
func canGit(workspace DashboardWorkspace) bool {
	return !workspace.MutationActive && (workspace.State == "running" || workspace.State == "stopped" || workspace.State == "needs_resolution" || workspace.State == "failed")
}
func canRemove(workspace DashboardWorkspace) bool {
	return !workspace.MutationActive && workspace.State != "deleted"
}
func canAWS(workspace DashboardWorkspace) bool {
	return !workspace.MutationActive && (workspace.State == "running" || workspace.State == "stopped" || workspace.State == "needs_resolution")
}

func boundedAWSState(value, fallback string) string {
	value = strings.TrimSpace(value)
	for _, allowed := range []string{"available", "unavailable", "disabled", "stopped", "current", "degraded", "host-unavailable", "manager-unavailable", "status-unavailable", "mirror-disabled", "mirror-degraded", "source_identity_changed", "source_unsafe", "source_oversized", "source_unavailable"} {
		if value == allowed {
			return value
		}
	}
	return fallback
}

func awsGrantLabel(enabled bool) string {
	if enabled {
		return "Enabled"
	}
	return "Disabled"
}

func awsAvailabilityLabel(availability string) string {
	if availability == "available" {
		return "Available — temporary default detected"
	}
	return "Unavailable — start the host default session"
}

func awsAvailabilityShort(availability string) string {
	if availability == "available" {
		return "Available"
	}
	return "Unavailable"
}

func awsMirrorLabel(health string) string {
	switch health {
	case "current":
		return "Current"
	case "stopped":
		return "Stopped"
	case "disabled":
		return "Disabled"
	case "degraded":
		return "Degraded"
	default:
		return "Unavailable"
	}
}
func workspaceStateLabel(state string) (string, string) {
	switch state {
	case "running":
		return "Running", "success"
	case "stopped":
		return "Stopped", "warning"
	case "needs_resolution":
		return "Needs resolution", "danger"
	case "planned":
		return "Planned", "warning"
	case "creating":
		return "Creating", "warning"
	case "failed":
		return "Failed", "danger"
	case "cleaning":
		return "Cleaning", "warning"
	default:
		return "Unavailable", "danger"
	}
}

func agentDisplayName(agent string) string {
	switch agent {
	case "omp":
		return "OMP"
	case "codex":
		return "Codex"
	case "claude":
		return "Claude"
	case "opencode":
		return "OpenCode"
	default:
		return boundedLine(agent)
	}
}

func agentListDisplay(agents []string) string {
	display := make([]string, len(agents))
	for index, agent := range agents {
		display[index] = agentDisplayName(agent)
	}
	return strings.Join(display, " · ")
}

func normalizedAgents(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = boundedLine(value)
		if value != "" && !slices.Contains(result, value) {
			result = append(result, value)
		}
	}
	slices.Sort(result)
	return result
}

func boundedLine(value string) string {
	return terminal.SanitizeN(strings.ReplaceAll(value, "\n", `\n`), maxDashboardFieldBytes)
}
