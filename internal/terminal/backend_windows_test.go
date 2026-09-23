package terminal

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"relaydock/internal/protocol"
)

// Exercise the exact pipes owned by Hangup with their other ends kept alive.
// A synchronous CreatePipe implementation hangs here while closing blocked I/O.
func TestConPTYHangupCancelsReadAndFullWrite(t *testing.T) {
	input, inputPeer, err := conPTYPipe(true)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(inputPeer)
	defer input.Close()
	output, outputPeer, err := conPTYPipe(false)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(outputPeer)
	defer output.Close()
	p := &windowsProcess{input: input, output: output}
	readDone, writeDone := make(chan error, 1), make(chan error, 1)
	go func() { _, err := p.Read(make([]byte, 1)); readDone <- err }()
	go func() { _, err := p.Write(make([]byte, 4<<20)); writeDone <- err }()
	for _, done := range []chan error{readDone, writeDone} {
		select {
		case err := <-done:
			t.Fatalf("I/O did not block: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	hungUp := make(chan struct{})
	go func() { p.Hangup(); p.Hangup(); close(hungUp) }()
	select {
	case <-hungUp:
	case <-time.After(2 * time.Second):
		t.Fatal("Hangup blocked with pending pipe I/O")
	}
	for _, done := range []chan error{readDone, writeDone} {
		select {
		case err := <-done:
			if !errors.Is(err, os.ErrClosed) {
				t.Errorf("canceled I/O error = %v, want os.ErrClosed", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Hangup did not cancel pending I/O")
		}
	}
	if err := p.Resize(protocol.TerminalSize{Cols: 80, Rows: 24}); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("resize after hangup = %v", err)
	}
}

type conPTYTestSession struct {
	p                  Process
	mu                 sync.Mutex
	output             []byte
	changed            chan struct{}
	readDone, exitDone chan struct{}
	result             protocol.TerminalExit
}

func startConPTYTest(t *testing.T, executable, cwd string) *conPTYTestSession {
	t.Helper()
	if !Supported() {
		t.Skip("ConPTY requires Windows 10 1809 or later")
	}
	p, err := Start(executable, cwd, "xterm-256color", protocol.TerminalSize{Cols: 110, Rows: 29})
	if err != nil {
		t.Fatal(err)
	}
	s := &conPTYTestSession{p: p, changed: make(chan struct{}, 1), readDone: make(chan struct{}), exitDone: make(chan struct{})}
	go func() { s.result = p.Wait(); close(s.exitDone) }()
	go func() {
		defer close(s.readDone)
		buf := make([]byte, protocol.TerminalChunk)
		for {
			n, err := p.Read(buf)
			s.mu.Lock()
			s.output = append(s.output, buf[:n]...)
			tooMuch := len(s.output) > 2<<20
			s.mu.Unlock()
			select {
			case s.changed <- struct{}{}:
			default:
			}
			if err != nil || tooMuch {
				return
			}
		}
	}()
	t.Cleanup(func() {
		p.Hangup()
		p.Kill()
		select {
		case <-s.exitDone:
		case <-time.After(5 * time.Second):
			t.Error("outer shell did not exit after Hangup/Kill")
		}
		select {
		case <-s.readDone:
		case <-time.After(2 * time.Second):
			t.Error("output read did not unblock after Hangup")
		}
	})
	s.command(t, "[Console]::WriteLine([char]95 + 'READY' + [char]95)")
	s.until(t, "_READY_")
	return s
}

func (s *conPTYTestSession) command(t *testing.T, command string) {
	t.Helper()
	if _, err := io.WriteString(s.p, command+"\r"); err != nil {
		t.Fatal(err)
	}
}

func (s *conPTYTestSession) until(t *testing.T, want string) {
	t.Helper()
	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		output := string(s.output)
		s.mu.Unlock()
		if strings.Contains(output, want) {
			return
		}
		select {
		case <-s.changed:
		case <-s.readDone:
			// Recheck once after the producer's final append.
			s.mu.Lock()
			output = string(s.output)
			s.mu.Unlock()
			if strings.Contains(output, want) {
				return
			}
			t.Fatalf("terminal closed before %q; output %q", want, output)
		case <-deadline.C:
			t.Fatalf("waiting for %q; output %q", want, output)
		}
	}
}

func TestConPTYPowerShellUnicodeEnvironmentResizeAndExit(t *testing.T) {
	for _, name := range []string{"powershell.exe", "pwsh.exe"} {
		t.Run(name, func(t *testing.T) {
			executable, err := exec.LookPath(name)
			if err != nil {
				t.Skipf("%s unavailable", name)
			}
			dir := filepath.Join(t.TempDir(), "中文 workspace's")
			if err = os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TERM", "replaced")
			t.Setenv("RELAYDOCK_TERMINAL_TEST", "继承🙂")
			// The test runner can itself be PowerShell 7, whose inherited module
			// path contains modules incompatible with Windows PowerShell 5.1.
			t.Setenv("PSModulePath", filepath.Join(filepath.Dir(executable), "Modules"))
			s := startConPTYTest(t, executable, dir)
			// Keep the real interactive editor enabled and complete Write-Output.
			// The command would fail if Tab were dropped or sent as literal text.
			s.command(t, "Write-Outp\t ([char]95 + 'TAB_COMPLETE' + [char]95)")
			s.until(t, "_TAB_COMPLETE_")
			s.command(t, "[Console]::WriteLine([char]95 + 'TTY:' + [Console]::IsInputRedirected + ':' + [Console]::IsOutputRedirected + [char]95)")
			s.until(t, "_TTY:False:False_")
			s.command(t, "[Console]::WriteLine([char]95 + $PWD.Path + [char]95)")
			s.until(t, "_"+dir+"_")
			s.command(t, "[Console]::WriteLine([char]95 + $env:TERM + ':' + $env:RELAYDOCK_TERMINAL_TEST + [char]95)")
			s.until(t, "_xterm-256color:继承🙂_")
			s.command(t, "[Console]::WriteLine([char]27 + '[31m' + [char]95 + 'RED' + [char]95 + [char]27 + '[0m')")
			s.until(t, "_RED_")
			s.mu.Lock()
			ansi := bytes.Contains(s.output, []byte("\x1b[31m"))
			s.mu.Unlock()
			if !ansi {
				t.Error("terminal did not preserve ANSI color output")
			}
			if err := s.p.Resize(protocol.TerminalSize{Cols: 117, Rows: 39}); err != nil {
				t.Fatal(err)
			}
			s.command(t, "[Console]::WriteLine([char]95 + 'SIZE:' + [Console]::WindowWidth + ':' + [Console]::WindowHeight + [char]95)")
			s.until(t, "_SIZE:117:39_")
			s.command(t, "exit 513")
			select {
			case <-s.exitDone:
				if s.result.Code != 513 || s.result.Error != "" {
					t.Fatalf("exit = %+v", s.result)
				}
			case <-time.After(8 * time.Second):
				t.Fatal("PowerShell exit did not complete")
			}
		})
	}
}

func TestConPTYStartRejectsInvalidInput(t *testing.T) {
	if !Supported() {
		t.Skip("ConPTY unavailable")
	}
	if p, err := Start("powershell.exe", t.TempDir(), "xterm", protocol.TerminalSize{}); err == nil || p != nil {
		t.Fatalf("invalid dimensions accepted: %v, %v", p, err)
	}
	if p, err := Start("relaydock-no-such-shell.exe", t.TempDir(), "xterm", protocol.TerminalSize{Cols: 80, Rows: 24}); err == nil || p != nil {
		t.Fatalf("missing executable accepted: %v, %v", p, err)
	}
}
