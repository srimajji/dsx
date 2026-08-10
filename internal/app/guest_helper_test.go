package app

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/srimajji/dsx/internal/runtime"
)

func TestStageGuestHelperMountDirectoryContainsOnlyVerifiedHelper(t *testing.T) {
	sourceRoot := canonicalTemporaryDirectory(t)
	source := filepath.Join(sourceRoot, "dsx-guest")
	contents := []byte("verified guest helper")
	if err := os.WriteFile(source, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "sibling-secret"), []byte("must not mount"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheParent := canonicalTemporaryDirectory(t)
	staged, err := StageGuestHelper(runtime.HostPath(source), filepath.Join(cacheParent, "guest-helper-cache"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(string(staged)))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "dsx-guest" {
		t.Fatalf("mounted helper directory disclosed siblings: %#v", entries)
	}
	got, err := os.ReadFile(string(staged))
	if err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("staged helper = %q, %v", got, err)
	}
	wantDigest := sha256.Sum256(contents)
	gotDigest := sha256.Sum256(got)
	if gotDigest != wantDigest {
		t.Fatalf("staged digest %x, want source digest %x", gotDigest, wantDigest)
	}
	if err := validateGuestHelperMountSource(staged); err != nil {
		t.Fatalf("staged helper mount validation failed: %v", err)
	}
}

func TestStageVerifiedGuestHelperRejectsTamperAndMissingDigest(t *testing.T) {
	sourceRoot := canonicalTemporaryDirectory(t)
	source := filepath.Join(sourceRoot, "dsx-guest")
	contents := []byte("release guest helper")
	if err := os.WriteFile(source, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	cacheRoot := filepath.Join(canonicalTemporaryDirectory(t), "guest-helper-cache")
	if _, err := StageVerifiedGuestHelper(runtime.HostPath(source), cacheRoot, fmt.Sprintf("%x", digest)); err != nil {
		t.Fatalf("verified helper rejected: %v", err)
	}
	if _, err := StageVerifiedGuestHelper(runtime.HostPath(source), cacheRoot, "unknown"); err == nil {
		t.Fatal("missing release digest was accepted")
	}
	if err := os.WriteFile(source, []byte("tampered"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := StageVerifiedGuestHelper(runtime.HostPath(source), cacheRoot, fmt.Sprintf("%x", digest)); err == nil {
		t.Fatal("tampered release helper was accepted")
	}
}

func TestGuestHelperMountValidationRejectsSiblingAndUnsafeMode(t *testing.T) {
	root := canonicalTemporaryDirectory(t)
	helper := filepath.Join(root, "dsx-guest")
	if err := os.WriteFile(helper, []byte("helper"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateGuestHelperMountSource(runtime.HostPath(helper)); err == nil {
		t.Fatal("helper directory with sibling secret was accepted")
	}
	if err := os.Remove(filepath.Join(root, "secret")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(helper, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validateGuestHelperMountSource(runtime.HostPath(helper)); err == nil {
		t.Fatal("world-executable helper mode was accepted")
	}
}

func TestGuestHelperCacheIsBounded(t *testing.T) {
	cacheParent := canonicalTemporaryDirectory(t)
	cacheRoot := filepath.Join(cacheParent, "guest-helper-cache")
	for index := range maxGuestHelperCacheEntries + 2 {
		sourceRoot := canonicalTemporaryDirectory(t)
		source := filepath.Join(sourceRoot, "dsx-guest")
		if err := os.WriteFile(source, []byte{byte(index + 1)}, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := StageGuestHelper(runtime.HostPath(source), cacheRoot); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	digests := 0
	lock := false
	for _, entry := range entries {
		switch {
		case entry.Name() == ".lock":
			lock = true
		case len(entry.Name()) == len("sha256-")+64 && entry.Name()[:len("sha256-")] == "sha256-":
			digests++
		default:
			t.Fatalf("unexpected helper cache entry %q", entry.Name())
		}
	}
	if !lock || digests != maxGuestHelperCacheEntries {
		t.Fatalf("helper cache lock=%t digest entries=%d, want %d", lock, digests, maxGuestHelperCacheEntries)
	}
}

func TestStageGuestHelperConcurrentCallsDoNotCrossDelete(t *testing.T) {
	cacheRoot := filepath.Join(canonicalTemporaryDirectory(t), "guest-helper-cache")
	const workers = 12
	sources := make([]runtime.HostPath, 0, workers)
	for index := range workers {
		root := canonicalTemporaryDirectory(t)
		source := filepath.Join(root, "dsx-guest")
		if err := os.WriteFile(source, []byte{byte(index + 1)}, 0o700); err != nil {
			t.Fatal(err)
		}
		sources = append(sources, runtime.HostPath(source))
	}
	start := make(chan struct{})
	failures := make(chan error, workers)
	var wait sync.WaitGroup
	for _, source := range sources {
		wait.Add(1)
		go func(source runtime.HostPath) {
			defer wait.Done()
			<-start
			_, err := StageGuestHelper(source, cacheRoot)
			failures <- err
		}(source)
	}
	close(start)
	wait.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatalf("concurrent helper staging failed: %v", err)
		}
	}
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	digests := 0
	for _, entry := range entries {
		if entry.Name() == ".lock" {
			if err := verifyOwnedMode(filepath.Join(cacheRoot, entry.Name()), false, 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if len(entry.Name()) != len("sha256-")+64 || entry.Name()[:len("sha256-")] != "sha256-" {
			t.Fatalf("concurrent cache has unexpected entry %q", entry.Name())
		}
		digests++
	}
	if digests != maxGuestHelperCacheEntries {
		t.Fatalf("concurrent digest cache entries = %d, want %d", digests, maxGuestHelperCacheEntries)
	}
}

func canonicalTemporaryDirectory(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}
