//go:build !linux

package wameow

import "syscall"

func tcpUserTimeout(_, _ string, _ syscall.RawConn) error { return nil }
