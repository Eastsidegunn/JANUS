//go:build linux

package collector

import (
	"syscall"
)

// opaqueDirectory recognizes native overlayfs opaque-directory xattrs without
// following links. Both kernel spellings are accepted because rootless setups
// may expose trusted or user namespace xattrs.
func opaqueDirectory(path string) bool {
	size, err := syscall.Listxattr(path, nil)
	if err != nil || size <= 0 {
		return false
	}
	buf := make([]byte, size)
	n, err := syscall.Listxattr(path, buf)
	if err != nil {
		return false
	}
	for _, raw := range splitNUL(buf[:n]) {
		if raw != "trusted.overlay.opaque" && raw != "user.overlay.opaque" {
			continue
		}
		value := make([]byte, 8)
		vn, err := syscall.Getxattr(path, raw, value)
		if err == nil && string(value[:vn]) == "y" {
			return true
		}
	}
	return false
}

func splitNUL(buf []byte) []string {
	var out []string
	start := 0
	for i, b := range buf {
		if b == 0 {
			if i > start {
				out = append(out, string(buf[start:i]))
			}
			start = i + 1
		}
	}
	return out
}
