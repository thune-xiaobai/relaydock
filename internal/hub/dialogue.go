package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"relaydock/internal/protocol"
	"relaydock/internal/store"
	"relaydock/internal/worker"
)

func (h *Hub) context(owner string, d dialogue) (any, error) {
	nodes, err := h.db.List("nodes")
	if err != nil {
		return nil, err
	}
	available := []map[string]any{}
	for id, b := range nodes {
		if !h.allowed(owner, id) {
			continue
		}
		var n protocol.Hello
		if err = json.Unmarshal(b, &n); err != nil {
			return nil, err
		}
		h.mu.Lock()
		online := h.workers[id] != nil
		h.mu.Unlock()
		available = append(available, map[string]any{"node": n, "online": online})
	}
	all, err := h.db.List("sessions")
	if err != nil {
		return nil, err
	}
	ss := []Binding{}
	for _, b := range all {
		var s Binding
		if err = json.Unmarshal(b, &s); err != nil {
			return nil, err
		}
		if s.Owner == owner && h.allowed(owner, s.Session.Node) {
			ss = append(ss, s)
		}
	}
	sort.Slice(ss, func(i, j int) bool { return ss[i].Session.Updated > ss[j].Session.Updated })
	// Keep the model context bounded. Older sessions are still stored locally.
	if len(ss) > 50 {
		ss = ss[:50]
	}
	for i := range ss {
		ss[i].LastText = short(ss[i].LastText, 2000)
	}
	return map[string]any{"nodes": available, "sessions": ss, "conversation": d}, nil
}

func (h *Hub) process(ctx context.Context, j job) {
	var d dialogue
	err := h.db.Get("dialogues", j.Owner, &d)
	if err != nil && !errors.Is(err, store.ErrMissing) {
		return
	}
	data, err := h.context(j.Owner, d)
	var decision Decision
	if err == nil {
		decision, err = h.router.Decide(ctx, j.Input.Text, data)
	}
	text := ""
	session := ""
	if err == nil {
		text, session, err = h.perform(ctx, j.Owner, j.Input.Text, &d, decision)
	}
	if err != nil {
		text = "未能确认完成这项请求：" + err.Error()
	}
	d.History = append(d.History, historyItem{"user", short(j.Input.Text, 2000)}, historyItem{"assistant", short(text, 2000)})
	if len(d.History) > 12 {
		d.History = d.History[len(d.History)-12:]
	}
	err = h.db.Update(func(t *store.Tx) error {
		if e := t.Put("dialogues", j.Owner, d); e != nil {
			return e
		}
		if e := t.Put("inbox", j.Owner+"/"+j.Input.ID, chatRecord{Input: j.Input, Done: true}); e != nil {
			return e
		}
		return h.putOutput(t, j.Owner, text, session, "")
	})
	if err != nil { /* Leave the durable inbox pending; restart reports uncertainty. */
		return
	}
}

func (h *Hub) owned(owner, id string) (Binding, error) {
	var b Binding
	if err := h.db.Get("sessions", id, &b); err != nil {
		return b, errors.New("没有找到该会话，请说明机器和项目")
	}
	if b.Owner != owner || !h.allowed(owner, b.Session.Node) {
		return Binding{}, errors.New("无权访问该会话")
	}
	return b, nil
}
func contains(a []string, s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}
func short(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func (h *Hub) perform(ctx context.Context, owner, input string, d *dialogue, v Decision) (string, string, error) {
	bad := func(s string) (string, string, error) { return "", "", errors.New(s) }
	if v.Action == "clarify" {
		if strings.TrimSpace(v.Text) == "" || len(v.Text) > 4000 {
			return bad("模型未返回有效的澄清问题")
		}
		if d.Pending == "" {
			d.Pending = input
		} else {
			d.Pending = short(d.Pending+"\n"+input, 8000)
		}
		return v.Text, "", nil
	}
	if v.Action == "nodes" {
		m, err := h.db.List("nodes")
		if err != nil {
			return "", "", err
		}
		lines := []string{}
		for id, raw := range m {
			if !h.allowed(owner, id) {
				continue
			}
			var n protocol.Hello
			if json.Unmarshal(raw, &n) != nil {
				continue
			}
			h.mu.Lock()
			online := h.workers[id] != nil
			h.mu.Unlock()
			s := "离线"
			if online {
				s = "在线"
			}
			lines = append(lines, fmt.Sprintf("%s：%s；项目 %s；pi 配置 %s", id, s, strings.Join(n.Workspaces, "、"), strings.Join(n.Agents, "、")))
		}
		sort.Strings(lines)
		if len(lines) == 0 {
			return "尚无获准 Worker 连接到 Hub。", "", nil
		}
		return strings.Join(lines, "\n"), "", nil
	}
	if v.Action == "start" {
		if !h.allowed(owner, v.Node) {
			return bad("目标机器不在允许范围内")
		}
		var n protocol.Hello
		if err := h.db.Get("nodes", v.Node, &n); err != nil {
			return bad("目标 Worker 尚未注册")
		}
		if !contains(n.Workspaces, v.Workspace) || !contains(n.Agents, v.Agent) {
			return bad("模型选择了不存在的工作目录或 pi 配置；未派发操作")
		}
		task := input
		if v.UsePending && d.Pending != "" {
			task = d.Pending + "\n\n用户补充：" + input
		}
		if len(task) > protocol.MaxText {
			return bad("任务文本过长")
		}
		r, err := h.call(ctx, owner, v.Node, "session.create", short(task, 100), worker.Arguments{Workspace: v.Workspace, Agent: v.Agent})
		if err != nil {
			return "", "", err
		}
		var s protocol.Session
		if err = json.Unmarshal(r.Data, &s); err != nil {
			return "", "", err
		}
		d.Focus = s.ID
		d.Pending = ""
		return h.submit(ctx, owner, s, task)
	}
	switch v.Action {
	case "continue", "status", "result", "cancel", "capture", "close":
	default:
		return bad("模型返回了不支持的动作；未派发操作")
	}
	if !protocol.ValidID(v.Session) {
		return bad("请说明要操作哪个机器、哪个项目的会话")
	}
	b, err := h.owned(owner, v.Session)
	if err != nil {
		return "", "", err
	}
	if v.Action == "result" {
		d.Focus = b.Session.ID
		if b.LastText == "" {
			return "该会话尚无 Agent 文本结果；记录状态：" + b.Session.Status, b.Session.ID, nil
		}
		return "[" + b.Session.Node + " / " + b.Session.Workspace + "] 最近结果（" + outcome(b.Outcome) + "）：\n" + b.LastText, b.Session.ID, nil
	}
	r, err := h.call(ctx, owner, b.Session.Node, "session.inspect", "", worker.Arguments{Session: b.Session.ID})
	if err != nil {
		return "", "", err
	}
	var s protocol.Session
	if err = json.Unmarshal(r.Data, &s); err != nil {
		return "", "", err
	}
	if s.ID != b.Session.ID {
		return bad("Worker 返回了错误的会话")
	}
	d.Focus = s.ID
	switch v.Action {
	case "status":
		return fmt.Sprintf("[%s / %s] pi 状态：%s；最近一轮：%s。", s.Node, s.Workspace, s.Status, outcome(b.Outcome)), s.ID, nil
	case "continue":
		task := input
		if v.UsePending && d.Pending != "" {
			task = d.Pending + "\n\n用户补充：" + input
		}
		text, id, err := h.submit(ctx, owner, s, task)
		if err == nil {
			d.Pending = ""
		}
		return text, id, err
	case "cancel":
		if s.Status == "idle" || s.RunID == "" {
			return "该 pi 当前没有运行中的轮次。", s.ID, nil
		}
		_, err = h.call(ctx, owner, s.Node, "agent.interrupt", "", worker.Arguments{Session: s.ID, Instance: s.Instance, Native: s.Native, RunID: s.RunID})
		return "已请求中断，等待 pi 的结束事件确认。", s.ID, err
	case "capture":
		r, err = h.call(ctx, owner, s.Node, "terminal.capture", "", worker.Arguments{Session: s.ID})
		if err != nil {
			return "", "", err
		}
		var out struct {
			Text string `json:"text"`
		}
		if err = json.Unmarshal(r.Data, &out); err != nil {
			return "", "", err
		}
		return out.Text, s.ID, nil
	case "close":
		_, err = h.call(ctx, owner, s.Node, "session.close", "", worker.Arguments{Session: s.ID, Instance: s.Instance, Native: s.Native})
		if err != nil {
			return "", "", err
		}
		err = h.db.Update(func(t *store.Tx) error {
			var current Binding
			if e := t.Get("sessions", s.ID, &current); e != nil {
				return e
			}
			current.Session.Status = "closed"
			return t.Put("sessions", s.ID, current)
		})
		return "该会话已关闭。", s.ID, err
	}
	return bad("unsupported action")
}
func (h *Hub) submit(ctx context.Context, owner string, s protocol.Session, text string) (string, string, error) {
	if s.Status != "idle" {
		return "", s.ID, fmt.Errorf("pi 当前为 %s；此版本只向空闲 pi 提交远程消息，本地终端仍可正常操作", s.Status)
	}
	run := protocol.ID("run_")
	if err := h.db.Put("runs", run, map[string]string{"session_id": s.ID, "owner": owner, "text": text, "status": "submitting"}); err != nil {
		return "", s.ID, err
	}
	_, err := h.call(ctx, owner, s.Node, "agent.submit", "", worker.Arguments{Session: s.ID, Instance: s.Instance, Native: s.Native, RunID: run, Text: text})
	if err != nil {
		return "", s.ID, err
	}
	return "[" + s.Node + " / " + s.Workspace + "] pi 已接受输入；执行进展和结果会继续回传。", s.ID, nil
}
