package hub

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"relaydock/internal/config"
	"relaydock/internal/protocol"
	"relaydock/internal/store"
	"relaydock/internal/transport"
)

type connection struct {
	peer  *transport.Peer
	hello protocol.Hello
}
type Binding struct {
	Session       protocol.Session `json:"session"`
	Owner         string           `json:"owner"`
	Title         string           `json:"title"`
	Outcome       string           `json:"outcome,omitempty"`
	LastText      string           `json:"last_text,omitempty"`
	EventTime     string           `json:"event_time,omitempty"`
	EventPosition int64            `json:"event_position,omitempty"`
}
type chatRecord struct {
	Input    protocol.ChatInput `json:"input"`
	Done     bool               `json:"done"`
	Started  bool               `json:"started"`
	Sequence int64              `json:"sequence"`
	Watch    *watchRecord       `json:"watch,omitempty"`
}
type pendingCall struct {
	Node   string           `json:"node"`
	Owner  string           `json:"owner"`
	Title  string           `json:"title,omitempty"`
	Call   protocol.Call    `json:"call"`
	Result *protocol.Result `json:"result,omitempty"`
}
type delivery struct {
	Owner  string              `json:"owner"`
	Output protocol.ChatOutput `json:"output"`
}
type historyItem struct {
	Role string `json:"role"`
	Text string `json:"text"`
}
type dialogue struct {
	Remote  string        `json:"remote_focus,omitempty"`
	Focus   string        `json:"focus,omitempty"`
	Pending string        `json:"pending,omitempty"`
	History []historyItem `json:"history"`
}
type job struct {
	Owner string
	Input protocol.ChatInput
	Watch *watchRecord
}
type Hub struct {
	c           config.Config
	db          *store.Store
	coordinator Coordinator
	mu          sync.Mutex
	workers     map[string]*connection
	channels    map[string]*connection
	waiters     map[string]chan protocol.Result
}

func New(c config.Config, coordinator Coordinator) (*Hub, error) {
	if err := c.CheckHub(); err != nil {
		return nil, err
	}
	for id := range c.Workers {
		if !protocol.ValidID(id) {
			return nil, errors.New("invalid worker ID")
		}
	}
	for id, p := range c.Channels {
		if !protocol.ValidID(id) {
			return nil, errors.New("invalid channel ID")
		}
		for _, n := range p.Nodes {
			if _, ok := c.Workers[n]; !ok {
				return nil, errors.New("channel references an unknown worker")
			}
		}
	}
	db, err := store.Open(c.StateDir)
	if err != nil {
		return nil, err
	}
	if coordinator == nil {
		coordinator = PiCoordinator{Config: c}
	}
	h := &Hub{c: c, db: db, coordinator: coordinator, workers: map[string]*connection{}, channels: map[string]*connection{}, waiters: map[string]chan protocol.Result{}}
	// Do not replay an uncertain natural-language action after Hub restart.
	m, err := db.List("inbox")
	if err != nil {
		db.Close()
		return nil, err
	}
	for key, b := range m {
		var r chatRecord
		if err = json.Unmarshal(b, &r); err != nil {
			db.Close()
			return nil, err
		}
		if r.Done || (!r.Started && r.Sequence > 0) {
			continue
		}
		owner, _, _ := strings.Cut(key, "/")
		err = db.Update(func(t *store.Tx) error {
			r.Done = true
			if e := t.Put("inbox", key, r); e != nil {
				return e
			}
			return h.putOutput(t, owner, "Hub 重启前的请求处理未能确认，未自动重发。请查询会话状态后继续。", "", "")
		})
		if err != nil {
			db.Close()
			return nil, err
		}
	}
	if err = h.recoverRemote(); err != nil {
		db.Close()
		return nil, err
	}
	return h, nil
}
func (h *Hub) Close() error {
	h.mu.Lock()
	for _, m := range []map[string]*connection{h.workers, h.channels} {
		for _, c := range m {
			c.peer.Close()
		}
	}
	h.mu.Unlock()
	return h.db.Close()
}
func (h *Hub) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/ws", h.accept)
	return m
}
func (h *Hub) allowed(owner, node string) bool {
	for _, n := range h.c.Channels[owner].Nodes {
		if n == node {
			return true
		}
	}
	return false
}

func (h *Hub) accept(w http.ResponseWriter, r *http.Request) {
	role, id := "", ""
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	for kind, peers := range map[string]map[string]config.Peer{"worker": h.c.Workers, "channel": h.c.Channels} {
		for k, v := range peers {
			if subtle.ConstantTimeCompare([]byte(token), []byte(v.Token)) == 1 {
				role, id = kind, k
			}
		}
	}
	if id == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	u := websocket.Upgrader{}
	ws, err := u.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	p := transport.New(ws)
	defer p.Close()
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	m, err := p.Read()
	if err != nil || m.Type != "hello" {
		return
	}
	hello, err := protocol.Decode[protocol.Hello](m)
	if err != nil || hello.Role != role || hello.ID != id {
		return
	}
	c := &connection{p, hello}
	h.mu.Lock()
	peers := h.workers
	if role == "channel" {
		peers = h.channels
	}
	if old := peers[id]; old != nil {
		old.peer.Close()
	}
	peers[id] = c
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if peers[id] == c {
			delete(peers, id)
		}
		h.mu.Unlock()
	}()
	if role == "worker" {
		if err = h.db.Put("nodes", id, hello); err != nil {
			return
		}
	}
	if p.Send(protocol.Wrap("welcome", "", map[string]string{"id": id})) != nil {
		return
	}
	if role == "worker" {
		calls, e := h.db.List("calls")
		if e != nil {
			return
		}
		for callID, raw := range calls {
			var c pendingCall
			if json.Unmarshal(raw, &c) != nil {
				return
			}
			if c.Node == id && (c.Result == nil || c.Result.Error != nil && c.Result.Error.Code == "unknown") {
				if p.Send(protocol.Wrap("call_status", callID, nil)) != nil {
					return
				}
			}
		}
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go p.KeepAlive(ctx)
	for {
		m, err = p.Read()
		if err != nil {
			return
		}
		h.mu.Lock()
		current := peers[id] == c
		h.mu.Unlock()
		if !current {
			return
		}
		if role == "worker" {
			switch m.Type {
			case "result":
				v, e := protocol.Decode[protocol.Result](m)
				if e != nil {
					return
				}
				if e = h.result(id, v); e != nil {
					log.Printf("worker result: %v", e)
					return
				}
			case "remote_event":
				v, e := protocol.Decode[protocol.RemoteSnapshot](m)
				if e != nil || m.ID != v.Job.ID {
					return
				}
				ack, e := h.remoteEvent(id, v)
				if e != nil {
					log.Printf("remote event: %v", e)
					return
				}
				if ack && p.Send(protocol.Wrap("remote_ack", m.ID, nil)) != nil {
					return
				}
			case "event":
				v, e := protocol.Decode[protocol.Event](m)
				if e != nil {
					return
				}
				ack, e := h.event(id, v)
				if e != nil {
					log.Printf("worker event: %v", e)
					return
				}
				if ack {
					if p.Send(protocol.Wrap("event_ack", v.ID, nil)) != nil {
						return
					}
				}
			default:
				return
			}
		} else {
			switch m.Type {
			case "chat":
				v, e := protocol.Decode[protocol.ChatInput](m)
				if e != nil || !protocol.ValidID(v.ID) || strings.TrimSpace(v.Text) == "" || len(v.Text) > protocol.MaxText {
					return
				}
				e = h.db.Update(func(t *store.Tx) error {
					var old chatRecord
					e := t.Get("inbox", id+"/"+v.ID, &old)
					if e == nil {
						if old.Input.Text != v.Text {
							return errors.New("message ID conflict")
						}
						return nil
					}
					if !errors.Is(e, store.ErrMissing) {
						return e
					}
					return h.queueInput(t, id, v)
				})
				if e != nil {
					return
				}
				if p.Send(protocol.Wrap("chat_ack", v.ID, nil)) != nil {
					return
				}
			case "output_ack":
				if m.Version != protocol.Version {
					return
				}
				var d delivery
				if e := h.db.Get("outbox", m.ID, &d); e == nil && d.Owner == id {
					if h.db.Delete("outbox", m.ID) != nil {
						return
					}
				}
			default:
				return
			}
		}
	}
}

func (h *Hub) result(node string, r protocol.Result) error {
	err := h.db.Update(func(t *store.Tx) error {
		var c pendingCall
		if e := t.Get("calls", r.ID, &c); e != nil {
			return e
		}
		if c.Node != node {
			return errors.New("result from wrong worker")
		}
		if c.Result != nil && (c.Result.Error == nil || c.Result.Error.Code != "unknown") {
			return nil
		}
		if strings.HasPrefix(c.Call.Tool, "remote.") && len(r.Data) > 0 {
			var a protocol.RemoteArgs
			var s protocol.RemoteSnapshot
			if json.Unmarshal(c.Call.Args, &a) != nil || json.Unmarshal(r.Data, &s) != nil || s.Job.ID != a.JobID {
				return errors.New("invalid shell result")
			}
			if _, e := h.mergeRemote(t, node, s); e != nil {
				return e
			}
		} else if c.Call.Tool == "remote.exec" && r.Error != nil {
			var a protocol.RemoteArgs
			var b RemoteBinding
			if e := json.Unmarshal(c.Call.Args, &a); e != nil {
				return e
			}
			if e := t.Get("remote_jobs", a.JobID, &b); e != nil {
				return e
			}
			if b.Snapshot.Job.Revision == 0 {
				b.Snapshot.Job.Status = "failed"
				if r.Error.Code == "unknown" {
					b.Snapshot.Job.Status = "unknown"
				}
				b.Snapshot.Job.Error = r.Error.Message
				if e := t.Put("remote_jobs", a.JobID, b); e != nil {
					return e
				}
			}
		}
		c.Result = &r
		if c.Call.Tool == "session.create" && len(r.Data) > 0 {
			var s protocol.Session
			if e := json.Unmarshal(r.Data, &s); e != nil || !protocol.ValidID(s.ID) {
				return errors.New("invalid created session")
			}
			s.Node = node
			var existing Binding
			if e := t.Get("sessions", s.ID, &existing); errors.Is(e, store.ErrMissing) {
				if e = t.Put("sessions", s.ID, Binding{Session: s, Owner: c.Owner, Title: c.Title}); e != nil {
					return e
				}
			} else if e != nil {
				return e
			}
		}
		return t.Put("calls", r.ID, c)
	})
	if err != nil {
		return err
	}
	h.mu.Lock()
	ch := h.waiters[r.ID]
	h.mu.Unlock()
	if ch != nil {
		select {
		case ch <- r:
		default:
		}
	}
	return nil
}
func (h *Hub) call(ctx context.Context, owner, node, tool, title string, args any) (protocol.Result, error) {
	if err := ctx.Err(); err != nil {
		return protocol.Result{}, err
	}
	if !h.allowed(owner, node) {
		return protocol.Result{}, errors.New("无权操作这台机器")
	}
	h.mu.Lock()
	peer := h.workers[node]
	h.mu.Unlock()
	if peer == nil {
		return protocol.Result{}, errors.New("目标 Worker 离线；未派发操作")
	}
	c := protocol.Call{ID: protocol.ID("c_"), Tool: tool, Args: protocol.JSON(args)}
	if err := h.db.Update(func(t *store.Tx) error {
		if tool == "remote.exec" {
			var a protocol.RemoteArgs
			if err := json.Unmarshal(c.Args, &a); err != nil {
				return err
			}
			var b RemoteBinding
			if err := t.Get("remote_jobs", a.JobID, &b); err != nil {
				return err
			}
			if b.Owner != owner || b.Snapshot.Job.Node != node || b.Snapshot.Job.CallID != "" {
				return errors.New("invalid shell dispatch ownership")
			}
			b.Snapshot.Job.CallID = c.ID
			if err := t.Put("remote_jobs", a.JobID, b); err != nil {
				return err
			}
		}
		return t.Put("calls", c.ID, pendingCall{Node: node, Owner: owner, Title: title, Call: c})
	}); err != nil {
		return protocol.Result{}, err
	}
	ch := make(chan protocol.Result, 1)
	h.mu.Lock()
	h.waiters[c.ID] = ch
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.waiters, c.ID); h.mu.Unlock() }()
	if err := peer.peer.Send(protocol.Wrap("call", c.ID, c)); err != nil {
		return protocol.Result{}, fmt.Errorf("%w: 发送结果未知；请先查询会话状态", ErrUncertain)
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	select {
	case <-ctx.Done():
		return protocol.Result{}, fmt.Errorf("%w: 调用回执超时，执行结果未知；不会自动重发，请先查询状态", ErrUncertain)
	case r := <-ch:
		if r.Error != nil {
			if r.Error.Code == "unknown" {
				return r, fmt.Errorf("%w: %s", ErrUncertain, r.Error.Message)
			}
			return r, fmt.Errorf("%s: %s", r.Error.Code, r.Error.Message)
		}
		return r, nil
	}
}

func (h *Hub) putOutput(t *store.Tx, owner, text, session, run string) error {
	return h.queueOutput(t, owner, protocol.ChatOutput{Text: text, Session: session, RunID: run})
}
func (h *Hub) queueOutput(t *store.Tx, owner string, o protocol.ChatOutput) error {
	var seq int64
	if err := t.Get("meta", "output_sequence", &seq); err != nil && !errors.Is(err, store.ErrMissing) {
		return err
	}
	seq++
	if err := t.Put("meta", "output_sequence", seq); err != nil {
		return err
	}
	o.ID, o.Sequence = protocol.ID("out_"), seq
	return t.Put("outbox", o.ID, delivery{owner, o})
}
func (h *Hub) event(node string, e protocol.Event) (bool, error) {
	if !protocol.ValidID(e.ID) || !protocol.ValidID(e.Session) || !protocol.ValidID(e.Instance) || len(e.Text) > protocol.MaxText+4 {
		return false, errors.New("invalid event")
	}
	if _, err := time.Parse(time.RFC3339Nano, e.Time); err != nil {
		return false, err
	}
	ack := false
	err := h.db.Update(func(t *store.Tx) error {
		var b Binding
		if err := t.Get("sessions", e.Session, &b); errors.Is(err, store.ErrMissing) {
			return nil
		} else if err != nil {
			return err
		}
		if b.Session.Node != node {
			return errors.New("event from wrong worker")
		}
		ack = true
		var seen bool
		if err := t.Get("events", e.ID, &seen); err == nil {
			return nil
		} else if !errors.Is(err, store.ErrMissing) {
			return err
		}
		if err := t.Put("events", e.ID, true); err != nil {
			return err
		}
		var seq int64
		key := e.Session + "/" + e.Instance
		if err := t.Get("sequences", key, &seq); err != nil && !errors.Is(err, store.ErrMissing) {
			return err
		}
		if e.Instance != "launcher" && e.Seq <= seq {
			return nil
		}
		if err := t.Put("sequences", key, e.Seq); err != nil {
			return err
		}
		// Old replay may be recorded, but must not regress the visible state.
		stamp, _ := time.Parse(time.RFC3339Nano, e.Time)
		last, _ := time.Parse(time.RFC3339Nano, b.EventTime)
		if (e.Position > 0 && e.Position <= b.EventPosition) || (e.Position == 0 && stamp.Before(last)) || b.Session.Status == "closed" {
			return nil
		}
		if e.Position > 0 {
			b.EventPosition = e.Position
		}
		b.EventTime = e.Time
		b.Session.Updated = e.Time
		if e.Kind == "ready" && b.Session.Native != "" && b.Session.Native != e.Native {
			b.Session.RunID = ""
			b.Outcome = ""
			b.LastText = ""
		}
		if e.Instance != "launcher" {
			b.Session.Instance = e.Instance
			b.Session.Native = e.Native
		}
		if e.RunID != "" && e.RunID != b.Session.RunID {
			b.Session.RunID = e.RunID
			b.Outcome = ""
			b.LastText = ""
		}
		prefix := "[" + b.Session.Node + " / " + b.Session.Workspace + "] "
		text := ""
		switch e.Kind {
		case "ready":
			b.Session.Status = e.Status
		case "input":
			if e.Source != "hub" {
				text = prefix + "本地输入：" + e.Text
			}
		case "started":
			b.Session.Status = "running"
			b.Outcome = ""
			text = prefix + "已开始执行。"
		case "output":
			b.LastText = e.Text
			text = prefix + e.Text
		case "waiting":
			b.Session.Status = "waiting"
			text = prefix + "pi 正在等待输入：" + e.Text + "。当前版本请在该 pi 终端中处理。"
		case "status":
			b.Session.Status = e.Status
		case "settled":
			b.Session.Status = "idle"
			b.Outcome = e.Status
			text = prefix + "本轮" + outcome(e.Status) + "。"
			if e.Text != "" {
				text += " " + e.Text
			}
		case "exited":
			b.Session.Status = "exited"
			if b.Outcome == "" {
				b.Outcome = "unknown"
			}
			text = prefix + "pi 已退出：" + e.Text
		case "disconnected":
			b.Session.Status = "unknown"
		default:
			return errors.New("unknown event kind")
		}
		if err := t.Put("sessions", e.Session, b); err != nil {
			return err
		}
		if e.RunID != "" {
			var run map[string]string
			if err := t.Get("runs", e.RunID, &run); err != nil {
				if !errors.Is(err, store.ErrMissing) {
					return err
				}
				run = map[string]string{"session_id": e.Session, "owner": b.Owner, "source": e.Source}
			}
			if run["session_id"] != e.Session {
				return errors.New("run belongs to another session")
			}
			if e.Kind == "input" && run["text"] == "" {
				run["text"] = e.Text
			}
			if e.Kind == "started" {
				run["status"] = "running"
			}
			if e.Kind == "settled" {
				run["status"] = e.Status
			}
			if err := t.Put("runs", e.RunID, run); err != nil {
				return err
			}
		}
		if e.Kind == "settled" {
			var w watchRecord
			if err := t.Get("watches", e.Session, &w); err == nil {
				if w.RunID == e.RunID {
					if err = h.fireWatch(t, w); err != nil {
						return err
					}
				}
			} else if !errors.Is(err, store.ErrMissing) {
				return err
			}
		}
		if text != "" {
			return h.putOutput(t, b.Owner, text, e.Session, e.RunID)
		}
		return nil
	})
	return ack, err
}
func outcome(s string) string {
	switch s {
	case "completed":
		return "响应结束"
	case "failed":
		return "执行失败"
	case "cancelled":
		return "已中断"
	default:
		return "结果待确认"
	}
}

// Serve runs background dispatch. HTTP hosting is separate for integration tests.
func (h *Hub) Serve(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				j, err := h.nextJob()
				if err != nil {
					log.Printf("hub inbox: %v", err)
					continue
				}
				if j != nil {
					h.process(ctx, *j)
				}
			}
		}
	}()
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m, err := h.db.List("outbox")
			if err != nil {
				log.Printf("hub outbox: %v", err)
				continue
			}
			ids := make([]string, 0, len(m))
			for id := range m {
				ids = append(ids, id)
			}
			sort.Slice(ids, func(i, j int) bool {
				var a, b delivery
				_ = json.Unmarshal(m[ids[i]], &a)
				_ = json.Unmarshal(m[ids[j]], &b)
				return a.Output.Sequence < b.Output.Sequence
			})
			for _, id := range ids {
				var d delivery
				if json.Unmarshal(m[id], &d) != nil {
					continue
				}
				h.mu.Lock()
				c := h.channels[d.Owner]
				h.mu.Unlock()
				if c != nil {
					if c.peer.Send(protocol.Wrap("output", id, d.Output)) != nil {
						c.peer.Close()
					}
				}
			}
		}
	}
}
