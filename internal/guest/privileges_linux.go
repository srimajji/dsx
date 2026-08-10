//go:build linux

package guest

import (
	goruntime "runtime"

	"golang.org/x/sys/unix"
)

func lockProcessPrivileges() error {
	return unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0)
}

func enableChildSubreaper() error {
	return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}

func startWithoutPrivilegeGains(start func() error) error {
	goruntime.LockOSThread()
	defer goruntime.UnlockOSThread()
	if err := lockProcessPrivileges(); err != nil {
		return err
	}
	return start()
}
