package terminal

import (
	"io"
	"os"
	"runtime"
	"sync"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

var cancelConsoleRead = windows.NewLazySystemDLL("kernel32.dll").NewProc("CancelSynchronousIo")

type consoleRead struct {
	bytes []byte
	err   error
}

// ReadConsoleW avoids legacy console code-page conversions and their loss of
// non-ASCII input. A dedicated OS thread lets Close cancel only our own read,
// without closing (or cancelling other users of) the caller's input handle.
type consoleInput struct {
	thread     windows.Handle
	reads      chan consoleRead
	stopped    chan struct{}
	done       chan struct{}
	mu         sync.Mutex
	control    sync.Mutex
	once       sync.Once
	pending    []byte
	pendingErr error
}

func openConsoleInput(handle windows.Handle) (*consoleInput, error) {
	if err := cancelConsoleRead.Find(); err != nil {
		return nil, err
	}
	r := &consoleInput{reads: make(chan consoleRead, 1), stopped: make(chan struct{}), done: make(chan struct{})}
	ready := make(chan error, 1)
	go r.run(handle, ready)
	if err := <-ready; err != nil {
		<-r.done
		return nil, err
	}
	return r, nil
}

func (r *consoleInput) run(handle windows.Handle, ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer func() {
		// Publish completion before this thread can return to Go's pool, and
		// serialize it with cancellation so no later I/O is cancelled.
		r.control.Lock()
		close(r.reads)
		close(r.done)
		r.control.Unlock()
	}()
	var err error
	r.thread, err = windows.OpenThread(windows.THREAD_TERMINATE, false, windows.GetCurrentThreadId())
	ready <- err
	if err != nil {
		return
	}
	var buf [2048]uint16
	var high uint16
	for {
		select {
		case <-r.stopped:
			return
		default:
		}
		var count uint32
		err = windows.ReadConsole(handle, &buf[0], uint32(len(buf)), &count, nil)
		chars := buf[:count]
		if high != 0 {
			chars = append([]uint16{high}, chars...)
			high = 0
		}
		if len(chars) > 0 && chars[len(chars)-1] >= 0xd800 && chars[len(chars)-1] <= 0xdbff && err == nil {
			high = chars[len(chars)-1]
			chars = chars[:len(chars)-1]
		}
		// Do not use UTF16ToString: it truncates at Ctrl+Space/NUL.
		data := []byte(string(utf16.Decode(chars)))
		if len(data) != 0 || err != nil {
			select {
			case r.reads <- consoleRead{data, err}:
			case <-r.stopped:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *consoleInput) Read(buf []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-r.stopped:
		return 0, os.ErrClosed
	default:
	}
	if len(buf) == 0 {
		return 0, nil
	}
	if len(r.pending) != 0 {
		n := copy(buf, r.pending)
		r.pending = r.pending[n:]
		if len(r.pending) == 0 {
			err := r.pendingErr
			r.pendingErr = nil
			return n, err
		}
		return n, nil
	}
	select {
	case <-r.stopped:
		return 0, os.ErrClosed
	case result, ok := <-r.reads:
		select {
		case <-r.stopped:
			return 0, os.ErrClosed
		default:
		}
		if !ok {
			return 0, io.EOF
		}
		n := copy(buf, result.bytes)
		r.pending = result.bytes[n:]
		if len(r.pending) != 0 {
			r.pendingErr = result.err
			return n, nil
		}
		return n, result.err
	}
}

func (r *consoleInput) Close() error {
	r.once.Do(func() {
		close(r.stopped)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			r.control.Lock()
			select {
			case <-r.done:
				r.control.Unlock()
				windows.CloseHandle(r.thread)
				return
			default:
				_, _, _ = cancelConsoleRead.Call(uintptr(r.thread))
			}
			r.control.Unlock()
			// A one-shot cancel races with entering ReadConsole. Retry until
			// the reading thread acknowledges shutdown, including that race.
			select {
			case <-r.done:
			case <-ticker.C:
			}
		}
	})
	return nil
}
