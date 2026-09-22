package terminal

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"relaydock/internal/config"
	"relaydock/internal/protocol"
	"relaydock/internal/transport"
)

func ResolveCWD(c config.Config, cwd string) (string, error) {
	if p, ok := c.Workspaces[cwd]; ok {
		cwd = p
	}
	if !filepath.IsAbs(cwd) {
		return "", errors.New("cwd must be a workspace alias or absolute directory")
	}
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", errors.New("cwd is not a directory")
	}
	return real, nil
}

// Serve owns one PTY and stream. Disconnects hang up the outer shell; only an
// explicit new CLI invocation can create another one. No output is spooled.
func Serve(parent context.Context, peer *transport.Peer, executable, cwd string, open protocol.TerminalOpen) {
	fail := func(err error) {
		_ = peer.Send(protocol.Wrap("shell_exit", "", protocol.TerminalExit{Code: -1, Error: err.Error()}))
	}
	if err := open.Validate(); err != nil {
		fail(err)
		return
	}
	if err := parent.Err(); err != nil {
		return
	}
	p, err := Start(executable, cwd, open.Term, open.TerminalSize)
	if err != nil {
		fail(err)
		return
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { peer.Close(); p.Hangup() })
	defer stop()
	exited := make(chan struct{})
	var result protocol.TerminalExit
	go func() { result = p.Wait(); close(exited) }()
	var wg sync.WaitGroup
	defer func() {
		cancel()
		peer.Close()
		p.Hangup()
		select {
		case <-exited:
		case <-time.After(time.Second):
			p.Kill()
			<-exited
		}
		wg.Wait()
	}()
	if err = peer.Send(protocol.Wrap("shell_ready", "", nil)); err != nil {
		return
	}
	outputDone := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer close(outputDone)
		buf := make([]byte, protocol.TerminalChunk)
		for {
			n, err := p.Read(buf)
			if n > 0 && peer.Send(protocol.Wrap("shell_output", "", protocol.TerminalData{Bytes: buf[:n]})) != nil {
				cancel()
				return
			}
			if err != nil {
				return
			} // EOF/EIO is normal when the slave closes.
		}
	}()
	go func() {
		defer wg.Done()
		for {
			m, err := peer.Read()
			if err != nil || protocol.TerminalInput(m) != nil {
				cancel()
				return
			}
			if m.Type == "shell_resize" {
				s, _ := protocol.Decode[protocol.TerminalSize](m)
				err = p.Resize(s)
			} else {
				d, _ := protocol.Decode[protocol.TerminalData](m)
				var n int
				n, err = p.Write(d.Bytes)
				if err == nil && n != len(d.Bytes) {
					err = io.ErrShortWrite
				}
			}
			if err != nil {
				cancel()
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
		return
	case <-exited:
	}
	// Drain final output before the exit frame, but don't wait forever on
	// inherited slave descriptors owned by background programs.
	select {
	case <-outputDone:
	case <-ctx.Done():
		return
	case <-time.After(time.Second):
		p.Hangup()
		<-outputDone
	}
	_ = peer.Send(protocol.Wrap("shell_exit", "", result))
}
