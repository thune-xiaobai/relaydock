//go:build windows

package hub

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"relaydock/internal/config"
	"relaydock/internal/protocol"
	"relaydock/internal/transport"
	"relaydock/internal/worker"
)

type windowsTerminalFixture struct {
	h         *Hub
	client    config.Config
	workspace string
	shell     string
	stop      func()
}

func setupWindowsTerminal(t *testing.T, maxRunning int) windowsTerminalFixture {
	t.Helper()
	h := testHub(t)
	h.c.Channels["other"] = config.Peer{Token: strings.Repeat("o", 32)}
	h.c.Workers["evil"] = config.Peer{Token: strings.Repeat("e", 32)}
	server := httptest.NewServer(h.Handler())
	t.Cleanup(server.Close)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	workspace := t.TempDir()
	w, err := worker.New(config.Config{ID: "n", Token: strings.Repeat("w", 32), Hub: url, StateDir: t.TempDir(), Workspaces: map[string]string{"project": workspace}, Shell: config.Shell{Enabled: true, Kind: "powershell", MaxRunning: maxRunning}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	if info := w.Hello().Shell; info == nil || !info.Interactive {
		t.Fatalf("Windows Worker did not advertise ConPTY: %+v", info)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	var once sync.Once
	stop := func() {
		t.Helper()
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(10 * time.Second):
				t.Error("Worker did not stop after terminal disconnect")
			}
		})
	}
	t.Cleanup(stop)
	remoteEventually(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.workers["n"] != nil })
	return windowsTerminalFixture{h: h, client: config.Config{ID: "u", Token: strings.Repeat("c", 32), Hub: url, StateDir: t.TempDir()}, workspace: workspace, shell: w.Hello().Shell.Executable, stop: stop}
}

func windowsTerminalRequest(c config.Config) protocol.TerminalOpen {
	return protocol.TerminalOpen{PeerID: c.ID, Node: "n", CWD: "project", Term: "xterm-256color", TerminalSize: protocol.TerminalSize{Cols: 100, Rows: 24}}
}

func windowsTerminalPeer(t *testing.T, c config.Config) *transport.Peer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := transport.Connect(ctx, c, "shell")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func windowsTerminalRead(t *testing.T, p *transport.Peer) protocol.Message {
	t.Helper()
	_ = p.C.SetReadDeadline(time.Now().Add(8 * time.Second))
	m, err := p.Read()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func windowsTerminalType(t *testing.T, p *transport.Peer, input string) {
	t.Helper()
	if err := p.Send(protocol.Wrap("shell_input", "", protocol.TerminalData{Bytes: []byte(input)})); err != nil {
		t.Fatal(err)
	}
}

func windowsTerminalUntil(t *testing.T, p *transport.Peer, want string) []byte {
	t.Helper()
	var output []byte
	for len(output) < 2<<20 {
		_ = p.C.SetReadDeadline(time.Now().Add(8 * time.Second))
		m, err := p.Read()
		if err != nil {
			t.Fatalf("waiting for %q: %v; output %q", want, err, output)
		}
		if m.Type != "shell_output" {
			t.Fatalf("unexpected frame waiting for %q: %+v; output %q", want, m, output)
		}
		data, err := protocol.Decode[protocol.TerminalData](m)
		if err != nil {
			t.Fatal(err)
		}
		output = append(output, data.Bytes...)
		if bytes.Contains(output, []byte(want)) {
			return output
		}
	}
	t.Fatalf("terminal output exceeded test budget waiting for %q", want)
	return nil
}

func windowsTerminalMarker(t *testing.T, p *transport.Peer, marker string) {
	t.Helper()
	// The expected marker is absent from the input, so echoed commands cannot
	// make a test pass before PowerShell has actually evaluated the command.
	windowsTerminalType(t, p, "[Console]::WriteLine([char]95 + '"+marker+"' + [char]95)\r")
	windowsTerminalUntil(t, p, "_"+marker+"_")
}

func openWindowsTerminal(t *testing.T, c config.Config) *transport.Peer {
	t.Helper()
	p := windowsTerminalPeer(t, c)
	if err := p.Send(protocol.Wrap("shell_open", "", windowsTerminalRequest(c))); err != nil {
		t.Fatal(err)
	}
	if m := windowsTerminalRead(t, p); m.Type != "shell_ready" {
		t.Fatalf("terminal startup: %+v", m)
	}
	// Disable optional interactive editing to make command input independent of
	// the installed PSReadLine version, while retaining the real console host.
	windowsTerminalType(t, p, "function prompt { 'RD_' + 'PROMPT> ' }; Remove-Module PSReadLine -ErrorAction SilentlyContinue\r")
	windowsTerminalUntil(t, p, "RD_PROMPT> ")
	windowsTerminalMarker(t, p, "READY")
	return p
}

func TestWindowsInteractiveTerminalIOResizeInterruptAndChat(t *testing.T) {
	f := setupWindowsTerminal(t, 4)
	chat, err := transport.Dial(context.Background(), f.client, protocol.Hello{Role: "channel", ID: f.client.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer chat.Close()
	p := openWindowsTerminal(t, f.client)
	windowsTerminalType(t, p, "[Console]::WriteLine([char]95 + 'TTY:' + [Console]::IsInputRedirected + ':' + [Console]::IsOutputRedirected + [char]95)\r")
	windowsTerminalUntil(t, p, "_TTY:False:False_")
	if err := os.Mkdir(filepath.Join(f.workspace, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	windowsTerminalType(t, p, "Set-Location sub; $env:RD_TERMINAL_VALUE='kept'\r")
	windowsTerminalType(t, p, "[Console]::WriteLine([char]95 + $env:RD_TERMINAL_VALUE + ':' + (Split-Path -Leaf $PWD.Path) + [char]95)\r")
	windowsTerminalUntil(t, p, "_kept:sub_")
	windowsTerminalMarker(t, p, "中文🙂")
	if err = p.Send(protocol.Wrap("shell_resize", "", protocol.TerminalSize{Cols: 117, Rows: 39})); err != nil {
		t.Fatal(err)
	}
	windowsTerminalType(t, p, "[Console]::WriteLine([char]95 + 'SIZE:' + [Console]::WindowHeight + ':' + [Console]::WindowWidth + [char]95)\r")
	windowsTerminalUntil(t, p, "_SIZE:39:117_")
	windowsTerminalType(t, p, "[Console]::WriteLine([char]95 + 'RUNNING' + [char]95); Start-Sleep -Seconds 30\r")
	windowsTerminalUntil(t, p, "_RUNNING_")
	time.Sleep(100 * time.Millisecond)
	windowsTerminalType(t, p, "\x03")
	windowsTerminalUntil(t, p, "RD_PROMPT> ")
	windowsTerminalMarker(t, p, "INTERRUPTED_SHELL_ALIVE")
	second := openWindowsTerminal(t, f.client)
	windowsTerminalType(t, second, "[Console]::WriteLine([char]95 + 'FRESH:' + [string]::IsNullOrEmpty($env:RD_TERMINAL_VALUE) + [char]95)\r")
	windowsTerminalUntil(t, second, "_FRESH:True_")
	input := protocol.ChatInput{ID: "chat_during_windows_terminal", Text: "local fixture only"}
	if err = chat.Send(protocol.Wrap("chat", input.ID, input)); err != nil {
		t.Fatal(err)
	}
	if m := windowsTerminalRead(t, chat); m.Type != "chat_ack" {
		t.Fatalf("terminal replaced the channel socket: %+v", m)
	}
	windowsTerminalType(t, p, "[Console]::WriteLine([char]95 + 'FINAL' + [char]95); exit 7\r")
	var output []byte
	for len(output) < 2<<20 {
		m := windowsTerminalRead(t, p)
		if m.Type == "shell_exit" {
			result, err := protocol.Decode[protocol.TerminalExit](m)
			if err != nil || result.Code != 7 || result.Error != "" || !bytes.Contains(output, []byte("_FINAL_")) {
				t.Fatalf("final output or exit status lost: %+v %v %q", result, err, output)
			}
			return
		}
		if m.Type != "shell_output" {
			t.Fatalf("unexpected terminal frame: %+v", m)
		}
		data, err := protocol.Decode[protocol.TerminalData](m)
		if err != nil {
			t.Fatal(err)
		}
		output = append(output, data.Bytes...)
	}
	t.Fatal("terminal did not exit within output budget")
}

func TestWindowsTerminalAuthorizationAndProtocolRejection(t *testing.T) {
	f := setupWindowsTerminal(t, 4)
	for _, token := range []string{"wrong", strings.Repeat("w", 32)} {
		c := f.client
		c.Token = token
		if p, err := transport.Connect(context.Background(), c, "shell"); err == nil {
			p.Close()
			t.Fatal("invalid channel credential admitted")
		}
	}
	for _, name := range []string{"wrong_id", "forbidden", "bad_size", "bad_term", "relative_cwd"} {
		t.Run(name, func(t *testing.T) {
			c := f.client
			o := windowsTerminalRequest(c)
			switch name {
			case "wrong_id":
				o.PeerID = "other"
			case "forbidden":
				c.ID, c.Token, o.PeerID = "other", strings.Repeat("o", 32), "other"
			case "bad_size":
				o.Cols = 0
			case "bad_term":
				o.Term = "xterm\nINJECT=1"
			case "relative_cwd":
				o.CWD = "relative/path"
			}
			p := windowsTerminalPeer(t, c)
			if err := p.Send(protocol.Wrap("shell_open", "", o)); err != nil {
				t.Fatal(err)
			}
			if m := windowsTerminalRead(t, p); m.Type != "shell_exit" {
				t.Fatalf("invalid request admitted: %+v", m)
			}
		})
	}
	p := openWindowsTerminal(t, f.client)
	f.h.mu.Lock()
	var id string
	for key := range f.h.terminals {
		id = key
	}
	f.h.mu.Unlock()
	for _, token := range []string{strings.Repeat("e", 32), strings.Repeat("w", 32)} {
		c := f.client
		c.Token = token
		rogue, err := transport.Connect(context.Background(), c, "shell/worker")
		if err != nil {
			t.Fatal(err)
		}
		_ = rogue.Send(protocol.Wrap("shell_join", id, nil))
		_ = rogue.C.SetReadDeadline(time.Now().Add(3 * time.Second))
		if m, err := rogue.Read(); err == nil {
			rogue.Close()
			t.Fatalf("wrong Worker or duplicate stream accepted: %+v", m)
		}
		rogue.Close()
	}
	windowsTerminalMarker(t, p, "ORIGINAL_INTACT")
	_ = p.Send(protocol.Wrap("shell_input", "", protocol.TerminalData{Bytes: make([]byte, protocol.TerminalChunk+1)}))
	remoteEventually(t, func() bool { f.h.mu.Lock(); defer f.h.mu.Unlock(); return len(f.h.terminals) == 0 })
}

func TestWindowsTerminalFullExitStatus(t *testing.T) {
	f := setupWindowsTerminal(t, 4)
	for _, tc := range []struct {
		command string
		want    int64
	}{
		{"exit 3010\r", 3010},
		{"[Environment]::Exit(-1)\r", 1<<32 - 1},
	} {
		t.Run(strconv.FormatInt(tc.want, 10), func(t *testing.T) {
			p := openWindowsTerminal(t, f.client)
			windowsTerminalType(t, p, tc.command)
			for i := 0; i < 128; i++ {
				m := windowsTerminalRead(t, p)
				if m.Type == "shell_exit" {
					result, err := protocol.Decode[protocol.TerminalExit](m)
					if err != nil || result.Code != tc.want || result.Error != "" {
						t.Fatalf("Windows exit status truncated: %+v %v", result, err)
					}
					return
				}
				if m.Type != "shell_output" {
					t.Fatalf("unexpected terminal frame: %+v", m)
				}
			}
			t.Fatal("terminal did not exit within frame budget")
		})
	}
}

func TestWindowsTerminalCapacitySlowClientAndDisconnect(t *testing.T) {
	f := setupWindowsTerminal(t, 1)
	p := openWindowsTerminal(t, f.client)
	q := windowsTerminalPeer(t, f.client)
	_ = q.Send(protocol.Wrap("shell_open", "", windowsTerminalRequest(f.client)))
	m := windowsTerminalRead(t, q)
	result, err := protocol.Decode[protocol.TerminalExit](m)
	if err != nil || m.Type != "shell_exit" || !strings.Contains(result.Error, "capacity") {
		t.Fatalf("capacity not enforced: %+v %v", m, err)
	}
	windowsTerminalType(t, p, "[IO.File]::WriteAllText((Join-Path $PWD 'outer.pid'), [string]$PID)\r")
	windowsTerminalMarker(t, p, "PID_WRITTEN")
	pidBytes, err := os.ReadFile(filepath.Join(f.workspace, "outer.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.ParseUint(string(pidBytes), 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(process)
	windowsTerminalType(t, p, "[Console]::WriteLine([char]95 + 'FLOOD_STARTED' + [char]95); while ($true) { [Console]::Write(('RD_FLOOD' * 4096)) }\r")
	windowsTerminalUntil(t, p, "_FLOOD_STARTED_")
	time.Sleep(250 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := f.h.call(ctx, "u", "n", "host.inspect", "fixture", struct{}{})
	if err != nil || r.Error != nil {
		t.Fatalf("terminal output blocked Worker control: %+v %v", r, err)
	}
	p.Close()
	remoteEventually(t, func() bool {
		state, err := windows.WaitForSingleObject(process, 0)
		return err == nil && state == windows.WAIT_OBJECT_0
	})
	remoteEventually(t, func() bool {
		candidate, err := transport.Connect(context.Background(), f.client, "shell")
		if err != nil {
			return false
		}
		defer candidate.Close()
		_ = candidate.C.SetReadDeadline(time.Now().Add(3 * time.Second))
		_ = candidate.Send(protocol.Wrap("shell_open", "", windowsTerminalRequest(f.client)))
		m, err := candidate.Read()
		return err == nil && m.Type == "shell_ready"
	})
}

func TestWindowsTerminalUnsupportedAndAbandonedStartup(t *testing.T) {
	h := testHub(t)
	server := httptest.NewServer(h.Handler())
	defer server.Close()
	c := config.Config{ID: "u", Token: strings.Repeat("c", 32), Hub: "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"}
	wc := c
	wc.ID, wc.Token = "n", strings.Repeat("w", 32)
	for _, interactive := range []bool{false, true} {
		w, err := transport.Dial(context.Background(), wc, protocol.Hello{Role: "worker", ID: "n", Shell: &protocol.ShellInfo{Interactive: interactive, MaxRunning: 1}})
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		client := windowsTerminalPeer(t, c)
		_ = client.Send(protocol.Wrap("shell_open", "", windowsTerminalRequest(c)))
		if !interactive {
			if m := windowsTerminalRead(t, client); m.Type != "shell_exit" {
				t.Fatalf("unsupported terminal admitted: %+v", m)
			}
			w.Close()
			remoteEventually(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.workers["n"] == nil })
			continue
		}
		open := windowsTerminalRead(t, w)
		if open.Type != "shell_open" {
			t.Fatalf("missing terminal request: %+v", open)
		}
		client.Close()
		remoteEventually(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.terminals) == 0 })
		late, err := transport.Connect(context.Background(), wc, "shell/worker")
		if err != nil {
			t.Fatal(err)
		}
		defer late.Close()
		_ = late.Send(protocol.Wrap("shell_join", open.ID, nil))
		_ = late.C.SetReadDeadline(time.Now().Add(3 * time.Second))
		if m, err := late.Read(); err == nil {
			t.Fatalf("abandoned startup accepted a late Worker: %+v", m)
		}
	}
}

func TestWindowsTerminalDisconnectPreservesPsmux(t *testing.T) {
	psmux, err := exec.LookPath("psmux")
	if err != nil {
		t.Skip("psmux is not installed")
	}
	// A Worker started inside an existing multiplexer inherits these markers.
	// Each remote terminal must start outside that parent's pane so it can
	// create and attach its own psmux session without forcing nested sessions.
	inherited := map[string]string{
		"PSMUX_SESSION":        "relaydock_parent_session",
		"PSMUX_ACTIVE":         "1",
		"PSMUX_SESSION_NAME":   "relaydock_parent_session",
		"PSMUX_REMOTE_ATTACH":  "1",
		"PSMUX_TARGET_SESSION": "relaydock_parent_session",
		"PSMUX_TARGET_FULL":    "relaydock_parent_namespace__session",
		"TMUX":                 "relaydock_parent_socket,123,0",
		"TMUX_PANE":            "%43",
	}
	for name, value := range inherited {
		t.Setenv(name, value)
	}
	t.Cleanup(func() {
		for name, want := range inherited {
			if got := os.Getenv(name); got != want {
				t.Errorf("Worker environment changed: %s = %q, want %q", name, got, want)
			}
		}
	})
	f := setupWindowsTerminal(t, 4)
	ns := "rdtty_" + protocol.ID("")[:12]
	configPath := filepath.Join(t.TempDir(), "psmux.conf")
	if err := os.WriteFile(configPath, []byte("set -g warm off\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runMux := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, psmux, args...)
		cmd.WaitDelay = time.Second
		return cmd.CombinedOutput()
	}
	mux := func(args ...string) ([]byte, error) {
		return runMux(append([]string{"-L", ns}, args...)...)
	}
	t.Cleanup(func() {
		// Use the complete session name: psmux 3.3.3 can acknowledge a kill with
		// -L without stopping the target. Warm sessions are disabled above.
		if out, err := runMux("-t", ns+"__persist", "kill-session"); err != nil {
			t.Logf("psmux fixture cleanup: %v: %s", err, out)
		}
		// psmux acknowledges shutdown before all pane/server processes exit.
		// Wait until no process keeps this working directory open without delete
		// sharing, otherwise Windows rejects the fixture's TempDir cleanup.
		path, err := windows.UTF16PtrFromString(f.workspace)
		if err != nil {
			t.Error(err)
			return
		}
		remoteEventually(t, func() bool {
			h, err := windows.CreateFile(path, windows.DELETE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
			if err != nil {
				return false
			}
			windows.CloseHandle(h)
			return true
		})
	})
	p := openWindowsTerminal(t, f.client)
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	init := "$env:RD_MUX_SESSION = '" + ns + "'"
	windowsTerminalType(t, p, fmt.Sprintf("& %s -L %s -f %s new-session -d -s persist -- %s -NoLogo -NoProfile -NoExit -Command %s; [Console]::WriteLine([char]95 + 'MUX_CREATED' + [char]95)\r", quote(psmux), ns, quote(configPath), quote(f.shell), quote(init)))
	createdOutput := windowsTerminalUntil(t, p, "_MUX_CREATED_")
	if out, err := mux("has-session", "-t", "persist"); err != nil {
		t.Fatalf("psmux did not start inside ConPTY: %v: %s; terminal output %q", err, out, createdOutput)
	}
	p.Close()
	remoteEventually(t, func() bool { f.h.mu.Lock(); defer f.h.mu.Unlock(); return len(f.h.terminals) == 0 })
	if out, err := mux("has-session", "-t", "persist"); err != nil {
		t.Fatalf("psmux died with terminal socket: %v: %s", err, out)
	}
	q := openWindowsTerminal(t, f.client)
	windowsTerminalType(t, q, fmt.Sprintf("& %s -L %s attach-session -t persist\r", quote(psmux), ns))
	time.Sleep(250 * time.Millisecond)
	// Only the existing mux pane owns this value. An attach failure that returns
	// to the outer shell must not masquerade as a successful reconnect.
	windowsTerminalType(t, q, "[Console]::WriteLine([char]95 + $env:RD_MUX_SESSION + [char]95)\r")
	windowsTerminalUntil(t, q, "_"+ns+"_")
	f.stop()
	remoteEventually(t, func() bool { f.h.mu.Lock(); defer f.h.mu.Unlock(); return len(f.h.terminals) == 0 })
	if out, err := mux("has-session", "-t", "persist"); err != nil {
		t.Fatalf("psmux died with Worker control connection: %v: %s", err, out)
	}
}
