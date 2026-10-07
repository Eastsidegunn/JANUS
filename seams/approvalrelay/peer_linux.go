//go:build linux

package approvalrelay

import (
	"net"
	"syscall"
)

var errPeerCredentialsUnavailable = syscall.ENOTSUP

func peerUID(c net.Conn) (int, error) {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return -1, syscall.EINVAL
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return -1, err
	}
	var uid int = -1
	var inner error
	err = raw.Control(func(fd uintptr) {
		u, e := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if e != nil {
			inner = e
		} else {
			uid = int(u.Uid)
		}
	})
	if err != nil {
		return -1, err
	}
	return uid, inner
}
