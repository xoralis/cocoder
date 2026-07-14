//go:build !windows

package execx

import (
	"os/exec"
	"syscall"
	"time"
)

// setSysProcAttr puts the child in its own process group so the whole tree
// can be signalled at once.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killTree sends SIGTERM to the child's process group, escalating to SIGKILL
// after a grace period. A stray SIGKILL to an already-dead pgid is harmless
// (ESRCH).
func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return cmd.Process.Kill()
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		return cmd.Process.Kill()
	}
	go func() {
		time.Sleep(5 * time.Second)
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}()
	return nil
}
