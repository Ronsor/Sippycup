//go:build unix

package libsippycup

import (
	"os/exec"
	"syscall"
)

// configureChildProcess terminates the child's process group on cancellation.
func configureChildProcess(cmd *exec.Cmd, control string) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}

		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	return nil
}
