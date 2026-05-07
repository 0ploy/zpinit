//go:build !linux

package supervisor

import "net"

// authorizePeer is a no-op on non-Linux. zpinit is production-Linux
// only; macOS dev builds skip the SO_PEERCRED check (the syscall
// shape differs across BSDs and the path isn't exercised in
// production).
func authorizePeer(conn net.Conn) error { return nil }
