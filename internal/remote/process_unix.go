//go:build !windows

package remote

import (
	"errors"
	"os/exec"
	"syscall"
)

type processTree struct{ cmd *exec.Cmd }

func startProcess(cmd *exec.Cmd) (*processTree, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &processTree{cmd: cmd}, nil
}
func (t *processTree) kill() error {
	err := syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
func (t *processTree) close() { _ = t.kill() }
