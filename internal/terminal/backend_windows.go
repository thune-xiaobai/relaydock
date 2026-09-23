package terminal

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
	"relaydock/internal/protocol"
)

var (
	conPTYDLL             = windows.NewLazySystemDLL("kernel32.dll")
	createConPTY          = conPTYDLL.NewProc("CreatePseudoConsole")
	resizeConPTY          = conPTYDLL.NewProc("ResizePseudoConsole")
	closeConPTY           = conPTYDLL.NewProc("ClosePseudoConsole")
	updateConPTYAttribute = conPTYDLL.NewProc("UpdateProcThreadAttribute")
)

// ConPTY requires Windows 10 1809 / Windows Server 2019 or later. Check the
// exports before advertising the capability, rather than panicking on old hosts.
func Supported() bool {
	return createConPTY.Find() == nil && resizeConPTY.Find() == nil && closeConPTY.Find() == nil
}
func SupportsShell(kind string) bool { return kind == "powershell" && Supported() }

type windowsProcess struct {
	input, output *os.File
	consoleMu     sync.Mutex
	console       windows.Handle
	processMu     sync.Mutex
	process       windows.Handle
	once          sync.Once
}

func Start(executable, cwd, term string, size protocol.TerminalSize) (_ Process, err error) {
	if !Supported() {
		return nil, ErrUnsupported
	}
	if err := size.Validate(); err != nil {
		return nil, err
	}
	executable, err = exec.LookPath(executable)
	if err != nil {
		return nil, err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, err
	}
	app, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		return nil, err
	}
	// Console applications must agree with ConPTY's UTF-8 byte stream. The
	// Windows PowerShell default OEM code page otherwise replaces emoji and
	// other nonlocal characters when .NET or native programs write to stdout.
	// Keep profiles enabled, then initialize encoding before the first prompt.
	const initialize = "[Console]::InputEncoding = [Console]::OutputEncoding = $OutputEncoding = New-Object System.Text.UTF8Encoding"
	command, err := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{executable, "-NoLogo", "-NoExit", "-Command", initialize}))
	if err != nil {
		return nil, err
	}
	dir, err := windows.UTF16PtrFromString(cwd)
	if err != nil {
		return nil, err
	}
	env, err := terminalEnvironment(term)
	if err != nil {
		return nil, err
	}

	input, consoleInput, err := conPTYPipe(true)
	if err != nil {
		return nil, fmt.Errorf("create terminal input: %w", err)
	}
	defer windows.CloseHandle(consoleInput)
	defer func() {
		if err != nil {
			_ = input.Close()
		}
	}()
	output, consoleOutput, err := conPTYPipe(false)
	if err != nil {
		return nil, fmt.Errorf("create terminal output: %w", err)
	}
	defer windows.CloseHandle(consoleOutput)
	defer func() {
		if err != nil {
			_ = output.Close()
		}
	}()
	var console windows.Handle
	err = windows.CreatePseudoConsole(windows.Coord{X: int16(size.Cols), Y: int16(size.Rows)}, consoleInput, consoleOutput, 0, &console)
	if err != nil {
		return nil, fmt.Errorf("create pseudoconsole: %w", err)
	}
	defer func() {
		if err != nil {
			// Close output before ConPTY, including startup failures: older Windows
			// waits for its final output frame and otherwise deadlocks on a full pipe.
			_ = output.Close()
			_ = input.Close()
			windows.ClosePseudoConsole(console)
		}
	}()
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, err
	}
	defer attrs.Delete()
	// PSEUDOCONSOLE uniquely takes the handle value as lpValue, not &handle.
	// Keep that opaque Windows value as uintptr (not a Go unsafe.Pointer).
	ok, _, callErr := updateConPTYAttribute.Call(uintptr(unsafe.Pointer(attrs.List())), 0,
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, uintptr(console), unsafe.Sizeof(console), 0, 0)
	runtime.KeepAlive(attrs)
	if ok == 0 {
		return nil, fmt.Errorf("associate pseudoconsole: %w", callErr)
	}
	startup := windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	startup.Cb = uint32(unsafe.Sizeof(startup))
	// Explicit NULL standard handles prevent Windows from duplicating the
	// Worker's redirected stdio into this child instead of connecting ConPTY.
	// https://github.com/microsoft/terminal/discussions/15814
	startup.Flags = windows.STARTF_USESTDHANDLES
	var info windows.ProcessInformation
	// In particular, do not assign a KILL_ON_JOB_CLOSE Job Object: detached
	// psmux servers must survive the lifetime of this terminal's outer shell.
	err = windows.CreateProcess(app, command, nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT,
		&env[0], dir, &startup.StartupInfo, &info)
	if err != nil {
		return nil, fmt.Errorf("start interactive PowerShell: %w", err)
	}
	_ = windows.CloseHandle(info.Thread)
	return &windowsProcess{input: input, output: output, console: console, process: info.Process}, nil
}

// ConPTY requires synchronous handles on its side. The host side instead uses
// OVERLAPPED I/O and Go's poller, so closing it cancels even a blocked write to a
// full input pipe. Anonymous CreatePipe handles cannot provide that guarantee.
func conPTYPipe(input bool) (*os.File, windows.Handle, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, 0, err
	}
	name, err := windows.UTF16PtrFromString(fmt.Sprintf(`\\.\pipe\relaydock-terminal-%x`, id))
	if err != nil {
		return nil, 0, err
	}
	mode, access := uint32(windows.PIPE_ACCESS_INBOUND), uint32(windows.GENERIC_WRITE)
	if input {
		mode, access = windows.PIPE_ACCESS_OUTBOUND, windows.GENERIC_READ
	}
	host, err := windows.CreateNamedPipe(name, mode|windows.FILE_FLAG_OVERLAPPED|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS, 1,
		protocol.TerminalChunk, protocol.TerminalChunk, 0, nil)
	if err != nil {
		return nil, 0, err
	}
	child, err := windows.CreateFile(name, access, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		_ = windows.CloseHandle(host)
		return nil, 0, err
	}
	// The client connected between CreateNamedPipe and ConnectNamedPipe.
	// No wait is needed; ERROR_PIPE_CONNECTED confirms the established pair.
	var overlapped windows.Overlapped
	if err = windows.ConnectNamedPipe(host, &overlapped); err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
		_ = windows.CloseHandle(child)
		_ = windows.CloseHandle(host)
		return nil, 0, err
	}
	return os.NewFile(uintptr(host), "terminal-pipe"), child, nil
}

func terminalEnvironment(term string) ([]uint16, error) {
	env := shellEnvironment(os.Environ(), term)
	// Windows environment blocks are case-insensitively sorted UTF-16 strings,
	// each NUL terminated, followed by one extra NUL.
	sort.Slice(env, func(i, j int) bool { return strings.ToUpper(env[i]) < strings.ToUpper(env[j]) })
	var block []uint16
	for _, value := range env {
		encoded, err := windows.UTF16FromString(value)
		if err != nil {
			return nil, err
		}
		block = append(block, encoded...)
	}
	return append(block, 0), nil
}

func (p *windowsProcess) Read(b []byte) (int, error)  { return p.output.Read(b) }
func (p *windowsProcess) Write(b []byte) (int, error) { return p.input.Write(b) }
func (p *windowsProcess) Resize(size protocol.TerminalSize) error {
	if err := size.Validate(); err != nil {
		return err
	}
	p.consoleMu.Lock()
	defer p.consoleMu.Unlock()
	if p.console == 0 {
		return os.ErrClosed
	}
	return windows.ResizePseudoConsole(p.console, windows.Coord{X: int16(size.Cols), Y: int16(size.Rows)})
}
func (p *windowsProcess) Hangup() {
	p.once.Do(func() {
		_ = p.input.Close()
		_ = p.output.Close()
		p.consoleMu.Lock()
		console := p.console
		p.console = 0
		p.consoleMu.Unlock()
		// Older Windows can wait for attached clients' CTRL_CLOSE_EVENT handlers.
		// Leave Serve free to enforce its outer-shell kill deadline in that case.
		if console != 0 {
			go windows.ClosePseudoConsole(console)
		}
	})
}
func (p *windowsProcess) Kill() {
	p.processMu.Lock()
	defer p.processMu.Unlock()
	if p.process != 0 {
		_ = windows.TerminateProcess(p.process, 1)
	}
}
func (p *windowsProcess) Wait() protocol.TerminalExit {
	p.processMu.Lock()
	process := p.process
	p.processMu.Unlock()
	defer func() {
		p.processMu.Lock()
		_ = windows.CloseHandle(process)
		p.process = 0
		p.processMu.Unlock()
	}()
	if _, err := windows.WaitForSingleObject(process, windows.INFINITE); err != nil {
		return protocol.TerminalExit{Code: -1, Error: err.Error()}
	}
	var code uint32
	if err := windows.GetExitCodeProcess(process, &code); err != nil {
		return protocol.TerminalExit{Code: -1, Error: err.Error()}
	}
	return protocol.TerminalExit{Code: int64(code)}
}
