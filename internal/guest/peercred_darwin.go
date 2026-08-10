//go:build darwin

package guest

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

func peerCredentials(connection *net.UnixConn) (uint32, uint32, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var credential *unix.Xucred
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		credential, controlErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, 0, err
	}
	if controlErr != nil {
		return 0, 0, controlErr
	}
	if credential == nil || credential.Ngroups < 1 {
		return 0, 0, errors.New("peer credentials contain no group")
	}
	return credential.Uid, uint32(credential.Groups[0]), nil
}
