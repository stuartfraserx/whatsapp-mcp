//go:build linux

package wameow

import (
	"golang.org/x/sys/unix"
	"syscall"
)

func tcpUserTimeout(_, _ string, conn syscall.RawConn) error {
	// Best effort: TCP keepalive and application pings still work on kernels
	// that do not support TCP_USER_TIMEOUT.
	return conn.Control(func(fd uintptr) {
		_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, 30000)
	})
}
