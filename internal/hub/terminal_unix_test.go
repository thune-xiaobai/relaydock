//go:build linux || darwin

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"relaydock/internal/config"
	"relaydock/internal/protocol"
	"relaydock/internal/transport"
	"relaydock/internal/worker"
)

type terminalFixture struct {
	h         *Hub
	client    config.Config
	workspace string
	stop      func()
}

func setupTerminal(t *testing.T, maxRunning ...int) terminalFixture {
	t.Helper()
	h := testHub(t)
	// These identities must not be able to open/join n's terminal streams.
	h.c.Channels["other"] = config.Peer{Token: strings.Repeat("o", 32)}
	h.c.Workers["evil"] = config.Peer{Token: strings.Repeat("e", 32)}
	server := httptest.NewServer(h.Handler())
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	workspace := t.TempDir()
	shellPath := filepath.Join(t.TempDir(), "bash-fixture")
	if err := os.WriteFile(shellPath, []byte("#!/bin/sh\nPS1='RD_PROMPT> '; export PS1; exec /bin/bash --noprofile --norc \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	limit := 4
	if len(maxRunning) > 0 {
		limit = maxRunning[0]
	}
	w, err := worker.New(config.Config{ID: "n", Token: strings.Repeat("w", 32), Hub: url, StateDir: t.TempDir(), Workspaces: map[string]string{"project": workspace}, Shell: config.Shell{Enabled: true, Kind: "sh", Executable: shellPath, MaxRunning: limit}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if e := w.Run(ctx); e != nil {
			t.Error(e)
		}
	}()
	stop := func() { cancel(); <-done }
	t.Cleanup(func() { stop(); w.Close(); server.Close() })
	remoteEventually(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.workers["n"] != nil })
	return terminalFixture{h: h, client: config.Config{ID: "u", Token: strings.Repeat("c", 32), Hub: url, StateDir: t.TempDir()}, workspace: workspace, stop: stop}
}

func terminalRequest(c config.Config) protocol.TerminalOpen {
	return protocol.TerminalOpen{PeerID: c.ID, Node: "n", CWD: "project", Term: "xterm-256color", TerminalSize: protocol.TerminalSize{Cols: 80, Rows: 24}}
}
func openTestTerminal(t *testing.T, c config.Config, open protocol.TerminalOpen) *transport.Peer {
	t.Helper()
	p, err := transport.Connect(context.Background(), c, "shell")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if err = p.Send(protocol.Wrap("shell_open", "", open)); err != nil {
		t.Fatal(err)
	}
	_ = p.C.SetReadDeadline(time.Now().Add(5 * time.Second))
	m, err := p.Read()
	if err != nil || m.Type != "shell_ready" {
		t.Fatalf("startup: %+v %v", m, err)
	}
	return p
}
func typeTerminal(t *testing.T, p *transport.Peer, text string) {
	t.Helper()
	if e := p.Send(protocol.Wrap("shell_input", "", protocol.TerminalData{Bytes: []byte(text)})); e != nil {
		t.Fatal(e)
	}
}
func terminalUntil(t *testing.T, p *transport.Peer, want []byte) []byte {
	t.Helper()
	var all []byte
	for len(all) < 2<<20 {
		_ = p.C.SetReadDeadline(time.Now().Add(5 * time.Second))
		m, err := p.Read()
		if err != nil {
			t.Fatalf("waiting for %q: %v; output %q", want, err, all)
		}
		if m.Type != "shell_output" {
			t.Fatalf("unexpected frame waiting for %q: %+v", want, m)
		}
		b, err := protocol.Decode[protocol.TerminalData](m)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, b.Bytes...)
		if bytes.Contains(all, want) {
			return all
		}
	}
	t.Fatal("terminal output exceeded test budget")
	return nil
}
func terminalMarker(t *testing.T, p *transport.Peer, marker string) {
	t.Helper()
	typeTerminal(t, p, "printf '\\137%s\\137\\n' '"+marker+"'\r")
	terminalUntil(t, p, []byte("_"+marker+"_"))
}

func TestInteractiveTerminalIOResizeInterruptAndChat(t *testing.T) {
	f := setupTerminal(t)
	chat, err := transport.Dial(context.Background(), f.client, protocol.Hello{Role: "channel", ID: f.client.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer chat.Close()
	p := openTestTerminal(t, f.client, terminalRequest(f.client))
	typeTerminal(t, p, "stty -echo\r")
	typeTerminal(t, p, "test -t 0 && test -t 1 && test -t 2 && printf '\\137%s\\137\\n' REALTTY\r")
	terminalUntil(t, p, []byte("_REALTTY_"))
	if err = os.Mkdir(filepath.Join(f.workspace, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	typeTerminal(t, p, "cd sub; export RD_VALUE=kept\r")
	typeTerminal(t, p, "printf '\\137%s:%s\\137\\n' \"$RD_VALUE\" \"${PWD##*/}\"\r")
	terminalUntil(t, p, []byte("_kept:sub_"))
	terminalMarker(t, p, "中文🙂")
	if err = p.Send(protocol.Wrap("shell_resize", "", protocol.TerminalSize{Cols: 117, Rows: 39})); err != nil {
		t.Fatal(err)
	}
	typeTerminal(t, p, "printf '\\137%s\\137\\n' \"$(stty size)\"\r")
	terminalUntil(t, p, []byte("_39 117_"))
	typeTerminal(t, p, "printf '\\377\\000\\033[31mX\\033[0m'\r")
	terminalUntil(t, p, []byte{255, 0, 27, '[', '3', '1', 'm', 'X', 27, '[', '0', 'm'})
	typeTerminal(t, p, "printf '\\137%s\\137\\n' RUNNING; sleep 30\r")
	terminalUntil(t, p, []byte("_RUNNING_"))
	time.Sleep(100 * time.Millisecond)
	typeTerminal(t, p, "\x03")
	terminalMarker(t, p, "INTERRUPTED_SHELL_ALIVE")
	// Tab completion is handled by bash, not by the transport or local client.
	if err = os.WriteFile(filepath.Join(f.workspace, "sub", "unique_completion_name"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	typeTerminal(t, p, "printf '\\137%s\\137\\n' unique_compl\t\r")
	terminalUntil(t, p, []byte("_unique_completion_name_"))
	second := openTestTerminal(t, f.client, terminalRequest(f.client))
	defer second.Close()
	typeTerminal(t, second, "printf '\\137%s\\137\\n' \"${RD_VALUE-unset}\"\r")
	terminalUntil(t, second, []byte("_unset_"))
	// Opening terminals with the same identity must not replace the chat socket.
	in := protocol.ChatInput{ID: "chat_during_terminal", Text: "local fixture only"}
	if err = chat.Send(protocol.Wrap("chat", in.ID, in)); err != nil {
		t.Fatal(err)
	}
	m, err := chat.Read()
	if err != nil || m.Type != "chat_ack" {
		t.Fatal(m, err)
	}
	typeTerminal(t, p, "printf '\\137%s\\137\\n' FINAL; exit 7\r")
	var output []byte
	for {
		_ = p.C.SetReadDeadline(time.Now().Add(5 * time.Second))
		m, err = p.Read()
		if err != nil {
			t.Fatal(err)
		}
		if m.Type == "shell_exit" {
			e, _ := protocol.Decode[protocol.TerminalExit](m)
			if e.Code != 7 || !bytes.Contains(output, []byte("_FINAL_")) {
				t.Fatal(e, string(output))
			}
			break
		}
		b, _ := protocol.Decode[protocol.TerminalData](m)
		output = append(output, b.Bytes...)
	}
}

func TestTerminalAuthorizationAndProtocolRejection(t *testing.T) {
	f := setupTerminal(t)
	for _, token := range []string{"wrong", strings.Repeat("w", 32)} {
		c := f.client
		c.Token = token
		if p, err := transport.Connect(context.Background(), c, "shell"); err == nil {
			p.Close()
			t.Fatal("invalid client identity admitted")
		}
	}
	for _, name := range []string{"wrong_id", "forbidden", "bad_size", "bad_term", "relative_cwd"} {
		t.Run(name, func(t *testing.T) {
			c := f.client
			o := terminalRequest(c)
			switch name {
			case "wrong_id":
				o.PeerID = "other"
			case "forbidden":
				c.ID = "other"
				c.Token = strings.Repeat("o", 32)
				o.PeerID = "other"
			case "bad_size":
				o.Cols = 0
			case "bad_term":
				o.Term = "xterm\nINJECT=1"
			case "relative_cwd":
				o.CWD = "relative/path"
			}
			p, e := transport.Connect(context.Background(), c, "shell")
			if e != nil {
				t.Fatal(e)
			}
			defer p.Close()
			_ = p.Send(protocol.Wrap("shell_open", "", o))
			_ = p.C.SetReadDeadline(time.Now().Add(5 * time.Second))
			m, e := p.Read()
			if e != nil || m.Type != "shell_exit" {
				t.Fatal(m, e)
			}
		})
	}
	p := openTestTerminal(t, f.client, terminalRequest(f.client))
	f.h.mu.Lock()
	id := ""
	for key := range f.h.terminals {
		id = key
	}
	f.h.mu.Unlock()
	for _, token := range []string{strings.Repeat("e", 32), strings.Repeat("w", 32)} {
		c := f.client
		c.Token = token
		rogue, e := transport.Connect(context.Background(), c, "shell/worker")
		if e != nil {
			t.Fatal(e)
		}
		_ = rogue.Send(protocol.Wrap("shell_join", id, nil))
		_ = rogue.C.SetReadDeadline(time.Now().Add(time.Second))
		if m, e := rogue.Read(); e == nil {
			t.Fatalf("wrong worker/duplicate stream accepted: %+v", m)
		}
		rogue.Close()
	}
	terminalMarker(t, p, "ORIGINAL_INTACT")
	_ = p.Send(protocol.Wrap("shell_input", "", protocol.TerminalData{Bytes: make([]byte, protocol.TerminalChunk+1)}))
	remoteEventually(t, func() bool { f.h.mu.Lock(); defer f.h.mu.Unlock(); return len(f.h.terminals) == 0 })
}

func TestTerminalDisconnectPreservesTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux needed for survival verification")
	}
	f := setupTerminal(t)
	ns := "rdtty_" + protocol.ID("")[:12]
	defer exec.Command("tmux", "-L", ns, "kill-session", "-t", "persist").Run()
	p := openTestTerminal(t, f.client, terminalRequest(f.client))
	typeTerminal(t, p, "tmux -L "+ns+" new-session -d -s persist\r")
	terminalMarker(t, p, "MUX_CREATED")
	p.Close()
	remoteEventually(t, func() bool { f.h.mu.Lock(); defer f.h.mu.Unlock(); return len(f.h.terminals) == 0 })
	if out, err := exec.Command("tmux", "-L", ns, "has-session", "-t", "persist").CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	// Attach from a fresh plain shell, just as after reconnecting with SSH.
	q := openTestTerminal(t, f.client, terminalRequest(f.client))
	typeTerminal(t, q, "tmux -L "+ns+" attach-session -t persist\r")
	time.Sleep(100 * time.Millisecond)
	terminalMarker(t, q, "INSIDE_MUX")
	f.stop() // Loss of the Worker control connection also closes the PTY bridge.
	remoteEventually(t, func() bool { f.h.mu.Lock(); defer f.h.mu.Unlock(); return len(f.h.terminals) == 0 })
	if out, err := exec.Command("tmux", "-L", ns, "has-session", "-t", "persist").CombinedOutput(); err != nil {
		t.Fatal("mux died with Worker connection", string(out), err)
	}
}

func TestTerminalCapacityAndSlowClientCleanup(t *testing.T) {
	f := setupTerminal(t, 1)
	p := openTestTerminal(t, f.client, terminalRequest(f.client))
	q, err := transport.Connect(context.Background(), f.client, "shell")
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	_ = q.Send(protocol.Wrap("shell_open", "", terminalRequest(f.client)))
	_ = q.C.SetReadDeadline(time.Now().Add(3 * time.Second))
	m, err := q.Read()
	e, _ := protocol.Decode[protocol.TerminalExit](m)
	if err != nil || m.Type != "shell_exit" || !strings.Contains(e.Error, "capacity") {
		t.Fatal("capacity not enforced", m, err)
	}
	// Stop reading output. Once socket buffers fill, only the terminal data
	// connection may stall; the Worker must still answer control requests.
	typeTerminal(t, p, "printf '%s' \"$$\" > outer.pid; yes RD_FLOOD\r")
	pid := 0
	remoteEventually(t, func() bool {
		b, _ := os.ReadFile(filepath.Join(f.workspace, "outer.pid"))
		pid, _ = strconv.Atoi(string(b))
		return pid > 0
	})
	time.Sleep(250 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := f.h.call(ctx, "u", "n", "host.inspect", "fixture", struct{}{})
	if err != nil || r.Error != nil {
		t.Fatal("terminal output blocked Worker control", r, err)
	}
	p.Close()
	remoteEventually(t, func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH })
	// The released slot must admit a fresh shell. Briefly wait for both the
	// Hub's bridge and Worker's PTY teardown, which finish independently.
	remoteEventually(t, func() bool {
		candidate, err := transport.Connect(context.Background(), f.client, "shell")
		if err != nil {
			return false
		}
		defer candidate.Close()
		_ = candidate.C.SetReadDeadline(time.Now().Add(3 * time.Second))
		_ = candidate.Send(protocol.Wrap("shell_open", "", terminalRequest(f.client)))
		m, err := candidate.Read()
		return err == nil && m.Type == "shell_ready"
	})
}

func TestTerminalAbandonedStartupCannotJoin(t *testing.T) {
	h := testHub(t)
	server := httptest.NewServer(h.Handler())
	defer server.Close()
	c := config.Config{ID: "u", Token: strings.Repeat("c", 32), Hub: "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"}
	wc := c
	wc.ID, wc.Token = "n", strings.Repeat("w", 32)
	w, err := transport.Dial(context.Background(), wc, protocol.Hello{Role: "worker", ID: "n", Shell: &protocol.ShellInfo{Interactive: false, MaxRunning: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	client, err := transport.Connect(context.Background(), c, "shell")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.Send(protocol.Wrap("shell_open", "", terminalRequest(c)))
	_ = client.C.SetReadDeadline(time.Now().Add(3 * time.Second))
	m, err := client.Read()
	if err != nil || m.Type != "shell_exit" {
		t.Fatal("unsupported terminal admitted", m, err)
	}
	w.Close()
	w, err = transport.Dial(context.Background(), wc, protocol.Hello{Role: "worker", ID: "n", Shell: &protocol.ShellInfo{Interactive: true, MaxRunning: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	client, err = transport.Connect(context.Background(), c, "shell")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.Send(protocol.Wrap("shell_open", "", terminalRequest(c)))
	_ = w.C.SetReadDeadline(time.Now().Add(3 * time.Second))
	open, err := w.Read()
	if err != nil || open.Type != "shell_open" {
		t.Fatal(open, err)
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
		t.Fatal("abandoned startup accepted late Worker", m)
	}
}

func TestShellCLIInRealLocalTTY(t *testing.T) {
	f := setupTerminal(t)
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "relaydock")
	build := exec.Command("go", "build", "-o", bin, "./cmd/relaydock")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	file := filepath.Join(t.TempDir(), "client.json")
	raw, _ := json.Marshal(f.client)
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"exit", "escape", "disconnect", "sigterm"} {
		t.Run(mode, func(t *testing.T) {
			// Parent shell stays alive long enough to compare tty modes after the
			// real CLI exits, including errors. Arguments never enter shell source.
			script := `before=$(stty -g); exec 3<&0; "$1" shell --config "$2" --node n --cwd project <&3 & child=$!; printf 'CLI_PID=%s\n' "$child"; wait "$child"; code=$?; after=$(stty -g); printf '\nTTY_BEFORE=%s\nTTY_AFTER=%s\nLOCAL_RESTORED_%s\n' "$before" "$after" "$code"; exit "$code"`
			cmd := exec.Command("/bin/sh", "-c", script, "fixture", bin, file)
			cmd.Env = append(os.Environ(), "TERM=xterm-256color")
			master, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 80, Rows: 24})
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			var mu sync.Mutex
			var output bytes.Buffer
			readDone := make(chan struct{})
			go func() {
				defer close(readDone)
				buf := make([]byte, 4096)
				for {
					n, e := master.Read(buf)
					mu.Lock()
					output.Write(buf[:n])
					mu.Unlock()
					if e != nil {
						return
					}
				}
			}()
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			waitText := func(text string) {
				t.Helper()
				deadline := time.Now().Add(8 * time.Second)
				for time.Now().Before(deadline) {
					mu.Lock()
					s := output.String()
					mu.Unlock()
					if strings.Contains(s, text) {
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
				mu.Lock()
				s := output.String()
				mu.Unlock()
				t.Fatalf("waiting %q, got %q", text, s)
			}
			// Wait for the remote prompt, then confirm a roundtrip.
			waitText("RD_PROMPT> ")
			_, _ = master.Write([]byte("printf '\\137%s\\137\\n' CLI_READY\r"))
			waitText("_CLI_READY_")
			if err = pty.Setsize(master, &pty.Winsize{Cols: 103, Rows: 37}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
			_, _ = master.Write([]byte("printf '\\137%s\\137\\n' \"$(stty size)\"\r"))
			waitText("_37 103_")
			switch mode {
			case "exit":
				_, _ = master.Write([]byte("exit 9\r"))
				waitText("LOCAL_RESTORED_9")
			case "escape":
				_, _ = master.Write([]byte("~."))
				waitText("LOCAL_RESTORED_0")
			case "disconnect":
				f.h.mu.Lock()
				for _, b := range f.h.terminals {
					b.close()
				}
				f.h.mu.Unlock()
				waitText("LOCAL_RESTORED_1")
			case "sigterm":
				mu.Lock()
				match := regexp.MustCompile(`CLI_PID=(\d+)`).FindStringSubmatch(output.String())
				mu.Unlock()
				if len(match) != 2 {
					t.Fatal("missing CLI pid")
				}
				pid, _ := strconv.Atoi(match[1])
				child, _ := os.FindProcess(pid)
				if err := child.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				waitText("LOCAL_RESTORED_1")
			}
			select {
			case err := <-done:
				if mode == "escape" && err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("CLI did not exit")
			}
			<-readDone
			mu.Lock()
			modes := regexp.MustCompile(`TTY_(?:BEFORE|AFTER)=(\S+)`).FindAllStringSubmatch(output.String(), -1)
			mu.Unlock()
			if len(modes) != 2 || normalizeTTYMode(modes[0][1]) != normalizeTTYMode(modes[1][1]) {
				t.Fatalf("terminal not restored: %v", modes)
			}
			remoteEventually(t, func() bool { f.h.mu.Lock(); defer f.h.mu.Unlock(); return len(f.h.terminals) == 0 })
		})
	}
}

func normalizeTTYMode(mode string) string {
	if runtime.GOOS != "darwin" {
		return mode
	}
	// Darwin sets PENDIN when ICANON is restored. It describes queued input
	// awaiting retyping, not a console setting; stty -pendin cannot clear it.
	return regexp.MustCompile(`lflag=([0-9a-f]+)`).ReplaceAllStringFunc(mode, func(flag string) string {
		n, _ := strconv.ParseUint(strings.TrimPrefix(flag, "lflag="), 16, 64)
		return "lflag=" + strconv.FormatUint(n&^0x20000000, 16)
	})
}
