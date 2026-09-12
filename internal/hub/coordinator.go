package hub

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"relaydock/internal/config"
)

// Coordinator is the replaceable reasoning boundary. Only Go can dispatch tools.
type Coordinator interface {
	Run(context.Context, Turn, func(context.Context, string, json.RawMessage) ToolReply) (string, error)
}
type Turn struct {
	Owner   string   `json:"owner"`
	Input   string   `json:"input"`
	Context any      `json:"context"`
	Nodes   []string `json:"nodes"`
}
type ToolReply struct {
	OK        bool   `json:"ok"`
	Data      any    `json:"data,omitempty"`
	Error     string `json:"error,omitempty"`
	Uncertain bool   `json:"uncertain,omitempty"`
}
type PiCoordinator struct{ Config config.Config }

//go:embed coordinator.mjs
var coordinatorScript []byte

func (p PiCoordinator) Run(parent context.Context, turn Turn, execute func(context.Context, string, json.RawMessage) ToolReply) (string, error) {
	if p.Config.Model.URL == "" || p.Config.Model.Model == "" {
		return "", errors.New("Hub model.url and model.model are required")
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	root := filepath.Join(p.Config.StateDir, "coordinator")
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	script := filepath.Join(root, "coordinator.mjs")
	if err := os.WriteFile(script, coordinatorScript, 0600); err != nil {
		return "", err
	}
	piPath := p.Config.Coordinator.Package
	if piPath == "" {
		var err error
		piPath, err = exec.LookPath("pi")
		if err != nil {
			return "", errors.New("Hub needs installed pi SDK; install pi or set coordinator.package")
		}
		piPath, err = filepath.EvalSymlinks(piPath)
		if err != nil {
			return "", err
		}
	}
	node := p.Config.Coordinator.Node
	if node == "" {
		node = "node"
	}
	cmd := exec.CommandContext(ctx, node, script)
	cmd.WaitDelay = 2 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		return "", err
	}
	defer func() { stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	enc := json.NewEncoder(stdin)
	if err = enc.Encode(map[string]any{"turn": turn, "model": p.Config.Model, "pi": piPath, "root": root, "tools": coordinatorTools}); err != nil {
		return "", err
	}
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 4096), 2<<20)
	calls := 0
	for scan.Scan() {
		var m struct {
			Type, ID, Name, Text, Error string
			Arguments                   json.RawMessage
		}
		if err = json.Unmarshal(scan.Bytes(), &m); err != nil {
			return "", fmt.Errorf("invalid coordinator protocol: %w", err)
		}
		switch m.Type {
		case "tool":
			calls++
			if calls > 24 {
				return "", errors.New("Hub reached the per-turn tool limit; query status before continuing")
			}
			if err = enc.Encode(map[string]any{"id": m.ID, "result": execute(ctx, m.Name, m.Arguments)}); err != nil {
				return "", err
			}
		case "done":
			if m.Text == "" {
				return "", errors.New("coordinator returned no reply")
			}
			return short(m.Text, 12000), nil
		case "error":
			return "", errors.New(m.Error)
		default:
			return "", errors.New("unexpected coordinator message")
		}
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err = scan.Err(); err != nil {
		return "", err
	}
	return "", errors.New("pi coordinator exited without a final response; dispatched calls are not replayed")
}
