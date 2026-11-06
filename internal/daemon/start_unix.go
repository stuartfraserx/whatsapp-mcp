//go:build unix

package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Start validates an executable and launches it in a detached session.
func Start(args []string) error {
	if len(args) == 0 || !filepath.IsAbs(args[0]) {
		return errors.New("daemon start command must be absolute")
	}
	info, err := os.Lstat(args[0])
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	uid := os.Getuid()
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Mode().Perm()&0100 == 0 || stat.Uid != uint32(uid) {
		return errors.New("daemon executable must be owned by current user and not group/world writable")
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer null.Close()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = null
	cmd.Stdout = null
	cmd.Stderr = null
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
