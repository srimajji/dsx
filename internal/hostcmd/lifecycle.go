package hostcmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/terminal"
	"github.com/srimajji/dsx/internal/tui"
)

const workspaceHelp = `Usage:
  dsx workspace create NAME [--root PATH] [--default-agent AGENT] [--approve-config HASH] [--open]
  dsx workspace list [--root PATH] [--format text|json]
  dsx workspace open NAME [--root PATH]
  dsx workspace start NAME [--root PATH]
  dsx workspace stop NAME [--root PATH]
  dsx workspace restart NAME [--root PATH]
  dsx workspace update NAME [--root PATH]
  dsx workspace remove NAME [--root PATH] [--force]
  dsx workspace remove --all|--legacy-resources [--root PATH] [--force]
`

type AWSWorkspaceRequest = app.AWSWorkspaceRequest

type AWSWorkspaceResult = app.AWSWorkspaceResult

func (dispatcher *Dispatcher) executeWorkspace(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageError(stderr, "dsx workspace", "workspace requires create, list, open, start, stop, restart, update, or remove")
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		if len(args) != 1 {
			return usageError(stderr, "dsx workspace", "workspace help does not accept arguments")
		}
		if _, err := io.WriteString(stdout, workspaceHelp); err != nil {
			return reportError(stderr, "dsx workspace", model.Wrap(model.CodeInternal, "write help", err))
		}
		return 0
	}
	switch args[0] {
	case "create":
		return dispatcher.executeWorkspaceCreate(ctx, args[1:], stdout, stderr)
	case "list":
		return dispatcher.executeWorkspaceList(ctx, args[1:], stdout, stderr)
	case "open", "start", "stop", "restart", "update":
		return dispatcher.executeNamedWorkspace(ctx, args[0], args[1:], stdout, stderr)
	case "remove":
		return dispatcher.executeWorkspaceRemove(ctx, args[1:], stdout, stderr)
	default:
		return usageError(stderr, "dsx workspace", fmt.Sprintf("unknown workspace command %q", args[0]))
	}
}

func (dispatcher *Dispatcher) executeWorkspaceCreate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageError(stderr, "dsx workspace create", "create requires a workspace name")
	}
	workspace, err := model.ParseWorkspaceName(args[0])
	if err != nil {
		return usageError(stderr, "dsx workspace create", err.Error())
	}
	flags := newFlagSet("workspace create")
	root := flags.String("root", ".", "project root")
	defaultAgent := flags.String("default-agent", "", "workspace default agent")
	approval := flags.String("approve-config", "", "exact executable configuration hash")
	open := flags.Bool("open", false, "open the workspace after creation")
	if exit, done := parseFlags(flags, args[1:], stdout, stderr, workspaceHelp); done {
		return exit
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "dsx workspace create", "create does not accept extra arguments")
	}
	if err := validateRoot(*root); err != nil {
		return reportError(stderr, "dsx workspace create", err)
	}
	if *defaultAgent != "" {
		if _, err := harness.ParseName(*defaultAgent); err != nil {
			return usageError(stderr, "dsx workspace create", err.Error())
		}
	}
	if *approval != "" && !validApprovalHash(*approval) {
		return usageError(stderr, "dsx workspace create", "--approve-config must be exactly 64 lowercase hexadecimal characters")
	}
	if *open && !dispatcher.interactive(stdout) {
		return usageError(stderr, "dsx workspace create", "--open requires an interactive terminal")
	}
	if dispatcher == nil || dispatcher.dependencies.Workspaces == nil {
		return reportError(stderr, "dsx workspace create", model.NewError(model.CodeUnavailable, "workspace service is unavailable", nil))
	}
	result, err := dispatcher.dependencies.Workspaces.Create(ctx, app.WorkspaceCreateRequest{Root: *root, Workspace: workspace, DefaultAgent: *defaultAgent, ApproveConfig: *approval, Open: *open, Stdin: dispatcher.dependencies.Stdin, Stdout: stdout, Stderr: stderr, RunInteractive: dispatcher.runInteractive})
	if err != nil {
		return reportError(stderr, "dsx workspace create", err)
	}
	if err := renderWorkspaceResult(stdout, result); err != nil {
		return reportError(stderr, "dsx workspace create", err)
	}
	return 0
}

func (dispatcher *Dispatcher) executeWorkspaceList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("workspace list")
	root := flags.String("root", ".", "project root")
	format := flags.String("format", "text", "output format: text or json")
	if exit, done := parseFlags(flags, args, stdout, stderr, workspaceHelp); done {
		return exit
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "dsx workspace list", "list does not accept positional arguments")
	}
	if err := validateRoot(*root); err != nil {
		return reportError(stderr, "dsx workspace list", err)
	}
	if err := validateFormat(*format); err != nil {
		return reportError(stderr, "dsx workspace list", err)
	}
	if dispatcher == nil || dispatcher.dependencies.Workspaces == nil {
		return reportError(stderr, "dsx workspace list", model.NewError(model.CodeUnavailable, "workspace service is unavailable", nil))
	}
	result, err := dispatcher.dependencies.Workspaces.List(ctx, app.WorkspaceListRequest{Root: *root})
	if err != nil {
		return reportError(stderr, "dsx workspace list", err)
	}
	if err := renderWorkspaceList(stdout, result, *format); err != nil {
		return reportError(stderr, "dsx workspace list", err)
	}
	return 0
}

func (dispatcher *Dispatcher) executeNamedWorkspace(ctx context.Context, operation string, args []string, stdout, stderr io.Writer) int {
	command := "dsx workspace " + operation
	if len(args) == 0 {
		return usageError(stderr, command, operation+" requires a workspace name")
	}
	workspace, err := model.ParseWorkspaceName(args[0])
	if err != nil {
		return usageError(stderr, command, err.Error())
	}
	flags := newFlagSet("workspace " + operation)
	root := flags.String("root", ".", "project root")
	if exit, done := parseFlags(flags, args[1:], stdout, stderr, workspaceHelp); done {
		return exit
	}
	if flags.NArg() != 0 {
		return usageError(stderr, command, operation+" does not accept extra arguments")
	}
	if err := validateRoot(*root); err != nil {
		return reportError(stderr, command, err)
	}
	if operation == "update" {
		if dispatcher == nil || dispatcher.dependencies.Git == nil {
			return reportError(stderr, command, model.NewError(model.CodeUnavailable, "workspace Git service is unavailable", nil))
		}
		result, err := dispatcher.dependencies.Git.Update(ctx, app.WorkspaceUpdateRequest{Root: *root, Workspace: workspace})
		if err != nil {
			return reportError(stderr, command, err)
		}
		if err := renderWorkspaceResult(stdout, result); err != nil {
			return reportError(stderr, command, err)
		}
		return 0
	}
	if dispatcher == nil || dispatcher.dependencies.Workspaces == nil {
		return reportError(stderr, command, model.NewError(model.CodeUnavailable, "workspace service is unavailable", nil))
	}
	switch operation {
	case "open":
		if !dispatcher.interactive(stdout) {
			return usageError(stderr, command, "open requires an interactive terminal")
		}
		result, err := dispatcher.dependencies.Workspaces.Open(ctx, app.WorkspaceOpenRequest{Root: *root, Workspace: workspace, Terminal: true, Stdin: dispatcher.dependencies.Stdin, Stdout: stdout, Stderr: stderr, RunInteractive: dispatcher.runInteractive})
		if err != nil {
			return reportError(stderr, command, err)
		}
		if err := renderWorkspaceResult(stdout, result.WorkspaceResult); err != nil {
			return reportError(stderr, command, err)
		}
		exit, err := runtimeExitCode(result.Exit, "workspace shell")
		if err != nil {
			return reportError(stderr, command, err)
		}
		return exit
	case "start":
		result, err := dispatcher.dependencies.Workspaces.Start(ctx, app.WorkspaceStartRequest{Root: *root, Workspace: workspace})
		if err != nil {
			return reportError(stderr, command, err)
		}
		if err := renderWorkspaceResult(stdout, result); err != nil {
			return reportError(stderr, command, err)
		}
	case "stop":
		result, err := dispatcher.dependencies.Workspaces.Stop(ctx, app.WorkspaceStopRequest{Root: *root, Workspace: workspace})
		if err != nil {
			return reportError(stderr, command, err)
		}
		if err := renderWorkspaceResult(stdout, result); err != nil {
			return reportError(stderr, command, err)
		}
	case "restart":
		result, err := dispatcher.dependencies.Workspaces.Restart(ctx, app.WorkspaceRestartRequest{Root: *root, Workspace: workspace})
		if err != nil {
			return reportError(stderr, command, err)
		}
		if err := renderWorkspaceResult(stdout, result); err != nil {
			return reportError(stderr, command, err)
		}
	}
	return 0
}

func (dispatcher *Dispatcher) executeWorkspaceRemove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageError(stderr, "dsx workspace remove", "remove requires a workspace name or an explicit cleanup selector")
	}
	if !strings.HasPrefix(args[0], "-") {
		workspace, err := model.ParseWorkspaceName(args[0])
		if err != nil {
			return usageError(stderr, "dsx workspace remove", err.Error())
		}
		flags := newFlagSet("workspace remove")
		root := flags.String("root", ".", "project root")
		force := flags.Bool("force", false, "confirm destructive removal and loss of unfetched work")
		if exit, done := parseFlags(flags, args[1:], stdout, stderr, workspaceHelp); done {
			return exit
		}
		if flags.NArg() != 0 {
			return usageError(stderr, "dsx workspace remove", "remove does not accept extra arguments")
		}
		if err := validateRoot(*root); err != nil {
			return reportError(stderr, "dsx workspace remove", err)
		}
		confirmed, exit := dispatcher.confirmDestructive(fmt.Sprintf("Remove workspace %s? [y/N] ", workspace), *force, stdout, stderr)
		if !confirmed {
			return exit
		}
		return dispatcher.removeWorkspace(ctx, *root, workspace, false, *force, stdout, stderr)
	}
	flags := newFlagSet("workspace remove")
	root := flags.String("root", ".", "project root")
	all := flags.Bool("all", false, "remove all current-project workspaces")
	legacy := flags.Bool("legacy-resources", false, "remove proven current-project legacy resources")
	allProjects := flags.Bool("all-projects", false, "remove proven workspaces for all projects")
	force := flags.Bool("force", false, "confirm destructive removal and loss of unfetched work")
	if exit, done := parseFlags(flags, args, stdout, stderr, workspaceHelp); done {
		return exit
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "dsx workspace remove", "remove does not accept positional arguments with a cleanup selector")
	}
	selected := 0
	if *all {
		selected++
	}
	if *legacy {
		selected++
	}
	if *allProjects {
		selected++
	}
	if selected != 1 {
		return usageError(stderr, "dsx workspace remove", "select exactly one of --all, --legacy-resources, or --all-projects")
	}
	if *allProjects {
		if dispatcher == nil || dispatcher.dependencies.Workspaces == nil || dispatcher.dependencies.Inventory == nil {
			return reportError(stderr, "dsx workspace remove", model.NewError(model.CodeUnavailable, "all-project cleanup inventory is unavailable", nil))
		}
		manifests, err := dispatcher.dependencies.Inventory.ListAllManifests(ctx)
		if err != nil {
			return reportError(stderr, "dsx workspace remove", err)
		}
		sort.SliceStable(manifests, func(i, j int) bool {
			if manifests[i].CanonicalRoot != manifests[j].CanonicalRoot {
				return manifests[i].CanonicalRoot < manifests[j].CanonicalRoot
			}
			if manifests[i].Workspace != manifests[j].Workspace {
				return manifests[i].Workspace < manifests[j].Workspace
			}
			if manifests[i].Legacy != manifests[j].Legacy {
				return !manifests[i].Legacy
			}
			return manifests[i].RunID < manifests[j].RunID
		})
		confirmed, exit := dispatcher.confirmDestructive("Remove proven workspace resources for all projects? [y/N] ", *force, stdout, stderr)
		if !confirmed {
			return exit
		}
		seen := make(map[string]struct{}, len(manifests))
		for _, manifest := range manifests {
			if manifest.State == model.StateDeleted {
				continue
			}
			key := manifest.CanonicalRoot + "\x00" + string(manifest.Workspace) + fmt.Sprint("\x00", manifest.Legacy)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			if exit := dispatcher.removeWorkspace(ctx, manifest.CanonicalRoot, manifest.Workspace, manifest.Legacy, *force, stdout, stderr); exit != 0 {
				return exit
			}
		}
		return 0
	}
	if err := validateRoot(*root); err != nil {
		return reportError(stderr, "dsx workspace remove", err)
	}
	if dispatcher == nil || dispatcher.dependencies.Workspaces == nil {
		return reportError(stderr, "dsx workspace remove", model.NewError(model.CodeUnavailable, "workspace service is unavailable", nil))
	}
	listed, err := dispatcher.dependencies.Workspaces.List(ctx, app.WorkspaceListRequest{Root: *root})
	if err != nil {
		return reportError(stderr, "dsx workspace remove", err)
	}
	confirmed, exit := dispatcher.confirmDestructive("Remove the selected workspace resources? [y/N] ", *force, stdout, stderr)
	if !confirmed {
		return exit
	}
	for _, summary := range listed.Workspaces {
		if *legacy != summary.Legacy {
			continue
		}
		if exit := dispatcher.removeWorkspace(ctx, *root, summary.Workspace, summary.Legacy, *force, stdout, stderr); exit != 0 {
			return exit
		}
	}
	return 0
}

func (dispatcher *Dispatcher) removeWorkspace(ctx context.Context, root string, workspace model.WorkspaceName, legacy, discard bool, stdout, stderr io.Writer) int {
	if dispatcher == nil || dispatcher.dependencies.Workspaces == nil {
		return reportError(stderr, "dsx workspace remove", model.NewError(model.CodeUnavailable, "workspace service is unavailable", nil))
	}
	result, err := dispatcher.dependencies.Workspaces.Remove(ctx, app.WorkspaceRemoveRequest{Root: root, Workspace: workspace, Confirmed: true, DiscardUnfetched: discard, LegacyResources: legacy})
	if err != nil {
		return reportError(stderr, "dsx workspace remove", err)
	}
	if err := renderWorkspaceRemove(stdout, result); err != nil {
		return reportError(stderr, "dsx workspace remove", err)
	}
	return 0
}

func (dispatcher *Dispatcher) loadDashboard(ctx context.Context, root string) (tui.DashboardData, error) {
	if dispatcher.dependencies.Inspector == nil || dispatcher.dependencies.Workspaces == nil {
		return tui.DashboardData{}, model.NewError(model.CodeUnavailable, "dashboard services are unavailable", nil)
	}
	inspected, err := dispatcher.dependencies.Inspector.Inspect(ctx, app.InspectRequest{Root: root})
	if err != nil {
		return tui.DashboardData{}, err
	}
	listed, err := dispatcher.dependencies.Workspaces.List(ctx, app.WorkspaceListRequest{Root: root})
	if err != nil {
		return tui.DashboardData{}, err
	}
	data := tui.DashboardData{
		Root: inspected.Facts.CanonicalRoot, Branch: inspected.Facts.Branch,
		Revision: inspected.Facts.Revision, Clean: inspected.Facts.Clean,
		AllowedAgents: append([]string(nil), inspected.Plan.Agents.Allowed...),
		DefaultAgent:  inspected.Plan.Agents.Default,
	}
	data.AWSCapability = inspected.Plan.AWS.Mode
	for _, workspace := range listed.Workspaces {
		if workspace.Legacy {
			continue
		}
		entry := tui.DashboardWorkspace{Name: string(workspace.Workspace), State: string(workspace.State), DefaultAgent: workspace.DefaultAgent, MutationActive: workspace.MutationActive}
		if inspected.Plan.AWS.Mode == "host-default" {
			entry.AWSHostAvailability = app.AWSHostUnavailable
			entry.AWSMirrorHealth = app.AWSMirrorDegraded
			entry.AWSFailureCode = "status-unavailable"
			if dispatcher.dependencies.AWS != nil {
				status, statusErr := dispatcher.dependencies.AWS.Status(ctx, app.AWSWorkspaceRequest{Root: inspected.Facts.CanonicalRoot, Workspace: workspace.Workspace})
				if statusErr == nil {
					entry.AWSEnabled = status.Enabled
					entry.AWSHostAvailability = status.HostAvailability
					entry.AWSMirrorHealth = status.MirrorHealth
					entry.AWSFailureCode = status.FailureCode
				} else if ctxErr := ctx.Err(); ctxErr != nil {
					return tui.DashboardData{}, ctxErr
				}
			}
		}
		data.Workspaces = append(data.Workspaces, entry)
	}
	return data, nil
}

func (dispatcher *Dispatcher) executeTUIWorkspaceCreate(ctx context.Context, request tui.RunRequest, intent tui.Intent, stdout, stderr io.Writer) int {
	root := intent.Root
	if strings.TrimSpace(root) == "" {
		root = "."
	}
	workspace, err := model.ParseWorkspaceName(intent.Workspace)
	if err != nil {
		return usageError(stderr, "dsx workspace create", err.Error())
	}
	if dispatcher == nil || dispatcher.dependencies.TUI == nil || dispatcher.dependencies.Workspaces == nil {
		return reportError(stderr, "dsx workspace create", model.NewError(model.CodeUnavailable, "workspace creation is unavailable", nil))
	}
	progress := tui.ProgressRequest{
		Title:      "Creating workspace",
		Project:    root,
		Detail:     "DSX is creating the approved workspace and waiting until it is ready.",
		Accessible: request.Accessible,
		Steps: []tui.ProgressStep{
			{ID: "validate", Label: "Validate approved project plan"},
			{ID: "workspace", Label: "Create and start workspace"},
			{ID: "ready", Label: "Workspace ready"},
		},
	}
	err = dispatcher.dependencies.TUI.RunProgress(ctx, progress, func(operationCtx context.Context, report func(string)) error {
		report("validate")
		report("workspace")
		_, createErr := dispatcher.dependencies.Workspaces.Create(operationCtx, app.WorkspaceCreateRequest{
			Root: root, Workspace: workspace, SourceBranch: intent.SourceBranch, SourceRevision: intent.SourceRevision,
			DefaultAgent: intent.Agent, Open: false,
		})
		if createErr == nil {
			report("ready")
		}
		return createErr
	})
	if err != nil {
		return reportError(stderr, "dsx workspace create", err)
	}
	if !intent.Open {
		_, _ = io.WriteString(stdout, "Run dsx, select the running workspace, and press v to attach with VS Code.\n")
		return 0
	}
	opened, err := dispatcher.dependencies.Workspaces.Open(ctx, app.WorkspaceOpenRequest{
		Root: root, Workspace: workspace, Terminal: true, Stdin: dispatcher.dependencies.Stdin,
		Stdout: stdout, Stderr: stderr, RunInteractive: dispatcher.runInteractive,
	})
	if err != nil {
		return reportError(stderr, "dsx workspace open", err)
	}
	exit, err := runtimeExitCode(opened.Exit, "workspace shell")
	if err != nil {
		return reportError(stderr, "dsx workspace open", err)
	}
	if exit == 0 {
		_, _ = io.WriteString(stdout, "Run dsx, select the running workspace, and press v to attach with VS Code.\n")
	}
	return exit
}

type systemVSCodeLauncher struct{}

func NewVSCodeLauncher() VSCodeLauncher { return systemVSCodeLauncher{} }

func (systemVSCodeLauncher) OpenSettings(ctx context.Context) error {
	return exec.CommandContext(ctx, "/usr/bin/open", "vscode://settings/dev.containers.experimentalAppleContainerSupport").Run()
}

func vscodeAttachGuidance(container string) string {
	name := terminal.SanitizeLine(container)
	return fmt.Sprintf("VS Code Apple-container attachment is experimental. Verified with Dev Containers 0.467.0+ and Apple container 1.2.2.\\n1. Ensure extension ms-vscode-remote.remote-containers is installed.\\n2. Enable Dev › Containers: Experimental Apple Container Support.\\n3. Run Dev Containers: Attach to Running Apple Container...\\n4. Choose %q.\\n5. Open /workspace.\\n", name)
}

func (dispatcher *Dispatcher) executeIntent(ctx context.Context, intent tui.Intent, stdout, stderr io.Writer) int {
	root := intent.Root
	if strings.TrimSpace(root) == "" {
		root = "."
	}
	workspace, err := model.ParseWorkspaceName(intent.Workspace)
	if err != nil {
		return usageError(stderr, "dsx "+intent.Action, err.Error())
	}
	if intent.Action == "aws-enable" || intent.Action == "aws-disable" {
		if dispatcher.dependencies.AWS == nil {
			return reportError(stderr, "dsx "+intent.Action, model.NewError(model.CodeUnavailable, "AWS workspace service is unavailable", nil))
		}
		request := app.AWSWorkspaceRequest{Root: root, Workspace: workspace}
		var result app.AWSWorkspaceResult
		if intent.Action == "aws-enable" {
			result, err = dispatcher.dependencies.AWS.Enable(ctx, request)
		} else {
			result, err = dispatcher.dependencies.AWS.Disable(ctx, request)
		}
		if err != nil {
			return reportError(stderr, "dsx "+intent.Action, err)
		}
		if err := renderAWSWorkspaceResult(stdout, result, "text"); err != nil {
			return reportError(stderr, "dsx "+intent.Action, err)
		}
		return 0
	}
	if dispatcher.dependencies.Workspaces == nil {
		return reportError(stderr, "dsx "+intent.Action, model.NewError(model.CodeUnavailable, "workspace service is unavailable", nil))
	}
	switch intent.Action {
	case "workspace-create":
		result, err := dispatcher.dependencies.Workspaces.Create(ctx, app.WorkspaceCreateRequest{
			Root: root, Workspace: workspace, SourceBranch: intent.SourceBranch, SourceRevision: intent.SourceRevision,
			DefaultAgent: intent.Agent, Open: intent.Open, Stdin: dispatcher.dependencies.Stdin,
			Stdout: stdout, Stderr: stderr, RunInteractive: dispatcher.runInteractive,
		})
		if err != nil {
			return reportError(stderr, "dsx workspace create", err)
		}
		if err := renderWorkspaceResult(stdout, result); err != nil {
			return reportError(stderr, "dsx workspace create", err)
		}
	case "vscode-attach":
		info, infoErr := dispatcher.dependencies.Workspaces.AttachInfo(ctx, app.WorkspaceAttachInfoRequest{Root: root, Workspace: workspace})
		if infoErr != nil {
			return reportError(stderr, "dsx vscode-attach", infoErr)
		}
		launcher := dispatcher.dependencies.VSCode
		if launcher == nil {
			launcher = systemVSCodeLauncher{}
		}
		guidance := vscodeAttachGuidance(info.Container)
		if launchErr := launcher.OpenSettings(ctx); launchErr != nil {
			return reportError(stderr, "dsx vscode-attach", model.Wrap(model.CodeUnavailable, strings.TrimSpace(guidance), launchErr))
		}
		if _, writeErr := io.WriteString(stdout, guidance); writeErr != nil {
			return reportError(stderr, "dsx vscode-attach", writeErr)
		}
	case "workspace-open":
		return dispatcher.executeNamedWorkspace(ctx, "open", []string{string(workspace), "--root", root}, stdout, stderr)
	case "workspace-start":
		return dispatcher.executeNamedWorkspace(ctx, "start", []string{string(workspace), "--root", root}, stdout, stderr)
	case "workspace-stop":
		return dispatcher.executeNamedWorkspace(ctx, "stop", []string{string(workspace), "--root", root}, stdout, stderr)
	case "workspace-restart":
		return dispatcher.executeNamedWorkspace(ctx, "restart", []string{string(workspace), "--root", root}, stdout, stderr)
	case "workspace-update":
		return dispatcher.executeNamedWorkspace(ctx, "update", []string{string(workspace), "--root", root}, stdout, stderr)
	case "workspace-remove":
		return dispatcher.removeWorkspace(ctx, root, workspace, false, false, stdout, stderr)
	case "agent-run":
		if dispatcher.dependencies.Agents == nil {
			return reportError(stderr, "dsx agent", model.NewError(model.CodeUnavailable, "agent service is unavailable", nil))
		}
		result, err := dispatcher.dependencies.Agents.Run(ctx, app.AgentRunRequest{Root: root, Workspace: string(workspace), Agent: intent.Agent, Browser: intent.Browser, Stdin: dispatcher.dependencies.Stdin, Stdout: stdout, Stderr: stderr, RunInteractive: dispatcher.runInteractive, BeforeExec: func(result app.AgentRunResult) error {
			return renderAgentIdentity(stdout, result.Agent, result.Version)
		}})
		if err != nil {
			return reportError(stderr, "dsx agent", err)
		}
		exit, err := runtimeExitCode(result.Exit, "agent")
		if err != nil {
			return reportError(stderr, "dsx agent", err)
		}
		return exit
	case "git-status":
		return dispatcher.executeGit(ctx, []string{"status", string(workspace), "--root", root}, stdout, stderr)
	case "git-diff":
		return dispatcher.executeGit(ctx, []string{"diff", string(workspace), "--root", root}, stdout, stderr)
	case "git-fetch":
		return dispatcher.executeGit(ctx, []string{"fetch", string(workspace), "--root", root}, stdout, stderr)
	case "git-apply":
		return dispatcher.executeGit(ctx, []string{"apply", string(workspace), "--root", root}, stdout, stderr)
	default:
		return usageError(stderr, "dsx", fmt.Sprintf("unknown terminal action %q", intent.Action))
	}
	return 0
}

func renderAWSWorkspaceResult(writer io.Writer, result AWSWorkspaceResult, format string) error {
	if format == "json" {
		return encodeJSON(writer, result)
	}
	if _, err := fmt.Fprintf(
		writer,
		"Workspace: %q\nEnabled: %t\nHost availability: %q\nMirror health: %q\nFailure code: %q\n",
		terminal.SanitizeLine(string(result.Workspace)),
		result.Enabled,
		terminal.SanitizeLine(result.HostAvailability),
		terminal.SanitizeLine(result.MirrorHealth),
		terminal.SanitizeLine(result.FailureCode),
	); err != nil {
		return model.Wrap(model.CodeInternal, "write AWS workspace result", err)
	}
	return nil
}

func renderWorkspaceResult(writer io.Writer, result app.WorkspaceResult) error {
	if _, err := fmt.Fprintf(writer, "Project: %q\nWorkspace: %q\nRun: %q\nState: %q\nExisting: %t\n", terminal.SanitizeLine(string(result.ProjectID)), terminal.SanitizeLine(string(result.Workspace)), terminal.SanitizeLine(string(result.RunID)), terminal.SanitizeLine(string(result.State)), result.Existing); err != nil {
		return model.Wrap(model.CodeInternal, "write workspace result", err)
	}
	warnings := append([]string(nil), result.Warnings...)
	sort.Strings(warnings)
	for _, warning := range warnings {
		if _, err := fmt.Fprintf(writer, "Warning: %q\n", terminal.SanitizeLine(warning)); err != nil {
			return model.Wrap(model.CodeInternal, "write workspace warning", err)
		}
	}
	urls := append([]string(nil), result.URLs...)
	sort.Strings(urls)
	for _, url := range urls {
		if _, err := fmt.Fprintf(writer, "URL: %q\n", terminal.SanitizeLine(url)); err != nil {
			return model.Wrap(model.CodeInternal, "write workspace URL", err)
		}
	}
	return nil
}

func renderWorkspaceList(writer io.Writer, result app.WorkspaceListResult, format string) error {
	workspaces := append([]app.WorkspaceSummary(nil), result.Workspaces...)
	for i := range workspaces {
		workspaces[i].Warnings = append([]string(nil), workspaces[i].Warnings...)
		sort.Strings(workspaces[i].Warnings)
		workspaces[i].URLs = append([]string(nil), workspaces[i].URLs...)
		sort.Strings(workspaces[i].URLs)
	}
	sort.SliceStable(workspaces, func(i, j int) bool {
		if workspaces[i].Workspace != workspaces[j].Workspace {
			return workspaces[i].Workspace < workspaces[j].Workspace
		}
		return workspaces[i].RunID < workspaces[j].RunID
	})
	if format == "json" {
		result.Workspaces = workspaces
		return encodeJSON(writer, result)
	}
	if len(workspaces) == 0 {
		_, err := io.WriteString(writer, "No workspaces.\n")
		return err
	}
	for _, workspace := range workspaces {
		if _, err := fmt.Fprintf(writer, "Workspace %q: state=%q run=%q source=%q@%q default_agent=%q resources=%d mutating=%t legacy=%t\n", terminal.SanitizeLine(string(workspace.Workspace)), terminal.SanitizeLine(string(workspace.State)), terminal.SanitizeLine(string(workspace.RunID)), terminal.SanitizeLine(workspace.SourceBranch), terminal.SanitizeLine(workspace.SourceRevision), terminal.SanitizeLine(workspace.DefaultAgent), workspace.Resources, workspace.MutationActive, workspace.Legacy); err != nil {
			return model.Wrap(model.CodeInternal, "write workspace list", err)
		}
		for _, warning := range workspace.Warnings {
			if _, err := fmt.Fprintf(writer, "  Warning: %q\n", terminal.SanitizeLine(warning)); err != nil {
				return model.Wrap(model.CodeInternal, "write workspace list", err)
			}
		}
		for _, url := range workspace.URLs {
			if _, err := fmt.Fprintf(writer, "  URL: %q\n", terminal.SanitizeLine(url)); err != nil {
				return model.Wrap(model.CodeInternal, "write workspace list", err)
			}
		}
	}
	return nil
}

func renderWorkspaceRemove(writer io.Writer, result app.WorkspaceRemoveResult) error {
	if _, err := fmt.Fprintf(writer, "Project: %q\nWorkspace: %q\nRun: %q\nState: %q\nDeleted resources: %d\nDeleted manifest: %t\n", terminal.SanitizeLine(string(result.ProjectID)), terminal.SanitizeLine(string(result.Workspace)), terminal.SanitizeLine(string(result.RunID)), terminal.SanitizeLine(string(result.State)), result.DeletedResources, result.DeletedManifest); err != nil {
		return model.Wrap(model.CodeInternal, "write workspace removal", err)
	}
	preserved := append([]string(nil), result.Preserved...)
	sort.Strings(preserved)
	for _, item := range preserved {
		if _, err := fmt.Fprintf(writer, "Preserved: %q\n", terminal.SanitizeLine(item)); err != nil {
			return model.Wrap(model.CodeInternal, "write workspace removal", err)
		}
	}
	return nil
}

func claimInteractiveSignals(ctx context.Context) (context.Context, <-chan os.Signal, func()) {
	if claimedCtx, signals, claimed := terminal.ClaimInteractiveSignalOwnership(ctx); claimed {
		return claimedCtx, signals, func() {}
	}
	signals, stopSignals := terminal.WatchSignals()
	return ctx, signals, stopSignals
}

func (dispatcher *Dispatcher) runInteractive(ctx context.Context, child app.InteractiveChild) (runtime.Exit, error) {
	if len(child.Argv) == 0 || child.Argv[0] == "" {
		return runtime.Exit{}, model.NewError(model.CodeInternal, "interactive child executable is missing", nil)
	}
	command := exec.Command(child.Argv[0], child.Argv[1:]...)
	command.Env = append([]string(nil), child.Env...)
	command.Dir = child.Dir
	output := child.Stdout
	if output == nil {
		output = child.Stderr
	}
	initial, resize, stopResize := terminal.WatchResize(child.Stdin, output)
	defer stopResize()
	handoffCtx, signals, stopSignals := claimInteractiveSignals(ctx)
	defer stopSignals()
	exit, err := (terminal.Handoff{Input: child.Stdin, Output: output, State: dispatcher.dependencies.TerminalState, InitialSize: initial, Resize: resize, Signals: signals}).Run(handoffCtx, command)
	result := runtime.Exit{}
	if exit.Signal != 0 {
		result.Signal = signalName(exit.Signal)
	} else if exit.ExitCode >= 0 {
		code := exit.ExitCode
		result.Code = &code
	}
	return result, err
}

func validApprovalHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	for i := range value {
		if (value[i] < '0' || value[i] > '9') && (value[i] < 'a' || value[i] > 'f') {
			return false
		}
	}
	return true
}

func validateRoot(root string) error {
	if strings.TrimSpace(root) == "" {
		return model.NewError(model.CodeUsage, "--root must not be empty", nil)
	}
	return nil
}

func argumentsFollowSeparator(args, positional []string) bool {
	separator := len(args) - len(positional) - 1
	if separator < 0 || separator >= len(args) || args[separator] != "--" {
		return false
	}
	for i := range positional {
		if positional[i] != args[separator+1+i] {
			return false
		}
	}
	return true
}

var interactiveSignals = map[string]syscall.Signal{"SIGHUP": syscall.SIGHUP, "SIGINT": syscall.SIGINT, "SIGQUIT": syscall.SIGQUIT, "SIGILL": syscall.SIGILL, "SIGTRAP": syscall.SIGTRAP, "SIGABRT": syscall.SIGABRT, "SIGBUS": syscall.SIGBUS, "SIGFPE": syscall.SIGFPE, "SIGKILL": syscall.SIGKILL, "SIGUSR1": syscall.SIGUSR1, "SIGSEGV": syscall.SIGSEGV, "SIGUSR2": syscall.SIGUSR2, "SIGPIPE": syscall.SIGPIPE, "SIGALRM": syscall.SIGALRM, "SIGTERM": syscall.SIGTERM, "SIGCHLD": syscall.SIGCHLD, "SIGCONT": syscall.SIGCONT, "SIGSTOP": syscall.SIGSTOP, "SIGTSTP": syscall.SIGTSTP, "SIGTTIN": syscall.SIGTTIN, "SIGTTOU": syscall.SIGTTOU, "SIGURG": syscall.SIGURG, "SIGXCPU": syscall.SIGXCPU, "SIGXFSZ": syscall.SIGXFSZ, "SIGVTALRM": syscall.SIGVTALRM, "SIGPROF": syscall.SIGPROF, "SIGWINCH": syscall.SIGWINCH, "SIGIO": syscall.SIGIO, "SIGSYS": syscall.SIGSYS}

func signalName(value syscall.Signal) string {
	for name, candidate := range interactiveSignals {
		if candidate == value {
			return name
		}
	}
	return fmt.Sprintf("SIG%d", value)
}
