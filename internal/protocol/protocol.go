// Package protocol contains the small, versioned wire contract shared by all roles.
package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const Version = 1
const MaxMessage = 1 << 20
const MaxText = 64 << 10

var identifier = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,100}$`)

func ValidID(s string) bool { return identifier.MatchString(s) }
func ID(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}
func Now() string                { return time.Now().UTC().Format(time.RFC3339Nano) }
func JSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

type Message struct {
	Version int             `json:"version"`
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func Wrap(kind, id string, v any) Message { return Message{Version, kind, id, JSON(v)} }
func Decode[T any](m Message) (T, error) {
	var v T
	if m.Version != Version {
		return v, fmt.Errorf("unsupported protocol version %d", m.Version)
	}
	err := json.Unmarshal(m.Data, &v)
	return v, err
}

type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}
type Hello struct {
	Role       string     `json:"role"`
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	OS         string     `json:"os,omitempty"`
	Tools      []Tool     `json:"tools,omitempty"`
	Workspaces []string   `json:"workspaces,omitempty"`
	Agents     []string   `json:"agents,omitempty"`
	Shell      *ShellInfo `json:"shell,omitempty"`
}
type Call struct {
	ID   string          `json:"call_id"`
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"arguments"`
}
type Fault struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type Result struct {
	ID    string          `json:"call_id"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error *Fault          `json:"error,omitempty"`
}

func OK(id string, data any) Result        { return Result{ID: id, Data: JSON(data)} }
func Fail(id, code, message string) Result { return Result{ID: id, Error: &Fault{code, message}} }

type Session struct {
	ID        string   `json:"id"`
	Node      string   `json:"node,omitempty"`
	Workspace string   `json:"workspace"`
	Agent     string   `json:"agent"`
	MuxName   string   `json:"mux_name,omitempty"`
	Pane      string   `json:"pane,omitempty"`
	Directory string   `json:"directory,omitempty"`
	Instance  string   `json:"instance,omitempty"`
	Native    string   `json:"native_session,omitempty"`
	Status    string   `json:"status"`
	RunID     string   `json:"run_id,omitempty"`
	Updated   string   `json:"updated_at"`
	Attach    []string `json:"attach,omitempty"`
}
type Event struct {
	ID       string `json:"id"`
	Session  string `json:"session_id"`
	Instance string `json:"instance"`
	Native   string `json:"native_session,omitempty"`
	RunID    string `json:"run_id,omitempty"`
	Seq      int64  `json:"seq"`
	Position int64  `json:"position,omitempty"`
	Kind     string `json:"kind"`
	Source   string `json:"source,omitempty"`
	Text     string `json:"text,omitempty"`
	Status   string `json:"status,omitempty"`
	Time     string `json:"time"`
}
type ChatInput struct {
	ID         string `json:"id"`
	Text       string `json:"text"`
	ObservedAt string `json:"observed_at,omitempty"`
}

// ErrInvalidInput is a permanent ingress rejection, never a storage/network error.
var ErrInvalidInput = errors.New("invalid chat input")

func (in ChatInput) Validate() error {
	if !ValidID(in.ID) || strings.TrimSpace(in.Text) == "" || len(in.Text) > MaxText {
		return fmt.Errorf("%w: valid ID and 1..65536 bytes of nonblank text required", ErrInvalidInput)
	}
	return nil
}

type ChatOutput struct {
	ID       string `json:"id"`
	Sequence int64  `json:"sequence"`
	Text     string `json:"text"`
	Session  string `json:"session_id,omitempty"`
	RunID    string `json:"run_id,omitempty"`
	JobID    string `json:"job_id,omitempty"`
}
type BridgeRequest struct {
	CallID   string `json:"call_id"`
	Action   string `json:"action"`
	Session  string `json:"session_id"`
	Instance string `json:"instance"`
	Native   string `json:"native_session"`
	RunID    string `json:"run_id,omitempty"`
	Text     string `json:"text,omitempty"`
}
type BridgeState struct {
	Session  string `json:"session_id"`
	Instance string `json:"instance"`
	Native   string `json:"native_session"`
	Status   string `json:"status"`
	RunID    string `json:"run_id,omitempty"`
	Updated  string `json:"updated_at"`
	PID      int    `json:"pid"`
}
