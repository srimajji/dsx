package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/auth"
	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
	statefs "github.com/srimajji/dsx/internal/state/fs"
)

func TestUnfetchedCleanupOwnershipMatrix(t *testing.T) {
	tests := map[string]struct {
		records       func(string, model.SandboxName) []state.GitRecord
		blocked       bool
		preservedRepo string
	}{
		"unfetched": {
			records: func(root string, sandbox model.SandboxName) []state.GitRecord {
				return []state.GitRecord{cleanupGitRecord(root, sandbox, "workspace", cleanupResultUnfetched)}
			},
			blocked: true, preservedRepo: "workspace",
		},
		"partially fetched composite": {
			records: func(root string, sandbox model.SandboxName) []state.GitRecord {
				return []state.GitRecord{
					cleanupGitRecord(root, sandbox, "api", cleanupResultFetched),
					cleanupGitRecord(root, sandbox, "web", cleanupResultUnfetched),
				}
			},
			blocked: true, preservedRepo: "web",
		},
		"mismatched fetched commit": {
			records: func(root string, sandbox model.SandboxName) []state.GitRecord {
				return []state.GitRecord{cleanupGitRecord(root, sandbox, "workspace", cleanupResultMismatched)}
			},
			blocked: true, preservedRepo: "workspace",
		},
		"no result work": {
			records: func(root string, sandbox model.SandboxName) []state.GitRecord {
				return []state.GitRecord{cleanupGitRecord(root, sandbox, "workspace", cleanupResultNone)}
			},
		},
		"fully fetched": {
			records: func(root string, sandbox model.SandboxName) []state.GitRecord {
				return []state.GitRecord{cleanupGitRecord(root, sandbox, "workspace", cleanupResultFetched)}
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			service, root, manifests, fake, _ := lifecycleFixture(t)
			sandbox := model.SandboxName("review")
			manifest := storeCleanupClone(t, manifests, fake, root, sandbox, "01890f5c-7b00-7000-8000-000000000011", test.records(root, sandbox))

			cleaned, err := service.Clean(context.Background(), CleanRequest{Root: root, Confirmed: true})
			if test.blocked {
				if model.ErrorCodeOf(err) != model.CodeDataLoss {
					t.Fatalf("Clean() error = %v (code %q), want data loss", err, model.ErrorCodeOf(err))
				}
				entry := "clone sandbox review repository " + test.preservedRepo + " has unfetched result work"
				if !reflect.DeepEqual(cleaned.Preserved, []string{entry}) || !strings.Contains(err.Error(), entry) {
					t.Fatalf("Clean() = %#v, error = %v", cleaned, err)
				}
				if cleaned.DeletedManifests != 0 || cleaned.DeletedResources != 0 {
					t.Fatalf("guarded cleanup deleted ownership: %#v", cleaned)
				}
				if !reflect.DeepEqual(fake.calls, []string{"stop", "start", "stop"}) {
					t.Fatalf("guarded cleanup did not quiesce workspace: calls=%#v", fake.calls)
				}
				if snapshot := fake.resources[runtime.ResourceID(manifest.Resources[0].RuntimeID)]; snapshot.State != "stopped" {
					t.Fatalf("guarded cleanup left workspace state %q", snapshot.State)
				}
				stored, found, loadErr := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
				if loadErr != nil || !found || !reflect.DeepEqual(stored, manifest) {
					t.Fatalf("guarded cleanup mutated manifest: found=%t manifest=%#v err=%v", found, stored, loadErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cleaned.DeletedManifests != 1 || cleaned.DeletedResources != 1 || len(cleaned.Preserved) != 0 {
				t.Fatalf("Clean() = %#v", cleaned)
			}
			if len(fake.resources) != 0 {
				t.Fatalf("eligible cleanup left resources: %#v", fake.resources)
			}
		})
	}
}

func TestForceConfirmationDoesNotBypassUnfetchedCleanupGuard(t *testing.T) {
	service, root, manifests, fake, _ := lifecycleFixture(t)
	sandbox := model.SandboxName("forced")
	manifest := storeCleanupClone(t, manifests, fake, root, sandbox, "01890f5c-7b00-7000-8000-000000000012", []state.GitRecord{
		cleanupGitRecord(root, sandbox, "workspace", cleanupResultUnfetched),
	})

	// Confirmed=true is the application request produced by the CLI --force
	// confirmation bypass. It must not authorize loss of clone results.
	_, err := service.Clean(context.Background(), CleanRequest{Root: root, Confirmed: true})
	if model.ErrorCodeOf(err) != model.CodeDataLoss {
		t.Fatalf("forced Clean() error = %v (code %q)", err, model.ErrorCodeOf(err))
	}
	if _, found, loadErr := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID); loadErr != nil || !found {
		t.Fatalf("forced refusal lost manifest: found=%t err=%v", found, loadErr)
	}
	if len(fake.resources) != 1 || !reflect.DeepEqual(fake.calls, []string{"stop", "start", "stop"}) {
		t.Fatalf("forced refusal did not retain a quiesced workspace: calls=%#v resources=%#v", fake.calls, fake.resources)
	}
}

func TestDiscardUnfetchedStillRequiresCleanupConfirmation(t *testing.T) {
	service, root, manifests, fake, manifest := pendingCleanupCaptureFixture(
		t, "unconfirmed-discard", "01890f5c-7b00-7000-8000-000000000047", "workspace",
	)
	beforeResources := cloneRuntimeResources(fake.resources)

	cleaned, err := service.Clean(context.Background(), CleanRequest{
		Root: root, Sandbox: "unconfirmed-discard", DiscardUnfetched: true,
	})
	if model.ErrorCodeOf(err) != model.CodeUnapproved || cleaned.DeletedManifests != 0 || cleaned.DeletedResources != 0 {
		t.Fatalf("unconfirmed explicit discard Clean() = %#v, %v", cleaned, err)
	}
	stored, found, loadErr := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if loadErr != nil || !found || !reflect.DeepEqual(stored, manifest) || !reflect.DeepEqual(fake.resources, beforeResources) || len(fake.specs) != 0 {
		t.Fatalf("unconfirmed discard mutated state: found=%t manifest=%#v resources=%#v specs=%#v err=%v", found, stored, fake.resources, fake.specs, loadErr)
	}
}

func TestNamedStopAndCleanPreserveSiblingProjectAndAuthentication(t *testing.T) {
	service, root, manifests, fake, _ := lifecycleFixture(t)
	ctx := context.Background()
	api := markCleanupCloneRunning(t, manifests, storeCleanupClone(t, manifests, fake, root, "api", "01890f5c-7b00-7000-8000-000000000021", []state.GitRecord{
		cleanupGitRecord(root, "api", "workspace", cleanupResultFetched),
	}))
	tests := markCleanupCloneRunning(t, manifests, storeCleanupClone(t, manifests, fake, root, "tests", "01890f5c-7b00-7000-8000-000000000022", []state.GitRecord{
		cleanupGitRecord(root, "tests", "workspace", cleanupResultFetched),
	}))
	otherRoot := canonicalCleanupRoot(t)
	other := markCleanupCloneRunning(t, manifests, storeCleanupClone(t, manifests, fake, otherRoot, "api", "01890f5c-7b00-7000-8000-000000000023", []state.GitRecord{
		cleanupGitRecord(otherRoot, "api", "workspace", cleanupResultFetched),
	}))

	stopped, err := service.Stop(ctx, StopRequest{Root: root, Sandbox: "api"})
	if err != nil || stopped.Sandbox != api.Sandbox || stopped.State != model.StateStopped {
		t.Fatalf("Stop(api) = %#v, %v: %v", stopped, err, errors.Unwrap(err))
	}
	if fake.resources[runtime.ResourceID(api.Resources[0].RuntimeID)].State != "stopped" {
		t.Fatal("selected clone workspace did not stop")
	}
	for _, sibling := range []state.Manifest{tests, other} {
		if fake.resources[runtime.ResourceID(sibling.Resources[0].RuntimeID)].State != "running" {
			t.Fatalf("stop changed sibling %s runtime", sibling.Sandbox)
		}
		stored, found, loadErr := manifests.LoadManifest(ctx, sibling.ProjectID, sibling.Sandbox, sibling.RunID)
		if loadErr != nil || !found || stored.State != model.StateRunning {
			t.Fatalf("stop changed sibling %s manifest: found=%t state=%q err=%v", sibling.Sandbox, found, stored.State, loadErr)
		}
	}

	authRoot := t.TempDir()
	globalAuth := filepath.Join(authRoot, "global.auth")
	candidateAuth := filepath.Join(authRoot, "global-conflict-candidate.auth")
	siblingAuth := filepath.Join(authRoot, "tests.auth")
	selectedAuth := filepath.Join(authRoot, "api.auth")
	globalBytes := []byte("global-auth-byte-identical")
	candidateBytes := []byte("global-candidate-byte-identical")
	siblingBytes := []byte("sibling-auth-byte-identical")
	if err := os.WriteFile(globalAuth, globalBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidateAuth, candidateBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(siblingAuth, siblingBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(selectedAuth, []byte("selected-sandbox-auth"), 0o600); err != nil {
		t.Fatal(err)
	}
	service.cleanSandboxAuth = func(_ context.Context, projectID model.ProjectID, sandbox model.SandboxName) error {
		if projectID != api.ProjectID || sandbox != api.Sandbox {
			return fmt.Errorf("auth cleanup selected %s/%s", projectID, sandbox)
		}
		if _, found := fake.resources[runtime.ResourceID(api.Resources[0].RuntimeID)]; found {
			return errors.New("auth cleanup ran before runtime resource deletion")
		}
		return os.Remove(selectedAuth)
	}

	cleaned, err := service.Clean(ctx, CleanRequest{Root: root, Sandbox: "api", Confirmed: true})
	if err != nil || cleaned.DeletedManifests != 1 || cleaned.DeletedResources != 1 {
		t.Fatalf("Clean(api) = %#v, %v", cleaned, err)
	}
	if _, found, loadErr := manifests.LoadManifest(ctx, api.ProjectID, api.Sandbox, api.RunID); loadErr != nil || found {
		t.Fatalf("selected manifest remains: found=%t err=%v", found, loadErr)
	}
	for _, sibling := range []state.Manifest{tests, other} {
		if _, found := fake.resources[runtime.ResourceID(sibling.Resources[0].RuntimeID)]; !found {
			t.Fatalf("clean deleted sibling %s runtime", sibling.Sandbox)
		}
		if _, found, loadErr := manifests.LoadManifest(ctx, sibling.ProjectID, sibling.Sandbox, sibling.RunID); loadErr != nil || !found {
			t.Fatalf("clean deleted sibling %s manifest: found=%t err=%v", sibling.Sandbox, found, loadErr)
		}
	}
	if _, err := os.Stat(selectedAuth); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("selected sandbox auth remains: %v", err)
	}
	if got, err := os.ReadFile(globalAuth); err != nil || !reflect.DeepEqual(got, globalBytes) {
		t.Fatalf("global auth changed: %q, %v", got, err)
	}
	if got, err := os.ReadFile(candidateAuth); err != nil || !reflect.DeepEqual(got, candidateBytes) {
		t.Fatalf("global conflict candidate changed: %q, %v", got, err)
	}
	if got, err := os.ReadFile(siblingAuth); err != nil || !reflect.DeepEqual(got, siblingBytes) {
		t.Fatalf("sibling auth changed: %q, %v", got, err)
	}

	repeated, err := service.Clean(ctx, CleanRequest{Root: root, Sandbox: "api", Confirmed: true})
	if err != nil || repeated.DeletedManifests != 0 || repeated.DeletedResources != 0 {
		t.Fatalf("repeat Clean(api) = %#v, %v", repeated, err)
	}
}

func TestSandboxAuthCleanupFailureRetainsDeletedManifestForRepeat(t *testing.T) {
	service, root, manifests, fake, _ := lifecycleFixture(t)
	manifest := storeCleanupClone(t, manifests, fake, root, "retry-auth", "01890f5c-7b00-7000-8000-000000000027", []state.GitRecord{
		cleanupGitRecord(root, "retry-auth", "workspace", cleanupResultFetched),
	})
	attempts := 0
	service.cleanSandboxAuth = func(context.Context, model.ProjectID, model.SandboxName) error {
		attempts++
		if attempts == 1 {
			return model.NewError(model.CodeConflict, "sandbox auth has an active run copy", nil)
		}
		return nil
	}

	first, err := service.Clean(context.Background(), CleanRequest{Root: root, Sandbox: "retry-auth", Confirmed: true})
	if model.ErrorCodeOf(err) != model.CodeConflict || first.DeletedResources != 1 || first.DeletedManifests != 0 {
		t.Fatalf("first Clean(retry-auth) = %#v, %v", first, err)
	}
	if len(fake.resources) != 0 {
		t.Fatalf("first cleanup did not prove runtime deletion: %#v", fake.resources)
	}
	stored, found, loadErr := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if loadErr != nil || !found || stored.State != model.StateDeleted {
		t.Fatalf("auth failure did not retain deleted manifest: found=%t state=%q err=%v", found, stored.State, loadErr)
	}

	second, err := service.Clean(context.Background(), CleanRequest{Root: root, Sandbox: "retry-auth", Confirmed: true})
	if err != nil || second.DeletedResources != 0 || second.DeletedManifests != 1 || attempts != 2 {
		t.Fatalf("repeat Clean(retry-auth) = %#v, attempts=%d, %v", second, attempts, err)
	}
	if _, found, loadErr := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID); loadErr != nil || found {
		t.Fatalf("repeat cleanup retained manifest: found=%t err=%v", found, loadErr)
	}
}

func TestNamedLifecycleInvalidAndMainSelectorsFailBeforeMutation(t *testing.T) {
	service, root, manifests, fake, _ := lifecycleFixture(t)
	manifest := markCleanupCloneRunning(t, manifests, storeCleanupClone(t, manifests, fake, root, "safe", "01890f5c-7b00-7000-8000-000000000024", []state.GitRecord{
		cleanupGitRecord(root, "safe", "workspace", cleanupResultFetched),
	}))
	beforeCalls := append([]string(nil), fake.calls...)
	beforeResources := cloneRuntimeResources(fake.resources)
	requests := []func() error{
		func() error {
			_, err := service.Stop(context.Background(), StopRequest{Root: root, Sandbox: "main"})
			return err
		},
		func() error {
			_, err := service.Stop(context.Background(), StopRequest{Root: root, Sandbox: "../bad"})
			return err
		},
		func() error {
			_, err := service.Clean(context.Background(), CleanRequest{Root: root, Sandbox: "main", Confirmed: true})
			return err
		},
		func() error {
			_, err := service.Clean(context.Background(), CleanRequest{Root: root, Sandbox: "../bad", Confirmed: true})
			return err
		},
	}
	for _, request := range requests {
		if err := request(); model.ErrorCodeOf(err) != model.CodeInvalidInput {
			t.Fatalf("selector error = %v (code %q)", err, model.ErrorCodeOf(err))
		}
	}
	if !reflect.DeepEqual(fake.calls, beforeCalls) || !reflect.DeepEqual(fake.resources, beforeResources) {
		t.Fatalf("invalid selector mutated runtime: calls=%#v resources=%#v", fake.calls, fake.resources)
	}
	stored, found, err := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if err != nil || !found || !reflect.DeepEqual(stored, manifest) {
		t.Fatalf("invalid selector mutated manifest: found=%t stored=%#v err=%v", found, stored, err)
	}
}

func TestNamedCleanPreservesAmbiguousOwnedResourceEvenWithDiscardIntent(t *testing.T) {
	service, root, manifests, fake, _ := lifecycleFixture(t)
	manifest := storeCleanupClone(t, manifests, fake, root, "ambiguous", "01890f5c-7b00-7000-8000-000000000026", []state.GitRecord{
		cleanupGitRecord(root, "ambiguous", "workspace", cleanupResultFetched),
	})
	resourceID := runtime.ResourceID(manifest.Resources[0].RuntimeID)
	snapshot := fake.resources[resourceID]
	snapshot.Labels = nil
	fake.resources[resourceID] = snapshot

	cleaned, err := service.Clean(context.Background(), CleanRequest{
		Root: root, Sandbox: "ambiguous", Confirmed: true, DiscardUnfetched: true,
	})
	if model.ErrorCodeOf(err) != model.CodeAmbiguous {
		t.Fatalf("Clean(ambiguous) error = %v (code %q)", err, model.ErrorCodeOf(err))
	}
	if cleaned.DeletedResources != 0 || len(cleaned.Preserved) != 1 || cleaned.Preserved[0] != manifest.Resources[0].Name {
		t.Fatalf("Clean(ambiguous) = %#v", cleaned)
	}
	if _, found := fake.resources[resourceID]; !found {
		t.Fatal("ambiguous runtime resource was deleted")
	}
	if _, found, loadErr := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID); loadErr != nil || !found {
		t.Fatalf("ambiguous manifest was deleted: found=%t err=%v", found, loadErr)
	}
}

func TestCleanRecoversPendingCloneCaptureBeforeUnfetchedGuard(t *testing.T) {
	service, root, manifests, fake, manifest := pendingCleanupCaptureFixture(
		t, "recover", "01890f5c-7b00-7000-8000-000000000040", "workspace",
	)
	cleaned, err := service.Clean(context.Background(), CleanRequest{Root: root, Sandbox: "recover", Confirmed: true})
	entry := "clone sandbox recover repository workspace has unfetched result work"
	if model.ErrorCodeOf(err) != model.CodeDataLoss || !reflect.DeepEqual(cleaned.Preserved, []string{entry}) {
		t.Fatalf("recovery Clean() = %#v, %v", cleaned, err)
	}
	stored, found, loadErr := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if loadErr != nil || !found || stored.UncapturedWork || stored.State != model.StateStopped || !stored.Git[0].HasResultWork() {
		t.Fatalf("recovered manifest: found=%t manifest=%#v err=%v", found, stored, loadErr)
	}
	if _, found := fake.resources[runtime.ResourceID(manifest.Resources[0].RuntimeID)]; !found {
		t.Fatal("unfetched recovery deleted the workspace")
	}
	beforeSpecs := len(fake.specs)
	repeated, repeatErr := service.Clean(context.Background(), CleanRequest{Root: root, Sandbox: "recover", Confirmed: true})
	reloaded, reloadedFound, reloadErr := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if model.ErrorCodeOf(repeatErr) != model.CodeDataLoss || !reflect.DeepEqual(repeated.Preserved, []string{entry}) {
		t.Fatalf("repeat recovery Clean() = %#v, %v", repeated, repeatErr)
	}
	if reloadErr != nil || !reloadedFound || reloaded.UncapturedWork || reloaded.Git[0].ResultCommit != stored.Git[0].ResultCommit || len(fake.specs) <= beforeSpecs {
		t.Fatalf("repeat cleanup did not recapture stable state: found=%t manifest=%#v specs=%d/%d err=%v", reloadedFound, reloaded, len(fake.specs), beforeSpecs, reloadErr)
	}
}
func TestCleanRecapturesDelayedWriterFromQuiescedCurrentState(t *testing.T) {
	service, root, manifests, fake, manifest := pendingCleanupCaptureFixture(
		t, "delayed-writer", "01890f5c-7b00-7000-8000-000000000048", "workspace",
	)
	ctx := context.Background()
	workspaceID := runtime.ResourceID(manifest.Resources[0].RuntimeID)
	if err := service.cloneCleanupRecovery(ctx, fake.resources[workspaceID], &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Git[0].FetchedCommit = manifest.Git[0].ResultCommit
	manifest.Git[0].FetchedHostRef = gitx.RefNamespace + string(manifest.Sandbox)
	browserName := state.CanonicalResourceName(manifest.ProjectID, manifest.Sandbox, browserRole)
	browserRecord := state.ResourceRecord{
		Kind:       string(runtime.ResourceBrowser),
		Role:       browserRole,
		Name:       browserName,
		ExpectedID: browserName,
		RuntimeID:  browserName,
		Labels:     state.ResourceOwnershipLabels(manifest.ProjectID, manifest.Sandbox, manifest.RunID, string(runtime.ResourceBrowser), browserRole),
		Created:    true,
	}
	manifest.Resources = append(manifest.Resources, browserRecord)
	if err := service.replace(ctx, &manifest); err != nil {
		t.Fatal(err)
	}
	fake.resources[runtime.ResourceID(browserName)] = runtime.ResourceSnapshot{
		Resource: runtime.Resource{ID: runtime.ResourceID(browserName), Name: browserName, Kind: runtime.ResourceBrowser},
		State:    "running",
		Labels:   runtimeLabels(browserRecord.Labels),
	}
	unrelated := runtime.ResourceSnapshot{
		Resource: runtime.Resource{ID: "unrelated-delayed", Name: "unrelated-delayed", Kind: runtime.ResourceWorkspace},
		State:    "running",
	}
	fake.resources[unrelated.ID] = unrelated
	fake.calls = nil
	fake.specs = nil
	delayedCommit := strings.Repeat("7", 40)
	fake.resultCommit = delayedCommit
	fake.diffCode = 1

	cleaned, err := service.Clean(ctx, CleanRequest{Root: root, Sandbox: "delayed-writer", Confirmed: true})
	entry := "clone sandbox delayed-writer repository workspace has unfetched result work"
	if model.ErrorCodeOf(err) != model.CodeDataLoss || !reflect.DeepEqual(cleaned.Preserved, []string{entry}) {
		t.Fatalf("delayed writer Clean() = %#v, %v", cleaned, err)
	}
	stored, found, loadErr := manifests.LoadManifest(ctx, manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if loadErr != nil || !found || stored.UncapturedWork || stored.Git[0].ResultCommit != delayedCommit || stored.Git[0].ResultFetched() {
		t.Fatalf("delayed result was not guarded: found=%t manifest=%#v err=%v", found, stored, loadErr)
	}
	if fake.resources[workspaceID].State != "stopped" || fake.resources[runtime.ResourceID(browserName)].State != "stopped" {
		t.Fatalf("clone writers were not quiesced: %#v", fake.resources)
	}
	if len(fake.calls) < 4 || fake.calls[0] != "stop" || fake.calls[1] != "stop" || fake.calls[2] != "start" || fake.calls[len(fake.calls)-1] != "stop" {
		t.Fatalf("stable capture order = %#v", fake.calls)
	}
	if snapshot, exists := fake.resources[unrelated.ID]; !exists || !reflect.DeepEqual(snapshot, unrelated) {
		t.Fatalf("stable capture changed unrelated resource: found=%t snapshot=%#v", exists, snapshot)
	}

	stored.Git[0].FetchedCommit = delayedCommit
	stored.Git[0].FetchedHostRef = gitx.RefNamespace + string(stored.Sandbox)
	if err := service.replace(ctx, &stored); err != nil {
		t.Fatal(err)
	}
	service.cloneCleanupFetchedVerifier = func(context.Context, state.Manifest) error { return nil }
	fake.diffCode = 0
	deleted, err := service.Clean(ctx, CleanRequest{Root: root, Sandbox: "delayed-writer", Confirmed: true})
	if err != nil || deleted.DeletedManifests != 1 || deleted.DeletedResources != 2 {
		t.Fatalf("fetched stable result Clean() = %#v, %v", deleted, err)
	}
	if snapshot, exists := fake.resources[unrelated.ID]; !exists || !reflect.DeepEqual(snapshot, unrelated) {
		t.Fatalf("cleanup changed unrelated resource: found=%t snapshot=%#v", exists, snapshot)
	}
}

func TestNamedCleanCannotEnterActiveCloneSandboxLease(t *testing.T) {
	service, root, manifests, fake, _ := lifecycleFixture(t)
	manifest := storeCleanupClone(t, manifests, fake, root, "active-clean", "01890f5c-7b00-7000-8000-000000000049", []state.GitRecord{
		cleanupGitRecord(root, "active-clean", "workspace", cleanupResultNone),
	})
	holder, err := manifests.LockSandbox(context.Background(), manifest.ProjectID, manifest.Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := holder.Unlock(); err != nil {
			t.Error(err)
		}
	}()
	beforeResources := cloneRuntimeResources(fake.resources)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	cleaned, err := service.Clean(ctx, CleanRequest{Root: root, Sandbox: "active-clean", Confirmed: true})
	if model.ErrorCodeOf(err) != model.CodeConflict || cleaned.DeletedManifests != 0 || cleaned.DeletedResources != 0 {
		t.Fatalf("active lease Clean() = %#v, %v", cleaned, err)
	}
	if len(fake.calls) != 0 || !reflect.DeepEqual(fake.resources, beforeResources) {
		t.Fatalf("active lease cleanup mutated runtime: calls=%#v resources=%#v", fake.calls, fake.resources)
	}
}

func TestCleanPendingCaptureUnavailableOrAmbiguousFailsClosed(t *testing.T) {
	tests := map[string]struct {
		arrange func(*cloneRecordingRuntime, state.Manifest)
		reason  string
	}{
		"unavailable": {
			arrange: func(fake *cloneRecordingRuntime, manifest state.Manifest) {
				delete(fake.resources, runtime.ResourceID(manifest.Resources[0].RuntimeID))
			},
			reason: "workspace is unavailable",
		},
		"ambiguous": {
			arrange: func(fake *cloneRecordingRuntime, manifest state.Manifest) {
				id := runtime.ResourceID(manifest.Resources[0].RuntimeID)
				snapshot := fake.resources[id]
				snapshot.Labels = nil
				fake.resources[id] = snapshot
			},
			reason: "ownership is ambiguous",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			service, root, manifests, fake, manifest := pendingCleanupCaptureFixture(
				t, model.SandboxName(name), "01890f5c-7b00-7000-8000-000000000041", "workspace",
			)
			test.arrange(fake, manifest)
			beforeResources := cloneRuntimeResources(fake.resources)
			cleaned, err := service.Clean(context.Background(), CleanRequest{
				Root: root, Sandbox: name, Confirmed: true,
			})
			if model.ErrorCodeOf(err) != model.CodeDataLoss || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("Clean() error = %v (code %q)", err, model.ErrorCodeOf(err))
			}
			uncaptured := "clone sandbox " + name + " has uncaptured repository work"
			if cleaned.DeletedManifests != 0 || cleaned.DeletedResources != 0 || !reflect.DeepEqual(cleaned.Preserved, []string{uncaptured}) {
				t.Fatalf("failed-closed Clean() = %#v", cleaned)
			}
			stored, found, loadErr := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
			if loadErr != nil || !found || !stored.UncapturedWork || !reflect.DeepEqual(stored.Git, manifest.Git) {
				t.Fatalf("failed-closed manifest: found=%t manifest=%#v err=%v", found, stored, loadErr)
			}
			if !reflect.DeepEqual(fake.resources, beforeResources) || len(fake.specs) != 0 {
				t.Fatalf("failed-closed recovery mutated runtime: resources=%#v specs=%#v", fake.resources, fake.specs)
			}
		})
	}
}

func TestCleanPendingCompositeCapturePersistsPartialRetry(t *testing.T) {
	service, root, manifests, fake, manifest := pendingCleanupCaptureFixture(
		t, "partial-clean", "01890f5c-7b00-7000-8000-000000000042", "api", "web",
	)
	injected := errors.New("second repository recovery failed")
	fake.failWorkingDirectory = "/workspace/services/web"
	fake.failErr = injected
	first, err := service.Clean(context.Background(), CleanRequest{Root: root, Sandbox: "partial-clean", Confirmed: true})
	if !errors.Is(err, injected) || first.DeletedManifests != 0 || first.DeletedResources != 0 {
		t.Fatalf("partial recovery Clean() = %#v, %v", first, err)
	}
	stored, found, loadErr := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if loadErr != nil || !found || !stored.UncapturedWork || !stored.Git[0].HasResultWork() || stored.Git[1].HasResultWork() {
		t.Fatalf("partial recovery was not durable: found=%t manifest=%#v err=%v", found, stored, loadErr)
	}
	fake.failErr = nil
	second, err := service.Clean(context.Background(), CleanRequest{Root: root, Sandbox: "partial-clean", Confirmed: true})
	if model.ErrorCodeOf(err) != model.CodeDataLoss || len(second.Preserved) != 2 {
		t.Fatalf("retried composite Clean() = %#v, %v", second, err)
	}
	stored, found, loadErr = manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if loadErr != nil || !found || stored.UncapturedWork || !stored.Git[0].HasResultWork() || !stored.Git[1].HasResultWork() {
		t.Fatalf("retried composite capture: found=%t manifest=%#v err=%v", found, stored, loadErr)
	}
}

func TestCleanPendingNoopCaptureClearsUncertainty(t *testing.T) {
	service, root, manifests, fake, manifest := pendingCleanupCaptureFixture(
		t, "noop-clean", "01890f5c-7b00-7000-8000-000000000043", "workspace",
	)
	fake.diffCode = 0
	fake.resultCommit = manifest.Git[0].SourceCommit
	injected := errors.New("retain recovered no-op manifest")
	service.cleanSandboxAuth = func(context.Context, model.ProjectID, model.SandboxName) error { return injected }
	cleaned, err := service.Clean(context.Background(), CleanRequest{Root: root, Sandbox: "noop-clean", Confirmed: true})
	if !errors.Is(err, injected) || cleaned.DeletedManifests != 0 || cleaned.DeletedResources != 1 || len(cleaned.Preserved) != 0 {
		t.Fatalf("no-op recovery Clean() = %#v, %v", cleaned, err)
	}
	stored, found, loadErr := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if loadErr != nil || !found || stored.UncapturedWork || stored.Git[0].HasResultWork() || stored.State != model.StateDeleted {
		t.Fatalf("no-op recovery uncertainty: found=%t manifest=%#v err=%v", found, stored, loadErr)
	}
	if len(fake.resources) != 0 {
		t.Fatalf("no-op recovery retained runtime: %#v", fake.resources)
	}
	service.cleanSandboxAuth = nil
	repeated, repeatErr := service.Clean(context.Background(), CleanRequest{Root: root, Sandbox: "noop-clean", Confirmed: true})
	if repeatErr != nil || repeated.DeletedManifests != 1 || repeated.DeletedResources != 0 {
		t.Fatalf("repeat no-op cleanup = %#v, %v", repeated, repeatErr)
	}
}

func TestExplicitDiscardDeletesPendingCloneWhenRuntimeIsUnavailable(t *testing.T) {
	service, root, manifests, fake, manifest := pendingCleanupCaptureFixture(
		t, "unavailable-discard", "01890f5c-7b00-7000-8000-000000000044", "workspace",
	)
	delete(fake.resources, runtime.ResourceID(manifest.Resources[0].RuntimeID))

	cleaned, err := service.Clean(context.Background(), CleanRequest{
		Root: root, Sandbox: "unavailable-discard", Confirmed: true, DiscardUnfetched: true,
	})
	if err != nil || cleaned.DeletedManifests != 1 || cleaned.DeletedResources != 0 || len(cleaned.Preserved) != 0 {
		t.Fatalf("unavailable explicit discard Clean() = %#v, %v", cleaned, err)
	}
	if _, found, loadErr := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID); loadErr != nil || found {
		t.Fatalf("unavailable explicit discard preserved manifest: found=%t err=%v", found, loadErr)
	}
	if len(fake.specs) != 0 {
		t.Fatalf("unavailable explicit discard attempted guest recovery: %#v", fake.specs)
	}
}

func TestExplicitDiscardSkipsPendingCaptureAndPreservesUnrelatedResource(t *testing.T) {
	service, root, manifests, fake, manifest := pendingCleanupCaptureFixture(
		t, "discard", "01890f5c-7b00-7000-8000-000000000025", "workspace",
	)
	fake.failCommandContains = " add -A"
	fake.failErr = errors.New("capture must not run")
	unrelated := runtime.ResourceSnapshot{
		Resource: runtime.Resource{ID: "unrelated", Name: "unrelated", Kind: runtime.ResourceWorkspace},
		State:    "running",
	}
	fake.resources[unrelated.ID] = unrelated

	cleaned, err := service.Clean(context.Background(), CleanRequest{
		Root: root, Sandbox: "discard", Confirmed: true, DiscardUnfetched: true,
	})
	if err != nil || cleaned.DeletedManifests != 1 || cleaned.DeletedResources != 1 || len(cleaned.Preserved) != 0 {
		t.Fatalf("explicit discard Clean() = %#v, %v", cleaned, err)
	}
	if _, found, loadErr := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID); loadErr != nil || found {
		t.Fatalf("explicit discard preserved manifest: found=%t err=%v", found, loadErr)
	}
	if snapshot, found := fake.resources[unrelated.ID]; !found || !reflect.DeepEqual(snapshot, unrelated) {
		t.Fatalf("explicit discard changed unrelated resource: found=%t snapshot=%#v", found, snapshot)
	}
	if len(fake.specs) != 0 {
		t.Fatalf("explicit discard attempted guest capture: %#v", fake.specs)
	}
}
func TestFailedUncapturedCloneStopRetainsUncertaintyAndAllowsCleanupRecovery(t *testing.T) {
	service, root, manifests, fake, manifest := pendingCleanupCaptureFixture(
		t, "failed-stop", "01890f5c-7b00-7000-8000-000000000045", "workspace",
	)
	if err := service.transition(context.Background(), &manifest, model.StateFailed, "capture", "simulated crash"); err != nil {
		t.Fatal(err)
	}
	stopped, err := service.Stop(context.Background(), StopRequest{Root: root, Sandbox: "failed-stop"})
	if err != nil || stopped.State != model.StateFailed {
		t.Fatalf("Stop() = %#v, %v", stopped, err)
	}
	id := runtime.ResourceID(manifest.Resources[0].RuntimeID)
	stored, found, loadErr := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if loadErr != nil || !found || stored.State != model.StateFailed || !stored.UncapturedWork || fake.resources[id].State != "stopped" {
		t.Fatalf("stopped failed clone: found=%t manifest=%#v runtime=%#v err=%v", found, stored, fake.resources[id], loadErr)
	}
	repeated, repeatErr := service.Stop(context.Background(), StopRequest{Root: root, Sandbox: "failed-stop"})
	if repeatErr != nil || repeated.State != model.StateFailed {
		t.Fatalf("repeat Stop() = %#v, %v", repeated, repeatErr)
	}
	cleaned, cleanErr := service.Clean(context.Background(), CleanRequest{Root: root, Sandbox: "failed-stop", Confirmed: true})
	if model.ErrorCodeOf(cleanErr) != model.CodeDataLoss || cleaned.DeletedResources != 0 {
		t.Fatalf("recovery Clean() = %#v, %v", cleaned, cleanErr)
	}
	stored, found, loadErr = manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if loadErr != nil || !found || stored.State != model.StateFailed || stored.UncapturedWork || !stored.Git[0].HasResultWork() || fake.resources[id].State != "stopped" {
		t.Fatalf("recovered stopped clone: found=%t manifest=%#v runtime=%#v err=%v", found, stored, fake.resources[id], loadErr)
	}
}

func TestFetchedCleanupVerifiesExactHostRefUnlessDiscarded(t *testing.T) {
	tests := []string{"moved", "missing"}
	for index, name := range tests {
		t.Run(name, func(t *testing.T) {
			service, root, manifests, fake, _ := lifecycleFixture(t)
			sandbox := model.SandboxName(name + "-ref")
			runID := model.RunID(fmt.Sprintf("01890f5c-7b00-7000-8000-00000000005%d", index))
			manifest := storeCleanupClone(t, manifests, fake, root, sandbox, runID, []state.GitRecord{
				cleanupGitRecord(root, sandbox, "workspace", cleanupResultFetched),
			})
			git := &cloneGitStub{status: gitx.Status{Fetched: false}}
			if _, err := NewCloneService(CloneDependencies{
				Lifecycle: service, Harness: &HarnessService{}, Git: git, TempRoot: t.TempDir(),
			}); err != nil {
				t.Fatal(err)
			}
			service.cloneCleanupRecovery = func(context.Context, runtime.ResourceSnapshot, *state.Manifest) error { return nil }
			cleaned, err := service.Clean(context.Background(), CleanRequest{Root: root, Sandbox: string(sandbox), Confirmed: true})
			if model.ErrorCodeOf(err) != model.CodeDataLoss || !strings.Contains(err.Error(), "host ref moved or is missing") {
				t.Fatalf("tampered ref Clean() = %#v, %v", cleaned, err)
			}
			stored, found, loadErr := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
			snapshot := fake.resources[runtime.ResourceID(manifest.Resources[0].RuntimeID)]
			if loadErr != nil || !found || !reflect.DeepEqual(stored, manifest) || snapshot.State != "stopped" {
				t.Fatalf("tampered ref cleanup did not retain quiesced ownership: found=%t manifest=%#v resources=%#v err=%v", found, stored, fake.resources, loadErr)
			}
			discarded, discardErr := service.Clean(context.Background(), CleanRequest{
				Root: root, Sandbox: string(sandbox), Confirmed: true, DiscardUnfetched: true,
			})
			if discardErr != nil || discarded.DeletedManifests != 1 || discarded.DeletedResources != 1 {
				t.Fatalf("explicit ref discard Clean() = %#v, %v", discarded, discardErr)
			}
		})
	}
}

func TestFetchedCleanupAcceptsHostRefBoundToResultCommit(t *testing.T) {
	service, root, manifests, fake, _ := lifecycleFixture(t)
	sandbox := model.SandboxName("bound-ref")
	storeCleanupClone(t, manifests, fake, root, sandbox, "01890f5c-7b00-7000-8000-000000000052", []state.GitRecord{
		cleanupGitRecord(root, sandbox, "workspace", cleanupResultFetched),
	})
	if _, err := NewCloneService(CloneDependencies{
		Lifecycle: service,
		Harness:   &HarnessService{},
		Git:       &cloneGitStub{status: gitx.Status{Fetched: true}},
		TempRoot:  t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	service.cloneCleanupRecovery = func(context.Context, runtime.ResourceSnapshot, *state.Manifest) error { return nil }
	cleaned, err := service.Clean(context.Background(), CleanRequest{Root: root, Sandbox: "bound-ref", Confirmed: true})
	if err != nil || cleaned.DeletedManifests != 1 || cleaned.DeletedResources != 1 {
		t.Fatalf("bound ref Clean() = %#v, %v", cleaned, err)
	}
}
func TestCleanupRepositoryIdentityChangeFailsBeforeDiscardMutation(t *testing.T) {
	service, root, manifests, fake, _ := lifecycleFixture(t)
	sandbox := model.SandboxName("identity-change")
	manifest := storeCleanupClone(t, manifests, fake, root, sandbox, "01890f5c-7b00-7000-8000-000000000055", []state.GitRecord{
		cleanupGitRecord(root, sandbox, "workspace", cleanupResultNone),
	})
	injected := errors.New("repository directory identity changed")
	if _, err := NewCloneService(CloneDependencies{
		Lifecycle: service,
		Harness:   &HarnessService{},
		Git:       &cloneGitStub{validateErr: injected},
		TempRoot:  t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	authPurges := 0
	service.cleanSandboxAuth = func(context.Context, model.ProjectID, model.SandboxName) error {
		authPurges++
		return nil
	}
	beforeCalls := append([]string(nil), fake.calls...)
	beforeResources := cloneRuntimeResources(fake.resources)
	cleaned, err := service.Clean(context.Background(), CleanRequest{
		Root: root, Sandbox: "identity-change", Confirmed: true, DiscardUnfetched: true,
	})
	if model.ErrorCodeOf(err) != model.CodeAmbiguous || !errors.Is(err, injected) {
		t.Fatalf("identity-changed Clean() = %#v, %v", cleaned, err)
	}
	stored, found, loadErr := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if loadErr != nil || !found || !reflect.DeepEqual(stored, manifest) {
		t.Fatalf("identity guard mutated manifest: found=%t manifest=%#v err=%v", found, stored, loadErr)
	}
	if !reflect.DeepEqual(fake.calls, beforeCalls) || !reflect.DeepEqual(fake.resources, beforeResources) || authPurges != 0 {
		t.Fatalf("identity guard mutated cleanup state: calls=%#v resources=%#v purges=%d", fake.calls, fake.resources, authPurges)
	}
}

func TestProjectCleanupDoesNotPurgeAuthForStaleSameNameManifest(t *testing.T) {
	service, root, manifests, fake, _ := lifecycleFixture(t)
	sandbox := model.SandboxName("duplicate")
	stale := storeCleanupClone(t, manifests, fake, root, sandbox, "01890f5c-7b00-7000-8000-000000000053", []state.GitRecord{
		cleanupGitRecord(root, sandbox, "workspace", cleanupResultFetched),
	})
	if err := service.transition(context.Background(), &stale, model.StateCleaning, "clean", ""); err != nil {
		t.Fatal(err)
	}
	delete(fake.resources, runtime.ResourceID(stale.Resources[0].RuntimeID))
	stale.Resources[0].Deleted = true
	if err := service.transition(context.Background(), &stale, model.StateDeleted, "clean", ""); err != nil {
		t.Fatal(err)
	}
	current := storeCleanupClone(t, manifests, fake, root, sandbox, "01890f5c-7b00-7000-8000-000000000054", []state.GitRecord{
		cleanupGitRecord(root, sandbox, "workspace", cleanupResultUnfetched),
	})
	authPurges := 0
	service.cleanSandboxAuth = func(context.Context, model.ProjectID, model.SandboxName) error {
		authPurges++
		return nil
	}
	cleaned, err := service.Clean(context.Background(), CleanRequest{Root: root, Confirmed: true})
	if err == nil || !strings.Contains(err.Error(), "another nondeleted lifecycle manifest") || authPurges != 0 {
		t.Fatalf("duplicate project Clean() = %#v, purges=%d, %v", cleaned, authPurges, err)
	}
	for _, manifest := range []state.Manifest{stale, current} {
		if _, found, loadErr := manifests.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID); loadErr != nil || !found {
			t.Fatalf("duplicate cleanup removed run %s: found=%t err=%v", manifest.RunID, found, loadErr)
		}
	}
	if _, found := fake.resources[runtime.ResourceID(current.Resources[0].RuntimeID)]; !found {
		t.Fatal("duplicate cleanup removed current workspace")
	}
}

func TestCleanAllPreservesGuardedCloneAndCleansEligibleProject(t *testing.T) {
	service, guardedRoot, manifests, fake, _ := lifecycleFixture(t)
	eligibleRoot := canonicalCleanupRoot(t)
	guardedSandbox := model.SandboxName("guarded")
	eligibleSandbox := model.SandboxName("eligible")
	guarded := storeCleanupClone(t, manifests, fake, guardedRoot, guardedSandbox, "01890f5c-7b00-7000-8000-000000000013", []state.GitRecord{
		cleanupGitRecord(guardedRoot, guardedSandbox, "workspace", cleanupResultUnfetched),
	})
	eligible := storeCleanupClone(t, manifests, fake, eligibleRoot, eligibleSandbox, "01890f5c-7b00-7000-8000-000000000014", []state.GitRecord{
		cleanupGitRecord(eligibleRoot, eligibleSandbox, "workspace", cleanupResultFetched),
	})

	cleaned, err := service.Clean(context.Background(), CleanRequest{All: true, Confirmed: true})
	if model.ErrorCodeOf(err) != model.CodeDataLoss {
		t.Fatalf("Clean(All) error = %v (code %q)", err, model.ErrorCodeOf(err))
	}
	entry := "clone sandbox guarded repository workspace has unfetched result work"
	if cleaned.Projects != 2 || cleaned.DeletedManifests != 1 || cleaned.DeletedResources != 1 || !reflect.DeepEqual(cleaned.Preserved, []string{entry}) {
		t.Fatalf("Clean(All) = %#v", cleaned)
	}
	if _, found, loadErr := manifests.LoadManifest(context.Background(), guarded.ProjectID, guarded.Sandbox, guarded.RunID); loadErr != nil || !found {
		t.Fatalf("guarded manifest not preserved: found=%t err=%v", found, loadErr)
	}
	if _, found, loadErr := manifests.LoadManifest(context.Background(), eligible.ProjectID, eligible.Sandbox, eligible.RunID); loadErr != nil || found {
		t.Fatalf("eligible manifest not deleted: found=%t err=%v", found, loadErr)
	}
	if len(fake.resources) != 1 {
		t.Fatalf("Clean(All) runtime resources = %#v", fake.resources)
	}
	if _, found := fake.resources[runtime.ResourceID(guarded.Resources[0].RuntimeID)]; !found {
		t.Fatal("guarded clone resource was not preserved")
	}
}

func TestFetchedCloneCleanupIsRepeatSafeAndPreservesUnrelatedResource(t *testing.T) {
	service, root, manifests, fake, _ := lifecycleFixture(t)
	sandbox := model.SandboxName("repeat")
	storeCleanupClone(t, manifests, fake, root, sandbox, "01890f5c-7b00-7000-8000-000000000015", []state.GitRecord{
		cleanupGitRecord(root, sandbox, "workspace", cleanupResultFetched),
	})
	unrelated := runtime.ResourceSnapshot{Resource: runtime.Resource{ID: "unrelated", Name: "unrelated", Kind: runtime.ResourceNetwork}, State: "running"}
	fake.resources[unrelated.ID] = unrelated

	first, err := service.Clean(context.Background(), CleanRequest{Root: root, Confirmed: true})
	if err != nil || first.DeletedManifests != 1 || first.DeletedResources != 1 {
		t.Fatalf("first Clean() = %#v, %v", first, err)
	}
	second, err := service.Clean(context.Background(), CleanRequest{Root: root, Confirmed: true})
	if err != nil || second.DeletedManifests != 0 || second.DeletedResources != 0 {
		t.Fatalf("second Clean() = %#v, %v", second, err)
	}
	if snapshot, found := fake.resources[unrelated.ID]; !found || !reflect.DeepEqual(snapshot, unrelated) {
		t.Fatalf("unrelated resource changed: found=%t snapshot=%#v", found, snapshot)
	}
}

func TestListWarnsAboutUnfetchedCloneWithoutCommitContent(t *testing.T) {
	service, root, manifests, fake, _ := lifecycleFixture(t)
	sandbox := model.SandboxName("status")
	record := cleanupGitRecord(root, sandbox, "workspace", cleanupResultMismatched)
	storeCleanupClone(t, manifests, fake, root, sandbox, "01890f5c-7b00-7000-8000-000000000016", []state.GitRecord{record})

	listed, err := service.List(context.Background(), ListRequest{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	warning := "clone sandbox status repository workspace has unfetched result work"
	if len(listed.Sandboxes) != 1 || !reflect.DeepEqual(listed.Sandboxes[0].Warnings, []string{warning}) {
		t.Fatalf("List() = %#v", listed)
	}
	if strings.Contains(listed.Sandboxes[0].Warnings[0], record.ResultCommit) || strings.Contains(listed.Sandboxes[0].Warnings[0], record.FetchedCommit) {
		t.Fatalf("warning leaked commit content: %q", listed.Sandboxes[0].Warnings[0])
	}
}

func pendingCleanupCaptureFixture(t *testing.T, sandbox model.SandboxName, runID model.RunID, repositories ...string) (*LifecycleService, string, *statefs.ManifestRepository, *cloneRecordingRuntime, state.Manifest) {
	t.Helper()
	service, root, manifests, base, _ := lifecycleFixture(t)
	service.guest = &lifecycleGuest{runtime: base}
	records := make([]state.GitRecord, 0, len(repositories))
	for _, repository := range repositories {
		records = append(records, cleanupGitRecord(root, sandbox, repository, cleanupResultNone))
	}
	manifest := storeCleanupClone(t, manifests, base, root, sandbox, runID, records)
	fake := &cloneRecordingRuntime{lifecycleRuntime: base, diffCode: 1, resultCommit: cloneTestResult}
	service.runtime = fake
	repository, err := auth.NewRepository(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	harnessService, err := NewHarnessService(service, repository)
	if err != nil {
		t.Fatal(err)
	}
	clones, err := NewCloneService(CloneDependencies{
		Lifecycle: service,
		Harness:   harnessService,
		Git: &cloneGitStub{status: gitx.Status{
			HostCommit: cloneTestCommit, HostTrackedClean: true, HostTrackedFingerprint: cloneTestFingerprint,
		}},
		TempRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.transition(context.Background(), &manifest, model.StateCreating, "create", ""); err != nil {
		t.Fatal(err)
	}
	if err := clones.markCapturePending(context.Background(), &manifest); err != nil {
		t.Fatal(err)
	}
	return service, root, manifests, fake, manifest
}

type cleanupResultState int

const (
	cleanupResultNone cleanupResultState = iota
	cleanupResultUnfetched
	cleanupResultFetched
	cleanupResultMismatched
)

func cleanupGitRecord(root string, sandbox model.SandboxName, repository string, result cleanupResultState) state.GitRecord {
	hostPath := root
	guestPath := "/workspace"
	if repository != "workspace" {
		hostPath += "/services/" + repository
		guestPath += "/services/" + repository
	}
	record := state.GitRecord{
		Repository:         repository,
		HostPath:           hostPath,
		GuestPath:          guestPath,
		Identity:           cleanupRepositoryIdentity(root, hostPath),
		SourceRef:          "refs/heads/main",
		SourceCommit:       strings.Repeat("1", 40),
		TrackedFingerprint: strings.Repeat("2", 64),
		ResultBranch:       "dsx/" + string(sandbox),
		SourceBundleDigest: strings.Repeat("3", 64),
	}
	if result == cleanupResultNone {
		return record
	}
	record.ResultCommit = strings.Repeat("4", 40)
	record.ResultBundleDigest = strings.Repeat("5", 64)
	if result == cleanupResultFetched || result == cleanupResultMismatched {
		record.FetchedCommit = record.ResultCommit
		if result == cleanupResultMismatched {
			record.FetchedCommit = strings.Repeat("6", 40)
		}
		record.FetchedHostRef = gitx.RefNamespace + string(sandbox)
	}
	return record
}
func cleanupRepositoryIdentity(root, worktree string) gitx.RepositoryIdentity {
	return gitx.RepositoryIdentity{
		ApprovedRoot: cleanupPhysicalPathIdentity(root),
		Worktree:     cleanupPhysicalPathIdentity(worktree),
		GitDir:       cleanupPhysicalPathIdentity(filepath.Join(worktree, ".git")),
	}
}

func cleanupPhysicalPathIdentity(value string) gitx.PhysicalPathIdentity {
	parts := strings.Split(strings.TrimPrefix(value, string(filepath.Separator)), string(filepath.Separator))
	components := []gitx.PathComponentIdentity{{Path: string(filepath.Separator), Device: 1, Inode: 1}}
	current := string(filepath.Separator)
	for index, part := range parts {
		current = filepath.Join(current, part)
		components = append(components, gitx.PathComponentIdentity{Path: current, Device: 1, Inode: uint64(index + 2)})
	}
	return gitx.PhysicalPathIdentity{CanonicalPath: value, Components: components}
}

func storeCleanupClone(t *testing.T, manifests *statefs.ManifestRepository, fake *lifecycleRuntime, root string, sandbox model.SandboxName, runID model.RunID, records []state.GitRecord) state.Manifest {
	t.Helper()
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	resource := state.ResourceRecord{
		Kind:       string(runtime.ResourceWorkspace),
		Role:       workspaceRole,
		Name:       state.CanonicalResourceName(projectID, sandbox, workspaceRole),
		ExpectedID: state.CanonicalResourceName(projectID, sandbox, workspaceRole),
		Labels:     state.ResourceOwnershipLabels(projectID, sandbox, runID, string(runtime.ResourceWorkspace), workspaceRole),
		Created:    true,
	}
	resource.RuntimeID = resource.ExpectedID
	manifest := state.Manifest{
		Version:       state.ManifestVersion,
		Generation:    1,
		ProjectID:     projectID,
		CanonicalRoot: root,
		Sandbox:       sandbox,
		RunID:         runID,
		Mode:          model.ModeClone,
		PlanHash:      strings.Repeat("a", 64),
		State:         model.StatePlanned,
		Operation:     "create",
		Resources:     []state.ResourceRecord{resource},
		Git:           records,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := manifests.CreateIntent(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	fake.resources[runtime.ResourceID(resource.RuntimeID)] = runtime.ResourceSnapshot{
		Resource: runtime.Resource{ID: runtime.ResourceID(resource.RuntimeID), Name: resource.Name, Kind: runtime.ResourceWorkspace},
		State:    "running",
		Labels:   runtimeLabels(resource.Labels),
	}
	return manifest
}

func markCleanupCloneRunning(t *testing.T, manifests *statefs.ManifestRepository, manifest state.Manifest) state.Manifest {
	t.Helper()
	replacement := manifest
	replacement.State = model.StateCreating
	if err := manifests.ReplaceManifest(context.Background(), replacement, replacement.Generation); err != nil {
		t.Fatalf("%v: %v", err, errors.Unwrap(err))
	}
	replacement.Generation++
	replacement.State = model.StateRunning
	if err := manifests.ReplaceManifest(context.Background(), replacement, replacement.Generation); err != nil {
		t.Fatalf("%v: %v", err, errors.Unwrap(err))
	}
	replacement.Generation++
	return replacement
}

func cloneRuntimeResources(resources map[runtime.ResourceID]runtime.ResourceSnapshot) map[runtime.ResourceID]runtime.ResourceSnapshot {
	cloned := make(map[runtime.ResourceID]runtime.ResourceSnapshot, len(resources))
	for id, resource := range resources {
		cloned[id] = resource
	}
	return cloned
}

func canonicalCleanupRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}
