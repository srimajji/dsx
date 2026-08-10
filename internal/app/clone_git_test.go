package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/plan"
	"github.com/srimajji/dsx/internal/state"
)

func TestSelectedGitIndexesUsesExactManifestMember(t *testing.T) {
	manifest := state.Manifest{Sandbox: "task", Git: []state.GitRecord{{Repository: "api"}, {Repository: "web"}}}
	indexes, err := selectedGitIndexes(manifest, "web")
	if err != nil || !reflect.DeepEqual(indexes, []int{1}) {
		t.Fatalf("selected indexes = %#v, %v", indexes, err)
	}
	all, err := selectedGitIndexes(manifest, "")
	if err != nil || !reflect.DeepEqual(all, []int{0, 1}) {
		t.Fatalf("all indexes = %#v, %v", all, err)
	}
	if _, err := selectedGitIndexes(manifest, "Web"); model.ErrorCodeOf(err) != model.CodeInvalidInput {
		t.Fatalf("case-mismatched selector error = %v", err)
	}
}

func TestCloneCompositeApplyPreparesAllAndRollsBackEveryMutation(t *testing.T) {
	lifecycle, root, manifests, _, _ := lifecycleFixture(t)
	events := make([]string, 0, 6)
	failure := errors.New("member two apply failed")
	git := &cloneGitStub{prepareApply: func(_ context.Context, request gitx.ApplyRequest) (gitx.ApplyTransaction, error) {
		name := request.Repository.Name
		events = append(events, "prepare "+name)
		transaction := &recordingApplyTransaction{name: name, events: &events, result: gitx.ApplyResult{Repository: name, AppliedCommit: cloneTestResult}}
		if name == "web" {
			transaction.commitErr = failure
		}
		return transaction, nil
	}}
	service := &CloneService{lifecycle: lifecycle, harness: &HarnessService{}, git: git, tempRoot: t.TempDir()}
	execution, artifacts := clonePlanFixture(t, "atomic")
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	execution.Project = plan.ProjectIdentity{ID: projectID, CanonicalRoot: root}
	execution.Repositories[0].HostPath = root
	artifacts[0].Repository.HostPath = root
	artifacts[0].Repository.Identity = cleanupRepositoryIdentity(root, root)
	webRoot := filepath.Join(root, "web")
	if err := os.Mkdir(webRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	execution.Repositories = append(execution.Repositories, plan.RepositoryPlan{Name: "web", HostPath: webRoot, GuestPath: "/workspace/web"})
	webArtifact := artifacts[0]
	webArtifact.Repository = gitx.Repository{Name: "web", HostPath: webRoot, GuestPath: "/workspace/web", Identity: cleanupRepositoryIdentity(root, webRoot)}
	artifacts = append(artifacts, webArtifact)
	runID, _ := model.ParseRunID("01890f5c-7b00-7000-8000-000000000031")
	manifest, _, err := plannedCloneManifest(execution, runID, lifecycle.now().UTC(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifests.CreateIntent(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateCreating, "create", ""); err != nil {
		t.Fatal(err)
	}
	for index := range manifest.Git {
		manifest.Git[index].ResultCommit = cloneTestResult
		manifest.Git[index].ResultBundleDigest = cloneTestDigest
		manifest.Git[index].FetchedCommit = cloneTestResult
		manifest.Git[index].FetchedHostRef = gitx.RefNamespace + "atomic"
	}
	if err := lifecycle.replace(context.Background(), &manifest); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.transition(context.Background(), &manifest, model.StateRunning, "capture", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GitApply(context.Background(), GitApplyRequest{Root: root, Sandbox: "atomic"}); !errors.Is(err, failure) {
		t.Fatalf("GitApply() error = %v, want %v", err, failure)
	}
	want := []string{"prepare workspace", "prepare web", "commit workspace", "commit web", "rollback web", "rollback workspace"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("transaction events = %#v, want %#v", events, want)
	}
}

func TestCloneGitOperationsHoldPerSandboxLease(t *testing.T) {
	lifecycle, root, manifests, runtime, _ := lifecycleFixture(t)
	projectID, err := model.NewProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	createRunning := func(sandbox, rawRunID string) {
		t.Helper()
		execution, artifacts := clonePlanFixture(t, sandbox)
		execution.Project = plan.ProjectIdentity{ID: projectID, CanonicalRoot: root}
		execution.Repositories[0].HostPath = root
		artifacts[0].Repository.HostPath = root
		artifacts[0].Repository.Identity = cleanupRepositoryIdentity(root, root)
		runID, parseErr := model.ParseRunID(rawRunID)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		manifest, _, planErr := plannedCloneManifest(execution, runID, lifecycle.now().UTC(), artifacts)
		if planErr != nil {
			t.Fatal(planErr)
		}
		if createErr := manifests.CreateIntent(context.Background(), manifest); createErr != nil {
			t.Fatal(createErr)
		}
		if transitionErr := lifecycle.transition(context.Background(), &manifest, model.StateCreating, "create", ""); transitionErr != nil {
			t.Fatal(transitionErr)
		}
		if transitionErr := lifecycle.transition(context.Background(), &manifest, model.StateRunning, "create", ""); transitionErr != nil {
			t.Fatal(transitionErr)
		}
	}
	createRunning("first", "01890f5c-7b00-7000-8000-000000000041")
	createRunning("second", "01890f5c-7b00-7000-8000-000000000042")
	git := &cloneGitStub{status: gitx.Status{HostCommit: cloneTestCommit, HostTrackedClean: true}}
	service := &CloneService{lifecycle: lifecycle, harness: &HarnessService{}, git: git, tempRoot: t.TempDir()}
	first, err := model.ParseSandboxName("first")
	if err != nil {
		t.Fatal(err)
	}
	active, err := lifecycle.locks.LockSandbox(context.Background(), projectID, first)
	if err != nil {
		t.Fatal(err)
	}
	contending := map[string]func(context.Context) error{
		"status": func(ctx context.Context) error {
			_, err := service.GitStatus(ctx, GitStatusRequest{Root: root, Sandbox: "first"})
			return err
		},
		"diff": func(ctx context.Context) error {
			_, err := service.GitDiff(ctx, GitDiffRequest{Root: root, Sandbox: "first"})
			return err
		},
		"fetch": func(ctx context.Context) error {
			_, err := service.GitFetch(ctx, GitFetchRequest{Root: root, Sandbox: "first"})
			return err
		},
		"apply": func(ctx context.Context) error {
			_, err := service.GitApply(ctx, GitApplyRequest{Root: root, Sandbox: "first"})
			return err
		},
	}
	runtimeCallCount := len(runtime.calls)
	for operation, invoke := range contending {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		err := invoke(ctx)
		cancel()
		if model.ErrorCodeOf(err) != model.CodeUnavailable {
			t.Fatalf("same-sandbox %s error = %v, want unavailable contention", operation, err)
		}
	}
	if len(git.statusCalls) != 0 || len(git.diffCalls) != 0 || len(git.fetchCalls) != 0 || len(git.applyCalls) != 0 || len(runtime.calls) != runtimeCallCount {
		t.Fatalf("same-sandbox contention reached result mutation: git=%#v runtime=%#v", git, runtime.calls[runtimeCallCount:])
	}
	if _, err := service.GitStatus(context.Background(), GitStatusRequest{Root: root, Sandbox: "second"}); err != nil {
		t.Fatalf("different-sandbox status contended: %v", err)
	}
	if len(git.statusCalls) != 1 || git.statusCalls[0].Sandbox != "second" {
		t.Fatalf("different-sandbox status calls = %#v", git.statusCalls)
	}
	if err := active.Unlock(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GitStatus(context.Background(), GitStatusRequest{Root: root, Sandbox: "first"}); err != nil {
		t.Fatalf("same-sandbox status after release = %v", err)
	}
}

type recordingApplyTransaction struct {
	name      string
	events    *[]string
	result    gitx.ApplyResult
	commitErr error
}

func (transaction *recordingApplyTransaction) Commit(context.Context) (gitx.ApplyResult, error) {
	*transaction.events = append(*transaction.events, "commit "+transaction.name)
	return transaction.result, transaction.commitErr
}

func (transaction *recordingApplyTransaction) Rollback(context.Context) error {
	*transaction.events = append(*transaction.events, "rollback "+transaction.name)
	return nil
}
