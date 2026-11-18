//go:build unix

package hook

import (
	"os/exec"
	"syscall"
)

// setGroup runs the hook in a process group of its own, and kills the whole
// group at the deadline: a hook's children must not outlive it.
func setGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
