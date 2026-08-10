//go:build linux

package guest

import (
	"net"

	"golang.org/x/sys/unix"
)

func peerCredentials(connection *net.UnixConn) (uint32, uint32, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var credential *unix.Ucred
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		credential, controlErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, 0, err
	}
	if controlErr != nil {
		return 0, 0, controlErr
	}
	return credential.Uid, credential.Gid, nil
}
