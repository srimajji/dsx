package gitx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestPrepareSourceRefusesDirtyTrackedAndReportsWarnings(t *testing.T) {
	t.Parallel()
	fixture := newRepository(t)
	writeFile(t, filepath.Join(fixture.path, ".gitignore"), "ignored.log\n")
	writeFile(t, filepath.Join(fixture.path, "tracked.txt"), "tracked\n")
	gitTest(t, fixture.path, "add", "-A")
	gitTest(t, fixture.path, "commit", "-m", "source")
	writeFile(t, filepath.Join(fixture.path, "untracked.txt"), "untracked\n")
	writeFile(t, filepath.Join(fixture.path, "ignored.log"), "ignored\n")

	artifact, err := fixture.service.PrepareSource(context.Background(), SourceRequest{Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "alpha", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("PrepareSource() error = %v", err)
	}
	if !artifact.WarnUntracked || !artifact.WarnIgnored {
		t.Fatalf("warnings = untracked:%v ignored:%v", artifact.WarnUntracked, artifact.WarnIgnored)
	}
	info, err := os.Lstat(artifact.BundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("bundle mode = %v", info.Mode())
	}
	if err := fixture.service.VerifyBundle(context.Background(), artifact.BundlePath, artifact.BundleDigest); err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if err := fixture.service.RemoveArtifact(artifact.BundlePath); err != nil {
		t.Fatalf("RemoveArtifact() error = %v", err)
	}

	writeFile(t, filepath.Join(fixture.path, "tracked.txt"), "dirty\n")
	_, err = fixture.service.PrepareSource(context.Background(), SourceRequest{Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "alpha", TempRoot: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "tracked or index changes") {
		t.Fatalf("dirty PrepareSource() error = %v", err)
	}
}

func TestPrepareSourceSnapshotCapturesFinalWorktreeWithoutHostMutation(t *testing.T) {
	fixture := newRepository(t)
	writeFile(t, filepath.Join(fixture.path, ".gitignore"), "ignored.log\ntracked-ignored.txt\n")
	writeFile(t, filepath.Join(fixture.path, "tracked.txt"), "original\n")
	writeFile(t, filepath.Join(fixture.path, "rename-source.txt"), "rename source\n")
	writeFile(t, filepath.Join(fixture.path, "deleted.txt"), "delete me\n")
	writeFile(t, filepath.Join(fixture.path, "binary.dat"), []byte{0, 1, 2, 3})
	writeFile(t, filepath.Join(fixture.path, "tracked-ignored.txt"), "tracked ignored original\n")
	gitTest(t, fixture.path, "add", ".gitignore", "tracked.txt", "rename-source.txt", "deleted.txt", "binary.dat")
	gitTest(t, fixture.path, "add", "-f", "tracked-ignored.txt")
	gitTest(t, fixture.path, "commit", "-m", "source")
	sourceHead := strings.TrimSpace(gitTest(t, fixture.path, "rev-parse", "HEAD"))

	writeFile(t, filepath.Join(fixture.path, "tracked.txt"), "staged\n")
	gitTest(t, fixture.path, "add", "tracked.txt")
	writeFile(t, filepath.Join(fixture.path, "tracked.txt"), "final unstaged\n")
	gitTest(t, fixture.path, "rm", "deleted.txt")
	gitTest(t, fixture.path, "mv", "rename-source.txt", "renamed.txt")
	writeFile(t, filepath.Join(fixture.path, "renamed.txt"), "renamed final\n")
	writeFile(t, filepath.Join(fixture.path, "binary.dat"), []byte{0, 9, 0xff, 4})
	writeFile(t, filepath.Join(fixture.path, "tracked-ignored.txt"), "tracked ignored final\n")
	if err := os.Mkdir(filepath.Join(fixture.path, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fixture.path, "nested", "untracked.txt"), "nested untracked\n")
	if err := os.Symlink("nested/untracked.txt", filepath.Join(fixture.path, "untracked-link")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fixture.path, "ignored.log"), "host only\n")
	gitTest(t, fixture.path, "config", "user.name", "Hostile User")
	gitTest(t, fixture.path, "config", "user.email", "hostile@example.invalid")
	t.Setenv("GIT_AUTHOR_NAME", "Ambient Hostile")
	t.Setenv("GIT_AUTHOR_EMAIL", "ambient@example.invalid")
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(t.TempDir(), "hostile-objects"))

	before := hostByteSnapshot(t, fixture.path)
	tempRoot := t.TempDir()
	request := SourceRequest{
		Repository: fixture.repository(), ApprovedRoot: fixture.path,
		Workspace: "snapshot", TempRoot: tempRoot, Snapshot: true,
	}
	artifact, err := fixture.service.PrepareSource(context.Background(), request)
	if err != nil {
		t.Fatalf("PrepareSource(snapshot) error = %v", err)
	}
	assertHostByteSnapshot(t, fixture.path, before)
	if !artifact.SourceSnapshot || artifact.SourceHeadRevision != sourceHead || artifact.SourceTree == "" {
		t.Fatalf("snapshot provenance = %#v", artifact)
	}
	if artifact.WarnUntracked || !artifact.WarnIgnored {
		t.Fatalf("snapshot warnings = untracked:%v ignored:%v", artifact.WarnUntracked, artifact.WarnIgnored)
	}
	entries, err := os.ReadDir(tempRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].IsDir() || filepath.Join(tempRoot, entries[0].Name()) != artifact.BundlePath {
		t.Fatalf("temporary artifacts after snapshot return = %#v", entries)
	}

	guest := filepath.Join(t.TempDir(), "guest")
	gitTest(t, "", "init", "--quiet", guest)
	gitTest(t, guest, "fetch", "--no-tags", "--no-write-fetch-head", "--", artifact.BundlePath, artifact.BundleRef)
	gitTest(t, guest, "checkout", "--quiet", "--detach", artifact.SourceRevision)
	if got := string(mustRead(t, filepath.Join(guest, "tracked.txt"))); got != "final unstaged\n" {
		t.Fatalf("tracked final content = %q", got)
	}
	if got := string(mustRead(t, filepath.Join(guest, "nested", "untracked.txt"))); got != "nested untracked\n" {
		t.Fatalf("nested untracked content = %q", got)
	}
	if got := string(mustRead(t, filepath.Join(guest, "tracked-ignored.txt"))); got != "tracked ignored final\n" {
		t.Fatalf("tracked ignored content = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(guest, "ignored.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ignored untracked file crossed snapshot: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(guest, "deleted.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted file remained in snapshot: %v", err)
	}
	if got := string(mustRead(t, filepath.Join(guest, "renamed.txt"))); got != "renamed final\n" {
		t.Fatalf("renamed content = %q", got)
	}
	if got := mustRead(t, filepath.Join(guest, "binary.dat")); !bytes.Equal(got, []byte{0, 9, 0xff, 4}) {
		t.Fatalf("binary content = %v", got)
	}
	if target, err := os.Readlink(filepath.Join(guest, "untracked-link")); err != nil || target != "nested/untracked.txt" {
		t.Fatalf("untracked symlink = %q, %v", target, err)
	}
	if parent := strings.TrimSpace(gitTest(t, guest, "rev-parse", artifact.SourceRevision+"^")); parent != sourceHead {
		t.Fatalf("snapshot parent = %s, want %s", parent, sourceHead)
	}
	if tree := strings.TrimSpace(gitTest(t, guest, "rev-parse", artifact.SourceRevision+"^{tree}")); tree != artifact.SourceTree {
		t.Fatalf("snapshot tree = %s, want %s", tree, artifact.SourceTree)
	}
	if got := strings.TrimSpace(gitTest(t, guest, "show", "-s", "--format=%an <%ae>|%cn <%ce>|%s", artifact.SourceRevision)); got != "DSX Snapshot <snapshot@dsx.invalid>|DSX Snapshot <snapshot@dsx.invalid>|DSX workspace source snapshot" {
		t.Fatalf("snapshot commit headers = %q", got)
	}

	second, err := fixture.service.PrepareSource(context.Background(), request)
	if err != nil {
		t.Fatalf("second PrepareSource(snapshot) error = %v", err)
	}
	if second.SourceRevision != artifact.SourceRevision || second.SourceTree != artifact.SourceTree || second.TrackedFingerprint != artifact.TrackedFingerprint {
		t.Fatalf("snapshot is not deterministic: first %#v second %#v", artifact, second)
	}
	assertHostByteSnapshot(t, fixture.path, before)
	if err := fixture.service.RemoveArtifact(artifact.BundlePath); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.RemoveArtifact(second.BundlePath); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(tempRoot); err != nil || len(entries) != 0 {
		t.Fatalf("temporary artifacts after removal = %#v, %v", entries, err)
	}
}

func TestPrepareSourceSnapshotRejectsUnsafeOrOversizedRepositoriesWithoutArtifacts(t *testing.T) {
	t.Run("unmerged paths", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		gitTest(t, fixture.path, "checkout", "-b", "other")
		writeFile(t, filepath.Join(fixture.path, "tracked.txt"), "other\n")
		gitTest(t, fixture.path, "commit", "-am", "other")
		gitTest(t, fixture.path, "checkout", "main")
		writeFile(t, filepath.Join(fixture.path, "tracked.txt"), "main\n")
		gitTest(t, fixture.path, "commit", "-am", "main")
		command := exec.Command("git", "merge", "other")
		command.Dir = fixture.path
		command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
		if output, err := command.CombinedOutput(); err == nil {
			t.Fatalf("git merge unexpectedly succeeded: %s", output)
		}
		tempRoot := t.TempDir()
		_, err := fixture.service.PrepareSource(context.Background(), SourceRequest{
			Repository: fixture.repository(), ApprovedRoot: fixture.path,
			Workspace: "unmerged", TempRoot: tempRoot, Snapshot: true,
		})
		if err == nil || !strings.Contains(err.Error(), "repository has unmerged paths") {
			t.Fatalf("unmerged snapshot error = %v", err)
		}
		if entries, readErr := os.ReadDir(tempRoot); readErr != nil || len(entries) != 0 {
			t.Fatalf("unmerged refusal artifacts = %#v, %v", entries, readErr)
		}
	})

	t.Run("tracked gitlink", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		head := strings.TrimSpace(gitTest(t, fixture.path, "rev-parse", "HEAD"))
		gitTest(t, fixture.path, "update-index", "--add", "--cacheinfo", "160000,"+head+",submodule")
		tempRoot := t.TempDir()
		_, err := fixture.service.PrepareSource(context.Background(), SourceRequest{
			Repository: fixture.repository(), ApprovedRoot: fixture.path,
			Workspace: "gitlink", TempRoot: tempRoot, Snapshot: true,
		})
		if err == nil || !strings.Contains(err.Error(), "submodules or embedded Git repositories") {
			t.Fatalf("tracked gitlink snapshot error = %v", err)
		}
		if entries, readErr := os.ReadDir(tempRoot); readErr != nil || len(entries) != 0 {
			t.Fatalf("tracked gitlink refusal artifacts = %#v, %v", entries, readErr)
		}
	})

	t.Run("new embedded repository", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		embedded := filepath.Join(fixture.path, "embedded")
		gitTest(t, "", "init", "--quiet", embedded)
		writeFile(t, filepath.Join(embedded, "nested.txt"), "nested\n")
		tempRoot := t.TempDir()
		_, err := fixture.service.PrepareSource(context.Background(), SourceRequest{
			Repository: fixture.repository(), ApprovedRoot: fixture.path,
			Workspace: "embedded", TempRoot: tempRoot, Snapshot: true,
		})
		if err == nil || !strings.Contains(err.Error(), "submodules or embedded Git repositories") {
			t.Fatalf("embedded repository snapshot error = %v", err)
		}
		if entries, readErr := os.ReadDir(tempRoot); readErr != nil || len(entries) != 0 {
			t.Fatalf("embedded repository refusal artifacts = %#v, %v", entries, readErr)
		}
	})

	t.Run("over cap before materialization", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		large := filepath.Join(fixture.path, "large-untracked.bin")
		if err := os.WriteFile(large, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(large, MaxSnapshotInputBytes+1); err != nil {
			t.Fatal(err)
		}
		before := hostByteSnapshot(t, fixture.path)
		tempRoot := t.TempDir()
		_, err := fixture.service.PrepareSource(context.Background(), SourceRequest{
			Repository: fixture.repository(), ApprovedRoot: fixture.path,
			Workspace: "large", TempRoot: tempRoot, Snapshot: true,
		})
		if err == nil || !strings.Contains(err.Error(), "snapshot materialized input exceeds") {
			t.Fatalf("over-cap snapshot error = %v", err)
		}
		assertHostByteSnapshot(t, fixture.path, before)
		if entries, readErr := os.ReadDir(tempRoot); readErr != nil || len(entries) != 0 {
			t.Fatalf("over-cap refusal artifacts = %#v, %v", entries, readErr)
		}
	})
}

func TestPrepareUpdateSourceRequiresCleanMatchingAdvancedBranch(t *testing.T) {
	tests := map[string]struct {
		mutate       func(*testing.T, repositoryFixture)
		snapshot     bool
		wantSnapshot bool
		wantErr      string
	}{
		"successful ordinary update": {
			mutate: func(t *testing.T, fixture repositoryFixture) {
				writeFile(t, filepath.Join(fixture.path, "tracked.txt"), "new source\n")
				gitTest(t, fixture.path, "add", "tracked.txt")
				gitTest(t, fixture.path, "commit", "-m", "new source")
			},
		},
		"wrong branch": {
			mutate: func(t *testing.T, fixture repositoryFixture) {
				gitTest(t, fixture.path, "checkout", "-b", "other")
			},
			wantErr: "does not match recorded source branch",
		},
		"dirty host ordinary": {
			mutate: func(t *testing.T, fixture repositoryFixture) {
				writeFile(t, filepath.Join(fixture.path, "tracked.txt"), "dirty\n")
			},
			wantErr: "tracked or index changes",
		},
		"no newer ordinary commit": {
			mutate:  func(*testing.T, repositoryFixture) {},
			wantErr: "no newer committed revision",
		},
		"rewritten ordinary source history": {
			mutate: func(t *testing.T, fixture repositoryFixture) {
				tree := strings.TrimSpace(gitTest(t, fixture.path, "write-tree"))
				revision := strings.TrimSpace(gitTest(t, fixture.path, "commit-tree", tree, "-m", "unrelated source"))
				gitTest(t, fixture.path, "update-ref", "refs/heads/main", revision)
			},
			wantErr: "does not descend from recorded source head revision",
		},
		"same parent changed snapshot": {
			mutate: func(t *testing.T, fixture repositoryFixture) {
				writeFile(t, filepath.Join(fixture.path, "tracked.txt"), "snapshot source\n")
				writeFile(t, filepath.Join(fixture.path, "untracked.txt"), "snapshot untracked\n")
			},
			snapshot: true, wantSnapshot: true,
		},
		"advanced parent snapshot": {
			mutate: func(t *testing.T, fixture repositoryFixture) {
				gitTest(t, fixture.path, "commit", "--allow-empty", "-m", "advanced parent")
				writeFile(t, filepath.Join(fixture.path, "tracked.txt"), "advanced snapshot\n")
			},
			snapshot: true, wantSnapshot: true,
		},
		"unchanged snapshot": {
			mutate:   func(*testing.T, repositoryFixture) {},
			snapshot: true,
			wantErr:  "local source snapshot has not changed",
		},
		"rewritten snapshot parent history": {
			mutate: func(t *testing.T, fixture repositoryFixture) {
				tree := strings.TrimSpace(gitTest(t, fixture.path, "write-tree"))
				revision := strings.TrimSpace(gitTest(t, fixture.path, "commit-tree", tree, "-m", "unrelated source"))
				gitTest(t, fixture.path, "update-ref", "refs/heads/main", revision)
				writeFile(t, filepath.Join(fixture.path, "tracked.txt"), "rewritten snapshot\n")
			},
			snapshot: true,
			wantErr:  "does not descend from recorded source head revision",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newRepositoryWithCommit(t)
			source := prepareSourceTest(t, &fixture, "alpha")
			t.Cleanup(func() { _ = fixture.service.RemoveArtifact(source.BundlePath) })
			test.mutate(t, fixture)

			artifact, err := fixture.service.PrepareUpdateSource(context.Background(), UpdateSourceRequest{
				Repository: fixture.repository(), Workspace: "alpha", TempRoot: t.TempDir(),
				SourceBranch: source.SourceBranch, SourceRevision: source.SourceRevision,
				SourceHeadRevision: source.SourceHeadRevision, SourceTree: source.SourceTree, Snapshot: test.snapshot,
			})
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("PrepareUpdateSource() error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("PrepareUpdateSource() error = %v", err)
			}
			t.Cleanup(func() { _ = fixture.service.RemoveArtifact(artifact.BundlePath) })
			if artifact.SourceBranch != "main" || artifact.SourceRevision == source.SourceRevision || artifact.SourceSnapshot != test.wantSnapshot {
				t.Fatalf("updated artifact = %#v", artifact)
			}
			if artifact.SourceHeadRevision == "" || artifact.SourceTree == "" {
				t.Fatalf("updated artifact lacks provenance = %#v", artifact)
			}
			if info, statErr := os.Lstat(artifact.BundlePath); statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != SourceBundleMode {
				t.Fatalf("updated bundle metadata = %v, error = %v", info, statErr)
			}
			if err := fixture.service.VerifyBundle(context.Background(), artifact.BundlePath, artifact.BundleDigest); err != nil {
				t.Fatalf("VerifyBundle(updated) error = %v", err)
			}
		})
	}
}

func TestPrepareSourceRejectsHostileNamesPathsAndArtifactReplacement(t *testing.T) {
	t.Parallel()
	fixture := newRepositoryWithCommit(t)
	tempRoot := t.TempDir()
	tempLink := filepath.Join(t.TempDir(), "temp-link")
	if err := os.Symlink(tempRoot, tempLink); err != nil {
		t.Fatal(err)
	}
	badRequests := []SourceRequest{
		{Repository: Repository{Name: "../bad", HostPath: fixture.path, GuestPath: "/workspace"}, ApprovedRoot: fixture.path, Workspace: "alpha", TempRoot: tempRoot},
		{Repository: Repository{Name: "workspace", HostPath: "relative", GuestPath: "/workspace"}, ApprovedRoot: fixture.path, Workspace: "alpha", TempRoot: tempRoot},
		{Repository: Repository{Name: "workspace", HostPath: fixture.path, GuestPath: "/../escape"}, ApprovedRoot: fixture.path, Workspace: "alpha", TempRoot: tempRoot},
		{Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "../escape", TempRoot: tempRoot},
		{Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "alpha", TempRoot: tempLink},
	}
	for _, request := range badRequests {
		if _, err := fixture.service.PrepareSource(context.Background(), request); err == nil {
			t.Fatalf("PrepareSource accepted hostile request %#v", request)
		}
	}
	artifact, err := fixture.service.PrepareSource(context.Background(), SourceRequest{Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "alpha", TempRoot: tempRoot})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(artifact.BundlePath); err != nil {
		t.Fatal(err)
	}
	writeFileMode(t, artifact.BundlePath, "replacement", 0o600)
	if err := fixture.service.RemoveArtifact(artifact.BundlePath); err == nil {
		t.Fatal("RemoveArtifact accepted replacement file")
	}
	if got := string(mustRead(t, artifact.BundlePath)); got != "replacement" {
		t.Fatalf("replacement was modified: %q", got)
	}
	if err := fixture.service.RemoveArtifact(filepath.Join(tempRoot, "other.bundle")); err == nil {
		t.Fatal("RemoveArtifact accepted unowned path")
	}
}

func TestVerifyBundleRejectsModeSymlinkDigestAndCorruption(t *testing.T) {
	t.Parallel()
	fixture := newRepositoryWithCommit(t)
	artifact := prepareSourceTest(t, &fixture, "alpha")
	if err := os.Chmod(artifact.BundlePath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.VerifyBundle(context.Background(), artifact.BundlePath, artifact.BundleDigest); err == nil {
		t.Fatal("VerifyBundle accepted mode 0644")
	}
	if err := os.Chmod(artifact.BundlePath, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.bundle")
	if err := os.Symlink(artifact.BundlePath, link); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.VerifyBundle(context.Background(), link, artifact.BundleDigest); err == nil {
		t.Fatal("VerifyBundle accepted symlink")
	}
	if err := fixture.service.VerifyBundle(context.Background(), artifact.BundlePath, strings.Repeat("0", 64)); err == nil {
		t.Fatal("VerifyBundle accepted wrong digest")
	}
	corrupt := filepath.Join(t.TempDir(), "corrupt.bundle")
	writeFileMode(t, corrupt, "not a git bundle", 0o600)
	digest, err := bundleSHA256(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.VerifyBundle(context.Background(), corrupt, digest); err == nil {
		t.Fatal("VerifyBundle accepted corrupt bundle")
	}
}

func TestResultFetchDiffAndSuccessfulSquashApplySmoke(t *testing.T) {
	fixture := newTransferFixture(t, OSRunner{})
	defer fixture.cleanup()

	if fixture.fetch.HostRef != "refs/remotes/dsx/alpha" || fixture.fetch.Commit != fixture.resultCommit {
		t.Fatalf("fetch result = %#v, result commit = %s", fixture.fetch, fixture.resultCommit)
	}
	observed := strings.TrimSpace(gitTest(t, fixture.host.path, "rev-parse", "refs/remotes/dsx/alpha^{commit}"))
	if observed != fixture.resultCommit {
		t.Fatalf("fetched ref = %s, want %s", observed, fixture.resultCommit)
	}
	diff, err := fixture.host.service.Diff(context.Background(), DiffRequest{
		Repository: fixture.host.repository(), BaseCommit: fixture.source.SourceRevision, HeadCommit: fixture.resultCommit, MaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"new.txt", "deleted.txt", "renamed.txt", "GIT binary patch"} {
		if !bytes.Contains(diff.Patch, []byte(token)) {
			t.Errorf("diff does not contain %q", token)
		}
	}
	capped, err := fixture.host.service.Diff(context.Background(), DiffRequest{
		Repository: fixture.host.repository(), BaseCommit: fixture.source.SourceRevision, HeadCommit: fixture.resultCommit, MaxBytes: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(capped.Patch) != 32 || !capped.Truncated {
		t.Fatalf("capped diff length=%d truncated=%v", len(capped.Patch), capped.Truncated)
	}
	status, err := fixture.host.service.Status(context.Background(), StatusRequest{Repository: fixture.host.repository(), Workspace: "alpha", SourceBranch: fixture.source.SourceBranch, SourceRevision: fixture.source.SourceRevision, SourceSnapshot: true, WorkspaceBranch: "dsx/alpha", ResultCommit: fixture.resultCommit, TrackedFingerprint: fixture.source.TrackedFingerprint, FetchedCommit: fixture.resultCommit})
	if err != nil {
		t.Fatal(err)
	}
	if !status.SourceSnapshot || !status.HostTrackedClean || status.SourceBranch != fixture.source.SourceBranch || status.WorkspaceBranch != "dsx/alpha" || status.HostCommit != fixture.source.SourceRevision || !status.Fetched || status.FetchedCommit != fixture.resultCommit {
		t.Fatalf("status = %#v", status)
	}
	transaction, err := fixture.host.service.PrepareApply(context.Background(), ApplyRequest{Repository: fixture.host.repository(), SourceRevision: fixture.source.SourceRevision, TrackedFingerprint: fixture.source.TrackedFingerprint, FetchedRef: fixture.fetch.HostRef, ExpectedCommit: fixture.resultCommit})
	if err != nil {
		t.Fatalf("PrepareApply() error = %v", err)
	}
	apply, err := transaction.Commit(context.Background())
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	wantPaths := []string{"binary.dat", "deleted.txt", "new.txt", "old.txt", "renamed.txt"}
	if !reflect.DeepEqual(apply.Paths, wantPaths) {
		t.Fatalf("applied paths = %#v, want %#v", apply.Paths, wantPaths)
	}
	if head := strings.TrimSpace(gitTest(t, fixture.host.path, "rev-parse", "HEAD")); head != fixture.source.SourceRevision {
		t.Fatalf("Apply committed host HEAD %s", head)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.host.path, "diff", "--cached", "--name-only")); got == "" {
		t.Fatal("successful squash apply did not stage changes")
	}
	if !bytes.Equal(mustRead(t, filepath.Join(fixture.host.path, "binary.dat")), []byte{0, 1, 2, 0xff, 0, 4}) {
		t.Fatal("binary result was not applied exactly")
	}
}

func TestSnapshotResultFetchDiffAndSuccessfulSquashApplySmoke(t *testing.T) {
	host := newRepository(t)
	writeFile(t, filepath.Join(host.path, "deleted.txt"), "delete me\n")
	writeFile(t, filepath.Join(host.path, "old.txt"), "rename me\n")
	writeFile(t, filepath.Join(host.path, "binary.dat"), []byte{0, 1, 0, 2})
	writeFile(t, filepath.Join(host.path, "tracked.txt"), "base\n")
	gitTest(t, host.path, "add", "-A")
	gitTest(t, host.path, "commit", "-m", "real source head")
	realSourceHead := strings.TrimSpace(gitTest(t, host.path, "rev-parse", "HEAD"))

	writeFile(t, filepath.Join(host.path, "tracked.txt"), "captured baseline\n")
	writeFile(t, filepath.Join(host.path, "baseline.txt"), "captured untracked baseline\n")
	sourceTempRoot := t.TempDir()
	source, err := host.service.PrepareSource(context.Background(), SourceRequest{
		Repository: host.repository(), ApprovedRoot: host.path,
		Workspace: "alpha", TempRoot: sourceTempRoot, Snapshot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.service.RemoveArtifact(source.BundlePath) })
	host.identity = source.Repository.Identity
	if source.SourceHeadRevision != realSourceHead || !source.SourceSnapshot {
		t.Fatalf("snapshot source = %#v", source)
	}

	guest := filepath.Join(t.TempDir(), "guest")
	gitTest(t, "", "init", "--quiet", guest)
	gitTest(t, guest, "fetch", "--no-tags", "--no-write-fetch-head", "--", source.BundlePath, source.BundleRef)
	gitTest(t, guest, "config", "user.name", "DSX Result")
	gitTest(t, guest, "config", "user.email", "dsx@example.invalid")
	gitTest(t, guest, "checkout", "--quiet", "-b", "dsx/alpha", source.SourceRevision)
	if err := os.Remove(filepath.Join(guest, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(guest, "old.txt"), filepath.Join(guest, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(guest, "new.txt"), "new file\n")
	writeFile(t, filepath.Join(guest, "binary.dat"), []byte{0, 1, 2, 0xff, 0, 4})
	gitTest(t, guest, "add", "-A")
	gitTest(t, guest, "commit", "-m", "snapshot agent result")
	resultCommit := strings.TrimSpace(gitTest(t, guest, "rev-parse", "HEAD"))
	resultBundle := filepath.Join(t.TempDir(), "result.bundle")
	gitTest(t, guest, "bundle", "create", resultBundle, "refs/heads/dsx/alpha")
	if err := os.Chmod(resultBundle, ResultBundleMode); err != nil {
		t.Fatal(err)
	}
	resultDigest, err := bundleSHA256(resultBundle)
	if err != nil {
		t.Fatal(err)
	}

	gitTest(t, host.path, "add", "-A")
	gitTest(t, host.path, "commit", "-m", "commit exact captured baseline")
	equivalentHostHead := strings.TrimSpace(gitTest(t, host.path, "rev-parse", "HEAD"))
	if equivalentHostHead == source.SourceHeadRevision {
		t.Fatal("tree-equivalent host HEAD did not advance")
	}
	if tree := strings.TrimSpace(gitTest(t, host.path, "rev-parse", "HEAD^{tree}")); tree != source.SourceTree {
		t.Fatalf("tree-equivalent host tree = %s, want %s", tree, source.SourceTree)
	}
	fetched, err := host.service.FetchResult(context.Background(), FetchRequest{
		Repository: host.repository(), Workspace: "alpha", BundlePath: resultBundle,
		Digest: resultDigest, ExpectedCommit: resultCommit,
	})
	if err != nil {
		t.Fatal(err)
	}
	applyTempRoot := t.TempDir()
	transaction, err := host.service.PrepareApply(context.Background(), ApplyRequest{
		Repository: host.repository(), SourceRevision: source.SourceRevision, SourceSnapshot: true,
		SourceHeadRevision: source.SourceHeadRevision, SourceTree: source.SourceTree,
		TrackedFingerprint: source.TrackedFingerprint, FetchedRef: fetched.HostRef,
		ExpectedCommit: resultCommit, TempRoot: applyTempRoot,
	})
	if err != nil {
		t.Fatalf("PrepareApply(snapshot) error = %v", err)
	}
	applied, err := transaction.Commit(context.Background())
	if err != nil {
		t.Fatalf("Commit(snapshot) error = %v", err)
	}
	wantPaths := []string{"binary.dat", "deleted.txt", "new.txt", "old.txt", "renamed.txt"}
	if applied.AppliedCommit != resultCommit || !reflect.DeepEqual(applied.Paths, wantPaths) {
		t.Fatalf("snapshot apply = %#v, want paths %#v", applied, wantPaths)
	}
	if head := strings.TrimSpace(gitTest(t, host.path, "rev-parse", "HEAD")); head != equivalentHostHead {
		t.Fatalf("snapshot apply changed host HEAD to %s, want %s", head, equivalentHostHead)
	}
	wantStaged := []string{"binary.dat", "deleted.txt", "new.txt", "renamed.txt"}
	if staged := strings.Fields(gitTest(t, host.path, "diff", "--cached", "--name-only")); !reflect.DeepEqual(staged, wantStaged) {
		t.Fatalf("snapshot apply staged paths = %#v, want %#v", staged, wantStaged)
	}
	if message := string(mustRead(t, filepath.Join(host.path, ".git", "SQUASH_MSG"))); message != "Apply DSX workspace result\n" {
		t.Fatalf("snapshot squash message = %q", message)
	}
	if got := string(mustRead(t, filepath.Join(host.path, "baseline.txt"))); got != "captured untracked baseline\n" {
		t.Fatalf("captured baseline changed = %q", got)
	}
	if entries, err := os.ReadDir(applyTempRoot); err != nil || len(entries) != 0 {
		t.Fatalf("snapshot apply quarantine leaked: %#v, %v", entries, err)
	}
}

func TestFreshResultBundleDiffIsBoundedHostImmutableAndDisposable(t *testing.T) {
	runner := &diffRepositoryRunner{delegate: OSRunner{}}
	fixture := newUnfetchedTransferFixture(t, runner)
	defer fixture.cleanup()
	status, err := fixture.host.service.Status(context.Background(), StatusRequest{Repository: fixture.host.repository(), Workspace: "alpha", SourceBranch: fixture.source.SourceBranch, SourceRevision: fixture.source.SourceRevision, WorkspaceBranch: "dsx/alpha", ResultCommit: fixture.resultCommit, TrackedFingerprint: fixture.source.TrackedFingerprint})
	if err != nil {
		t.Fatal(err)
	}
	if status.Fetched || status.FetchedCommit != "" {
		t.Fatalf("fresh result status = %#v", status)
	}
	before := hostByteSnapshot(t, fixture.host.path)
	request := DiffRequest{
		Repository: fixture.host.repository(), BaseCommit: fixture.source.SourceRevision, HeadCommit: fixture.resultCommit,
		Bundle: &DiffBundle{Path: fixture.resultBundle, Digest: fixture.resultDigest, Ref: "refs/heads/dsx/alpha"},
	}
	diff, err := fixture.host.service.Diff(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"new.txt", "deleted.txt", "renamed.txt", "GIT binary patch"} {
		if !bytes.Contains(diff.Patch, []byte(token)) {
			t.Errorf("fresh result diff does not contain %q", token)
		}
	}
	request.MaxBytes = 32
	capped, err := fixture.host.service.Diff(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped.Patch) != 32 || !capped.Truncated {
		t.Fatalf("capped fresh diff length=%d truncated=%v", len(capped.Patch), capped.Truncated)
	}
	assertHostByteSnapshot(t, fixture.host.path, before)
	if _, found, err := fixture.host.service.optionalRefCommit(context.Background(), fixture.host.path, RefNamespace+"alpha"); err != nil || found {
		t.Fatalf("fresh diff created host result ref: found=%v err=%v", found, err)
	}
	runner.assertPrivateRepositoriesRemoved(t)

	malformed := filepath.Join(t.TempDir(), "malformed.bundle")
	writeFileMode(t, malformed, "not a git bundle", ResultBundleMode)
	malformedDigest, err := bundleSHA256(malformed)
	if err != nil {
		t.Fatal(err)
	}
	request.Bundle = &DiffBundle{Path: malformed, Digest: malformedDigest, Ref: "refs/heads/dsx/alpha"}
	if _, err := fixture.host.service.Diff(context.Background(), request); err == nil {
		t.Fatal("Diff accepted malformed result bundle")
	}
	assertHostByteSnapshot(t, fixture.host.path, before)
	runner.assertPrivateRepositoriesRemoved(t)

	fetched, err := fixture.host.service.FetchResult(context.Background(), FetchRequest{Repository: fixture.host.repository(), Workspace: "alpha", BundlePath: fixture.resultBundle, Digest: fixture.resultDigest, ExpectedCommit: fixture.resultCommit})
	if err != nil {
		t.Fatal(err)
	}
	if fetched.HostRef != RefNamespace+"alpha" || fetched.Commit != fixture.resultCommit {
		t.Fatalf("later fetch = %#v", fetched)
	}
	observed := strings.TrimSpace(gitTest(t, fixture.host.path, "rev-parse", fetched.HostRef+"^{commit}"))
	if observed != fixture.resultCommit {
		t.Fatalf("later fetched ref = %s, want %s", observed, fixture.resultCommit)
	}
}

func TestFetchRejectsHostileWorkspaceAndUnexpectedBundleRef(t *testing.T) {
	t.Parallel()
	fixture := newRepositoryWithCommit(t)
	artifact := prepareSourceTest(t, &fixture, "alpha")
	if _, err := fixture.service.FetchResult(context.Background(), FetchRequest{Repository: fixture.repository(), Workspace: "../evil", BundlePath: artifact.BundlePath, Digest: artifact.BundleDigest, ExpectedCommit: artifact.SourceRevision}); err == nil {
		t.Fatal("FetchResult accepted hostile workspace")
	}
	if _, err := fixture.service.FetchResult(context.Background(), FetchRequest{Repository: fixture.repository(), Workspace: "alpha", BundlePath: artifact.BundlePath, Digest: artifact.BundleDigest, ExpectedCommit: artifact.SourceRevision}); err == nil || !strings.Contains(err.Error(), "does not match required") {
		t.Fatalf("FetchResult unexpected-ref error = %v", err)
	}
}

func TestFetchMaintainsIndependentWorkspaceRefs(t *testing.T) {
	fixture := newRepositoryWithCommit(t)
	source := prepareSourceTest(t, &fixture, "alpha")
	defer fixture.service.RemoveArtifact(source.BundlePath)
	guest := filepath.Join(t.TempDir(), "guest")
	gitTest(t, "", "init", "--quiet", guest)
	gitTest(t, guest, "fetch", "--no-tags", "--no-write-fetch-head", "--", source.BundlePath, source.BundleRef)
	gitTest(t, guest, "config", "user.name", "DSX Result")
	gitTest(t, guest, "config", "user.email", "dsx@example.invalid")

	commits := make(map[string]string, 2)
	for _, workspace := range []string{"alpha", "beta"} {
		gitTest(t, guest, "checkout", "--quiet", "--detach", source.SourceRevision)
		gitTest(t, guest, "switch", "--quiet", "-C", "dsx/"+workspace)
		writeFile(t, filepath.Join(guest, workspace+".txt"), workspace+"\n")
		gitTest(t, guest, "add", workspace+".txt")
		gitTest(t, guest, "commit", "-m", workspace)
		commit := strings.TrimSpace(gitTest(t, guest, "rev-parse", "HEAD"))
		bundle := filepath.Join(t.TempDir(), workspace+".bundle")
		gitTest(t, guest, "bundle", "create", bundle, "refs/heads/dsx/"+workspace)
		if err := os.Chmod(bundle, ResultBundleMode); err != nil {
			t.Fatal(err)
		}
		digest, err := bundleSHA256(bundle)
		if err != nil {
			t.Fatal(err)
		}
		fetched, err := fixture.service.FetchResult(context.Background(), FetchRequest{
			Repository: fixture.repository(), Workspace: workspace, BundlePath: bundle,
			Digest: digest, ExpectedCommit: commit,
		})
		if err != nil {
			t.Fatalf("FetchResult(%s) error = %v", workspace, err)
		}
		if fetched.HostRef != RefNamespace+workspace {
			t.Fatalf("FetchResult(%s) ref = %q", workspace, fetched.HostRef)
		}
		commits[workspace] = commit
	}
	for workspace, commit := range commits {
		if got := strings.TrimSpace(gitTest(t, fixture.path, "rev-parse", RefNamespace+workspace)); got != commit {
			t.Fatalf("%s ref = %s, want %s", workspace, got, commit)
		}
	}
}

func TestFetchExpectedCommitMismatchPreservesExistingHostRef(t *testing.T) {
	fixture := newTransferFixture(t, OSRunner{})
	defer fixture.cleanup()
	before := strings.TrimSpace(gitTest(t, fixture.host.path, "rev-parse", fixture.fetch.HostRef+"^{commit}"))
	digest, err := bundleSHA256(fixture.resultBundle)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.host.service.FetchResult(context.Background(), FetchRequest{Repository: fixture.host.repository(), Workspace: "alpha", BundlePath: fixture.resultBundle, Digest: digest, ExpectedCommit: strings.Repeat("0", len(fixture.resultCommit))})
	if err == nil || !strings.Contains(err.Error(), "does not match expected commit") {
		t.Fatalf("FetchResult mismatch error = %v", err)
	}
	after := strings.TrimSpace(gitTest(t, fixture.host.path, "rev-parse", fixture.fetch.HostRef+"^{commit}"))
	if after != before {
		t.Fatalf("host ref changed from %s to %s", before, after)
	}
}

func TestHostGitUsesProtectedConfigurationAndRejectsUnallowlistedLocalConfig(t *testing.T) {
	t.Run("ambient global config and PATH", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		realGit := testGitExecutable(t)
		marker := filepath.Join(t.TempDir(), "executed")
		script := filepath.Join(t.TempDir(), "hostile")
		writeFile(t, script, fmt.Sprintf("#!/bin/sh\n/usr/bin/touch %q\n", marker))
		if err := os.Chmod(script, 0o700); err != nil {
			t.Fatal(err)
		}
		global := filepath.Join(t.TempDir(), "gitconfig")
		writeFile(t, global, "[core]\n\tfsmonitor = "+script+"\n")
		fakeBin := t.TempDir()
		writeFile(t, filepath.Join(fakeBin, "git"), fmt.Sprintf("#!/bin/sh\n/usr/bin/touch %q\nexit 99\n", marker))
		if err := os.Chmod(filepath.Join(fakeBin, "git"), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", t.TempDir())
		t.Setenv("PATH", fakeBin)
		t.Setenv("GIT_CONFIG_GLOBAL", global)
		service, err := NewService(OSRunner{}, realGit)
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{"core.protectHFS=true", "core.protectNTFS=true"} {
			if !containsArg(service.gitArgv("status"), required) {
				t.Fatalf("host Git argv does not force %s", required)
			}
		}
		artifact, err := service.PrepareSource(context.Background(), SourceRequest{Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "secure", TempRoot: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		defer service.RemoveArtifact(artifact.BundlePath)
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ambient executable ran: %v", err)
		}
		for _, entry := range service.environment {
			name, _, _ := strings.Cut(entry, "=")
			if name == "HOME" || name == "PATH" || (strings.HasPrefix(name, "GIT_") && name != "GIT_ALLOW_PROTOCOL" && name != "GIT_CONFIG_GLOBAL" && name != "GIT_CONFIG_NOSYSTEM" && name != "GIT_CONFIG_SYSTEM" && name != "GIT_OPTIONAL_LOCKS" && name != "GIT_PROTOCOL_FROM_USER" && name != "GIT_TERMINAL_PROMPT") {
				t.Fatalf("ambient environment leaked into Git: %q", entry)
			}
		}
	})
	t.Run("ordinary clone configuration", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		gitTest(t, fixture.path, "config", "--local", "remote.origin.url", "https://example.invalid/owner/repository.git")
		gitTest(t, fixture.path, "config", "--local", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
		gitTest(t, fixture.path, "config", "--local", "branch.main.remote", "origin")
		gitTest(t, fixture.path, "config", "--local", "branch.main.merge", "refs/heads/main")
		gitTest(t, fixture.path, "config", "--local", "branch.main.vscode-merge-base", "origin/main")
		gitTest(t, fixture.path, "config", "--local", "branch.main.github-pr-owner-number", "StuDocu#repository#11")
		gitTest(t, fixture.path, "config", "--local", "branch.feat/core-19.vscode-merge-base", "origin/main")
		gitTest(t, fixture.path, "config", "--local", "branch.feat/core-19.github-pr-owner-number", "StuDocu#repository#11")
		gitTest(t, fixture.path, "config", "--local", "branch.feat/core-19.gh-merge-base", "main")
		gitTest(t, fixture.path, "config", "--local", "branch.release.1.0.vscode-merge-base", "origin/main")
		gitTest(t, fixture.path, "config", "--local", "remote.origin.gh-resolved", "base")
		artifact, err := fixture.service.PrepareSource(context.Background(), SourceRequest{Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "safe", TempRoot: t.TempDir()})
		if err != nil {
			t.Fatalf("PrepareSource with ordinary clone configuration: %v", err)
		}
		defer fixture.service.RemoveArtifact(artifact.BundlePath)
		if err := fixture.service.VerifyBundle(context.Background(), artifact.BundlePath, artifact.BundleDigest); err != nil {
			t.Fatalf("VerifyBundle() error = %v", err)
		}
	})
	for name, test := range map[string]struct {
		key   string
		value string
	}{
		"unreviewed branch leaf":             {key: "branch.main.unreviewed", value: "value"},
		"branch leaf resembling vscode":      {key: "branch.main.vscode-command", value: "/tmp/run-me"},
		"branch leaf resembling github":      {key: "branch.main.github-pr-command", value: "/tmp/run-me"},
		"unreviewed remote leaf":             {key: "remote.origin.gh-unreviewed", value: "value"},
		"unsafe empty vscode merge base":     {key: "branch.main.vscode-merge-base", value: ""},
		"unsafe empty github owner number":   {key: "branch.feat/core-19.github-pr-owner-number", value: ""},
		"unsafe empty gh merge base":         {key: "branch.feat/core-19.gh-merge-base", value: ""},
		"unsafe empty gh resolved":           {key: "remote.origin.gh-resolved", value: ""},
		"worktree configuration extension":   {key: "extensions.worktreeConfig", value: "true"},
		"valueless unreviewed implicit true": {key: "dsx.unreviewed", value: ""},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newRepositoryWithCommit(t)
			gitTest(t, fixture.path, "config", "--local", test.key, test.value)
			_, err := fixture.service.PrepareSource(context.Background(), SourceRequest{Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "secure", TempRoot: t.TempDir()})
			want := wantUnallowlistedConfigError(normalizeGitConfigKey(test.key))
			if err == nil || err.Error() != want {
				t.Fatalf("PrepareSource with %s error = %v, want %q", test.key, err, want)
			}
		})
	}
	t.Run("valueless allowlisted implicit boolean", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		appendFile(t, filepath.Join(fixture.path, ".git", "config"), "[core]\n\tprecomposeunicode\n")
		if records := gitTest(t, fixture.path, "config", "--local", "--list"); !strings.Contains(records, "\ncore.precomposeunicode\n") {
			t.Fatalf("configuration does not contain a valueless record: %q", records)
		}
		artifact, err := fixture.service.PrepareSource(context.Background(), SourceRequest{Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "safe", TempRoot: t.TempDir()})
		if err != nil {
			t.Fatalf("PrepareSource with valueless implicit boolean: %v", err)
		}
		defer fixture.service.RemoveArtifact(artifact.BundlePath)
	})
	t.Run("mixed case subsection remediation", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		gitTest(t, fixture.path, "config", "--local", "branch.Feat/Core-19.unreviewed", "value")
		_, err := fixture.service.PrepareSource(context.Background(), SourceRequest{Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "secure", TempRoot: t.TempDir()})
		want := wantUnallowlistedConfigError("branch.Feat/Core-19.unreviewed")
		if err == nil || err.Error() != want {
			t.Fatalf("PrepareSource with mixed-case subsection error = %v, want %q", err, want)
		}
		gitTest(t, fixture.path, "config", "--local", "--unset-all", "branch.Feat/Core-19.unreviewed")
		artifact, err := fixture.service.PrepareSource(context.Background(), SourceRequest{Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "safe", TempRoot: t.TempDir()})
		if err != nil {
			t.Fatalf("PrepareSource after documented remediation: %v", err)
		}
		defer fixture.service.RemoveArtifact(artifact.BundlePath)
	})
	for _, key := range []string{
		"core.fsmonitor", "core.alternateRefsCommand", "credential.helper",
		"filter.payload.process", "diff.payload.command", "merge.payload.driver",
		"gc.recentObjectsHook", "include.path", "dsx.unreviewed",
	} {
		t.Run(key, func(t *testing.T) {
			fixture := newRepositoryWithCommit(t)
			marker := filepath.Join(t.TempDir(), "executed")
			script := filepath.Join(t.TempDir(), "hostile")
			writeFile(t, script, fmt.Sprintf("#!/bin/sh\n/usr/bin/touch %q\n", marker))
			if err := os.Chmod(script, 0o700); err != nil {
				t.Fatal(err)
			}
			value := script
			if key == "credential.helper" {
				value = "!" + script
			}
			gitTest(t, fixture.path, "config", "--local", key, value)
			_, err := fixture.service.PrepareSource(context.Background(), SourceRequest{Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "secure", TempRoot: t.TempDir()})
			want := wantUnallowlistedConfigError(strings.ToLower(key))
			if err == nil || err.Error() != want {
				t.Fatalf("PrepareSource with %s error = %v, want %q", key, err, want)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s executable ran: %v", key, err)
			}
		})
	}
	for name, config := range map[string]string{
		"empty include section":                   "[include]\n",
		"empty conditional include section":       "[includeIf \"gitdir:/does/not/match/\"]\n",
		"comment-only include section":            "[include]\n\t# path = /must-not-read\n\t; still no variable\n",
		"include followed by an ordinary section": "[include]\n[core]\n\tprecomposeunicode = true\n",
		"adjacent include and ordinary headers":   "[include][core]\n\tprecomposeunicode = true\n",
	} {
		t.Run(name+" does not fabricate include path", func(t *testing.T) {
			fixture := newRepositoryWithCommit(t)
			appendFile(t, filepath.Join(fixture.path, ".git", "config"), config)
			artifact, err := fixture.service.PrepareSource(context.Background(), SourceRequest{
				Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "safe", TempRoot: t.TempDir(),
			})
			if err != nil {
				t.Fatalf("PrepareSource with %s: %v", name, err)
			}
			defer fixture.service.RemoveArtifact(artifact.BundlePath)
			if err := fixture.service.VerifyBundle(context.Background(), artifact.BundlePath, artifact.BundleDigest); err != nil {
				t.Fatalf("VerifyBundle() with %s: %v", name, err)
			}
		})
	}
	for name, test := range map[string]struct {
		config  string
		wantKey string
	}{
		"include section": {
			config:  "[include]\n\tunrelated = inert\n",
			wantKey: "include.unrelated",
		},
		"conditional include section": {
			config:  "[includeIf \"gitdir:/does/not/match/\"]\n\tunrelated = inert\n",
			wantKey: "includeif.gitdir:/does/not/match/.unrelated",
		},
	} {
		t.Run(name+" reports an unrelated variable as itself", func(t *testing.T) {
			fixture := newRepositoryWithCommit(t)
			appendFile(t, filepath.Join(fixture.path, ".git", "config"), test.config)
			_, err := fixture.service.PrepareSource(context.Background(), SourceRequest{
				Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "secure", TempRoot: t.TempDir(),
			})
			want := wantUnallowlistedConfigError(test.wantKey)
			if err == nil || err.Error() != want {
				t.Fatalf("PrepareSource with unrelated %s variable error = %v, want %q", name, err, want)
			}
		})
	}
	for name, test := range map[string]struct {
		config     string
		wantKey    string
		wantRecord string
	}{
		"include subsection path": {
			config:     "[include \"metadata\"] path = inert\n",
			wantKey:    "include.metadata.path",
			wantRecord: "include.metadata.path=inert",
		},
		"conditional include without condition path": {
			config:     "[includeIf] path = inert\n",
			wantKey:    "includeif.path",
			wantRecord: "includeif.path=inert",
		},
		"explicit empty include subsection path": {
			config:     "[include \"\"] path = inert\n",
			wantKey:    "include..path",
			wantRecord: "include..path=inert",
		},
		"explicit empty extensions subsection worktreeConfig": {
			config:     "[extensions \"\"] worktreeConfig = true\n",
			wantKey:    "extensions..worktreeconfig",
			wantRecord: "extensions..worktreeconfig=true",
		},
	} {
		t.Run(name+" is handled by normal aggregation", func(t *testing.T) {
			fixture := newRepositoryWithCommit(t)
			appendFile(t, filepath.Join(fixture.path, ".git", "config"),
				test.config+"[dsx]\n\tunreviewed = inert\n")
			if records := gitTest(t, fixture.path, "config", "--local", "--list"); !strings.Contains("\n"+records, "\n"+test.wantRecord+"\n") {
				t.Fatalf("Git configuration records = %q, want exact record %q", records, test.wantRecord)
			}
			_, err := fixture.service.PrepareSource(context.Background(), SourceRequest{
				Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "secure", TempRoot: t.TempDir(),
			})
			want := wantUnallowlistedConfigError("dsx.unreviewed", test.wantKey)
			if err == nil || err.Error() != want {
				t.Fatalf("PrepareSource with %s error = %v, want aggregated error %q", name, err, want)
			}
		})
	}
	t.Run("BOM-prefixed config rejects include before Git", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		configPath := filepath.Join(fixture.path, ".git", "config")
		included := filepath.Join(t.TempDir(), "malformed-included-config")
		writeFile(t, included, "not a Git configuration line\n")
		config := append([]byte{0xef, 0xbb, 0xbf}, mustRead(t, configPath)...)
		config = append(config, []byte(fmt.Sprintf("[include]\n\tpath = %s\n", included))...)
		writeFile(t, configPath, config)
		err := prepareSourceBeforeGit(t, fixture)
		want := wantUnallowlistedConfigError("include.path")
		if err.Error() != want {
			t.Fatalf("PrepareSource with BOM-prefixed include error = %v, want %q", err, want)
		}
		if strings.Contains(err.Error(), included) {
			t.Fatalf("include target leaked into error: %v", err)
		}
	})
	t.Run("gitfile without commondir rejects include before Git", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		gitDir := moveGitDirectoryAside(t, fixture.path)
		writeFile(t, filepath.Join(fixture.path, ".git"), "gitdir: "+gitDir+"\n")
		gitFileInfo, err := os.Lstat(filepath.Join(fixture.path, ".git"))
		if err != nil {
			t.Fatal(err)
		}
		if !gitFileInfo.Mode().IsRegular() {
			t.Fatalf("repository .git mode = %v, want regular gitfile", gitFileInfo.Mode())
		}
		gitDirInfo, err := os.Lstat(gitDir)
		if err != nil {
			t.Fatal(err)
		}
		if !gitDirInfo.IsDir() || gitDirInfo.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("Git directory mode = %v, want physical directory", gitDirInfo.Mode())
		}
		if _, err := os.Lstat(filepath.Join(gitDir, "commondir")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Git directory commondir error = %v, want not exist", err)
		}

		included := filepath.Join(t.TempDir(), "malformed-included-config")
		writeFile(t, included, "not a Git configuration line\n")
		if err := os.Chmod(included, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = os.Chmod(included, 0o600)
		})
		appendFile(t, filepath.Join(gitDir, "config"), fmt.Sprintf("[include]\n\tpath = %s\n", included))

		err = prepareSourceBeforeGit(t, fixture)
		want := wantUnallowlistedConfigError("include.path")
		if err.Error() != want {
			t.Fatalf("PrepareSource with no-commondir gitfile include error = %v, want %q", err, want)
		}
		if strings.Contains(err.Error(), included) {
			t.Fatalf("include target leaked into error: %v", err)
		}
	})
	for name, conditional := range map[string]bool{
		"unconditional include": false,
		"matching includeIf":    true,
	} {
		t.Run("linked worktree "+name+" rejects before Git", func(t *testing.T) {
			fixture, gitDir, commonDir := newLinkedWorktree(t, newRepositoryWithCommit(t))
			included := filepath.Join(t.TempDir(), "malformed-included-config")
			writeFile(t, included, "not a Git configuration line\n")
			header := "[include]"
			wantKey := "include.path"
			if conditional {
				condition := "gitdir:" + filepath.ToSlash(gitDir) + "/"
				header = `[includeIf "` + condition + `"]`
				wantKey = "includeif." + condition + ".path"
			}
			appendFile(t, filepath.Join(commonDir, "config"), fmt.Sprintf("%s\n\tpath = %s\n", header, included))
			err := prepareSourceBeforeGit(t, fixture)
			want := wantUnallowlistedConfigError(wantKey)
			if err.Error() != want {
				t.Fatalf("PrepareSource with linked-worktree %s error = %v, want %q", name, err, want)
			}
			if strings.Contains(err.Error(), included) {
				t.Fatalf("include target leaked into error: %v", err)
			}
		})
	}
	for name, test := range map[string]struct {
		config  string
		wantKey string
	}{
		"multiline include path": {
			config:  "[include]\n\tpath = %s\n",
			wantKey: "include.path",
		},
		"multiline conditional include path": {
			config:  "[includeIf \"gitdir:/Repos/\"]\n\tpath = %s\n",
			wantKey: "includeif.gitdir:/Repos/.path",
		},
		"same-line include path": {
			config:  "[include] path = %s\n",
			wantKey: "include.path",
		},
		"mixed-case include path": {
			config:  "[InClUdE]\n\tPaTh = %s\n",
			wantKey: "include.path",
		},
		"adjacent section headers before include path": {
			config:  "[core][include]\n\tpath = %s\n",
			wantKey: "include.path",
		},
		"section transition before include path": {
			config:  "[core]\n\tbare = false\n[include]\n\tpath = %s\n",
			wantKey: "include.path",
		},
	} {
		t.Run("pre-scan rejects "+name, func(t *testing.T) {
			fixture := newRepositoryWithCommit(t)
			included := filepath.Join(t.TempDir(), "malformed-included-config")
			writeFile(t, included, "not a Git configuration line\n")
			appendFile(t, filepath.Join(fixture.path, ".git", "config"), fmt.Sprintf(test.config, included))
			err := prepareSourceBeforeGit(t, fixture)
			want := wantUnallowlistedConfigError(test.wantKey)
			if err.Error() != want {
				t.Fatalf("PrepareSource with %s error = %v, want %q", name, err, want)
			}
		})
	}
	t.Run("include is reported before other rejected keys", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		included := filepath.Join(t.TempDir(), "malformed-included-config")
		writeFile(t, included, "not a Git configuration line\n")
		appendFile(t, filepath.Join(fixture.path, ".git", "config"),
			fmt.Sprintf("[core]\n\tfsmonitor = /must-not-run\n[include]\n\tpath = %s\n", included))
		err := prepareSourceBeforeGit(t, fixture)
		want := wantUnallowlistedConfigError("include.path")
		if err.Error() != want {
			t.Fatalf("PrepareSource with include and unsupported key error = %v, want %q", err, want)
		}
	})
	t.Run("aggregated unsupported keys", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		marker := filepath.Join(t.TempDir(), "executed")
		script := filepath.Join(t.TempDir(), "hostile")
		writeFile(t, script, fmt.Sprintf("#!/bin/sh\n/usr/bin/touch %q\n", marker))
		if err := os.Chmod(script, 0o700); err != nil {
			t.Fatal(err)
		}
		gitTest(t, fixture.path, "config", "--local", "core.fsmonitor", script)
		gitTest(t, fixture.path, "config", "--local", "--add", "credential.helper", "!"+script)
		gitTest(t, fixture.path, "config", "--local", "filter.payload.process", script)
		gitTest(t, fixture.path, "config", "--local", "merge.payload.driver", script)
		gitTest(t, fixture.path, "config", "--local", "--add", "credential.helper", "!"+script)
		before := hostByteSnapshot(t, fixture.path)
		tempRoot := t.TempDir()
		_, err := fixture.service.PrepareSource(context.Background(), SourceRequest{Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "secure", TempRoot: tempRoot})
		want := wantUnallowlistedConfigError("core.fsmonitor", "credential.helper", "filter.payload.process", "merge.payload.driver")
		if err == nil || err.Error() != want {
			t.Fatalf("PrepareSource with multiple unsupported keys error = %v, want %q", err, want)
		}
		if strings.Contains(err.Error(), script) {
			t.Fatalf("configured value leaked into error: %v", err)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("configured executable ran: %v", err)
		}
		entries, err := os.ReadDir(tempRoot)
		if err != nil || len(entries) != 0 {
			t.Fatalf("temporary root entries = %v, err = %v", entries, err)
		}
		assertHostByteSnapshot(t, fixture.path, before)
	})
	t.Run("command remote transport", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		marker := filepath.Join(t.TempDir(), "command-transport-ran")
		transport := filepath.Join(t.TempDir(), "remote-transport")
		writeFile(t, transport, fmt.Sprintf("#!/bin/sh\n/usr/bin/touch %q\nexit 1\n", marker))
		if err := os.Chmod(transport, 0o700); err != nil {
			t.Fatal(err)
		}
		gitTest(t, fixture.path, "config", "--local", "remote.origin.url", "ext::"+transport)
		before := hostByteSnapshot(t, fixture.path)
		tempRoot := t.TempDir()
		_, err := fixture.service.PrepareSource(context.Background(), SourceRequest{Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "secure", TempRoot: tempRoot})
		want := wantUnallowlistedConfigError("remote.origin.url")
		if err == nil || err.Error() != want {
			t.Fatalf("PrepareSource with command transport error = %v, want %q", err, want)
		}
		if strings.Contains(err.Error(), transport) {
			t.Fatalf("command transport value leaked into error: %v", err)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("command transport executed: %v", err)
		}
		entries, readErr := os.ReadDir(tempRoot)
		if readErr != nil || len(entries) != 0 {
			t.Fatalf("temporary root entries = %v, err = %v", entries, readErr)
		}
		assertHostByteSnapshot(t, fixture.path, before)
	})
}

func TestWorktreeConfigActivationIsRejectedBeforeGit(t *testing.T) {
	t.Run("valueless activation rejects before Git or included target read", func(t *testing.T) {
		fixture, _, commonDir := newLinkedWorktree(t, newRepositoryWithCommit(t))
		appendFile(t, filepath.Join(commonDir, "config"), "[extensions]\n\tworktreeConfig\n")

		included := filepath.Join(t.TempDir(), "malformed-unreadable-config")
		writeFile(t, included, "not a Git configuration line\n")
		if err := os.Chmod(included, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = os.Chmod(included, 0o600)
		})
		writeFile(t, filepath.Join(commonDir, "config.worktree"), fmt.Sprintf("[include]\n\tpath = %s\n", included))

		err := prepareSourceBeforeGit(t, fixture)
		want := wantUnallowlistedConfigError("extensions.worktreeconfig")
		if err.Error() != want {
			t.Fatalf("PrepareSource with valueless extensions.worktreeConfig error = %v, want %q", err, want)
		}
		if strings.Contains(err.Error(), included) {
			t.Fatalf("config.worktree include target leaked into error: %v", err)
		}
	})

	t.Run("explicit false ignores config.worktree and follows normal allowlist rejection", func(t *testing.T) {
		fixture, _, commonDir := newLinkedWorktree(t, newRepositoryWithCommit(t))
		appendFile(t, filepath.Join(commonDir, "config"), "[extensions]\n\tworktreeConfig = false\n")

		included := filepath.Join(t.TempDir(), "malformed-unreadable-config")
		writeFile(t, included, "not a Git configuration line\n")
		if err := os.Chmod(included, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = os.Chmod(included, 0o600)
		})
		writeFile(t, filepath.Join(commonDir, "config.worktree"), fmt.Sprintf("[include]\n\tpath = %s\n", included))

		_, err := fixture.service.PrepareSource(context.Background(), SourceRequest{
			Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "secure", TempRoot: t.TempDir(),
		})
		want := wantUnallowlistedConfigError("extensions.worktreeconfig")
		if err == nil || err.Error() != want {
			t.Fatalf("PrepareSource with extensions.worktreeConfig=false error = %v, want %q", err, want)
		}
		if strings.Contains(err.Error(), included) {
			t.Fatalf("inactive config.worktree include target leaked into error: %v", err)
		}
	})
}

func TestHostGitMetadataStructuralBoundariesRejectBeforeGit(t *testing.T) {
	t.Run("malformed gitdir declaration", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		gitDirectory := moveGitDirectoryAside(t, fixture.path)
		writeFile(t, filepath.Join(fixture.path, ".git"), "gitdir "+gitDirectory+"\n")
		assertStructuralGitMetadataError(t, prepareSourceBeforeGit(t, fixture), ".git gitfile")
	})
	t.Run("missing gitdir target", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		moveGitDirectoryAside(t, fixture.path)
		writeFile(t, filepath.Join(fixture.path, ".git"), "gitdir: "+filepath.Join(t.TempDir(), "missing")+"\n")
		assertStructuralGitMetadataError(t, prepareSourceBeforeGit(t, fixture), "Git directory target")
	})
	t.Run("symlink gitfile", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		gitDirectory := moveGitDirectoryAside(t, fixture.path)
		pointer := filepath.Join(t.TempDir(), "gitfile")
		writeFile(t, pointer, "gitdir: "+gitDirectory+"\n")
		if err := os.Symlink(pointer, filepath.Join(fixture.path, ".git")); err != nil {
			t.Fatal(err)
		}
		assertStructuralGitMetadataError(t, prepareSourceBeforeGit(t, fixture), "repository .git entry")
	})
	t.Run("oversized gitfile", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		moveGitDirectoryAside(t, fixture.path)
		gitFile := filepath.Join(fixture.path, ".git")
		writeFile(t, gitFile, "gitdir: ")
		if err := os.Truncate(gitFile, (1<<20)+1); err != nil {
			t.Fatal(err)
		}
		assertStructuralGitMetadataError(t, prepareSourceBeforeGit(t, fixture), ".git gitfile")
	})
	t.Run("malformed commondir", func(t *testing.T) {
		fixture, gitDir, _ := newLinkedWorktree(t, newRepositoryWithCommit(t))
		writeFile(t, filepath.Join(gitDir, "commondir"), "../..\nextra\n")
		assertStructuralGitMetadataError(t, prepareSourceBeforeGit(t, fixture), "common-directory pointer")
	})
	t.Run("oversized commondir", func(t *testing.T) {
		fixture, gitDir, _ := newLinkedWorktree(t, newRepositoryWithCommit(t))
		commonPointer := filepath.Join(gitDir, "commondir")
		writeFile(t, commonPointer, "../..")
		if err := os.Truncate(commonPointer, (1<<20)+1); err != nil {
			t.Fatal(err)
		}
		assertStructuralGitMetadataError(t, prepareSourceBeforeGit(t, fixture), "common-directory pointer")
	})
	t.Run("common config symlink replacement", func(t *testing.T) {
		fixture, _, commonDir := newLinkedWorktree(t, newRepositoryWithCommit(t))
		configPath := filepath.Join(commonDir, "config")
		originalPath := filepath.Join(commonDir, "config.before-replacement")
		if err := os.Rename(configPath, originalPath); err != nil {
			t.Fatal(err)
		}
		replacement := filepath.Join(t.TempDir(), "replacement-config")
		writeFile(t, replacement, "[include]\n\tpath = /must-not-read\n")
		if err := os.Symlink(replacement, configPath); err != nil {
			t.Fatal(err)
		}
		original := append([]byte(nil), mustRead(t, originalPath)...)
		replacementBytes := append([]byte(nil), mustRead(t, replacement)...)
		assertStructuralGitMetadataError(t, prepareSourceBeforeGit(t, fixture), "repository common Git configuration")
		if !bytes.Equal(mustRead(t, originalPath), original) || !bytes.Equal(mustRead(t, replacement), replacementBytes) {
			t.Fatal("common config replacement or preserved original was mutated")
		}
	})
	for name, header := range map[string]string{
		"spaced include":   "[include ]\n\tpath = /must-not-read\n",
		"spaced includeIf": "[includeIf \"gitdir:/Repos/\" ]\n\tpath = /must-not-read\n",
	} {
		t.Run(name+" is a structural parse error", func(t *testing.T) {
			fixture := newRepositoryWithCommit(t)
			appendFile(t, filepath.Join(fixture.path, ".git", "config"), header)
			_, err := fixture.service.PrepareSource(context.Background(), SourceRequest{
				Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "secure", TempRoot: t.TempDir(),
			})
			assertStructuralGitMetadataError(t, err, "inspect repository-local Git configuration:")
		})
	}
	for name, test := range map[string]struct {
		config string
		key    string
	}{
		"worktreeConfig activation": {config: "[extensions]\n\tworktreeConfig = true\\", key: "extensions.worktreeconfig"},
		"include path":              {config: "[include]\n\tpath = /x\\", key: "include.path"},
	} {
		t.Run(name+" continuation at EOF follows Git semantics", func(t *testing.T) {
			fixture := newRepositoryWithCommit(t)
			appendFile(t, filepath.Join(fixture.path, ".git", "config"), test.config)
			err := prepareSourceBeforeGit(t, fixture)
			want := wantUnallowlistedConfigError(test.key)
			if err.Error() != want {
				t.Fatalf("PrepareSource with continuation at EOF error = %v, want %q", err, want)
			}
		})
	}
	t.Run("valid continued harmless scalar", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		appendFile(t, filepath.Join(fixture.path, ".git", "config"), "[branch \"main\"]\n\tvscode-merge-base = origin/\\\nmain\n")
		artifact, err := fixture.service.PrepareSource(context.Background(), SourceRequest{
			Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "safe", TempRoot: t.TempDir(),
		})
		if err != nil {
			t.Fatalf("PrepareSource with valid continued harmless scalar: %v", err)
		}
		defer fixture.service.RemoveArtifact(artifact.BundlePath)
	})
}

func wantUnallowlistedConfigError(keys ...string) string {
	quoted := make([]string, 0, len(keys))
	for _, key := range keys {
		quoted = append(quoted, strconv.Quote(key))
	}
	return "repository-local Git configuration keys are not allowlisted: " + strings.Join(quoted, ", ") +
		"; remove a key with: git config --local --unset-all <key>"
}

func newLinkedWorktree(t *testing.T, base repositoryFixture) (repositoryFixture, string, string) {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worktreePath := filepath.Join(parent, "linked")
	gitTest(t, base.path, "worktree", "add", "--quiet", "--detach", worktreePath, "HEAD")
	gitFile := strings.TrimSuffix(strings.TrimSuffix(string(mustRead(t, filepath.Join(worktreePath, ".git"))), "\n"), "\r")
	gitDirText, ok := strings.CutPrefix(gitFile, "gitdir: ")
	if !ok || gitDirText == "" {
		t.Fatalf("linked worktree .git = %q", gitFile)
	}
	gitDir := gitDirText
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(worktreePath, gitDir)
	}
	gitDir, err = filepath.EvalSymlinks(filepath.Clean(gitDir))
	if err != nil {
		t.Fatal(err)
	}
	commonText := strings.TrimSuffix(strings.TrimSuffix(string(mustRead(t, filepath.Join(gitDir, "commondir"))), "\n"), "\r")
	if commonText == "" {
		t.Fatal("linked worktree commondir is empty")
	}
	commonDir := commonText
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(gitDir, commonDir)
	}
	commonDir, err = filepath.EvalSymlinks(filepath.Clean(commonDir))
	if err != nil {
		t.Fatal(err)
	}
	return repositoryFixture{t: t, path: worktreePath, service: base.service}, gitDir, commonDir
}

func moveGitDirectoryAside(t *testing.T, repositoryPath string) string {
	t.Helper()
	external, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(external, "gitdir")
	if err := os.Rename(filepath.Join(repositoryPath, ".git"), moved); err != nil {
		t.Fatal(err)
	}
	return moved
}

func prepareSourceBeforeGit(t *testing.T, fixture repositoryFixture) error {
	t.Helper()
	runner := &unexpectedGitRunner{}
	service, err := NewService(runner, testGitExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	fixture.service = service
	_, err = fixture.service.PrepareSource(context.Background(), SourceRequest{
		Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "secure", TempRoot: t.TempDir(),
	})
	if err == nil {
		t.Fatal("PrepareSource accepted rejected repository-local Git metadata")
	}
	if runner.calls != 0 {
		t.Fatalf("PrepareSource invoked Git %d time(s) before rejecting repository-local Git metadata", runner.calls)
	}
	return err
}

func assertStructuralGitMetadataError(t *testing.T, err error, boundary string) {
	t.Helper()
	if err == nil {
		t.Fatal("repository-local Git metadata was accepted")
	}
	if !strings.Contains(err.Error(), boundary) {
		t.Fatalf("structural Git metadata error = %v, want boundary %q", err, boundary)
	}
	if strings.Contains(err.Error(), "not allowlisted") || strings.Contains(err.Error(), "git config --local --unset-all") {
		t.Fatalf("structural Git metadata error was reported as policy remediation: %v", err)
	}
}

type unexpectedGitRunner struct {
	calls int
}

func (runner *unexpectedGitRunner) Run(context.Context, Command) (Exit, error) {
	runner.calls++
	return Exit{Code: -1}, errors.New("unexpected Git invocation")
}

func TestApplyRollbackCaptureIsBoundedBeforeMutation(t *testing.T) {
	fixture := newRepositoryWithCommit(t)
	identity, err := fixture.service.captureRepositoryIdentity(context.Background(), fixture.repository(), fixture.path)
	if err != nil {
		t.Fatal(err)
	}
	fixture.identity = identity
	large := filepath.Join(fixture.path, "large.bin")
	if err := os.WriteFile(large, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(large, maxApplyRollbackBytes+1); err != nil {
		t.Fatal(err)
	}
	before := hostByteSnapshot(t, fixture.path)
	if _, err := fixture.service.captureApplyRollback(context.Background(), fixture.repository(), []string{"large.bin"}); err == nil || !strings.Contains(err.Error(), "rollback bytes exceed") {
		t.Fatalf("captureApplyRollback error = %v", err)
	}
	assertHostByteSnapshot(t, fixture.path, before)
}

func TestPrepareApplyRejectsMacOSGitMetadataAliasesBeforeMutation(t *testing.T) {
	for _, alias := range []string{".GIT/config", "nested/.g\u200cit/config"} {
		t.Run(alias, func(t *testing.T) {
			runner := &gitPathAliasRunner{delegate: OSRunner{}, path: alias}
			fixture := newTransferFixture(t, runner)
			defer fixture.cleanup()
			before := hostByteSnapshot(t, fixture.host.path)
			if _, err := fixture.host.service.PrepareApply(context.Background(), fixture.applyRequest()); err == nil || !strings.Contains(err.Error(), "aliases Git metadata on macOS") {
				t.Fatalf("PrepareApply alias error = %v", err)
			}
			if runner.mutated {
				t.Fatal("PrepareApply ran the host Git mutation")
			}
			assertHostByteSnapshot(t, fixture.host.path, before)
		})
	}
}

func TestApplyPreconditionsLeaveHostByteIdentical(t *testing.T) {
	t.Run("host advanced", func(t *testing.T) {
		fixture := newTransferFixture(t, OSRunner{})
		defer fixture.cleanup()
		writeFile(t, filepath.Join(fixture.host.path, "advanced.txt"), "advanced\n")
		gitTest(t, fixture.host.path, "add", "advanced.txt")
		gitTest(t, fixture.host.path, "commit", "-m", "advance host")
		before := hostByteSnapshot(t, fixture.host.path)
		_, err := fixture.host.service.PrepareApply(context.Background(), fixture.applyRequest())
		if err == nil || !strings.Contains(err.Error(), "host advanced") {
			t.Fatalf("Apply error = %v", err)
		}
		assertHostByteSnapshot(t, fixture.host.path, before)
	})
	t.Run("fingerprint changed", func(t *testing.T) {
		fixture := newTransferFixture(t, OSRunner{})
		defer fixture.cleanup()
		request := fixture.applyRequest()
		request.TrackedFingerprint = strings.Repeat("0", 64)
		before := hostByteSnapshot(t, fixture.host.path)
		if _, err := fixture.host.service.PrepareApply(context.Background(), request); err == nil {
			t.Fatal("Apply accepted changed fingerprint")
		}
		assertHostByteSnapshot(t, fixture.host.path, before)
	})
	t.Run("untracked collision", func(t *testing.T) {
		fixture := newTransferFixture(t, OSRunner{})
		defer fixture.cleanup()
		writeFile(t, filepath.Join(fixture.host.path, "new.txt"), "host untracked bytes\n")
		before := hostByteSnapshot(t, fixture.host.path)
		if _, err := fixture.host.service.PrepareApply(context.Background(), fixture.applyRequest()); err == nil || !strings.Contains(err.Error(), "collides") {
			t.Fatalf("Apply collision error = %v", err)
		}
		assertHostByteSnapshot(t, fixture.host.path, before)
	})
	t.Run("fetched ref moved", func(t *testing.T) {
		fixture := newTransferFixture(t, OSRunner{})
		defer fixture.cleanup()
		transaction, err := fixture.host.service.PrepareApply(context.Background(), fixture.applyRequest())
		if err != nil {
			t.Fatal(err)
		}
		gitTest(t, fixture.host.path, "update-ref", fixture.fetch.HostRef, fixture.source.SourceRevision)
		before := hostByteSnapshot(t, fixture.host.path)
		if _, err := transaction.Commit(context.Background()); err == nil || !strings.Contains(err.Error(), "want recorded result") {
			t.Fatalf("Apply moved-ref boundary error = %v", err)
		}
		assertHostByteSnapshot(t, fixture.host.path, before)
	})
	t.Run("fetched ref missing", func(t *testing.T) {
		fixture := newTransferFixture(t, OSRunner{})
		defer fixture.cleanup()
		gitTest(t, fixture.host.path, "update-ref", "-d", fixture.fetch.HostRef)
		before := hostByteSnapshot(t, fixture.host.path)
		if _, err := fixture.host.service.PrepareApply(context.Background(), fixture.applyRequest()); err == nil || !strings.Contains(err.Error(), "resolve fetched result ref") {
			t.Fatalf("Apply missing-ref error = %v", err)
		}
		assertHostByteSnapshot(t, fixture.host.path, before)
	})
	t.Run("snapshot tree mismatch", func(t *testing.T) {
		fixture := newSnapshotApplyFixture(t)
		writeFile(t, filepath.Join(fixture.host.path, "mismatch.txt"), "one byte mismatch\n")
		gitTest(t, fixture.host.path, "add", "mismatch.txt")
		gitTest(t, fixture.host.path, "commit", "-m", "mismatched tree")
		before := hostByteSnapshot(t, fixture.host.path)
		_, err := fixture.host.service.PrepareApply(context.Background(), fixture.applyRequest())
		if err == nil || !strings.Contains(err.Error(), "host HEAD tree does not match snapshot source tree") {
			t.Fatalf("snapshot tree mismatch error = %v", err)
		}
		assertHostByteSnapshot(t, fixture.host.path, before)
	})
	t.Run("snapshot dirty host", func(t *testing.T) {
		fixture := newSnapshotApplyFixture(t)
		writeFile(t, filepath.Join(fixture.host.path, "tracked.txt"), "dirty after baseline commit\n")
		before := hostByteSnapshot(t, fixture.host.path)
		_, err := fixture.host.service.PrepareApply(context.Background(), fixture.applyRequest())
		if err == nil || !strings.Contains(err.Error(), "host index or tracked work tree is not clean") {
			t.Fatalf("snapshot dirty host error = %v", err)
		}
		assertHostByteSnapshot(t, fixture.host.path, before)
	})
	t.Run("snapshot unmerged host", func(t *testing.T) {
		fixture := newSnapshotApplyFixture(t)
		gitTest(t, fixture.host.path, "checkout", "-b", "conflict-side")
		writeFile(t, filepath.Join(fixture.host.path, "tracked.txt"), "conflict side\n")
		gitTest(t, fixture.host.path, "commit", "-am", "conflict side")
		gitTest(t, fixture.host.path, "checkout", "main")
		writeFile(t, filepath.Join(fixture.host.path, "tracked.txt"), "conflict main\n")
		gitTest(t, fixture.host.path, "commit", "-am", "conflict main")
		command := exec.Command("git", "merge", "conflict-side")
		command.Dir = fixture.host.path
		command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
		if output, err := command.CombinedOutput(); err == nil {
			t.Fatalf("conflicting merge unexpectedly succeeded: %s", output)
		}
		before := hostByteSnapshot(t, fixture.host.path)
		_, err := fixture.host.service.PrepareApply(context.Background(), fixture.applyRequest())
		if err == nil || !strings.Contains(err.Error(), "host repository has unmerged paths") {
			t.Fatalf("snapshot unmerged host error = %v", err)
		}
		assertHostByteSnapshot(t, fixture.host.path, before)
	})
	t.Run("snapshot fetched ref moved at boundary", func(t *testing.T) {
		fixture := newSnapshotApplyFixture(t)
		transaction, err := fixture.host.service.PrepareApply(context.Background(), fixture.applyRequest())
		if err != nil {
			t.Fatal(err)
		}
		gitTest(t, fixture.host.path, "update-ref", fixture.fetch.HostRef, fixture.source.SourceRevision)
		before := hostByteSnapshot(t, fixture.host.path)
		if _, err := transaction.Commit(context.Background()); err == nil || !strings.Contains(err.Error(), "want recorded result") {
			t.Fatalf("snapshot moved-ref boundary error = %v", err)
		}
		assertHostByteSnapshot(t, fixture.host.path, before)
		if entries, err := os.ReadDir(fixture.tempRoot); err != nil || len(entries) != 0 {
			t.Fatalf("snapshot moved-ref quarantine artifacts = %#v, %v", entries, err)
		}
	})
	if service, err := NewService(OSRunner{}, testGitExecutable(t)); err != nil {
		t.Fatal(err)
	} else if _, err := service.PrepareApply(context.Background(), ApplyRequest{Repository: Repository{Name: "workspace", HostPath: t.TempDir(), GuestPath: "/workspace"}, SourceRevision: strings.Repeat("0", 40), TrackedFingerprint: strings.Repeat("0", 64), FetchedRef: "refs/remotes/dsx/alpha/evil", ExpectedCommit: strings.Repeat("1", 40)}); err == nil {
		t.Fatal("Apply accepted hostile fetched ref")
	}
}

func TestApplyGitFailureRollsBackIndexAndWorktree(t *testing.T) {
	fixture := newTransferFixture(t, OSRunner{})
	defer fixture.cleanup()
	failing, err := NewService(postMutationFailureRunner{delegate: OSRunner{}}, testGitExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	before := hostByteSnapshot(t, fixture.host.path)
	request := fixture.applyRequest()
	transaction, err := failing.PrepareApply(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	_, err = transaction.Commit(context.Background())
	if err == nil {
		t.Fatal("Commit unexpectedly succeeded")
	}
	if rollbackErr := transaction.Rollback(context.Background()); rollbackErr != nil {
		t.Fatalf("Rollback() error = %v", rollbackErr)
	}
	assertHostByteSnapshot(t, fixture.host.path, before)
	if got := gitTest(t, fixture.host.path, "status", "--porcelain"); got != "" {
		t.Fatalf("repository dirty after rollback: %q", got)
	}
}

func TestSnapshotApplyGitFailureRollsBackAndCleansQuarantine(t *testing.T) {
	fixture := newSnapshotApplyFixture(t)
	failing, err := NewService(postMutationFailureRunner{delegate: OSRunner{}}, testGitExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	before := hostByteSnapshot(t, fixture.host.path)
	transaction, err := failing.PrepareApply(context.Background(), fixture.applyRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Commit(context.Background()); err == nil {
		t.Fatal("snapshot Commit unexpectedly succeeded")
	}
	if rollbackErr := transaction.Rollback(context.Background()); rollbackErr != nil {
		t.Fatalf("snapshot Rollback() error = %v", rollbackErr)
	}
	assertHostByteSnapshot(t, fixture.host.path, before)
	if entries, err := os.ReadDir(fixture.tempRoot); err != nil || len(entries) != 0 {
		t.Fatalf("snapshot apply quarantine leaked after rollback: %#v, %v", entries, err)
	}
}

func TestApplyRollbackRefusesConcurrentPostMergeEdit(t *testing.T) {
	fixture := newTransferFixture(t, OSRunner{})
	defer fixture.cleanup()
	transaction, err := fixture.host.service.PrepareApply(context.Background(), fixture.applyRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	concurrent := []byte("concurrent host edit after merge\n")
	if err := os.WriteFile(filepath.Join(fixture.host.path, "binary.dat"), concurrent, 0o644); err != nil {
		t.Fatal(err)
	}
	beforeRollback := hostByteSnapshot(t, fixture.host.path)
	err = transaction.Rollback(context.Background())
	if err == nil || !strings.Contains(err.Error(), "compare-and-swap refused") {
		t.Fatalf("Rollback() error = %v, want compare-and-swap refusal", err)
	}
	assertHostByteSnapshot(t, fixture.host.path, beforeRollback)
	if got := mustRead(t, filepath.Join(fixture.host.path, "binary.dat")); !bytes.Equal(got, concurrent) {
		t.Fatalf("concurrent edit was overwritten: %q", got)
	}
}

func TestApplyRollbackDescriptorRootRejectsSymlinkSwappedParent(t *testing.T) {
	fixture := newRepository(t)
	nested := filepath.Join(fixture.path, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(nested, "target.txt")
	writeFile(t, target, "before\n")
	gitTest(t, fixture.path, "add", "nested/target.txt")
	gitTest(t, fixture.path, "commit", "-m", "nested source")
	identity, err := fixture.service.captureRepositoryIdentity(context.Background(), fixture.repository(), fixture.path)
	if err != nil {
		t.Fatal(err)
	}
	fixture.identity = identity
	rollback, err := fixture.service.captureApplyRollback(context.Background(), fixture.repository(), []string{"nested/target.txt"})
	if err != nil {
		t.Fatal(err)
	}
	defer rollback.close()
	writeFile(t, target, "DSX mutation\n")
	if err := rollback.captureMutatedState(); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(fixture.path, "nested.dsx-evidence")
	if err := os.Rename(nested, evidence); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "target.txt")
	writeFile(t, sentinel, "outside sentinel\n")
	if err := os.Symlink(outside, nested); err != nil {
		t.Fatal(err)
	}
	err = rollback.restore()
	if err == nil || !strings.Contains(err.Error(), "compare-and-swap") {
		t.Fatalf("restore() error = %v, want descriptor-rooted compare-and-swap refusal", err)
	}
	if got := string(mustRead(t, sentinel)); got != "outside sentinel\n" {
		t.Fatalf("rollback wrote outside repository root: %q", got)
	}
	if got := string(mustRead(t, filepath.Join(evidence, "target.txt"))); got != "DSX mutation\n" {
		t.Fatalf("rollback destroyed preserved evidence: %q", got)
	}
	link, err := os.Readlink(nested)
	if err != nil || link != outside {
		t.Fatalf("swapped parent changed: link=%q err=%v", link, err)
	}
}

func TestApplyFailureDisablesRerereHooksAndCustomMergeExecution(t *testing.T) {
	t.Run("rerere and hook metadata remain byte-identical", func(t *testing.T) {
		fixture := newTransferFixture(t, OSRunner{})
		defer fixture.cleanup()
		gitTest(t, fixture.host.path, "config", "--local", "rerere.enabled", "true")
		gitTest(t, fixture.host.path, "config", "--local", "rerere.autoupdate", "true")
		hookMarker := filepath.Join(t.TempDir(), "hook-ran")
		hook := filepath.Join(fixture.host.path, ".git", "hooks", "post-merge")
		writeFile(t, hook, fmt.Sprintf("#!/bin/sh\n/usr/bin/touch %q\n", hookMarker))
		if err := os.Chmod(hook, 0o700); err != nil {
			t.Fatal(err)
		}
		rrEntry := filepath.Join(fixture.host.path, ".git", "rr-cache", strings.Repeat("a", 40))
		if err := os.MkdirAll(rrEntry, 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(rrEntry, "preimage"), "existing rerere metadata\n")
		runner := &applyGuardFailureRunner{delegate: OSRunner{}, expectedCommit: fixture.resultCommit}
		service, err := NewService(runner, testGitExecutable(t))
		if err != nil {
			t.Fatal(err)
		}
		before := hostByteSnapshot(t, fixture.host.path)
		transaction, err := service.PrepareApply(context.Background(), fixture.applyRequest())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.Commit(context.Background()); err == nil {
			t.Fatal("Commit unexpectedly succeeded")
		}
		if err := transaction.Rollback(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !runner.guarded {
			t.Fatal("squash apply did not use command-scoped rerere guards and the recorded full commit")
		}
		if _, err := os.Stat(hookMarker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("post-merge hook executed: %v", err)
		}
		assertHostByteSnapshot(t, fixture.host.path, before)
	})

	t.Run("custom merge driver is rejected before mutation", func(t *testing.T) {
		fixture := newTransferFixture(t, OSRunner{})
		defer fixture.cleanup()
		marker := filepath.Join(t.TempDir(), "merge-driver-ran")
		driver := filepath.Join(t.TempDir(), "merge-driver")
		writeFile(t, driver, fmt.Sprintf("#!/bin/sh\n/usr/bin/touch %q\nexit 1\n", marker))
		if err := os.Chmod(driver, 0o700); err != nil {
			t.Fatal(err)
		}
		gitTest(t, fixture.host.path, "config", "--local", "merge.payload.driver", driver)
		before := hostByteSnapshot(t, fixture.host.path)
		if _, err := fixture.host.service.PrepareApply(context.Background(), fixture.applyRequest()); err == nil || !strings.Contains(err.Error(), "repository-local Git configuration") {
			t.Fatalf("custom merge driver error = %v", err)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("custom merge driver executed: %v", err)
		}
		assertHostByteSnapshot(t, fixture.host.path, before)
	})
}

func TestCompositeMemberTwoFailureRestoresMemberOneByteIndexAndHEADIdentical(t *testing.T) {
	first := newTransferFixture(t, OSRunner{})
	defer first.cleanup()
	second := newTransferFixture(t, OSRunner{})
	defer second.cleanup()
	failingSecond, err := NewService(postMutationFailureRunner{delegate: OSRunner{}}, testGitExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	firstBefore := hostByteSnapshot(t, first.host.path)
	firstHEAD := strings.TrimSpace(gitTest(t, first.host.path, "rev-parse", "HEAD"))
	firstTransaction, err := first.host.service.PrepareApply(context.Background(), first.applyRequest())
	if err != nil {
		t.Fatal(err)
	}
	secondTransaction, err := failingSecond.PrepareApply(context.Background(), second.applyRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := firstTransaction.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := secondTransaction.Commit(context.Background()); err == nil {
		t.Fatal("second member commit unexpectedly succeeded")
	}
	if err := secondTransaction.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := firstTransaction.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertHostByteSnapshot(t, first.host.path, firstBefore)
	if after := strings.TrimSpace(gitTest(t, first.host.path, "rev-parse", "HEAD")); after != firstHEAD {
		t.Fatalf("member one HEAD changed from %s to %s", firstHEAD, after)
	}
}

func TestApplyHostRaceAfterBoundaryValidationRollsBack(t *testing.T) {
	fixture := newTransferFixture(t, OSRunner{})
	defer fixture.cleanup()
	runner := &boundaryRaceRunner{delegate: OSRunner{}, path: filepath.Join(fixture.host.path, "binary.dat")}
	service, err := NewService(runner, testGitExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	before := hostByteSnapshot(t, fixture.host.path)
	transaction, err := service.PrepareApply(context.Background(), fixture.applyRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Commit(context.Background()); err == nil {
		t.Fatal("Commit accepted host mutation racing the apply")
	}
	if err := transaction.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertHostByteSnapshot(t, fixture.host.path, before)
}

type repositoryFixture struct {
	t        *testing.T
	path     string
	service  *Service
	identity RepositoryIdentity
}

func newRepository(t *testing.T) repositoryFixture {
	t.Helper()
	service, err := NewService(OSRunner{}, testGitExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitTest(t, "", "init", "--quiet", "--initial-branch=main", root)
	gitTest(t, root, "config", "user.name", "DSX Test")
	gitTest(t, root, "config", "user.email", "dsx@example.invalid")
	gitTest(t, root, "config", "commit.gpgSign", "false")
	return repositoryFixture{t: t, path: root, service: service}
}

func newRepositoryWithCommit(t *testing.T) repositoryFixture {
	t.Helper()
	fixture := newRepository(t)
	writeFile(t, filepath.Join(fixture.path, "tracked.txt"), "source\n")
	gitTest(t, fixture.path, "add", "tracked.txt")
	gitTest(t, fixture.path, "commit", "-m", "source")
	return fixture
}

func (fixture repositoryFixture) repository() Repository {
	return Repository{Name: "workspace", HostPath: fixture.path, GuestPath: "/workspace", Identity: fixture.identity}
}

func prepareSourceTest(t *testing.T, fixture *repositoryFixture, workspace string) SourceArtifact {
	t.Helper()
	artifact, err := fixture.service.PrepareSource(context.Background(), SourceRequest{Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: workspace, TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	fixture.identity = artifact.Repository.Identity
	return artifact
}

type transferFixture struct {
	t            *testing.T
	host         repositoryFixture
	source       SourceArtifact
	resultBundle string
	resultDigest string
	resultCommit string
	fetch        FetchResult
}

func newTransferFixture(t *testing.T, runner Runner) transferFixture {
	t.Helper()
	fixture := newUnfetchedTransferFixture(t, runner)
	fetch, err := fixture.host.service.FetchResult(context.Background(), FetchRequest{Repository: fixture.host.repository(), Workspace: "alpha", BundlePath: fixture.resultBundle, Digest: fixture.resultDigest, ExpectedCommit: fixture.resultCommit})
	if err != nil {
		t.Fatal(err)
	}
	fixture.fetch = fetch
	return fixture
}

func newUnfetchedTransferFixture(t *testing.T, runner Runner) transferFixture {
	t.Helper()
	host := newRepository(t)
	service, err := NewService(runner, testGitExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	host.service = service
	writeFile(t, filepath.Join(host.path, "deleted.txt"), "delete me\n")
	writeFile(t, filepath.Join(host.path, "old.txt"), "rename me\n")
	writeFile(t, filepath.Join(host.path, "binary.dat"), []byte{0, 1, 0, 2})
	gitTest(t, host.path, "add", "-A")
	gitTest(t, host.path, "commit", "-m", "source")
	source := prepareSourceTest(t, &host, "alpha")
	guest := filepath.Join(t.TempDir(), "guest")
	gitTest(t, "", "init", "--quiet", guest)
	gitTest(t, guest, "fetch", "--no-tags", "--no-write-fetch-head", "--", source.BundlePath, source.BundleRef)
	gitTest(t, guest, "config", "user.name", "DSX Result")
	gitTest(t, guest, "config", "user.email", "dsx@example.invalid")
	gitTest(t, guest, "checkout", "--quiet", "--detach", source.SourceRevision)
	gitTest(t, guest, "switch", "--quiet", "-c", "dsx/alpha")
	if sameGitDirectory(t, host.path, guest) {
		t.Fatal("guest clone shares Git directory with host")
	}
	if err := os.Remove(filepath.Join(guest, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(guest, "old.txt"), filepath.Join(guest, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(guest, "new.txt"), "new file\n")
	writeFile(t, filepath.Join(guest, "binary.dat"), []byte{0, 1, 2, 0xff, 0, 4})
	gitTest(t, guest, "add", "-A")
	gitTest(t, guest, "commit", "-m", "DSX deterministic result")
	resultCommit := strings.TrimSpace(gitTest(t, guest, "rev-parse", "HEAD"))
	resultBundle := filepath.Join(t.TempDir(), "result.bundle")
	gitTest(t, guest, "bundle", "create", resultBundle, "refs/heads/dsx/alpha")
	if err := os.Chmod(resultBundle, 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := bundleSHA256(resultBundle)
	if err != nil {
		t.Fatal(err)
	}
	return transferFixture{
		t: t, host: host, source: source, resultBundle: resultBundle,
		resultDigest: digest, resultCommit: resultCommit,
	}
}

func (fixture transferFixture) cleanup() {
	_ = fixture.host.service.RemoveArtifact(fixture.source.BundlePath)
}

func (fixture transferFixture) applyRequest() ApplyRequest {
	return ApplyRequest{Repository: fixture.host.repository(), SourceRevision: fixture.source.SourceRevision, TrackedFingerprint: fixture.source.TrackedFingerprint, FetchedRef: fixture.fetch.HostRef, ExpectedCommit: fixture.resultCommit}
}

type snapshotApplyFixture struct {
	host         repositoryFixture
	source       SourceArtifact
	fetch        FetchResult
	resultCommit string
	tempRoot     string
}

func newSnapshotApplyFixture(t *testing.T) snapshotApplyFixture {
	t.Helper()
	host := newRepositoryWithCommit(t)
	writeFile(t, filepath.Join(host.path, "tracked.txt"), "captured baseline\n")
	source, err := host.service.PrepareSource(context.Background(), SourceRequest{
		Repository: host.repository(), ApprovedRoot: host.path,
		Workspace: "alpha", TempRoot: t.TempDir(), Snapshot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	host.identity = source.Repository.Identity
	t.Cleanup(func() { _ = host.service.RemoveArtifact(source.BundlePath) })

	guest := filepath.Join(t.TempDir(), "guest")
	gitTest(t, "", "init", "--quiet", guest)
	gitTest(t, guest, "fetch", "--no-tags", "--no-write-fetch-head", "--", source.BundlePath, source.BundleRef)
	gitTest(t, guest, "config", "user.name", "DSX Result")
	gitTest(t, guest, "config", "user.email", "dsx@example.invalid")
	gitTest(t, guest, "checkout", "--quiet", "-b", "dsx/alpha", source.SourceRevision)
	writeFile(t, filepath.Join(guest, "agent.txt"), "agent result\n")
	gitTest(t, guest, "add", "agent.txt")
	gitTest(t, guest, "commit", "-m", "agent result")
	resultCommit := strings.TrimSpace(gitTest(t, guest, "rev-parse", "HEAD"))
	bundle := filepath.Join(t.TempDir(), "result.bundle")
	gitTest(t, guest, "bundle", "create", bundle, "refs/heads/dsx/alpha")
	if err := os.Chmod(bundle, ResultBundleMode); err != nil {
		t.Fatal(err)
	}
	digest, err := bundleSHA256(bundle)
	if err != nil {
		t.Fatal(err)
	}

	gitTest(t, host.path, "add", "-A")
	gitTest(t, host.path, "commit", "-m", "commit captured baseline")
	fetch, err := host.service.FetchResult(context.Background(), FetchRequest{
		Repository: host.repository(), Workspace: "alpha", BundlePath: bundle,
		Digest: digest, ExpectedCommit: resultCommit,
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshotApplyFixture{host: host, source: source, fetch: fetch, resultCommit: resultCommit, tempRoot: t.TempDir()}
}

func (fixture snapshotApplyFixture) applyRequest() ApplyRequest {
	return ApplyRequest{
		Repository: fixture.host.repository(), SourceRevision: fixture.source.SourceRevision, SourceSnapshot: true,
		SourceHeadRevision: fixture.source.SourceHeadRevision, SourceTree: fixture.source.SourceTree,
		TrackedFingerprint: fixture.source.TrackedFingerprint, FetchedRef: fixture.fetch.HostRef,
		ExpectedCommit: fixture.resultCommit, TempRoot: fixture.tempRoot,
	}
}

type diffRepositoryRunner struct {
	delegate Runner
	roots    []string
	insecure bool
}

func (runner *diffRepositoryRunner) Run(ctx context.Context, command Command) (Exit, error) {
	if containsArg(command.Argv, "init") && containsArg(command.Argv, "--bare") {
		repositoryPath := command.Argv[len(command.Argv)-1]
		info, err := os.Lstat(repositoryPath)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
			runner.insecure = true
		}
		runner.roots = append(runner.roots, filepath.Dir(repositoryPath))
	}
	return runner.delegate.Run(ctx, command)
}

func (runner *diffRepositoryRunner) assertPrivateRepositoriesRemoved(t *testing.T) {
	t.Helper()
	if runner.insecure {
		t.Fatal("private diff repository was not a mode-0700 non-symlink directory")
	}
	if len(runner.roots) == 0 {
		t.Fatal("diff did not materialize a private repository")
	}
	for _, root := range runner.roots {
		if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("private diff root %q remains: %v", root, err)
		}
	}
	runner.roots = nil
}

type gitPathAliasRunner struct {
	delegate Runner
	path     string
	mutated  bool
}

func (runner *gitPathAliasRunner) Run(ctx context.Context, command Command) (Exit, error) {
	if containsArg(command.Argv, "merge") && containsArg(command.Argv, "--squash") {
		runner.mutated = true
	}
	if containsArg(command.Argv, "diff") && containsArg(command.Argv, "--name-status") {
		_, err := command.Stdout.Write([]byte("A\x00" + runner.path + "\x00"))
		return Exit{}, err
	}
	return runner.delegate.Run(ctx, command)
}

type applyGuardFailureRunner struct {
	delegate       Runner
	expectedCommit string
	guarded        bool
}

func (runner *applyGuardFailureRunner) Run(ctx context.Context, command Command) (Exit, error) {
	exit, err := runner.delegate.Run(ctx, command)
	if err == nil && containsArg(command.Argv, "merge") && containsArg(command.Argv, "--squash") {
		runner.guarded = containsArg(command.Argv, "rerere.enabled=false") &&
			containsArg(command.Argv, "rerere.autoupdate=false") &&
			containsArg(command.Argv, "core.hooksPath="+os.DevNull) &&
			containsArg(command.Argv, runner.expectedCommit)
		return Exit{Code: 97}, errors.New("injected interruption after guarded squash mutation")
	}
	return exit, err
}

type postMutationFailureRunner struct{ delegate Runner }

func (runner postMutationFailureRunner) Run(ctx context.Context, command Command) (Exit, error) {
	exit, err := runner.delegate.Run(ctx, command)
	if err == nil && containsArg(command.Argv, "merge") && containsArg(command.Argv, "--squash") {
		return Exit{Code: 99}, errors.New("injected interruption after Git mutation")
	}
	return exit, err
}

type boundaryRaceRunner struct {
	delegate Runner
	path     string
	mutated  bool
}

func (runner *boundaryRaceRunner) Run(ctx context.Context, command Command) (Exit, error) {
	if !runner.mutated && containsArg(command.Argv, "merge") && containsArg(command.Argv, "--squash") {
		runner.mutated = true
		if err := os.WriteFile(runner.path, []byte("racing host bytes\n"), 0o644); err != nil {
			return Exit{Code: -1}, err
		}
		exit, err := runner.delegate.Run(ctx, command)
		if err == nil {
			return Exit{Code: 98}, errors.New("injected host race after mutation boundary")
		}
		return exit, err
	}
	return runner.delegate.Run(ctx, command)
}

func containsArg(arguments []string, target string) bool {
	for _, argument := range arguments {
		if argument == target {
			return true
		}
	}
	return false
}

func testGitExecutable(t *testing.T) string {
	t.Helper()
	discovered, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	absolute, err := filepath.Abs(discovered)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func gitTest(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return string(output)
}

func writeFile(t *testing.T, filePath string, content any) {
	t.Helper()
	var data []byte
	switch value := content.(type) {
	case string:
		data = []byte(value)
	case []byte:
		data = value
	default:
		t.Fatalf("unsupported file content %T", content)
	}
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, filePath, content string) {
	t.Helper()
	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString(content)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
}

func writeFileMode(t *testing.T, filePath, content string, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(filePath, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, filePath string) []byte {
	t.Helper()
	value, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func sameGitDirectory(t *testing.T, left, right string) bool {
	t.Helper()
	leftGit := strings.TrimSpace(gitTest(t, left, "rev-parse", "--absolute-git-dir"))
	rightGit := strings.TrimSpace(gitTest(t, right, "rev-parse", "--absolute-git-dir"))
	leftInfo, err := os.Stat(leftGit)
	if err != nil {
		t.Fatal(err)
	}
	rightInfo, err := os.Stat(rightGit)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(leftInfo, rightInfo)
}

func hostByteSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	err := filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, _ = fmt.Fprintf(hash, "%v\x00", info.Mode())
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(filePath)
			if err != nil {
				return err
			}
			_, _ = hash.Write([]byte(target))
		} else {
			data, err := os.ReadFile(filePath)
			if err != nil {
				return err
			}
			_, _ = hash.Write(data)
		}
		result[filepath.ToSlash(relative)] = hex.EncodeToString(hash.Sum(nil))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertHostByteSnapshot(t *testing.T, root string, before map[string]string) {
	t.Helper()
	after := hostByteSnapshot(t, root)
	if !reflect.DeepEqual(after, before) {
		beforeKeys, afterKeys := make([]string, 0, len(before)), make([]string, 0, len(after))
		changed := make([]string, 0)
		for key, beforeHash := range before {
			beforeKeys = append(beforeKeys, key)
			if afterHash, found := after[key]; found && afterHash != beforeHash {
				changed = append(changed, key)
			}
		}
		for key := range after {
			afterKeys = append(afterKeys, key)
		}
		sort.Strings(beforeKeys)
		sort.Strings(afterKeys)
		sort.Strings(changed)
		t.Fatalf("host bytes changed\nchanged=%v\nbefore keys=%v\nafter keys=%v", changed, beforeKeys, afterKeys)
	}
}
