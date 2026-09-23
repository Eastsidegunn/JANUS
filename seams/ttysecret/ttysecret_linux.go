//go:build linux

package ttysecret

import "golang.org/x/sys/unix"

// Terminal attribute ioctls differ by OS; on Linux they are TCGETS/TCSETS.
const (
	ioctlReadTermios  = unix.TCGETS
	ioctlWriteTermios = unix.TCSETS
)
