package transport

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"relaydock/internal/config"
	"relaydock/internal/protocol"
)

type Peer struct {
	C  *websocket.Conn
	mu sync.Mutex
}

func New(c *websocket.Conn) *Peer {
	c.SetReadLimit(protocol.MaxMessage)
	_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.SetPongHandler(func(string) error { return c.SetReadDeadline(time.Now().Add(60 * time.Second)) })
	return &Peer{C: c}
}
func (p *Peer) Send(m protocol.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = p.C.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return p.C.WriteJSON(m)
}
func (p *Peer) Read() (protocol.Message, error) {
	var m protocol.Message
	err := p.C.ReadJSON(&m)
	if err == nil {
		_ = p.C.SetReadDeadline(time.Now().Add(60 * time.Second))
	}
	return m, err
}
func (p *Peer) Close() { _ = p.C.Close() }
func (p *Peer) KeepAlive(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := p.C.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				p.Close()
				return
			}
		}
	}
}

// Connect opens an authenticated socket without a protocol handshake. Route is
// relative to the /ws endpoint's prefix; terminal sockets never register as chat.
func Connect(ctx context.Context, c config.Config, route string) (*Peer, error) {
	tls, err := config.ClientTLS(c)
	if err != nil {
		return nil, err
	}
	d := websocket.Dialer{TLSClientConfig: tls, HandshakeTimeout: 15 * time.Second}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+c.Token)
	address := c.Hub
	if route != "" {
		u, err := url.Parse(address)
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(u.Path, "/ws") {
			return nil, fmt.Errorf("Hub URL must end in /ws for terminal connections")
		}
		u.Path = strings.TrimSuffix(u.Path, "/ws") + "/" + route
		u.RawPath = ""
		address = u.String()
	}
	conn, _, err := d.DialContext(ctx, address, h)
	if err != nil {
		return nil, err
	}
	return New(conn), nil
}

func Dial(ctx context.Context, c config.Config, hello protocol.Hello) (*Peer, error) {
	p, err := Connect(ctx, c, "")
	if err != nil {
		return nil, err
	}
	if err = p.Send(protocol.Wrap("hello", "", hello)); err != nil {
		p.Close()
		return nil, err
	}
	m, err := p.Read()
	if err != nil {
		p.Close()
		return nil, err
	}
	if m.Type != "welcome" || m.Version != protocol.Version {
		p.Close()
		return nil, &HandshakeError{}
	}
	return p, nil
}

type HandshakeError struct{}

func (*HandshakeError) Error() string { return "hub rejected registration" }
