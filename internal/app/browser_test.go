package app

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
)

func browserCloneFixture(t *testing.T, sandbox string) (*CloneService, *cloneRecordingRuntime, state.Manifest, cloneVolumeIndexes, runtime.ResourceSnapshot) {
	t.Helper()
	lifecycle, _, _, base, _ := lifecycleFixture(t)
	fake := &cloneRecordingRuntime{lifecycleRuntime: base, diffCode: 0, resultCommit: cloneTestCommit}
	lifecycle.runtime = fake
	service := &CloneService{
		lifecycle: lifecycle,
		harness:   &HarnessService{},
		git:       &cloneGitStub{status: gitx.Status{HostCommit: cloneTestCommit, HostTrackedClean: true, HostTrackedFingerprint: cloneTestFingerprint}},
		tempRoot:  t.TempDir(),
	}
	execution, artifacts := clonePlanFixture(t, sandbox)
	execution.Browser = &plan.BrowserPlan{
		Enabled: true, ImageReference: "example/browser@sha256:" + cloneTestDigest, ImageDigest: cloneTestDigest,
	}
	runID, err := model.ParseRunID("01890f5c-7b00-7000-8000-000000000039")
	if err != nil {
		t.Fatal(err)
	}
	manifest, indexes, err := plannedCloneManifest(execution, runID, lifecycle.now().UTC(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.manifests.CreateIntent(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateCreating, "create", ""); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.createResource(context.Background(), &manifest, 0, func(record state.ResourceRecord) (runtime.Resource, error) {
		return fake.CreateNetwork(context.Background(), runtime.NetworkSpec{Name: record.Name, Labels: runtimeLabels(record.Labels)})
	}); err != nil {
		t.Fatal(err)
	}
	workspaceRecord := manifest.Resources[indexes.owner]
	if err := lifecycle.createResource(context.Background(), &manifest, indexes.owner, func(state.ResourceRecord) (runtime.Resource, error) {
		return fake.CreateWorkspace(context.Background(), runtime.WorkspaceSpec{
			Name: workspaceRecord.Name, Networks: []string{manifest.Resources[0].Name}, Labels: runtimeLabels(workspaceRecord.Labels),
		})
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := fake.Inspect(context.Background(), runtime.ResourceID(workspaceRecord.ExpectedID))
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.StartWorkspace(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	owner, err = fake.Inspect(context.Background(), runtime.ResourceID(workspaceRecord.ExpectedID))
	if err != nil {
		t.Fatal(err)
	}
	return service, fake, manifest, indexes, owner
}

func TestBrowserManifestAndSpecAreIsolatedAndCleanupOrdered(t *testing.T) {
	service, fake, manifest, indexes, owner := browserCloneFixture(t, "browser-contract")
	if indexes.browser != indexes.owner+1 || indexes.browser != len(manifest.Resources)-1 {
		t.Fatalf("browser manifest order = owner %d browser %d resources %d", indexes.owner, indexes.browser, len(manifest.Resources))
	}
	server, err := service.createBrowser(context.Background(), owner, browserExecutionPlan(t, manifest), indexes.browser, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	record := manifest.Resources[indexes.browser]
	if want := (harness.MCPServer{Name: "playwright", URL: "http://192.168.64.10:8931/mcp"}); !reflect.DeepEqual(*server, want) {
		t.Fatalf("browser MCP = %#v, want %#v", *server, want)
	}
	if fake.browserSpec.Name != record.Name ||
		!reflect.DeepEqual(fake.browserSpec.Networks, []string{manifest.Resources[0].Name}) ||
		!reflect.DeepEqual(fake.browserSpec.Entrypoint, browserEntrypoint) || len(fake.browserSpec.Env) != 0 {
		t.Fatalf("browser spec = %#v", fake.browserSpec)
	}
	browserType := reflect.TypeOf(runtime.BrowserSpec{})
	for _, forbidden := range []string{"Mounts", "Ports", "WorkingDir", "User", "HostPath", "Volumes"} {
		if _, found := browserType.FieldByName(forbidden); found {
			t.Fatalf("BrowserSpec exposes forbidden field %q", forbidden)
		}
	}
	if err := service.deleteBrowser(context.Background(), indexes.browser, &manifest); err != nil {
		t.Fatal(err)
	}
	if !manifest.Resources[indexes.browser].Deleted {
		t.Fatal("browser deletion was not persisted")
	}
	if err := service.markCapturePending(context.Background(), &manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := service.captureResults(context.Background(), owner, &manifest); err != nil {
		t.Fatal(err)
	}
	deleteAt, captureAt := callIndex(fake.calls, "delete:browser"), callIndex(fake.calls, "exec:"+DefaultGuestHelperPath+" exec -- /usr/bin/git")
	if deleteAt < 0 || captureAt < 0 || deleteAt >= captureAt {
		t.Fatalf("browser was not deleted before capture: %#v", fake.calls)
	}
}

func TestBrowserReadinessTimeoutAndCrashFailClosed(t *testing.T) {
	service, fake, manifest, indexes, owner := browserCloneFixture(t, "browser-readiness")
	execution := browserExecutionPlan(t, manifest)
	server, err := service.createBrowser(context.Background(), owner, execution, indexes.browser, &manifest)
	if err != nil || server == nil {
		t.Fatalf("initial browser creation = %#v, %v", server, err)
	}
	record := manifest.Resources[indexes.browser]
	browser := fake.resources[runtime.ResourceID(record.ExpectedID)]
	failureCode := 7
	fake.execExit = runtime.Exit{Code: &failureCode}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := service.waitBrowserReady(ctx, manifest.Resources[indexes.owner], record, manifest.Resources[0].Name, execution.Browser.ImageDigest, server.URL); model.ErrorCodeOf(err) != model.CodeUnavailable || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("readiness timeout = %v", err)
	}
	ownerCrash := fake.resources[owner.ID]
	ownerCrash.State = "exited"
	fake.resources[owner.ID] = ownerCrash
	beforeOwnerCrash := len(fake.specs)
	if err := service.waitBrowserReady(context.Background(), manifest.Resources[indexes.owner], record, manifest.Resources[0].Name, execution.Browser.ImageDigest, server.URL); model.ErrorCodeOf(err) != model.CodeUnavailable || !strings.Contains(err.Error(), "owner workspace exited") {
		t.Fatalf("owner crash = %v", err)
	}
	if len(fake.specs) != beforeOwnerCrash {
		t.Fatal("readiness probe ran after the owner workspace crash")
	}
	ownerCrash.State = "running"
	fake.resources[owner.ID] = ownerCrash
	browser.State = "exited"
	fake.resources[browser.ID] = browser
	before := len(fake.specs)
	if err := service.waitBrowserReady(context.Background(), manifest.Resources[indexes.owner], record, manifest.Resources[0].Name, execution.Browser.ImageDigest, server.URL); model.ErrorCodeOf(err) != model.CodeUnavailable || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("browser crash = %v", err)
	}
	if len(fake.specs) != before {
		t.Fatal("readiness probe ran after the browser crash")
	}
}

func TestBrowserStartupFailureCleansCreatedResource(t *testing.T) {
	service, fake, manifest, indexes, owner := browserCloneFixture(t, "browser-start-fail")
	injected := errors.New("injected browser start failure")
	fake.failStart = injected
	if _, err := service.createBrowser(context.Background(), owner, browserExecutionPlan(t, manifest), indexes.browser, &manifest); !errors.Is(err, injected) {
		t.Fatalf("createBrowser error = %v, want injected failure", err)
	}
	record := manifest.Resources[indexes.browser]
	if !record.Deleted {
		t.Fatalf("failed browser was not durably deleted: %#v", record)
	}
	if _, found := fake.resources[runtime.ResourceID(record.ExpectedID)]; found {
		t.Fatalf("failed browser %q remains in runtime", record.ExpectedID)
	}
}
func TestBrowserPreHarnessFailureCleansCreatedResource(t *testing.T) {
	service, fake, manifest, indexes, owner := browserCloneFixture(t, "browser-preflight-fail")
	injected := errors.New("injected project unlock failure")
	_, err := service.executeCloneRun(
		context.Background(),
		CloneRunRequest{},
		browserExecutionPlan(t, manifest),
		owner,
		indexes,
		&manifest,
		nil,
		nil,
		func() {},
		func() error { return injected },
	)
	if !errors.Is(err, injected) {
		t.Fatalf("executeCloneRun error = %v, want injected failure", err)
	}
	record := manifest.Resources[indexes.browser]
	if !record.Deleted {
		t.Fatalf("pre-harness failure did not durably delete browser: %#v", record)
	}
	if _, found := fake.resources[runtime.ResourceID(record.ExpectedID)]; found {
		t.Fatalf("pre-harness failure left browser %q running", record.ExpectedID)
	}
}

func TestBrowserMCPDuplicateAndNetworkAddressValidation(t *testing.T) {
	if err := rejectPlaywrightMCP([]harness.MCPServer{{Name: "other"}, {Name: "playwright"}}); model.ErrorCodeOf(err) != model.CodeInvalidInput {
		t.Fatalf("duplicate playwright MCP = %v", err)
	}
	if err := rejectPlaywrightMCP([]harness.MCPServer{{Name: "Playwright"}}); err != nil {
		t.Fatalf("case-distinct MCP name was rejected: %v", err)
	}
	request := CloneRunRequest{MCPServers: []harness.MCPServer{{Name: "other", Command: []string{"tool"}}}}
	injected, err := injectPlaywrightMCP(request, harness.MCPServer{Name: "playwright", URL: "http://192.168.64.7:8931/mcp"})
	if err != nil || len(injected.MCPServers) != 2 || injected.MCPServers[0].Name != "other" || injected.MCPServers[1].Name != "playwright" {
		t.Fatalf("playwright MCP injection = %#v, %v", injected.MCPServers, err)
	}
	if len(request.MCPServers) != 1 {
		t.Fatalf("playwright injection mutated caller request: %#v", request.MCPServers)
	}
	if _, err := injectPlaywrightMCP(CloneRunRequest{MCPServers: []harness.MCPServer{{Name: "playwright"}}}, harness.MCPServer{Name: "playwright", URL: "http://192.168.64.7:8931/mcp"}); model.ErrorCodeOf(err) != model.CodeInvalidInput {
		t.Fatalf("duplicate injection error = %v", err)
	}
	network := "owner-network"
	for _, test := range []struct {
		name      string
		addresses []netip.Addr
		want      string
	}{
		{name: "owner private IPv4", addresses: []netip.Addr{netip.MustParseAddr("fd00::10"), netip.MustParseAddr("192.168.64.7")}, want: "192.168.64.7"},
		{name: "public rejected", addresses: []netip.Addr{netip.MustParseAddr("203.0.113.7")}},
		{name: "ambiguous rejected", addresses: []netip.Addr{netip.MustParseAddr("192.168.64.7"), netip.MustParseAddr("192.168.64.8")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			address, err := browserNetworkIPv4(runtime.ResourceSnapshot{NetworkAddresses: map[string][]netip.Addr{network: test.addresses}}, network)
			if test.want == "" && err == nil {
				t.Fatalf("browserNetworkIPv4 = %s, want error", address)
			}
			if test.want != "" && (err != nil || address.String() != test.want) {
				t.Fatalf("browserNetworkIPv4 = %s, %v, want %s", address, err, test.want)
			}
		})
	}
}

func TestBrowserRollbackAndCleanupFailureRetention(t *testing.T) {
	t.Run("rollback is reverse ownership order", func(t *testing.T) {
		service, fake, manifest, indexes, owner := browserCloneFixture(t, "browser-rollback")
		if _, err := service.createBrowser(context.Background(), owner, browserExecutionPlan(t, manifest), indexes.browser, &manifest); err != nil {
			t.Fatal(err)
		}
		if err := service.lifecycle.rollbackCreate(context.Background(), &manifest); err != nil {
			t.Fatal(err)
		}
		browserAt, workspaceAt, networkAt := callIndex(fake.calls, "delete:browser"), callIndex(fake.calls, "delete:workspace"), callIndex(fake.calls, "delete:network")
		if !(browserAt >= 0 && browserAt < workspaceAt && workspaceAt < networkAt) {
			t.Fatalf("rollback order = %#v", fake.calls)
		}
	})

	t.Run("delete failure retains exact manifest evidence", func(t *testing.T) {
		service, fake, manifest, indexes, owner := browserCloneFixture(t, "browser-retain")
		if _, err := service.createBrowser(context.Background(), owner, browserExecutionPlan(t, manifest), indexes.browser, &manifest); err != nil {
			t.Fatal(err)
		}
		injected := errors.New("injected browser delete failure")
		fake.failBrowserDelete = injected
		if err := service.deleteBrowser(context.Background(), indexes.browser, &manifest); !errors.Is(err, injected) {
			t.Fatalf("deleteBrowser error = %v, want injected failure", err)
		}
		stored, found, err := service.lifecycle.manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
		if err != nil || !found {
			t.Fatalf("load retained manifest = found %t, %v", found, err)
		}
		record := stored.Resources[indexes.browser]
		if !record.Created || record.Deleted || record.RuntimeID != record.ExpectedID {
			t.Fatalf("retained browser evidence = %#v", record)
		}
		if _, found := fake.resources[runtime.ResourceID(record.ExpectedID)]; !found {
			t.Fatal("failed browser cleanup removed the runtime evidence")
		}
	})
}

func browserExecutionPlan(t *testing.T, manifest state.Manifest) plan.ExecutionPlan {
	t.Helper()
	execution, _ := clonePlanFixture(t, string(manifest.Sandbox))
	execution.Project.ID = manifest.ProjectID
	execution.Project.CanonicalRoot = manifest.CanonicalRoot
	execution.Sandbox.RunID = manifest.RunID
	execution.Browser = &plan.BrowserPlan{
		Enabled: true, ImageReference: "example/browser@sha256:" + cloneTestDigest, ImageDigest: cloneTestDigest,
	}
	return execution
}

func callIndex(calls []string, prefix string) int {
	for index, call := range calls {
		if strings.HasPrefix(call, prefix) {
			return index
		}
	}
	return -1
}
