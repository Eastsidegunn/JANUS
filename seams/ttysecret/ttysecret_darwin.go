//go:build darwin

package ttysecret

import "golang.org/x/sys/unix"

// Terminal attribute ioctls differ by OS; on darwin they are TIOCGETA/TIOCSETA.
const (
	ioctlReadTermios  = unix.TIOCGETA
	ioctlWriteTermios = unix.TIOCSETA
)
