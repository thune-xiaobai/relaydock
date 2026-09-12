package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"relaydock/internal/protocol"
	"relaydock/internal/store"
)

// Snapshot holds the first output page only. Arbitrary cursor pages are read
// from the Worker; they must never overwrite this offline preview.
type RemoteBinding struct {
	Owner    string                  `json:"owner"`
	Snapshot protocol.RemoteSnapshot `json:"snapshot"`
	Notified bool                    `json:"notified,omitempty"`
}

func (h *Hub) recoverRemote() error {
	return h.db.Update(func(t *store.Tx) error {
		all, err := t.List("remote_jobs")
		if err != nil {
			return err
		}
		for id, raw := range all {
			var b RemoteBinding
			if err := json.Unmarshal(raw, &b); err != nil {
				return err
			}
			if b.Snapshot.Job.Revision == 0 && b.Snapshot.Job.Status == "starting" {
				b.Snapshot.Job.Status = "unknown"
				b.Snapshot.Job.Error = "Hub restarted before launch was confirmed; query the existing job, do not replay."
				if err := t.Put("remote_jobs", id, b); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func remoteDefinitions() []toolDefinition {
	exec := tool("remote_exec", "Execute a noninteractive command on an authorized Worker. cwd is an advertised workspace alias or absolute existing directory. Uses advertised shell, independent of other calls. Returns a job and bounded stdout/stderr; completion is notified automatically. Does not start an interactive pi session.", []string{"node", "command", "cwd"}, "node", "command", "cwd")
	status := tool("remote_status", "Read an owned shell job and incremental output using next_cursor. Omitting cursor reads from the beginning. Offline returns only cached preview, explicitly marked stale. Never reruns a command.", []string{"job_id"}, "job_id", "cursor")
	cancel := tool("remote_cancel", "Request cancellation of an owned shell job when authorized by the user. Already finished jobs are unchanged; unknown old processes cannot be cancelled by saved PID.", []string{"job_id"}, "job_id")
	integer := func(t toolDefinition, name string, min, max int) {
		t.Parameters.(map[string]any)["properties"].(map[string]any)[name] = map[string]any{"type": "integer", "minimum": min, "maximum": max}
	}
	integer(exec, "timeout_ms", 1, 86400000)
	integer(exec, "wait_ms", 0, 2000)
	integer(status, "limit", 4, 32768)
	return []toolDefinition{exec, status, cancel}
}

func validateArguments(def toolDefinition, raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return errors.New("tool arguments must be an object")
	}
	schema := def.Parameters.(map[string]any)
	props := schema["properties"].(map[string]any)
	for k, raw := range fields {
		v, ok := props[k]
		if !ok || string(raw) == "null" {
			return fmt.Errorf("invalid argument %q", k)
		}
		p := v.(map[string]any)
		switch p["type"] {
		case "string":
			var s string
			if json.Unmarshal(raw, &s) != nil || strings.TrimSpace(s) == "" || len(s) > protocol.MaxText {
				return fmt.Errorf("invalid argument %q", k)
			}
		case "integer":
			var n int
			if json.Unmarshal(raw, &n) != nil || n < p["minimum"].(int) || n > p["maximum"].(int) {
				return fmt.Errorf("invalid argument %q", k)
			}
		default:
			return errors.New("unsupported tool schema")
		}
	}
	for _, k := range schema["required"].([]string) {
		if _, ok := fields[k]; !ok {
			return fmt.Errorf("missing argument %s", k)
		}
	}
	return nil
}

func (h *Hub) remoteOwned(owner, id string) (RemoteBinding, error) {
	var b RemoteBinding
	if err := h.db.Get("remote_jobs", id, &b); err != nil {
		return b, errors.New("shell job not found")
	}
	if b.Owner != owner || !h.allowed(owner, b.Snapshot.Job.Node) {
		return RemoteBinding{}, errors.New("shell job is not authorized")
	}
	return b, nil
}

func (h *Hub) remoteTool(ctx context.Context, owner string, d *dialogue, name string, raw json.RawMessage) (any, error) {
	var a struct {
		Node, Command, CWD, Cursor string
		JobID                      string `json:"job_id"`
		TimeoutMS                  int    `json:"timeout_ms"`
		WaitMS                     *int   `json:"wait_ms"`
		Limit                      int
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if name == "remote_exec" {
		if !h.allowed(owner, a.Node) {
			return nil, errors.New("target node is not authorized")
		}
		var n protocol.Hello
		if err := h.db.Get("nodes", a.Node, &n); err != nil {
			return nil, err
		}
		if n.Shell == nil {
			return nil, errors.New("Worker does not advertise remote shell")
		}
		if a.TimeoutMS > n.Shell.MaxTimeoutMS {
			return nil, errors.New("timeout_ms exceeds Worker shell limit")
		}
		a.JobID = protocol.ID("job_")
		b := RemoteBinding{Owner: owner, Snapshot: protocol.RemoteSnapshot{Job: protocol.RemoteJob{ID: a.JobID, Node: a.Node, Command: a.Command, CWD: a.CWD, Shell: n.Shell.Kind, Status: "starting", CreatedAt: protocol.Now()}}}
		// Ownership exists before dispatch so even an immediate completion can
		// be safely attributed to the initiating channel.
		if err := h.db.Put("remote_jobs", a.JobID, b); err != nil {
			return nil, err
		}
		d.Focus, d.Remote = "", a.JobID
		r, err := h.call(ctx, owner, a.Node, "remote.exec", "", protocol.RemoteArgs{JobID: a.JobID, Command: a.Command, CWD: a.CWD, TimeoutMS: a.TimeoutMS})
		if err != nil {
			if e := h.db.Update(func(t *store.Tx) error {
				if e := t.Get("remote_jobs", a.JobID, &b); e != nil {
					return e
				}
				if b.Snapshot.Job.Revision == 0 {
					b.Snapshot.Job.Status = "failed"
					if errors.Is(err, ErrUncertain) {
						b.Snapshot.Job.Status = "unknown"
					}
					b.Snapshot.Job.Error = err.Error()
					return t.Put("remote_jobs", a.JobID, b)
				}
				return nil
			}); e != nil {
				return b.Snapshot, fmt.Errorf("%w: %v; %v", ErrUncertain, err, e)
			}
			return b.Snapshot, err
		}
		var s protocol.RemoteSnapshot
		if err := json.Unmarshal(r.Data, &s); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUncertain, err)
		}
		waitMS := 1000
		if a.WaitMS != nil {
			waitMS = *a.WaitMS
		}
		waitCtx, cancel := context.WithTimeout(ctx, time.Duration(waitMS)*time.Millisecond)
		defer cancel()
		for !s.Job.Terminal() && waitMS > 0 {
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-waitCtx.Done():
				timer.Stop()
				return remoteReply(s)
			case <-timer.C:
			}
			r, e := h.call(waitCtx, owner, a.Node, "remote.status", "", protocol.RemoteArgs{JobID: a.JobID})
			if e != nil {
				break
			} // Accepted jobs keep running if a status read fails.
			if json.Unmarshal(r.Data, &s) != nil {
				break
			}
		}
		return remoteReply(s)
	}
	b, err := h.remoteOwned(owner, a.JobID)
	if err != nil {
		return nil, err
	}
	d.Focus, d.Remote = "", a.JobID
	h.mu.Lock()
	online := h.workers[b.Snapshot.Job.Node] != nil
	h.mu.Unlock()
	if name == "remote_status" && !online {
		// A stale cached page has no useful continuation cursor while offline.
		data := map[string]any{"snapshot": b.Snapshot, "stale": true, "offline": true, "note": "Cached first page only; requested cursor/limit not applied. Reconnect Worker to read live status and remaining output."}
		if b.Snapshot.Job.Status == "unknown" {
			return data, ErrUncertain
		}
		return data, nil
	}
	wire := "remote.status"
	if name == "remote_cancel" {
		wire = "remote.cancel"
	}
	r, err := h.call(ctx, owner, b.Snapshot.Job.Node, wire, "", protocol.RemoteArgs{JobID: a.JobID, Cursor: a.Cursor, Limit: a.Limit})
	if err != nil {
		return b.Snapshot, err
	}
	var s protocol.RemoteSnapshot
	if err = json.Unmarshal(r.Data, &s); err != nil {
		return nil, err
	}
	return remoteReply(s)
}

func remoteReply(s protocol.RemoteSnapshot) (any, error) {
	if s.Job.Status == "unknown" {
		return s, fmt.Errorf("%w: shell job status unknown; do not rerun", ErrUncertain)
	}
	return s, nil
}

// Merge is shared by call receipts and async events. A delayed running receipt
// must not regress a terminal event, and cursor pages must not erase previews.
func (h *Hub) mergeRemote(t *store.Tx, node string, s protocol.RemoteSnapshot) (RemoteBinding, error) {
	var b RemoteBinding
	if err := t.Get("remote_jobs", s.Job.ID, &b); err != nil {
		return b, err
	}
	if b.Snapshot.Job.Node != node || s.Job.Node != node || s.Job.CallID == "" || s.Job.CallID != b.Snapshot.Job.CallID {
		return b, errors.New("shell result from wrong job or Worker")
	}
	if s.Job.Revision < 1 || (!s.Job.Terminal() && s.Job.Status != "starting" && s.Job.Status != "running") || len(s.Stdout) > 3*32768 || len(s.Stderr) > 3*32768 {
		return b, errors.New("invalid shell snapshot")
	}
	old := b.Snapshot.Job
	if s.Job.Revision < old.Revision || old.Revision > 0 && old.Terminal() && !s.Job.Terminal() {
		return b, nil
	}
	if s.Cursor == "" {
		b.Snapshot = s
	} else {
		b.Snapshot.Job = s.Job
		b.Snapshot.CancelRequested = s.CancelRequested
	}
	return b, t.Put("remote_jobs", s.Job.ID, b)
}

func (h *Hub) remoteEvent(node string, s protocol.RemoteSnapshot) (bool, error) {
	if !protocol.ValidID(s.Job.ID) || !s.Job.Terminal() {
		return false, errors.New("invalid shell completion")
	}
	ack := false
	err := h.db.Update(func(t *store.Tx) error {
		b, err := h.mergeRemote(t, node, s)
		if errors.Is(err, store.ErrMissing) {
			return nil
		}
		if err != nil {
			return err
		}
		ack = true
		if b.Notified {
			return nil
		}
		b.Notified = true
		if err := t.Put("remote_jobs", s.Job.ID, b); err != nil {
			return err
		}
		if !h.allowed(b.Owner, node) {
			return nil
		}
		j := b.Snapshot.Job
		label := map[string]string{"succeeded": "已结束", "failed": "执行失败", "cancelled": "已取消", "timed_out": "已超时", "unknown": "结果未知，未自动重跑"}[j.Status]
		text := fmt.Sprintf("[%s / shell / %s] %s", node, j.ID, label)
		if j.ExitCode != nil {
			text += fmt.Sprintf("（退出码 %d）", *j.ExitCode)
		}
		text += "\n" + short(j.Command, 240)
		if b.Snapshot.Stdout != "" {
			text += "\nstdout:\n" + short(b.Snapshot.Stdout, 1800)
		}
		if b.Snapshot.Stderr != "" {
			text += "\nstderr:\n" + short(b.Snapshot.Stderr, 1200)
		}
		if j.Error != "" {
			text += "\n" + short(j.Error, 300)
		}
		if j.Truncated {
			text += "\n输出超过保存上限，后续内容已丢弃。"
		} else if b.Snapshot.HasMore || len([]rune(b.Snapshot.Stdout)) > 1800 || len([]rune(b.Snapshot.Stderr)) > 1200 {
			text += "\n这里只显示部分输出，可以继续查询。"
		}
		return h.queueOutput(t, b.Owner, protocol.ChatOutput{Text: text, JobID: j.ID})
	})
	return ack, err
}
