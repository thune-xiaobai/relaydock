package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"relaydock/internal/config"
)

type fixtureNative struct {
	look             int
	draft            string
	changed          bool
	switchAfterWrite bool
	pressed          int
}

func (f *fixtureNative) Call(_ context.Context, cmd string, args, out any) error {
	a := args.(map[string]any)
	var result any
	ref := func(s string) string { return fmt.Sprintf("look%d-%s", f.look, s) }
	switch cmd {
	case "listRoots":
		result = map[string]any{"roots": []map[string]any{{"rootRef": "hwnd:42", "appName": "WXWork", "title": "企业微信", "pid": 42}}}
	case "look":
		f.look++
		result = look{LookID: fmt.Sprint(f.look), Outline: node{Ref: ref("root"), Children: []node{
			{Ref: ref("chat"), Identifier: "chat"}, {Ref: ref("messages"), Identifier: "messages", Children: []node{{Ref: ref("row"), Role: "ListItem"}}},
			{Ref: ref("input"), Identifier: "input"}, {Ref: ref("send"), Identifier: "send"},
		}}}
	case "uiaReadText":
		if a["lookId"] != fmt.Sprint(f.look) {
			return errors.New("stale look")
		}
		r := a["elementRef"].(string)
		text := ""
		switch {
		case strings.HasSuffix(r, "-chat"):
			text = "远程助手"
			if f.changed {
				text = "另一个聊天"
			}
		case strings.HasSuffix(r, "-row"):
			text = "msg42|peer|检查测试"
		case strings.HasSuffix(r, "-input"):
			text = f.draft
		}
		result = map[string]any{"text": text, "hasMore": false}
	case "act":
		if a["lookId"] != fmt.Sprint(f.look) || a["policy"] != "ax_only" {
			return errors.New("unsafe action")
		}
		if a["action"] == "setText" {
			f.draft = a["params"].(map[string]string)["text"]
			if f.switchAfterWrite {
				f.changed = true
			}
		} else if a["action"] == "press" {
			f.pressed++
		}
		result = map[string]any{"outcome": "worked"}
	default:
		return fmt.Errorf("unexpected native command %s", cmd)
	}
	raw, _ := json.Marshal(result)
	return json.Unmarshal(raw, out)
}
func fixtureConfig() config.WeCom {
	return config.WeCom{App: "WXWork", WindowTitle: "企业微信", ChatTitle: "远程助手", Peer: "peer", Self: "self",
		Chat: config.Selector{Identifier: "chat"}, Messages: config.Selector{Identifier: "messages"}, Row: config.Selector{Role: "ListItem"}, Input: config.Selector{Identifier: "input"}, Send: config.Selector{Identifier: "send"}, MessagePattern: `^(?P<id>[^|]+)\|(?P<sender>[^|]+)\|(?P<text>[\s\S]+)$`}
}
func TestNativeWorkflowUsesFreshRefsAndChatGuard(t *testing.T) {
	f := &fixtureNative{}
	w, err := NewWindows(f, fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	s, err := w.Read(context.Background())
	if err != nil || len(s.Messages) != 1 || s.Messages[0].ID != "msg42" {
		t.Fatal(s, err)
	}
	if err = w.Send(context.Background(), "hello\r\nworld"); err != nil {
		t.Fatal(err)
	}
	if f.pressed != 1 {
		t.Fatal("missing press")
	}
	f.draft = "human draft"
	if err = w.Send(context.Background(), "new"); err == nil || f.pressed != 1 {
		t.Fatal("human draft overwritten")
	}
	f.draft = ""
	f.switchAfterWrite = true
	if err = w.Send(context.Background(), "new"); err == nil || f.pressed != 1 {
		t.Fatal("sent into changed conversation")
	}
}
