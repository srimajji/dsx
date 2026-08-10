package fs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/gitx"
	"github.com/srimajji/dsx/internal/model"
	"github.com/srimajji/dsx/internal/state"
)

func TestManifestPermissionsSymlinkAndCreateNoReplace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	repository, err := NewManifestRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := testManifest(t, "/tmp/dsx-manifest-permissions", "main", "01890f5c-7b00-7000-8000-000000000001")
	if err := repository.CreateIntent(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	path, err := repository.manifestPath(manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := manifest
	changed.Failure = "must not replace"
	if err := repository.CreateIntent(context.Background(), changed); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatalf("CreateIntent duplicate error = %v, want conflict", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("duplicate CreateIntent changed existing manifest bytes")
	}
	if err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		want := os.FileMode(0o600)
		if info.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			return fmt.Errorf("%s mode = %04o, want %04o", path, info.Mode().Perm(), want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(t.TempDir(), "outside.json")
	outsideData := []byte("outside must survive\n")
	if err := os.WriteFile(outside, outsideData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID); err == nil {
		t.Fatal("LoadManifest accepted a symlink")
	}
	preserved, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(preserved, outsideData) {
		t.Fatal("symlink target was changed")
	}

	realRoot := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManifestRepository(linkedRoot); err == nil {
		t.Fatal("NewManifestRepository accepted a symlink root")
	}
}
func TestManifestRoundTripsUncapturedCloneWork(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	repository, err := NewManifestRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	projectRoot := "/tmp/dsx-uncaptured-roundtrip"
	manifest := testManifest(t, projectRoot, "uncaptured", "01890f5c-7b00-7000-8000-000000000099")
	manifest.Mode = model.ModeClone
	manifest.Git = []state.GitRecord{{
		Repository:         "workspace",
		HostPath:           projectRoot,
		GuestPath:          "/workspace",
		Identity:           fsManifestRepositoryIdentity(projectRoot),
		SourceRef:          "refs/heads/main",
		SourceCommit:       strings.Repeat("1", 40),
		TrackedFingerprint: strings.Repeat("2", 64),
		ResultBranch:       "dsx/uncaptured",
		SourceBundleDigest: strings.Repeat("3", 64),
	}}
	if err := repository.CreateIntent(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	replacement := manifest
	replacement.State = model.StateCreating
	replacement.UncapturedWork = true
	replacement.UpdatedAt = replacement.UpdatedAt.Add(time.Second)
	if err := repository.ReplaceManifest(context.Background(), replacement, manifest.Generation); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := repository.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if err != nil || !found {
		t.Fatalf("LoadManifest() = %v, found=%t", err, found)
	}
	if !loaded.UncapturedWork || loaded.State != model.StateCreating || loaded.Generation != 2 {
		t.Fatalf("uncaptured work was not durable: %#v", loaded)
	}
}

func fsManifestRepositoryIdentity(worktree string) gitx.RepositoryIdentity {
	pathIdentity := func(value string) gitx.PhysicalPathIdentity {
		parts := strings.Split(strings.TrimPrefix(value, "/"), "/")
		components := []gitx.PathComponentIdentity{{Path: "/", Device: 1, Inode: 1}}
		current := ""
		for index, part := range parts {
			current += "/" + part
			components = append(components, gitx.PathComponentIdentity{Path: current, Device: 1, Inode: uint64(index + 2)})
		}
		return gitx.PhysicalPathIdentity{CanonicalPath: value, Components: components}
	}
	return gitx.RepositoryIdentity{ApprovedRoot: pathIdentity(worktree), Worktree: pathIdentity(worktree), GitDir: pathIdentity(worktree + "/.git")}
}

func TestAtomicReplaceFailpointsAndConflict(t *testing.T) {
	repository, manifest, path := createTestIntent(t, "/tmp/dsx-atomic")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := manifest
	replacement.State = model.StateCreating
	replacement.UpdatedAt = replacement.UpdatedAt.Add(time.Second)
	injected := errors.New("injected atomic failure")
	repository.beforeRename = func() error { return injected }
	if err := repository.ReplaceManifest(context.Background(), replacement, 1); !errors.Is(err, injected) {
		t.Fatalf("ReplaceManifest before-rename error = %v", err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(unchanged, original) {
		t.Fatal("before-rename failure changed manifest bytes")
	}

	repository.beforeRename = nil
	repository.afterRename = func() error { return injected }
	if err := repository.ReplaceManifest(context.Background(), replacement, 1); !errors.Is(err, injected) {
		t.Fatalf("ReplaceManifest after-rename error = %v", err)
	}
	repository.afterRename = nil
	loaded, found, err := repository.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if err != nil || !found {
		t.Fatalf("LoadManifest after rename = found %v, error %v", found, err)
	}
	if loaded.Generation != 2 || loaded.State != model.StateCreating {
		t.Fatalf("loaded replacement = generation %d state %q", loaded.Generation, loaded.State)
	}
	published, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceManifest(context.Background(), replacement, 1); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatalf("stale ReplaceManifest error = %v, want conflict", err)
	}
	afterConflict, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterConflict, published) {
		t.Fatal("generation conflict changed manifest bytes")
	}
}

func TestManifestTempRecoveryDurableContinuation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	repository, err := NewManifestRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	manifests := []state.Manifest{
		testManifest(t, "/tmp/dsx-temp-recovery", "main", "01890f5c-7b00-7000-8000-000000000001"),
		testManifest(t, "/tmp/dsx-temp-recovery", "main", "01890f5c-7b00-7000-8000-000000000002"),
	}
	for _, manifest := range manifests {
		if err := repository.CreateIntent(context.Background(), manifest); err != nil {
			t.Fatal(err)
		}
	}
	manifestPath, err := repository.manifestPath(manifests[0].ProjectID, manifests[0].Sandbox, manifests[0].RunID)
	if err != nil {
		t.Fatal(err)
	}
	sandboxDirectory := filepath.Dir(manifestPath)
	temporary, err := os.CreateTemp(sandboxDirectory, atomicWriteTempPattern)
	if err != nil {
		t.Fatal(err)
	}
	temporaryPath := temporary.Name()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		t.Fatal(err)
	}
	if _, err := temporary.Write([]byte("never authoritative\n")); err != nil {
		_ = temporary.Close()
		t.Fatal(err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		t.Fatal(err)
	}
	if err := temporary.Close(); err != nil {
		t.Fatal(err)
	}
	if err := syncDirectory(sandboxDirectory); err != nil {
		t.Fatal(err)
	}
	directorySyncs := 0
	repository.syncDirectory = func(path string) error {
		directorySyncs++
		return syncDirectory(path)
	}

	projectManifests, err := repository.ListProjectManifests(context.Background(), manifests[0].ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projectManifests) != len(manifests) {
		t.Fatalf("ListProjectManifests returned %d valid manifests, want %d", len(projectManifests), len(manifests))
	}
	if _, err := os.Lstat(temporaryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("atomic-write crash residue still exists: %v", err)
	}
	if directorySyncs != 1 {
		t.Fatalf("crash-residue removal synced directory %d times, want 1", directorySyncs)
	}
	allManifests, err := repository.ListAllManifests(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(allManifests) != len(manifests) {
		t.Fatalf("ListAllManifests returned %d valid manifests, want %d", len(allManifests), len(manifests))
	}
}

func TestManifestTempRecoveryRejectsSymlinkAndUnknownEntry(t *testing.T) {
	t.Run("matching symlink", func(t *testing.T) {
		repository, manifest, manifestPath := createTestIntent(t, "/tmp/dsx-temp-symlink")
		outsidePath := filepath.Join(t.TempDir(), "outside")
		outsideData := []byte("must survive\n")
		if err := os.WriteFile(outsidePath, outsideData, 0o600); err != nil {
			t.Fatal(err)
		}
		temporaryPath := filepath.Join(filepath.Dir(manifestPath), atomicWriteTempPrefix+"1234567890")
		if err := os.Symlink(outsidePath, temporaryPath); err != nil {
			t.Fatal(err)
		}
		if _, err := repository.ListProjectManifests(context.Background(), manifest.ProjectID); err == nil {
			t.Fatal("manifest listing removed or ignored a symlink with an atomic-write-like name")
		}
		info, err := os.Lstat(temporaryPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("malicious symlink was not preserved")
		}
		preserved, err := os.ReadFile(outsidePath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(preserved, outsideData) {
			t.Fatal("malicious symlink target was changed")
		}
	})

	t.Run("ambiguous regular entry", func(t *testing.T) {
		repository, _, manifestPath := createTestIntent(t, "/tmp/dsx-temp-unknown")
		unknownPath := filepath.Join(filepath.Dir(manifestPath), atomicWriteTempPrefix+"not-ours")
		unknownData := []byte("ambiguous\n")
		if err := os.WriteFile(unknownPath, unknownData, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := repository.ListAllManifests(context.Background()); err == nil {
			t.Fatal("manifest listing ignored an ambiguous temporary entry")
		}
		preserved, err := os.ReadFile(unknownPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(preserved, unknownData) {
			t.Fatal("ambiguous temporary entry was changed")
		}
	})
}

func TestRecoveryCorruptManifestPreserved(t *testing.T) {
	tests := map[string][]byte{
		"duplicate": []byte(`{"version":1,"version":1}`),
		"unknown":   []byte(`{"unknown":true}`),
		"trailing":  []byte(`{} {}`),
		"malformed": []byte(`{"version":`),
	}
	for name, corrupt := range tests {
		t.Run(name, func(t *testing.T) {
			repository, manifest, path := createTestIntent(t, "/tmp/dsx-recovery-"+name)
			corrupt = append(append([]byte(nil), corrupt...), '\n')
			if err := os.WriteFile(path, corrupt, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := repository.LoadManifest(context.Background(), manifest.ProjectID, manifest.Sandbox, manifest.RunID); err == nil {
				t.Fatal("LoadManifest accepted corrupt data")
			}
			if _, err := repository.ListAllManifests(context.Background()); err == nil {
				t.Fatal("ListAllManifests silently skipped corrupt data")
			}
			preserved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(preserved, corrupt) {
				t.Fatal("corrupt manifest was not preserved byte-for-byte")
			}
		})
	}
}

func TestManifestListingDeterministicAndDeleteExact(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	repository, err := NewManifestRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	inputs := []state.Manifest{
		testManifest(t, "/tmp/dsx-list-b", "zeta", "01890f5c-7b00-7000-8000-000000000003"),
		testManifest(t, "/tmp/dsx-list-a", "main", "01890f5c-7b00-7000-8000-000000000002"),
		testManifest(t, "/tmp/dsx-list-a", "main", "01890f5c-7b00-7000-8000-000000000001"),
	}
	for _, manifest := range inputs {
		if err := repository.CreateIntent(context.Background(), manifest); err != nil {
			t.Fatal(err)
		}
	}
	all, err := repository.ListAllManifests(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("ListAllManifests returned %d records", len(all))
	}
	for index := 1; index < len(all); index++ {
		previous := string(all[index-1].ProjectID) + "/" + string(all[index-1].Sandbox) + "/" + string(all[index-1].RunID)
		current := string(all[index].ProjectID) + "/" + string(all[index].Sandbox) + "/" + string(all[index].RunID)
		if previous >= current {
			t.Fatalf("listing is not deterministic: %q before %q", previous, current)
		}
	}
	project, err := repository.ListProjectManifests(context.Background(), inputs[1].ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(project) != 2 || project[0].RunID >= project[1].RunID {
		t.Fatalf("project listing order = %#v", project)
	}
	if err := repository.DeleteManifest(context.Background(), inputs[1].ProjectID, inputs[1].Sandbox, inputs[1].RunID); err != nil {
		t.Fatal(err)
	}
	if err := repository.DeleteManifest(context.Background(), inputs[1].ProjectID, inputs[1].Sandbox, inputs[1].RunID); err != nil {
		t.Fatalf("idempotent DeleteManifest = %v", err)
	}

	remainingPath, err := repository.manifestPath(inputs[2].ProjectID, inputs[2].Sandbox, inputs[2].RunID)
	if err != nil {
		t.Fatal(err)
	}
	unexpected := filepath.Join(filepath.Dir(remainingPath), "unexpected")
	if err := os.WriteFile(unexpected, []byte("not a manifest"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ListProjectManifests(context.Background(), inputs[2].ProjectID); err == nil {
		t.Fatal("listing silently skipped an unexpected entry")
	}
}

func TestManifestInventoryCountsWriteAheadIntent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	repository, err := NewManifestRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	empty := testManifest(t, "/tmp/dsx-inventory", "main", "01890f5c-7b00-7000-8000-000000000001")
	withResources := testManifest(t, "/tmp/dsx-inventory", "clone", "01890f5c-7b00-7000-8000-000000000002")
	withResources.Resources = []state.ResourceRecord{
		testResource(withResources, "workspace", "workspace"),
		testResource(withResources, "network", "network"),
	}
	for _, manifest := range []state.Manifest{empty, withResources} {
		if err := repository.CreateIntent(context.Background(), manifest); err != nil {
			t.Fatal(err)
		}
	}
	count, err := repository.CountOwnedResources(context.Background(), empty.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("CountOwnedResources() = %d, want intent sentinel plus two resources", count)
	}
}

func TestManifestValidationResources(t *testing.T) {
	manifest := testManifest(t, "/tmp/dsx-resource-validation", "main", "01890f5c-7b00-7000-8000-000000000001")
	manifest.Resources = []state.ResourceRecord{testResource(manifest, "workspace", "workspace")}
	if err := state.ValidateManifest(manifest); err != nil {
		t.Fatalf("valid resource rejected: %v", err)
	}
	manifest.Resources[0].Labels[4].Value = "01890f5c-7b00-7000-8000-000000000002"
	if err := state.ValidateManifest(manifest); err == nil {
		t.Fatal("resource with contradictory run label accepted")
	}
}

func TestDeletedManifestRequiresInspectedResourcePostconditions(t *testing.T) {
	manifest := testManifest(t, "/tmp/dsx-terminal-resource-validation", "main", "01890f5c-7b00-7000-8000-000000000001")
	manifest.State = model.StateDeleted
	manifest.Operation = "clean"
	manifest.Resources = []state.ResourceRecord{testResource(manifest, "workspace", "workspace")}
	if err := state.ValidateManifest(manifest); err == nil {
		t.Fatal("deleted manifest accepted an unresolved write-ahead resource")
	}
	manifest.Resources[0].Absent = true
	if err := state.ValidateManifest(manifest); err != nil {
		t.Fatalf("deleted manifest rejected a proven-absent resource: %v", err)
	}
	manifest.Resources[0].Absent = false
	manifest.Resources[0].Created = true
	manifest.Resources[0].RuntimeID = manifest.Resources[0].ExpectedID
	manifest.Resources[0].Deleted = true
	if err := state.ValidateManifest(manifest); err != nil {
		t.Fatalf("deleted manifest rejected a proven-deleted resource: %v", err)
	}
}

func TestLockCrossProcessContentionCancellationRelease(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	repository, err := NewManifestRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := model.NewProjectID("/tmp/dsx-lock-project")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := repository.LockProject(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	output, err := runLockHelper(t, root, projectID, "150ms")
	if err == nil || !strings.Contains(output, string(model.CodeUnavailable)) {
		t.Fatalf("contending process = output %q, error %v", output, err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	output, err = runLockHelper(t, root, projectID, "2s")
	if err != nil || !strings.Contains(output, "acquired") {
		t.Fatalf("released lock helper = output %q, error %v", output, err)
	}
	lockPath := filepath.Join(root, projectLockDirectory, string(projectID)+".lock")
	info, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock mode = %v, want regular 0600", info.Mode())
	}
}

func TestSandboxLockIsCrossProcessScopedCrashReleasingAndPrivate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	repository, err := NewManifestRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := model.NewProjectID("/tmp/dsx-sandbox-lock-project")
	if err != nil {
		t.Fatal(err)
	}
	first, err := model.ParseSandboxName("first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := model.ParseSandboxName("second")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := repository.LockSandbox(context.Background(), projectID, first)
	if err != nil {
		t.Fatal(err)
	}
	output, err := runSandboxLockHelper(t, root, projectID, first, "150ms", false)
	if err == nil || !strings.Contains(output, string(model.CodeUnavailable)) {
		t.Fatalf("same-sandbox contending process = output %q, error %v", output, err)
	}
	output, err = runSandboxLockHelper(t, root, projectID, second, "2s", false)
	if err != nil || !strings.Contains(output, "acquired") {
		t.Fatalf("different-sandbox process contended = output %q, error %v", output, err)
	}
	otherProjectID, err := model.NewProjectID("/tmp/dsx-other-sandbox-lock-project")
	if err != nil {
		t.Fatal(err)
	}
	output, err = runSandboxLockHelper(t, root, otherProjectID, first, "2s", false)
	if err != nil || !strings.Contains(output, "acquired") {
		t.Fatalf("same-name different-project process contended = output %q, error %v", output, err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	output, err = runSandboxLockHelper(t, root, projectID, first, "2s", true)
	if err == nil || !strings.Contains(output, "acquired") {
		t.Fatalf("abruptly exiting lock holder = output %q, error %v", output, err)
	}
	recovered, err := repository.LockSandbox(context.Background(), projectID, first)
	if err != nil {
		t.Fatalf("lock after process crash = %v", err)
	}
	if err := recovered.Unlock(); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, sandboxLockDirectory, string(projectID), string(first)+".lock")
	info, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("sandbox lock mode = %v, want regular 0600", info.Mode())
	}
	directoryInfo, err := os.Lstat(filepath.Dir(lockPath))
	if err != nil {
		t.Fatal(err)
	}
	if !directoryInfo.IsDir() || directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("sandbox lock directory mode = %v, want directory 0700", directoryInfo.Mode())
	}
}

func TestSandboxLockRejectsPathEscapeNames(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	repository, err := NewManifestRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := model.NewProjectID("/tmp/dsx-sandbox-lock-validation")
	if err != nil {
		t.Fatal(err)
	}
	for _, sandbox := range []model.SandboxName{"../escape", "/escape", "name.lock", "Upper"} {
		if _, err := repository.LockSandbox(context.Background(), projectID, sandbox); model.ErrorCodeOf(err) != model.CodeInvalidInput {
			t.Errorf("LockSandbox(%q) error = %v, want invalid input", sandbox, err)
		}
	}
	if _, err := repository.LockSandbox(context.Background(), model.ProjectID("../escape"), "valid"); model.ErrorCodeOf(err) != model.CodeInvalidInput {
		t.Errorf("LockSandbox(path-escaping project ID) error = %v, want invalid input", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "escape.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path-escaped lock exists or cannot be inspected: %v", err)
	}
}

func TestLockContentionBoundedWithOwnerDiagnostic(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	repository, err := NewManifestRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := model.NewProjectID("/tmp/dsx-bounded-lock")
	if err != nil {
		t.Fatal(err)
	}
	holder, err := repository.LockProject(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := holder.Unlock(); err != nil {
			t.Error(err)
		}
	}()
	lockPath := filepath.Join(root, projectLockDirectory, string(projectID)+".lock")
	ownerFile, _, err := openLockFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	ownerDiagnostic := readLockOwner(ownerFile)
	if err := ownerFile.Close(); err != nil {
		t.Fatal(err)
	}
	if ownerDiagnostic == "" {
		t.Fatal("lock holder did not publish owner metadata")
	}
	repository.projectLockWait = 60 * time.Millisecond
	started := time.Now()
	_, err = repository.LockProject(context.Background(), projectID)
	elapsed := time.Since(started)
	if model.ErrorCodeOf(err) != model.CodeUnavailable || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded contention error = %v, want unavailable deadline", err)
	}
	if !strings.Contains(err.Error(), ownerDiagnostic) {
		t.Fatalf("bounded contention error %q omits owner %q", err, ownerDiagnostic)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("bounded contention took %s, want no more than 500ms", elapsed)
	}
}

func TestLockContentionUsesEarlierCallerCancellation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	repository, err := NewManifestRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := model.NewProjectID("/tmp/dsx-cancelled-lock")
	if err != nil {
		t.Fatal(err)
	}
	holder, err := repository.LockProject(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := holder.Unlock(); err != nil {
			t.Error(err)
		}
	}()
	repository.projectLockWait = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(30*time.Millisecond, cancel)
	defer timer.Stop()
	started := time.Now()
	_, err = repository.LockProject(ctx, projectID)
	elapsed := time.Since(started)
	if model.ErrorCodeOf(err) != model.CodeUnavailable || !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation error = %v, want unavailable cancellation", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("caller cancellation took %s, want no more than 500ms", elapsed)
	}
}

func TestLockDoubleUnlock(t *testing.T) {
	repository, err := NewManifestRepository(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := model.NewProjectID("/tmp/dsx-double-unlock")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := repository.LockProject(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatalf("second Unlock = %v", err)
	}
}

func TestLockProcessHelper(t *testing.T) {
	if os.Getenv("DSX_LOCK_HELPER") != "1" {
		return
	}
	root := os.Getenv("DSX_LOCK_ROOT")
	projectID := model.ProjectID(os.Getenv("DSX_LOCK_PROJECT"))
	sandbox := model.SandboxName(os.Getenv("DSX_LOCK_SANDBOX"))
	timeout, err := time.ParseDuration(os.Getenv("DSX_LOCK_TIMEOUT"))
	if err != nil {
		fmt.Println(model.CodeInternal)
		os.Exit(2)
	}
	repository, err := NewManifestRepository(root)
	if err != nil {
		fmt.Println(model.ErrorCodeOf(err))
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var lock state.ProjectLock
	if sandbox == "" {
		lock, err = repository.LockProject(ctx, projectID)
	} else {
		lock, err = repository.LockSandbox(ctx, projectID, sandbox)
	}
	if err != nil {
		fmt.Println(model.ErrorCodeOf(err))
		os.Exit(3)
	}
	fmt.Println("acquired")
	if os.Getenv("DSX_LOCK_CRASH") == "1" {
		os.Exit(9)
	}
	if err := lock.Unlock(); err != nil {
		os.Exit(4)
	}
}

func runLockHelper(t *testing.T, root string, projectID model.ProjectID, timeout string) (string, error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestLockProcessHelper$")
	command.Env = append(os.Environ(),
		"DSX_LOCK_HELPER=1",
		"DSX_LOCK_ROOT="+root,
		"DSX_LOCK_PROJECT="+string(projectID),
		"DSX_LOCK_TIMEOUT="+timeout,
	)
	output, err := command.CombinedOutput()
	return string(output), err
}

func runSandboxLockHelper(t *testing.T, root string, projectID model.ProjectID, sandbox model.SandboxName, timeout string, crash bool) (string, error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestLockProcessHelper$")
	command.Env = append(os.Environ(),
		"DSX_LOCK_HELPER=1",
		"DSX_LOCK_ROOT="+root,
		"DSX_LOCK_PROJECT="+string(projectID),
		"DSX_LOCK_SANDBOX="+string(sandbox),
		"DSX_LOCK_TIMEOUT="+timeout,
	)
	if crash {
		command.Env = append(command.Env, "DSX_LOCK_CRASH=1")
	}
	output, err := command.CombinedOutput()
	return string(output), err
}

func createTestIntent(t *testing.T, canonicalRoot string) (*ManifestRepository, state.Manifest, string) {
	t.Helper()
	repository, err := NewManifestRepository(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := testManifest(t, canonicalRoot, "main", "01890f5c-7b00-7000-8000-000000000001")
	if err := repository.CreateIntent(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	path, err := repository.manifestPath(manifest.ProjectID, manifest.Sandbox, manifest.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return repository, manifest, path
}

func testManifest(t *testing.T, canonicalRoot, sandboxValue, runValue string) state.Manifest {
	t.Helper()
	projectID, err := model.NewProjectID(canonicalRoot)
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := model.ParseSandboxName(sandboxValue)
	if err != nil {
		t.Fatal(err)
	}
	runID, err := model.ParseRunID(runValue)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	return state.Manifest{
		Version:       state.ManifestVersion,
		Generation:    1,
		ProjectID:     projectID,
		CanonicalRoot: canonicalRoot,
		Sandbox:       sandbox,
		RunID:         runID,
		Mode:          model.ModeLive,
		PlanHash:      strings.Repeat("a", 64),
		State:         model.StatePlanned,
		Operation:     "create",
		Resources:     []state.ResourceRecord{},
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

func testResource(manifest state.Manifest, kind, role string) state.ResourceRecord {
	name := state.CanonicalResourceName(manifest.ProjectID, manifest.Sandbox, role)
	return state.ResourceRecord{
		Kind:       kind,
		Role:       role,
		Name:       name,
		ExpectedID: name,
		Labels:     state.ResourceOwnershipLabels(manifest.ProjectID, manifest.Sandbox, manifest.RunID, kind, role),
	}
}
