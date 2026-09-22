//go:build linux || darwin

package terminal

import (
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// Darwin cannot register /dev/tty with Go's kqueue poller. Poll the independent,
// nonblocking descriptor with a wakeup pipe so Close interrupts an idle read
// without changing flags on the caller's stdin/stdout or leaking a goroutine.
type consoleInput struct {
	fd, wakeRead, wakeWrite int
	stopped                 chan struct{}
	mu                      sync.Mutex
	once                    sync.Once
}

func openConsoleInput() (*consoleInput, error) {
	fd, err := unix.Open("/dev/tty", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var pipe [2]int
	if err = unix.Pipe(pipe[:]); err != nil {
		unix.Close(fd)
		return nil, err
	}
	unix.CloseOnExec(pipe[0])
	unix.CloseOnExec(pipe[1])
	return &consoleInput{fd: fd, wakeRead: pipe[0], wakeWrite: pipe[1], stopped: make(chan struct{})}, nil
}

func (r *consoleInput) Read(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		select {
		case <-r.stopped:
			return 0, os.ErrClosed
		default:
		}
		fds := []unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN}, {Fd: int32(r.wakeRead), Events: unix.POLLIN}}
		if _, err := unix.Poll(fds, -1); err != nil {
			if err == unix.EINTR {
				continue
			}
			return 0, err
		}
		if fds[1].Revents != 0 {
			return 0, os.ErrClosed
		}
		n, err := unix.Read(r.fd, b)
		if err == unix.EAGAIN || err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, io.EOF
		}
		return n, nil
	}
}

func (r *consoleInput) Close() error {
	r.once.Do(func() {
		close(r.stopped)
		for {
			if _, err := unix.Write(r.wakeWrite, []byte{1}); err != unix.EINTR {
				break
			}
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		unix.Close(r.fd)
		unix.Close(r.wakeRead)
		unix.Close(r.wakeWrite)
	})
	return nil
}
