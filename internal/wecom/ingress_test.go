package wecom

import (
	"errors"
	"strings"
	"testing"

	"relaydock/internal/config"
	"relaydock/internal/protocol"
)

func TestInvalidPendingAndNewInputsDoNotPoisonCheckpoint(t *testing.T) {
	var accepted []string
	fail := true
	g, err := New(config.Config{StateDir: t.TempDir(), WeCom: &config.WeCom{Peer: "peer", Self: "self"}}, &fakeDesktop{}, func(in protocol.ChatInput) error {
		if in.Text == "conflict" {
			return protocol.ErrInvalidInput
		}
		if fail {
			return errors.New("temporary storage failure")
		}
		accepted = append(accepted, in.Text)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	anchor := []Message{{ID: "anchor", Sender: "peer", Text: "baseline"}}
	if err := g.db.Put("meta", "checkpoint", checkpoint{Messages: anchor, Pending: []protocol.ChatInput{{ID: "old", Text: "  "}, {ID: "conflict", Text: "conflict"}, {ID: "retry", Text: "old valid"}}}); err != nil {
		t.Fatal(err)
	}
	after := append(append([]Message{}, anchor...), Message{ID: "blank", Sender: "peer", Text: "\t"}, Message{ID: "huge", Sender: "peer", Text: strings.Repeat("x", protocol.MaxText+1)}, Message{ID: "good", Sender: "peer", Text: "new valid"})
	if err := g.ingest(Snapshot{Messages: after}); err == nil {
		t.Fatal("transient failure was dropped")
	}
	fail = false
	if err := g.ingest(Snapshot{Messages: after}); err != nil {
		t.Fatal(err)
	}
	if err := g.ingest(Snapshot{Messages: after}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(accepted, ",") != "old valid,new valid" {
		t.Fatal(accepted)
	}
	var cp checkpoint
	if err := g.db.Get("meta", "checkpoint", &cp); err != nil || len(cp.Pending) != 0 {
		t.Fatal(cp, err)
	}
	if m, err := g.db.List("rejected_inputs"); err != nil || len(m) != 4 {
		t.Fatalf("rejected %d %v", len(m), err)
	}
}
