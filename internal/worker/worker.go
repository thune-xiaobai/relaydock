package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"relaydock/internal/config"
	"relaydock/internal/localfile"
	"relaydock/internal/protocol"
	"relaydock/internal/store"
	"relaydock/internal/transport"
)

var Tools = []protocol.Tool{
	{Name: "host.inspect", Description: "Inspect this host and configured capabilities"},
	{Name: "session.list", Description: "List managed pi sessions"},
	{Name: "session.inspect", Description: "Read the bridge status of one session"},
	{Name: "session.create", Description: "Create an interactive pi in a dedicated mux session"},
	{Name: "agent.submit", Description: "Submit a message to an idle pi; returns acceptance, not completion"},
	{Name: "agent.interrupt", Description: "Request abortion of the identified current pi run"},
	{Name: "terminal.capture", Description: "Read a bounded snapshot of the original terminal pane"},
	{Name: "session.close", Description: "Close this managed mux session only"},
}

type Worker struct {
	c  config.Config
	db *store.Store
	mu sync.Mutex
}
type callRecord struct {
	Hash    string           `json:"hash"`
	Result  *protocol.Result `json:"result,omitempty"`
	Created string           `json:"created_session,omitempty"`
	Target  string           `json:"target_session,omitempty"`
}
type Arguments struct {
	Session   string `json:"session_id,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Instance  string `json:"instance,omitempty"`
	Native    string `json:"native_session,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	Text      string `json:"text,omitempty"`
}

func New(c config.Config) (*Worker, error) {
	w := &Worker{c: c}
	if err := w.validate(); err != nil {
		return nil, err
	}
	db, err := store.Open(c.StateDir)
	if err != nil {
		return nil, err
	}
	w.db = db
	return w, nil
}
func (w *Worker) Close() error         { return w.db.Close() }
func (w *Worker) dir(id string) string { return filepath.Join(w.c.StateDir, "sessions", id) }
func keys[V any](m map[string]V) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func (w *Worker) Hello() protocol.Hello {
	return protocol.Hello{Role: "worker", ID: w.c.ID, Name: w.c.Name, OS: runtime.GOOS, Tools: Tools, Workspaces: keys(w.c.Workspaces), Agents: keys(w.c.Agents)}
}

func (w *Worker) inspect(id string) (protocol.Session, error) {
	var s protocol.Session
	if err := w.db.Get("sessions", id, &s); err != nil {
		return s, err
	}
	if s.Status == "closed" {
		return s, nil
	}
	var exited map[string]string
	if localfile.Read(filepath.Join(w.dir(id), "exit.json"), &exited) == nil {
		s.Status = "exited"
		return s, nil
	}
	var b protocol.BridgeState
	if err := localfile.Read(filepath.Join(w.dir(id), "state.json"), &b); err != nil {
		s.Status = "unknown"
		return s, nil
	}
	stamp, err := time.Parse(time.RFC3339Nano, b.Updated)
	if err != nil || time.Since(stamp) > 8*time.Second || b.Session != id {
		s.Status = "unknown"
		return s, nil
	}
	s.Instance = b.Instance
	s.Native = b.Native
	s.Status = b.Status
	s.RunID = b.RunID
	s.Updated = b.Updated
	return s, nil
}
func (w *Worker) Sessions() ([]protocol.Session, error) {
	m, err := w.db.List("sessions")
	if err != nil {
		return nil, err
	}
	out := []protocol.Session{}
	for _, id := range keys(m) {
		s, err := w.inspect(id)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// Calls are journaled before any side effect. An interrupted call is never
// automatically repeated; its ID can only recover an existing bridge receipt.
func (w *Worker) Call(ctx context.Context, c protocol.Call) protocol.Result {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !protocol.ValidID(c.ID) {
		return protocol.Fail(c.ID, "invalid", "invalid call_id")
	}
	var normalized any
	if err := json.Unmarshal(c.Args, &normalized); err != nil {
		return protocol.Fail(c.ID, "invalid", "invalid arguments")
	}
	h := sha256.Sum256(append([]byte(c.Tool+"\n"), protocol.JSON(normalized)...))
	hash := hex.EncodeToString(h[:])
	var record callRecord
	err := w.db.Get("calls", c.ID, &record)
	if err == nil {
		if record.Hash != hash {
			return protocol.Fail(c.ID, "conflict", "call_id already belongs to different arguments")
		}
		if record.Result != nil && (record.Result.Error == nil || record.Result.Error.Code != "unknown") {
			return *record.Result
		}
		var a Arguments
		_ = json.Unmarshal(c.Args, &a)
		if protocol.ValidID(a.Session) {
			var r protocol.Result
			if localfile.Read(filepath.Join(w.dir(a.Session), "receipts", c.ID+".json"), &r) == nil && r.ID == c.ID {
				record.Result = &r
				if e := w.db.Put("calls", c.ID, record); e != nil {
					return protocol.Fail(c.ID, "storage", e.Error())
				}
				return r
			}
		}
		if record.Result != nil {
			return *record.Result
		}
		return protocol.Fail(c.ID, "unknown", "previous call was interrupted; inspect the session before any new operation")
	}
	if !errors.Is(err, store.ErrMissing) {
		return protocol.Fail(c.ID, "storage", err.Error())
	}
	var target Arguments
	_ = json.Unmarshal(c.Args, &target)
	record = callRecord{Hash: hash, Target: target.Session}
	if err = w.db.Put("calls", c.ID, record); err != nil {
		return protocol.Fail(c.ID, "storage", err.Error())
	}
	r := w.execute(ctx, c)
	if err = w.db.Get("calls", c.ID, &record); err != nil {
		return protocol.Fail(c.ID, "unknown", "could not reload call journal")
	}
	record.Result = &r
	if err = w.db.Put("calls", c.ID, record); err != nil {
		return protocol.Fail(c.ID, "unknown", "operation may have executed; could not persist result")
	}
	return r
}

// Lookup recovers receipts after reconnect without dispatching an operation.
func (w *Worker) Lookup(id string) protocol.Result {
	if !protocol.ValidID(id) {
		return protocol.Fail(id, "invalid", "invalid call ID")
	}
	var rec callRecord
	if e := w.db.Get("calls", id, &rec); e != nil {
		return protocol.Fail(id, "unknown", "no durable call result is available; no operation was replayed")
	}
	if rec.Result != nil && (rec.Result.Error == nil || rec.Result.Error.Code != "unknown") {
		return *rec.Result
	}
	if protocol.ValidID(rec.Target) {
		var r protocol.Result
		if localfile.Read(filepath.Join(w.dir(rec.Target), "receipts", id+".json"), &r) == nil && r.ID == id {
			return r
		}
	}
	if protocol.ValidID(rec.Created) {
		s, e := w.inspect(rec.Created)
		if e == nil {
			return protocol.Result{ID: id, Data: protocol.JSON(s), Error: &protocol.Fault{Code: "unknown", Message: "recovered created session; inspect before continuing"}}
		}
	}
	if rec.Result != nil {
		return *rec.Result
	}
	return protocol.Fail(id, "unknown", "previous operation was interrupted; no operation was replayed")
}

func (w *Worker) execute(ctx context.Context, c protocol.Call) protocol.Result {
	var a Arguments
	d := json.NewDecoder(bytes.NewReader(c.Args))
	d.DisallowUnknownFields()
	if err := d.Decode(&a); err != nil {
		return protocol.Fail(c.ID, "invalid", err.Error())
	}
	ok := func(v any) protocol.Result { return protocol.OK(c.ID, v) }
	fail := func(code string, err error) protocol.Result { return protocol.Fail(c.ID, code, err.Error()) }
	if c.Tool == "host.inspect" {
		return ok(w.Hello())
	}
	if c.Tool == "session.list" {
		s, e := w.Sessions()
		if e != nil {
			return fail("storage", e)
		}
		return ok(s)
	}
	if c.Tool == "session.create" {
		if _, yes := w.c.Workspaces[a.Workspace]; !yes {
			return protocol.Fail(c.ID, "invalid", "unknown workspace")
		}
		if _, yes := w.c.Agents[a.Agent]; !yes {
			return protocol.Fail(c.ID, "invalid", "unknown pi profile")
		}
		ss, err := w.Sessions()
		if err != nil {
			return fail("storage", err)
		}
		live := 0
		for _, s := range ss {
			if s.Status == "closed" || s.Status == "exited" {
				continue
			}
			live++
			// Reserve a workspace for the entire live session. This also covers
			// local inputs, which can start while Hub has no pending call.
			directory := s.Directory
			if directory == "" {
				directory = w.c.Workspaces[s.Workspace]
			}
			if directory == "" || pathsOverlap(directory, w.c.Workspaces[a.Workspace]) {
				return protocol.Fail(c.ID, "workspace_busy", "a live session already owns this directory or an overlapping directory; continue it or use another worktree")
			}
		}
		if live >= w.c.MaxRunning {
			return protocol.Fail(c.ID, "capacity", "maximum live sessions reached")
		}
		s := protocol.Session{ID: protocol.ID("s_"), Node: w.c.ID, Workspace: a.Workspace, Agent: a.Agent, Status: "starting", Updated: protocol.Now()}
		s.MuxName = s.ID
		s.Directory = w.c.Workspaces[a.Workspace]
		s.Attach = []string{w.c.Mux, "-L", w.c.Namespace, "attach-session", "-t", s.MuxName}
		if err = w.db.Put("sessions", s.ID, s); err != nil {
			return fail("storage", err)
		}
		if err = w.db.Update(func(t *store.Tx) error {
			var rec callRecord
			if e := t.Get("calls", c.ID, &rec); e != nil {
				return e
			}
			rec.Created = s.ID
			return t.Put("calls", c.ID, rec)
		}); err != nil {
			return fail("storage", err)
		}
		if err = w.start(ctx, &s); err != nil {
			return protocol.Result{ID: c.ID, Data: protocol.JSON(s), Error: &protocol.Fault{Code: "unknown", Message: err.Error()}}
		}
		deadline := time.NewTimer(25 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return protocol.Result{ID: c.ID, Data: protocol.JSON(s), Error: &protocol.Fault{Code: "unknown", Message: ctx.Err().Error()}}
			case <-deadline.C:
				return protocol.Result{ID: c.ID, Data: protocol.JSON(s), Error: &protocol.Fault{Code: "unknown", Message: "pi bridge did not become ready; inspect this session"}}
			case <-tick.C:
				current, e := w.inspect(s.ID)
				if e != nil {
					return fail("storage", e)
				}
				if current.Status == "idle" {
					if e = w.db.Put("sessions", current.ID, current); e != nil {
						return fail("storage", e)
					}
					return ok(current)
				}
				if current.Status == "exited" {
					return protocol.Result{ID: c.ID, Data: protocol.JSON(current), Error: &protocol.Fault{Code: "exited", Message: "pi exited during startup"}}
				}
			}
		}
	}
	if !protocol.ValidID(a.Session) {
		return protocol.Fail(c.ID, "invalid", "valid session_id required")
	}
	s, err := w.inspect(a.Session)
	if err != nil {
		return fail("not_found", err)
	}
	switch c.Tool {
	case "session.inspect":
		return ok(s)
	case "terminal.capture":
		if s.Pane == "" {
			return protocol.Fail(c.ID, "unknown", "original pane was not identified")
		}
		args := []string{"capture-pane", "-p", "-t", s.Pane, "-S", "-100"}
		if w.c.Backend == "psmux" {
			args = append([]string{"-t", s.MuxName}, args...)
		}
		b, e := w.mux(ctx, args...)
		if e != nil {
			return fail("terminal", e)
		}
		return ok(map[string]string{"text": string(b)})
	case "session.close":
		if s.Status == "closed" {
			return ok(s)
		}
		if s.Status != "exited" {
			if a.Instance == "" || a.Instance != s.Instance || a.Native != s.Native {
				return protocol.Fail(c.ID, "stale_target", "inspect session and supply current instance/native_session")
			}
			if _, e := w.mux(ctx, "kill-session", "-t", s.MuxName); e != nil {
				return fail("unknown", e)
			}
		} else if _, e := w.mux(ctx, "has-session", "-t", s.MuxName); e == nil {
			if _, e = w.mux(ctx, "kill-session", "-t", s.MuxName); e != nil {
				return fail("unknown", e)
			}
		}
		s.Status = "closed"
		s.Updated = protocol.Now()
		if e := w.db.Put("sessions", s.ID, s); e != nil {
			return fail("storage", e)
		}
		return ok(s)
	case "agent.submit", "agent.interrupt":
		if s.Status == "unknown" || s.Status == "closed" || s.Status == "exited" {
			return protocol.Fail(c.ID, "unavailable", "pi bridge is not available")
		}
		if a.Instance == "" || a.Instance != s.Instance || a.Native != s.Native {
			return protocol.Fail(c.ID, "stale_target", "pi instance or native session changed")
		}
		if !protocol.ValidID(a.RunID) {
			return protocol.Fail(c.ID, "invalid", "run_id is required")
		}
		if c.Tool == "agent.submit" && (s.Status != "idle" || strings.TrimSpace(a.Text) == "" || len(a.Text) > protocol.MaxText) {
			return protocol.Fail(c.ID, "busy_or_invalid", "pi must be idle and text must contain 1..65536 bytes")
		}
		if c.Tool == "agent.interrupt" && a.RunID != s.RunID {
			return protocol.Fail(c.ID, "stale_target", "current run changed")
		}
		req := protocol.BridgeRequest{CallID: c.ID, Action: strings.TrimPrefix(c.Tool, "agent."), Session: s.ID, Instance: s.Instance, Native: s.Native, RunID: a.RunID, Text: a.Text}
		if err = localfile.Write(filepath.Join(w.dir(s.ID), "requests", c.ID+".json"), req); err != nil {
			return fail("storage", err)
		}
		deadline := time.NewTimer(8 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return protocol.Fail(c.ID, "unknown", ctx.Err().Error())
			case <-deadline.C:
				return protocol.Fail(c.ID, "unknown", "receipt timeout; inspect session, do not resubmit automatically")
			case <-tick.C:
				var r protocol.Result
				if localfile.Read(filepath.Join(w.dir(s.ID), "receipts", c.ID+".json"), &r) == nil && r.ID == c.ID && (r.Error == nil || r.Error.Code != "unknown") {
					return r
				}
			}
		}
	default:
		return protocol.Fail(c.ID, "unsupported", "unknown tool")
	}
}

func pathsOverlap(a, b string) bool {
	if runtime.GOOS == "windows" {
		a = strings.ToLower(a)
		b = strings.ToLower(b)
	}
	for _, p := range [][2]string{{a, b}, {b, a}} {
		r, e := filepath.Rel(p[0], p[1])
		if e == nil && (r == "." || r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

// collect commits event and cursor together, and never consumes a partial line.
func (w *Worker) collect() error {
	ss, err := w.Sessions()
	if err != nil {
		return err
	}
	for _, s := range ss {
		f, e := os.Open(filepath.Join(w.dir(s.ID), "events.ndjson"))
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		var offset int64
		e = w.db.Get("cursors", s.ID, &offset)
		if e != nil && !errors.Is(e, store.ErrMissing) {
			f.Close()
			return e
		}
		if _, e = f.Seek(offset, io.SeekStart); e != nil {
			f.Close()
			return e
		}
		r := bufio.NewReaderSize(f, protocol.MaxMessage)
		atEnd := false
		for n := 0; n < 256; n++ {
			line, readErr := r.ReadSlice('\n')
			if readErr == io.EOF {
				atEnd = true
				break
			}
			if readErr != nil {
				f.Close()
				return readErr
			}
			var ev protocol.Event
			if e = json.Unmarshal(line, &ev); e != nil {
				f.Close()
				return e
			}
			if ev.Session != s.ID || !protocol.ValidID(ev.ID) {
				f.Close()
				return errors.New("invalid local event")
			}
			next := offset + int64(len(line))
			ev.Position = next
			e = w.db.Update(func(t *store.Tx) error {
				if e := t.Put("outbox", ev.ID, ev); e != nil {
					return e
				}
				return t.Put("cursors", s.ID, next)
			})
			if e != nil {
				f.Close()
				return e
			}
			offset = next
		}
		f.Close()
		// A launcher exit is an authoritative process fact, including abrupt pi exit.
		var ex map[string]string
		if atEnd && localfile.Read(filepath.Join(w.dir(s.ID), "exit.json"), &ex) == nil {
			id := "exit_" + s.ID
			e = w.db.Update(func(t *store.Tx) error {
				var seen bool
				e := t.Get("exits", id, &seen)
				if e == nil {
					return nil
				}
				if !errors.Is(e, store.ErrMissing) {
					return e
				}
				ev := protocol.Event{ID: id, Session: s.ID, Instance: "launcher", Kind: "exited", Status: "exited", Text: ex["error"], Time: ex["time"]}
				ev.Position = offset + 1
				if e = t.Put("outbox", id, ev); e != nil {
					return e
				}
				return t.Put("exits", id, true)
			})
			if e != nil {
				return e
			}
		}
	}
	return nil
}

func (w *Worker) Run(ctx context.Context) error {
	if _, e := config.ClientTLS(w.c); e != nil {
		return e
	}
	for ctx.Err() == nil {
		p, err := transport.Dial(ctx, w.c, w.Hello())
		if err == nil {
			log.Printf("worker %s connected", w.c.ID)
			err = w.connected(ctx, p)
			p.Close()
		}
		if ctx.Err() != nil {
			break
		}
		log.Printf("worker reconnect: %v", err)
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return nil
}
func (w *Worker) connected(parent context.Context, p *transport.Peer) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go p.KeepAlive(ctx)
	go func() { <-ctx.Done(); p.Close() }()
	jobs := make(chan protocol.Call, 32)
	var wg sync.WaitGroup
	wg.Add(2)
	defer wg.Wait()
	// Register cancel after Wait so the call goroutine exits before returning.
	defer cancel()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case c := <-jobs:
				r := w.Call(ctx, c)
				if p.Send(protocol.Wrap("result", c.ID, r)) != nil {
					p.Close()
					return
				}
			}
		}
	}()
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if e := w.collect(); e != nil {
					log.Printf("worker event journal: %v", e)
					p.Close()
					return
				}
				m, e := w.db.List("outbox")
				if e != nil {
					p.Close()
					return
				}
				events := []protocol.Event{}
				for _, b := range m {
					var ev protocol.Event
					if json.Unmarshal(b, &ev) == nil {
						events = append(events, ev)
					}
				}
				sort.Slice(events, func(i, j int) bool {
					if events[i].Session != events[j].Session {
						return events[i].Session < events[j].Session
					}
					return events[i].Position < events[j].Position
				})
				for i, ev := range events {
					if i >= 256 {
						break
					}
					if p.Send(protocol.Wrap("event", ev.ID, ev)) != nil {
						p.Close()
						return
					}
				}
			}
		}
	}()
	for {
		m, e := p.Read()
		if e != nil {
			return e
		}
		switch m.Type {
		case "call_status":
			if m.Version != protocol.Version {
				return errors.New("bad version")
			}
			r := w.Lookup(m.ID)
			if e = p.Send(protocol.Wrap("result", m.ID, r)); e != nil {
				return e
			}
		case "call":
			c, e := protocol.Decode[protocol.Call](m)
			if e != nil {
				return e
			}
			select {
			case jobs <- c:
			case <-ctx.Done():
				return ctx.Err()
			default:
				return fmt.Errorf("too many pending calls")
			}
		case "event_ack":
			if m.Version != protocol.Version {
				return errors.New("bad version")
			}
			if e = w.db.Delete("outbox", m.ID); e != nil {
				return e
			}
		default:
			return errors.New("unexpected worker message")
		}
	}
}
