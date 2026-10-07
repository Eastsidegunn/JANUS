//go:build !linux

package collector

// opaqueDirectory is unavailable on non-Linux hosts; Linux integration is the
// authoritative native-overlay gate. Portable unit tests use the .wh..wh..opq
// marker representation.
func opaqueDirectory(string) bool { return false }
