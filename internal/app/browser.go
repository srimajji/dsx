package app

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/ownership"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
)

const (
	browserRole           = "browser"
	browserMCPName        = "playwright"
	browserMCPPort        = 8931
	browserMCPPath        = "/mcp"
	browserReadyTimeout   = 30 * time.Second
	browserReadyPoll      = 250 * time.Millisecond
	browserCleanupTimeout = 30 * time.Second
)

var browserEntrypoint = []string{"node", "/app/entrypoint.mjs"}

type browserSession struct {
	Root      string
	Workspace model.WorkspaceName
	Record    state.ResourceRecord
	Server    harness.MCPServer
}

func (service *AgentService) createBrowserSession(ctx context.Context, access workspaceAccess) (session *browserSession, returnErr error) {
	if access.Plan.Browser == nil || access.Plan.Browser.ImageReference == "" || access.Plan.Browser.ImageDigest == "" {
		return nil, model.NewError(model.CodeUnapproved, "browser is not approved with a pinned image", nil)
	}
	identity, err := ownership.NewIdentity(
		access.Manifest.ProjectID, access.Manifest.CanonicalRoot, access.Manifest.Workspace,
		access.Manifest.RunID, runtime.ResourceBrowser, browserRole,
	)
	if err != nil {
		return nil, model.Wrap(model.CodeInternal, "prepare browser ownership", err)
	}
	recordIndex, found := manifestResourceIndex(access.Manifest.Resources, runtime.ResourceBrowser)
	if found {
		record := access.Manifest.Resources[recordIndex]
		if record.Created && !record.Deleted {
			return nil, model.NewError(model.CodeConflict, "a prior browser session still requires cleanup", nil)
		}
		access.Manifest.Resources[recordIndex] = identity.ManifestRecord()
	} else {
		recordIndex = len(access.Manifest.Resources)
		access.Manifest.Resources = append(access.Manifest.Resources, identity.ManifestRecord())
	}
	if err := service.workspaces.replaceManifest(ctx, access.Manifest); err != nil {
		return nil, model.Wrap(model.CodeUnavailable, "persist browser creation intent", err)
	}
	record := access.Manifest.Resources[recordIndex]
	cleanupNeeded := true
	defer func() {
		if !cleanupNeeded {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), browserCleanupTimeout)
		defer cancel()
		returnErr = errors.Join(returnErr, service.deleteBrowserWithAccess(cleanupCtx, access, recordIndex))
	}()

	image, err := service.workspaces.runtime.EnsureImage(ctx, runtime.ImageSpec{Reference: access.Plan.Browser.ImageReference})
	if err != nil {
		return nil, model.Wrap(model.CodeUnavailable, "ensure pinned browser image", err)
	}
	if image.Digest != "sha256:"+access.Plan.Browser.ImageDigest {
		return nil, model.NewError(model.CodeUnavailable, "runtime returned an unexpected browser image digest", nil)
	}
	spec := runtime.BrowserSpec{
		Name: record.Name, Image: image, Entrypoint: append([]string(nil), browserEntrypoint...),
		Networks: []string{access.Network.Name}, Labels: identity.Labels(),
		CPUs: access.Plan.Limits.CPUs, MemoryBytes: access.Plan.Limits.MemoryBytes,
	}
	created, err := service.workspaces.runtime.CreateBrowser(ctx, spec)
	if err != nil {
		return nil, model.Wrap(model.CodeUnavailable, "create disposable browser", err)
	}
	if created.ID != runtime.ResourceID(record.ExpectedID) || created.Name != record.Name || created.Kind != runtime.ResourceBrowser {
		return nil, model.NewError(model.CodeAmbiguous, "runtime returned a different browser resource identity", nil)
	}
	record = access.Manifest.Resources[recordIndex]
	record.Created = true
	record.RuntimeID = string(created.ID)
	record.Absent = false
	record.Deleted = false
	access.Manifest.Resources[recordIndex] = record
	if err := service.workspaces.replaceManifest(ctx, access.Manifest); err != nil {
		return nil, model.Wrap(model.CodeUnavailable, "persist created browser", err)
	}

	browser, err := service.workspaces.runtime.Inspect(ctx, created.ID)
	if err != nil {
		return nil, model.Wrap(model.CodeUnavailable, "inspect created browser", err)
	}
	if err := verifyBrowserSnapshot(record, browser, access.Network.Name, access.Plan.Browser.ImageDigest, false); err != nil {
		return nil, err
	}
	if err := service.workspaces.runtime.StartWorkspace(ctx, browser); err != nil {
		return nil, model.Wrap(model.CodeUnavailable, "start disposable browser", err)
	}
	browser, err = service.workspaces.runtime.Inspect(ctx, created.ID)
	if err != nil {
		return nil, model.Wrap(model.CodeUnavailable, "inspect started browser", err)
	}
	if err := verifyBrowserSnapshot(record, browser, access.Network.Name, access.Plan.Browser.ImageDigest, true); err != nil {
		return nil, err
	}
	address, err := browserNetworkIPv4(browser, access.Network.Name)
	if err != nil {
		return nil, err
	}
	server := harness.MCPServer{Name: browserMCPName, URL: fmt.Sprintf("http://%s:%d%s", address, browserMCPPort, browserMCPPath)}
	readyCtx, cancel := context.WithTimeout(ctx, browserReadyTimeout)
	defer cancel()
	if err := service.waitBrowserReady(readyCtx, access, record, server.URL); err != nil {
		return nil, err
	}
	cleanupNeeded = false
	return &browserSession{Root: access.Manifest.CanonicalRoot, Workspace: access.Manifest.Workspace, Record: record, Server: server}, nil
}

func verifyBrowserSnapshot(record state.ResourceRecord, snapshot runtime.ResourceSnapshot, network, imageDigest string, requireRunning bool) error {
	classification := ownership.Classify(&record, &snapshot)
	if !classification.AdoptAllowed {
		return model.NewError(model.CodeAmbiguous, classification.Reason, nil)
	}
	if snapshot.Kind != runtime.ResourceBrowser || len(snapshot.Mounts) != 0 || len(snapshot.Ports) != 0 || len(snapshot.Networks) != 1 || snapshot.Networks[0] != network {
		return model.NewError(model.CodeUnavailable, "browser runtime violated its isolated network-only contract", nil)
	}
	if snapshot.ImageDigest != "sha256:"+imageDigest {
		return model.NewError(model.CodeUnavailable, "browser runtime inspected an unexpected image digest", nil)
	}
	if requireRunning && snapshot.State != "running" {
		return model.NewError(model.CodeUnavailable, "browser exited before readiness", nil)
	}
	return nil
}

func browserNetworkIPv4(snapshot runtime.ResourceSnapshot, network string) (netip.Addr, error) {
	addresses := snapshot.NetworkAddresses[network]
	var selected netip.Addr
	for _, address := range addresses {
		address = address.Unmap()
		if !address.IsValid() || !address.Is4() || !address.IsPrivate() {
			continue
		}
		if selected.IsValid() && selected != address {
			return netip.Addr{}, model.NewError(model.CodeAmbiguous, "browser has multiple private IPv4 addresses on its workspace network", nil)
		}
		selected = address
	}
	if !selected.IsValid() {
		return netip.Addr{}, model.NewError(model.CodeUnavailable, "browser has no inspected private IPv4 address on its workspace network", nil)
	}
	return selected, nil
}

func (service *AgentService) waitBrowserReady(ctx context.Context, access workspaceAccess, record state.ResourceRecord, url string) error {
	workspaceIndex, found := manifestResourceIndex(access.Manifest.Resources, runtime.ResourceWorkspace)
	if !found {
		return model.NewError(model.CodeInternal, "workspace ownership record is missing", nil)
	}
	workspaceRecord := access.Manifest.Resources[workspaceIndex]
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return model.Wrap(model.CodeUnavailable, "browser MCP readiness timed out", ctx.Err())
		case <-timer.C:
		}
		owner, err := service.workspaces.runtime.Inspect(ctx, runtime.ResourceID(workspaceRecord.ExpectedID))
		if err != nil {
			return model.Wrap(model.CodeUnavailable, "inspect browser workspace during readiness", err)
		}
		classification := ownership.Classify(&workspaceRecord, &owner)
		if !classification.AdoptAllowed || owner.State != "running" {
			return model.NewError(model.CodeUnavailable, "browser workspace exited before readiness", nil)
		}
		browser, err := service.workspaces.runtime.Inspect(ctx, runtime.ResourceID(record.ExpectedID))
		if err != nil {
			if errors.Is(err, runtime.ErrResourceNotFound) {
				return model.NewError(model.CodeUnavailable, "browser disappeared before readiness", nil)
			}
			return model.Wrap(model.CodeUnavailable, "inspect browser readiness", err)
		}
		if err := verifyBrowserSnapshot(record, browser, access.Network.Name, access.Plan.Browser.ImageDigest, true); err != nil {
			return err
		}
		exit, probeErr := service.shell(ctx, owner, []string{
			"/usr/bin/curl", "--silent", "--show-error", "--output", "/dev/null", "--max-time", "2", url,
		}, nil, false, nil, nil, nil, nil)
		if probeErr == nil && successfulGuestCommand(exit) {
			return nil
		}
		timer.Reset(browserReadyPoll)
	}
}

func (service *AgentService) deleteBrowserSession(ctx context.Context, session *browserSession) (returnErr error) {
	if session == nil {
		return nil
	}
	projectID, err := model.NewProjectID(session.Root)
	if err != nil {
		return model.Wrap(model.CodeInternal, "recover browser project identity", err)
	}
	_, _, unlock, err := service.workspaces.lockWorkspaceProject(ctx, projectID, session.Workspace)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, unlock()) }()
	manifest, err := service.workspaces.oneWorkspaceManifest(ctx, projectID, session.Workspace, false)
	if err != nil {
		browser, inspectErr := service.workspaces.runtime.Inspect(ctx, runtime.ResourceID(session.Record.ExpectedID))
		if errors.Is(inspectErr, runtime.ErrResourceNotFound) {
			return nil
		}
		if inspectErr != nil {
			return model.Wrap(model.CodeUnavailable, "inspect browser after workspace removal", inspectErr)
		}
		classification := ownership.Classify(&session.Record, &browser)
		if !classification.DeleteAllowed {
			return model.NewError(model.CodeAmbiguous, classification.Reason, nil)
		}
		if browser.State == "running" {
			if stopErr := service.workspaces.runtime.Stop(ctx, browser, runtime.StopPolicy{TimeoutSeconds: workspaceStopSeconds, Signal: "TERM"}); stopErr != nil {
				return stopErr
			}
			browser, inspectErr = service.workspaces.runtime.Inspect(ctx, browser.ID)
			if errors.Is(inspectErr, runtime.ErrResourceNotFound) {
				return nil
			}
			if inspectErr != nil {
				return model.Wrap(model.CodeUnavailable, "inspect stopped browser after workspace removal", inspectErr)
			}
			classification = ownership.Classify(&session.Record, &browser)
			if !classification.DeleteAllowed {
				return model.NewError(model.CodeAmbiguous, classification.Reason, nil)
			}
		}
		return service.workspaces.runtime.Delete(ctx, browser)
	}
	access := workspaceAccess{Manifest: &manifest}
	index, found := manifestResourceIndex(manifest.Resources, runtime.ResourceBrowser)
	if !found {
		return nil
	}
	if manifest.Resources[index].Name != session.Record.Name {
		return model.NewError(model.CodeAmbiguous, "browser cleanup ownership changed during the agent session", nil)
	}
	return service.deleteBrowserWithAccess(ctx, access, index)
}

func (service *AgentService) deleteBrowserWithAccess(ctx context.Context, access workspaceAccess, index int) error {
	if index < 0 || index >= len(access.Manifest.Resources) {
		return model.NewError(model.CodeInternal, "browser ownership record is missing during cleanup", nil)
	}
	record := access.Manifest.Resources[index]
	browser, err := service.workspaces.runtime.Inspect(ctx, runtime.ResourceID(record.ExpectedID))
	if errors.Is(err, runtime.ErrResourceNotFound) {
		if record.Created {
			record.Deleted = true
			record.Absent = false
		} else {
			record.Absent = true
		}
		access.Manifest.Resources[index] = record
		return service.workspaces.replaceManifest(ctx, access.Manifest)
	}
	if err != nil {
		return model.Wrap(model.CodeUnavailable, "inspect browser for cleanup", err)
	}
	classification := ownership.Classify(&record, &browser)
	if !classification.DeleteAllowed {
		return model.NewError(model.CodeAmbiguous, classification.Reason, nil)
	}
	if browser.State == "running" {
		if err := service.workspaces.runtime.Stop(ctx, browser, runtime.StopPolicy{TimeoutSeconds: workspaceStopSeconds, Signal: "TERM"}); err != nil {
			return model.Wrap(model.CodeUnavailable, "stop disposable browser", err)
		}
		browser, err = service.workspaces.runtime.Inspect(ctx, browser.ID)
		if errors.Is(err, runtime.ErrResourceNotFound) {
			record.Deleted = true
			access.Manifest.Resources[index] = record
			return service.workspaces.replaceManifest(ctx, access.Manifest)
		}
		if err != nil {
			return model.Wrap(model.CodeUnavailable, "inspect stopped browser", err)
		}
		classification = ownership.Classify(&record, &browser)
		if !classification.DeleteAllowed {
			return model.NewError(model.CodeAmbiguous, classification.Reason, nil)
		}
		if browser.State == "running" {
			return model.NewError(model.CodeUnavailable, "browser remained running after stop", nil)
		}
	}
	if err := service.workspaces.runtime.Delete(ctx, browser); err != nil {
		return model.Wrap(model.CodeUnavailable, "delete disposable browser", err)
	}
	record.Deleted = true
	record.Absent = false
	access.Manifest.Resources[index] = record
	return service.workspaces.replaceManifest(ctx, access.Manifest)
}

func manifestResourceIndex(resources []state.ResourceRecord, kind runtime.ResourceKind) (int, bool) {
	found := -1
	for index := range resources {
		if resources[index].Kind != string(kind) {
			continue
		}
		if found >= 0 {
			return -1, false
		}
		found = index
	}
	return found, found >= 0
}
