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
	"github.com/srimajji/dsx/internal/plan"
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

func browserEnableOverride(enabled bool) *bool {
	if !enabled {
		return nil
	}
	value := true
	return &value
}

func browserEnabled(execution plan.ExecutionPlan) bool {
	return execution.Browser != nil && execution.Browser.Enabled
}

func rejectPlaywrightMCP(servers []harness.MCPServer) error {
	for _, server := range servers {
		if server.Name == browserMCPName {
			return model.NewError(model.CodeInvalidInput, "the playwright MCP server name is reserved for the requested DSX browser", nil)
		}
	}
	return nil
}
func injectPlaywrightMCP(request CloneRunRequest, server harness.MCPServer) (CloneRunRequest, error) {
	if err := rejectPlaywrightMCP(request.MCPServers); err != nil {
		return request, err
	}
	if server.Name != browserMCPName || server.URL == "" || len(server.Command) != 0 || len(server.Env) != 0 {
		return request, model.NewError(model.CodeInternal, "generated playwright MCP server is invalid", nil)
	}
	request.MCPServers = append(append([]harness.MCPServer(nil), request.MCPServers...), server)
	return request, nil
}

func browserSpecForClone(execution plan.ExecutionPlan, image runtime.Image, record state.ResourceRecord, network string) (runtime.BrowserSpec, error) {
	if !browserEnabled(execution) || execution.Browser.ImageReference == "" || execution.Browser.ImageDigest == "" {
		return runtime.BrowserSpec{}, errors.New("enabled browser plan requires a pinned image")
	}
	if record.Kind != string(runtime.ResourceBrowser) || record.Role != browserRole || network == "" {
		return runtime.BrowserSpec{}, errors.New("browser requires an owned resource identity and network")
	}
	return runtime.BrowserSpec{
		Name:        record.Name,
		Image:       image,
		Entrypoint:  append([]string(nil), browserEntrypoint...),
		Networks:    []string{network},
		Labels:      runtimeLabels(record.Labels),
		CPUs:        execution.Limits.CPUs,
		MemoryBytes: execution.Limits.MemoryBytes,
	}, nil
}

func (service *CloneService) createBrowser(ctx context.Context, owner runtime.ResourceSnapshot, execution plan.ExecutionPlan, browserIndex int, manifest *state.Manifest) (server *harness.MCPServer, returnErr error) {
	if browserIndex < 0 || browserIndex >= len(manifest.Resources) {
		return nil, model.NewError(model.CodeInternal, "planned browser resource is missing", nil)
	}
	workspaceRecord, err := manifestResource(*manifest, runtime.ResourceWorkspace, workspaceRole)
	if err != nil {
		return nil, err
	}
	owner, err = service.lifecycle.runtime.Inspect(ctx, runtime.ResourceID(workspaceRecord.ExpectedID))
	if err != nil {
		return nil, model.Wrap(model.CodeUnavailable, "inspect browser owner workspace", err)
	}
	classification := ownership.Classify(&workspaceRecord, &owner)
	if !classification.DeleteAllowed || owner.State != "running" {
		return nil, model.NewError(model.CodeConflict, "browser owner workspace is not running with verified ownership", nil)
	}
	networkRecord, err := manifestResource(*manifest, runtime.ResourceNetwork, networkRole)
	if err != nil {
		return nil, err
	}
	browserPlan := execution.Browser
	image, err := service.lifecycle.runtime.EnsureImage(ctx, runtime.ImageSpec{Reference: browserPlan.ImageReference})
	if err != nil {
		return nil, model.Wrap(model.CodeUnavailable, "ensure pinned browser image", err)
	}
	if image.Digest != "sha256:"+browserPlan.ImageDigest {
		return nil, model.NewError(model.CodeUnavailable, "runtime returned an unexpected browser image digest", nil)
	}
	record := manifest.Resources[browserIndex]
	spec, err := browserSpecForClone(execution, image, record, networkRecord.Name)
	if err != nil {
		return nil, model.Wrap(model.CodeInvalidInput, "prepare browser resource", err)
	}
	if err := service.lifecycle.createResource(ctx, manifest, browserIndex, func(state.ResourceRecord) (runtime.Resource, error) {
		return service.lifecycle.runtime.CreateBrowser(ctx, spec)
	}); err != nil {
		return nil, err
	}
	cleanupNeeded := true
	defer func() {
		if !cleanupNeeded {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), browserCleanupTimeout)
		defer cancel()
		returnErr = errors.Join(returnErr, service.deleteBrowser(cleanupCtx, browserIndex, manifest))
	}()
	browser, err := service.lifecycle.runtime.Inspect(ctx, runtime.ResourceID(record.ExpectedID))
	if err != nil {
		return nil, model.Wrap(model.CodeUnavailable, "inspect created browser", err)
	}
	if err := verifyBrowserSnapshot(record, browser, networkRecord.Name, browserPlan.ImageDigest, false); err != nil {
		return nil, err
	}
	if err := service.lifecycle.runtime.StartWorkspace(ctx, browser); err != nil {
		return nil, model.Wrap(model.CodeUnavailable, "start browser", err)
	}
	browser, err = service.lifecycle.runtime.Inspect(ctx, runtime.ResourceID(record.ExpectedID))
	if err != nil {
		return nil, model.Wrap(model.CodeUnavailable, "inspect started browser", err)
	}
	if err := verifyBrowserSnapshot(record, browser, networkRecord.Name, browserPlan.ImageDigest, true); err != nil {
		return nil, err
	}
	address, err := browserNetworkIPv4(browser, networkRecord.Name)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("http://%s:%d%s", address.String(), browserMCPPort, browserMCPPath)
	readyCtx, cancel := context.WithTimeout(ctx, browserReadyTimeout)
	defer cancel()
	if err := service.waitBrowserReady(readyCtx, workspaceRecord, record, networkRecord.Name, browserPlan.ImageDigest, url); err != nil {
		return nil, err
	}
	cleanupNeeded = false
	return &harness.MCPServer{Name: browserMCPName, URL: url}, nil
}

func verifyBrowserSnapshot(record state.ResourceRecord, snapshot runtime.ResourceSnapshot, network, imageDigest string, requireRunning bool) error {
	classification := ownership.Classify(&record, &snapshot)
	if !classification.DeleteAllowed {
		return model.NewError(model.CodeAmbiguous, classification.Reason, nil)
	}
	if snapshot.Kind != runtime.ResourceBrowser || len(snapshot.Mounts) != 0 || len(snapshot.Ports) != 0 || len(snapshot.Networks) != 1 || snapshot.Networks[0] != network {
		return model.NewError(model.CodeUnavailable, "browser runtime violated its isolated resource contract", nil)
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
			return netip.Addr{}, model.NewError(model.CodeAmbiguous, "browser has multiple private IPv4 addresses on its owner network", nil)
		}
		selected = address
	}
	if !selected.IsValid() {
		return netip.Addr{}, model.NewError(model.CodeUnavailable, "browser has no inspected private IPv4 address on its owner network", nil)
	}
	return selected, nil
}

func (service *CloneService) waitBrowserReady(ctx context.Context, ownerRecord, record state.ResourceRecord, network, imageDigest, url string) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return model.Wrap(model.CodeUnavailable, "browser MCP readiness timed out", ctx.Err())
		case <-timer.C:
		}
		owner, err := service.lifecycle.runtime.Inspect(ctx, runtime.ResourceID(ownerRecord.ExpectedID))
		if err != nil {
			return model.Wrap(model.CodeUnavailable, "inspect browser owner during readiness", err)
		}
		ownerClassification := ownership.Classify(&ownerRecord, &owner)
		if !ownerClassification.DeleteAllowed || owner.State != "running" {
			return model.NewError(model.CodeUnavailable, "browser owner workspace exited before readiness", nil)
		}
		browser, err := service.lifecycle.runtime.Inspect(ctx, runtime.ResourceID(record.ExpectedID))
		if err != nil {
			if errors.Is(err, runtime.ErrResourceNotFound) {
				return model.NewError(model.CodeUnavailable, "browser disappeared before readiness", nil)
			}
			return model.Wrap(model.CodeUnavailable, "inspect browser readiness", err)
		}
		if err := verifyBrowserSnapshot(record, browser, network, imageDigest, true); err != nil {
			return err
		}
		exit, probeErr := service.shell(ctx, owner, []string{
			"/usr/bin/curl", "--silent", "--show-error", "--output", "/dev/null", "--max-time", "2", url,
		}, nil, false, nil, nil, nil, nil, string(workspaceGuestRoot))
		if probeErr == nil && exit.Signal == "" && exit.Code != nil && *exit.Code == 0 {
			return nil
		}
		timer.Reset(browserReadyPoll)
	}
}

func (service *CloneService) deleteBrowser(ctx context.Context, browserIndex int, manifest *state.Manifest) error {
	if browserIndex < 0 || browserIndex >= len(manifest.Resources) {
		return model.NewError(model.CodeInternal, "planned browser resource is missing during cleanup", nil)
	}
	record := manifest.Resources[browserIndex]
	browser, found, err := service.lifecycle.findRuntimeResource(ctx, record)
	if err != nil {
		return model.Wrap(model.CodeUnavailable, "inspect browser for cleanup", err)
	}
	if !found {
		return service.persistDeletedBrowser(ctx, browserIndex, manifest, record.Created)
	}
	classification := ownership.Classify(&record, &browser)
	if !classification.DeleteAllowed {
		return model.NewError(model.CodeAmbiguous, classification.Reason, nil)
	}
	if browser.State == "running" {
		if err := service.lifecycle.runtime.Stop(ctx, browser, runtime.StopPolicy{TimeoutSeconds: lifecycleStopSeconds, Signal: "TERM"}); err != nil {
			return model.Wrap(model.CodeUnavailable, "stop browser", err)
		}
		browser, found, err = service.lifecycle.findRuntimeResource(ctx, record)
		if err != nil {
			return model.Wrap(model.CodeUnavailable, "inspect stopped browser", err)
		}
		if !found {
			return service.persistDeletedBrowser(ctx, browserIndex, manifest, true)
		}
		classification = ownership.Classify(&record, &browser)
		if !classification.DeleteAllowed {
			return model.NewError(model.CodeAmbiguous, classification.Reason, nil)
		}
		if browser.State == "running" {
			return model.NewError(model.CodeUnavailable, "browser remained running after stop", nil)
		}
	}
	if err := service.lifecycle.runtime.Delete(ctx, browser); err != nil {
		return model.Wrap(model.CodeUnavailable, "delete browser", err)
	}
	return service.persistDeletedBrowser(ctx, browserIndex, manifest, true)
}

func (service *CloneService) persistDeletedBrowser(ctx context.Context, browserIndex int, manifest *state.Manifest, created bool) error {
	next := *manifest
	next.Resources = append([]state.ResourceRecord(nil), manifest.Resources...)
	record := &next.Resources[browserIndex]
	if created {
		record.RuntimeID = record.ExpectedID
		record.Created = true
		record.Deleted = true
		record.Absent = false
	} else {
		record.Absent = true
	}
	if err := service.lifecycle.replace(ctx, &next); err != nil {
		return model.Wrap(model.CodeUnavailable, "persist deleted browser evidence", err)
	}
	*manifest = next
	return nil
}
