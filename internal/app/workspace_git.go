package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/runtime"
	"github.com/srimajji/dsx/internal/state"
)

const workspaceGitCleanupTimeout = 30 * time.Second

func (service *WorkspaceService) Update(ctx context.Context, request WorkspaceUpdateRequest) (result WorkspaceResult, returnErr error) {
	access, unlock, finish, err := service.workspaceGitAccess(ctx, request.Root, request.Workspace)
	if err != nil { return result, err }
	keepRunning := false
	defer func() { returnErr = errors.Join(returnErr, finish(keepRunning), unlock()) }()
	manifest := access.Manifest
	if manifest.ActiveSession != nil {
		return result, model.NewError(model.CodeConflict, "workspace has an active session", nil)
	}
	if manifest.State == model.StateNeedsResolution || workspaceHasConflict(*manifest) {
		resolved, reconcileErr := service.reconcileWorkspaceConflict(ctx, access)
		if reconcileErr != nil {
			return result, reconcileErr
		}
		if !resolved {
			return result, model.NewError(model.CodeConflict, "workspace update is unavailable until the existing rebase is continued or aborted", nil)
		}
		return workspaceResult(*manifest), nil
	}
	if manifest.State != model.StateRunning && manifest.State != model.StateStopped {
		return result, model.NewError(model.CodeConflict, "workspace is not ready to update", nil)
	}
	artifacts := make([]gitx.SourceArtifact, 0, len(manifest.Git))
	defer func() { returnErr = errors.Join(returnErr, service.removeSourceArtifacts(artifacts)) }()
	for _, record := range manifest.Git {
		artifact, prepareErr := service.git.PrepareUpdateSource(ctx, gitx.UpdateSourceRequest{
			Repository: workspaceGitRepository(record), Workspace: string(manifest.Workspace), TempRoot: service.tempRoot,
			SourceBranch: record.SourceBranch, SourceRevision: record.SourceRevision,
		})
		if prepareErr != nil { return result, prepareErr }
		if verifyErr := service.git.VerifyBundle(ctx, artifact.BundlePath, artifact.BundleDigest); verifyErr != nil {
			return result, errors.Join(verifyErr, service.git.RemoveArtifact(artifact.BundlePath))
		}
		artifacts = append(artifacts, artifact)
	}

	stageRoot := "/tmp/dsx-update-" + string(manifest.RunID)
	if err := service.workspaceCommand(ctx, access.Workspace, []string{"/bin/mkdir", "-m", "0700", "-p", stageRoot}, "/workspace", nil, nil); err != nil { return result, err }
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workspaceGitCleanupTimeout); defer cancel()
		returnErr = errors.Join(returnErr, service.workspaceCommand(cleanupCtx, access.Workspace, []string{"/bin/rm", "-rf", "--", stageRoot}, "/workspace", nil, nil))
	}()
	execute := service.workspaceGitExecutor(access.Workspace)
	originalState := manifest.State
	for index, artifact := range artifacts {
		guestBundle := path.Join(stageRoot, fmt.Sprintf("source-%d.bundle", index))
		if err := service.runtime.CopyTo(ctx, access.Workspace, runtime.HostPath(artifact.BundlePath), runtime.GuestPath(guestBundle)); err != nil { return result, err }
		if err := service.workspaceCommand(ctx, access.Workspace, []string{"/bin/chmod", "0600", guestBundle}, "/workspace", nil, nil); err != nil { return result, err }
		record := &manifest.Git[index]
		beforeRevision, err := service.workspaceRevision(ctx, access.Workspace, record.GuestPath, record.WorkspaceBranch)
		if err != nil {
			return result, err
		}
		backupRef := "refs/dsx/backups/" + string(manifest.Workspace)
		if err := executeGitCommand(ctx, execute, record.GuestPath, []string{"update-ref", backupRef, beforeRevision}, nil, io.Discard); err != nil {
			return result, fmt.Errorf("create workspace backup ref: %w", err)
		}
		record.BackupRef = backupRef
		record.Conflict, record.ConflictSourceRevision, record.ConflictBundleDigest = true, artifact.SourceRevision, artifact.BundleDigest
		if err := service.transitionManifest(ctx, manifest, model.StateNeedsResolution, "update", "workspace update intent is pending"); err != nil {
			return result, err
		}
		keepRunning = true
		rebase, err := performWorkspaceRebase(ctx, manifest.Workspace, manifest.Git[index], artifact, guestBundle, execute)
		if err != nil {
			manifest.Failure = boundedWorkspaceFailure(err.Error())
			_ = service.replaceManifest(context.WithoutCancel(ctx), manifest)
			return result, err
		}
		record.BackupRef = rebase.BackupRef
		if rebase.Conflict {
			manifest.Operation, manifest.Failure = "", "rebase conflict requires manual resolution"
			if err := service.replaceManifest(ctx, manifest); err != nil { return result, err }
			keepRunning = true
			return workspaceResult(*manifest), nil
		}
		record.SourceBranch, record.SourceRevision = artifact.SourceBranch, artifact.SourceRevision
		record.TrackedFingerprint, record.SourceBundleDigest = artifact.TrackedFingerprint, artifact.BundleDigest
		record.WarnUntracked, record.WarnIgnored = artifact.WarnUntracked, artifact.WarnIgnored
		record.Conflict, record.ConflictSourceRevision, record.ConflictBundleDigest = false, "", ""
		record.FetchedCommit, record.FetchedHostRef = "", ""
		if rebase.ResultCommit == artifact.SourceRevision {
			record.ResultCommit, record.ResultBundleDigest = "", ""
		} else {
			digest, bundleErr := service.workspaceResultDigest(ctx, access.Workspace, *record, rebase.ResultCommit, stageRoot, index)
			if bundleErr != nil { return result, bundleErr }
			record.ResultCommit, record.ResultBundleDigest = rebase.ResultCommit, digest
		}
		if err := service.replaceManifest(ctx, manifest); err != nil { return result, err }
	}
	manifest.Operation, manifest.Failure = "", ""
	if err := service.transitionManifest(ctx, manifest, originalState, "", ""); err != nil { return result, err }
	keepRunning = originalState == model.StateRunning
	return workspaceResult(*manifest), nil
}

func (service *WorkspaceService) GitStatus(ctx context.Context, request GitStatusRequest) (result GitStatusResult, returnErr error) {
	access, unlock, finish, err := service.workspaceGitAccess(ctx, request.Root, request.Workspace)
	if err != nil { return result, err }
	defer func() { returnErr = errors.Join(returnErr, finish(false), unlock()) }()
	indexes, err := selectedWorkspaceGitIndexes(*access.Manifest, request.Repository)
	if err != nil { return result, err }
	result.ProjectID, result.Workspace = access.Manifest.ProjectID, access.Manifest.Workspace
	for _, index := range indexes {
		record := access.Manifest.Git[index]
		status, err := service.git.Status(ctx, gitx.StatusRequest{Repository: workspaceGitRepository(record), Workspace: string(access.Manifest.Workspace), SourceBranch: record.SourceBranch, SourceRevision: record.SourceRevision, WorkspaceBranch: record.WorkspaceBranch, ResultCommit: record.ResultCommit, TrackedFingerprint: record.TrackedFingerprint, FetchedCommit: record.FetchedCommit})
		if err != nil { return result, err }
		guest, err := service.workspaceWorkingState(ctx, access.Workspace, record)
		if err != nil { return result, err }
		status.WorkspaceTrackedClean, status.WorkspaceUntracked, status.RebaseInProgress = guest.trackedClean, guest.untracked, guest.rebase
		status.WarnUntracked, status.WarnIgnored = record.WarnUntracked, record.WarnIgnored
		status.Fetched = record.ResultFetched() && status.Fetched && status.FetchedCommit == record.ResultCommit
		result.Repositories = append(result.Repositories, status)
	}
	return result, nil
}

func (service *WorkspaceService) GitFetch(ctx context.Context, request GitFetchRequest) (result GitFetchResult, returnErr error) {
	access, unlock, finish, err := service.workspaceGitAccess(ctx, request.Root, request.Workspace)
	if err != nil { return result, err }
	defer func() { returnErr = errors.Join(returnErr, finish(false), unlock()) }()
	indexes, err := selectedWorkspaceGitIndexes(*access.Manifest, request.Repository)
	if err != nil { return result, err }
	result.ProjectID, result.Workspace = access.Manifest.ProjectID, access.Manifest.Workspace
	stageRoot := "/tmp/dsx-fetch-" + string(access.Manifest.RunID)
	if err := service.workspaceCommand(ctx, access.Workspace, []string{"/bin/mkdir", "-m", "0700", "-p", stageRoot}, "/workspace", nil, nil); err != nil { return result, err }
	defer func() { cleanupCtx,cancel:=context.WithTimeout(context.WithoutCancel(ctx),workspaceGitCleanupTimeout);defer cancel();returnErr=errors.Join(returnErr,service.workspaceCommand(cleanupCtx,access.Workspace,[]string{"/bin/rm","-rf","--",stageRoot},"/workspace",nil,nil)) }()
	for _, index := range indexes {
		record := &access.Manifest.Git[index]
		state, err := service.workspaceWorkingState(ctx, access.Workspace, *record)
		if err != nil { return result, err }
		if state.rebase { return result, model.NewError(model.CodeConflict, "workspace rebase must be continued or aborted before fetch", nil) }
		head, err := service.workspaceRevision(ctx, access.Workspace, record.GuestPath, record.WorkspaceBranch)
		if err != nil { return result, err }
		if head == record.SourceRevision { continue }
		bundle, digest, cleanup, err := service.copyWorkspaceResult(ctx, access.Workspace, *record, head, stageRoot, index)
		if err != nil { return result, err }
		fetched, fetchErr := service.git.FetchResult(ctx, gitx.FetchRequest{Repository: workspaceGitRepository(*record), Workspace: string(access.Manifest.Workspace), BundlePath: bundle, Digest: digest, ExpectedCommit: head})
		cleanupErr := cleanup()
		if fetchErr != nil || cleanupErr != nil { return result, errors.Join(fetchErr, cleanupErr) }
		record.ResultCommit, record.ResultBundleDigest = head, digest
		record.FetchedCommit, record.FetchedHostRef = fetched.Commit, fetched.HostRef
		if err := service.replaceManifest(ctx, access.Manifest); err != nil { return result, err }
		result.Repositories = append(result.Repositories, fetched)
	}
	return result, nil
}

func (service *WorkspaceService) GitDiff(ctx context.Context, request GitDiffRequest) (result GitDiffResult, returnErr error) {
	access, unlock, finish, err := service.workspaceGitAccess(ctx, request.Root, request.Workspace)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, finish(false), unlock()) }()
	indexes, err := selectedWorkspaceGitIndexes(*access.Manifest, request.Repository)
	if err != nil {
		return result, err
	}
	result.ProjectID, result.Workspace = access.Manifest.ProjectID, access.Manifest.Workspace
	for _, index := range indexes {
		record := access.Manifest.Git[index]
		var patch bytes.Buffer
		limit := request.MaxBytes
		if limit <= 0 {
			limit = 1 << 20
		}
		capture := &hardLimitWriter{writer: &patch, remaining: limit}
		exit, execErr := service.workspaceGitExecutor(access.Workspace)(ctx, record.GuestPath, []string{"diff", "--binary", "--no-ext-diff", "--no-textconv", record.SourceRevision, "--"}, capture, io.Discard)
		truncated := errors.Is(execErr, errWorkspaceGitOutputLimit)
		if !truncated && (execErr != nil || exit.Code == nil || *exit.Code != 0 || exit.Signal != "") {
			return result, errors.Join(execErr, model.NewError(model.CodeUnavailable, "workspace Git diff failed", nil))
		}
		if !truncated {
			err := service.appendUntrackedDiff(ctx, access.Workspace, record.GuestPath, capture)
			truncated = errors.Is(err, errWorkspaceGitOutputLimit)
			if err != nil && !truncated {
				return result, err
			}
		}
		result.Diffs = append(result.Diffs, RepositoryDiff{Repository: record.Repository, Patch: patch.Bytes(), Truncated: truncated})
	}
	return result, nil
}

func (service *WorkspaceService) GitApply(ctx context.Context, request GitApplyRequest) (result GitApplyResult, returnErr error) {
	access, unlock, finish, err := service.workspaceGitAccess(ctx, request.Root, request.Workspace)
	if err != nil { return result, err }
	defer func() { returnErr = errors.Join(returnErr, finish(false), unlock()) }()
	indexes, err := selectedWorkspaceGitIndexes(*access.Manifest, request.Repository)
	if err != nil { return result, err }
	result.ProjectID, result.Workspace = access.Manifest.ProjectID, access.Manifest.Workspace
	type prepared struct { repository string; transaction gitx.ApplyTransaction }
	transactions := make([]prepared, 0, len(indexes))
	for _, index := range indexes {
		record := access.Manifest.Git[index]
		if !record.HasResultWork() { continue }
		if !record.ResultFetched() || record.FetchedHostRef != gitx.RefNamespace+string(access.Manifest.Workspace) { return result, model.NewError(model.CodeConflict, "workspace result must be fetched before apply", nil) }
		transaction, err := service.git.PrepareApply(ctx, gitx.ApplyRequest{Repository: workspaceGitRepository(record), SourceRevision: record.SourceRevision, TrackedFingerprint: record.TrackedFingerprint, FetchedRef: record.FetchedHostRef, ExpectedCommit: record.ResultCommit})
		if err != nil { return result, err }
		transactions = append(transactions, prepared{record.Repository, transaction})
	}
	for _, item := range transactions {
		applied, err := item.transaction.Commit(ctx)
		if err != nil {
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workspaceGitCleanupTimeout)
			var rollbackErr error
			for index := len(transactions) - 1; index >= 0; index-- {
				rollbackErr = errors.Join(rollbackErr, transactions[index].transaction.Rollback(rollbackCtx))
			}
			cancel()
			return result, errors.Join(err, rollbackErr)
		}
		result.Repositories = append(result.Repositories, applied)
	}
	return result, nil
}

func (service *WorkspaceService) reconcileWorkspaceConflict(ctx context.Context, access workspaceAccess) (resolved bool, returnErr error) {
	if !workspaceHasConflict(*access.Manifest) {
		return true, nil
	}
	execute := service.workspaceGitExecutor(access.Workspace)
	stageRoot := "/tmp/dsx-reconcile-" + string(access.Manifest.RunID)
	if err := service.workspaceCommand(ctx, access.Workspace, []string{"/bin/mkdir", "-m", "0700", "-p", stageRoot}, "/workspace", nil, nil); err != nil {
		return false, err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workspaceGitCleanupTimeout)
		defer cancel()
		returnErr = errors.Join(returnErr, service.workspaceCommand(cleanupCtx, access.Workspace, []string{"/bin/rm", "-rf", "--", stageRoot}, "/workspace", nil, nil))
	}()
	for index := range access.Manifest.Git {
		record := &access.Manifest.Git[index]
		if !record.Conflict {
			continue
		}
		resolution, err := inspectWorkspaceRebaseResolution(ctx, execute, *record)
		if err != nil {
			return false, err
		}
		if resolution.Pending {
			return false, nil
		}
		if resolution.Aborted {
			record.Conflict, record.ConflictSourceRevision, record.ConflictBundleDigest = false, "", ""
			continue
		}
		head := resolution.Head
		artifact, err := service.git.PrepareUpdateSource(ctx, gitx.UpdateSourceRequest{
			Repository: workspaceGitRepository(*record), Workspace: string(access.Manifest.Workspace), TempRoot: service.tempRoot,
			SourceBranch: record.SourceBranch, SourceRevision: record.SourceRevision,
		})
		if err != nil {
			return false, err
		}
		defer func() { returnErr = errors.Join(returnErr, service.git.RemoveArtifact(artifact.BundlePath)) }()
		if artifact.SourceRevision != record.ConflictSourceRevision {
			return false, model.NewError(model.CodeConflict, "local source changed again while reconciling the workspace rebase", nil)
		}
		record.SourceRevision, record.SourceBundleDigest = artifact.SourceRevision, artifact.BundleDigest
		record.TrackedFingerprint = artifact.TrackedFingerprint
		record.FetchedCommit, record.FetchedHostRef = "", ""
		if head == artifact.SourceRevision {
			record.ResultCommit, record.ResultBundleDigest = "", ""
		} else {
			digest, err := service.workspaceResultDigest(ctx, access.Workspace, *record, head, stageRoot, index)
			if err != nil {
				return false, err
			}
			record.ResultCommit, record.ResultBundleDigest = head, digest
		}
		record.Conflict, record.ConflictSourceRevision, record.ConflictBundleDigest = false, "", ""
	}
	if workspaceHasConflict(*access.Manifest) {
		return false, nil
	}
	if err := service.transitionManifest(ctx, access.Manifest, model.StateRunning, "", ""); err != nil {
		return false, err
	}
	return true, nil
}

func (service *WorkspaceService) protectWorkspaceRemoval(ctx context.Context, manifest state.Manifest, snapshot runtime.ResourceSnapshot) (protected []string, returnErr error) {
	temporary := manifest.State == model.StateStopped
	if temporary {
		if err := service.runtime.StartWorkspace(ctx, snapshot); err != nil {
			return nil, err
		}
		owner, err := workspaceManifestResource(manifest, runtime.ResourceWorkspace, workspaceOwnerRole)
		if err != nil {
			return nil, err
		}
		snapshot, err = service.ownedSnapshot(ctx, manifest, owner, true)
		if err != nil {
			return nil, err
		}
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workspaceGitCleanupTimeout)
			defer cancel()
			returnErr = errors.Join(returnErr, service.runtime.Stop(cleanupCtx, snapshot, runtime.StopPolicy{TimeoutSeconds: workspaceStopSeconds, Signal: runtime.Signal("TERM")}))
		}()
	}
	for _, record := range manifest.Git {
		working, err := service.workspaceWorkingState(ctx, snapshot, record)
		if err != nil {
			return nil, err
		}
		if !working.trackedClean {
			protected = append(protected, record.Repository+": uncommitted tracked changes")
		}
		if working.untracked {
			protected = append(protected, record.Repository+": untracked files")
		}
		if working.rebase {
			protected = append(protected, record.Repository+": rebase in progress")
		}
		head, err := service.workspaceRevision(ctx, snapshot, record.GuestPath, record.WorkspaceBranch)
		if err != nil {
			return nil, err
		}
		if head != record.SourceRevision && head != record.FetchedCommit {
			protected = append(protected, record.Repository+": unfetched commits")
			continue
		}
		if head != record.SourceRevision && head == record.FetchedCommit {
			status, err := service.git.Status(ctx, gitx.StatusRequest{
				Repository: workspaceGitRepository(record), Workspace: string(manifest.Workspace),
				SourceBranch: record.SourceBranch, SourceRevision: record.SourceRevision,
				WorkspaceBranch: record.WorkspaceBranch, ResultCommit: head,
				TrackedFingerprint: record.TrackedFingerprint, FetchedCommit: record.FetchedCommit,
			})
			if err != nil {
				return nil, err
			}
			if !status.Fetched {
				protected = append(protected, record.Repository+": fetched host ref is missing or changed")
			}
		}
	}
	return uniqueSorted(protected), nil
}

type workspaceWorkingStateResult struct{ trackedClean, untracked, rebase bool }

func (service *WorkspaceService) workspaceWorkingState(ctx context.Context, snapshot runtime.ResourceSnapshot, record state.GitRecord) (workspaceWorkingStateResult, error) {
	var output bytes.Buffer
	capture := &hardLimitWriter{writer: &output, remaining: maxWorkspaceGitOutput}
	exit, err := service.workspaceGitExecutor(snapshot)(ctx, record.GuestPath, []string{"status", "--porcelain=v1", "-z", "--untracked-files=all"}, capture, io.Discard)
	if err != nil || exit.Code == nil || *exit.Code != 0 {
		return workspaceWorkingStateResult{}, errors.Join(err, model.NewError(model.CodeUnavailable, "inspect workspace Git status failed or exceeded its output limit", nil))
	}
	result := workspaceWorkingStateResult{trackedClean: true}
	for _, item := range bytes.Split(output.Bytes(), []byte{0}) {
		if len(item) == 0 {
			continue
		}
		if bytes.HasPrefix(item, []byte("?? ")) {
			result.untracked = true
		} else {
			result.trackedClean = false
		}
	}
	result.rebase, err = gitRefExists(ctx, service.workspaceGitExecutor(snapshot), record.GuestPath, "REBASE_HEAD")
	return result, err
}
func (service *WorkspaceService) workspaceGitAccess(ctx context.Context, root string, name model.WorkspaceName) (workspaceAccess, func() error, func(bool) error, error) {
	access, unlock, err := service.workspaceAccess(ctx, root, name, false)
	if err != nil { return access, nil, nil, err }
	temporary := access.Manifest.State == model.StateStopped
	if temporary {
		if err := service.runtime.StartWorkspace(ctx, access.Workspace); err != nil { _=unlock(); return access,nil,nil,err }
		owner, ownerErr := workspaceManifestResource(*access.Manifest, runtime.ResourceWorkspace, workspaceOwnerRole)
		if ownerErr != nil { _=unlock(); return access,nil,nil,ownerErr }
		access.Workspace, err = service.ownedSnapshot(ctx, *access.Manifest, owner, true)
		if err != nil { _=unlock(); return access,nil,nil,err }
	}
	finish := func(keepRunning bool) error { if !temporary || keepRunning{return nil}; cleanupCtx,cancel:=context.WithTimeout(context.WithoutCancel(ctx),workspaceGitCleanupTimeout);defer cancel();return service.runtime.Stop(cleanupCtx,access.Workspace,runtime.StopPolicy{TimeoutSeconds:workspaceStopSeconds,Signal:runtime.Signal("TERM")}) }
	return access, unlock, finish, nil
}

func (service *WorkspaceService) workspaceGitExecutor(snapshot runtime.ResourceSnapshot) workspaceGitExecutor {
	return func(ctx context.Context, directory string, arguments []string, stdout, stderr io.Writer) (runtime.Exit, error) {
		argv := []string{"/usr/bin/git", "--no-pager", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgSign=false", "-c", "tag.gpgSign=false"}
		argv = append(argv, arguments...)
		env := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_PAGER=cat", "LC_ALL=C"}
		return service.execWorkspace(ctx, snapshot, runtime.ExecSpec{Argv: argv, Env: env, WorkingDir: runtime.GuestPath(directory), User: service.user()}, runtime.ExecIO{Stdout: stdout, Stderr: stderr})
	}
}

func (service *WorkspaceService) workspaceCommand(ctx context.Context, snapshot runtime.ResourceSnapshot, argv []string, directory string, stdout, stderr io.Writer) error {
	exit, err := service.execWorkspace(ctx, snapshot, runtime.ExecSpec{Argv: argv, WorkingDir: runtime.GuestPath(directory), User: service.user()}, runtime.ExecIO{Stdout: stdout, Stderr: stderr})
	if err != nil {
		return err
	}
	if exit.Code == nil || *exit.Code != 0 || exit.Signal != "" {
		return model.NewError(model.CodeUnavailable, "workspace command failed", nil)
	}
	return nil
}

func workspaceGitRepository(record state.GitRecord) gitx.Repository {
	return gitx.Repository{Name: record.Repository, HostPath: record.HostPath, GuestPath: record.GuestPath, Identity: record.Identity}
}

func selectedWorkspaceGitIndexes(manifest state.Manifest, repository string) ([]int, error) {
	if repository == "" {
		if len(manifest.Git) > 1 {
			return nil, model.NewError(model.CodeInvalidInput, "composite workspace Git operation requires --repo", nil)
		}
		return []int{0}, nil
	}
	for index, record := range manifest.Git {
		if record.Repository == repository {
			return []int{index}, nil
		}
	}
	return nil, model.NewError(model.CodeInvalidInput, fmt.Sprintf("repository %q is not a workspace member", repository), nil)
}

func (service *WorkspaceService) workspaceRevision(ctx context.Context, snapshot runtime.ResourceSnapshot, directory, revision string) (string, error) {
	var output bytes.Buffer
	err := executeGitCommand(ctx, service.workspaceGitExecutor(snapshot), directory, []string{"rev-parse", "--verify", revision + "^{commit}"}, &output, io.Discard)
	return strings.TrimSpace(output.String()), err
}

func (service *WorkspaceService) appendUntrackedDiff(ctx context.Context, snapshot runtime.ResourceSnapshot, directory string, capture *hardLimitWriter) error {
	var names bytes.Buffer
	nameCapture := &hardLimitWriter{writer: &names, remaining: maxWorkspaceGitOutput}
	exit, err := service.workspaceGitExecutor(snapshot)(ctx, directory, []string{"ls-files", "--others", "--exclude-standard", "-z"}, nameCapture, io.Discard)
	if err != nil || exit.Code == nil || *exit.Code != 0 || exit.Signal != "" {
		return errors.Join(err, model.NewError(model.CodeUnavailable, "list untracked workspace files failed", nil))
	}
	for _, item := range bytes.Split(names.Bytes(), []byte{0}) {
		if len(item) == 0 {
			continue
		}
		name := string(item)
		clean := path.Clean(name)
		if clean != name || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
			return model.NewError(model.CodeConflict, "workspace returned an unsafe untracked path", nil)
		}
		exit, err := service.workspaceGitExecutor(snapshot)(ctx, directory, []string{"diff", "--no-index", "--binary", "--no-ext-diff", "--no-textconv", "--", "/dev/null", name}, capture, io.Discard)
		if err != nil || exit.Code == nil || (*exit.Code != 0 && *exit.Code != 1) || exit.Signal != "" {
			return errors.Join(err, model.NewError(model.CodeUnavailable, "render untracked workspace diff failed", nil))
		}
	}
	return nil
}

func (service *WorkspaceService) workspaceResultDigest(ctx context.Context, snapshot runtime.ResourceSnapshot, record state.GitRecord, head, stageRoot string, index int) (digest string, returnErr error) {
	_, digest, cleanup, err := service.copyWorkspaceResult(ctx, snapshot, record, head, stageRoot, index)
	if cleanup != nil {
		defer func() { returnErr = errors.Join(returnErr, cleanup()) }()
	}
	return digest, err
}

func (service *WorkspaceService) copyWorkspaceResult(
	ctx context.Context,
	snapshot runtime.ResourceSnapshot,
	record state.GitRecord,
	head string,
	stageRoot string,
	index int,
) (host, digest string, cleanup func() error, returnErr error) {
	guest := path.Join(stageRoot, fmt.Sprintf("result-%d.bundle", index))
	if err := service.workspaceCommand(ctx, snapshot, []string{"/bin/rm", "-f", "--", guest}, "/workspace", nil, nil); err != nil {
		return "", "", nil, err
	}
	if err := executeGitCommand(ctx, service.workspaceGitExecutor(snapshot), record.GuestPath, []string{"bundle", "create", guest, "refs/heads/" + record.WorkspaceBranch}, nil, io.Discard); err != nil {
		return "", "", nil, err
	}
	if err := service.workspaceCommand(ctx, snapshot, []string{"/bin/chmod", "0600", guest}, "/workspace", nil, nil); err != nil {
		return "", "", nil, err
	}
	var advertised bytes.Buffer
	if err := executeGitCommand(ctx, service.workspaceGitExecutor(snapshot), record.GuestPath, []string{"bundle", "list-heads", guest}, &advertised, io.Discard); err != nil {
		return "", "", nil, err
	}
	fields := strings.Fields(strings.TrimSpace(advertised.String()))
	expectedRef := "refs/heads/" + record.WorkspaceBranch
	if len(fields) != 2 || fields[0] != head || fields[1] != expectedRef {
		return "", "", nil, model.NewError(model.CodeConflict, "workspace result bundle advertised an unexpected revision or ref", nil)
	}

	file, err := os.CreateTemp(service.tempRoot, "dsx-result-*.bundle")
	if err != nil {
		return "", "", nil, err
	}
	host = file.Name()
	cleanup = func() error { return os.Remove(host) }
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, cleanup())
		}
	}()
	if err := file.Chmod(gitx.ResultBundleMode); err != nil {
		_ = file.Close()
		return "", "", cleanup, err
	}
	created, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return "", "", cleanup, err
	}
	copyCapture := &hardLimitWriter{writer: file, remaining: int(gitx.MaxResultBundleBytes)}
	exit, copyErr := service.execWorkspace(ctx, snapshot, runtime.ExecSpec{
		Argv: []string{"/bin/cat", "--", guest}, WorkingDir: "/workspace", User: service.user(),
	}, runtime.ExecIO{Stdout: copyCapture, Stderr: io.Discard})
	syncErr := file.Sync()
	closeErr := file.Close()
	if errors.Is(copyErr, errWorkspaceGitOutputLimit) {
		return "", "", cleanup, model.NewError(model.CodeConflict, "workspace result bundle exceeds the transfer limit", copyErr)
	}
	if copyErr != nil || syncErr != nil || closeErr != nil || exit.Code == nil || *exit.Code != 0 || exit.Signal != "" {
		return "", "", cleanup, errors.Join(copyErr, syncErr, closeErr, model.NewError(model.CodeUnavailable, "bounded workspace result transfer failed", nil))
	}
	info, err := os.Lstat(host)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != gitx.ResultBundleMode ||
		info.Size() < 0 || info.Size() > gitx.MaxResultBundleBytes || !os.SameFile(created, info) {
		return "", "", cleanup, errors.Join(err, model.NewError(model.CodeConflict, "copied result bundle has unsafe metadata", nil))
	}
	digest, err = workspaceFileSHA256(host)
	if err != nil {
		return "", "", cleanup, err
	}
	if err := service.git.VerifyBundle(ctx, host, digest); err != nil {
		return "", "", cleanup, err
	}
	return host, digest, cleanup, nil
}

func workspaceFileSHA256(name string) (string, error) {
	file, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, gitx.MaxResultBundleBytes+1)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}


