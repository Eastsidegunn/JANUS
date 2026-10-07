//go:build linux

package collector

import (
	"os"
	"syscall"
	"testing"
	"time"
)

type fakeWhiteoutInfo struct {
	mode os.FileMode
	stat syscall.Stat_t
}

func (f fakeWhiteoutInfo) Name() string       { return "whiteout" }
func (f fakeWhiteoutInfo) Size() int64        { return 0 }
func (f fakeWhiteoutInfo) Mode() os.FileMode  { return f.mode }
func (f fakeWhiteoutInfo) ModTime() time.Time { return time.Time{} }
func (f fakeWhiteoutInfo) IsDir() bool        { return false }
func (f fakeWhiteoutInfo) Sys() any           { return &f.stat }

func TestIsWhiteoutIgnoresOwnerAndRequiresZeroRdev(t *testing.T) {
	foreignUID := uint32(os.Getuid()) + 1
	if foreignUID == uint32(os.Getuid()) {
		foreignUID = ^uint32(0)
	}
	charDevice := os.ModeCharDevice

	if !isWhiteout(fakeWhiteoutInfo{mode: charDevice, stat: syscall.Stat_t{Uid: foreignUID, Rdev: 0}}) {
		t.Fatal("char device with zero rdev was not recognized regardless of owner")
	}
	if isWhiteout(fakeWhiteoutInfo{mode: charDevice, stat: syscall.Stat_t{Uid: foreignUID, Rdev: 1}}) {
		t.Fatal("char device with non-zero rdev was recognized as whiteout")
	}
	if isWhiteout(fakeWhiteoutInfo{mode: 0, stat: syscall.Stat_t{Uid: foreignUID, Rdev: 0}}) {
		t.Fatal("regular file was recognized as whiteout")
	}
}
