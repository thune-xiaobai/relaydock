package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"relaydock/internal/config"
	"relaydock/internal/protocol"
)

func TestShellOnlyWorkerDedupeAndRestart(t *testing.T) {
	c := config.Config{ID: "shell", Token: strings.Repeat("s", 32), StateDir: t.TempDir(), Workspaces: map[string]string{"project": t.TempDir()}, Shell: config.Shell{Enabled: true}}
	w, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { w.Close() }()
	if h := w.Hello(); h.Shell == nil || len(h.Agents) != 0 || len(h.Tools) != 4 {
		t.Fatal(h)
	}
	cmd := "printf 'once' >> count"
	if runtime.GOOS == "windows" {
		cmd = "[IO.File]::AppendAllText((Join-Path (Get-Location).Path 'count'), 'once')"
	}
	call := protocol.Call{ID: "call_once", Tool: "remote.exec", Args: protocol.JSON(protocol.RemoteArgs{JobID: "once", Command: cmd, CWD: "project"})}
	r := w.Call(context.Background(), call)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if again := w.Call(context.Background(), call); again.Error != nil {
		t.Fatal(again.Error)
	}
	conflict := call
	conflict.Args = protocol.JSON(protocol.RemoteArgs{JobID: "changed", Command: cmd, CWD: "project"})
	if bad := w.Call(context.Background(), conflict); bad.Error == nil || bad.Error.Code != "conflict" {
		t.Fatal(bad)
	}
	until := time.Now().Add(5 * time.Second)
	for {
		s, e := w.shell.Status("once", "", 0)
		if e != nil {
			t.Fatal(e)
		}
		if s.Job.Terminal() {
			break
		}
		if time.Now().After(until) {
			t.Fatal("shell hung")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Emulate lost receipt after process completion; lookup must recover state.
	var rec callRecord
	if err = w.db.Get("calls", call.ID, &rec); err != nil {
		t.Fatal(err)
	}
	rec.Result = nil
	if err = w.db.Put("calls", call.ID, rec); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	w, err = New(c)
	if err != nil {
		t.Fatal(err)
	}
	var s protocol.RemoteSnapshot
	r = w.Lookup(call.ID)
	if r.Error != nil || json.Unmarshal(r.Data, &s) != nil || s.Job.Status != "succeeded" {
		t.Fatal(r, s)
	}
	if again := w.Call(context.Background(), call); again.Error != nil {
		t.Fatal(again.Error)
	}
	text, err := os.ReadFile(filepath.Join(c.Workspaces["project"], "count"))
	if err != nil || string(text) != "once" {
		t.Fatalf("replayed: %s %v", text, err)
	}
}

func TestShellDisabledRejectsDispatch(t *testing.T) {
	w := testWorker(t)
	r := w.Call(context.Background(), protocol.Call{ID: "disabled", Tool: "remote.exec", Args: protocol.JSON(protocol.RemoteArgs{JobID: "j", Command: "ignored", CWD: "project"})})
	if r.Error == nil || r.Error.Code != "disabled" {
		t.Fatal(r)
	}
	if w.Hello().Shell != nil {
		t.Fatal("disabled shell advertised")
	}
}
