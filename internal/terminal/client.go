package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/term"
	"relaydock/internal/config"
	"relaydock/internal/protocol"
	"relaydock/internal/transport"
)

type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("remote shell exited with status %d", e.Code) }

// Local console setup is a second, small platform boundary for Windows work.
type console struct {
	input   io.ReadCloser
	output  io.Writer
	size    func() (protocol.TerminalSize, error)
	resized <-chan os.Signal
	restore func() error
}

func Run(parent context.Context, c config.Config, open protocol.TerminalOpen, in, out *os.File) (err error) {
	if !Supported() {
		return ErrUnsupported
	}
	if !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) {
		return errors.New("shell requires a local terminal on stdin and stdout")
	}
	cols, rows, err := term.GetSize(int(out.Fd()))
	if err != nil {
		return err
	}
	open.PeerID = c.ID
	open.TerminalSize = protocol.TerminalSize{Cols: uint16(cols), Rows: uint16(rows)}
	open.Term = os.Getenv("TERM")
	if open.Term == "" {
		open.Term = "xterm-256color"
	}
	if err = open.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	p, err := transport.Connect(ctx, c, "shell")
	if err != nil {
		return err
	}
	defer p.Close()
	stop := context.AfterFunc(ctx, p.Close)
	defer stop()
	_ = p.C.SetReadDeadline(time.Now().Add(20 * time.Second))
	if err = p.Send(protocol.Wrap("shell_open", "", open)); err != nil {
		return err
	}
	m, err := p.Read()
	if err != nil {
		return fmt.Errorf("terminal startup: %w", err)
	}
	if err = protocol.TerminalOutput(m); err != nil {
		return err
	}
	if m.Type == "shell_exit" {
		return exitResult(m)
	}
	if m.Type != "shell_ready" {
		return errors.New("expected terminal readiness")
	}
	con, err := prepareConsole(in, out)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	defer func() {
		cancel()
		p.Close()
		con.input.Close()
		wg.Wait()
		err = errors.Join(err, con.restore())
	}()
	go p.KeepAlive(ctx)
	localResult := make(chan error, 1)
	wg.Add(2)
	go func() {
		defer wg.Done()
		fail := func(e error) { localResult <- e; p.Close() }
		buf := make([]byte, protocol.TerminalChunk)
		escape := escapeFilter{lineStart: true}
		for {
			n, e := con.input.Read(buf)
			if n > 0 {
				data, quit := escape.feed(buf[:n])
				if quit {
					fail(nil)
					return
				}
				if len(data) > 0 {
					// A pending '~' can expand this chunk by one byte.
					for len(data) > 0 {
						size := min(len(data), protocol.TerminalChunk)
						if e = p.Send(protocol.Wrap("shell_input", "", protocol.TerminalData{Bytes: data[:size]})); e != nil {
							fail(e)
							return
						}
						data = data[size:]
					}
				}
			}
			if e != nil {
				fail(e)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-con.resized:
				s, e := con.size()
				if e == nil && s.Validate() == nil {
					if p.Send(protocol.Wrap("shell_resize", "", s)) != nil {
						p.Close()
						return
					}
				}
			}
		}
	}()
	// Capture a resize that occurred during the startup handshake.
	if size, e := con.size(); e == nil && size.Validate() == nil {
		if err = p.Send(protocol.Wrap("shell_resize", "", size)); err != nil {
			return err
		}
	}
	for {
		m, err = p.Read()
		if err != nil {
			if parent.Err() != nil {
				return parent.Err()
			}
			select {
			case e := <-localResult:
				return e
			default:
			}
			return fmt.Errorf("terminal disconnected (not reconnected): %w", err)
		}
		if err = protocol.TerminalOutput(m); err != nil {
			return err
		}
		switch m.Type {
		case "shell_output":
			b, _ := protocol.Decode[protocol.TerminalData](m)
			n, e := con.output.Write(b.Bytes)
			if e != nil {
				return e
			}
			if n != len(b.Bytes) {
				return io.ErrShortWrite
			}
		case "shell_exit":
			return exitResult(m)
		default:
			return errors.New("unexpected terminal readiness frame")
		}
	}
}

func exitResult(m protocol.Message) error {
	e, err := protocol.Decode[protocol.TerminalExit](m)
	if err != nil {
		return err
	}
	if e.Error != "" {
		return errors.New(e.Error)
	}
	if e.Code != 0 {
		return &ExitError{Code: e.Code}
	}
	return nil
}

// Like SSH, '~.' at the start of a line closes the connection locally; '~~'
// sends a literal '~'. State survives arbitrary network/keyboard chunking.
type escapeFilter struct{ lineStart, pending bool }

func (f *escapeFilter) feed(b []byte) ([]byte, bool) {
	out := make([]byte, 0, len(b)+1)
	for _, c := range b {
		if f.pending {
			f.pending = false
			if c == '.' {
				return out, true
			}
			out = append(out, '~')
			f.lineStart = false
			if c == '~' {
				continue
			}
		} else if f.lineStart && c == '~' {
			f.pending = true
			continue
		}
		out = append(out, c)
		f.lineStart = c == '\r' || c == '\n'
	}
	return out, false
}
