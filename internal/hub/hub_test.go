package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"relaydock/internal/config"
	"relaydock/internal/protocol"
	"relaydock/internal/store"
	"relaydock/internal/transport"
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
	if r := h.executeTool(context.Background(), "other", &dialogue{}, "session_result", protocol.JSON(map[string]string{"session_id": "s"})); r.OK {
		t.Fatal("cross-channel result access allowed")
	}
}
func TestInvalidModelResourcesDoNotDispatch(t *testing.T) {
	h := testHub(t)
	for _, c := range []struct {
		name string
		args any
	}{
		{"exec", map[string]string{}}, {"session_create", map[string]string{"node": "invented", "workspace": "a", "agent": "pi"}},
		{"agent_submit", map[string]string{"session_id": "missing", "text": "hello"}},
		{"inventory", map[string]string{"unrecognized": "x"}},
	} {
		if r := h.executeTool(context.Background(), "u", &dialogue{}, c.name, protocol.JSON(c.args)); r.OK {
			t.Fatal("bad tool accepted")
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
	if next, err := restored.nextJob(); err != nil || next != nil {
		t.Fatal("interrupted task was automatically queued")
	}
	out, e := restored.db.List("outbox")
	if e != nil || len(out) != 1 {
		t.Fatal("missing uncertainty notice", e)
	}
}

func TestQueuedInputSurvivesRestartInOrder(t *testing.T) {
	h := testHub(t)
	c := h.c
	for _, id := range []string{"z_first", "a_second"} {
		if e := h.db.Update(func(tx *store.Tx) error { return h.queueInput(tx, "u", protocol.ChatInput{ID: id, Text: "hello"}) }); e != nil {
			t.Fatal(e)
		}
	}
	h.Close()
	restored, e := New(c, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close()
	for _, id := range []string{"z_first", "a_second"} {
		j, e := restored.nextJob()
		if e != nil || j == nil || j.Input.ID != id {
			t.Fatal(j, e)
		}
	}
}

func TestWatchCompletionRaceAndDuplicateEvent(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint(late), func(t *testing.T) {
			h := testHub(t)
			b := Binding{Owner: "u", Session: protocol.Session{ID: "s", Node: "n", RunID: "run", Status: "running"}}
			if e := h.db.Put("sessions", "s", b); e != nil {
				t.Fatal(e)
			}
			if e := h.db.Put("runs", "run", map[string]string{"session_id": "s", "owner": "u", "status": "running"}); e != nil {
				t.Fatal(e)
			}
			ev := protocol.Event{ID: "done", Session: "s", Instance: "i", RunID: "run", Seq: 1, Kind: "settled", Status: "completed", Time: protocol.Now()}
			settle := func() {
				t.Helper()
				if ack, e := h.event("n", ev); e != nil || !ack {
					t.Fatal(ack, e)
				}
			}
			if late {
				settle()
			}
			if _, e := h.watch("u", b, "run", "完成后总结结果"); e != nil {
				t.Fatal(e)
			}
			settle()
			settle()
			j, e := h.nextJob()
			if e != nil || j == nil || j.Watch == nil || j.Watch.RunID != "run" {
				t.Fatal(j, e)
			}
			if e = h.db.Put("dialogues", "u", dialogue{Focus: "another"}); e != nil {
				t.Fatal(e)
			}
			h.coordinator = coordinatorFunc(func(ctx context.Context, _ Turn, call func(context.Context, string, json.RawMessage) ToolReply) (string, error) {
				if r := call(ctx, "session_result", protocol.JSON(map[string]string{"session_id": "s"})); !r.OK {
					t.Fatal(r)
				}
				return "watch fixture", nil
			})
			h.process(context.Background(), *j)
			var dialogueAfter dialogue
			if e = h.db.Get("dialogues", "u", &dialogueAfter); e != nil || dialogueAfter.Focus != "another" {
				t.Fatal("watch stole chat focus", dialogueAfter, e)
			}
			outputs, e := h.db.List("outbox")
			if e != nil {
				t.Fatal(e)
			}
			found := false
			for _, raw := range outputs {
				var delivery delivery
				_ = json.Unmarshal(raw, &delivery)
				if delivery.Output.Text == "watch fixture" {
					found = true
					if delivery.Output.Session != "s" || delivery.Output.RunID != "run" {
						t.Fatal("watch reply bound to wrong session", delivery)
					}
				}
			}
			if !found {
				t.Fatal("missing watch reply")
			}
			if j, e = h.nextJob(); e != nil || j != nil {
				t.Fatal("watch fired twice", j, e)
			}
		})
	}
}

type coordinatorFunc func(context.Context, Turn, func(context.Context, string, json.RawMessage) ToolReply) (string, error)

func (f coordinatorFunc) Run(ctx context.Context, turn Turn, call func(context.Context, string, json.RawMessage) ToolReply) (string, error) {
	return f(ctx, turn, call)
}

func TestUncertainMutationBlocksFurtherActionsWithinLoop(t *testing.T) {
	h := testHub(t)
	server := httptest.NewServer(h.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := transport.Dial(ctx, config.Config{Hub: "ws" + strings.TrimPrefix(server.URL, "http") + "/ws", Token: strings.Repeat("w", 32)}, protocol.Hello{Role: "worker", ID: "n", Workspaces: []string{"a"}, Agents: []string{"pi"}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			m, e := p.Read()
			if e != nil {
				return
			}
			if m.Type == "call" {
				c, e := protocol.Decode[protocol.Call](m)
				if e != nil {
					return
				}
				if p.Send(protocol.Wrap("result", c.ID, protocol.Fail(c.ID, "unknown", "lost receipt"))) != nil {
					return
				}
			}
		}
	}()
	defer func() { p.Close(); <-done }()
	h.coordinator = coordinatorFunc(func(ctx context.Context, _ Turn, call func(context.Context, string, json.RawMessage) ToolReply) (string, error) {
		args := protocol.JSON(map[string]string{"node": "n", "workspace": "a", "agent": "pi"})
		first := call(ctx, "session_create", args)
		if first.OK || !first.Uncertain {
			t.Fatal("expected uncertain receipt", first)
		}
		second := call(ctx, "session_create", args)
		if second.OK || !second.Uncertain {
			t.Fatal("second mutation was not blocked", second)
		}
		if r := call(ctx, "inventory", protocol.JSON(map[string]string{})); !r.OK {
			t.Fatal("read-only query was blocked", r)
		}
		return "请先核对状态。", nil
	})
	if err = h.db.Update(func(tx *store.Tx) error {
		return h.queueInput(tx, "u", protocol.ChatInput{ID: "request", Text: "创建任务"})
	}); err != nil {
		t.Fatal(err)
	}
	j, err := h.nextJob()
	if err != nil || j == nil {
		t.Fatal(j, err)
	}
	h.process(ctx, *j)
	calls, err := h.db.List("calls")
	if err != nil || len(calls) != 1 {
		t.Fatal("unknown action was redispatched", len(calls), err)
	}
}
