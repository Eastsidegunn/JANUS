//go:build darwin

package approvalrelay

import (
	"golang.org/x/sys/unix"
	"net"
	"syscall"
)

func peerUID(c net.Conn) (int, error) {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return -1, unix.EINVAL
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return -1, err
	}
	var uid int = -1
	err = raw.Control(func(fd uintptr) {
		u, e := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if e == nil {
			uid = int(u.Uid)
		}
	})
	return uid, err
}
