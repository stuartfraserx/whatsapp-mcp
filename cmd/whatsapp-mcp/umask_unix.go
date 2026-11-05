//go:build unix

package main

import "syscall"

func restrictUmask() { syscall.Umask(0o077) }
