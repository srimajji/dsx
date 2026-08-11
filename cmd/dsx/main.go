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
	var containerExecutable string
	executable, containerRuntimeErr := apple.DiscoverContainerExecutable()
	if containerRuntimeErr == nil {
		runtimeRunner := apple.OSRunner{}
		adapter, containerRuntimeErr = apple.NewAdapter(runtimeRunner, executable)
		if containerRuntimeErr == nil {
			containerExecutable = executable
		}
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
	var lifecycle *app.LifecycleService
	var harnesses *app.HarnessService
	var clones app.CloneManager
	if stateRoot, err := dsxStateRoot(); err == nil {
		if approvals, approvalErr := statefs.NewApprovalRepository(stateRoot); approvalErr == nil {
			var manifests *statefs.ManifestRepository
			var inventory app.OwnedResourceInventory
			if repository, manifestErr := statefs.NewManifestRepository(stateRoot); manifestErr == nil {
				manifests = repository
				inventory = repository
			}
			var bridgeLeases bridge.LeaseManager
			var leappMirrors bridge.LeappMirrorManager
			if executable, executableErr := os.Executable(); executableErr == nil {
				bridgeLeases, _ = bridge.NewProductionLeaseManagerWithContainer(stateRoot, executable, containerExecutable)
				leappMirrors, _ = bridge.NewProductionLeappMirrorManager(stateRoot, executable)
			}
			if manifests != nil && adapter != nil {
				authRepository, authErr := authrepo.NewRepository(filepath.Join(stateRoot, "auth"))
				cleanSandboxAuth := func(ctx context.Context, projectID model.ProjectID, sandbox model.SandboxName) error {
					if authErr != nil {
						return authErr
					}
					err := authRepository.PurgeCleanedSandbox(ctx, projectID, string(sandbox))
					return err
				}
				lifecycle = app.NewLifecycleService(app.LifecycleDependencies{
					Inspection:        inspection,
					Approvals:         approvals,
					Manifests:         manifests,
					Locks:             manifests,
					Runtime:           adapter,
					Guest:             app.NewGuestClient(adapter),
					GuestHelperSource: func() (runtime.HostPath, error) { return installedGuestHelper(stateRoot) },
					CleanSandboxAuth:  cleanSandboxAuth,
					BridgeLeases:      bridgeLeases,
					LeappMirrors:      leappMirrors,
				})
				if authErr == nil {
					harnesses, _ = app.NewHarnessService(lifecycle, authRepository, catalog.All()...)
				}
				if harnesses != nil {
					gitExecutable, gitPathErr := canonicalGitExecutable()
					if gitPathErr == nil {
						if gitService, gitErr := gitx.NewService(gitx.OSRunner{}, gitExecutable); gitErr == nil {
							clones, _ = app.NewCloneService(app.CloneDependencies{
								Lifecycle: lifecycle,
								Harness:   harnesses,
								Git:       gitService,
								TempRoot:  os.TempDir(),
							})
						}
					}
				}
			}
			setup = app.NewSetupServiceWithDependencies(app.SetupDependencies{
				Inspection: inspection, Approvals: approvals, Inventory: inventory,
				ContainerSystem: containerSystem, ImagePreparer: lifecycle,
			})
		}
	}
	runner := &tui.Runner{Application: setup, Input: os.Stdin, Output: os.Stdout}
	loginBrowser, err := productionLoginBrowserOpener()
	if err != nil {
		panic(fmt.Sprintf("initialize host login opener: %v", err))
	}
	dispatcher := hostcmd.NewDispatcher(hostcmd.Dependencies{
		Inspector:     inspection,
		Doctor:        doctor,
		Lifecycle:     lifecycle,
		Harness:       harnesses,
		Clones:        clones,
		TUI:           runner,
		Stdin:         os.Stdin,
		TerminalState: terminal.NewRawState(os.Stdin),
		LoginBrowser:  loginBrowser,
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

func productionLoginBrowserOpener() (app.LoginBrowserOpener, error) {
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
