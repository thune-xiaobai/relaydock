package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"time"

	"golang.org/x/term"
	"relaydock/internal/config"
	"relaydock/internal/protocol"
	"relaydock/internal/transport"
)

type ExitError struct{ Code int64 }

func (e *ExitError) Error() string { return fmt.Sprintf("remote shell exited with status %d", e.Code) }

// ExitStatus preserves Windows DWORD status codes on Windows. Unix exposes
// only eight bits, but a remote failure must never become local success.
func (e *ExitError) ExitStatus() int {
	if e.Code < 1 || e.Code > 1<<32-1 {
		return 1
	}
	if runtime.GOOS == "windows" {
		return int(uint32(e.Code))
	}
	if code := int(e.Code & 255); code != 0 {
		return code
	}
	return 1
}

// Local console setup is independent of the remote shell's platform.
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
					// Buffered escape/key records can expand this chunk.
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
type escapeFilter struct {
	lineStart, pending bool
	pendingEncoded     bool
	pendingBytes       []byte
	sequence           []byte
	sequenceHeld       bool
	sequenceLineStart  bool
}

func (f *escapeFilter) feed(b []byte) ([]byte, bool) {
	out := make([]byte, 0, len(b)+len(f.pendingBytes))
	for _, c := range b {
		if len(f.sequence) == 0 && c == '\x1b' {
			f.sequenceLineStart = f.lineStart
			f.sequenceHeld = f.pending && f.pendingEncoded
			f.sequence = append(f.sequence, c)
			if !f.sequenceHeld {
				// Stream ESC immediately: a lone Escape key must not wait for
				// another key just because it might begin a Win32 record.
				f.key(&out, []byte{c}, int(c), false, false)
			}
			continue
		}
		if len(f.sequence) != 0 {
			f.sequence = append(f.sequence, c)
			if c == '_' && len(f.sequence) >= 3 {
				if key, ok := parseWin32Key(f.sequence); ok {
					f.lineStart = f.sequenceLineStart
					raw := f.sequence
					if !f.sequenceHeld {
						// All preceding bytes are already on the wire. Holding
						// this terminator still withholds the complete key event.
						raw = []byte{'_'}
					}
					quit := f.key(&out, raw, key.char, key.passive, true)
					f.sequence = f.sequence[:0]
					if quit {
						return out, true
					}
					continue
				}
			}
			validPrefix := len(f.sequence) == 2 && c == '[' || len(f.sequence) > 2 && (c >= '0' && c <= '9' || c == ';')
			if !validPrefix || len(f.sequence) >= 96 {
				if f.sequenceHeld {
					f.flushPending(&out)
					out = append(out, f.sequence...)
				} else {
					out = append(out, c)
				}
				f.lineStart = c == '\r' || c == '\n'
				f.sequence = f.sequence[:0]
			} else if !f.sequenceHeld {
				out = append(out, c)
			}
			continue
		}
		if f.key(&out, []byte{c}, int(c), false, false) {
			return out, true
		}
	}
	return out, false
}

func (f *escapeFilter) flushPending(out *[]byte) {
	*out = append(*out, f.pendingBytes...)
	f.pendingBytes = f.pendingBytes[:0]
	f.pending = false
	f.lineStart = false
}

func (f *escapeFilter) key(out *[]byte, raw []byte, char int, passive, encoded bool) bool {
	if passive {
		if f.pending {
			f.pendingBytes = append(f.pendingBytes, raw...)
			// Modifier/key-up floods must not grow an unfinished escape.
			if len(f.pendingBytes) > 4096 {
				f.flushPending(out)
			}
		} else {
			*out = append(*out, raw...)
		}
		return false
	}
	if f.pending {
		if char == '.' {
			return true
		}
		f.flushPending(out)
		if char == '~' {
			return false
		}
	} else if f.lineStart && char == '~' {
		f.pending = true
		f.pendingEncoded = encoded
		f.pendingBytes = append(f.pendingBytes[:0], raw...)
		return false
	}
	*out = append(*out, raw...)
	f.lineStart = char == '\r' || char == '\n'
	return false
}
