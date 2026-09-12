package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"relaydock/internal/protocol"
)

type fixtureMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func contentText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct{ Type, Text string }
	_ = json.Unmarshal(raw, &parts)
	for _, p := range parts {
		if p.Type == "text" {
			text += p.Text
		}
	}
	return text
}

// A real streaming tool-call provider, not a substitute for the SDK loop. The
// next request must contain the previous Go tool result to advance the fixture.
func coordinatorFixture(w http.ResponseWriter, messages []fixtureMessage) {
	lastUser := -1
	for i, m := range messages {
		if m.Role == "user" {
			lastUser = i
		}
	}
	if lastUser < 0 {
		http.Error(w, "missing user", 400)
		return
	}
	var in struct {
		User    string `json:"user_request"`
		Context struct {
			Conversation dialogue `json:"conversation"`
		} `json:"context"`
	}
	_ = json.Unmarshal([]byte(contentText(messages[lastUser].Content)), &in)
	var results []struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error"`
	}
	for _, m := range messages[lastUser+1:] {
		if m.Role == "tool" {
			var r struct {
				OK    bool            `json:"ok"`
				Data  json.RawMessage `json:"data"`
				Error string          `json:"error"`
			}
			_ = json.Unmarshal([]byte(contentText(m.Content)), &r)
			results = append(results, r)
		}
	}
	name := ""
	args := map[string]string{}
	final := "已处理这项请求。"
	if len(results) == 0 {
		name = "inventory"
	} else {
		for _, r := range results {
			if !r.OK {
				final = "FIXTURE_TOOL_ERROR: " + r.Error
				goto respond
			}
		}
		switch in.User {
		case "在项目 A 开始检查", "在项目 B 开始检查":
			switch len(results) {
			case 1:
				name = "session_create"
				workspace := "a"
				if in.User == "在项目 B 开始检查" {
					workspace = "b"
				}
				args = map[string]string{"node": "local", "workspace": workspace, "agent": "pi", "title": in.User}
			case 2:
				var s protocol.Session
				_ = json.Unmarshal(results[1].Data, &s)
				name = "agent_submit"
				args = map[string]string{"session_id": s.ID, "text": in.User}
			default:
				final = "pi 已接受输入；结果会继续回传。"
			}
		case "继续项目 A":
			if len(results) == 1 {
				var inv struct {
					Sessions []Binding `json:"sessions"`
				}
				_ = json.Unmarshal(results[0].Data, &inv)
				for _, b := range inv.Sessions {
					if b.Session.Workspace == "a" {
						name = "agent_submit"
						args = map[string]string{"session_id": b.Session.ID, "text": in.User}
					}
				}
			}
		case "现在怎么样":
			if len(results) == 1 {
				name = "session_inspect"
				args = map[string]string{"session_id": in.Context.Conversation.Focus}
			}
		case "等项目 A 本轮结束后总结", "总结项目 A 本轮结果":
			if len(results) == 1 {
				var inv struct {
					Sessions []Binding `json:"sessions"`
				}
				_ = json.Unmarshal(results[0].Data, &inv)
				for _, b := range inv.Sessions {
					if b.Session.Workspace == "a" {
						if in.User == "等项目 A 本轮结束后总结" {
							name = "session_watch"
							args = map[string]string{"session_id": b.Session.ID, "run_id": b.Session.RunID, "instruction": "总结项目 A 本轮结果"}
						}
						if in.User == "总结项目 A 本轮结果" {
							name = "session_result"
							args = map[string]string{"session_id": b.Session.ID}
						}
					}
				}
			} else if in.User == "总结项目 A 本轮结果" {
				final = "FOLLOWUP_FIXTURE: 已读取项目 A 的结果。"
			}
		}
	}
respond:
	w.Header().Set("Content-Type", "text/event-stream")
	send := func(delta any, stop any) {
		b := protocol.JSON(map[string]any{"id": "coordinator-fixture", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": "coordinator", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": stop}}})
		fmt.Fprintf(w, "data: %s\n\n", b)
		w.(http.Flusher).Flush()
	}
	if name != "" {
		send(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("fixture_%d", len(results)), "type": "function", "function": map[string]string{"name": name, "arguments": string(protocol.JSON(args))}}}}, nil)
		send(map[string]any{}, "tool_calls")
	} else {
		send(map[string]string{"role": "assistant", "content": final}, nil)
		send(map[string]any{}, "stop")
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}
