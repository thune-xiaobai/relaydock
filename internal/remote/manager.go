// Package remote runs noninteractive shell jobs independently of WSS calls.
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"relaydock/internal/config"
	"relaydock/internal/protocol"
	"relaydock/internal/store"
)

type activeJob struct {
	out, err  *capture
	cancel    chan struct{}
	cancelled bool
	done      chan struct{}
}
type Manager struct {
	mu     sync.Mutex
	c      config.Config
	db     *store.Store
	active map[string]*activeJob
	closed bool
}

var ErrUncertain = errors.New("shell execution uncertain")

func New(c config.Config, db *store.Store) (*Manager, error) {
	s := &c.Shell
	if s.Kind == "" {
		s.Kind = "sh"
		if runtime.GOOS == "windows" {
			s.Kind = "powershell"
		}
	}
	if s.Kind != "sh" && s.Kind != "powershell" {
		return nil, errors.New("shell.kind must be sh or powershell")
	}
	if s.Executable == "" {
		s.Executable = "/bin/sh"
		if s.Kind == "powershell" {
			s.Executable = "pwsh"
			if _, e := exec.LookPath(s.Executable); e != nil && runtime.GOOS == "windows" {
				s.Executable = "powershell.exe"
			}
		}
	}
	path, err := exec.LookPath(s.Executable)
	if err != nil {
		return nil, fmt.Errorf("shell executable: %w", err)
	}
	s.Executable = path
	if s.MaxRunning == 0 {
		s.MaxRunning = 4
	}
	if s.MaxTimeoutMS == 0 {
		s.MaxTimeoutMS = 3600000
	}
	if s.MaxOutputBytes == 0 {
		s.MaxOutputBytes = 1 << 20
	}
	if s.MaxRunning < 1 || s.MaxRunning > 64 || s.MaxTimeoutMS < 1 || s.MaxTimeoutMS > 86400000 || s.MaxOutputBytes < 64 || s.MaxOutputBytes > 16<<20 {
		return nil, errors.New("invalid shell limits (jobs 1..64, timeout 1..86400000 ms, bytes 64..16777216 per stream)")
	}
	m := &Manager{c: c, db: db, active: map[string]*activeJob{}}
	all, err := db.List("remote_jobs")
	if err != nil {
		return nil, err
	}
	for id, raw := range all {
		var j protocol.RemoteJob
		if err = json.Unmarshal(raw, &j); err != nil {
			return nil, err
		}
		if j.ID != id || !protocol.ValidID(id) {
			return nil, errors.New("invalid stored shell job")
		}
		if !j.Terminal() {
			j.Status = "unknown"
			j.Revision++
			j.FinishedAt = protocol.Now()
			j.Error = "Worker restarted before a durable exit result; command was not replayed. Do not kill or reuse a saved PID."
			if err = m.saveTerminal(j); err != nil {
				return nil, err
			}
		}
	}
	return m, nil
}
func (m *Manager) Info() *protocol.ShellInfo {
	s := m.c.Shell
	return &protocol.ShellInfo{Kind: s.Kind, Executable: s.Executable, MaxRunning: s.MaxRunning, MaxTimeoutMS: s.MaxTimeoutMS, MaxOutputBytes: s.MaxOutputBytes}
}
func (m *Manager) dir(id string) string { return filepath.Join(m.c.StateDir, "shell-jobs", id) }
func (m *Manager) command(j protocol.RemoteJob) (*exec.Cmd, error) {
	if m.c.Shell.Kind == "sh" {
		return exec.Command(m.c.Shell.Executable, "-c", j.Command), nil
	}
	// UTF-8 BOM also works with Windows PowerShell 5.1. The command lives in a
	// script file, avoiding command-line length and quoting transformations.
	script := filepath.Join(m.dir(j.ID), "command.ps1")
	text := "\ufeff$ErrorActionPreference = 'Stop'\n[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)\n$OutputEncoding = [Console]::OutputEncoding\n$global:LASTEXITCODE = 0\ntry {\n" + j.Command + "\nif (-not $?) { if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }; exit 1 }\nexit 0\n} catch { [Console]::Error.WriteLine($_.ToString()); exit 1 }\n"
	if err := os.WriteFile(script, []byte(text), 0600); err != nil {
		return nil, err
	}
	return exec.Command(m.c.Shell.Executable, "-NoLogo", "-NoProfile", "-NonInteractive", "-File", script), nil
}

func (m *Manager) Start(ctx context.Context, callID string, a protocol.RemoteArgs) (protocol.RemoteSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return protocol.RemoteSnapshot{}, err
	}
	if m.closed {
		return protocol.RemoteSnapshot{}, errors.New("Worker is closing")
	}
	if !protocol.ValidID(a.JobID) || strings.TrimSpace(a.Command) == "" || len(a.Command) > protocol.MaxText {
		return protocol.RemoteSnapshot{}, errors.New("valid job_id and 1..65536 byte command required")
	}
	if len(m.active) >= m.c.Shell.MaxRunning {
		return protocol.RemoteSnapshot{}, errors.New("shell job capacity reached")
	}
	var old protocol.RemoteJob
	if e := m.db.Get("remote_jobs", a.JobID, &old); e == nil {
		return protocol.RemoteSnapshot{}, errors.New("job_id already exists; query it instead of rerunning")
	} else if !errors.Is(e, store.ErrMissing) {
		return protocol.RemoteSnapshot{}, e
	}
	cwd := a.CWD
	if p, ok := m.c.Workspaces[cwd]; ok {
		cwd = p
	}
	if !filepath.IsAbs(cwd) {
		return protocol.RemoteSnapshot{}, errors.New("cwd must be a configured workspace alias or absolute directory")
	}
	cwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return protocol.RemoteSnapshot{}, err
	}
	st, err := os.Stat(cwd)
	if err != nil {
		return protocol.RemoteSnapshot{}, err
	}
	if !st.IsDir() {
		return protocol.RemoteSnapshot{}, errors.New("cwd is not a directory")
	}
	if a.TimeoutMS == 0 {
		a.TimeoutMS = min(60000, m.c.Shell.MaxTimeoutMS)
	}
	if a.TimeoutMS < 1 || a.TimeoutMS > m.c.Shell.MaxTimeoutMS {
		return protocol.RemoteSnapshot{}, errors.New("timeout_ms exceeds configured shell limit")
	}
	j := protocol.RemoteJob{ID: a.JobID, Node: m.c.ID, CallID: callID, Command: a.Command, CWD: cwd, Shell: m.c.Shell.Kind, Status: "starting", Revision: 1, CreatedAt: protocol.Now(), TimeoutMS: a.TimeoutMS}
	if err = m.db.Put("remote_jobs", j.ID, j); err != nil {
		return protocol.RemoteSnapshot{}, err
	}
	fail := func(err error) (protocol.RemoteSnapshot, error) {
		j.Status = "failed"
		j.Revision++
		j.FinishedAt = protocol.Now()
		j.Error = err.Error()
		if e := m.saveTerminal(j); e != nil {
			return protocol.RemoteSnapshot{}, e
		}
		return m.snapshot(j, "", 0)
	}
	if err = os.MkdirAll(m.dir(j.ID), 0700); err != nil {
		return fail(err)
	}
	cmd, err := m.command(j)
	if err != nil {
		return fail(err)
	}
	out, err := os.OpenFile(filepath.Join(m.dir(j.ID), "stdout"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fail(err)
	}
	errFile, err := os.OpenFile(filepath.Join(m.dir(j.ID), "stderr"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		out.Close()
		return fail(err)
	}
	r := &activeJob{out: &capture{file: out, cap: m.c.Shell.MaxOutputBytes}, err: &capture{file: errFile, cap: m.c.Shell.MaxOutputBytes}, cancel: make(chan struct{}), done: make(chan struct{})}
	cmd.Dir = j.CWD
	cmd.Stdout = r.out
	cmd.Stderr = r.err
	cmd.WaitDelay = 2 * time.Second
	tree, err := startProcess(cmd)
	if err != nil {
		r.out.close()
		r.err.close()
		return fail(err)
	}
	j.Status = "running"
	j.Revision++
	if err = m.db.Put("remote_jobs", j.ID, j); err != nil {
		_ = tree.kill()
		_ = cmd.Wait()
		tree.close()
		r.out.close()
		r.err.close()
		return protocol.RemoteSnapshot{}, fmt.Errorf("%w: process started but state persistence failed: %v", ErrUncertain, err)
	}
	m.active[j.ID] = r
	go m.wait(j, r, cmd, tree)
	s, err := m.snapshot(j, "", 0)
	if err != nil {
		return s, fmt.Errorf("%w: %v", ErrUncertain, err)
	}
	return s, nil
}

func (m *Manager) wait(j protocol.RemoteJob, r *activeJob, cmd *exec.Cmd, tree *processTree) {
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	timer := time.NewTimer(time.Duration(j.TimeoutMS) * time.Millisecond)
	defer timer.Stop()
	status := ""
	var err, killErr error
	select {
	case err = <-wait:
	case <-r.cancel:
		status = "cancelled"
		killErr = tree.kill()
		if killErr != nil {
			_ = cmd.Process.Kill()
		}
		err = <-wait
	case <-timer.C:
		status = "timed_out"
		killErr = tree.kill()
		if killErr != nil {
			_ = cmd.Process.Kill()
		}
		err = <-wait
	}
	tree.close()
	r.out.close()
	r.err.close()
	code := cmd.ProcessState.ExitCode()
	j.ExitCode = &code
	if status == "" {
		status = "succeeded"
		if err != nil {
			status = "failed"
		}
	}
	j.Status = status
	j.Revision++
	j.FinishedAt = protocol.Now()
	if err != nil {
		j.Error = err.Error()
	}
	if killErr != nil {
		j.Error += "; process tree cancellation: " + killErr.Error()
		j.Status = "unknown"
	}
	var ot, et bool
	var oe, ee error
	j.StdoutBytes, ot, oe = r.out.info()
	j.StderrBytes, et, ee = r.err.info()
	j.Truncated = ot || et
	if captureErr := errors.Join(oe, ee); captureErr != nil {
		j.Error += "; output capture: " + captureErr.Error()
		j.Status = "unknown"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	defer close(r.done)
	if err = m.saveTerminal(j); err != nil {
		log.Printf("shell job %s final result could not be saved: %v", j.ID, err)
	}
	delete(m.active, j.ID)
}
func (m *Manager) saveTerminal(j protocol.RemoteJob) error {
	s, err := m.snapshot(j, "", 0)
	if err != nil {
		j.Status = "unknown"
		j.Error += "; unable to read saved output: " + err.Error()
		s = protocol.RemoteSnapshot{Job: j}
	}
	return m.db.Update(func(t *store.Tx) error {
		if e := t.Put("remote_jobs", j.ID, j); e != nil {
			return e
		}
		return t.Put("remote_outbox", j.ID, s)
	})
}
func (m *Manager) snapshot(j protocol.RemoteJob, cursor string, limit int) (protocol.RemoteSnapshot, error) {
	s := protocol.RemoteSnapshot{Job: j, Cursor: cursor}
	if limit == 0 {
		limit = 16384
	}
	if limit < 4 || limit > 32768 {
		return s, errors.New("limit must be 4..32768 bytes per stream")
	}
	a, b, err := parseCursor(cursor)
	if err != nil {
		return s, err
	}
	var am, bm bool
	s.Stdout, a, am, err = readOutput(filepath.Join(m.dir(j.ID), "stdout"), a, limit, j.Terminal())
	if err != nil {
		return s, err
	}
	s.Stderr, b, bm, err = readOutput(filepath.Join(m.dir(j.ID), "stderr"), b, limit, j.Terminal())
	if err != nil {
		return s, err
	}
	s.NextCursor = fmt.Sprintf("%d:%d", a, b)
	s.HasMore = am || bm
	return s, nil
}
func (m *Manager) Status(id, cursor string, limit int) (protocol.RemoteSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !protocol.ValidID(id) {
		return protocol.RemoteSnapshot{}, errors.New("invalid job_id")
	}
	var j protocol.RemoteJob
	if err := m.db.Get("remote_jobs", id, &j); err != nil {
		return protocol.RemoteSnapshot{}, err
	}
	r := m.active[id]
	if r != nil {
		var a, b bool
		j.StdoutBytes, a, _ = r.out.info()
		j.StderrBytes, b, _ = r.err.info()
		j.Truncated = a || b
	} else if !j.Terminal() {
		j.Status = "unknown"
		j.Error = "no active process or durable final result"
		j.Revision++
	}
	s, err := m.snapshot(j, cursor, limit)
	if r != nil {
		s.CancelRequested = r.cancelled
	}
	return s, err
}
func (m *Manager) Cancel(id string) (protocol.RemoteSnapshot, error) {
	m.mu.Lock()
	r := m.active[id]
	if r != nil && !r.cancelled {
		r.cancelled = true
		close(r.cancel)
	}
	m.mu.Unlock()
	s, err := m.Status(id, "", 0)
	if err == nil && r == nil && !s.Job.Terminal() {
		return s, errors.New("no live process handle; cancellation result unknown")
	}
	if err == nil && s.Job.Status == "unknown" {
		return s, errors.New("cannot cancel an uncertain process from a previous Worker; inspect locally")
	}
	return s, err
}
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	var done []chan struct{}
	for _, r := range m.active {
		if !r.cancelled {
			r.cancelled = true
			close(r.cancel)
		}
		done = append(done, r.done)
	}
	m.mu.Unlock()
	for _, d := range done {
		<-d
	}
}
