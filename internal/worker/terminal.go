package worker

import (
	"context"
	"sync"
	"time"

	"relaydock/internal/protocol"
	"relaydock/internal/terminal"
	"relaydock/internal/transport"
)

// A new stream uses an outbound data socket on the same Hub listener. Terminal
// backpressure cannot block call receipts, heartbeats, or chat notifications.
func (w *Worker) openTerminal(ctx context.Context, control *transport.Peer, m protocol.Message, slots chan struct{}, wg *sync.WaitGroup) {
	fail := func(message string) {
		_ = control.Send(protocol.Wrap("shell_error", m.ID, protocol.TerminalExit{Code: -1, Error: message}))
	}
	open, err := protocol.Decode[protocol.TerminalOpen](m)
	if err != nil || !protocol.ValidID(m.ID) || open.Validate() != nil || open.Node != w.c.ID {
		fail("invalid terminal request")
		return
	}
	if w.shell == nil || !w.Hello().Shell.Interactive {
		fail("interactive shell is disabled or unsupported")
		return
	}
	select {
	case slots <- struct{}{}:
	default:
		fail("terminal capacity reached")
		return
	}
	wg.Add(1)
	go func() {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		defer wg.Done()
		defer func() { <-slots }()
		p, err := transport.Connect(ctx, w.c, "shell/worker")
		if err != nil {
			if ctx.Err() == nil {
				fail(err.Error())
			}
			return
		}
		defer p.Close()
		stop := context.AfterFunc(ctx, p.Close)
		defer stop()
		_ = p.C.SetReadDeadline(time.Now().Add(10 * time.Second))
		if p.Send(protocol.Wrap("shell_join", m.ID, nil)) != nil {
			return
		}
		welcome, err := p.Read()
		if err != nil || welcome.Type != "welcome" || welcome.Version != protocol.Version {
			return
		}
		go p.KeepAlive(ctx)
		cwd, err := terminal.ResolveCWD(w.c, open.CWD)
		if err != nil {
			_ = p.Send(protocol.Wrap("shell_exit", "", protocol.TerminalExit{Code: -1, Error: err.Error()}))
			return
		}
		terminal.Serve(ctx, p, w.shell.Info().Executable, cwd, open)
	}()
}
