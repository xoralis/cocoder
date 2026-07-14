//go:build windows

package execx

import (
	"io"
	"os/exec"
	"strconv"
	"syscall"
)

// setSysProcAttr detaches the child from our console Ctrl-C group so that we
// control shutdown ordering (cancel -> kill tree -> flush state) instead of
// the console broadcasting SIGINT into the child first.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killTree kills the whole process tree. npm-installed CLIs are
// cmd.exe -> node chains, so /T (tree) is mandatory.
// TODO: upgrade to a Job Object (JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE) if
// taskkill proves leaky; the Proc interface already hides the mechanism.
func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	kill.Stdout, kill.Stderr = io.Discard, io.Discard
	if err := kill.Run(); err != nil {
		return cmd.Process.Kill() // fallback: at least kill the direct child
	}
	return nil
}
