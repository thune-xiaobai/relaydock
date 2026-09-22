package hub

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"relaydock/internal/protocol"
	"relaydock/internal/transport"
)

// A bridge lives only as long as its two sockets and original Worker control
// connection. It is never persisted, reattached, or replayed after restart.
type terminalBridge struct {
	owner, node string
	control     *connection
	client      *transport.Peer
	mu          sync.Mutex
	worker      *transport.Peer
	done        chan struct{}
	ready       chan struct{}
	once        sync.Once
}

func (b *terminalBridge) close() {
	b.once.Do(func() {
		close(b.done)
		b.client.Close()
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.worker != nil {
			b.worker.Close()
		}
	})
}
func (b *terminalBridge) fail(message string) {
	_ = b.client.Send(protocol.Wrap("shell_exit", "", protocol.TerminalExit{Code: -1, Error: message}))
	b.close()
}
func upgradeTerminal(w http.ResponseWriter, r *http.Request) (*transport.Peer, error) {
	u := websocket.Upgrader{}
	c, err := u.Upgrade(w, r, nil)
	if err != nil {
		return nil, err
	}
	p := transport.New(c)
	c.SetReadLimit(64 << 10)
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	return p, nil
}

func (h *Hub) acceptTerminal(w http.ResponseWriter, r *http.Request) {
	role, owner := h.authenticate(r)
	if role != "channel" {
		http.Error(w, "channel credential required", http.StatusUnauthorized)
		return
	}
	p, err := upgradeTerminal(w, r)
	if err != nil {
		return
	}
	defer p.Close()
	fail := func(s string) { _ = p.Send(protocol.Wrap("shell_exit", "", protocol.TerminalExit{Code: -1, Error: s})) }
	m, err := p.Read()
	if err != nil {
		return
	}
	open, err := protocol.Decode[protocol.TerminalOpen](m)
	if err != nil || m.Type != "shell_open" || m.ID != "" || open.PeerID != owner || open.Validate() != nil {
		fail("invalid terminal request or channel identity")
		return
	}
	if !h.allowed(owner, open.Node) {
		fail("target node is not authorized")
		return
	}
	h.mu.Lock()
	c := h.workers[open.Node]
	if c == nil || c.hello.Shell == nil || !c.hello.Shell.Interactive {
		h.mu.Unlock()
		fail("Worker is offline or does not support interactive terminals")
		return
	}
	count := 0
	for _, b := range h.terminals {
		if b.node == open.Node {
			count++
		}
	}
	limit := min(64, c.hello.Shell.MaxRunning)
	if limit < 1 || count >= limit {
		h.mu.Unlock()
		fail("terminal capacity reached")
		return
	}
	id := protocol.ID("tty_")
	b := &terminalBridge{owner: owner, node: open.Node, control: c, client: p, done: make(chan struct{}), ready: make(chan struct{})}
	h.terminals[id] = b
	h.mu.Unlock()
	defer func() { b.close(); h.mu.Lock(); delete(h.terminals, id); h.mu.Unlock() }()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go p.KeepAlive(ctx)
	go func() {
		timer := time.NewTimer(15 * time.Second)
		defer timer.Stop()
		select {
		case <-b.ready:
		case <-b.done:
		case <-timer.C:
			b.fail("Worker terminal startup timed out; no retry was sent")
		}
	}()
	if err = c.peer.Send(protocol.Wrap("shell_open", id, open)); err != nil {
		fail("Worker connection failed; terminal request was not retried")
		return
	}
	for {
		m, err = p.Read()
		if err != nil {
			return
		}
		if err = protocol.TerminalInput(m); err != nil {
			fail(err.Error())
			return
		}
		select {
		case <-b.ready:
		default:
			fail("terminal is not ready")
			return
		}
		if !h.terminalAllowed(b) {
			return
		}
		b.mu.Lock()
		target := b.worker
		b.mu.Unlock()
		if target == nil || target.Send(m) != nil {
			return
		}
	}
}

func (h *Hub) terminalAllowed(b *terminalBridge) bool {
	h.mu.Lock()
	current := h.workers[b.node] == b.control
	h.mu.Unlock()
	return current && h.allowed(b.owner, b.node)
}

func (h *Hub) joinTerminal(w http.ResponseWriter, r *http.Request) {
	role, node := h.authenticate(r)
	if role != "worker" {
		http.Error(w, "worker credential required", http.StatusUnauthorized)
		return
	}
	p, err := upgradeTerminal(w, r)
	if err != nil {
		return
	}
	defer p.Close()
	m, err := p.Read()
	if err != nil || m.Type != "shell_join" || m.Version != protocol.Version {
		return
	}
	h.mu.Lock()
	b := h.terminals[m.ID]
	h.mu.Unlock()
	if b == nil || b.node != node || !h.terminalAllowed(b) {
		return
	}
	b.mu.Lock()
	select {
	case <-b.done:
		b.mu.Unlock()
		return
	default:
	}
	if b.worker != nil {
		b.mu.Unlock()
		return
	}
	b.worker = p
	b.mu.Unlock()
	defer b.close()
	if p.Send(protocol.Wrap("welcome", "", nil)) != nil {
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go p.KeepAlive(ctx)
	ready := false
	for {
		m, err = p.Read()
		if err != nil {
			return
		}
		if err = protocol.TerminalOutput(m); err != nil {
			return
		}
		if !h.terminalAllowed(b) {
			return
		}
		if m.Type == "shell_ready" {
			if ready {
				return
			}
			ready = true
			close(b.ready)
		} else if !ready && m.Type != "shell_exit" {
			return
		}
		if b.client.Send(m) != nil || m.Type == "shell_exit" {
			return
		}
	}
}
