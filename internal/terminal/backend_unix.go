//go:build linux || darwin

package terminal

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"relaydock/internal/protocol"
)

func Supported() bool                { return true }
func SupportsShell(kind string) bool { return kind == "sh" }

type unixProcess struct {
	*os.File
	cmd  *exec.Cmd
	once sync.Once
}

func Start(executable, cwd, term string, size protocol.TerminalSize) (Process, error) {
	cmd := exec.Command(executable, "-i")
	cmd.Dir = cwd
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "TERM=") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "TERM="+term)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: size.Cols, Rows: size.Rows})
	if err != nil {
		return nil, err
	}
	// Rewrap a nonblocking descriptor so Go's poller can interrupt a blocked
	// read/write when the network drops, even if a child still holds the slave.
	fd, err := unix.Dup(int(f.Fd()))
	if err == nil {
		unix.CloseOnExec(fd)
		err = unix.SetNonblock(fd, true)
		if err != nil {
			unix.Close(fd)
		}
	}
	_ = f.Close()
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	return &unixProcess{File: os.NewFile(uintptr(fd), "terminal-pty"), cmd: cmd}, nil
}

func (p *unixProcess) Resize(s protocol.TerminalSize) error {
	if err := s.Validate(); err != nil {
		return err
	}
	raw, err := p.File.SyscallConn()
	if err != nil {
		return err
	}
	var resizeErr error
	err = raw.Control(func(fd uintptr) {
		resizeErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Col: s.Cols, Row: s.Rows})
	})
	return errors.Join(err, resizeErr)
}
func (p *unixProcess) Hangup() {
	p.once.Do(func() { _ = p.File.Close(); _ = p.cmd.Process.Signal(syscall.SIGHUP) })
}
func (p *unixProcess) Kill() { _ = p.cmd.Process.Kill() }
func (p *unixProcess) Wait() protocol.TerminalExit {
	err := p.cmd.Wait()
	if err == nil {
		return protocol.TerminalExit{Code: 0}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			code = 128 + int(status.Signal())
		}
		return protocol.TerminalExit{Code: int64(code)}
	}
	return protocol.TerminalExit{Code: -1, Error: err.Error()}
}
