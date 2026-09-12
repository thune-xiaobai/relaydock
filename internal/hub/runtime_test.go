package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"relaydock/internal/channel"
	"relaydock/internal/config"
	"relaydock/internal/localfile"
	"relaydock/internal/protocol"
	"relaydock/internal/worker"
)

// This exercises the real tmux + interactive pi runtime against a local fixture
// model. It makes no external LLM calls and never opens/sends a WeChat message.
func TestRealPiRuntime(t *testing.T) {
	if os.Getenv("RELAYDOCK_TEST_PI") != "1" {
		t.Skip("set RELAYDOCK_TEST_PI=1 to test installed pi + tmux")
	}
	if _, e := exec.LookPath("tmux"); e != nil {
		t.Fatal(e)
	}
	if _, e := exec.LookPath("pi"); e != nil {
		t.Fatal(e)
	}
	root, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "relaydock")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/relaydock")
	cmd.Dir = root
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, b)
	}
	var countsMu sync.Mutex
	counts := map[string]int{}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string           `json:"model"`
			Messages []fixtureMessage `json:"messages"`
			Tools    []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
			http.Error(w, e.Error(), 400)
			return
		}
		if req.Model == "coordinator" {
			for _, tool := range req.Tools {
				if tool.Function.Name == "bash" || tool.Function.Name == "read" || tool.Function.Name == "write" {
					t.Error("coordinator exposed builtin tool")
				}
			}
			coordinatorFixture(w, req.Messages)
			return
		}
		text := ""
		for _, m := range req.Messages {
			if m.Role == "user" {
				var s string
				if json.Unmarshal(m.Content, &s) == nil {
					text = s
				} else {
					var parts []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					}
					_ = json.Unmarshal(m.Content, &parts)
					for _, p := range parts {
						if p.Type == "text" {
							text = p.Text
						}
					}
				}
			}
		}
		countsMu.Lock()
		counts[text]++
		countsMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(text, "WAIT_FIXTURE") {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Second):
			}
		}
		send := func(delta any, stop any) {
			b := protocol.JSON(map[string]any{"id": "fixture", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": "fixture", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": stop}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
			w.(http.Flusher).Flush()
		}
		send(map[string]string{"role": "assistant", "content": "FIXTURE_RESULT: " + text}, nil)
		send(map[string]string{}, "stop")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer model.Close()
	piDir := filepath.Join(tmp, "pi-home")
	if e := os.MkdirAll(piDir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := localfile.Write(filepath.Join(piDir, "models.json"), map[string]any{"providers": map[string]any{"fixture": map[string]any{"baseUrl": model.URL + "/v1", "api": "openai-completions", "apiKey": "fixture", "models": []any{map[string]any{"id": "fixture", "contextWindow": 64000, "maxTokens": 2000}}}}}); e != nil {
		t.Fatal(e)
	}
	h, e := New(config.Config{StateDir: filepath.Join(tmp, "hub"), Listen: "127.0.0.1:0", Workers: map[string]config.Peer{"local": {Token: strings.Repeat("w", 32)}}, Channels: map[string]config.Peer{"console": {Token: strings.Repeat("c", 32), Nodes: []string{"local"}}}, Model: config.Model{URL: model.URL + "/v1", Model: "coordinator"}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(h.Handler())
	hubURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	ctx, cancel := context.WithCancel(context.Background())
	hubDone := make(chan struct{})
	go func() { defer close(hubDone); h.Serve(ctx) }()
	defer func() { cancel(); <-hubDone; h.Close(); server.Close() }()
	for _, d := range []string{"a", "b"} {
		if e := os.MkdirAll(filepath.Join(tmp, d), 0700); e != nil {
			t.Fatal(e)
		}
	}
	ns := "rdtest_" + protocol.ID("")[:8]
	wc := config.Config{StateDir: filepath.Join(tmp, "worker"), ID: "local", Hub: hubURL, Token: strings.Repeat("w", 32), Backend: "tmux", Mux: "tmux", Namespace: ns, Bridge: filepath.Join(root, "extensions", "relaydock.ts"), Workspaces: map[string]string{"a": filepath.Join(tmp, "a"), "b": filepath.Join(tmp, "b")}, Agents: map[string]config.Agent{"pi": {Executable: "pi", Args: []string{"--offline", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-context-files", "--no-tools", "--provider", "fixture", "--model", "fixture"}, Env: map[string]string{"PI_CODING_AGENT_DIR": piDir}}}, MaxRunning: 4}
	workerFile := filepath.Join(tmp, "worker.json")
	if e := localfile.Write(workerFile, wc); e != nil {
		t.Fatal(e)
	}
	logFile, e := os.Create(filepath.Join(tmp, "worker.log"))
	if e != nil {
		t.Fatal(e)
	}
	defer logFile.Close()
	startWorker := func() func() {
		runCtx, stop := context.WithCancel(ctx)
		c := exec.CommandContext(runCtx, bin, "worker", "--config", workerFile)
		c.Stdout = logFile
		c.Stderr = logFile
		if e := c.Start(); e != nil {
			t.Fatal(e)
		}
		return func() { stop(); _ = c.Wait() }
	}
	stopWorker := startWorker()
	defer func() { stopWorker() }()
	defer func() {
		m, _ := h.db.List("sessions")
		for _, b := range m {
			var binding Binding
			_ = json.Unmarshal(b, &binding)
			if t.Failed() {
				out, _ := exec.Command("tmux", "-L", ns, "capture-pane", "-p", "-t", binding.Session.MuxName).CombinedOutput()
				t.Log(string(out))
			}
			_ = exec.Command("tmux", "-L", ns, "kill-session", "-t", binding.Session.MuxName).Run()
		}
		if t.Failed() {
			b, _ := os.ReadFile(logFile.Name())
			t.Log(string(b))
		}
	}()
	wait := func(label string, fn func() bool) {
		t.Helper()
		deadline := time.Now().Add(40 * time.Second)
		for time.Now().Before(deadline) {
			if fn() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("timeout: %s", label)
	}
	wait("worker registration", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.workers["local"] != nil })
	ch, e := channel.New(config.Config{StateDir: filepath.Join(tmp, "channel"), Hub: hubURL, ID: "console", Token: strings.Repeat("c", 32)})
	if e != nil {
		t.Fatal(e)
	}
	var outMu sync.Mutex
	outputs := []protocol.ChatOutput{}
	ch.Output = func(o protocol.ChatOutput) error {
		outMu.Lock()
		defer outMu.Unlock()
		outputs = append(outputs, o)
		return nil
	}
	chCtx, chCancel := context.WithCancel(ctx)
	chDone := make(chan struct{})
	go func() { defer close(chDone); _ = ch.Run(chCtx) }()
	defer func() { chCancel(); <-chDone; ch.Close() }()
	sendChat := func(id, text string) {
		t.Helper()
		if e := ch.Enqueue(protocol.ChatInput{ID: id, Text: text}); e != nil {
			t.Fatal(e)
		}
	}
	find := func(workspace string) Binding {
		m, _ := h.db.List("sessions")
		for _, raw := range m {
			var b Binding
			_ = json.Unmarshal(raw, &b)
			if b.Session.Workspace == workspace {
				return b
			}
		}
		return Binding{}
	}
	sendChat("in_a", "在项目 A 开始检查")
	wait("A completed", func() bool {
		b := find("a")
		return b.Outcome == "completed" && strings.Contains(b.LastText, "FIXTURE_RESULT")
	})
	a := find("a")
	sendChat("in_watch", "等项目 A 本轮结束后总结")
	wait("watch resumed real coordinator", func() bool {
		outMu.Lock()
		defer outMu.Unlock()
		for _, o := range outputs {
			if strings.Contains(o.Text, "FOLLOWUP_FIXTURE") {
				return true
			}
		}
		return false
	})
	typeLocal := func(s protocol.Session, text string) {
		t.Helper()
		if out, e := exec.Command("tmux", "-L", ns, "send-keys", "-t", s.Pane, "-l", text).CombinedOutput(); e != nil {
			t.Fatalf("local input: %v %s", e, out)
		}
		if e := exec.Command("tmux", "-L", ns, "send-keys", "-t", s.Pane, "Enter").Run(); e != nil {
			t.Fatal(e)
		}
	}
	var before protocol.BridgeState
	statePath := filepath.Join(wc.StateDir, "sessions", a.Session.ID, "state.json")
	if e := localfile.Read(statePath, &before); e != nil {
		t.Fatal(e)
	}
	sendChat("in_a", "在项目 A 开始检查") // Same occurrence is not another execution.
	sendChat("in_b", "在项目 B 开始检查")
	wait("B completed", func() bool { return find("b").Outcome == "completed" })
	b := find("b")
	if b.Session.Native == a.Session.Native {
		t.Fatal("sessions share native pi identity")
	}
	stopWorker()
	typeLocal(a.Session, "OFFLINE_FIXTURE")
	wait("pi works without Worker", func() bool { countsMu.Lock(); defer countsMu.Unlock(); return counts["OFFLINE_FIXTURE"] == 1 })
	stopWorker = startWorker()
	wait("worker reconnected", func() bool {
		r, e := h.call(ctx, "console", "local", "session.inspect", "", worker.Arguments{Session: a.Session.ID})
		return e == nil && strings.Contains(string(r.Data), before.Instance)
	})
	var after protocol.BridgeState
	if e := localfile.Read(statePath, &after); e != nil {
		t.Fatal(e)
	}
	if before.PID != after.PID || before.Native != after.Native {
		t.Fatal("worker restart replaced pi")
	}
	wait("offline events replayed", func() bool { return strings.Contains(find("a").LastText, "OFFLINE_FIXTURE") })
	sendChat("in_continue", "继续项目 A")
	wait("continued original session", func() bool {
		return strings.Contains(find("a").LastText, "继续项目 A") && find("a").Outcome == "completed"
	})
	if find("a").Session.Native != before.Native {
		t.Fatal("continue changed native session")
	}
	typeLocal(a.Session, "LOCAL_FIXTURE")
	wait("local input forwarded to chat", func() bool {
		outMu.Lock()
		defer outMu.Unlock()
		for _, o := range outputs {
			if strings.Contains(o.Text, "FIXTURE_RESULT: LOCAL_FIXTURE") {
				return true
			}
		}
		return false
	})
	countsMu.Lock()
	initialCount := counts["在项目 A 开始检查"]
	localCount := counts["LOCAL_FIXTURE"]
	countsMu.Unlock()
	if initialCount != 1 || localCount != 1 {
		t.Fatalf("wrong execution count: initial %d local %d", initialCount, localCount)
	}
	// Interrupt is a pi API request; terminal liveness is not its result.
	r, e := h.call(ctx, "console", "local", "session.inspect", "", worker.Arguments{Session: a.Session.ID})
	if e != nil {
		t.Fatal(e)
	}
	var live protocol.Session
	_ = json.Unmarshal(r.Data, &live)
	run := protocol.ID("run_")
	if _, e = h.call(ctx, "console", "local", "agent.submit", "", worker.Arguments{Session: live.ID, Instance: live.Instance, Native: live.Native, RunID: run, Text: "WAIT_FIXTURE"}); e != nil {
		t.Fatal(e)
	}
	wait("long request entered fixture", func() bool { countsMu.Lock(); defer countsMu.Unlock(); return counts["WAIT_FIXTURE"] == 1 })
	if _, e = h.call(ctx, "console", "local", "agent.interrupt", "", worker.Arguments{Session: live.ID, Instance: live.Instance, Native: live.Native, RunID: run}); e != nil {
		t.Fatal(e)
	}
	wait("pi abort event", func() bool { return find("a").Outcome == "cancelled" })
	// Both independent workspaces can be executing at the same time.
	running := []protocol.Session{}
	for _, binding := range []Binding{find("a"), find("b")} {
		s := binding.Session
		s.RunID = protocol.ID("run_")
		if _, e = h.call(ctx, "console", "local", "agent.submit", "", worker.Arguments{Session: s.ID, Instance: s.Instance, Native: s.Native, RunID: s.RunID, Text: "WAIT_FIXTURE_" + s.Workspace}); e != nil {
			t.Fatal(e)
		}
		running = append(running, s)
	}
	wait("two pi runs overlap", func() bool {
		countsMu.Lock()
		defer countsMu.Unlock()
		return counts["WAIT_FIXTURE_a"] == 1 && counts["WAIT_FIXTURE_b"] == 1 && find("a").Session.Status == "running" && find("b").Session.Status == "running"
	})
	for _, s := range running {
		if _, e = h.call(ctx, "console", "local", "agent.interrupt", "", worker.Arguments{Session: s.ID, Instance: s.Instance, Native: s.Native, RunID: s.RunID}); e != nil {
			t.Fatal(e)
		}
	}
	wait("parallel runs aborted", func() bool { return find("a").Outcome == "cancelled" && find("b").Outcome == "cancelled" })
	// A local /new invalidates old remote targets without any handoff workflow.
	stale := find("a").Session
	typeLocal(stale, "/new")
	wait("local native session switch", func() bool { return find("a").Session.Native != stale.Native })
	if _, e = h.call(ctx, "console", "local", "agent.submit", "", worker.Arguments{Session: stale.ID, Instance: stale.Instance, Native: stale.Native, RunID: protocol.ID("run_"), Text: "must not run"}); e == nil || !strings.Contains(e.Error(), "stale_target") {
		t.Fatalf("stale target accepted: %v", e)
	}
	// Keep the pane, explicitly exit pi, and verify it is not reported as idle.
	if e := exec.Command("tmux", "-L", ns, "set-window-option", "-t", b.Session.MuxName, "remain-on-exit", "on").Run(); e != nil {
		t.Fatal(e)
	}
	typeLocal(b.Session, "/quit")
	wait("pi exit with retained pane", func() bool { return find("b").Session.Status == "exited" })
	r, e = h.call(ctx, "console", "local", "session.inspect", "", worker.Arguments{Session: b.Session.ID})
	if e != nil {
		t.Fatal(e)
	}
	_ = json.Unmarshal(r.Data, &live)
	if live.Status != "exited" {
		t.Fatal(live)
	}
	// Close both a live bridge and an exited pi with a retained pane through
	// the actual Hub/Worker tools, including tmux's exact-name kill target.
	for _, binding := range []Binding{find("a"), find("b")} {
		r := h.executeTool(ctx, "console", &dialogue{}, "session_close", protocol.JSON(map[string]string{"session_id": binding.Session.ID}))
		if !r.OK {
			t.Fatal(r)
		}
	}
	if find("a").Session.Status != "closed" || find("b").Session.Status != "closed" {
		t.Fatal("close did not persist")
	}
	t.Log("real pi SDK multistep coordinator + tmux/pi: parallel sessions, native continuation/switch, local/offline event replay, Worker restart, dedupe, interrupt and pi exit verified using fixture model")
}
