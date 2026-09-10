// Package channel implements the replaceable console/file-spool channel edge.
package channel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"relaydock/internal/config"
	"relaydock/internal/localfile"
	"relaydock/internal/protocol"
	"relaydock/internal/store"
	"relaydock/internal/transport"
)

type Channel struct {
	c      config.Config
	db     *store.Store
	Output func(protocol.ChatOutput) error
}

type queuedInput struct {
	Sequence int64              `json:"sequence"`
	Input    protocol.ChatInput `json:"input"`
}

func New(c config.Config) (*Channel, error) {
	if !protocol.ValidID(c.ID) || len(c.Token) < 24 {
		return nil, errors.New("channel needs a valid ID and token")
	}
	if _, e := config.ClientTLS(c); e != nil {
		return nil, e
	}
	db, e := store.Open(c.StateDir)
	if e != nil {
		return nil, e
	}
	return &Channel{c: c, db: db}, nil
}
func (c *Channel) Close() error { return c.db.Close() }
func (c *Channel) Enqueue(input protocol.ChatInput) error {
	if !protocol.ValidID(input.ID) || len(input.Text) == 0 || len(input.Text) > protocol.MaxText {
		return errors.New("invalid channel input")
	}
	return c.db.Update(func(t *store.Tx) error {
		var old protocol.ChatInput
		e := t.Get("inputs", input.ID, &old)
		if e == nil {
			if old.Text != input.Text {
				return errors.New("input ID already used for different text")
			}
			return nil
		}
		if !errors.Is(e, store.ErrMissing) {
			return e
		}
		if e = t.Put("inputs", input.ID, input); e != nil {
			return e
		}
		var seq int64
		if e = t.Get("meta", "input_sequence", &seq); e != nil && !errors.Is(e, store.ErrMissing) {
			return e
		}
		seq++
		if e = t.Put("meta", "input_sequence", seq); e != nil {
			return e
		}
		return t.Put("pending", input.ID, queuedInput{seq, input})
	})
}
func (c *Channel) Console(r io.Reader, w io.Writer) {
	c.Output = func(o protocol.ChatOutput) error { _, e := fmt.Fprintln(w, "\n"+o.Text); return e }
	go func() {
		s := bufio.NewScanner(r)
		s.Buffer(make([]byte, 4096), protocol.MaxText)
		for s.Scan() {
			if s.Text() == "" {
				continue
			}
			if e := c.Enqueue(protocol.ChatInput{ID: protocol.ID("in_"), Text: s.Text()}); e != nil {
				log.Printf("console input: %v", e)
			}
		}
		if e := s.Err(); e != nil {
			log.Printf("console input: %v", e)
		}
	}()
}
func (c *Channel) spool() error {
	if c.c.Spool == "" {
		return nil
	}
	for _, dir := range []string{"inbox", "outbox"} {
		if e := os.MkdirAll(filepath.Join(c.c.Spool, dir), 0700); e != nil {
			return e
		}
	}
	files, e := os.ReadDir(filepath.Join(c.c.Spool, "inbox"))
	if e != nil {
		return e
	}
	inputs := []protocol.ChatInput{}
	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		var in protocol.ChatInput
		if e = localfile.Read(filepath.Join(c.c.Spool, "inbox", f.Name()), &in); e != nil {
			return e
		}
		if in.ID+".json" != f.Name() {
			return errors.New("spool input ID mismatch")
		}
		inputs = append(inputs, in)
	}
	sort.SliceStable(inputs, func(i, j int) bool { return inputs[i].ObservedAt < inputs[j].ObservedAt })
	for _, in := range inputs {
		if e = c.Enqueue(in); e != nil {
			return e
		}
	}
	return nil
}
func (c *Channel) deliver(o protocol.ChatOutput) error {
	var seen bool
	if e := c.db.Get("outputs", o.ID, &seen); e == nil {
		return nil
	} else if !errors.Is(e, store.ErrMissing) {
		return e
	}
	if c.c.Spool != "" {
		if e := localfile.Write(filepath.Join(c.c.Spool, "outbox", o.ID+".json"), o); e != nil {
			return e
		}
	}
	if c.Output != nil {
		if e := c.Output(o); e != nil {
			return e
		}
	}
	return c.db.Put("outputs", o.ID, true)
}
func (c *Channel) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		p, e := transport.Dial(ctx, c.c, protocol.Hello{Role: "channel", ID: c.c.ID, Name: c.c.Name})
		if e == nil {
			log.Printf("channel %s connected", c.c.ID)
			e = c.connected(ctx, p)
			p.Close()
		}
		if ctx.Err() != nil {
			break
		}
		log.Printf("channel reconnect: %v", e)
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return nil
}
func (c *Channel) connected(parent context.Context, p *transport.Peer) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	go p.KeepAlive(ctx)
	go func() { <-ctx.Done(); p.Close() }()
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if e := c.spool(); e != nil {
					log.Printf("channel spool: %v", e)
					p.Close()
					return
				}
				m, e := c.db.List("pending")
				if e != nil {
					p.Close()
					return
				}
				ids := make([]string, 0, len(m))
				for id := range m {
					ids = append(ids, id)
				}
				sort.Slice(ids, func(i, j int) bool {
					var a, b queuedInput
					_ = json.Unmarshal(m[ids[i]], &a)
					_ = json.Unmarshal(m[ids[j]], &b)
					return a.Sequence < b.Sequence
				})
				for _, id := range ids {
					var input queuedInput
					if json.Unmarshal(m[id], &input) != nil {
						p.Close()
						return
					}
					if p.Send(protocol.Wrap("chat", id, input.Input)) != nil {
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
		case "chat_ack":
			if m.Version != protocol.Version {
				return errors.New("bad version")
			}
			if e = c.db.Delete("pending", m.ID); e != nil {
				return e
			}
		case "output":
			o, e := protocol.Decode[protocol.ChatOutput](m)
			if e != nil || !protocol.ValidID(o.ID) {
				return errors.New("invalid output")
			}
			if e = c.deliver(o); e != nil {
				return e
			}
			if e = p.Send(protocol.Wrap("output_ack", o.ID, nil)); e != nil {
				return e
			}
		default:
			return errors.New("unexpected channel message")
		}
	}
}
