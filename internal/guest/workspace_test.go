package guest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitializeOwnedDirectoryUsesDescriptorAndExactOwnership(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "lost+found"), 0o700); err != nil {
		t.Fatal(err)
	}
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	if uid == 0 || gid == 0 {
		t.Skip("test requires a non-root UID and GID")
	}
	if err := initializeOwnedDirectory(root, uid, gid); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("workspace mode = %o, want 0700", info.Mode().Perm())
	}
	if _, err := os.Lstat(filepath.Join(root, "lost+found")); !os.IsNotExist(err) {
		t.Fatalf("volume metadata directory remains: %v", err)
	}

	link := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := initializeOwnedDirectory(link, uid, gid); err == nil {
		t.Fatal("symlink workspace was accepted")
	}
}

func TestInitializeOwnedWorkspacesRejectsIncompletePathsAndRootIdentity(t *testing.T) {
	if err := InitializeOwnedWorkspaces([]string{"/tmp/workspace"}, 501, 20); err == nil {
		t.Fatal("non-authorized workspace path was accepted")
	}
	if err := InitializeOwnedWorkspaces(append([]string(nil), OwnedWorkspacePaths...), 0, 20); err == nil {
		t.Fatal("root child UID was accepted")
	}
	reordered := append([]string(nil), OwnedWorkspacePaths...)
	reordered[0], reordered[1] = reordered[1], reordered[0]
	if err := InitializeOwnedWorkspaces(reordered, 501, 20); err == nil {
		t.Fatal("reordered path allowlist was accepted")
	}
}
