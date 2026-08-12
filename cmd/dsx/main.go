package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/srimajji/dsx/internal/app"
	authrepo "github.com/srimajji/dsx/internal/auth"
	"github.com/srimajji/dsx/internal/bridge"
	"github.com/srimajji/dsx/internal/buildinfo"
	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/harness/catalog"
	"github.com/srimajji/dsx/internal/hostcmd"
	"github.com/srimajji/dsx/internal/hostopen"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/runtime/apple"
	statefs "github.com/srimajji/dsx/internal/state/fs"
	"github.com/srimajji/dsx/internal/terminal"
	"github.com/srimajji/dsx/internal/tui"
)

func main() {
	if len(os.Args) >= 2 && (os.Args[1] == "__bridge-helper" || os.Args[1] == "__bridge-control" || os.Args[1] == "__dsx_leapp_mirror_v1") {
		if len(os.Args) != 2 {
			os.Exit(2)
		}
		switch os.Args[1] {
		case "__bridge-helper":
			os.Exit(bridge.RunLeaseHelper())
		case "__bridge-control":
			os.Exit(bridge.RunLeaseControlClient())
		default:
			os.Exit(bridge.RunLeappMirrorCommand())
		}
	}
	inspectionDependencies := app.InspectionDependencies{Resolver: plan.NewResolver()}
	if configRoot, err := dsxConfigRoot(); err == nil {
		inspectionDependencies.ConfigRoot = configRoot
	}
	inspection := app.NewInspectionServiceWithDependencies(inspectionDependencies)
	var adapter *apple.Adapter
	executable, containerRuntimeErr := apple.DiscoverContainerExecutable()
	if containerRuntimeErr == nil {
		runtimeRunner := apple.OSRunner{}
		adapter, containerRuntimeErr = apple.NewAdapter(runtimeRunner, executable)
	}
	var containerSystem app.ContainerSystemController = unavailableContainerSystem{cause: containerRuntimeErr}
	if adapter != nil {
		containerSystem = adapter
	}
	var doctor *app.DoctorService
	if adapter != nil {
		doctor = app.NewDoctorService(adapter)
	}

	var setup *app.SetupService
	var workspaces *app.WorkspaceService
	var authentication *app.AuthService
	var agents *app.AgentService
	var workspaceGit app.WorkspaceGitManager
	var workspaceInventory hostcmd.WorkspaceInventory
	if stateRoot, err := dsxStateRoot(); err == nil {
		if approvals, approvalErr := statefs.NewApprovalRepository(stateRoot); approvalErr == nil {
			var manifests *statefs.ManifestRepository
			var inventory app.OwnedResourceInventory
			if repository, manifestErr := statefs.NewManifestRepository(stateRoot); manifestErr == nil {
				manifests = repository
				inventory = repository
				workspaceInventory = repository
			}
			if manifests != nil && adapter != nil {
				gitExecutable, gitPathErr := canonicalGitExecutable()
				if gitPathErr == nil {
					if gitService, gitErr := gitx.NewService(gitx.OSRunner{}, gitExecutable); gitErr == nil {
						workspaces = app.NewWorkspaceService(app.WorkspaceDependencies{
							Inspection:        inspection,
							Approvals:         approvals,
							Manifests:         manifests,
							Locks:             manifests,
							Runtime:           adapter,
							Git:               gitService,
							TempRoot:          filepath.Clean(os.TempDir()),
							GuestHelperSource: func() (runtime.HostPath, error) { return installedGuestHelper(stateRoot) },
						})
						workspaceGit = workspaces
					}
				}
				if workspaces != nil {
					if authRepository, authErr := authrepo.NewRepository(filepath.Join(stateRoot, "auth")); authErr == nil {
						if home, homeErr := os.UserHomeDir(); homeErr == nil {
							if discovery, discoveryErr := authrepo.NewHostDiscovery(home); discoveryErr == nil {
								if authRunner, runnerErr := app.NewRuntimeAuthSessionRunner(workspaces, authRepository, catalog.All()...); runnerErr == nil {
									authentication, _ = app.NewAuthService(authRepository, discovery, authRunner, catalog.All()...)
								}
							}
						}
					}
				}
				if workspaces != nil && authentication != nil {
					agents, _ = app.NewAgentService(workspaces, authentication, catalog.All()...)
				}
			}
			setup = app.NewSetupServiceWithDependencies(app.SetupDependencies{
				Inspection: inspection, Approvals: approvals, Inventory: inventory,
				ContainerSystem: containerSystem, ImagePreparer: workspaces,
			})
		}
	}
	runner := &tui.Runner{Application: setup, Input: os.Stdin, Output: os.Stdout}
	dispatcher := hostcmd.NewDispatcher(hostcmd.Dependencies{
		Inspector:     inspection,
		Doctor:        doctor,
		Workspaces:    workspaces,
		Git:           workspaceGit,
		Agents:        agents,
		Auth:          authentication,
		Inventory:     workspaceInventory,
		TUI:           runner,
		Stdin:         os.Stdin,
		TerminalState: terminal.NewRawState(os.Stdin),
		Accessible:    os.Getenv("DSX_ACCESSIBLE") == "1",
	})
	ctx, stopSignals := terminal.CommandSignalContext(context.Background())
	exit := dispatcher.Execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stopSignals()
	os.Exit(exit)
}

type unavailableContainerSystem struct {
	cause error
}

func (system unavailableContainerSystem) CheckSystemStatus(context.Context) error {
	return model.Wrap(
		model.CodeUnavailable,
		"Apple container runtime is not installed at a supported path; install container 1.2.2 and retry",
		system.cause,
	)
}
func (system unavailableContainerSystem) Status(context.Context) (runtime.SystemStatus, error) {
	return runtime.SystemStatus{
		State: runtime.SystemStateNotInstalled, Remediation: "Install Apple Container 1.2.2 to continue.",
	}, nil
}

func (system unavailableContainerSystem) StartSystem(context.Context) error {
	return model.Wrap(
		model.CodeUnavailable,
		"Apple container runtime is not installed at a supported path; install container 1.2.2 and retry",
		system.cause,
	)
}

func productionLoginBrowserOpener() (func(context.Context, string) error, error) {
	opener, err := hostopen.New(hostopen.OSRunner{})
	if err != nil {
		return nil, err
	}
	return opener.Open, nil
}

func canonicalGitExecutable() (string, error) {
	discovered, err := exec.LookPath("git")
	if err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(discovered)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(canonical) || filepath.Clean(canonical) != canonical {
		return "", fmt.Errorf("resolved git executable is not canonical: %q", canonical)
	}
	return canonical, nil
}

func dsxConfigRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".dsx"), nil
}

func dsxStateRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "DSX", "v1"), nil
}

func installedGuestHelper(stateRoot string) (runtime.HostPath, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}
	helper := filepath.Join(filepath.Dir(executable), "dsx-guest")
	helper, err = filepath.EvalSymlinks(helper)
	if err != nil {
		return "", fmt.Errorf("find dsx-guest beside dsx: %w", err)
	}
	if buildinfo.Version == "dev" && buildinfo.GuestSHA256 == "unknown" {
		return app.StageGuestHelper(runtime.HostPath(helper), filepath.Join(stateRoot, "guest-helper-cache"))
	}
	return app.StageVerifiedGuestHelper(runtime.HostPath(helper), filepath.Join(stateRoot, "guest-helper-cache"), buildinfo.GuestSHA256)
}
