package fs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/state"
	"golang.org/x/sys/unix"
)

const (
	manifestDirectory      = "manifests"
	projectLockDirectory   = "locks"
	sandboxLockDirectory   = "sandbox-locks"
	manifestLockDirectory  = "manifest-locks"
	maxManifestBytes       = 4 << 20
	maxManifestEntries     = 10_000
	maxLockMetadataBytes   = 4 << 10
	atomicWriteTempPattern = ".manifest-*"
	atomicWriteTempPrefix  = ".manifest-"
	maxProjectLockWait     = 30 * time.Second
)

// ManifestRepository is the durable filesystem implementation of both the
// manifest and process-lock ports. Its root and every object below it are
// private to the current effective user.
type ManifestRepository struct {
	root            string
	syncDirectory   func(string) error
	beforeRename    func() error
	afterRename     func() error
	projectLockWait time.Duration
}

var (
	_ state.ManifestRepository = (*ManifestRepository)(nil)
	_ state.LockRepository     = (*ManifestRepository)(nil)
)

// NewManifestRepository creates a private state root when absent and rejects
// an existing root that is not a real, mode-0700, current-user directory.
func NewManifestRepository(stateRoot string) (*ManifestRepository, error) {
	if stateRoot == "" || !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return nil, model.NewError(model.CodeInvalidInput, "DSX state root must be a clean absolute path", nil)
	}
	if err := ensurePrivateRoot(stateRoot); err != nil {
		return nil, model.Wrap(model.CodeInternal, "initialize DSX state root", err)
	}
	return &ManifestRepository{
		root:            stateRoot,
		syncDirectory:   syncDirectory,
		projectLockWait: maxProjectLockWait,
	}, nil
}

func (repository *ManifestRepository) CreateIntent(ctx context.Context, manifest state.Manifest) error {
	if err := ctx.Err(); err != nil {
		return model.Wrap(model.CodeUnavailable, "create manifest intent", err)
	}
	if err := state.ValidateManifest(manifest); err != nil {
		return model.NewError(model.CodeInvalidInput, "invalid manifest intent", err)
	}
	if manifest.Version != state.ManifestVersion || manifest.Generation != 1 || manifest.State != model.StatePlanned || manifest.Operation != "create" {
		return model.NewError(model.CodeInvalidInput, "manifest intent must be version 1, generation 1, planned, and operation create", nil)
	}
	path, err := repository.manifestPath(manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if err != nil {
		return err
	}
	data, err := encodeManifest(manifest)
	if err != nil {
		return err
	}
	if err := repository.ensureManifestParents(manifest.ProjectID, manifest.Sandbox); err != nil {
		return model.Wrap(model.CodeInternal, "create manifest directories", err)
	}
	lock, err := repository.lockRun(ctx, manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return model.Wrap(model.CodeUnavailable, "create manifest intent", err)
	}
	if err := repository.atomicWrite(path, data, false); err != nil {
		if errors.Is(err, os.ErrExist) || errors.Is(err, unix.EEXIST) {
			return model.NewError(model.CodeConflict, "manifest already exists", err)
		}
		return model.Wrap(model.CodeInternal, "write manifest intent", err)
	}
	return nil
}

func (repository *ManifestRepository) LoadManifest(ctx context.Context, projectID model.ProjectID, sandbox model.SandboxName, runID model.RunID) (state.Manifest, bool, error) {
	if err := ctx.Err(); err != nil {
		return state.Manifest{}, false, model.Wrap(model.CodeUnavailable, "load manifest", err)
	}
	path, err := repository.manifestPath(projectID, sandbox, runID)
	if err != nil {
		return state.Manifest{}, false, err
	}
	if err := repository.verifyManifestParents(projectID, sandbox); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state.Manifest{}, false, nil
		}
		return state.Manifest{}, false, model.Wrap(model.CodeInternal, "verify manifest directories", err)
	}
	manifest, found, err := loadManifestFile(path, projectID, sandbox, runID)
	if err != nil {
		return state.Manifest{}, false, err
	}
	return manifest, found, nil
}

func (repository *ManifestRepository) ReplaceManifest(ctx context.Context, replacement state.Manifest, expectedGeneration uint64) error {
	if err := ctx.Err(); err != nil {
		return model.Wrap(model.CodeUnavailable, "replace manifest", err)
	}
	if err := state.ValidateManifest(replacement); err != nil {
		return model.NewError(model.CodeInvalidInput, "invalid replacement manifest", err)
	}
	if expectedGeneration == 0 || replacement.Generation != expectedGeneration || expectedGeneration == ^uint64(0) {
		return model.NewError(model.CodeInvalidInput, "replacement generation must equal the positive expected generation and be incrementable", nil)
	}
	path, err := repository.manifestPath(replacement.ProjectID, replacement.Sandbox, replacement.RunID)
	if err != nil {
		return err
	}
	lock, err := repository.lockRun(ctx, replacement.ProjectID, replacement.Sandbox, replacement.RunID)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	current, found, err := repository.LoadManifest(ctx, replacement.ProjectID, replacement.Sandbox, replacement.RunID)
	if err != nil {
		return err
	}
	if !found || current.Generation != expectedGeneration {
		return model.NewError(model.CodeConflict, "manifest generation conflict", nil)
	}
	if err := validateReplacement(current, replacement); err != nil {
		return model.NewError(model.CodeInvalidInput, "invalid replacement manifest", err)
	}
	replacement.Generation = expectedGeneration + 1
	data, err := encodeManifest(replacement)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return model.Wrap(model.CodeUnavailable, "replace manifest", err)
	}
	if err := repository.atomicWrite(path, data, true); err != nil {
		return model.Wrap(model.CodeInternal, "replace manifest", err)
	}
	return nil
}

func (repository *ManifestRepository) ListProjectManifests(ctx context.Context, projectID model.ProjectID) ([]state.Manifest, error) {
	if err := validateProjectID(projectID); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, model.Wrap(model.CodeUnavailable, "list project manifests", err)
	}
	if err := verifySecureDirectory(repository.root); err != nil {
		return nil, model.Wrap(model.CodeInternal, "verify DSX state root", err)
	}
	base := filepath.Join(repository.root, manifestDirectory)
	if err := verifySecureDirectory(base); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []state.Manifest{}, nil
		}
		return nil, model.Wrap(model.CodeInternal, "verify manifest directory", err)
	}
	projectPath := filepath.Join(base, string(projectID))
	if err := verifySecureDirectory(projectPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []state.Manifest{}, nil
		}
		return nil, model.Wrap(model.CodeInternal, "verify project manifest directory", err)
	}
	budget := maxManifestEntries
	manifests, err := repository.listProject(ctx, projectID, projectPath, &budget)
	if err != nil {
		return nil, err
	}
	return manifests, nil
}

// CountOwnedResources is the manifest-backed, read-only inventory used by the
// bare command. A write-ahead intent counts as discoverable state even before
// its first resource is created, so interrupted creation routes to recovery.
func (repository *ManifestRepository) CountOwnedResources(ctx context.Context, projectID model.ProjectID) (int, error) {
	manifests, err := repository.ListProjectManifests(ctx, projectID)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, manifest := range manifests {
		manifestResources := 0
		for _, resource := range manifest.Resources {
			if !resource.Deleted {
				manifestResources++
			}
		}
		if manifestResources == 0 {
			manifestResources = 1
		}
		count += manifestResources
	}
	return count, nil
}

func (repository *ManifestRepository) ListAllManifests(ctx context.Context) ([]state.Manifest, error) {
	if err := ctx.Err(); err != nil {
		return nil, model.Wrap(model.CodeUnavailable, "list manifests", err)
	}
	if err := verifySecureDirectory(repository.root); err != nil {
		return nil, model.Wrap(model.CodeInternal, "verify DSX state root", err)
	}
	base := filepath.Join(repository.root, manifestDirectory)
	if err := verifySecureDirectory(base); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []state.Manifest{}, nil
		}
		return nil, model.Wrap(model.CodeInternal, "verify manifest directory", err)
	}
	entries, err := readDirectoryBounded(base)
	if err != nil {
		return nil, model.Wrap(model.CodeInternal, "read manifest directory", err)
	}
	budget := maxManifestEntries
	result := make([]state.Manifest, 0)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, model.Wrap(model.CodeUnavailable, "list manifests", err)
		}
		if err := consumeEntryBudget(&budget); err != nil {
			return nil, err
		}
		projectID, parseErr := model.ParseProjectID(entry.Name())
		if parseErr != nil {
			return nil, corruptManifest(fmt.Errorf("unexpected entry %q in manifest directory", entry.Name()))
		}
		projectPath := filepath.Join(base, entry.Name())
		if err := verifySecureDirectory(projectPath); err != nil {
			return nil, corruptManifest(fmt.Errorf("project entry %q: %w", entry.Name(), err))
		}
		projectManifests, err := repository.listProject(ctx, projectID, projectPath, &budget)
		if err != nil {
			return nil, err
		}
		result = append(result, projectManifests...)
	}
	sortManifests(result)
	return result, nil
}

func (repository *ManifestRepository) DeleteManifest(ctx context.Context, projectID model.ProjectID, sandbox model.SandboxName, runID model.RunID) error {
	if err := ctx.Err(); err != nil {
		return model.Wrap(model.CodeUnavailable, "delete manifest", err)
	}
	path, err := repository.manifestPath(projectID, sandbox, runID)
	if err != nil {
		return err
	}
	lock, err := repository.lockRun(ctx, projectID, sandbox, runID)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	if err := repository.verifyManifestParents(projectID, sandbox); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return model.Wrap(model.CodeInternal, "verify manifest directories for deletion", err)
	}
	if _, found, err := loadManifestFile(path, projectID, sandbox, runID); err != nil {
		return err
	} else if !found {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return model.Wrap(model.CodeUnavailable, "delete manifest", err)
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return model.Wrap(model.CodeInternal, "delete manifest", err)
	}
	if err := repository.syncDirectory(filepath.Dir(path)); err != nil {
		return model.Wrap(model.CodeInternal, "sync manifest directory after deletion", err)
	}
	return nil
}

func (repository *ManifestRepository) LockProject(ctx context.Context, projectID model.ProjectID) (state.ProjectLock, error) {
	if err := validateProjectID(projectID); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, model.Wrap(model.CodeUnavailable, "lock project", err)
	}
	if err := repository.ensureDirectoryPath(projectLockDirectory); err != nil {
		return nil, model.Wrap(model.CodeInternal, "create project lock directory", err)
	}
	path := filepath.Join(repository.root, projectLockDirectory, string(projectID)+".lock")
	lockContext, cancel := context.WithTimeout(ctx, repository.projectLockWait)
	defer cancel()
	return repository.acquireLock(lockContext, path, projectID, "", "project")
}

func (repository *ManifestRepository) LockSandbox(ctx context.Context, projectID model.ProjectID, sandbox model.SandboxName) (state.ProjectLock, error) {
	if err := validateProjectID(projectID); err != nil {
		return nil, err
	}
	parsedSandbox, err := model.ParseSandboxName(string(sandbox))
	if err != nil || parsedSandbox != sandbox {
		return nil, model.NewError(model.CodeInvalidInput, "invalid sandbox lock name", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, model.Wrap(model.CodeUnavailable, "lock sandbox", err)
	}
	if err := repository.ensureDirectoryPath(sandboxLockDirectory); err != nil {
		return nil, model.Wrap(model.CodeInternal, "create sandbox lock directory", err)
	}
	if err := repository.ensureDirectoryPath(sandboxLockDirectory, string(projectID)); err != nil {
		return nil, model.Wrap(model.CodeInternal, "create sandbox project lock directory", err)
	}
	directory := filepath.Join(repository.root, sandboxLockDirectory, string(projectID))
	path := filepath.Join(directory, string(sandbox)+".lock")
	if filepath.Dir(path) != directory {
		return nil, model.NewError(model.CodeInvalidInput, "sandbox lock path escapes state root", nil)
	}
	lockContext, cancel := context.WithTimeout(ctx, repository.projectLockWait)
	defer cancel()
	return repository.acquireLock(lockContext, path, projectID, sandbox, "sandbox")
}

func (repository *ManifestRepository) listProject(ctx context.Context, projectID model.ProjectID, projectPath string, budget *int) ([]state.Manifest, error) {
	sandboxEntries, err := readDirectoryBounded(projectPath)
	if err != nil {
		return nil, model.Wrap(model.CodeInternal, "read project manifest directory", err)
	}
	result := make([]state.Manifest, 0)
	for _, sandboxEntry := range sandboxEntries {
		if err := ctx.Err(); err != nil {
			return nil, model.Wrap(model.CodeUnavailable, "list project manifests", err)
		}
		if err := consumeEntryBudget(budget); err != nil {
			return nil, err
		}
		sandbox, parseErr := model.ParseSandboxName(sandboxEntry.Name())
		if parseErr != nil {
			return nil, corruptManifest(fmt.Errorf("unexpected entry %q in project manifest directory", sandboxEntry.Name()))
		}
		sandboxPath := filepath.Join(projectPath, sandboxEntry.Name())
		if err := verifySecureDirectory(sandboxPath); err != nil {
			return nil, corruptManifest(fmt.Errorf("sandbox entry %q: %w", sandboxEntry.Name(), err))
		}
		runEntries, err := readDirectoryBounded(sandboxPath)
		if err != nil {
			return nil, model.Wrap(model.CodeInternal, "read sandbox manifest directory", err)
		}
		for _, runEntry := range runEntries {
			if err := consumeEntryBudget(budget); err != nil {
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				return nil, model.Wrap(model.CodeUnavailable, "list project manifests", err)
			}
			name := runEntry.Name()
			if isAtomicWriteTempName(name) {
				if err := repository.removeAtomicWriteTemp(sandboxPath, name); err != nil {
					return nil, corruptManifest(fmt.Errorf("atomic-write temporary entry %q: %w", name, err))
				}
				continue
			}
			if filepath.Ext(name) != ".json" {
				return nil, corruptManifest(fmt.Errorf("unexpected entry %q in sandbox manifest directory", name))
			}
			runID, parseErr := model.ParseRunID(name[:len(name)-len(".json")])
			if parseErr != nil {
				return nil, corruptManifest(fmt.Errorf("invalid run manifest filename %q", name))
			}
			manifest, found, err := loadManifestFile(filepath.Join(sandboxPath, name), projectID, sandbox, runID)
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, corruptManifest(fmt.Errorf("manifest %q disappeared during listing", name))
			}
			result = append(result, manifest)
		}
	}
	sortManifests(result)
	return result, nil
}

func (repository *ManifestRepository) manifestPath(projectID model.ProjectID, sandbox model.SandboxName, runID model.RunID) (string, error) {
	if err := validateProjectID(projectID); err != nil {
		return "", err
	}
	parsedSandbox, err := model.ParseSandboxName(string(sandbox))
	if err != nil || parsedSandbox != sandbox {
		return "", model.NewError(model.CodeInvalidInput, "invalid manifest sandbox", err)
	}
	parsedRunID, err := model.ParseRunID(string(runID))
	if err != nil || parsedRunID != runID {
		return "", model.NewError(model.CodeInvalidInput, "invalid manifest run ID", err)
	}
	directory := filepath.Join(repository.root, manifestDirectory, string(projectID), string(sandbox))
	path := filepath.Join(directory, string(runID)+".json")
	if filepath.Dir(path) != directory {
		return "", model.NewError(model.CodeInvalidInput, "manifest path escapes state root", nil)
	}
	return path, nil
}

func validateProjectID(projectID model.ProjectID) error {
	parsed, err := model.ParseProjectID(string(projectID))
	if err != nil || parsed != projectID {
		return model.NewError(model.CodeInvalidInput, "invalid manifest project ID", err)
	}
	return nil
}

func (repository *ManifestRepository) ensureManifestParents(projectID model.ProjectID, sandbox model.SandboxName) error {
	if err := repository.ensureDirectoryPath(manifestDirectory); err != nil {
		return err
	}
	if err := repository.ensureDirectoryPath(manifestDirectory, string(projectID)); err != nil {
		return err
	}
	return repository.ensureDirectoryPath(manifestDirectory, string(projectID), string(sandbox))
}

func (repository *ManifestRepository) verifyManifestParents(projectID model.ProjectID, sandbox model.SandboxName) error {
	paths := []string{
		repository.root,
		filepath.Join(repository.root, manifestDirectory),
		filepath.Join(repository.root, manifestDirectory, string(projectID)),
		filepath.Join(repository.root, manifestDirectory, string(projectID), string(sandbox)),
	}
	for _, path := range paths {
		if err := verifySecureDirectory(path); err != nil {
			return err
		}
	}
	return nil
}

func (repository *ManifestRepository) ensureDirectoryPath(parts ...string) error {
	if err := verifySecureDirectory(repository.root); err != nil {
		return err
	}
	current := repository.root
	for _, part := range parts {
		parent := current
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil {
				if !errors.Is(err, os.ErrExist) {
					return err
				}
			} else {
				if err := os.Chmod(current, 0o700); err != nil {
					return err
				}
				if err := repository.syncDirectory(parent); err != nil {
					return err
				}
			}
		} else if err != nil {
			return err
		} else if err := verifySecureInfo(current, info, true); err != nil {
			return err
		}
		if err := verifySecureDirectory(current); err != nil {
			return err
		}
	}
	return nil
}

func ensurePrivateRoot(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		return verifySecureInfo(path, info, true)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	return verifySecureDirectory(path)
}

func verifySecureDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return verifySecureInfo(path, info, true)
}

func verifySecureInfo(path string, info os.FileInfo, directory bool) error {
	if directory {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
			return fmt.Errorf("%s is not a real mode-0700 directory", path)
		}
	} else if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("%s is not a real mode-0600 regular file", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is not owned by the current effective user", path)
	}
	return nil
}

func encodeManifest(manifest state.Manifest) ([]byte, error) {
	manifest.CreatedAt = manifest.CreatedAt.UTC()
	manifest.UpdatedAt = manifest.UpdatedAt.UTC()
	data, err := json.Marshal(manifest)
	if err != nil {
		return nil, model.Wrap(model.CodeInternal, "encode manifest", err)
	}
	data = append(data, '\n')
	if len(data) > maxManifestBytes {
		return nil, model.NewError(model.CodeInvalidInput, "manifest exceeds size limit", nil)
	}
	return data, nil
}

func loadManifestFile(path string, projectID model.ProjectID, sandbox model.SandboxName, runID model.RunID) (state.Manifest, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return state.Manifest{}, false, nil
	}
	if err != nil {
		return state.Manifest{}, false, model.Wrap(model.CodeInternal, "inspect manifest", err)
	}
	if err := verifySecureInfo(path, info, false); err != nil {
		return state.Manifest{}, false, corruptManifest(err)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return state.Manifest{}, false, model.Wrap(model.CodeInternal, "open manifest", err)
	}
	file := os.NewFile(uintptr(fd), path)
	openedInfo, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return state.Manifest{}, false, model.Wrap(model.CodeInternal, "inspect opened manifest", statErr)
	}
	if err := verifySecureInfo(path, openedInfo, false); err != nil {
		_ = file.Close()
		return state.Manifest{}, false, corruptManifest(err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return state.Manifest{}, false, model.Wrap(model.CodeInternal, "read manifest", readErr)
	}
	if closeErr != nil {
		return state.Manifest{}, false, model.Wrap(model.CodeInternal, "close manifest", closeErr)
	}
	if len(data) > maxManifestBytes {
		return state.Manifest{}, false, corruptManifest(errors.New("manifest exceeds size limit"))
	}
	if err := validateStrictJSON(data); err != nil {
		return state.Manifest{}, false, corruptManifest(err)
	}
	var manifest state.Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return state.Manifest{}, false, corruptManifest(err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return state.Manifest{}, false, corruptManifest(err)
	}
	if manifest.ProjectID != projectID || manifest.Sandbox != sandbox || manifest.RunID != runID {
		return state.Manifest{}, false, corruptManifest(errors.New("manifest identity does not match its path"))
	}
	if err := state.ValidateManifest(manifest); err != nil {
		return state.Manifest{}, false, corruptManifest(err)
	}
	return manifest, true, nil
}

func validateReplacement(current, replacement state.Manifest) error {
	identityChanged := replacement.Version != current.Version ||
		replacement.ProjectID != current.ProjectID ||
		replacement.CanonicalRoot != current.CanonicalRoot ||
		replacement.Sandbox != current.Sandbox ||
		replacement.RunID != current.RunID ||
		replacement.Mode != current.Mode ||
		!replacement.CreatedAt.Equal(current.CreatedAt)
	planChangedWithoutPortReconfiguration := replacement.PlanHash != current.PlanHash && replacement.Operation != "reconfigure-ports"
	if identityChanged || planChangedWithoutPortReconfiguration {
		return errors.New("replacement changes immutable manifest identity")
	}
	if replacement.UpdatedAt.Before(current.UpdatedAt) {
		return errors.New("replacement updated_at precedes current updated_at")
	}
	if replacement.State != current.State {
		if err := current.State.Transition(replacement.State); err != nil && !isCapturePendingTransition(current, replacement) {
			return err
		}
	}
	return nil
}

func isCapturePendingTransition(current, replacement state.Manifest) bool {
	return current.Mode == model.ModeClone &&
		(current.State == model.StateRunning || current.State == model.StateStopped) &&
		!current.UncapturedWork &&
		replacement.State == model.StateCreating &&
		replacement.Operation == "capture" &&
		replacement.UncapturedWork
}

func (repository *ManifestRepository) atomicWrite(destination string, data []byte, replace bool) (result error) {
	directory := filepath.Dir(destination)
	temporary, err := os.CreateTemp(directory, atomicWriteTempPattern)
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		if temporary != nil {
			_ = temporary.Close()
		}
		if result == nil {
			return
		}
		if removeErr := os.Remove(temporaryPath); removeErr == nil {
			result = errors.Join(result, repository.syncDirectory(directory))
		} else if !errors.Is(removeErr, os.ErrNotExist) {
			result = errors.Join(result, removeErr)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	temporary = nil
	if repository.beforeRename != nil {
		if err := repository.beforeRename(); err != nil {
			return err
		}
	}
	if replace {
		err = os.Rename(temporaryPath, destination)
	} else {
		err = renameNoReplace(temporaryPath, destination)
	}
	if err != nil {
		return err
	}
	if repository.afterRename != nil {
		if err := repository.afterRename(); err != nil {
			return err
		}
	}
	return repository.syncDirectory(directory)
}

func isAtomicWriteTempName(name string) bool {
	if !strings.HasPrefix(name, atomicWriteTempPrefix) {
		return false
	}
	suffix := name[len(atomicWriteTempPrefix):]
	if len(suffix) == 0 || len(suffix) > 10 {
		return false
	}
	for _, character := range suffix {
		if character < '0' || character > '9' {
			return false
		}
	}
	value, err := strconv.ParseUint(suffix, 10, 32)
	return err == nil && strconv.FormatUint(value, 10) == suffix
}

func (repository *ManifestRepository) removeAtomicWriteTemp(directory, name string) error {
	path := filepath.Join(directory, name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := verifySecureInfo(path, info, false); err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	openedInfo, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return statErr
	}
	if err := verifySecureInfo(path, openedInfo, false); err != nil {
		_ = file.Close()
		return err
	}
	if !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return errors.New("atomic-write temporary entry changed while being inspected")
	}
	if err := file.Close(); err != nil {
		return err
	}
	currentInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := verifySecureInfo(path, currentInfo, false); err != nil {
		return err
	}
	if !os.SameFile(openedInfo, currentInfo) {
		return errors.New("atomic-write temporary entry changed before removal")
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return repository.syncDirectory(directory)
}

func readDirectoryBounded(path string) ([]os.DirEntry, error) {
	if err := verifySecureDirectory(path); err != nil {
		return nil, err
	}
	directory, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	entries, readErr := directory.ReadDir(maxManifestEntries + 1)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, errors.Join(readErr, closeErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(entries) > maxManifestEntries {
		return nil, errors.New("manifest listing exceeds entry limit")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

func consumeEntryBudget(budget *int) error {
	if *budget == 0 {
		return corruptManifest(errors.New("manifest listing exceeds entry limit"))
	}
	*budget--
	return nil
}

func sortManifests(manifests []state.Manifest) {
	sort.Slice(manifests, func(i, j int) bool {
		if manifests[i].ProjectID != manifests[j].ProjectID {
			return manifests[i].ProjectID < manifests[j].ProjectID
		}
		if manifests[i].Sandbox != manifests[j].Sandbox {
			return manifests[i].Sandbox < manifests[j].Sandbox
		}
		return manifests[i].RunID < manifests[j].RunID
	})
}

func corruptManifest(err error) error {
	return model.NewError(model.CodeInternal, "manifest repository contains corrupt data", err)
}

type lockOwner struct {
	PID        int               `json:"pid"`
	ProjectID  model.ProjectID   `json:"project_id"`
	Scope      string            `json:"scope"`
	Sandbox    model.SandboxName `json:"sandbox,omitempty"`
	AcquiredAt time.Time         `json:"acquired_at"`
}

type fileLock struct {
	mutex    sync.Mutex
	file     *os.File
	unlocked bool
}

func (repository *ManifestRepository) lockRun(ctx context.Context, projectID model.ProjectID, sandbox model.SandboxName, runID model.RunID) (*fileLock, error) {
	if err := repository.ensureDirectoryPath(manifestLockDirectory); err != nil {
		return nil, model.Wrap(model.CodeInternal, "create manifest lock directory", err)
	}
	if err := repository.ensureDirectoryPath(manifestLockDirectory, string(projectID)); err != nil {
		return nil, model.Wrap(model.CodeInternal, "create manifest project lock directory", err)
	}
	if err := repository.ensureDirectoryPath(manifestLockDirectory, string(projectID), string(sandbox)); err != nil {
		return nil, model.Wrap(model.CodeInternal, "create manifest sandbox lock directory", err)
	}
	path := filepath.Join(repository.root, manifestLockDirectory, string(projectID), string(sandbox), string(runID)+".lock")
	return repository.acquireLock(ctx, path, projectID, sandbox, "manifest")
}

func (repository *ManifestRepository) acquireLock(ctx context.Context, path string, projectID model.ProjectID, sandbox model.SandboxName, scope string) (*fileLock, error) {
	file, created, err := openLockFile(path)
	if err != nil {
		return nil, model.Wrap(model.CodeInternal, "open "+scope+" lock", err)
	}
	if created {
		if err := repository.syncDirectory(filepath.Dir(path)); err != nil {
			_ = file.Close()
			return nil, model.Wrap(model.CodeInternal, "sync "+scope+" lock directory", err)
		}
	}
	cancelAcquisition := func(contended bool) error {
		owner := ""
		if contended {
			owner = readLockOwner(file)
		}
		_ = file.Close()
		message := "acquire " + scope + " lock canceled"
		if contended {
			message = scope + " lock is held"
			if owner != "" {
				message += " by " + owner
			}
		}
		return model.NewError(model.CodeUnavailable, message, ctx.Err())
	}
	contended := false
	for {
		if ctx.Err() != nil {
			return nil, cancelAcquisition(contended)
		}
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			break
		} else if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, model.Wrap(model.CodeInternal, "acquire "+scope+" lock", err)
		}
		contended = true
		select {
		case <-ctx.Done():
			return nil, cancelAcquisition(true)
		case <-time.After(20 * time.Millisecond):
		}
	}
	owner := lockOwner{PID: os.Getpid(), ProjectID: projectID, Sandbox: sandbox, Scope: scope, AcquiredAt: time.Now().UTC()}
	data, err := json.Marshal(owner)
	if err == nil {
		data = append(data, '\n')
		err = writeLockOwner(file, data)
	}
	if err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
		return nil, model.Wrap(model.CodeInternal, "write "+scope+" lock owner", err)
	}
	return &fileLock{file: file}, nil
}

func openLockFile(path string) (*os.File, bool, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	created := err == nil
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		return nil, false, err
	}
	file := os.NewFile(uintptr(fd), path)
	if created {
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			return nil, false, err
		}
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, false, err
	}
	if err := verifySecureInfo(path, info, false); err != nil {
		_ = file.Close()
		return nil, false, err
	}
	return file, created, nil
}

func writeLockOwner(file *os.File, data []byte) error {
	if len(data) > maxLockMetadataBytes {
		return errors.New("lock owner metadata exceeds size limit")
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

func readLockOwner(file *os.File) string {
	buffer := make([]byte, maxLockMetadataBytes+1)
	count, err := file.ReadAt(buffer, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	if count == 0 || count > maxLockMetadataBytes || validateStrictJSON(buffer[:count]) != nil {
		return ""
	}
	var owner lockOwner
	decoder := json.NewDecoder(bytes.NewReader(buffer[:count]))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&owner) != nil || owner.PID <= 0 || owner.ProjectID == "" || owner.Scope == "" || owner.AcquiredAt.IsZero() {
		return ""
	}
	return "pid " + strconv.Itoa(owner.PID) + " since " + owner.AcquiredAt.UTC().Format(time.RFC3339Nano)
}

func (lock *fileLock) Unlock() error {
	lock.mutex.Lock()
	defer lock.mutex.Unlock()
	if lock.unlocked {
		return nil
	}
	lock.unlocked = true
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	return errors.Join(unlockErr, closeErr)
}
