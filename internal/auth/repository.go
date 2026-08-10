package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"golang.org/x/sys/unix"
)

const currentFile = "current"

var ErrActiveCopies = errors.New("authentication profile has active run copies")

type Profile struct {
	Harness   harness.Name
	Name      string
	ProjectID model.ProjectID
	Sandbox   string
}

func SandboxProfile(profile Profile, projectID model.ProjectID, sandbox string) Profile {
	profile.ProjectID = projectID
	profile.Sandbox = sandbox
	return profile
}

type Copy struct {
	Profile        Profile
	RunID          model.RunID
	OwnerProjectID model.ProjectID
	OwnerSandbox   string
	Root           string
	ReadOnlyRoot   string
	BaselineDigest string
}

type Promotion struct {
	Digest        string
	Conflict      bool
	CandidateRoot string
}
type Seeder interface {
	AuthLayout() harness.AuthLayout
	Seed(context.Context, harness.SeedRequest) error
}

type Repository struct {
	root string
}

func NewRepository(root string) (*Repository, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {

		return nil, err
	}
	absolute = filepath.Clean(absolute)
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(absolute, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("auth repository root must be a real directory")
	}
	return &Repository{root: absolute}, nil
}
func (repository *Repository) Ensure(ctx context.Context, profile Profile, seeder Seeder) (string, error) {
	if err := validateProfile(profile); err != nil {
		return "", err
	}
	if isSandboxProfile(profile) {
		return "", errors.New("global authentication profile cannot include sandbox scope")
	}
	layout, err := validateSeeder(seeder)
	if err != nil {
		return "", err
	}
	var digest string
	err = repository.withProfileLock(ctx, profile, func() error {
		current, found, err := repository.current(profile)
		if err != nil {
			return err
		}
		if found {
			digest = current
			return nil
		}
		empty, err := os.MkdirTemp(repository.root, ".empty-profile-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(empty)
		digest, err = repository.installGeneration(ctx, profile, empty, layout, seeder)
		if err != nil {
			return err
		}
		return repository.writeCurrent(profile, digest)
	})
	return digest, err
}

func (repository *Repository) Import(ctx context.Context, profile Profile, sourceRoot string, seeder Seeder) (string, error) {
	if err := validateProfile(profile); err != nil {
		return "", err
	}
	if isSandboxProfile(profile) {
		return "", errors.New("global authentication import cannot include sandbox scope")
	}
	layout, err := validateSeeder(seeder)
	if err != nil {
		return "", err
	}
	var digest string
	err = repository.withProfileLock(ctx, profile, func() error {
		if _, found, err := repository.current(profile); err != nil {
			return err
		} else if found {
			return errors.New("authentication profile already exists")
		}
		var err error
		digest, err = repository.installGeneration(ctx, profile, sourceRoot, layout, seeder)
		if err != nil {
			return err
		}
		if err := repository.installReadOnlyConfig(ctx, profile, sourceRoot, layout); err != nil {
			return err
		}
		return repository.writeCurrent(profile, digest)
	})
	return digest, err
}

func (repository *Repository) Prepare(ctx context.Context, profile Profile, runID model.RunID, seeder Seeder) (Copy, error) {
	if err := validateProfile(profile); err != nil {
		return Copy{}, err
	}
	if isSandboxProfile(profile) {
		return Copy{}, errors.New("sandbox authentication requires PrepareSandbox")
	}
	return repository.prepareGlobal(ctx, profile, runID, "", "", seeder)
}

// PrepareGlobalSandbox creates an invocation copy from a global profile seed
// while retaining exact project and sandbox ownership for crash recovery.
func (repository *Repository) PrepareGlobalSandbox(ctx context.Context, profile Profile, runID model.RunID, projectID model.ProjectID, sandbox string, seeder Seeder) (Copy, error) {
	if err := validateProfile(profile); err != nil {
		return Copy{}, err
	}
	if isSandboxProfile(profile) {
		return Copy{}, errors.New("global authentication profile cannot include sandbox scope")
	}
	if err := validateSandboxScope(projectID, sandbox); err != nil {
		return Copy{}, err
	}
	return repository.prepareGlobal(ctx, profile, runID, projectID, sandbox, seeder)
}

func (repository *Repository) prepareGlobal(ctx context.Context, profile Profile, runID model.RunID, projectID model.ProjectID, sandbox string, seeder Seeder) (Copy, error) {
	if _, err := model.ParseRunID(string(runID)); err != nil {
		return Copy{}, err
	}
	layout, err := validateSeeder(seeder)
	if err != nil {
		return Copy{}, err
	}
	result := Copy{
		Profile: profile, RunID: runID, OwnerProjectID: projectID, OwnerSandbox: sandbox,
		ReadOnlyRoot: repository.readOnlyRoot(profile),
	}
	result.Root = repository.copyRunRoot(result)
	err = repository.withCopyLock(ctx, result, func() error {
		digest, found, err := repository.current(profile)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("authentication profile does not exist")
		}
		result.BaselineDigest = digest
		if _, err := os.Lstat(result.Root); err == nil {
			return errors.New("authentication run copy already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		source := repository.generationRoot(profile, digest)
		observed, err := fingerprint(ctx, source, layout)
		if err != nil {
			return err
		}
		if observed != digest {
			return errors.New("authentication seed digest mismatch")
		}
		return repository.seedRunCopy(ctx, result, source, layout, seeder)
	})
	return result, err
}
func (repository *Repository) PrepareSandbox(ctx context.Context, profile Profile, runID model.RunID, seeder Seeder) (Copy, error) {
	if err := validateProfile(profile); err != nil {
		return Copy{}, err
	}
	if !isSandboxProfile(profile) {
		return Copy{}, errors.New("sandbox authentication scope is required")
	}
	if _, err := model.ParseRunID(string(runID)); err != nil {
		return Copy{}, err
	}
	layout, err := validateSeeder(seeder)
	if err != nil {
		return Copy{}, err
	}
	result := Copy{
		Profile: profile, RunID: runID, OwnerProjectID: profile.ProjectID, OwnerSandbox: profile.Sandbox,
		ReadOnlyRoot: repository.readOnlyRoot(profile),
	}
	result.Root = repository.copyRunRoot(result)
	err = repository.withCopyLock(ctx, result, func() error {
		digest, found, err := repository.current(profile)
		if err != nil {
			return err
		}
		if !found {
			empty, err := os.MkdirTemp(repository.root, ".empty-sandbox-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(empty)
			digest, err = repository.installGeneration(ctx, profile, empty, layout, seeder)
			if err != nil {
				return err
			}
			if err := repository.writeCurrent(profile, digest); err != nil {
				return err
			}
		}
		result.BaselineDigest = digest
		if _, err := os.Lstat(result.Root); err == nil {
			return errors.New("authentication run copy already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		source := repository.generationRoot(profile, digest)
		observed, err := fingerprint(ctx, source, layout)
		if err != nil {
			return err
		}
		if observed != digest {
			return errors.New("authentication sandbox seed digest mismatch")
		}
		return repository.seedRunCopy(ctx, result, source, layout, seeder)
	})
	return result, err
}

func (repository *Repository) Refresh(ctx context.Context, copy Copy, sourceRoot string, seeder Seeder) error {
	if err := repository.validateCopyAuthority(copy); err != nil {
		return err
	}
	layout, err := validateSeeder(seeder)
	if err != nil {
		return err
	}
	return repository.withCopyLock(ctx, copy, func() error {
		if _, err := os.Lstat(copy.Root); err != nil {
			return fmt.Errorf("authentication run copy is not active: %w", err)
		}
		return replaceSnapshot(ctx, sourceRoot, copy.Root, layout, seeder)
	})
}

func (repository *Repository) Promote(ctx context.Context, copy Copy, seeder Seeder) (Promotion, error) {
	if err := repository.validateCopyAuthority(copy); err != nil {
		return Promotion{}, err
	}
	if err := validateDigest(copy.BaselineDigest); err != nil {
		return Promotion{}, fmt.Errorf("baseline: %w", err)
	}
	layout, err := validateSeeder(seeder)
	if err != nil {
		return Promotion{}, err
	}
	result := Promotion{}
	err = repository.withCopyLock(ctx, copy, func() error {
		current, found, err := repository.current(copy.Profile)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("authentication profile does not exist")
		}
		if current != copy.BaselineDigest {
			candidate := repository.copyCandidateRoot(copy)
			if err := replaceSnapshot(ctx, copy.Root, candidate, layout, seeder); err != nil {
				return err
			}
			result.Conflict = true
			result.CandidateRoot = candidate
			return nil
		}
		digest, err := repository.installGeneration(ctx, copy.Profile, copy.Root, layout, seeder)
		if err != nil {
			return err
		}
		if err := repository.writeCurrent(copy.Profile, digest); err != nil {
			return err
		}
		result.Digest = digest
		return nil
	})
	return result, err
}

func (repository *Repository) RemoveRun(ctx context.Context, copy Copy) error {
	if err := repository.validateCopyAuthority(copy); err != nil {
		return err
	}
	return repository.withCopyLock(ctx, copy, func() error {
		if err := removeExact(copy.Root); err != nil {
			return err
		}
		if copy.OwnerProjectID != "" {
			marker := repository.copyScopeMarker(copy)
			if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			ownerRoot := filepath.Join(
				repository.root, "runs", string(copy.RunID), "sandboxes",
				string(copy.OwnerProjectID), copy.OwnerSandbox,
			)
			if err := removeEmptyRunParents(filepath.Dir(marker), ownerRoot); err != nil {
				return err
			}
		}
		return removeEmptyRunParents(filepath.Dir(copy.Root), filepath.Join(repository.root, "runs"))
	})
}

func (repository *Repository) Purge(ctx context.Context, profile Profile) error {
	if err := validateProfile(profile); err != nil {
		return err
	}
	if isSandboxProfile(profile) {
		return errors.New("global authentication purge cannot include sandbox scope")
	}
	return repository.withProfileLock(ctx, profile, func() error {
		legacyRuns, scopedRuns, err := repository.matchingGlobalRoots("runs", profile)
		if err != nil {
			return err
		}
		if len(legacyRuns) != 0 {
			return fmt.Errorf("%w: %d legacy unscoped run copies cannot be associated with a sandbox; remove each exact run copy before purging the global profile", ErrActiveCopies, len(legacyRuns))
		}
		if len(scopedRuns) != 0 {
			return fmt.Errorf("%w: %d sandbox-scoped run copies require exact cleaned-sandbox purge", ErrActiveCopies, len(scopedRuns))
		}
		return removeExact(repository.profileRoot(profile))
	})
}

func (repository *Repository) PurgeSandbox(ctx context.Context, projectID model.ProjectID, sandbox string) error {
	if _, err := model.ParseProjectID(string(projectID)); err != nil {
		return err
	}
	if _, err := model.ParseSandboxName(sandbox); err != nil {
		return err
	}
	return repository.withSandboxLock(ctx, projectID, sandbox, func() error {
		active, err := repository.matchingSandboxRoots("runs", projectID, sandbox)
		if err != nil {
			return err
		}
		if len(active) != 0 {
			return fmt.Errorf("%w: %d", ErrActiveCopies, len(active))
		}
		sandboxRoot := repository.sandboxRoot(projectID, sandbox)
		if err := removeExact(sandboxRoot); err != nil {
			return err
		}
		return removeEmptyRunParents(filepath.Dir(sandboxRoot), filepath.Join(repository.root, "sandboxes"))
	})
}

// PurgeCleanedSandbox removes exact sandbox-owned profile material and abandoned
// run copies after the caller proves that the sandbox's runtime resources have
// been deleted. Global-profile conflict candidates are durable profile material.
func (repository *Repository) PurgeCleanedSandbox(ctx context.Context, projectID model.ProjectID, sandbox string) error {
	if _, err := model.ParseProjectID(string(projectID)); err != nil {
		return err
	}
	if _, err := model.ParseSandboxName(sandbox); err != nil {
		return err
	}
	return repository.withSandboxLock(ctx, projectID, sandbox, func() error {
		roots, err := repository.matchingSandboxRoots("runs", projectID, sandbox)
		if err != nil {
			return err
		}
		for _, root := range roots {
			if err := removeExact(root); err != nil {
				return err
			}
			if err := removeEmptyRunParents(filepath.Dir(root), filepath.Join(repository.root, "runs")); err != nil {
				return err
			}
		}
		sandboxRoot := repository.sandboxRoot(projectID, sandbox)
		if err := removeExact(sandboxRoot); err != nil {
			return err
		}
		return removeEmptyRunParents(filepath.Dir(sandboxRoot), filepath.Join(repository.root, "sandboxes"))
	})
}

func validateProfile(profile Profile) error {
	if _, err := harness.ParseName(string(profile.Harness)); err != nil {
		return err
	}
	if _, err := model.ParseSandboxName(profile.Name); err != nil {
		return fmt.Errorf("invalid profile: %w", err)
	}
	if profile.ProjectID == "" && profile.Sandbox == "" {
		return nil
	}
	if profile.ProjectID == "" || profile.Sandbox == "" {
		return errors.New("authentication sandbox scope requires project ID and sandbox name")
	}
	if _, err := model.ParseProjectID(string(profile.ProjectID)); err != nil {
		return err
	}
	if _, err := model.ParseSandboxName(profile.Sandbox); err != nil {
		return err
	}
	return nil
}

func validateSandboxScope(projectID model.ProjectID, sandbox string) error {
	if _, err := model.ParseProjectID(string(projectID)); err != nil {
		return err
	}
	if _, err := model.ParseSandboxName(sandbox); err != nil {
		return err
	}
	return nil
}

func (repository *Repository) validateCopyAuthority(copy Copy) error {
	if err := validateProfile(copy.Profile); err != nil {
		return err
	}
	if _, err := model.ParseRunID(string(copy.RunID)); err != nil {
		return err
	}
	hasProject := copy.OwnerProjectID != ""
	hasSandbox := copy.OwnerSandbox != ""
	if hasProject != hasSandbox {
		return errors.New("authentication copy ownership requires project ID and sandbox name")
	}
	if hasProject {
		if err := validateSandboxScope(copy.OwnerProjectID, copy.OwnerSandbox); err != nil {
			return err
		}
	}
	if isSandboxProfile(copy.Profile) {
		if copy.OwnerProjectID != copy.Profile.ProjectID || copy.OwnerSandbox != copy.Profile.Sandbox {
			return errors.New("authentication copy ownership does not match sandbox profile scope")
		}
	}
	if filepath.Clean(copy.Root) != repository.copyRunRoot(copy) {
		return errors.New("authentication copy path does not match repository authority")
	}
	if hasProject {
		scope, err := os.ReadFile(repository.copyScopeMarker(copy))
		if err != nil {
			return fmt.Errorf("authentication copy scope index is unavailable: %w", err)
		}
		expected := "global\n"
		if isSandboxProfile(copy.Profile) {
			expected = "sandbox\n"
		}
		if string(scope) != expected {
			return errors.New("authentication copy profile scope does not match its durable scope index")
		}
	}
	return nil
}

func isSandboxProfile(profile Profile) bool {
	return profile.ProjectID != "" && profile.Sandbox != ""
}
func validateSeeder(seeder Seeder) (harness.AuthLayout, error) {
	if seeder == nil {
		return harness.AuthLayout{}, errors.New("authentication seeder is required")
	}
	layout := seeder.AuthLayout()
	if err := validateLayout(layout); err != nil {
		return harness.AuthLayout{}, err
	}
	return layout, nil
}

func validateLayout(layout harness.AuthLayout) error {
	return harness.ValidateAuthLayout(layout)
}

func (repository *Repository) installGeneration(ctx context.Context, profile Profile, sourceRoot string, layout harness.AuthLayout, seeder Seeder) (string, error) {
	digest, err := fingerprint(ctx, sourceRoot, layout)
	if err != nil {
		return "", err
	}
	destination := repository.generationRoot(profile, digest)
	if info, err := os.Lstat(destination); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("authentication generation is not a real directory")
		}
		observed, fingerprintErr := fingerprint(ctx, destination, layout)
		if fingerprintErr != nil {
			return "", fingerprintErr
		}
		if observed != digest {
			return "", errors.New("authentication generation digest mismatch")
		}
		return digest, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := seedNewDirectory(ctx, sourceRoot, destination, layout, seeder); err != nil {
		return "", err
	}
	observed, err := fingerprint(ctx, destination, layout)
	if err != nil || observed != digest {
		_ = os.RemoveAll(destination)
		if err != nil {
			return "", err
		}
		return "", errors.New("authentication source changed during generation copy")
	}
	return digest, nil
}
func (repository *Repository) installReadOnlyConfig(ctx context.Context, profile Profile, sourceRoot string, layout harness.AuthLayout) error {
	artifacts := layout.ReadOnlyConfig
	if len(artifacts) == 0 {
		return nil
	}
	before, err := fingerprintReadOnly(ctx, sourceRoot, artifacts, layout.MaxArtifactBytes)
	if err != nil {
		return err
	}
	destination := repository.readOnlyRoot(profile)
	if _, err := os.Lstat(destination); err == nil {
		return errors.New("reviewed read-only configuration already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, ".reviewed-config-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, 0o700); err != nil {
		return err
	}
	request := harness.SeedRequest{
		SourceRoot:       sourceRoot,
		DestinationRoot:  staging,
		Artifacts:        append([]string(nil), artifacts...),
		MaxArtifactBytes: layout.MaxArtifactBytes,
	}
	if err := harness.SeedArtifacts(ctx, request); err != nil {
		return fmt.Errorf("copy reviewed read-only configuration: %w", err)
	}
	staged, err := fingerprintReadOnly(ctx, staging, artifacts, layout.MaxArtifactBytes)
	if err != nil {
		return err
	}
	after, err := fingerprintReadOnly(ctx, sourceRoot, artifacts, layout.MaxArtifactBytes)
	if err != nil {
		return err
	}
	if before != staged || before != after {
		return errors.New("reviewed configuration source changed during coherent copy")
	}
	for _, artifact := range artifacts {
		target := filepath.Join(staging, filepath.FromSlash(artifact))
		if err := os.Chmod(target, 0o400); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(staging, destination); err != nil {
		return err
	}
	return syncDirectory(parent)
}
func (repository *Repository) seedRunCopy(ctx context.Context, copy Copy, sourceRoot string, layout harness.AuthLayout, seeder Seeder) error {
	if err := seedNewDirectory(ctx, sourceRoot, copy.Root, layout, seeder); err != nil {
		return err
	}
	if copy.OwnerProjectID == "" {
		return nil
	}
	marker := repository.copyScopeMarker(copy)
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		return errors.Join(err, removeExact(copy.Root))
	}
	scope := "global\n"
	if isSandboxProfile(copy.Profile) {
		scope = "sandbox\n"
	}
	file, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.Join(err, removeExact(copy.Root))
	}
	if _, err := io.WriteString(file, scope); err != nil {
		return errors.Join(err, file.Close(), os.Remove(marker), removeExact(copy.Root))
	}
	if err := errors.Join(file.Sync(), file.Close(), syncDirectory(filepath.Dir(marker))); err != nil {
		return errors.Join(err, os.Remove(marker), removeExact(copy.Root))
	}
	return nil
}

func seedNewDirectory(ctx context.Context, sourceRoot, destination string, layout harness.AuthLayout, seeder Seeder) error {
	if _, err := os.Lstat(destination); err == nil {
		return errors.New("authentication snapshot destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	staging, err := stageSnapshot(ctx, sourceRoot, filepath.Dir(destination), layout, seeder)
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := os.Rename(staging, destination); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}

func replaceSnapshot(ctx context.Context, sourceRoot, destination string, layout harness.AuthLayout, seeder Seeder) error {
	staging, err := stageSnapshot(ctx, sourceRoot, filepath.Dir(destination), layout, seeder)
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	backup, err := os.MkdirTemp(filepath.Dir(destination), ".replaced-auth-")
	if err != nil {
		return err
	}
	if err := os.Remove(backup); err != nil {
		return err
	}
	defer os.RemoveAll(backup)
	if info, statErr := os.Lstat(destination); statErr == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("authentication snapshot destination is not a real directory")
		}
		if err := os.Rename(destination, backup); err != nil {
			return err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if err := os.Rename(staging, destination); err != nil {
		rollbackErr := os.Rename(backup, destination)
		return errors.Join(err, rollbackErr)
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return err
	}
	return removeExact(backup)
}

func stageSnapshot(ctx context.Context, sourceRoot, parent string, layout harness.AuthLayout, seeder Seeder) (string, error) {
	before, err := fingerprint(ctx, sourceRoot, layout)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(parent, ".auth-snapshot-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(staging, 0o700); err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}
	request := harness.SeedRequest{
		SourceRoot:       sourceRoot,
		DestinationRoot:  staging,
		Artifacts:        append([]string(nil), layout.CredentialArtifacts...),
		MaxArtifactBytes: layout.MaxArtifactBytes,
	}
	if err := seeder.Seed(ctx, request); err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}
	staged, err := fingerprint(ctx, staging, layout)
	if err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}
	after, err := fingerprint(ctx, sourceRoot, layout)
	if err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}
	if before != staged || before != after {
		_ = os.RemoveAll(staging)
		return "", errors.New("authentication source changed during coherent snapshot")
	}
	return staging, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func fingerprint(ctx context.Context, root string, layout harness.AuthLayout) (string, error) {
	return fingerprintArtifacts(ctx, root, layout.CredentialArtifacts, layout.MaxArtifactBytes, true)
}

func fingerprintReadOnly(ctx context.Context, root string, artifacts []string, maxArtifactBytes int64) (string, error) {
	return fingerprintArtifacts(ctx, root, artifacts, maxArtifactBytes, false)
}

func fingerprintArtifacts(ctx context.Context, root string, artifacts []string, maxArtifactBytes int64, requirePrivate bool) (string, error) {
	ordered := append([]string(nil), artifacts...)
	sort.Strings(ordered)
	hash := sha256.New()
	for _, artifact := range ordered {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		path := filepath.Join(root, filepath.FromSlash(artifact))
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			_, _ = io.WriteString(hash, artifact+"\x00missing\x00")
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || requirePrivate && info.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("authentication artifact %q must be a regular file with approved permissions", artifact)
		}
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		_, _ = io.WriteString(hash, artifact+"\x00file\x00")
		copied, copyErr := io.Copy(hash, io.LimitReader(file, maxArtifactBytes+1))
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return "", errors.Join(copyErr, closeErr)
		}
		if copied > maxArtifactBytes || info.Size() > maxArtifactBytes {
			return "", fmt.Errorf("authentication artifact %q exceeds size limit of %d bytes", artifact, maxArtifactBytes)
		}
		_, _ = io.WriteString(hash, "\x00")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (repository *Repository) current(profile Profile) (string, bool, error) {
	data, err := os.ReadFile(filepath.Join(repository.profileRoot(profile), currentFile))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	digest := strings.TrimSpace(string(data))
	if err := validateDigest(digest); err != nil {
		return "", false, fmt.Errorf("corrupt authentication profile pointer: %w", err)
	}
	return digest, true, nil
}

func validateDigest(value string) error {
	if len(value) != sha256.Size*2 {
		return errors.New("digest must be 64 lowercase hexadecimal characters")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || hex.EncodeToString(decoded) != value {
		return errors.New("digest must be 64 lowercase hexadecimal characters")
	}
	return nil
}

func (repository *Repository) writeCurrent(profile Profile, digest string) error {
	if err := validateDigest(digest); err != nil {
		return err
	}
	directory := repository.profileRoot(profile)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".current-")
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
	if err := os.Rename(temporaryPath, filepath.Join(directory, currentFile)); err != nil {
		return err
	}
	parent, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}

func (repository *Repository) withProfileLock(ctx context.Context, profile Profile, action func() error) error {
	if isSandboxProfile(profile) {
		return repository.withSandboxLock(ctx, profile.ProjectID, profile.Sandbox, action)
	}
	return repository.withFileLock(ctx, filepath.Join(repository.root, "locks", string(profile.Harness), profile.Name+".lock"), action)
}

func (repository *Repository) withCopyLock(ctx context.Context, copy Copy, action func() error) error {
	if isSandboxProfile(copy.Profile) || copy.OwnerProjectID == "" {
		return repository.withProfileLock(ctx, copy.Profile, action)
	}
	return repository.withSandboxLock(ctx, copy.OwnerProjectID, copy.OwnerSandbox, func() error {
		return repository.withProfileLock(ctx, copy.Profile, action)
	})
}

func (repository *Repository) withSandboxLock(ctx context.Context, projectID model.ProjectID, sandbox string, action func() error) error {
	return repository.withFileLock(ctx, filepath.Join(repository.root, "locks", "sandboxes", string(projectID), sandbox+".lock"), action)
}

func (repository *Repository) withFileLock(ctx context.Context, lockPath string, action func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
			return action()
		} else if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (repository *Repository) sandboxRoot(projectID model.ProjectID, sandbox string) string {
	return filepath.Join(repository.root, "sandboxes", string(projectID), sandbox)
}

func (repository *Repository) profileRoot(profile Profile) string {
	if isSandboxProfile(profile) {
		return filepath.Join(repository.sandboxRoot(profile.ProjectID, profile.Sandbox), "profiles", string(profile.Harness), profile.Name)
	}
	return filepath.Join(repository.root, "profiles", string(profile.Harness), profile.Name)
}

func (repository *Repository) readOnlyRoot(profile Profile) string {
	profile.ProjectID = ""
	profile.Sandbox = ""
	return filepath.Join(repository.profileRoot(profile), "read-only")
}

func (repository *Repository) generationRoot(profile Profile, digest string) string {
	return filepath.Join(repository.profileRoot(profile), "generations", digest)
}

func (repository *Repository) copyRunRoot(copy Copy) string {
	if copy.OwnerProjectID != "" && copy.OwnerSandbox != "" {
		return filepath.Join(
			repository.root, "runs", string(copy.RunID), "sandboxes",
			string(copy.OwnerProjectID), copy.OwnerSandbox, string(copy.Profile.Harness), copy.Profile.Name,
		)
	}
	return repository.runRoot(copy.Profile, copy.RunID)
}

func (repository *Repository) copyCandidateRoot(copy Copy) string {
	return repository.candidateRoot(copy.Profile, copy.RunID)
}

func (repository *Repository) copyScopeMarker(copy Copy) string {
	return filepath.Join(
		repository.root, "runs", string(copy.RunID), "sandboxes",
		string(copy.OwnerProjectID), copy.OwnerSandbox, ".copy-scopes",
		string(copy.Profile.Harness), copy.Profile.Name,
	)
}

func (repository *Repository) runRoot(profile Profile, runID model.RunID) string {
	parent := filepath.Join(repository.root, "runs", string(runID))
	if isSandboxProfile(profile) {
		return filepath.Join(parent, "sandboxes", string(profile.ProjectID), profile.Sandbox, string(profile.Harness), profile.Name)
	}
	return filepath.Join(parent, string(profile.Harness), profile.Name)
}

func (repository *Repository) candidateRoot(profile Profile, runID model.RunID) string {
	return filepath.Join(repository.profileRoot(profile), "conflicts", string(runID))
}

func (repository *Repository) matchingGlobalRoots(kind string, profile Profile) (legacy, scoped []string, err error) {
	parent := filepath.Join(repository.root, kind)
	entries, err := os.ReadDir(parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil, nil, fmt.Errorf("authentication %s run root is not a real directory", kind)
		}
		runRoot := filepath.Join(parent, entry.Name())
		candidate := filepath.Join(runRoot, string(profile.Harness), profile.Name)
		if info, err := os.Lstat(candidate); err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return nil, nil, fmt.Errorf("authentication %s legacy copy is not a real directory", kind)
			}
			legacy = append(legacy, candidate)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
		scopedRoot := filepath.Join(runRoot, "sandboxes")
		projects, err := os.ReadDir(scopedRoot)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		for _, project := range projects {
			if !project.IsDir() || project.Type()&os.ModeSymlink != 0 {
				return nil, nil, fmt.Errorf("authentication %s scoped project root is not a real directory", kind)
			}
			projectRoot := filepath.Join(scopedRoot, project.Name())
			sandboxes, err := os.ReadDir(projectRoot)
			if err != nil {
				return nil, nil, err
			}
			for _, sandbox := range sandboxes {
				if !sandbox.IsDir() || sandbox.Type()&os.ModeSymlink != 0 {
					return nil, nil, fmt.Errorf("authentication %s scoped sandbox root is not a real directory", kind)
				}
				candidate := filepath.Join(projectRoot, sandbox.Name(), string(profile.Harness), profile.Name)
				if info, err := os.Lstat(candidate); err == nil {
					if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
						return nil, nil, fmt.Errorf("authentication %s scoped copy is not a real directory", kind)
					}
					scoped = append(scoped, candidate)
				} else if !errors.Is(err, os.ErrNotExist) {
					return nil, nil, err
				}
			}
		}
	}
	sort.Strings(legacy)
	sort.Strings(scoped)
	return legacy, scoped, nil
}

func (repository *Repository) matchingSandboxRoots(kind string, projectID model.ProjectID, sandbox string) ([]string, error) {
	parent := filepath.Join(repository.root, kind)
	entries, err := os.ReadDir(parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	matches := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			return nil, fmt.Errorf("authentication %s run root is not a directory", kind)
		}
		candidate := filepath.Join(parent, entry.Name(), "sandboxes", string(projectID), sandbox)
		info, err := os.Lstat(candidate)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("authentication %s sandbox root is not a real directory", kind)
			}
			matches = append(matches, candidate)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	sort.Strings(matches)
	return matches, nil
}

func removeExact(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to remove non-directory authentication path")
	}
	return os.RemoveAll(path)
}

func removeEmptyRunParents(directory, stop string) error {
	stop = filepath.Clean(stop)
	for current := filepath.Clean(directory); current != stop; current = filepath.Dir(current) {
		if current == "." || current == string(filepath.Separator) || !strings.HasPrefix(current, stop+string(filepath.Separator)) {
			return errors.New("authentication run parent escapes repository authority")
		}
		err := os.Remove(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}
