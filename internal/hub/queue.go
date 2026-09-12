package hub

import (
	"encoding/json"
	"errors"
	"strings"

	"relaydock/internal/protocol"
	"relaydock/internal/store"
)

type watchRecord struct {
	ID          string `json:"id"`
	Owner       string `json:"owner"`
	Session     string `json:"session_id"`
	RunID       string `json:"run_id"`
	Instruction string `json:"instruction"`
	Fired       bool   `json:"fired"`
}

func (h *Hub) queueInput(t *store.Tx, owner string, input protocol.ChatInput) error {
	var seq int64
	if err := t.Get("meta", "input_sequence", &seq); err != nil && !errors.Is(err, store.ErrMissing) {
		return err
	}
	seq++
	if err := t.Put("meta", "input_sequence", seq); err != nil {
		return err
	}
	return t.Put("inbox", owner+"/"+input.ID, chatRecord{Input: input, Sequence: seq})
}
func (h *Hub) nextJob() (*job, error) {
	var next *job
	err := h.db.Update(func(t *store.Tx) error {
		all, err := t.List("inbox")
		if err != nil {
			return err
		}
		key := ""
		var record chatRecord
		for k, raw := range all {
			var r chatRecord
			if err = json.Unmarshal(raw, &r); err != nil {
				return err
			}
			if !r.Done && !r.Started && (key == "" || r.Sequence < record.Sequence) {
				key = k
				record = r
			}
		}
		if key == "" {
			return nil
		}
		owner, _, _ := strings.Cut(key, "/")
		if record.Watch != nil {
			var w watchRecord
			err = t.Get("watches", record.Watch.Session, &w)
			if err != nil && !errors.Is(err, store.ErrMissing) {
				return err
			}
			var b Binding
			be := t.Get("sessions", record.Watch.Session, &b)
			if be != nil && !errors.Is(be, store.ErrMissing) {
				return be
			}
			if err != nil || w.ID != record.Watch.ID || be != nil || b.Session.RunID != w.RunID || b.Session.Status == "closed" || !h.allowed(owner, b.Session.Node) {
				record.Done = true
				if e := t.Put("inbox", key, record); e != nil {
					return e
				}
				return h.putOutput(t, owner, "会话已变化或后续安排已取消，本次自动后续未执行。", record.Watch.Session, record.Watch.RunID)
			}
		}
		record.Started = true
		if err = t.Put("inbox", key, record); err != nil {
			return err
		}
		next = &job{Owner: owner, Input: record.Input, Watch: record.Watch}
		return nil
	})
	return next, err
}

func (h *Hub) fireWatch(t *store.Tx, w watchRecord) error {
	if w.Fired {
		return nil
	}
	w.Fired = true
	input := protocol.ChatInput{ID: w.ID, Text: w.Instruction, ObservedAt: protocol.Now()}
	if err := h.queueInput(t, w.Owner, input); err != nil {
		return err
	}
	var r chatRecord
	if err := t.Get("inbox", w.Owner+"/"+w.ID, &r); err != nil {
		return err
	}
	r.Watch = &w
	if err := t.Put("inbox", w.Owner+"/"+w.ID, r); err != nil {
		return err
	}
	return t.Put("watches", w.Session, w)
}
func (h *Hub) watch(owner string, b Binding, runID, instruction string) (any, error) {
	w := watchRecord{ID: protocol.ID("watch_"), Owner: owner, Session: b.Session.ID, RunID: runID, Instruction: instruction}
	err := h.db.Update(func(t *store.Tx) error {
		var run map[string]string
		if err := t.Get("runs", runID, &run); err != nil {
			return err
		}
		if run["session_id"] != w.Session || run["owner"] != owner {
			return errors.New("run does not belong to this session/channel")
		}
		// Re-read inside the transaction: a completion can race watch registration.
		var current Binding
		if err := t.Get("sessions", w.Session, &current); err != nil {
			return err
		}
		if current.Session.Status == "closed" || current.Session.RunID != "" && current.Session.RunID != runID && run["status"] != "submitting" {
			return errors.New("session has switched to another run")
		}
		var old watchRecord
		if e := t.Get("watches", w.Session, &old); e == nil && old.RunID == runID {
			if old.Instruction != instruction {
				return errors.New("this run already has a follow-up")
			}
			w = old
			return nil
		} else if e != nil && !errors.Is(e, store.ErrMissing) {
			return e
		}
		switch run["status"] {
		case "completed", "failed", "cancelled":
			return h.fireWatch(t, w)
		}
		return t.Put("watches", w.Session, w)
	})
	return map[string]any{"watch_id": w.ID, "run_id": runID, "registered": err == nil}, err
}
func (h *Hub) removeWatch(session string) error { return h.db.Delete("watches", session) }
