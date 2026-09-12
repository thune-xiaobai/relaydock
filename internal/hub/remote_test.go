package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"relaydock/internal/config"
	"relaydock/internal/protocol"
	"relaydock/internal/store"
	"relaydock/internal/worker"
)

func remoteEventually(t *testing.T, check func() bool) {
	t.Helper()
	until := time.Now().Add(8 * time.Second)
	for time.Now().Before(until) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("remote condition timed out")
}
func liveShell(t *testing.T, h *Hub) (*worker.Worker, func(), func()) {
	t.Helper()
	server := httptest.NewServer(h.Handler())
	c := config.Config{ID: "n", Token: strings.Repeat("w", 32), Hub: "ws" + strings.TrimPrefix(server.URL, "http") + "/ws", StateDir: t.TempDir(), Workspaces: map[string]string{"project": t.TempDir()}, Shell: config.Shell{Enabled: true}}
	w, err := worker.New(c)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	var stop context.CancelFunc
	var done chan struct{}
	start := func() {
		ctx, cancel := context.WithCancel(context.Background())
		stop = cancel
		done = make(chan struct{})
		go func() {
			defer close(done)
			if err := w.Run(ctx); err != nil {
				t.Error(err)
			}
		}()
		remoteEventually(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.workers["n"] != nil })
	}
	disconnect := func() {
		stop()
		<-done
		remoteEventually(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.workers["n"] == nil })
	}
	start()
	t.Cleanup(func() { stop(); <-done; w.Close(); server.Close() })
	return w, disconnect, start
}
func runRemoteTool(t *testing.T, h *Hub, d *dialogue, name string, args any) protocol.RemoteSnapshot {
	t.Helper()
	r := h.executeTool(context.Background(), "u", d, name, protocol.JSON(args))
	if !r.OK {
		t.Fatalf("%s: %+v", name, r)
	}
	var s protocol.RemoteSnapshot
	if err := json.Unmarshal(protocol.JSON(r.Data), &s); err != nil || s.Job.ID == "" {
		t.Fatal(r, err)
	}
	return s
}
func remoteCommand(unix, windows string) string {
	if runtime.GOOS == "windows" {
		return windows
	}
	return unix
}

func TestRemoteToolsOverWSSAndReconnect(t *testing.T) {
	h := testHub(t)
	_, disconnect, reconnect := liveShell(t, h)
	d := dialogue{Focus: "old_session"}
	s := runRemoteTool(t, h, &d, "remote_exec", map[string]any{"node": "n", "command": remoteCommand("printf 'short结果'", "[Console]::Out.Write('short结果')"), "cwd": "project", "wait_ms": 2000})
	if s.Job.Status != "succeeded" || s.Stdout != "short结果" || d.Remote != s.Job.ID || d.Focus != "" {
		t.Fatal(s, d)
	}
	long := runRemoteTool(t, h, &d, "remote_exec", map[string]any{"node": "n", "command": remoteCommand("sleep 1; printf 'after-reconnect'", "Start-Sleep -Seconds 1; [Console]::Out.Write('after-reconnect')"), "cwd": "project", "wait_ms": 0})
	if long.Job.Terminal() {
		t.Fatal("long command blocked dispatch", long)
	}
	// A second call reaches Worker while the previous process still runs.
	r, err := h.call(context.Background(), "u", "n", "host.inspect", "", worker.Arguments{})
	if err != nil || len(r.Data) == 0 {
		t.Fatal(r, err)
	}
	disconnect()
	rr := h.executeTool(context.Background(), "u", &d, "remote_status", protocol.JSON(map[string]any{"job_id": long.Job.ID}))
	if !rr.OK || !strings.Contains(string(protocol.JSON(rr.Data)), `"stale":true`) {
		t.Fatal(rr)
	}
	// Let the process finish with no WSS connection; cancellation of Run is not
	// Worker.Close, and must not own the shell job lifetime.
	time.Sleep(1200 * time.Millisecond)
	reconnect()
	remoteEventually(t, func() bool {
		b, e := h.remoteOwned("u", long.Job.ID)
		return e == nil && b.Notified && b.Snapshot.Job.Status == "succeeded"
	})
	b, err := h.remoteOwned("u", long.Job.ID)
	if err != nil || b.Snapshot.Stdout != "after-reconnect" {
		t.Fatal(b, err)
	}
	if _, err = h.remoteOwned("other", long.Job.ID); err == nil {
		t.Fatal("other chat read job")
	}
	for _, name := range []string{"remote_status", "remote_cancel"} {
		if r := h.executeTool(context.Background(), "other", &dialogue{}, name, protocol.JSON(map[string]string{"job_id": long.Job.ID})); r.OK {
			t.Fatal("cross-owner tool accepted", name)
		}
	}
	before, _ := h.db.List("outbox")
	for i := 0; i < 2; i++ {
		if ack, e := h.remoteEvent("n", b.Snapshot); e != nil || !ack {
			t.Fatal(ack, e)
		}
	}
	after, _ := h.db.List("outbox")
	if len(before) != len(after) {
		t.Fatal("duplicate completion notification")
	}
	if d.Remote != long.Job.ID {
		t.Fatal("notification changed focus")
	}
	job := runRemoteTool(t, h, &d, "remote_exec", map[string]any{"node": "n", "command": remoteCommand("sleep 30", "Start-Sleep -Seconds 30"), "cwd": "project", "wait_ms": 0})
	runRemoteTool(t, h, &d, "remote_cancel", map[string]string{"job_id": job.Job.ID})
	remoteEventually(t, func() bool {
		b, e := h.remoteOwned("u", job.Job.ID)
		return e == nil && b.Snapshot.Job.Status == "cancelled"
	})
}

func TestRemoteCompletionBeforeReceiptAndOwnership(t *testing.T) {
	h := testHub(t)
	j := protocol.RemoteJob{ID: "j", Node: "n", CallID: "c", Status: "starting", Revision: 0}
	if e := h.db.Put("remote_jobs", j.ID, RemoteBinding{Owner: "u", Snapshot: protocol.RemoteSnapshot{Job: j}}); e != nil {
		t.Fatal(e)
	}
	c := pendingCall{Node: "n", Owner: "u", Call: protocol.Call{ID: "c", Tool: "remote.exec", Args: protocol.JSON(protocol.RemoteArgs{JobID: "j"})}}
	if e := h.db.Put("calls", "c", c); e != nil {
		t.Fatal(e)
	}
	if e := h.db.Put("dialogues", "u", dialogue{Remote: "other"}); e != nil {
		t.Fatal(e)
	}
	j.Status, j.Revision = "succeeded", 3
	finished := protocol.RemoteSnapshot{Job: j, Stdout: "complete", NextCursor: "8:0"}
	if ack, e := h.remoteEvent("wrong", finished); e == nil || ack {
		t.Fatal("wrong node accepted")
	}
	wrong := finished
	wrong.Job.CallID = "foreign"
	if ack, e := h.remoteEvent("n", wrong); e == nil || ack {
		t.Fatal("foreign call accepted")
	}
	if ack, e := h.remoteEvent("n", finished); e != nil || !ack {
		t.Fatal(ack, e)
	}
	j.Status, j.Revision = "running", 2
	if e := h.result("n", protocol.OK("c", protocol.RemoteSnapshot{Job: j})); e != nil {
		t.Fatal(e)
	}
	b, e := h.remoteOwned("u", "j")
	if e != nil || b.Snapshot.Job.Status != "succeeded" || b.Snapshot.Stdout != "complete" {
		t.Fatal(b, e)
	}
	var d dialogue
	_ = h.db.Get("dialogues", "u", &d)
	if d.Remote != "other" {
		t.Fatal(d)
	}
	all, _ := h.db.List("outbox")
	if len(all) != 1 {
		t.Fatal(all)
	}
	for _, raw := range all {
		var out delivery
		_ = json.Unmarshal(raw, &out)
		if out.Owner != "u" || out.Output.JobID != "j" {
			t.Fatal(out)
		}
	}
	// Cursor pages carry updated metadata but do not overwrite the first page.
	page := finished
	page.Cursor = "4:0"
	page.Stdout = "lete"
	if e := h.db.Update(func(tx *store.Tx) error { _, e := h.mergeRemote(tx, "n", page); return e }); e != nil {
		t.Fatal(e)
	}
	b, _ = h.remoteOwned("u", "j")
	if b.Snapshot.Stdout != "complete" {
		t.Fatal(b)
	}
}

func TestRemoteArgumentValidationAndUnknown(t *testing.T) {
	h := testHub(t)
	for _, raw := range []string{
		`{"node":"n","cwd":"p","command":"x","wait_ms":2001}`,
		`{"node":"n","cwd":"p","command":"x","wait_ms":"1"}`,
		`{"node":"n","cwd":"p","command":"x","wait_ms":null}`,
		`{"node":"n","cwd":"p","command":"x","timeout_ms":1.5}`,
		`{"node":"n","cwd":"p","command":"x","unexpected":true}`,
	} {
		if r := h.executeTool(context.Background(), "u", &dialogue{}, "remote_exec", json.RawMessage(raw)); r.OK || !strings.Contains(r.Error, "invalid argument") {
			t.Fatal(raw, r)
		}
	}
	if !mutation("remote_exec") || !mutation("remote_cancel") || mutation("remote_status") {
		t.Fatal("mutation guard")
	}
	s := protocol.RemoteSnapshot{Job: protocol.RemoteJob{ID: "unknown", Node: "n", Status: "unknown"}}
	if _, err := remoteReply(s); !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	if err := h.db.Put("remote_jobs", s.Job.ID, RemoteBinding{Owner: "u", Snapshot: s}); err != nil {
		t.Fatal(err)
	}
	if r := h.executeTool(context.Background(), "u", &dialogue{}, "remote_status", protocol.JSON(map[string]string{"job_id": s.Job.ID})); !r.Uncertain || r.Data == nil {
		t.Fatal(r)
	}
	if r := h.executeTool(context.Background(), "other", &dialogue{}, "inventory", protocol.JSON(map[string]any{})); !r.OK || strings.Contains(string(protocol.JSON(r.Data)), `"id":"unknown"`) {
		t.Fatal(r)
	}
}

func TestHubRestartRecoversUnconfirmedShellLaunch(t *testing.T) {
	h := testHub(t)
	j := protocol.RemoteJob{ID: "pending", Node: "n", CallID: "c", Status: "starting"}
	if err := h.db.Put("remote_jobs", j.ID, RemoteBinding{Owner: "u", Snapshot: protocol.RemoteSnapshot{Job: j}}); err != nil {
		t.Fatal(err)
	}
	c := pendingCall{Owner: "u", Node: "n", Call: protocol.Call{ID: "c", Tool: "remote.exec", Args: protocol.JSON(protocol.RemoteArgs{JobID: j.ID})}}
	if err := h.db.Put("calls", "c", c); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(h.c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	b, err := reopened.remoteOwned("u", j.ID)
	if err != nil || b.Snapshot.Job.Status != "unknown" {
		t.Fatal(b, err)
	}
	// A late definitive rejection can resolve this uncertainty without a retry.
	if err := reopened.result("n", protocol.Fail("c", "execution", "cwd not found")); err != nil {
		t.Fatal(err)
	}
	b, err = reopened.remoteOwned("u", j.ID)
	if err != nil || b.Snapshot.Job.Status != "failed" {
		t.Fatal(b, err)
	}
}

func TestRealPiRemoteRuntime(t *testing.T) {
	if os.Getenv("RELAYDOCK_TEST_PI") != "1" {
		t.Skip("set RELAYDOCK_TEST_PI=1 to exercise installed pi SDK")
	}
	h := testHub(t)
	liveShell(t, h)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Messages []fixtureMessage }
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		coordinatorFixture(w, req.Messages)
	}))
	defer model.Close()
	p := PiCoordinator{Config: config.Config{StateDir: t.TempDir(), Model: config.Model{URL: model.URL + "/v1", Model: "coordinator"}}}
	d := dialogue{}
	called := map[string]int{}
	for _, input := range []string{"启动远程长任务", "检查远程任务", "停止远程任务"} {
		inv, err := h.context("u", d)
		if err != nil {
			t.Fatal(err)
		}
		text, err := p.Run(context.Background(), Turn{Owner: "u", Input: input, Context: inv}, func(ctx context.Context, name string, raw json.RawMessage) ToolReply {
			called[name]++
			return h.executeTool(ctx, "u", &d, name, raw)
		})
		if err != nil || strings.Contains(text, "FIXTURE_TOOL_ERROR") {
			t.Fatal(text, err)
		}
	}
	for _, name := range []string{"remote_exec", "remote_status", "remote_cancel"} {
		if called[name] != 1 {
			t.Fatalf("%s: %v", name, called)
		}
	}
	remoteEventually(t, func() bool {
		b, e := h.remoteOwned("u", d.Remote)
		return e == nil && b.Snapshot.Job.Status == "cancelled"
	})
}
