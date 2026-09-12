package remote

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"relaydock/internal/config"
	"relaydock/internal/protocol"
	"relaydock/internal/store"
)

func testManager(t *testing.T, shell config.Shell) (*Manager, *store.Store, config.Config) {
	t.Helper()
	c := config.Config{ID: "n", StateDir: t.TempDir(), Workspaces: map[string]string{"project": t.TempDir()}, Shell: shell}
	db, err := store.Open(c.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(c, db)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close(); db.Close() })
	return m, db, c
}
func shellCommand(unix, windows string) string {
	if runtime.GOOS == "windows" {
		return windows
	}
	return unix
}
func startJob(t *testing.T, m *Manager, id, cmd string, timeout int) protocol.RemoteSnapshot {
	t.Helper()
	s, err := m.Start(context.Background(), "call_"+id, protocol.RemoteArgs{JobID: id, Command: cmd, CWD: "project", TimeoutMS: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func waitJob(t *testing.T, m *Manager, id string) protocol.RemoteSnapshot {
	t.Helper()
	end := time.Now().Add(8 * time.Second)
	for time.Now().Before(end) {
		s, err := m.Status(id, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		if s.Job.Terminal() {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not settle: " + id)
	return protocol.RemoteSnapshot{}
}
func TestCommandOutputExitAndWorkingDirectory(t *testing.T) {
	m, _, c := testManager(t, config.Shell{})
	cmd := shellCommand("printf '中文🙂'; printf '问题' >&2; pwd; exit 7", "[Console]::Out.Write('中文🙂'); [Console]::Error.Write('问题'); (Get-Location).Path; exit 7")
	startJob(t, m, "output", cmd, 5000)
	s := waitJob(t, m, "output")
	if s.Job.Status != "failed" || s.Job.ExitCode == nil || *s.Job.ExitCode != 7 || !strings.Contains(s.Stdout, "中文🙂") || s.Stderr != "问题" {
		t.Fatalf("%+v", s)
	}
	real, _ := filepath.EvalSymlinks(c.Workspaces["project"])
	if !strings.Contains(s.Stdout, real) {
		t.Fatalf("cwd not applied: %q", s.Stdout)
	}
	if _, err := m.Start(context.Background(), "another", protocol.RemoteArgs{JobID: "output", Command: cmd, CWD: "project"}); err == nil {
		t.Fatal("job ID replayed")
	}
	for _, a := range []protocol.RemoteArgs{
		{JobID: "../bad", Command: cmd, CWD: "project"}, {JobID: "relative", Command: cmd, CWD: "relative/path"}, {JobID: "timeout", Command: cmd, CWD: "project", TimeoutMS: 86400001},
	} {
		if _, err := m.Start(context.Background(), "invalid", a); err == nil {
			t.Fatalf("accepted %+v", a)
		}
	}
}
func TestUTF8CursorPages(t *testing.T) {
	m, _, _ := testManager(t, config.Shell{})
	want := strings.Repeat("a中文🙂", 40)
	cmd := shellCommand("printf '"+want+"'", "[Console]::Out.Write('"+want+"')")
	startJob(t, m, "unicode", cmd, 5000)
	waitJob(t, m, "unicode")
	cursor, got := "", ""
	for i := 0; i < 300; i++ {
		s, err := m.Status("unicode", cursor, 7)
		if err != nil {
			t.Fatal(err)
		}
		if !utf8.ValidString(s.Stdout) {
			t.Fatal("invalid UTF-8 page")
		}
		got += s.Stdout
		if !s.HasMore {
			break
		}
		if s.NextCursor == cursor {
			t.Fatal("cursor stuck")
		}
		cursor = s.NextCursor
	}
	if got != want {
		t.Fatalf("duplicate/missing output: %q", got)
	}
	for _, cursor := range []string{"bad", "-1:0", "999999:0"} {
		if _, err := m.Status("unicode", cursor, 7); err == nil {
			t.Fatal("bad cursor accepted")
		}
	}
}
func TestOutputLimitDrainsPipes(t *testing.T) {
	m, _, _ := testManager(t, config.Shell{MaxOutputBytes: 64})
	cmd := shellCommand("head -c 131072 /dev/zero; head -c 131072 /dev/zero >&2", "[Console]::Out.Write(('x'*131072)); [Console]::Error.Write(('y'*131072))")
	startJob(t, m, "cap", cmd, 5000)
	s := waitJob(t, m, "cap")
	if s.Job.Status != "succeeded" || !s.Job.Truncated || s.Job.StdoutBytes != 131072 || s.Job.StderrBytes != 131072 || len(s.Stdout) != 64 || len(s.Stderr) != 64 {
		t.Fatalf("%+v", s)
	}
}
func TestConcurrentCancelTimeoutAndDisconnectedContext(t *testing.T) {
	m, _, _ := testManager(t, config.Shell{MaxRunning: 2})
	long := shellCommand("sleep 30", "Start-Sleep -Seconds 30")
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := m.Start(ctx, "call_one", protocol.RemoteArgs{JobID: "one", Command: long, CWD: "project"}); err != nil {
		t.Fatal(err)
	}
	cancel() // The request/WSS context must not own the launched process.
	startJob(t, m, "two", long, 200)
	if _, err := m.Start(context.Background(), "call_three", protocol.RemoteArgs{JobID: "three", Command: long, CWD: "project"}); err == nil {
		t.Fatal("capacity not enforced")
	}
	s, err := m.Status("one", "", 0)
	if err != nil || s.Job.Status != "running" {
		t.Fatal(s, err)
	}
	if _, err = m.Cancel("one"); err != nil {
		t.Fatal(err)
	}
	if s = waitJob(t, m, "one"); s.Job.Status != "cancelled" {
		t.Fatal(s)
	}
	if s = waitJob(t, m, "two"); s.Job.Status != "timed_out" {
		t.Fatal(s)
	}
	if _, err = m.Cancel("one"); err != nil {
		t.Fatal("completed cancel not idempotent", err)
	}
}
func TestShutdownAndRestartNeverReplay(t *testing.T) {
	m, db, c := testManager(t, config.Shell{})
	startJob(t, m, "stopped", shellCommand("sleep 30", "Start-Sleep -Seconds 30"), 5000)
	m.Close()
	if s := waitJob(t, m, "stopped"); s.Job.Status != "cancelled" {
		t.Fatal(s)
	}
	// An interrupted launch journal survives, but its command must not run.
	j := protocol.RemoteJob{ID: "interrupted", Node: "n", CallID: "old", Status: "running", Revision: 2, Command: shellCommand("touch replayed", "New-Item replayed"), CWD: c.Workspaces["project"]}
	if err := db.Put("remote_jobs", j.ID, j); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(c, db)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s, err := reopened.Status(j.ID, "", 0)
	if err != nil || s.Job.Status != "unknown" || s.Job.Revision != 3 {
		t.Fatal(s, err)
	}
	if _, err = reopened.Cancel(j.ID); err == nil {
		t.Fatal("unknown process cancellable")
	}
	if _, err = os.Stat(filepath.Join(j.CWD, "replayed")); !os.IsNotExist(err) {
		t.Fatal("replayed unknown command")
	}
	var event protocol.RemoteSnapshot
	if err = db.Get("remote_outbox", j.ID, &event); err != nil || event.Job.Status != "unknown" {
		t.Fatal(event, err)
	}
	if s, err = reopened.Status("stopped", "", 0); err != nil || s.Job.Status != "cancelled" {
		t.Fatal(s, err)
	}
}
func TestOutputReadFailureStillPersistsTerminalState(t *testing.T) {
	m, db, _ := testManager(t, config.Shell{})
	id := "broken_output"
	if err := os.MkdirAll(filepath.Join(m.dir(id), "stdout"), 0700); err != nil {
		t.Fatal(err)
	}
	j := protocol.RemoteJob{ID: id, Status: "failed", Revision: 3}
	if err := m.saveTerminal(j); err != nil {
		t.Fatal(err)
	}
	if err := db.Get("remote_jobs", id, &j); err != nil || j.Status != "unknown" || !strings.Contains(j.Error, "saved output") {
		t.Fatal(j, err)
	}
}
