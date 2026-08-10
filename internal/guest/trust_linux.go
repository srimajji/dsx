//go:build linux

package guest

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const InstalledExecutable = "/usr/local/libexec/dsx/dsx-guest"
const installedDirectory = "/usr/local/libexec/dsx"

func VerifyInstalledExecutable() error {
	resolved, err := filepath.EvalSymlinks(InstalledExecutable)
	if err != nil {
		return fmt.Errorf("resolve installed helper: %w", err)
	}
	if resolved != InstalledExecutable {
		return errors.New("installed helper path contains a symlink")
	}
	for _, directory := range []string{"/", "/usr", "/usr/local", "/usr/local/libexec"} {
		info, err := os.Lstat(directory)
		if err != nil {
			return fmt.Errorf("inspect helper ancestor %s: %w", directory, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("helper ancestor %s is not root-owned and non-writable", directory)
		}
	}
	directory, err := os.Lstat(installedDirectory)
	if err != nil || !directory.IsDir() || directory.Mode()&os.ModeSymlink != 0 {
		return errors.New("installed helper directory is not a real directory")
	}
	if !readOnlyMount(installedDirectory) {
		return errors.New("installed helper directory is not an exact read-only mount")
	}
	executable, err := os.Lstat(InstalledExecutable)
	if err != nil || !executable.Mode().IsRegular() || executable.Mode().Perm()&0o111 == 0 {
		return errors.New("installed helper is not an executable regular file")
	}
	return nil
}

func readOnlyMount(target string) bool {
	mounts, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer mounts.Close()
	scanner := bufio.NewScanner(mounts)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 || fields[4] != target {
			continue
		}
		for _, option := range strings.Split(fields[5], ",") {
			if option == "ro" {
				return true
			}
		}
		return false
	}
	return false
}
