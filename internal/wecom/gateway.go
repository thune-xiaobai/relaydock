package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"relaydock/internal/config"
	"relaydock/internal/protocol"
	"relaydock/internal/store"
)

type checkpoint struct {
	Messages []Message
	Pending  []protocol.ChatInput
}
type Delivery struct {
	Output    protocol.ChatOutput `json:"output"`
	Status    string              `json:"status"`
	Before    []Message           `json:"before,omitempty"`
	AttemptAt string              `json:"attempt_at,omitempty"`
	Error     string              `json:"error,omitempty"`
}
type Gateway struct {
	c       config.WeCom
	db      *store.Store
	desktop Desktop
	input   func(protocol.ChatInput) error
}

func OpenState(dir string) (*store.Store, error) { return store.Open(filepath.Join(dir, "wecom")) }
func New(c config.Config, d Desktop, input func(protocol.ChatInput) error) (*Gateway, error) {
	if c.WeCom == nil {
		return nil, errors.New("wecom configuration required")
	}
	db, err := OpenState(c.StateDir)
	if err != nil {
		return nil, err
	}
	g := &Gateway{c: *c.WeCom, db: db, desktop: d, input: input}
	bound := map[string]string{"channel": c.ID, "app": c.WeCom.App, "window": c.WeCom.WindowTitle, "chat": c.WeCom.ChatTitle, "peer": c.WeCom.Peer, "self": c.WeCom.Self}
	err = db.Update(func(t *store.Tx) error {
		var old map[string]string
		if e := t.Get("meta", "binding", &old); e == nil {
			if !reflect.DeepEqual(old, bound) {
				return errors.New("wecom state belongs to another chat/source; use a separate state_dir")
			}
			return nil
		} else if !errors.Is(e, store.ErrMissing) {
			return e
		}
		return t.Put("meta", "binding", bound)
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	// An interrupted send can be verified by observation, but never dispatched again.
	return g, nil
}
func (g *Gateway) Close() error { return g.db.Close() }
func (g *Gateway) EnqueueOutput(o protocol.ChatOutput) error {
	if !protocol.ValidID(o.ID) || o.Text == "" {
		return errors.New("invalid output")
	}
	return g.db.Update(func(t *store.Tx) error {
		var old Delivery
		if e := t.Get("deliveries", o.ID, &old); e == nil {
			if !reflect.DeepEqual(old.Output, o) {
				return errors.New("output ID conflict")
			}
			return nil
		} else if !errors.Is(e, store.ErrMissing) {
			return e
		}
		return t.Put("deliveries", o.ID, Delivery{Output: o, Status: "queued"})
	})
}

// delta locates a unique suffix of the previous observation in the new one.
// Stable IDs are optional; without them require up to three consecutive anchors.
// No overlap or ambiguous repeated anchors pause ingestion instead of guessing.
func delta(before, after []Message) ([]Message, error) {
	for _, list := range [][]Message{before, after} {
		ids := map[string]bool{}
		for _, m := range list {
			if m.ID != "" {
				if ids[m.ID] {
					return nil, errors.New("duplicate stable message ID")
				}
				ids[m.ID] = true
			}
		}
	}
	if reflect.DeepEqual(before, after) {
		return nil, nil
	}
	if len(before) == 0 || len(after) == 0 {
		return nil, errors.New("no message anchor available")
	}
	for _, a := range before {
		if a.ID != "" {
			for _, b := range after {
				if a.ID == b.ID && a != b {
					return nil, errors.New("message changed under the same identity")
				}
			}
		}
	}
	minimum := 3
	if len(before) < minimum {
		minimum = len(before)
	}
	// A real message identity is sufficient for a one-row anchor.
	if before[len(before)-1].ID != "" {
		minimum = 1
	}
	for k := len(before); k >= minimum; k-- {
		start := -1
		matches := 0
		for i := 0; i+k <= len(after); i++ {
			if reflect.DeepEqual(before[len(before)-k:], after[i:i+k]) {
				matches++
				start = i
			}
		}
		if matches > 1 {
			return nil, errors.New("ambiguous repeated messages; checkpoint unchanged")
		}
		if matches == 1 {
			return after[start+k:], nil
		}
	}
	return nil, errors.New("message continuity lost; restore the latest conversation view or establish a new baseline")
}
func (g *Gateway) ingest(s Snapshot) error {
	var cp checkpoint
	err := g.db.Get("meta", "checkpoint", &cp)
	if errors.Is(err, store.ErrMissing) {
		if len(s.Messages) == 0 {
			return errors.New("cannot baseline an empty message view")
		}
		if err = g.db.Put("meta", "checkpoint", checkpoint{Messages: s.Messages}); err != nil {
			return err
		}
		log.Printf("wecom baseline established (%d existing messages skipped); ready for new input", len(s.Messages))
		return nil
	}
	if err != nil {
		return err
	}
	// Replay durable IDs if Channel acceptance was interrupted.
	for _, in := range cp.Pending {
		if err = g.input(in); err != nil {
			return err
		}
	}
	cp.Pending = nil
	added, err := delta(cp.Messages, s.Messages)
	if err != nil {
		return err
	}
	for _, m := range added {
		if m.Sender == g.c.Peer {
			if m.Text == "" || len(m.Text) > protocol.MaxText {
				return errors.New("incoming message exceeds Hub text limit; checkpoint unchanged")
			}
			cp.Pending = append(cp.Pending, protocol.ChatInput{ID: protocol.ID("in_"), Text: m.Text, ObservedAt: protocol.Now()})
		}
	}
	cp.Messages = s.Messages
	if err = g.db.Put("meta", "checkpoint", cp); err != nil {
		return err
	}
	for _, in := range cp.Pending {
		if err = g.input(in); err != nil {
			return err
		}
	}
	cp.Pending = nil
	return g.db.Put("meta", "checkpoint", cp)
}
func (g *Gateway) oldest() (*Delivery, error) {
	all, err := g.db.List("deliveries")
	if err != nil {
		return nil, err
	}
	var queue []Delivery
	for _, raw := range all {
		var d Delivery
		if err = json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		if d.Status != "sent" {
			queue = append(queue, d)
		}
	}
	sort.Slice(queue, func(i, j int) bool { return queue[i].Output.Sequence < queue[j].Output.Sequence })
	if len(queue) == 0 {
		return nil, nil
	}
	return &queue[0], nil
}
func (g *Gateway) Step(ctx context.Context) error {
	s, err := g.desktop.Read(ctx)
	if err != nil {
		return err
	}
	if err = g.ingest(s); err != nil {
		return err
	}
	d, err := g.oldest()
	if err != nil || d == nil {
		return err
	}
	if d.Status == "unknown" {
		return fmt.Errorf("output %s needs delivery resolution: %s", d.Output.ID, d.Error)
	}
	if d.Status == "sending" {
		added, e := delta(d.Before, s.Messages)
		matches := 0
		if e == nil {
			for _, m := range added {
				if m.Sender == g.c.Self && canonicalText(m.Text) == canonicalText(d.Output.Text) {
					matches++
				}
			}
		}
		if matches == 1 {
			d.Status = "sent"
			d.Error = ""
			return g.db.Put("deliveries", d.Output.ID, d)
		}
		at, parseErr := time.Parse(time.RFC3339Nano, d.AttemptAt)
		if e != nil || matches > 1 || parseErr != nil || time.Since(at) > 10*time.Second {
			d.Status = "unknown"
			d.Error = "send not verified by a unique new outgoing message; no automatic resend"
			if e = g.db.Put("deliveries", d.Output.ID, d); e != nil {
				return e
			}
			return fmt.Errorf("output %s: %s", d.Output.ID, d.Error)
		}
		return nil
	}
	if d.Status != "queued" {
		return errors.New("invalid stored delivery status")
	}
	d.Status = "sending"
	d.Before = s.Messages
	d.AttemptAt = protocol.Now()
	if err = g.db.Put("deliveries", d.Output.ID, d); err != nil {
		return err
	}
	if err = g.desktop.Send(ctx, d.Output.Text); err != nil {
		// Even a failed action may have sent. The next observation only verifies it.
		d.Error = err.Error()
		if e := g.db.Put("deliveries", d.Output.ID, d); e != nil {
			return e
		}
		return err
	}
	return nil
}
func (g *Gateway) Run(ctx context.Context) {
	ms := g.c.PollMS
	if ms < 500 {
		ms = 2000
	}
	tick := time.NewTicker(time.Duration(ms) * time.Millisecond)
	defer tick.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			err := g.Step(ctx)
			message := ""
			if err != nil {
				message = err.Error()
			}
			if message != last {
				if message != "" {
					log.Printf("wecom: %s", message)
				} else if last != "" {
					log.Print("wecom workflow resumed")
				}
				last = message
			}
		}
	}
}

// Resolve is an explicit local operator decision, never an automatic retry.
func Resolve(db *store.Store, id, status string) error {
	if status != "sent" && status != "retry" {
		return errors.New("delivery must be sent or retry")
	}
	var d Delivery
	if err := db.Get("deliveries", id, &d); err != nil {
		return err
	}
	if d.Status != "unknown" && d.Status != "sending" {
		return errors.New("only uncertain deliveries may be resolved")
	}
	d.Status = "sent"
	if status == "retry" {
		d.Status = "queued"
	}
	d.Error = ""
	d.Before = nil
	d.AttemptAt = ""
	return db.Put("deliveries", id, d)
}

// Baseline deliberately skips the visible history. Pending accepted inputs must
// be drained first; delivery records remain untouched.
func (g *Gateway) Baseline(ctx context.Context) error {
	s, err := g.desktop.Read(ctx)
	if err != nil {
		return err
	}
	if len(s.Messages) == 0 {
		return errors.New("cannot baseline an empty message view")
	}
	return g.db.Update(func(t *store.Tx) error {
		var cp checkpoint
		if e := t.Get("meta", "checkpoint", &cp); e != nil && !errors.Is(e, store.ErrMissing) {
			return e
		}
		if len(cp.Pending) > 0 {
			return errors.New("pending input acceptance must complete before rebaselining")
		}
		return t.Put("meta", "checkpoint", checkpoint{Messages: s.Messages})
	})
}
