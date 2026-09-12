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

	"relaydock/internal/localfile"
	"relaydock/internal/protocol"
)

func muxFixture(t *testing.T, w *Worker) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture uses POSIX sh; Windows requires target psmux validation")
	}
	file := filepath.Join(t.TempDir(), "mux")
	const script = `#!/bin/sh
case "$3" in
new-session) exit 9;;
list-sessions)
  if [ "$RELAYDOCK_MUX_TEST" = broken ]; then echo 'permission denied' >&2; exit 1; fi
  if [ "$RELAYDOCK_MUX_TEST" = no-server ]; then echo 'no server running on /tmp/test-mux' >&2; exit 1; fi
  if [ -n "$RELAYDOCK_MUX_TEST" ]; then printf '%s\n' "$RELAYDOCK_MUX_TEST"; fi;;
kill-session) printf '%s\n' "$5" > "$RELAYDOCK_MUX_KILLED";;
*) exit 4;;
esac
`
	if err := os.WriteFile(file, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	killed := filepath.Join(t.TempDir(), "killed")
	t.Setenv("RELAYDOCK_MUX_KILLED", killed)
	t.Setenv("RELAYDOCK_MUX_TEST", "")
	w.c.Mux, w.c.Namespace, w.c.Backend = file, "testnamespace", "tmux"
	return killed
}

func TestFailedLaunchCanBeExplicitlyClosed(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "never-initialized mux"}[live], func(t *testing.T) {
			w := testWorker(t)
			killed := muxFixture(t, w)
			if err := w.db.Delete("sessions", "s_test"); err != nil {
				t.Fatal(err)
			}
			create := func(id string) protocol.Result {
				return w.Call(context.Background(), protocol.Call{ID: id, Tool: "session.create", Args: protocol.JSON(Arguments{Workspace: "a", Agent: "pi"})})
			}
			r := create("first")
			if r.Error == nil {
				t.Fatal("fixture must fail startup")
			}
			var s protocol.Session
			if err := json.Unmarshal(r.Data, &s); err != nil {
				t.Fatal(err)
			}
			if live {
				t.Setenv("RELAYDOCK_MUX_TEST", s.ID)
			}
			if r := create("while_reserved"); r.Error == nil || r.Error.Code != "workspace_busy" {
				t.Fatal(r)
			}
			closeCall := protocol.Call{ID: "close", Tool: "session.close", Args: protocol.JSON(Arguments{Session: s.ID})}
			if r := w.Call(context.Background(), closeCall); r.Error != nil {
				t.Fatal(r)
			}
			if r := w.Call(context.Background(), closeCall); r.Error != nil {
				t.Fatal("close replay", r)
			}
			if live {
				b, err := os.ReadFile(killed)
				if err != nil || strings.TrimSpace(string(b)) != "="+s.ID {
					t.Fatal(string(b), err)
				}
			} else if _, err := os.Stat(killed); !os.IsNotExist(err) {
				t.Fatal("killed an absent mux")
			}
			if r := create("after_close"); r.Error == nil || r.Error.Code == "workspace_busy" {
				t.Fatal(r)
			}
		})
	}
}

func TestClosePreservesIdentityAndDoesNotConfuseMuxErrorsWithAbsence(t *testing.T) {
	w := testWorker(t)
	killed := muxFixture(t, w)
	var s protocol.Session
	if err := w.db.Get("sessions", "s_test", &s); err != nil {
		t.Fatal(err)
	}
	s.MuxName = s.ID
	if err := w.db.Put("sessions", s.ID, s); err != nil {
		t.Fatal(err)
	}
	close := func(id, instance string) protocol.Result {
		return w.Call(context.Background(), protocol.Call{ID: id, Tool: "session.close", Args: protocol.JSON(Arguments{Session: s.ID, Instance: instance, Native: "native"})})
	}
	t.Setenv("RELAYDOCK_MUX_TEST", s.ID)
	// Retain stale heartbeat identity for cleanup without allowing stale callers.
	if err := localfile.Write(filepath.Join(w.dir(s.ID), "state.json"), protocol.BridgeState{Session: s.ID, Instance: "i_test", Native: "native", Status: "idle", Updated: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	if r := close("stale", "old"); r.Error == nil || r.Error.Code != "stale_target" {
		t.Fatal(r)
	}
	t.Setenv("RELAYDOCK_MUX_TEST", "broken")
	if r := close("broken", "i_test"); r.Error == nil || r.Error.Code != "unknown" {
		t.Fatal(r)
	}
	if s, err := w.inspect(s.ID); err != nil || s.Status == "closed" {
		t.Fatal(s, err)
	}
	// Only a whole-name match is live; do not kill the longer sibling.
	t.Setenv("RELAYDOCK_MUX_TEST", s.ID+"_sibling")
	if r := close("absent", "i_test"); r.Error != nil {
		t.Fatal(r)
	}
	if _, err := os.Stat(killed); !os.IsNotExist(err) {
		t.Fatal("killed unrelated mux")
	}
}

func TestMuxNoServerIsDistinctFromExecutableFailure(t *testing.T) {
	w := testWorker(t)
	muxFixture(t, w)
	t.Setenv("RELAYDOCK_MUX_TEST", "no-server")
	if exists, err := w.muxExists(context.Background(), "s_test"); err != nil || exists {
		t.Fatal(exists, err)
	}
	w.c.Mux = filepath.Join(t.TempDir(), "nonexistent")
	if _, err := w.muxExists(context.Background(), "s_test"); err == nil {
		t.Fatal("missing binary treated as absence")
	}
}

func TestPSMuxRegistryDoesNotMissAnUnresponsiveSession(t *testing.T) {
	w := testWorker(t)
	w.c.Backend, w.c.Namespace = "psmux", "rd"
	dir := t.TempDir()
	t.Setenv("PSMUX_DATA_DIR", dir)
	check := func(want bool) {
		t.Helper()
		got, err := w.muxExists(context.Background(), "s_test")
		if err != nil || got != want {
			t.Fatal(got, err)
		}
	}
	check(false)
	// A .pid or .spawnlock can outlive the port entry. Do not release its
	// directory while psmux might still be starting or unable to answer TCP.
	for _, suffix := range []string{"port", "pid", "spawnlock"} {
		file := filepath.Join(dir, "rd__s_test."+suffix)
		if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		check(true)
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"rd__s_test_sibling.port", "other__s_test.port"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	check(false)
	t.Setenv("PSMUX_DATA_DIR", "relative")
	if _, err := w.muxExists(context.Background(), "s_test"); err == nil {
		t.Fatal("guessed registry from working directory")
	}
}
