package guest

import (
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var OwnedWorkspacePaths = []string{
	"/workspace",
	"/home/dsx/.dsx/auth",
	"/home/dsx/.local/state/dsx",
	"/home/dsx/.cache",
	"/var/lib/dsx",
}

// InitializeOwnedWorkspaces transfers only the exact DSX-owned volume roots to
// the non-root child identity. It never traverses persistent contents.
func InitializeOwnedWorkspaces(paths []string, uid, gid uint32) error {
	if uid == 0 || gid == 0 {
		return errors.New("owned workspaces require a non-root UID and GID")
	}
	if len(paths) != len(OwnedWorkspacePaths) {
		return errors.New("owned workspace path set is incomplete")
	}
	for index, expected := range OwnedWorkspacePaths {
		if paths[index] != expected {
			return errors.New("owned workspace paths must match the ordered allowlist")
		}
	}
	for _, path := range paths {
		if err := initializeOwnedDirectory(path, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

func initializeOwnedDirectory(path string, uid, gid uint32) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("owned workspace path must be clean and absolute")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open owned workspace %q: %w", path, err)
	}
	defer unix.Close(fd)
	if err := unix.Unlinkat(fd, "lost+found", unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove empty volume metadata directory from %q: %w", path, err)
	}
	if err := unix.Fchown(fd, int(uid), int(gid)); err != nil {
		return fmt.Errorf("set owned workspace identity on %q: %w", path, err)
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		return fmt.Errorf("set owned workspace permissions on %q: %w", path, err)
	}
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return fmt.Errorf("verify owned workspace %q: %w", path, err)
	}
	if info.Mode&unix.S_IFMT != unix.S_IFDIR || info.Uid != uid || info.Gid != gid || info.Mode&0o777 != 0o700 {
		return fmt.Errorf("owned workspace %q identity or permissions do not match", path)
	}
	return nil
}
