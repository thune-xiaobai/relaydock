package wecom

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"relaydock/internal/config"
	"relaydock/internal/protocol"
)

type fakeDesktop struct {
	messages []Message
	sends    []string
	echo     bool
	fail     bool
}

func (f *fakeDesktop) Read(context.Context) (Snapshot, error) {
	return Snapshot{Messages: append([]Message(nil), f.messages...)}, nil
}
func (f *fakeDesktop) Send(_ context.Context, text string) error {
	f.sends = append(f.sends, text)
	if f.echo {
		f.messages = append(f.messages, Message{ID: protocol.ID("echo_"), Sender: "self", Text: text})
	}
	if f.fail {
		return errors.New("helper disconnected after press")
	}
	return nil
}
func TestDeltaOccurrencesAndAmbiguity(t *testing.T) {
	old := []Message{{Sender: "peer", Text: "anchor"}, {Sender: "self", Text: "ok"}, {Sender: "peer", Text: "继续"}}
	next := append(append([]Message{}, old...), Message{Sender: "peer", Text: "继续"})
	added, err := delta(old, next)
	if err != nil || len(added) != 1 || added[0].Text != "继续" {
		t.Fatal(added, err)
	}
	if _, err = delta([]Message{{Text: "same"}, {Text: "same"}}, []Message{{Text: "same"}, {Text: "same"}, {Text: "same"}}); err == nil {
		t.Fatal("ambiguous occurrence accepted")
	}
	if _, err = delta(old, []Message{{Text: "unrelated"}}); err == nil {
		t.Fatal("lost continuity accepted")
	}
	if _, err = delta([]Message{{ID: "stable", Text: "old"}}, []Message{{ID: "stable", Text: "edited"}}); err == nil {
		t.Fatal("identity conflict accepted")
	}
}
func TestGatewayBaselineDedupeDeliveryAndRestart(t *testing.T) {
	c := config.Config{StateDir: t.TempDir(), WeCom: &config.WeCom{Peer: "peer", Self: "self"}}
	f := &fakeDesktop{messages: []Message{{ID: "1", Sender: "peer", Text: "history"}}, echo: true}
	inputs := map[string]string{}
	accept := func(in protocol.ChatInput) error { inputs[in.ID] = in.Text; return nil }
	g, err := New(c, f, accept)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	step := func() {
		t.Helper()
		if err := g.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	step()
	if len(inputs) != 0 {
		t.Fatal("history replayed on first observation")
	}
	f.messages = append(f.messages, Message{ID: "2", Sender: "peer", Text: "继续"}, Message{ID: "3", Sender: "peer", Text: "继续"})
	step()
	step()
	if len(inputs) != 2 {
		t.Fatal("distinct repeated text was lost or duplicated", inputs)
	}
	for _, o := range []protocol.ChatOutput{{ID: "later", Sequence: 2, Text: "later"}, {ID: "first", Sequence: 1, Text: "first"}} {
		if err = g.EnqueueOutput(o); err != nil {
			t.Fatal(err)
		}
	}
	step() // first press, deliberately before verification
	if err = g.Close(); err != nil {
		t.Fatal(err)
	}
	g, err = New(c, f, accept)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	step()
	step()
	step()
	if !reflect.DeepEqual(f.sends, []string{"first", "later"}) {
		t.Fatal("restart resent or changed order", f.sends)
	}
	if len(inputs) != 2 {
		t.Fatal("self echoes reached Hub")
	}
	if err = g.EnqueueOutput(protocol.ChatOutput{ID: "first", Sequence: 1, Text: "first"}); err != nil {
		t.Fatal(err)
	}
	step()
	if len(f.sends) != 2 {
		t.Fatal("duplicate Hub output resent")
	}
}
func TestInterruptedInputAcceptanceUsesSameID(t *testing.T) {
	c := config.Config{StateDir: t.TempDir(), WeCom: &config.WeCom{Peer: "peer", Self: "self"}}
	f := &fakeDesktop{messages: []Message{{ID: "1", Sender: "peer", Text: "history"}}}
	var ids []string
	fail := true
	accept := func(in protocol.ChatInput) error {
		ids = append(ids, in.ID)
		if fail {
			return errors.New("interrupted after durable channel acceptance")
		}
		return nil
	}
	g, err := New(c, f, accept)
	if err != nil {
		t.Fatal(err)
	}
	if err = g.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.messages = append(f.messages, Message{ID: "2", Sender: "peer", Text: "run"})
	if err = g.Step(context.Background()); err == nil {
		t.Fatal("expected acceptance interruption")
	}
	g.Close()
	fail = false
	g, err = New(c, f, accept)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err = g.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Fatal("recovery invented another occurrence", ids)
	}
}
func TestUnknownSendBlocksResendButKeepsReceiving(t *testing.T) {
	c := config.Config{StateDir: t.TempDir(), WeCom: &config.WeCom{Peer: "peer", Self: "self"}}
	f := &fakeDesktop{messages: []Message{{ID: "1", Sender: "peer", Text: "history"}}, fail: true}
	inputs := 0
	g, err := New(c, f, func(protocol.ChatInput) error { inputs++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ctx := context.Background()
	if err = g.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if err = g.EnqueueOutput(protocol.ChatOutput{ID: "out", Sequence: 1, Text: "result"}); err != nil {
		t.Fatal(err)
	}
	if err = g.Step(ctx); err == nil {
		t.Fatal("expected send error")
	}
	var d Delivery
	if err = g.db.Get("deliveries", "out", &d); err != nil {
		t.Fatal(err)
	}
	d.AttemptAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	if err = g.db.Put("deliveries", "out", d); err != nil {
		t.Fatal(err)
	}
	_ = g.Step(ctx)
	f.messages = append(f.messages, Message{ID: "2", Sender: "peer", Text: "new request"})
	_ = g.Step(ctx)
	_ = g.Step(ctx)
	if len(f.sends) != 1 || inputs != 1 {
		t.Fatal("uncertain send replayed or stopped input", f.sends, inputs)
	}
	if err = Resolve(g.db, "out", "sent"); err != nil {
		t.Fatal(err)
	}
	if err = g.Step(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestChatBindingAndBaselineProtectPendingState(t *testing.T) {
	c := config.Config{ID: "channel", StateDir: t.TempDir(), WeCom: &config.WeCom{ChatTitle: "original", Peer: "peer", Self: "self"}}
	f := &fakeDesktop{messages: []Message{{ID: "1", Sender: "peer", Text: "visible history"}}}
	g, err := New(c, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = g.Baseline(context.Background()); err != nil {
		t.Fatal(err)
	}
	cp := checkpoint{Messages: f.messages, Pending: []protocol.ChatInput{{ID: "pending", Text: "must preserve"}}}
	if err = g.db.Put("meta", "checkpoint", cp); err != nil {
		t.Fatal(err)
	}
	if err = g.Baseline(context.Background()); err == nil {
		t.Fatal("baseline discarded pending acceptance")
	}
	if err = g.Close(); err != nil {
		t.Fatal(err)
	}
	c.WeCom.ChatTitle = "different"
	if other, err := New(c, f, nil); err == nil {
		other.Close()
		t.Fatal("old outputs rebound to another chat")
	}
}
