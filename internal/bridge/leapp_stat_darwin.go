//go:build darwin

package bridge

import "golang.org/x/sys/unix"

func sameLeappDirectoryVersion(before, after unix.Stat_t) bool {
	return before.Dev == after.Dev &&
		before.Ino == after.Ino &&
		before.Mode == after.Mode &&
		before.Uid == after.Uid &&
		before.Gid == after.Gid &&
		before.Nlink == after.Nlink &&
		before.Size == after.Size &&
		before.Mtim == after.Mtim &&
		before.Ctim == after.Ctim
}
