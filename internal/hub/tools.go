package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"relaydock/internal/protocol"
	"relaydock/internal/store"
	"relaydock/internal/worker"
)

var ErrUncertain = errors.New("execution uncertain")

type toolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

func tool(name, description string, required []string, fields ...string) toolDefinition {
	props := map[string]any{}
	for _, f := range fields {
		props[f] = map[string]any{"type": "string", "minLength": 1, "maxLength": protocol.MaxText}
	}
	return toolDefinition{name, description, map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}}
}

var coordinatorTools = []toolDefinition{
	tool("inventory", "List authorized hosts, capabilities and owned sessions with latest recorded results.", []string{}),
	tool("session_create", "Create an idle interactive pi in an advertised workspace. Does not submit a task. Reuse existing sessions where possible.", []string{"node", "workspace", "agent"}, "node", "workspace", "agent", "title"),
	tool("session_inspect", "Read current session state from its Worker, plus last recorded result.", []string{"session_id"}, "session_id"),
	tool("session_result", "Read stored session output and outcome, including when Worker is offline. Does not rerun.", []string{"session_id"}, "session_id"),
	tool("agent_submit", "Submit a task to an idle session. Returns run_id and acceptance, not task completion.", []string{"session_id", "text"}, "session_id", "text"),
	tool("agent_interrupt", "Request interruption of current run when authorized by user. Removes its follow-up watch.", []string{"session_id"}, "session_id"),
	tool("terminal_capture", "Read the current terminal screen for an owned session.", []string{"session_id"}, "session_id"),
	tool("session_close", "Close an owned session only when requested.", []string{"session_id"}, "session_id"),
	tool("session_watch", "Resume Hub once after the specified run settles, following a user-authorized instruction. Also works if that run has just settled. Not a recurring monitor.", []string{"session_id", "run_id", "instruction"}, "session_id", "run_id", "instruction"),
}

func mutation(name string) bool {
	switch name {
	case "session_create", "agent_submit", "agent_interrupt", "session_close", "session_watch":
		return true
	}
	return false
}

// Each tool is validated independently of SDK/model validation. IDs and tokens
// on Worker calls are generated here, never supplied by the coordinator.
func (h *Hub) executeTool(ctx context.Context, owner string, d *dialogue, name string, raw json.RawMessage) ToolReply {
	data, err := h.tool(ctx, owner, d, name, raw)
	if err != nil {
		return ToolReply{Error: err.Error(), Uncertain: errors.Is(err, ErrUncertain)}
	}
	return ToolReply{OK: true, Data: data}
}
func (h *Hub) tool(ctx context.Context, owner string, d *dialogue, name string, raw json.RawMessage) (any, error) {
	var def *toolDefinition
	for i := range coordinatorTools {
		if coordinatorTools[i].Name == name {
			def = &coordinatorTools[i]
			break
		}
	}
	if def == nil {
		return nil, errors.New("unknown Hub tool")
	}
	var fields map[string]string
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errors.New("tool arguments must be a string-valued object")
	}
	schema := def.Parameters.(map[string]any)
	props := schema["properties"].(map[string]any)
	for k, v := range fields {
		if _, ok := props[k]; !ok || strings.TrimSpace(v) == "" || len(v) > protocol.MaxText {
			return nil, fmt.Errorf("invalid argument %q", k)
		}
	}
	for _, k := range schema["required"].([]string) {
		if fields[k] == "" {
			return nil, fmt.Errorf("missing argument %s", k)
		}
	}
	var a struct {
		Node, Workspace, Agent, Title, Text, Instruction string
		Session                                          string `json:"session_id"`
		RunID                                            string `json:"run_id"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing arguments")
	}
	if name == "inventory" {
		return h.context(owner, *d)
	}
	if name == "session_create" {
		if !h.allowed(owner, a.Node) {
			return nil, errors.New("target node is not authorized")
		}
		var n protocol.Hello
		if err := h.db.Get("nodes", a.Node, &n); err != nil {
			return nil, err
		}
		if !contains(n.Workspaces, a.Workspace) || !contains(n.Agents, a.Agent) {
			return nil, errors.New("workspace or agent is not advertised by Worker")
		}
		r, err := h.call(ctx, owner, a.Node, "session.create", short(a.Title, 100), worker.Arguments{Workspace: a.Workspace, Agent: a.Agent})
		if err != nil {
			return nil, err
		}
		var s protocol.Session
		if err = json.Unmarshal(r.Data, &s); err != nil {
			return nil, err
		}
		s.Node = a.Node
		d.Focus = s.ID
		return s, nil
	}
	b, err := h.owned(owner, a.Session)
	if err != nil {
		return nil, err
	}
	if name == "session_watch" {
		return h.watch(owner, b, a.RunID, a.Instruction)
	}
	d.Focus = b.Session.ID
	if name == "session_result" {
		return b, nil
	}
	if name == "terminal_capture" {
		r, err := h.call(ctx, owner, b.Session.Node, "terminal.capture", "", worker.Arguments{Session: b.Session.ID})
		return r.Data, err
	}
	r, err := h.call(ctx, owner, b.Session.Node, "session.inspect", "", worker.Arguments{Session: b.Session.ID})
	if err != nil {
		return nil, err
	}
	var s protocol.Session
	if err = json.Unmarshal(r.Data, &s); err != nil {
		return nil, err
	}
	if s.ID != b.Session.ID {
		return nil, errors.New("Worker returned wrong session")
	}
	s.Node = b.Session.Node
	if name == "session_inspect" {
		b.Session = s
		return b, nil
	}
	args := worker.Arguments{Session: s.ID, Instance: s.Instance, Native: s.Native, RunID: s.RunID}
	switch name {
	case "agent_submit":
		if s.Status != "idle" {
			return nil, fmt.Errorf("pi is %s; only idle sessions accept remote input", s.Status)
		}
		args.RunID = protocol.ID("run_")
		args.Text = a.Text
		if err = h.db.Put("runs", args.RunID, map[string]string{"session_id": s.ID, "owner": owner, "text": a.Text, "status": "submitting"}); err != nil {
			return nil, err
		}
		_, err = h.call(ctx, owner, s.Node, "agent.submit", "", args)
		if err != nil {
			return nil, err
		}
		return map[string]any{"session_id": s.ID, "run_id": args.RunID, "accepted": true}, nil
	case "agent_interrupt", "session_close":
		if err = h.removeWatch(s.ID); err != nil {
			return nil, err
		}
		if name == "agent_interrupt" && (s.Status == "idle" || s.RunID == "") {
			return map[string]any{"already_idle": true}, nil
		}
		wire := "agent.interrupt"
		if name == "session_close" {
			wire = "session.close"
		}
		_, err = h.call(ctx, owner, s.Node, wire, "", args)
		if err != nil {
			return nil, err
		}
		if name == "session_close" {
			err = h.db.Update(func(t *store.Tx) error {
				var current Binding
				if e := t.Get("sessions", s.ID, &current); e != nil {
					return e
				}
				current.Session.Status = "closed"
				return t.Put("sessions", s.ID, current)
			})
		}
		return map[string]any{"accepted": true, "session_id": s.ID}, err
	}
	return nil, errors.New("unsupported tool")
}
