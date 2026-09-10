package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"relaydock/internal/config"
	"relaydock/internal/localfile"
	"relaydock/internal/protocol"
	"relaydock/internal/store"
)

func testWorker(t *testing.T) *Worker {
	t.Helper()
	root := t.TempDir()
	db, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	w := &Worker{db: db, c: config.Config{StateDir: root, ID: "node", Workspaces: map[string]string{"a": filepath.Join(root, "project"), "alias": filepath.Join(root, "project")}, Agents: map[string]config.Agent{"pi": {}}, MaxRunning: 4}}
	s := protocol.Session{ID: "s_test", Node: "node", Workspace: "a", Agent: "pi", Status: "idle"}
	if err = db.Put("sessions", s.ID, s); err != nil {
		t.Fatal(err)
	}
	if err = localfile.Write(filepath.Join(w.dir(s.ID), "state.json"), protocol.BridgeState{Session: s.ID, Instance: "i_test", Native: "native", Status: "idle", Updated: protocol.Now()}); err != nil {
		t.Fatal(err)
	}
	return w
}
func TestCallDedupeAndStaleTarget(t *testing.T) {
	w := testWorker(t)
	c := protocol.Call{ID: "c_once", Tool: "agent.submit", Args: protocol.JSON(Arguments{Session: "s_test", Instance: "i_test", Native: "native", RunID: "r_one", Text: "检查测试"})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			if _, e := os.Stat(filepath.Join(w.dir("s_test"), "requests", c.ID+".json")); e == nil {
				_ = localfile.Write(filepath.Join(w.dir("s_test"), "receipts", c.ID+".json"), protocol.OK(c.ID, map[string]bool{"accepted": true}))
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	first := w.Call(context.Background(), c)
	<-done
	if first.Error != nil {
		t.Fatal(first.Error)
	}
	if err := os.Remove(filepath.Join(w.dir("s_test"), "requests", c.ID+".json")); err != nil {
		t.Fatal(err)
	}
	again := w.Call(context.Background(), c)
	if string(protocol.JSON(first)) != string(protocol.JSON(again)) {
		t.Fatal("same call did not recover exact result")
	}
	if _, e := os.Stat(filepath.Join(w.dir("s_test"), "requests", c.ID+".json")); !os.IsNotExist(e) {
		t.Fatal("replayed call wrote another request")
	}
	c.Args = protocol.JSON(Arguments{Session: "s_test", Instance: "wrong", Native: "native", RunID: "r_one", Text: "changed"})
	if r := w.Call(context.Background(), c); r.Error == nil || r.Error.Code != "conflict" {
		t.Fatal(r)
	}
	c.ID = "c_stale"
	if r := w.Call(context.Background(), c); r.Error == nil || r.Error.Code != "stale_target" {
		t.Fatal(r)
	}
}
func TestWorkspaceReservationAndBridgeHealth(t *testing.T) {
	w := testWorker(t)
	r := w.Call(context.Background(), protocol.Call{ID: "c_alias", Tool: "session.create", Args: protocol.JSON(Arguments{Workspace: "alias", Agent: "pi"})})
	if r.Error == nil || r.Error.Code != "workspace_busy" {
		t.Fatal(r)
	}
	if err := localfile.Write(filepath.Join(w.dir("s_test"), "state.json"), protocol.BridgeState{Session: "s_test", Instance: "i_test", Native: "native", Status: "idle", Updated: time.Now().Add(-time.Minute).Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	s, e := w.inspect("s_test")
	if e != nil || s.Status != "unknown" {
		t.Fatalf("stale heartbeat: %+v %v", s, e)
	}
	if err := localfile.Write(filepath.Join(w.dir("s_test"), "exit.json"), map[string]string{"time": protocol.Now(), "error": "exit"}); err != nil {
		t.Fatal(err)
	}
	s, e = w.inspect("s_test")
	if e != nil || s.Status != "exited" {
		t.Fatalf("exit: %+v %v", s, e)
	}
}
func TestEventCursorDoesNotConsumePartialLine(t *testing.T) {
	w := testWorker(t)
	ev := protocol.Event{ID: "e_one", Session: "s_test", Instance: "i_test", Seq: 1, Kind: "started", Time: protocol.Now()}
	full := append(protocol.JSON(ev), '\n')
	file := filepath.Join(w.dir("s_test"), "events.ndjson")
	if e := os.WriteFile(file, full[:len(full)-1], 0600); e != nil {
		t.Fatal(e)
	}
	if e := w.collect(); e != nil {
		t.Fatal(e)
	}
	m, _ := w.db.List("outbox")
	if len(m) != 0 {
		t.Fatal("consumed incomplete event")
	}
	f, e := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	_, e = f.Write([]byte{'\n'})
	f.Close()
	if e != nil {
		t.Fatal(e)
	}
	if e = w.collect(); e != nil {
		t.Fatal(e)
	}
	if e = w.collect(); e != nil {
		t.Fatal(e)
	}
	m, _ = w.db.List("outbox")
	if len(m) != 1 {
		t.Fatalf("want one event, got %d", len(m))
	}
	var got protocol.Event
	_ = json.Unmarshal(m[ev.ID], &got)
	if got.ID != ev.ID {
		t.Fatal(got)
	}
}

func TestLookupNeverDispatchesAndRecoversLateReceipt(t *testing.T) {
	w := testWorker(t)
	if r := w.Lookup("c_missing"); r.Error == nil || r.Error.Code != "unknown" {
		t.Fatal(r)
	}
	if e := w.db.Put("calls", "c_late", callRecord{Target: "s_test", Result: ptrResult(protocol.Fail("c_late", "unknown", "interrupted"))}); e != nil {
		t.Fatal(e)
	}
	if e := localfile.Write(filepath.Join(w.dir("s_test"), "receipts", "c_late.json"), protocol.OK("c_late", map[string]bool{"accepted": true})); e != nil {
		t.Fatal(e)
	}
	if r := w.Lookup("c_late"); r.Error != nil {
		t.Fatal(r)
	}
	if _, e := os.Stat(filepath.Join(w.dir("s_test"), "requests", "c_late.json")); !os.IsNotExist(e) {
		t.Fatal("lookup created request")
	}
}
func ptrResult(r protocol.Result) *protocol.Result { return &r }
