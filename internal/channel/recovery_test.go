package channel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"relaydock/internal/config"
	"relaydock/internal/protocol"
	"relaydock/internal/transport"
)

func TestPendingInvalidAndRejectedInputsDoNotBlockDelivery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := websocket.Upgrader{}
		ws, err := u.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		p := transport.New(ws)
		defer p.Close()
		if _, err := p.Read(); err != nil {
			return
		}
		if err := p.Send(protocol.Wrap("welcome", "", nil)); err != nil {
			return
		}
		for {
			m, err := p.Read()
			if err != nil {
				return
			}
			if m.ID == "conflict" {
				err = p.Send(protocol.Wrap("chat_reject", m.ID, protocol.Fault{Code: "conflict", Message: "ID already used"}))
			} else {
				err = p.Send(protocol.Wrap("chat_ack", m.ID, nil))
			}
			if err != nil {
				return
			}
		}
	}))
	defer server.Close()
	c, err := New(config.Config{StateDir: t.TempDir(), ID: "chat", Token: strings.Repeat("c", 32), Hub: "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Enqueue(protocol.ChatInput{ID: "blank", Text: " \t\u3000\n"}); !errors.Is(err, protocol.ErrInvalidInput) {
		t.Fatal(err)
	}
	// Emulate the old queue format: this message used to reconnect forever.
	if err := c.db.Put("pending", "legacy", queuedInput{Sequence: 0, Input: protocol.ChatInput{ID: "legacy", Text: "   "}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"conflict", "good"} {
		if err := c.Enqueue(protocol.ChatInput{ID: id, Text: "status"}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	defer func() { cancel(); <-done }()
	for {
		m, err := c.db.List("pending")
		if err != nil {
			t.Fatal(err)
		}
		if len(m) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("valid input stranded")
		case <-time.After(20 * time.Millisecond):
		}
	}
	rejected, err := c.db.List("rejected")
	if err != nil || len(rejected) != 2 {
		t.Fatal(rejected, err)
	}
}

func TestSpoolQuarantinesBadFilesAndContinues(t *testing.T) {
	c, err := New(config.Config{StateDir: t.TempDir(), Spool: t.TempDir(), ID: "chat", Token: strings.Repeat("c", 32), Hub: "ws://127.0.0.1:1/ws"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.spool(); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"syntax.json": []byte("{"), "wrong.json": protocol.JSON(protocol.ChatInput{ID: "different", Text: "hello"}), "blank.json": protocol.JSON(protocol.ChatInput{ID: "blank", Text: "  "}), "valid.json": protocol.JSON(protocol.ChatInput{ID: "valid", Text: "check status"})}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(c.c.Spool, "inbox", name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := c.spool(); err != nil {
			t.Fatal(err)
		}
	}
	m, err := c.db.List("pending")
	if err != nil || len(m) != 1 {
		t.Fatal(m, err)
	}
	var got queuedInput
	if err := json.Unmarshal(m["valid"], &got); err != nil || got.Input.Text != "check status" {
		t.Fatal(got, err)
	}
	rejected, err := os.ReadDir(filepath.Join(c.c.Spool, "rejected"))
	if err != nil || len(rejected) != 3 {
		t.Fatal(rejected, err)
	}
}
