package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"relaydock/internal/config"
	"relaydock/internal/localfile"
	"relaydock/internal/protocol"
)

type Launch struct {
	Agent     config.Agent `json:"agent"`
	Workspace string       `json:"workspace"`
	Bridge    string       `json:"bridge"`
	Directory string       `json:"directory"`
	Session   string       `json:"session"`
}

// Spawn runs inside the mux, independently of the Worker connection/process.
func Spawn(path string) error {
	var l Launch
	if err := localfile.Read(path, &l); err != nil {
		return err
	}
	args := append([]string{}, l.Agent.Args...)
	args = append(args, "--extension", l.Bridge, "--session-dir", filepath.Join(l.Directory, "native"))
	c := exec.Command(l.Agent.Executable, args...)
	c.Dir = l.Workspace
	c.Env = os.Environ()
	for k, v := range l.Agent.Env {
		c.Env = append(c.Env, k+"="+v)
	}
	c.Env = append(c.Env, "RELAYDOCK_SESSION="+l.Session, "RELAYDOCK_BRIDGE="+l.Directory)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := c.Run()
	msg := "pi exited"
	if err != nil {
		msg = err.Error()
	}
	writeErr := localfile.Write(filepath.Join(l.Directory, "exit.json"), map[string]string{"time": protocol.Now(), "error": msg})
	if err != nil {
		return err
	}
	return writeErr
}

func (w *Worker) mux(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	argv := append([]string{"-L", w.c.Namespace}, args...)
	b, err := exec.CommandContext(ctx, w.c.Mux, argv...).CombinedOutput()
	if len(b) > protocol.MaxText {
		b = b[len(b)-protocol.MaxText:]
	}
	if err != nil {
		return b, fmt.Errorf("%s: %w: %s", w.c.Backend, err, b)
	}
	return b, nil
}

func (w *Worker) start(ctx context.Context, s *protocol.Session) error {
	dir := w.dir(s.ID)
	l := Launch{w.c.Agents[s.Agent], w.c.Workspaces[s.Workspace], w.c.Bridge, dir, s.ID}
	path := filepath.Join(dir, "launch.json")
	if err := localfile.Write(path, l); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.ndjson"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	// Both supported backends accept a multi-token argv after --. No task text
	// or shell command is constructed; the launcher inherits the terminal.
	_, err = w.mux(ctx, "new-session", "-d", "-s", s.MuxName, "-c", l.Workspace, "--", self, "_spawn", "--launch", path)
	if err != nil {
		return err
	}
	var panes []byte
	if w.c.Backend == "psmux" {
		panes, err = w.mux(ctx, "-t", s.MuxName, "list-panes", "-F", "#{pane_id}")
	} else {
		panes, err = w.mux(ctx, "list-panes", "-t", s.MuxName, "-F", "#{pane_id}")
	}
	if err != nil {
		return err
	}
	s.Pane = strings.TrimSpace(strings.SplitN(string(panes), "\n", 2)[0])
	if !regexp.MustCompile(`^%[0-9]+$`).MatchString(s.Pane) {
		return errors.New("could not identify original pi pane")
	}
	return w.db.Put("sessions", s.ID, *s)
}

func (w *Worker) validate() error {
	if !protocol.ValidID(w.c.ID) {
		return errors.New("invalid worker ID or mux namespace")
	}
	if len(w.c.Token) < 24 {
		return errors.New("worker token must have at least 24 characters")
	}
	if len(w.c.Agents) == 0 && !w.c.Shell.Enabled {
		return errors.New("configure pi profiles or enable shell")
	}
	if len(w.c.Agents) > 0 {
		if !protocol.ValidID(w.c.Namespace) {
			return errors.New("invalid mux namespace")
		}
		if w.c.Backend != "tmux" && w.c.Backend != "psmux" {
			return errors.New("backend must be tmux or psmux")
		}
		if _, err := exec.LookPath(w.c.Mux); err != nil {
			return err
		}
		if _, err := os.Stat(w.c.Bridge); err != nil {
			return fmt.Errorf("pi extension: %w", err)
		}
		if len(w.c.Workspaces) == 0 {
			return errors.New("configure at least one workspace for pi")
		}
	}

	for name, p := range w.c.Workspaces {
		if !protocol.ValidID(name) {
			return errors.New("invalid workspace alias")
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return err
		}
		real, err = filepath.Abs(real)
		if err != nil {
			return err
		}
		st, err := os.Stat(real)
		if err != nil {
			return err
		}
		if !st.IsDir() {
			return errors.New("workspace is not a directory")
		}
		w.c.Workspaces[name] = filepath.Clean(real)
	}
	for name, a := range w.c.Agents {
		if !protocol.ValidID(name) {
			return errors.New("invalid pi profile alias")
		}
		p, err := exec.LookPath(a.Executable)
		if err != nil {
			return err
		}
		a.Executable = p
		w.c.Agents[name] = a
	}
	return nil
}
