package tui

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

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
	dashboardActionGap        = "   "
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
		model.notice = "Select Snapshot local changes to create from a dirty checkout."
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
	header := theme.header("Project", friendlyProjectName(model.data.Root), model.width)
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
	rendered := terminal.Wrap(header+"\n\n"+content, tuiContentWidth(model.width))
	if !model.accessible {
		rendered = theme.layout(rendered, model.width)
	}
	view := tea.NewView(rendered)
	view.AltScreen = !model.accessible
	return view
}

func (model *DashboardModel) renderHome(theme visualTheme) string {
	checkout := theme.value.Render(model.checkoutLabel())
	cleanliness := theme.success.Render("Clean")
	if !model.data.Clean {
		cleanliness = theme.warning.Render("Not clean — ordinary create/update require a commit; reviewed snapshots are available.")
	} else if !model.sourceIdentityReady() {
		cleanliness = theme.warning.Render("Source branch or revision unavailable")
	}
	if model.height < defaultDashboardHeight {
		return theme.section.Render("Local checkout") + "\n" + checkout + " · " + cleanliness + "\n\n" +
			theme.section.Render("Workspaces") + "\n" + model.renderWorkspaceList(theme) + "\n\n" + model.renderActions(theme)
	}
	return theme.panel("Local checkout", checkout+"\n"+cleanliness, model.width, false) + "\n\n" +
		theme.panel("Workspaces", model.renderWorkspaceList(theme), model.width, len(model.data.Workspaces) != 0) + "\n\n" + model.renderActions(theme)
}

func (model *DashboardModel) renderWorkspaceList(theme visualTheme) string {
	if len(model.data.Workspaces) == 0 {
		return theme.muted.Render("No workspaces yet. Press c to create one from the committed local checkout.")
	}
	rowHeight := workspaceDisplayRowHeight
	if model.data.AWSCapability == "host-default" {
		rowHeight += 2
	}
	maxVisible := max(1, min(maxVisibleWorkspaceRows, (model.height-dashboardNonWorkspaceRows)/rowHeight))
	start := max(0, model.selected-maxVisible+1)
	end := min(len(model.data.Workspaces), start+maxVisible)
	if end-start < maxVisible {
		start = max(0, end-maxVisible)
	}
	var output strings.Builder
	if start > 0 {
		fmt.Fprintf(&output, "  ↑ %d more\n", start)
	}
	for index := start; index < end; index++ {
		workspace := model.data.Workspaces[index]
		marker, name := "  ", theme.title
		if index == model.selected {
			marker, name = "> ", theme.accent
		}
		state, tone := workspaceStateLabel(workspace.State)
		defaultAgent, inherited := workspace.DefaultAgent, ""
		if defaultAgent == "" {
			defaultAgent, inherited = model.data.DefaultAgent, " (project default)"
		}
		fmt.Fprintf(&output, "%s%s  %s\n", marker, name.Render(boundedLine(workspace.Name)), theme.badge(state, tone))
		fmt.Fprintf(&output, "    Default: %s%s · Approved: %s\n", agentDisplayName(defaultAgent), inherited, agentListDisplay(model.data.AllowedAgents))
		if model.data.AWSCapability == "host-default" {
			status := fmt.Sprintf("    AWS: %s · Host: %s · Mirror: %s", awsGrantLabel(workspace.AWSEnabled), awsAvailabilityShort(workspace.AWSHostAvailability), awsMirrorLabel(workspace.AWSMirrorHealth))
			if workspace.AWSFailureCode != "" {
				status += " · Reason: " + workspace.AWSFailureCode
			}
			fmt.Fprintln(&output, status)
		}
	}
	if remaining := len(model.data.Workspaces) - end; remaining > 0 {
		fmt.Fprintf(&output, "  ↓ %d more\n", remaining)
	}
	return strings.TrimRight(output.String(), "\n")
}

func (model *DashboardModel) renderActions(theme visualTheme) string {
	actions := []string{"[c] Create workspace"}
	if !model.sourceIdentityReady() {
		actions[0] = "[c] Create unavailable — " + model.sourceBlockedReason()
	}
	workspace := model.selectedWorkspace()
	if workspace != nil {
		if canOpen(*workspace) {
			actions = append(actions, "[Enter] Open")
		}
		if workspace.State == "running" && !workspace.MutationActive {
			actions = append(actions, "[v] Attach with VS Code (experimental)")
		}
		if canAgent(*workspace) && len(model.data.AllowedAgents) != 0 {
			actions = append(actions, "[a] Open agent")
		}
		if model.sourceReady() && canUpdate(*workspace) {
			actions = append(actions, "[u] Update from local checkout")
		} else if workspace.MutationActive {
			actions = append(actions, "[u] Update unavailable while lifecycle change runs")
		} else if canUpdate(*workspace) && !model.sourceIdentityReady() {
			actions = append(actions, "[u] Update unavailable — "+model.sourceBlockedReason())
		} else if canUpdate(*workspace) && !model.data.Clean {
			actions = append(actions, "[u] Update unavailable — use dsx workspace update NAME --snapshot")
		}
		if !workspace.MutationActive && (workspace.State == "running" || workspace.State == "needs_resolution") {
			actions = append(actions, "[s] Stop")
		} else if !workspace.MutationActive && workspace.State == "stopped" {
			actions = append(actions, "[s] Start")
		}
		if canRestart(*workspace) {
			actions = append(actions, "[r] Restart")
		} else if workspace.MutationActive {
			actions = append(actions, "[r] Restart unavailable while lifecycle change runs")
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
		} else if model.data.AWSCapability == "host-default" && workspace.MutationActive {
			actions = append(actions, "[w] AWS unavailable while lifecycle change runs")
		}
		if canRemove(*workspace) {
			actions = append(actions, "[d] Remove")
		}
	}
	actions = append(actions, "[q] Quit")
	rendered := make([]string, len(actions))
	for index, action := range actions {
		rendered[index] = theme.help(action)
	}
	var lines strings.Builder
	lineWidth := 0
	for _, action := range rendered {
		actionWidth := terminal.Width(action)
		if lineWidth > 0 && lineWidth+terminal.Width(dashboardActionGap)+actionWidth > tuiContentWidth(model.width) {
			lines.WriteByte('\n')
			lineWidth = 0
		}
		if lineWidth > 0 {
			lines.WriteString(dashboardActionGap)
			lineWidth += terminal.Width(dashboardActionGap)
		}
		lines.WriteString(action)
		lineWidth += actionWidth
	}
	return lines.String()
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
	name := model.name
	if name == "" {
		name = "e.g. feature-a"
	}
	agent := model.selectedAgent()
	displayAgent := agentDisplayName(agent)
	if agent == "" {
		displayAgent = "No approved agents"
	}
	inherited := ""
	if agent == model.data.DefaultAgent {
		inherited = " — inherited from project"
	}
	check := "[ ]"
	if model.snapshot {
		check = "[x]"
	}
	body := formRow(theme, model.focus == 0, "Name", name) + "\n\n" +
		formRow(theme, false, "Starting point", boundedLine(model.data.Branch)+" @ "+boundedLine(model.data.Revision)) + "\n\n" +
		formRow(theme, model.focus == 1, "Default agent", displayAgent+inherited) + "\n\n" +
		formRow(theme, model.focus == 2, "Snapshot local changes", check+" Include reviewed final working-tree content") + "\n\n" +
		formChoice(theme, model.focus == 3, "Create and open") + "\n" +
		formChoice(theme, model.focus == 4, "Create in background")
	if model.notice != "" {
		body += "\n\n" + theme.warning.Render(boundedLine(model.notice))
	}
	body += "\n\n" + theme.help("[Tab] Next field", "[←/→] Select agent", "[Space] Toggle snapshot", "[Enter] Choose action", "[Esc] Cancel")
	return theme.panel("Create workspace", body, model.width, true)
}

func (model *DashboardModel) renderCreateSnapshotReview(theme visualTheme) string {
	body := theme.warning.Render("Create workspace from a source snapshot?") +
		"\n\nWorkspace\n  " + boundedLine(model.name) +
		"\n\nReal parent\n  " + boundedLine(model.data.Branch) + " @ " + boundedLine(model.data.Revision) +
		"\n\nIncludes final tracked file content and nonignored untracked files." +
		"\nIgnored untracked files stay on the host; tracked files remain included." +
		"\nUnmerged paths and Git submodules are rejected." +
		"\nYour branch, HEAD, index, worktree, and durable refs are not changed." +
		"\n\n" + theme.help("[y/Enter] Create snapshot workspace", "[n/Esc] Back")
	return theme.panel("Review source snapshot", body, model.width, true)
}

func (model *DashboardModel) renderAgent(theme visualTheme) string {
	workspace := model.selectedWorkspace()
	name := ""
	if workspace != nil {
		name = workspace.Name
	}
	check := "[ ]"
	if model.browser {
		check = "[x]"
	}
	body := theme.muted.Render("Workspace  "+boundedLine(name)) + "\n\n" + formRow(theme, model.focus == 0, "Agent", agentDisplayName(model.selectedAgent())) + "\n\n" + formRow(theme, model.focus == 1, "Browser", check+" Enable isolated browser for this session only") + "\n\n" + formChoice(theme, model.focus == 2, "Open agent") + "\n\n" + theme.help("[Tab] Next field", "[←/→] Select agent", "[Space] Toggle browser", "[Enter] Choose", "[Esc] Cancel")
	return theme.panel("Open agent", body, model.width, true)
}
func (model *DashboardModel) renderGit(theme visualTheme) string {
	workspace := model.selectedWorkspace()
	name := ""
	if workspace != nil {
		name = workspace.Name
	}
	return theme.panel("Review Git changes", "Workspace  "+boundedLine(name)+"\n\n"+theme.help("[s] Status", "[d] Diff", "[f] Fetch", "[a] Apply", "[Esc] Cancel"), model.width, true)
}
func (model *DashboardModel) renderRemove(theme visualTheme) string {
	workspace := model.selectedWorkspace()
	name := ""
	if workspace != nil {
		name = workspace.Name
	}
	body := theme.danger.Render("Remove workspace "+boundedLine(name)+"?") + "\n\nDSX will preserve unfetched or uncertain work unless loss is explicitly confirmed outside this dashboard.\n\n" + theme.help("[y] Remove", "[n/Esc] Cancel")
	return theme.panel("Confirm removal", body, model.width, true)
}
func (model *DashboardModel) renderAWS(theme visualTheme) string {
	workspace := model.selectedWorkspace()
	if workspace == nil {
		return theme.panel("AWS access", "No workspace selected.\n\n"+theme.help("[Esc] Cancel"), model.width, true)
	}
	host := awsAvailabilityLabel(workspace.AWSHostAvailability)
	if workspace.AWSHostAvailability != "available" {
		host += "\n  Start or renew one complete temporary [default] session in Leapp Desktop or a compatible provider, then try again."
	}
	if workspace.AWSEnabled {
		body := theme.danger.Render("Disable AWS for "+boundedLine(workspace.Name)+"?") +
			"\n\nHost default\n  " + host +
			"\n\nEffect\n  Access is revoked immediately for this workspace only. Its AWS mirror and helper are removed; other workspaces are unchanged." +
			"\n\n" + theme.help("[y/Enter] Disable", "[n/Esc] Cancel")
		return theme.panel("Confirm AWS revocation", body, model.width, true)
	}
	body := theme.warning.Render("Enable AWS for "+boundedLine(workspace.Name)+"?") +
		"\n\nHost default\n  " + host +
		"\n\nEffect\n  This workspace and its agents will continuously follow whichever AWS account and role the host provider assigns to default. Switching the host default changes authority without another approval or workspace restart. Named profiles are unavailable. Other workspaces are unchanged." +
		"\n\n" + theme.help("[y/Enter] Enable", "[n/Esc] Cancel")
	return theme.panel("Confirm dynamic AWS authority", body, model.width, true)
}

func formRow(theme visualTheme, active bool, label, value string) string {
	marker, style := "  ", theme.value
	if active {
		marker, style = "> ", theme.accent
	}
	return marker + theme.label.Render(label) + "\n    " + style.Render(boundedLine(value))
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
