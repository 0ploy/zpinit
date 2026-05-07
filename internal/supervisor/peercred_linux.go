//go:build linux

package supervisor

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
)

// authorizePeer enforces that the connecting peer's UID matches the
// daemon's effective UID. Reads SO_PEERCRED from the underlying socket
// FD: the kernel records peer creds at connect time, so a connection
// that slipped through the bind→chmod window is still recognised as
// the wrong UID.
func authorizePeer(conn net.Conn) error {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("not a unix conn")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var (
		ucred   *syscall.Ucred
		sockerr error
	)
	if cerr := raw.Control(func(fd uintptr) {
		ucred, sockerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); cerr != nil {
		return cerr
	}
	if sockerr != nil {
		return sockerr
	}
	self := uint32(os.Geteuid())
	if ucred.Uid != self {
		return fmt.Errorf("peer uid %d != daemon uid %d (pid %d)", ucred.Uid, self, ucred.Pid)
	}
	return nil
}
