package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"relaydock/internal/config"
)

type node struct {
	Ref, Role, Identifier, Title, Description, Value string
	Offscreen, Truncated                             bool
	Children                                         []node
}
type look struct {
	LookID  string `json:"lookId"`
	Outline node
}
type root struct {
	RootRef, WindowRef, Ref, AppName, ProcessName, Title string
	PID                                                  int
	IsMinimized                                          bool
}
type Message struct {
	ID     string `json:"id,omitempty"`
	Sender string `json:"sender"`
	Text   string `json:"text"`
}
type Snapshot struct{ Messages []Message }
type Desktop interface {
	Read(context.Context) (Snapshot, error)
	Send(context.Context, string) error
}
type Windows struct {
	Native  Native
	Config  config.WeCom
	pattern *regexp.Regexp
}

func canonicalText(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

func NewWindows(n Native, c config.WeCom) (*Windows, error) {
	if c.App == "" || c.WindowTitle == "" || c.ChatTitle == "" || c.Peer == "" || c.Self == "" || c.Peer == c.Self {
		return nil, errors.New("wecom needs app, window_title, chat_title, distinct peer and self")
	}
	for _, s := range []config.Selector{c.Chat, c.Messages, c.Row, c.Input, c.Send} {
		if s == (config.Selector{}) {
			return nil, errors.New("wecom selectors must be configured using wecom-inspect")
		}
	}
	r, err := regexp.Compile(c.MessagePattern)
	if err != nil {
		return nil, err
	}
	if r.SubexpIndex("sender") < 0 || r.SubexpIndex("text") < 0 {
		return nil, errors.New("message_pattern needs named sender and text captures; optional id must be a stable message identity")
	}
	return &Windows{Native: n, Config: c, pattern: r}, nil
}
func matches(n node, s config.Selector) bool {
	return !n.Offscreen && (s.Role == "" || s.Role == n.Role) && (s.Identifier == "" || s.Identifier == n.Identifier) && (s.Title == "" || s.Title == n.Title) && (s.Description == "" || s.Description == n.Description)
}
func selectNodes(n node, s config.Selector) []node {
	var out []node
	if matches(n, s) {
		out = append(out, n)
	}
	for _, c := range n.Children {
		out = append(out, selectNodes(c, s)...)
	}
	return out
}
func one(n node, s config.Selector) (node, error) {
	m := selectNodes(n, s)
	if len(m) != 1 || m[0].Ref == "" {
		return node{}, fmt.Errorf("selector matched %d controls, need exactly one", len(m))
	}
	return m[0], nil
}
func (w *Windows) observe(ctx context.Context) (look, error) {
	var raw json.RawMessage
	if err := w.Native.Call(ctx, "listRoots", map[string]any{}, &raw); err != nil {
		return look{}, err
	}
	var roots []root
	if err := json.Unmarshal(raw, &roots); err != nil {
		var wrapped struct{ Roots []root }
		if err = json.Unmarshal(raw, &wrapped); err != nil {
			return look{}, err
		}
		roots = wrapped.Roots
	}
	var selected []root
	for _, r := range roots {
		app := r.AppName
		if app == "" {
			app = r.ProcessName
		}
		if strings.EqualFold(strings.TrimSuffix(strings.ToLower(app), ".exe"), strings.TrimSuffix(strings.ToLower(w.Config.App), ".exe")) && r.Title == w.Config.WindowTitle && !r.IsMinimized {
			selected = append(selected, r)
		}
	}
	if len(selected) != 1 {
		return look{}, fmt.Errorf("matched %d WeCom windows, need exactly one visible window", len(selected))
	}
	r := selected[0]
	ref := r.RootRef
	if ref == "" {
		ref = r.WindowRef
	}
	if ref == "" {
		ref = r.Ref
	}
	if ref == "" {
		return look{}, errors.New("window has no native root ref")
	}
	var l look
	if err := w.Native.Call(ctx, "look", map[string]any{"pid": r.PID, "rootRef": ref, "readText": "never", "includeImage": false}, &l); err != nil {
		return l, err
	}
	if l.LookID == "" {
		return l, errors.New("missing native lookId")
	}
	chat, err := one(l.Outline, w.Config.Chat)
	if err != nil {
		return l, err
	}
	text, err := w.readText(ctx, l, chat)
	if err != nil {
		return l, err
	}
	if text != w.Config.ChatTitle {
		return l, errors.New("active conversation does not match configured chat_title")
	}
	return l, nil
}
func (w *Windows) readText(ctx context.Context, l look, n node) (string, error) {
	var r struct {
		Text    string
		HasMore bool `json:"hasMore"`
	}
	err := w.Native.Call(ctx, "uiaReadText", map[string]any{"lookId": l.LookID, "elementRef": n.Ref, "offset": 0, "limit": 65536}, &r)
	if r.HasMore {
		return "", errors.New("UIA text exceeds extraction limit")
	}
	return canonicalText(r.Text), err
}
func (w *Windows) Read(ctx context.Context) (Snapshot, error) {
	l, err := w.observe(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	container, err := one(l.Outline, w.Config.Messages)
	if err != nil {
		return Snapshot{}, err
	}
	if container.Truncated {
		return Snapshot{}, errors.New("message container outline is truncated")
	}
	rows := selectNodes(container, w.Config.Row)
	if len(rows) == 0 {
		return Snapshot{}, errors.New("no message rows visible; baseline unchanged")
	}
	s := Snapshot{}
	for _, row := range rows {
		if row.Truncated {
			return Snapshot{}, errors.New("message row is truncated")
		}
		text, err := w.readText(ctx, l, row)
		if err != nil {
			return s, err
		}
		parts := w.pattern.FindStringSubmatch(text)
		if parts == nil {
			return s, errors.New("message row does not match message_pattern; checkpoint unchanged")
		}
		m := Message{Sender: parts[w.pattern.SubexpIndex("sender")], Text: parts[w.pattern.SubexpIndex("text")]}
		if idx := w.pattern.SubexpIndex("id"); idx >= 0 {
			m.ID = parts[idx]
			if m.ID == "" {
				return s, errors.New("empty stable message identity")
			}
		}
		if m.Sender == "" || m.Text == "" {
			return s, errors.New("message sender/text is empty")
		}
		s.Messages = append(s.Messages, m)
	}
	return s, nil
}
func (w *Windows) act(ctx context.Context, l look, n node, action string, params any) error {
	var result struct {
		Outcome string
		Error   json.RawMessage
	}
	if err := w.Native.Call(ctx, "act", map[string]any{"lookId": l.LookID, "target": map[string]string{"ref": n.Ref}, "policy": "ax_only", "action": action, "params": params}, &result); err != nil {
		return err
	}
	if result.Outcome != "worked" {
		return fmt.Errorf("native %s outcome=%s: %s", action, result.Outcome, result.Error)
	}
	return nil
}
func (w *Windows) Send(ctx context.Context, text string) error {
	l, err := w.observe(ctx)
	if err != nil {
		return err
	}
	input, err := one(l.Outline, w.Config.Input)
	if err != nil {
		return err
	}
	current, err := w.readText(ctx, l, input)
	if err != nil {
		return err
	}
	if strings.TrimSpace(current) != "" {
		return errors.New("chat input has a draft; send paused")
	}
	if err = w.act(ctx, l, input, "setText", map[string]string{"text": text}); err != nil {
		return err
	}
	// Reobserve and recheck the conversation after writing. Never reuse old refs.
	l, err = w.observe(ctx)
	if err != nil {
		return err
	}
	input, err = one(l.Outline, w.Config.Input)
	if err != nil {
		return err
	}
	current, err = w.readText(ctx, l, input)
	if err != nil {
		return err
	}
	if current != canonicalText(text) {
		return errors.New("chat draft read-back mismatch")
	}
	send, err := one(l.Outline, w.Config.Send)
	if err != nil {
		return err
	}
	return w.act(ctx, l, send, "press", map[string]any{})
}

// Inspect never focuses, clicks, types or sends. Raw refs are diagnostic only.
func Inspect(ctx context.Context, n Native, pid int, rootRef string, rows config.Selector) (any, error) {
	var result any
	cmd := "listRoots"
	args := map[string]any{}
	if pid > 0 {
		args["pid"] = pid
	}
	if rootRef != "" {
		cmd = "look"
		args["rootRef"] = rootRef
		args["readText"] = "never"
		args["includeImage"] = false
	}
	err := n.Call(ctx, cmd, args, &result)
	if err != nil || rootRef == "" || rows == (config.Selector{}) {
		return result, err
	}
	var l look
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	w := Windows{Native: n}
	texts := []map[string]string{}
	for _, row := range selectNodes(l.Outline, rows) {
		if len(texts) >= 200 {
			return nil, errors.New("too many rows; narrow the row selector")
		}
		text, err := w.readText(ctx, l, row)
		if err != nil {
			return nil, err
		}
		texts = append(texts, map[string]string{"ref": row.Ref, "text": text})
	}
	return map[string]any{"look": result, "row_text": texts}, nil
}
