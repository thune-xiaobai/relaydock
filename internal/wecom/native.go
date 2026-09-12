// Package wecom implements a fixed workflow over pi-computer-use's Windows
// native helper. It does not load pi, a model, or the computer-use extension.
package wecom

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"relaydock/internal/protocol"
)

type Native interface {
	Call(context.Context, string, any, any) error
}
type nativeResponse struct {
	Version int             `json:"protocolVersion"`
	ID      string          `json:"id"`
	OK      bool            `json:"ok"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}
type Helper struct {
	mu        sync.Mutex
	cmd       *exec.Cmd
	in        io.WriteCloser
	responses chan nativeResponse
	dead      chan struct{}
	readErr   error
	broken    bool
}

func StartHelper(ctx context.Context, path string) (*Helper, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, ".pi", "agent", "helpers", "pi-computer-use", "windows-bridge.exe")
	}
	cmd := exec.CommandContext(ctx, path)
	cmd.WaitDelay = 2 * time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		return nil, fmt.Errorf("start pi-computer-use Windows helper: %w", err)
	}
	h := &Helper{cmd: cmd, in: in, responses: make(chan nativeResponse, 1), dead: make(chan struct{})}
	go func() {
		defer close(h.dead)
		s := bufio.NewScanner(out)
		s.Buffer(make([]byte, 4096), 16<<20)
		for s.Scan() {
			var r nativeResponse
			if err := json.Unmarshal(s.Bytes(), &r); err != nil {
				h.readErr = err
				_ = cmd.Process.Kill()
				return
			}
			select {
			case h.responses <- r:
			default:
				h.readErr = errors.New("unexpected native response")
				_ = cmd.Process.Kill()
				return
			}
		}
		h.readErr = s.Err()
	}()
	var diagnostics struct {
		Version int `json:"protocolVersion"`
	}
	if err = h.Call(ctx, "diagnostics", map[string]any{}, &diagnostics); err != nil {
		h.Close()
		return nil, err
	}
	if diagnostics.Version != 3 {
		h.Close()
		return nil, errors.New("pi-computer-use helper requires protocolVersion 3")
	}
	return h, nil
}
func (h *Helper) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.broken = true
	h.in.Close()
	_ = h.cmd.Process.Kill()
	err := h.cmd.Wait()
	<-h.dead
	return err
}
func (h *Helper) Call(parent context.Context, name string, args, out any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := parent.Err(); err != nil {
		return err
	}
	if h.broken {
		return errors.New("native connection was interrupted; restart Channel to reobserve")
	}
	abort := func() { h.broken = true; _ = h.cmd.Process.Kill() }
	select {
	case <-h.dead:
		return fmt.Errorf("native helper exited: %v", h.readErr)
	default:
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	id := protocol.ID("req_")
	written := make(chan error, 1)
	go func() {
		written <- json.NewEncoder(h.in).Encode(map[string]any{"protocolVersion": 3, "id": id, "cmd": name, "args": args})
	}()
	select {
	case <-ctx.Done():
		abort()
		return ctx.Err()
	case err := <-written:
		if err != nil {
			abort()
			return err
		}
	}
	select {
	case <-ctx.Done():
		abort()
		return fmt.Errorf("native %s timed out; action outcome may be unknown: %w", name, ctx.Err())
	case <-h.dead:
		return fmt.Errorf("native helper exited during %s: %v", name, h.readErr)
	case r := <-h.responses:
		if r.Version != 3 || r.ID != id {
			abort()
			return errors.New("native response protocol or request ID mismatch")
		}
		if !r.OK {
			return fmt.Errorf("native %s: %s", name, r.Error)
		}
		return json.Unmarshal(r.Result, out)
	}
}
