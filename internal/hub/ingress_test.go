package hub

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"relaydock/internal/config"
	"relaydock/internal/protocol"
	"relaydock/internal/transport"
)

func TestHubRejectsBadChatWithoutDisconnect(t *testing.T) {
	h := testHub(t)
	server := httptest.NewServer(h.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := transport.Dial(ctx, config.Config{Hub: "ws" + strings.TrimPrefix(server.URL, "http") + "/ws", Token: strings.Repeat("c", 32)}, protocol.Hello{Role: "channel", ID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, tc := range []struct{ id, text, want string }{{"blank", " \t\u3000", "chat_reject"}, {"good", "check status", "chat_ack"}, {"good", "different text", "chat_reject"}, {"next", "continue", "chat_ack"}} {
		if err := p.Send(protocol.Wrap("chat", tc.id, protocol.ChatInput{ID: tc.id, Text: tc.text})); err != nil {
			t.Fatal(err)
		}
		m, err := p.Read()
		if err != nil || m.Type != tc.want || m.ID != tc.id {
			t.Fatal(m, err)
		}
	}
	if m, err := h.db.List("inbox"); err != nil || len(m) != 2 {
		t.Fatal(m, err)
	}
}
