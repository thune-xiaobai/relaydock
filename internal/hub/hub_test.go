package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"relaydock/internal/config"
	"relaydock/internal/protocol"
)

func testHub(t *testing.T) *Hub {
	t.Helper()
	h, e := New(config.Config{StateDir: t.TempDir(), Listen: "127.0.0.1:0", Workers: map[string]config.Peer{"n": {Token: strings.Repeat("w", 32)}}, Channels: map[string]config.Peer{"u": {Token: strings.Repeat("c", 32), Nodes: []string{"n"}}, "other": {Token: strings.Repeat("o", 32), Nodes: []string{"n"}}}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { h.Close() })
	return h
}
func TestAuthBindsRoleAndNode(t *testing.T) {
	h := testHub(t)
	server := httptest.NewServer(h.Handler())
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	_, resp, e := websocket.DefaultDialer.Dial(url, nil)
	if e == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("missing credential accepted")
	}
	head := http.Header{"Authorization": []string{"Bearer " + strings.Repeat("c", 32)}}
	c, _, e := websocket.DefaultDialer.Dial(url, head)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	_ = c.WriteJSON(protocol.Wrap("hello", "", protocol.Hello{Role: "worker", ID: "n"}))
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	var m protocol.Message
	if e = c.ReadJSON(&m); e == nil {
		t.Fatal("channel token impersonated worker")
	}
}
func TestEventsDoNotChangeFocusOrRegressCompletion(t *testing.T) {
	h := testHub(t)
	s := protocol.Session{ID: "s", Node: "n", Workspace: "a", Status: "running", RunID: "run"}
	if e := h.db.Put("sessions", "s", Binding{Session: s, Owner: "u"}); e != nil {
		t.Fatal(e)
	}
	if e := h.db.Put("dialogues", "u", dialogue{Focus: "another"}); e != nil {
		t.Fatal(e)
	}
	base := time.Now().UTC()
	ev := protocol.Event{ID: "e_done", Session: "s", Instance: "i", RunID: "run", Seq: 3, Kind: "settled", Status: "completed", Time: base.Format(time.RFC3339Nano)}
	for i := 0; i < 2; i++ {
		ack, e := h.event("n", ev)
		if e != nil || !ack {
			t.Fatal(ack, e)
		}
	}
	ev.ID = "e_old"
	ev.Seq = 2
	ev.Kind = "started"
	ev.Time = base.Add(-time.Second).Format(time.RFC3339Nano)
	if _, e := h.event("n", ev); e != nil {
		t.Fatal(e)
	}
	var b Binding
	_ = h.db.Get("sessions", "s", &b)
	if b.Outcome != "completed" || b.Session.Status != "idle" {
		t.Fatal(b)
	}
	m, _ := h.db.List("outbox")
	if len(m) != 1 {
		t.Fatalf("replayed duplicate notification: %d", len(m))
	}
	var d dialogue
	_ = h.db.Get("dialogues", "u", &d)
	if d.Focus != "another" {
		t.Fatal("async event stole focus")
	}
	if _, _, e := h.perform(context.Background(), "other", "结果", &dialogue{}, Decision{Action: "result", Session: "s"}); e == nil {
		t.Fatal("cross-channel result access allowed")
	}
}
func TestInvalidModelResourcesDoNotDispatch(t *testing.T) {
	h := testHub(t)
	for _, d := range []Decision{{Action: "exec"}, {Action: "start", Node: "invented"}, {Action: "continue", Session: "missing"}} {
		if _, _, e := h.perform(context.Background(), "u", "hello", &dialogue{}, d); e == nil {
			t.Fatal("bad decision accepted")
		}
	}
	m, e := h.db.List("calls")
	if e != nil || len(m) != 0 {
		t.Fatal("invalid decision dispatched", e)
	}
}

func TestHubRestartDoesNotReplayIncompleteChat(t *testing.T) {
	h := testHub(t)
	c := h.c
	if e := h.db.Put("inbox", "u/in_old", chatRecord{Input: protocol.ChatInput{ID: "in_old", Text: "执行任务"}}); e != nil {
		t.Fatal(e)
	}
	if e := h.Close(); e != nil {
		t.Fatal(e)
	}
	restored, e := New(c, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close()
	var in chatRecord
	if e = restored.db.Get("inbox", "u/in_old", &in); e != nil || !in.Done {
		t.Fatal(in, e)
	}
	if len(restored.jobs) != 0 {
		t.Fatal("interrupted task was automatically queued")
	}
	out, e := restored.db.List("outbox")
	if e != nil || len(out) != 1 {
		t.Fatal("missing uncertainty notice", e)
	}
}
