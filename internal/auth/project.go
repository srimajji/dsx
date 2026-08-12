package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
)

const workspaceCredentialProfile = "credentials"

// Project identifies one canonical, harness-separated credential store.
type Project struct {
	ID      model.ProjectID
	Harness harness.Name
}

// Workspace identifies one persistent writable credential copy.
type Workspace struct {
	ProjectID model.ProjectID
	Name      model.WorkspaceName
	Harness   harness.Name
}

// ProjectStatus describes canonical credentials without exposing their path or contents.
type ProjectStatus struct {
	Configured bool
	Digest     string
}

// WorkspaceCopy is an exclusive session copy backed by persistent workspace credentials.
// ReleaseWorkspace removes only the active session; the workspace seed survives restart.
type WorkspaceCopy struct {
	Workspace      Workspace
	SessionID      model.RunID
	Root           string
	ReadOnlyRoot   string
	BaselineDigest string

	copy      Copy
	leasePath string
}

func validateProject(project Project) error {
	if _, err := model.ParseProjectID(string(project.ID)); err != nil {
		return err
	}
	_, err := harness.ParseName(string(project.Harness))
	return err
}

func validateWorkspace(workspace Workspace) error {
	if _, err := model.ParseProjectID(string(workspace.ProjectID)); err != nil {
		return err
	}
	if _, err := model.ParseWorkspaceName(string(workspace.Name)); err != nil {
		return err
	}
	_, err := harness.ParseName(string(workspace.Harness))
	return err
}

func projectProfile(project Project) Profile {
	return Profile{Harness: project.Harness, Name: string(project.ID)}
}

func workspaceProfile(workspace Workspace) Profile {
	return Profile{
		Harness:   workspace.Harness,
		Name:      workspaceCredentialProfile,
		ProjectID: workspace.ProjectID,
		Sandbox:   string(workspace.Name),
	}
}

func (repository *Repository) ProjectStatus(ctx context.Context, project Project, seeder Seeder) (ProjectStatus, error) {
	if err := validateProject(project); err != nil {
		return ProjectStatus{}, err
	}
	layout, err := validateSeeder(seeder)
	if err != nil {
		return ProjectStatus{}, err
	}
	var result ProjectStatus
	err = repository.withProfileLock(ctx, projectProfile(project), func() error {
		digest, found, err := repository.current(projectProfile(project))
		if err != nil || !found {
			return err
		}
		generation := repository.generationRoot(projectProfile(project), digest)
		observed, err := fingerprint(ctx, generation, layout)
		if err != nil {
			return err
		}
		if observed != digest {
			return errors.New("canonical project credentials failed integrity validation")
		}
		configured, err := hasCredentialArtifact(generation, layout)
		if err != nil {
			return err
		}
		result = ProjectStatus{Configured: configured, Digest: digest}
		return nil
	})
	return result, err
}

func hasCredentialArtifact(root string, layout harness.AuthLayout) (bool, error) {
	for _, artifact := range layout.CredentialArtifacts {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(artifact)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if !info.Mode().IsRegular() {
			return false, errors.New("canonical credential artifact is not a regular file")
		}
		return true, nil
	}
	return false, nil
}

// ImportProject atomically creates or replaces canonical project credentials.
func (repository *Repository) ImportProject(ctx context.Context, project Project, sourceRoot string, replace bool, seeder Seeder) (string, error) {
	if err := validateProject(project); err != nil {
		return "", err
	}
	layout, err := validateSeeder(seeder)
	if err != nil {
		return "", err
	}
	profile := projectProfile(project)
	var digest string
	err = repository.withProfileLock(ctx, profile, func() error {
		_, found, err := repository.current(profile)
		if err != nil {
			return err
		}
		if found && !replace {
			return errors.New("canonical project credentials already exist")
		}
		digest, err = repository.installGeneration(ctx, profile, sourceRoot, layout, seeder)
		if err != nil {
			return err
		}
		return repository.writeCurrent(profile, digest)
	})
	return digest, err
}

func (repository *Repository) withProjectAuthorityLock(ctx context.Context, project Project, action func() error) error {
	return repository.withFileLock(ctx, filepath.Join(
		repository.root, "locks", "projects", string(project.ID), string(project.Harness)+".lock",
	), action)
}

func (repository *Repository) withWorkspaceAuthorityLock(ctx context.Context, workspace Workspace, action func() error) error {
	project := Project{ID: workspace.ProjectID, Harness: workspace.Harness}
	return repository.withProjectAuthorityLock(ctx, project, func() error {
		return repository.withFileLock(ctx, filepath.Join(
			repository.root, "locks", "projects", string(workspace.ProjectID),
			string(workspace.Harness), "workspaces", string(workspace.Name)+".lock",
		), action)
	})
}

func (repository *Repository) AcquireWorkspace(ctx context.Context, workspace Workspace, sessionID model.RunID, seeder Seeder) (result WorkspaceCopy, returnErr error) {
	if err := validateWorkspace(workspace); err != nil {
		return result, err
	}
	err := repository.withWorkspaceAuthorityLock(ctx, workspace, func() error {
		var err error
		result, err = repository.acquireWorkspace(ctx, workspace, sessionID, seeder)
		return err
	})
	return result, err
}

// AcquireWorkspace lazily seeds a persistent workspace+harness store from the
// canonical project store and creates one exclusive writable session copy.
func (repository *Repository) acquireWorkspace(ctx context.Context, workspace Workspace, sessionID model.RunID, seeder Seeder) (result WorkspaceCopy, returnErr error) {
	if err := validateWorkspace(workspace); err != nil {
		return result, err
	}
	if _, err := model.ParseRunID(string(sessionID)); err != nil {
		return result, err
	}
	if _, err := validateSeeder(seeder); err != nil {
		return result, err
	}

	leasePath := filepath.Join(repository.profileRoot(workspaceProfile(workspace)), "active")
	if err := createWorkspaceLease(leasePath, sessionID); err != nil {
		return result, err
	}
	leaseOwned := true
	defer func() {
		if returnErr != nil && leaseOwned {
			returnErr = errors.Join(returnErr, removeWorkspaceLease(leasePath, sessionID))
		}
	}()

	project := Project{ID: workspace.ProjectID, Harness: workspace.Harness}
	status, err := repository.ProjectStatus(ctx, project, seeder)
	if err != nil {
		return result, err
	}
	if !status.Configured {
		return result, errors.New("canonical project credentials are not configured")
	}
	canonicalProfile := projectProfile(project)
	canonicalCopy, err := repository.PrepareGlobalSandbox(ctx, canonicalProfile, sessionID, workspace.ProjectID, string(workspace.Name), seeder)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, repository.RemoveRun(context.WithoutCancel(ctx), canonicalCopy)) }()

	profile := workspaceProfile(workspace)
	_, initialized, err := repository.current(profile)
	if err != nil {
		return result, err
	}
	working, err := repository.PrepareSandbox(ctx, profile, sessionID, seeder)
	if err != nil {
		return result, err
	}
	workingOwned := true
	defer func() {
		if returnErr != nil && workingOwned {
			returnErr = errors.Join(returnErr, repository.RemoveRun(context.WithoutCancel(ctx), working))
		}
	}()

	baseline := ""
	if !initialized {
		if err := repository.Refresh(ctx, working, canonicalCopy.Root, seeder); err != nil {
			return result, err
		}
		promotion, err := repository.Promote(ctx, working, seeder)
		if err != nil || promotion.Conflict {
			if err == nil {
				err = errors.New("initial workspace credential seed conflicted")
			}
			return result, err
		}
		working.BaselineDigest = promotion.Digest
		baseline = canonicalCopy.BaselineDigest
		if err := repository.writeWorkspaceBaseline(workspace, baseline); err != nil {
			return result, err
		}
	} else {
		baseline, err = repository.readWorkspaceBaseline(workspace)
		if err != nil {
			return result, err
		}
	}

	workingOwned = false
	leaseOwned = false
	return WorkspaceCopy{
		Workspace:      workspace,
		SessionID:      sessionID,
		Root:           working.Root,
		ReadOnlyRoot:   working.ReadOnlyRoot,
		BaselineDigest: baseline,
		copy:           working,
		leasePath:      leasePath,
	}, nil
}

func (repository *Repository) PromoteWorkspace(ctx context.Context, copy WorkspaceCopy, seeder Seeder) (result Promotion, returnErr error) {
	if err := repository.validateWorkspaceCopy(copy); err != nil {
		return result, err
	}
	err := repository.withWorkspaceAuthorityLock(ctx, copy.Workspace, func() error {
		var err error
		result, err = repository.promoteWorkspace(ctx, copy, seeder)
		return err
	})
	return result, err
}

// PromoteWorkspace first persists the workspace copy, then serializes a CAS
// promotion into the canonical project store. Concurrent refreshes preserve a
// conflict candidate rather than overwriting newer credentials.
func (repository *Repository) promoteWorkspace(ctx context.Context, copy WorkspaceCopy, seeder Seeder) (Promotion, error) {
	if err := repository.validateWorkspaceCopy(copy); err != nil {
		return Promotion{}, err
	}
	workspacePromotion, err := repository.Promote(ctx, copy.copy, seeder)
	if err != nil {
		return Promotion{}, err
	}
	if workspacePromotion.Conflict {
		return workspacePromotion, nil
	}

	project := Project{ID: copy.Workspace.ProjectID, Harness: copy.Workspace.Harness}
	canonical, err := repository.PrepareGlobalSandbox(ctx, projectProfile(project), copy.SessionID, copy.Workspace.ProjectID, string(copy.Workspace.Name), seeder)
	if err != nil {
		return Promotion{}, err
	}
	defer repository.RemoveRun(context.WithoutCancel(ctx), canonical)
	if err := repository.Refresh(ctx, canonical, copy.Root, seeder); err != nil {
		return Promotion{}, err
	}
	canonical.BaselineDigest = copy.BaselineDigest
	promotion, err := repository.Promote(ctx, canonical, seeder)
	if err != nil || promotion.Conflict {
		return promotion, err
	}
	if err := repository.writeWorkspaceBaseline(copy.Workspace, promotion.Digest); err != nil {
		return Promotion{}, err
	}
	return promotion, nil
}

func (repository *Repository) ReleaseWorkspace(ctx context.Context, copy WorkspaceCopy) error {
	if err := repository.validateWorkspaceCopy(copy); err != nil {
		return err
	}
	return repository.withWorkspaceAuthorityLock(ctx, copy.Workspace, func() error {
		return repository.releaseWorkspace(ctx, copy)
	})
}

// ReleaseWorkspace removes the active session and lease while preserving the
// persistent workspace credential seed.
func (repository *Repository) releaseWorkspace(ctx context.Context, copy WorkspaceCopy) error {
	if err := repository.validateWorkspaceCopy(copy); err != nil {
		return err
	}
	return errors.Join(
		repository.RemoveRun(ctx, copy.copy),
		removeWorkspaceLease(copy.leasePath, copy.SessionID),
	)
}

func (repository *Repository) validateWorkspaceCopy(copy WorkspaceCopy) error {
	if err := validateWorkspace(copy.Workspace); err != nil {
		return err
	}
	if _, err := model.ParseRunID(string(copy.SessionID)); err != nil {
		return err
	}
	if copy.Root != copy.copy.Root || copy.ReadOnlyRoot != copy.copy.ReadOnlyRoot || copy.SessionID != copy.copy.RunID {
		return errors.New("workspace credential copy authority does not match")
	}
	if copy.copy.Profile != workspaceProfile(copy.Workspace) {
		return errors.New("workspace credential profile authority does not match")
	}
	if err := validateDigest(copy.BaselineDigest); err != nil {
		return fmt.Errorf("workspace credential baseline: %w", err)
	}
	expectedLease := filepath.Join(repository.profileRoot(workspaceProfile(copy.Workspace)), "active")
	if copy.leasePath != expectedLease {
		return errors.New("workspace credential lease authority does not match")
	}
	if err := validateWorkspaceLease(expectedLease, copy.SessionID); err != nil {
		return fmt.Errorf("workspace credential lease: %w", err)
	}
	return nil
}

func (repository *Repository) PurgeProject(ctx context.Context, project Project) error {
	if err := validateProject(project); err != nil {
		return err
	}
	return repository.withProjectAuthorityLock(ctx, project, func() error {
		return repository.purgeProject(ctx, project)
	})
}

// PurgeProject removes one harness's canonical project credentials and all
// inactive workspace copies. Any active project or workspace copy blocks it.
func (repository *Repository) purgeProject(ctx context.Context, project Project) error {
	if err := validateProject(project); err != nil {
		return err
	}
	active, err := repository.activeProjectWorkspaceCopies(project)
	if err != nil {
		return err
	}
	if active != 0 {
		return fmt.Errorf("%w: %d workspace copies", ErrActiveCopies, active)
	}
	if err := repository.Purge(ctx, projectProfile(project)); err != nil {
		return err
	}
	projectRoot := filepath.Join(repository.root, "sandboxes", string(project.ID))
	entries, err := os.ReadDir(projectRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := model.ParseWorkspaceName(entry.Name()); err != nil {
			continue
		}
		profileRoot := filepath.Join(projectRoot, entry.Name(), "profiles", string(project.Harness), workspaceCredentialProfile)
		if err := removeExact(profileRoot); err != nil {
			return err
		}
	}
	return nil
}

func (repository *Repository) activeProjectWorkspaceCopies(project Project) (int, error) {
	projectRoot := filepath.Join(repository.root, "sandboxes", string(project.ID))
	workspaces, leaseErr := os.ReadDir(projectRoot)
	if leaseErr != nil && !errors.Is(leaseErr, os.ErrNotExist) {
		return 0, leaseErr
	}
	count := 0
	for _, workspace := range workspaces {
		if !workspace.IsDir() {
			continue
		}
		if _, err := model.ParseWorkspaceName(workspace.Name()); err != nil {
			continue
		}
		leasePath := filepath.Join(projectRoot, workspace.Name(), "profiles", string(project.Harness), workspaceCredentialProfile, "active")
		info, err := os.Lstat(leasePath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() <= 0 || info.Size() > 64 {
			return 0, errors.New("workspace credential lease is unsafe")
		}
		data, err := os.ReadFile(leasePath)
		if err != nil {
			return 0, err
		}
		if _, err := model.ParseRunID(strings.TrimSpace(string(data))); err != nil {
			return 0, errors.New("workspace credential lease is corrupt")
		}
		count++
	}

	runs, err := os.ReadDir(filepath.Join(repository.root, "runs"))
	if errors.Is(err, os.ErrNotExist) {
		return count, nil
	}
	if err != nil {
		return 0, err
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Name() < runs[j].Name() })
	// Run-copy discovery is defense in depth for interrupted acquisitions whose
	// lease could not be durably removed.
	for _, run := range runs {
		if !run.IsDir() {
			continue
		}
		workspaceRoot := filepath.Join(repository.root, "runs", run.Name(), "sandboxes", string(project.ID))
		workspaces, err := os.ReadDir(workspaceRoot)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		for _, workspace := range workspaces {
			path := filepath.Join(workspaceRoot, workspace.Name(), string(project.Harness), workspaceCredentialProfile)
			if info, err := os.Lstat(path); err == nil && info.IsDir() {
				count++
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return 0, err
			}
		}
	}
	return count, nil
}

func (repository *Repository) workspaceBaselinePath(workspace Workspace) string {
	return filepath.Join(repository.profileRoot(workspaceProfile(workspace)), "project-baseline")
}

func (repository *Repository) writeWorkspaceBaseline(workspace Workspace, digest string) error {
	if err := validateDigest(digest); err != nil {
		return err
	}
	path := repository.workspaceBaselinePath(workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".baseline-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(digest + "\n"); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func (repository *Repository) readWorkspaceBaseline(workspace Workspace) (string, error) {
	data, err := os.ReadFile(repository.workspaceBaselinePath(workspace))
	if err != nil {
		return "", err
	}
	digest := strings.TrimSpace(string(data))
	if err := validateDigest(digest); err != nil {
		return "", errors.New("workspace credential baseline is corrupt")
	}
	return digest, nil
}

// ProjectSession is a private writable copy used by explicit DSX login.
type ProjectSession struct {
	Project      Project
	SessionID    model.RunID
	Root         string
	ReadOnlyRoot string

	copy Copy
}

func (repository *Repository) AcquireProjectSession(ctx context.Context, project Project, sessionID model.RunID, allowEmpty bool, seeder Seeder) (result ProjectSession, returnErr error) {
	if err := validateProject(project); err != nil {
		return result, err
	}
	err := repository.withProjectAuthorityLock(ctx, project, func() error {
		var err error
		result, err = repository.acquireProjectSession(ctx, project, sessionID, allowEmpty, seeder)
		return err
	})
	return result, err
}

func (repository *Repository) acquireProjectSession(ctx context.Context, project Project, sessionID model.RunID, allowEmpty bool, seeder Seeder) (ProjectSession, error) {
	if err := validateProject(project); err != nil {
		return ProjectSession{}, err
	}
	if _, err := model.ParseRunID(string(sessionID)); err != nil {
		return ProjectSession{}, err
	}
	profile := projectProfile(project)
	if allowEmpty {
		if _, err := repository.Ensure(ctx, profile, seeder); err != nil {
			return ProjectSession{}, err
		}
	}
	copy, err := repository.Prepare(ctx, profile, sessionID, seeder)
	if err != nil {
		return ProjectSession{}, err
	}
	return ProjectSession{
		Project: project, SessionID: sessionID, Root: copy.Root, ReadOnlyRoot: copy.ReadOnlyRoot, copy: copy,
	}, nil
}

func (repository *Repository) RefreshProjectSession(ctx context.Context, session ProjectSession, sourceRoot string, seeder Seeder) error {
	if err := repository.validateProjectSession(session); err != nil {
		return err
	}
	return repository.withProjectAuthorityLock(ctx, session.Project, func() error {
		return repository.refreshProjectSession(ctx, session, sourceRoot, seeder)
	})
}

func (repository *Repository) refreshProjectSession(ctx context.Context, session ProjectSession, sourceRoot string, seeder Seeder) error {
	if err := repository.validateProjectSession(session); err != nil {
		return err
	}
	return repository.Refresh(ctx, session.copy, sourceRoot, seeder)
}

func (repository *Repository) PromoteProjectSession(ctx context.Context, session ProjectSession, seeder Seeder) (result Promotion, returnErr error) {
	if err := repository.validateProjectSession(session); err != nil {
		return result, err
	}
	err := repository.withProjectAuthorityLock(ctx, session.Project, func() error {
		var err error
		result, err = repository.promoteProjectSession(ctx, session, seeder)
		return err
	})
	return result, err
}

func (repository *Repository) promoteProjectSession(ctx context.Context, session ProjectSession, seeder Seeder) (Promotion, error) {
	if err := repository.validateProjectSession(session); err != nil {
		return Promotion{}, err
	}
	return repository.Promote(ctx, session.copy, seeder)
}

func (repository *Repository) ReleaseProjectSession(ctx context.Context, session ProjectSession) error {
	if err := repository.validateProjectSession(session); err != nil {
		return err
	}
	return repository.withProjectAuthorityLock(ctx, session.Project, func() error {
		return repository.releaseProjectSession(ctx, session)
	})
}

func (repository *Repository) releaseProjectSession(ctx context.Context, session ProjectSession) error {
	if err := repository.validateProjectSession(session); err != nil {
		return err
	}
	return repository.RemoveRun(ctx, session.copy)
}

func (repository *Repository) validateProjectSession(session ProjectSession) error {
	if err := validateProject(session.Project); err != nil {
		return err
	}
	if _, err := model.ParseRunID(string(session.SessionID)); err != nil {
		return err
	}
	if session.copy.Profile != projectProfile(session.Project) || session.copy.RunID != session.SessionID ||
		session.Root != session.copy.Root || session.ReadOnlyRoot != session.copy.ReadOnlyRoot {
		return errors.New("project credential session authority does not match")
	}
	return repository.validateCopyAuthority(session.copy)
}

func validateWorkspaceLease(path string, sessionID model.RunID) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() <= 0 || info.Size() > 64 {
		return errors.New("workspace credential lease is unsafe")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) != string(sessionID) {
		return errors.New("workspace credential lease belongs to another session")
	}
	return nil
}

func createWorkspaceLease(path string, sessionID model.RunID) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%w: workspace harness copy", ErrActiveCopies)
	}
	if err != nil {
		return err
	}
	if _, err := file.WriteString(string(sessionID) + "\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func removeWorkspaceLease(path string, sessionID model.RunID) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) != string(sessionID) {
		return errors.New("workspace credential lease belongs to another session")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
