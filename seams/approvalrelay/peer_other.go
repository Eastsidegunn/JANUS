//go:build !linux && !darwin

package approvalrelay

import (
	"errors"
	"net"
)

var errPeerCredentialsUnavailable = errors.New("peer credentials unavailable")

// Peer credential APIs are platform-specific and unavailable in the stdlib on
// this target; fail closed until a reviewed native implementation is added.
func peerUID(net.Conn) (int, error) { return -1, errPeerCredentialsUnavailable }
