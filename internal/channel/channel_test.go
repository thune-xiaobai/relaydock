package channel

import (
	"encoding/json"
	"strings"
	"testing"

	"relaydock/internal/config"
	"relaydock/internal/protocol"
)

func TestInputOrderingAndDedupe(t *testing.T) {
	c, e := New(config.Config{StateDir: t.TempDir(), ID: "chat", Token: strings.Repeat("c", 32), Hub: "ws://127.0.0.1:1/ws"})
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	for _, id := range []string{"z_first", "a_second", "z_first"} {
		if e = c.Enqueue(protocol.ChatInput{ID: id, Text: id}); e != nil {
			t.Fatal(e)
		}
	}
	m, e := c.db.List("pending")
	if e != nil {
		t.Fatal(e)
	}
	if len(m) != 2 {
		t.Fatal("duplicate enqueued")
	}
	var first, second queuedInput
	_ = json.Unmarshal(m["z_first"], &first)
	_ = json.Unmarshal(m["a_second"], &second)
	if first.Sequence >= second.Sequence {
		t.Fatal("input order lost")
	}
	if e = c.Enqueue(protocol.ChatInput{ID: "z_first", Text: "different"}); e == nil {
		t.Fatal("ID reused for different input")
	}
}
