//go:build windows

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"relaydock/internal/config"
	"relaydock/internal/protocol"
	"relaydock/internal/transport"
	"relaydock/internal/worker"
)

type psmuxWorkerFixture struct {
	Config config.Config
	Status string
	Stop   string
	Done   string
	Marker string
}

// This helper actually runs in the psmux pane being attached. Its output goes
// to that pane, whereas the Worker's terminal traffic must use separate pipes.
func TestWindowsPsmuxWorkerHelper(t *testing.T) {
	path := os.Getenv("RD_PSMUX_WORKER_FIXTURE")
	if path == "" {
		t.Skip("subprocess helper")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f psmuxWorkerFixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.WriteFile(f.Done, []byte("stopped"), 0600) }()
	status := map[string]string{"PSMUX_SESSION": os.Getenv("PSMUX_SESSION"), "TMUX": os.Getenv("TMUX")}
	if err := os.WriteFile(f.Status, protocol.JSON(status), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := worker.New(f.Config)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for n := 0; ; n++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return
		case <-ticker.C:
			if _, err := os.Stat(f.Stop); err == nil {
				cancel()
			} else {
				fmt.Printf("%s:%08d\r\n", f.Marker, n)
			}
		}
	}
}

func TestWindowsTerminalAttachesWorkerOwnPsmuxSession(t *testing.T) {
	psmux, err := exec.LookPath("psmux")
	if err != nil {
		t.Skip("psmux is not installed")
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Fatal(err)
	}
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(t)
	server := httptest.NewServer(h.Handler())
	t.Cleanup(server.Close)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	dir := t.TempDir()
	ns := "rdloop_" + protocol.ID("")[:12]
	target := ns + "__worker"
	f := psmuxWorkerFixture{
		Config: config.Config{ID: "n", Token: strings.Repeat("w", 32), Hub: url, StateDir: filepath.Join(dir, "state"), Workspaces: map[string]string{"project": dir}, Shell: config.Shell{Enabled: true, Kind: "powershell", MaxRunning: 4}},
		Status: filepath.Join(dir, "inherited.json"), Stop: filepath.Join(dir, "stop"), Done: filepath.Join(dir, "done"), Marker: "RD_PANE_" + ns,
	}
	fixturePath := filepath.Join(dir, "fixture.json")
	configPath := filepath.Join(dir, "psmux.conf")
	scriptPath := filepath.Join(dir, "worker.ps1")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	for path, data := range map[string][]byte{
		fixturePath: protocol.JSON(f),
		configPath:  []byte("set -g warm off\n"),
		scriptPath:  []byte("$env:RD_PSMUX_WORKER_FIXTURE = " + quote(fixturePath) + "\r\n& " + quote(helper) + " '-test.run=^TestWindowsPsmuxWorkerHelper$' '-test.timeout=75s'\r\nif ($LASTEXITCODE -ne 0) { Start-Sleep -Seconds 30 }\r\nexit $LASTEXITCODE\r\n"),
	} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	runMux := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, psmux, args...)
		cmd.WaitDelay = time.Second
		return cmd.CombinedOutput()
	}
	t.Cleanup(func() {
		_ = os.WriteFile(f.Stop, []byte("stop"), 0600)
		if _, err := os.Stat(f.Status); err == nil {
			stopped := false
			for until := time.Now().Add(5 * time.Second); time.Now().Before(until); time.Sleep(25 * time.Millisecond) {
				if _, err := os.Stat(f.Done); err == nil {
					stopped = true
					break
				}
			}
			if !stopped {
				t.Error("Worker helper did not stop gracefully")
			}
		}
		// Never use kill-server, a warm target, or a namespace-only kill: psmux
		// may resolve those to an unrelated session. This full target is unique.
		_, _ = runMux("-t", target, "kill-session")
		path, err := windows.UTF16PtrFromString(dir)
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
	if out, err := runMux("-L", ns, "-f", configPath, "new-session", "-d", "-s", "worker", "--", powershell, "-NoLogo", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptPath); err != nil {
		t.Fatalf("start Worker in psmux: %v: %s", err, out)
	}
	connected := false
	for until := time.Now().Add(10 * time.Second); time.Now().Before(until); time.Sleep(25 * time.Millisecond) {
		h.mu.Lock()
		connected = h.workers["n"] != nil
		h.mu.Unlock()
		if connected {
			break
		}
	}
	if !connected {
		pane, captureErr := runMux("-t", target, "capture-pane", "-p", "-S", "-100")
		status, statusErr := os.ReadFile(f.Status)
		t.Fatalf("Worker did not connect from psmux; pane capture: %s (%v); helper environment: %s (%v)", pane, captureErr, status, statusErr)
	}
	inherited, err := os.ReadFile(f.Status)
	if err != nil {
		t.Fatal(err)
	}
	var markers map[string]string
	if err := json.Unmarshal(inherited, &markers); err != nil || markers["PSMUX_SESSION"] != "worker" || !strings.Contains(markers["TMUX"], ns) {
		t.Fatalf("Worker was not started in a real psmux pane: %s (%v)", inherited, err)
	}
	t.Logf("Worker inherited actual psmux context: %s", inherited)
	c := config.Config{ID: "u", Token: strings.Repeat("c", 32), Hub: url}
	checkControl := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		result, err := h.call(ctx, "u", "n", "host.inspect", protocol.ID("inspect_"), struct{}{})
		if err != nil || result.Error != nil {
			t.Fatalf("Worker control stopped responding: %+v %v", result, err)
		}
	}
	attach := func(p *transport.Peer) {
		windowsTerminalType(t, p, "[Console]::WriteLine([char]95 + 'MUX_CONTEXT:' + (@('TMUX','TMUX_PANE','PSMUX_SESSION','PSMUX_ACTIVE','PSMUX_SESSION_NAME','PSMUX_REMOTE_ATTACH','PSMUX_TARGET_SESSION','PSMUX_TARGET_FULL') | Where-Object { Test-Path ('Env:' + $_) }).Count + [char]95)\r")
		windowsTerminalUntil(t, p, "_MUX_CONTEXT:0_")
		windowsTerminalType(t, p, "& "+quote(psmux)+" -t "+quote(target)+" attach-session\r")
		output := windowsTerminalUntil(t, p, f.Marker)
		if bytes.Contains(output, []byte("sessions should be nested")) {
			t.Fatalf("false nesting warning: %q", output)
		}
	}
	p := openWindowsTerminal(t, c)
	attach(p)
	// Keep draining while attached to the very pane hosting this Worker. A
	// feedback loop would amplify its pulses instead of delivering bounded output.
	chunks := make(chan []byte, 16)
	readErrors := make(chan error, 1)
	stopRead := make(chan struct{})
	defer close(stopRead)
	go func() {
		for {
			m, err := p.Read()
			if err != nil {
				readErrors <- err
				return
			}
			data, err := protocol.Decode[protocol.TerminalData](m)
			if err != nil || m.Type != "shell_output" {
				readErrors <- fmt.Errorf("unexpected attached terminal frame: %+v (%v)", m, err)
				return
			}
			select {
			case chunks <- data.Bytes:
			case <-stopRead:
				return
			}
		}
	}()
	var observed []byte
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
observe:
	for {
		select {
		case chunk := <-chunks:
			observed = append(observed, chunk...)
			if len(observed) > 256<<10 {
				t.Fatal("same-session attach amplified terminal output beyond 256 KiB")
			}
		case err := <-readErrors:
			t.Fatal(err)
		case <-timer.C:
			break observe
		}
	}
	pulses := regexp.MustCompile(regexp.QuoteMeta(f.Marker)+`:[0-9]{8}`).FindAll(observed, -1)
	unique := make(map[string]bool)
	for _, pulse := range pulses {
		unique[string(pulse)] = true
	}
	if count := len(pulses); len(unique) < 3 || count > 200 {
		t.Fatalf("unexpected Worker pulse count %d in %d bytes", count, len(observed))
	}
	t.Logf("same-session attach delivered %d bytes and %d distinct pulses in 3 seconds", len(observed), len(unique))
	checkControl()
	// Abrupt stream loss must remove only this attach client and its outer shell.
	p.Close()
	remoteEventually(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.terminals) == 0 })
	checkControl()
	if out, err := runMux("-t", target, "has-session"); err != nil {
		t.Fatalf("Worker's session died on terminal disconnect: %v: %s", err, out)
	}
	q := openWindowsTerminal(t, c)
	attach(q)
	checkControl()
	windowsTerminalType(t, q, "\x02d")
	windowsTerminalMarker(t, q, "DETACHED_WORKER_ALIVE")
	checkControl()
	q.Close()
	t.Log("Worker control responded while attached, after stream loss, after reattach, and after detach; original psmux session survived")
}
