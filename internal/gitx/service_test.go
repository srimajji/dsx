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

func TestPrepareUpdateSourceRequiresCleanMatchingAdvancedBranch(t *testing.T) {
	tests := map[string]struct {
		mutate  func(*testing.T, repositoryFixture)
		wantErr string
	}{
		"successful update": {
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
		"dirty host": {
			mutate: func(t *testing.T, fixture repositoryFixture) {
				writeFile(t, filepath.Join(fixture.path, "tracked.txt"), "dirty\n")
			},
			wantErr: "tracked or index changes",
		},
		"no newer commit": {
			mutate:  func(*testing.T, repositoryFixture) {},
			wantErr: "no newer committed revision",
		},
		"rewritten source history": {
			mutate: func(t *testing.T, fixture repositoryFixture) {
				tree := strings.TrimSpace(gitTest(t, fixture.path, "write-tree"))
				revision := strings.TrimSpace(gitTest(t, fixture.path, "commit-tree", tree, "-m", "unrelated source"))
				gitTest(t, fixture.path, "update-ref", "refs/heads/main", revision)
			},
			wantErr: "does not descend from recorded source revision",
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
			if artifact.SourceBranch != "main" || artifact.SourceRevision == source.SourceRevision {
				t.Fatalf("updated artifact = %#v", artifact)
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
	status, err := fixture.host.service.Status(context.Background(), StatusRequest{Repository: fixture.host.repository(), Workspace: "alpha", SourceBranch: fixture.source.SourceBranch, SourceRevision: fixture.source.SourceRevision, WorkspaceBranch: "dsx/alpha", ResultCommit: fixture.resultCommit, TrackedFingerprint: fixture.source.TrackedFingerprint, FetchedCommit: fixture.resultCommit})
	if err != nil {
		t.Fatal(err)
	}
	if !status.HostTrackedClean || status.SourceBranch != fixture.source.SourceBranch || status.WorkspaceBranch != "dsx/alpha" || status.HostCommit != fixture.source.SourceRevision || !status.Fetched || status.FetchedCommit != fixture.resultCommit {
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
		artifact, err := fixture.service.PrepareSource(context.Background(), SourceRequest{Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "safe", TempRoot: t.TempDir()})
		if err != nil {
			t.Fatalf("PrepareSource with ordinary clone configuration: %v", err)
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
			want := fmt.Sprintf("repository-local Git configuration %q is not allowlisted", strings.ToLower(key))
			if err == nil || err.Error() != want {
				t.Fatalf("PrepareSource with %s error = %v, want %q", key, err, want)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s executable ran: %v", key, err)
			}
		})
	}
	t.Run("command remote transport", func(t *testing.T) {
		fixture := newRepositoryWithCommit(t)
		gitTest(t, fixture.path, "config", "--local", "remote.origin.url", "ext::touch command-transport-ran")
		_, err := fixture.service.PrepareSource(context.Background(), SourceRequest{Repository: fixture.repository(), ApprovedRoot: fixture.path, Workspace: "secure", TempRoot: t.TempDir()})
		if err == nil || err.Error() != `repository-local Git configuration "remote.origin.url" is not allowlisted` {
			t.Fatalf("PrepareSource with command transport error = %v", err)
		}
	})
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
		for key := range before {
			beforeKeys = append(beforeKeys, key)
		}
		for key := range after {
			afterKeys = append(afterKeys, key)
		}
		sort.Strings(beforeKeys)
		sort.Strings(afterKeys)
		t.Fatalf("host bytes changed\nbefore keys=%v\nafter keys=%v", beforeKeys, afterKeys)
	}
}
