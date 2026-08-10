package guest

import (
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const ownedWorkspacePath = "/workspace"

// InitializeOwnedWorkspace transfers only the DSX-owned clone volume root to
// the non-root child identity. The Apple adapter permits this startup mode only
// when /workspace is a volume mount, never a host bind.
func InitializeOwnedWorkspace(path string, uid, gid uint32) error {
	if path != ownedWorkspacePath {
		return errors.New("owned workspace initialization is restricted to /workspace")
	}
	if uid == 0 || gid == 0 {
		return errors.New("owned workspace requires a non-root UID and GID")
	}
	return initializeOwnedDirectory(path, uid, gid)
}

func initializeOwnedDirectory(path string, uid, gid uint32) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("owned workspace path must be clean and absolute")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open owned workspace: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Unlinkat(fd, "lost+found", unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove empty volume metadata directory: %w", err)
	}
	if err := unix.Fchown(fd, int(uid), int(gid)); err != nil {
		return fmt.Errorf("set owned workspace identity: %w", err)
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		return fmt.Errorf("set owned workspace permissions: %w", err)
	}
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return fmt.Errorf("verify owned workspace: %w", err)
	}
	if info.Mode&unix.S_IFMT != unix.S_IFDIR || info.Uid != uid || info.Gid != gid || info.Mode&0o777 != 0o700 {
		return errors.New("owned workspace identity or permissions do not match")
	}
	return nil
}
