package guest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srimajji/dsx/internal/harness"
	"github.com/srimajji/dsx/internal/model"
	"golang.org/x/sys/unix"
)

func TestEnsureRunDirectoryAndStageRunFileArePrivateAndChildReadable(t *testing.T) {
	runID, err := model.NewRunID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	runRoot := filepath.Join(guestTemporaryRootPath, "dsx-run", string(runID))
	t.Cleanup(func() { _ = os.RemoveAll(runRoot) })
	ensureTestHarnessRoot(t)
	directory := filepath.ToSlash(filepath.Join("/tmp/dsx-run", string(runID), "auth", "provider"))
	if err := EnsureRunDirectory(directory); err != nil {
		t.Fatal(err)
	}
	for current := filepath.Join(guestTemporaryRootPath, "dsx-run"); current != filepath.Join(guestTemporaryRootPath, "dsx-run", string(runID), "auth", "provider"); {
		info, statErr := os.Lstat(current)
		if statErr != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("private directory %q = %v, %v", current, info, statErr)
		}
		switch filepath.Base(current) {
		case "dsx-run":
			current = filepath.Join(current, string(runID))
		case string(runID):
			current = filepath.Join(current, "auth")
		case "auth":
			current = filepath.Join(current, "provider")
		}
	}
	credential := directory + "/credential.json"
	contents := []byte(`{"token":"child-readable"}`)
	if err := StageRunFile(credential, bytes.NewReader(contents), 1<<20); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(guestTemporaryRootPath, "dsx-run", string(runID), "auth", "provider", "credential.json")
	got, err := os.ReadFile(staged)
	if err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("child read staged credential = %q, %v", got, err)
	}
	info, err := os.Lstat(staged)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("staged credential metadata = %v, %v", info, err)
	}
	if err := StageRunFile(credential, bytes.NewReader(contents), 1<<20); err == nil {
		t.Fatal("exclusive staging overwrote an existing credential")
	}
}

func TestDescriptorDirectoryWalkRejectsSymlinkWrongOwnerAndPermissiveAncestor(t *testing.T) {
	root := t.TempDir()
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(rootFD)
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())

	t.Run("symlink", func(t *testing.T) {
		target := filepath.Join(root, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
			t.Fatal(err)
		}
		if _, err := openDirectoryChain(rootFD, []string{"linked"}, false, uid, gid); err == nil {
			t.Fatal("descriptor walk followed a symlink")
		}
	})

	t.Run("mode-0777", func(t *testing.T) {
		name := filepath.Join(root, "permissive")
		if err := os.Mkdir(name, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(name, 0o777); err != nil {
			t.Fatal(err)
		}
		if _, err := openDirectoryChain(rootFD, []string{"permissive"}, false, uid, gid); err == nil {
			t.Fatal("descriptor walk accepted a 0777 ancestor")
		}
	})

	wrongUID := uid + 1
	if wrongUID == 0 {
		wrongUID = uid - 1
	}
	metadata := unix.Stat_t{Mode: unix.S_IFDIR | 0o700, Uid: wrongUID, Gid: gid}
	if err := validatePrivateDirectory(metadata, uid, gid); err == nil {
		t.Fatal("directory validator accepted a wrong owner")
	}
}

func TestDescriptorChainDetectsParentSwap(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"dsx-run", "dsx-run/run", "dsx-run/run/auth"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(rootFD)
	chain, err := openDirectoryChain(rootFD, []string{"dsx-run", "run", "auth"}, false, uint32(os.Geteuid()), uint32(os.Getegid()))
	if err != nil {
		t.Fatal(err)
	}
	defer chain.close()
	original := filepath.Join(root, "dsx-run", "run")
	moved := filepath.Join(root, "dsx-run", "run-old")
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := chain.revalidate(rootFD); err == nil {
		t.Fatal("descriptor chain accepted a swapped parent")
	}
}

func TestStageRunFileRejectsUnauthorizedAndOversizedInput(t *testing.T) {
	for _, name := range []string{
		"/tmp/dsx-run/00000000-0000-7000-8000-000000000000/data/secret",
		"/tmp/dsx-run/00000000-0000-7000-8000-000000000000/auth/../secret",
		"/tmp/dsx-run/00000000-0000-4000-8000-000000000000/auth/secret",
	} {
		if err := StageRunFile(name, bytes.NewReader(nil), 1<<20); err == nil {
			t.Fatalf("accepted unauthorized path %q", name)
		}
	}
	name := "/tmp/dsx-run/00000000-0000-7000-8000-000000000000/auth/secret"
	if err := StageRunFile(name, bytes.NewReader(make([]byte, harness.MaxAuthArtifactBytes+1)), harness.MaxAuthArtifactBytes); err == nil {
		t.Fatal("accepted oversized staged file")
	}
}

func TestReadOnlyConfigPathAndRootOwnershipContracts(t *testing.T) {
	name := "/tmp/dsx-readonly/00000000-0000-7000-8000-000000000000/provider/settings.json"
	components, err := authorizedReadOnlyRunComponents(name)
	if err != nil || len(components) != 4 {
		t.Fatalf("authorized read-only components = %#v, %v", components, err)
	}
	for _, invalid := range []string{
		"/tmp/dsx-run/00000000-0000-7000-8000-000000000000/config/settings.json",
		"/tmp/dsx-readonly/00000000-0000-7000-8000-000000000000/../settings.json",
	} {
		if _, err := authorizedReadOnlyRunComponents(invalid); err == nil {
			t.Fatalf("accepted unsafe read-only path %q", invalid)
		}
	}
	root, err := authorizedReadOnlyRootComponents("/tmp/dsx-readonly/00000000-0000-7000-8000-000000000000")
	if err != nil || len(root) != 2 {
		t.Fatalf("authorized cleanup root = %#v, %v", root, err)
	}
	if _, err := authorizedReadOnlyRootComponents("/tmp/dsx-readonly/00000000-0000-7000-8000-000000000000/provider"); err == nil {
		t.Fatal("nested read-only cleanup root was accepted")
	}
	rootDirectory := unix.Stat_t{Mode: unix.S_IFDIR | 0o555, Uid: 0, Gid: 0}
	if err := validateOwnedDirectory(rootDirectory, 0, 0, 0o555); err != nil {
		t.Fatalf("root-owned read-only directory rejected: %v", err)
	}
	rootFile := unix.Stat_t{Mode: unix.S_IFREG | 0o444, Uid: 0, Gid: 0, Size: 7}
	if err := validateOwnedFile(rootFile, 0, 0, 0o444, 7); err != nil {
		t.Fatalf("root-owned read-only file rejected: %v", err)
	}
	rootFile.Mode = unix.S_IFREG | 0o644
	if err := validateOwnedFile(rootFile, 0, 0, 0o444, 7); err == nil {
		t.Fatal("writable read-only config mode was accepted")
	}
}

func ensureTestHarnessRoot(t *testing.T) {
	t.Helper()
	root := filepath.Join(guestTemporaryRootPath, "dsx-run")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(root, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
}
