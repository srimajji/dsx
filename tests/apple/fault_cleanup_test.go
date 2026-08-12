package apple_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/srimajji/dsx/internal/app"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
)

type workspaceFaultBoundary string

const (
	faultCreateNetwork   workspaceFaultBoundary = "create_network"
	faultCreateVolume    workspaceFaultBoundary = "create_volume"
	faultCreateWorkspace workspaceFaultBoundary = "create_workspace"
	faultStartWorkspace  workspaceFaultBoundary = "start_workspace"
	faultCopySource      workspaceFaultBoundary = "copy_source"
	faultBootstrap       workspaceFaultBoundary = "bootstrap"
)

var workspaceFaultBoundaries = []workspaceFaultBoundary{
	faultCreateNetwork, faultCreateVolume, faultCreateWorkspace,
	faultStartWorkspace, faultCopySource, faultBootstrap,
}

type workspaceFaultAdapter struct {
	runtime.Adapter
	boundary workspaceFaultBoundary
	injected error
	fired    bool
}

func (adapter *workspaceFaultAdapter) fail(boundary workspaceFaultBoundary) error {
	if adapter.boundary != boundary || adapter.fired {
		return nil
	}
	adapter.fired = true
	return adapter.injected
}

func (adapter *workspaceFaultAdapter) CreateNetwork(ctx context.Context, spec runtime.NetworkSpec) (runtime.Resource, error) {
	if err := adapter.fail(faultCreateNetwork); err != nil {
		return runtime.Resource{}, err
	}
	return adapter.Adapter.CreateNetwork(ctx, spec)
}

func (adapter *workspaceFaultAdapter) CreateVolume(ctx context.Context, spec runtime.VolumeSpec) (runtime.Resource, error) {
	if err := adapter.fail(faultCreateVolume); err != nil {
		return runtime.Resource{}, err
	}
	return adapter.Adapter.CreateVolume(ctx, spec)
}

func (adapter *workspaceFaultAdapter) CreateWorkspace(ctx context.Context, spec runtime.WorkspaceSpec) (runtime.Resource, error) {
	if err := adapter.fail(faultCreateWorkspace); err != nil {
		return runtime.Resource{}, err
	}
	return adapter.Adapter.CreateWorkspace(ctx, spec)
}

func (adapter *workspaceFaultAdapter) StartWorkspace(ctx context.Context, snapshot runtime.ResourceSnapshot) error {
	if err := adapter.fail(faultStartWorkspace); err != nil {
		return err
	}
	return adapter.Adapter.StartWorkspace(ctx, snapshot)
}

func (adapter *workspaceFaultAdapter) CopyTo(ctx context.Context, snapshot runtime.ResourceSnapshot, source runtime.HostPath, destination runtime.GuestPath) error {
	if err := adapter.fail(faultCopySource); err != nil {
		return err
	}
	return adapter.Adapter.CopyTo(ctx, snapshot, source, destination)
}

func (adapter *workspaceFaultAdapter) Exec(ctx context.Context, snapshot runtime.ResourceSnapshot, spec runtime.ExecSpec, streams runtime.ExecIO) (runtime.Exit, error) {
	if err := adapter.fail(faultBootstrap); err != nil {
		return runtime.Exit{}, err
	}
	return adapter.Adapter.Exec(ctx, snapshot, spec, streams)
}

func TestFaultCleanupNamedWorkspaceWriteAheadRollback(t *testing.T) {
	real := requireCoreRuntime(t)
	for _, boundary := range workspaceFaultBoundaries {
		t.Run(string(boundary), func(t *testing.T) {
			injected := errors.New("injected " + string(boundary) + " failure")
			fault := &workspaceFaultAdapter{Adapter: real.adapter, boundary: boundary, injected: injected}
			fixture := newWorkspaceFixture(t, real, fault, model.WorkspaceName("fault-"+workspaceNameSuffix(string(boundary))))
			defer fixture.recover()
			_, err := fixture.service.Create(context.Background(), app.WorkspaceCreateRequest{Root: fixture.root, Workspace: fixture.workspace, ApproveConfig: fixture.planHash})
			if !errors.Is(err, injected) || !fault.fired {
				t.Fatalf("Create() = %v, fault fired=%t", err, fault.fired)
			}
			fixture.assertRecovered()
		})
	}
}

func workspaceNameSuffix(value string) string {
	value = strings.ReplaceAll(value, "_", "-")
	if len(value) > 16 {
		value = value[:16]
	}
	return strings.Trim(value, "-")
}

var _ runtime.Adapter = (*workspaceFaultAdapter)(nil)
