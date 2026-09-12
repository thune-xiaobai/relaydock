package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"relaydock/internal/channel"
	"relaydock/internal/config"
	"relaydock/internal/protocol"
	"relaydock/internal/store"
)

func TestClosedSessionDrainsLateEvents(t *testing.T) {
	h := testHub(t)
	b := Binding{Owner: "u", Session: protocol.Session{ID: "s", Node: "n", Status: "closed", RunID: "run", Workspace: "project"}}
	if err := h.db.Put("sessions", "s", b); err != nil {
		t.Fatal(err)
	}
	if err := h.db.Put("watches", "s", watchRecord{ID: "watch", Owner: "u", Session: "s", RunID: "run", Instruction: "continue"}); err != nil {
		t.Fatal(err)
	}
	for i, kind := range []string{"ready", "started", "output", "settled", "disconnected"} {
		e := protocol.Event{ID: "e_" + kind, Session: "s", Instance: "i", Native: "native", RunID: "run", Seq: int64(i + 1), Position: int64(i + 1), Kind: kind, Status: "completed", Time: protocol.Now()}
		if kind == "output" {
			e.Text = "final build result"
		}
		for repeat := 0; repeat < 2; repeat++ {
			if ack, err := h.event("n", e); err != nil || !ack {
				t.Fatal(ack, err)
			}
		}
	}
	if err := h.db.Get("sessions", "s", &b); err != nil {
		t.Fatal(err)
	}
	if b.Session.Status != "closed" || b.LastText != "final build result" || b.Outcome != "completed" {
		t.Fatal(b)
	}
	out, err := h.db.List("outbox")
	if err != nil || len(out) != 3 {
		t.Fatalf("late notifications: %d %v", len(out), err)
	}
	var run map[string]string
	if err := h.db.Get("runs", "run", &run); err != nil || run["status"] != "completed" {
		t.Fatal(run, err)
	}
	if j, err := h.nextJob(); err != nil || j != nil {
		t.Fatalf("closed watch fired: %v %v", j, err)
	}
}

func TestRevocationFiltersEventsQueuedRepliesAndHistory(t *testing.T) {
	h := testHub(t)
	h.c.Workers["m"] = config.Peer{Token: strings.Repeat("m", 32)}
	p := h.c.Channels["u"]
	p.Nodes = []string{"n", "m"}
	h.c.Channels["u"] = p
	for _, node := range []string{"n", "m"} {
		if err := h.db.Put("sessions", "s_"+node, Binding{Owner: "u", Session: protocol.Session{ID: "s_" + node, Node: node}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.db.Put("remote_jobs", "job", RemoteBinding{Owner: "u", Snapshot: protocol.RemoteSnapshot{Job: protocol.RemoteJob{ID: "job", Node: "n", CallID: "call", Status: "running", Revision: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.Update(func(tx *store.Tx) error {
		for _, o := range []protocol.ChatOutput{{Text: "queued pi output", Session: "s_n"}, {Text: "queued shell output", JobID: "job"}} {
			if err := h.queueOutput(tx, "u", o); err != nil {
				return err
			}
		}
		if err := h.queueOutput(tx, "u", protocol.ChatOutput{Text: "multi-node summary", Session: "s_m"}, "n", "m"); err != nil {
			return err
		}
		if err := tx.Put("outbox", "legacy", delivery{Owner: "u", Output: protocol.ChatOutput{ID: "legacy", Text: "old unscoped reply"}}); err != nil {
			return err
		}
		return h.queueOutput(tx, "u", protocol.ChatOutput{Text: "still authorized", Session: "s_m"})
	}); err != nil {
		t.Fatal(err)
	}
	old := dialogue{Nodes: []string{"n", "m"}, Focus: "s_n", History: []historyItem{{"assistant", "private node data"}}}
	if err := h.db.Put("dialogues", "u", old); err != nil {
		t.Fatal(err)
	}
	// Apply revocation through the same restart boundary as configuration reload.
	cfg := h.c
	p.Nodes = []string{"m"}
	cfg.Channels["u"] = p
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if d := h.filterDialogue("u", old); len(d.History) != 0 || d.Focus != "" {
		t.Fatal(d)
	}
	if ack, err := h.event("n", protocol.Event{ID: "revoked", Session: "s_n", Instance: "i", Seq: 1, Position: 1, Kind: "output", Text: "new private output", Time: protocol.Now()}); err != nil || !ack {
		t.Fatal(ack, err)
	}
	if ack, err := h.remoteEvent("n", protocol.RemoteSnapshot{Job: protocol.RemoteJob{ID: "job", Node: "n", CallID: "call", Status: "succeeded", Revision: 2}, Stdout: "private shell output"}); err != nil || !ack {
		t.Fatal(ack, err)
	}
	server := httptest.NewServer(h.Handler())
	defer server.Close()
	ch, err := channel.New(config.Config{StateDir: t.TempDir(), ID: "u", Token: strings.Repeat("c", 32), Hub: "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"})
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	outputs := make(chan string, 20)
	ch.Output = func(o protocol.ChatOutput) error { outputs <- o.Text; return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done, channelDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(done); h.Serve(ctx) }()
	go func() { defer close(channelDone); ch.Run(ctx) }()
	defer func() { cancel(); <-done; <-channelDone }()
	select {
	case text := <-outputs:
		if text != "still authorized" {
			t.Fatalf("leaked: %s", text)
		}
	case <-ctx.Done():
		t.Fatal("authorized delivery missing")
	}
	for {
		out, err := h.db.List("outbox")
		if err != nil {
			t.Fatal(err)
		}
		if len(out) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("revoked output not drained")
		case <-time.After(20 * time.Millisecond):
		}
	}
	select {
	case text := <-outputs:
		t.Fatalf("unexpected output: %s", text)
	default:
	}
}

func TestResultPersistenceRetriesWithoutExecutingAgain(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "recovery", true: "shutdown"}[shutdown], func(t *testing.T) {
			h := testHub(t)
			var calls atomic.Int32
			h.coordinator = coordinatorFunc(func(context.Context, Turn, func(context.Context, string, json.RawMessage) ToolReply) (string, error) {
				calls.Add(1)
				return "original completed result", nil
			})
			if err := h.db.Update(func(tx *store.Tx) error {
				return h.queueInput(tx, "u", protocol.ChatInput{ID: "request", Text: "run once"})
			}); err != nil {
				t.Fatal(err)
			}
			j, err := h.nextJob()
			if err != nil || j == nil {
				t.Fatal(j, err)
			}
			db, err := sql.Open("sqlite", filepath.Join(h.c.StateDir, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err = db.Exec("CREATE TRIGGER fail_result BEFORE INSERT ON kv WHEN NEW.bucket='outbox' BEGIN SELECT RAISE(ABORT, 'temporary test write failure'); END"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); h.process(ctx, *j) }()
			for calls.Load() == 0 {
				select {
				case <-ctx.Done():
					t.Fatal("coordinator did not run")
				case <-time.After(10 * time.Millisecond):
				}
			}
			time.Sleep(250 * time.Millisecond)
			var r chatRecord
			if err = h.db.Get("inbox", "u/request", &r); err != nil || r.Done {
				t.Fatal(r, err)
			}
			if shutdown {
				cancel()
				<-done
			}
			if _, err = db.Exec("DROP TRIGGER fail_result"); err != nil {
				t.Fatal(err)
			}
			if !shutdown {
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal("finalization did not recover")
				}
				if err = h.db.Get("inbox", "u/request", &r); err != nil || !r.Done {
					t.Fatal(r, err)
				}
				var d dialogue
				if err = h.db.Get("dialogues", "u", &d); err != nil || len(d.History) != 2 {
					t.Fatal(d, err)
				}
			} else {
				cfg := h.c
				h.Close()
				h, err = New(cfg, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer h.Close()
			}
			out, err := h.db.List("outbox")
			if err != nil || len(out) != 1 {
				t.Fatalf("outputs %d: %v", len(out), err)
			}
			for _, raw := range out {
				var d delivery
				if err := json.Unmarshal(raw, &d); err != nil {
					t.Fatal(err)
				}
				if !shutdown && d.Output.Text != "original completed result" {
					t.Fatal(d)
				}
				if shutdown && !strings.Contains(d.Output.Text, "未自动重发") {
					t.Fatal(d)
				}
			}
			if calls.Load() != 1 {
				t.Fatal("coordinator reran", calls.Load())
			}
			if next, err := h.nextJob(); err != nil || next != nil {
				t.Fatal(next, err)
			}
		})
	}
}

func TestDialogueReadFailureDoesNotStrandInput(t *testing.T) {
	h := testHub(t)
	if err := h.db.Put("dialogues", "u", "invalid stored dialogue"); err != nil {
		t.Fatal(err)
	}
	if err := h.db.Update(func(tx *store.Tx) error { return h.queueInput(tx, "u", protocol.ChatInput{ID: "read", Text: "status"}) }); err != nil {
		t.Fatal(err)
	}
	j, err := h.nextJob()
	if err != nil || j == nil {
		t.Fatal(j, err)
	}
	var calls atomic.Int32
	h.coordinator = coordinatorFunc(func(context.Context, Turn, func(context.Context, string, json.RawMessage) ToolReply) (string, error) {
		calls.Add(1)
		return "recovered", nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); h.process(ctx, *j) }()
	time.Sleep(200 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("ran without history")
	}
	if err := h.db.Put("dialogues", "u", dialogue{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("read did not recover")
	}
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
}
