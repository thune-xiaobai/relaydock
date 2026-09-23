//go:build windows

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Use a hidden real Windows console, then run the built CLI as its child. This
// covers terminal.Run, console input/output, stream transport and exit handling
// without touching the developer's console or displaying a desktop window.
func TestWindowsShellCLIInRealConsole(t *testing.T) {
	f := setupWindowsTerminal(t, 4)
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "relaydock.exe")
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go.exe"), "build", "-o", bin, "./cmd/relaydock")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v: %s", err, out)
	}
	configPath := filepath.Join(t.TempDir(), "client.json")
	raw, err := json.Marshal(f.client)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"exit", "escape", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWindowsShellCLIConsoleHelper$", "-test.v")
			cmd.Env = append(os.Environ(), "RELAYDOCK_CLI_HELPER="+mode, "RELAYDOCK_CLI_BIN="+bin, "RELAYDOCK_CLI_CONFIG="+configPath, "RELAYDOCK_CLI_REPORT="+dir)
			cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE, HideWindow: true}
			type childResult struct {
				out []byte
				err error
			}
			done := make(chan childResult, 1)
			go func() { out, err := cmd.CombinedOutput(); done <- childResult{out, err} }()
			if mode == "disconnect" {
				deadline := time.Now().Add(25 * time.Second)
				for {
					if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
						break
					}
					select {
					case result := <-done:
						t.Fatalf("CLI helper stopped before disconnect: %v: %s", result.err, result.out)
					case <-time.After(20 * time.Millisecond):
					}
					if time.Now().After(deadline) {
						t.Fatal("CLI helper did not become ready")
					}
				}
				f.h.mu.Lock()
				for _, bridge := range f.h.terminals {
					bridge.close()
				}
				f.h.mu.Unlock()
			}
			result := <-done
			if result.err != nil {
				t.Fatalf("real console CLI: %v: %s", result.err, result.out)
			}
			t.Logf("%s", result.out)
			remoteEventually(t, func() bool { f.h.mu.Lock(); defer f.h.mu.Unlock(); return len(f.h.terminals) == 0 })
		})
	}
}

func TestWindowsShellCLIConsoleHelper(t *testing.T) {
	mode := os.Getenv("RELAYDOCK_CLI_HELPER")
	if mode == "" {
		t.Skip("run in a separate hidden console")
	}
	in, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	input, output := windows.Handle(in.Fd()), windows.Handle(out.Fd())
	if err := windows.SetConsoleCP(437); err != nil {
		t.Fatal(err)
	}
	if err := windows.SetConsoleOutputCP(437); err != nil {
		t.Fatal(err)
	}
	var beforeInput, beforeOutput uint32
	if err := windows.GetConsoleMode(input, &beforeInput); err != nil {
		t.Fatal(err)
	}
	if err := windows.GetConsoleMode(output, &beforeOutput); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Getenv("RELAYDOCK_CLI_BIN"), "shell", "--config", os.Getenv("RELAYDOCK_CLI_CONFIG"), "--node", "n", "--cwd", "project")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	defer func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}()
	readScreen := func() string {
		t.Helper()
		var info windows.ConsoleScreenBufferInfo
		if err := windows.GetConsoleScreenBufferInfo(output, &info); err != nil {
			t.Fatal(err)
		}
		chars := make([]uint16, int(info.Size.X)*int(info.Size.Y))
		var count uint32
		proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("ReadConsoleOutputCharacterW")
		ok, _, err := proc.Call(uintptr(output), uintptr(unsafe.Pointer(&chars[0])), uintptr(len(chars)), 0, uintptr(unsafe.Pointer(&count)))
		if ok == 0 {
			t.Fatal(err)
		}
		return string(utf16.Decode(chars[:count]))
	}
	waitScreen := func(want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		var screen string
		for time.Now().Before(deadline) {
			screen = readScreen()
			if strings.Contains(screen, want) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("waiting for console text %q: %q", want, strings.TrimSpace(screen))
	}
	typeInput := func(text string) { t.Helper(); windowsCLIInput(t, input, text) }
	waitScreen("PS ")
	typeInput("function prompt { 'CLI_' + 'PROMPT> ' }; Remove-Module PSReadLine -ErrorAction SilentlyContinue\r")
	waitScreen("CLI_PROMPT> ")
	typeInput("[Console]::WriteLine([char]95 + 'CLI_READY' + [char]95)\r")
	waitScreen("_CLI_READY_")
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(output, &info); err != nil {
		t.Fatal(err)
	}
	rect := info.Window
	rect.Right -= 5
	rect.Bottom -= 2
	setWindow := windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleWindowInfo")
	if ok, _, err := setWindow.Call(uintptr(output), 1, uintptr(unsafe.Pointer(&rect))); ok == 0 {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond) // Let the CLI's idle resize poll fire.
	typeInput("[Console]::WriteLine([char]95 + 'CLI_SIZE:' + [Console]::WindowHeight + ':' + [Console]::WindowWidth + [char]95)\r")
	waitScreen(fmt.Sprintf("_CLI_SIZE:%d:%d_", rect.Bottom-rect.Top+1, rect.Right-rect.Left+1))
	wantCode := 0
	switch mode {
	case "exit":
		typeInput("exit 3010\r")
		wantCode = 3010
	case "escape":
		typeInput("~.")
	case "disconnect":
		if err := os.WriteFile(filepath.Join(os.Getenv("RELAYDOCK_CLI_REPORT"), "ready"), []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		wantCode = 1
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("CLI did not exit")
	}
	if code := cmd.ProcessState.ExitCode(); code != wantCode {
		t.Fatalf("CLI exit %d, want %d: %q", code, wantCode, strings.TrimSpace(readScreen()))
	}
	var afterInput, afterOutput uint32
	if err := windows.GetConsoleMode(input, &afterInput); err != nil {
		t.Fatal(err)
	}
	if err := windows.GetConsoleMode(output, &afterOutput); err != nil {
		t.Fatal(err)
	}
	inputCP, inputErr := windows.GetConsoleCP()
	outputCP, outputErr := windows.GetConsoleOutputCP()
	if beforeInput != afterInput || beforeOutput != afterOutput || inputCP != 437 || outputCP != 437 || inputErr != nil || outputErr != nil {
		t.Fatalf("CLI did not restore console: modes %#x/%#x -> %#x/%#x, code pages %d/%d, errors %v/%v", beforeInput, beforeOutput, afterInput, afterOutput, inputCP, outputCP, inputErr, outputErr)
	}
	t.Log("CLI console restored; exit " + strconv.Itoa(wantCode))
}

func windowsCLIInput(t *testing.T, handle windows.Handle, text string) {
	t.Helper()
	type keyRecord struct {
		EventType  uint16
		Padding    uint16
		KeyDown    int32
		Repeat     uint16
		VirtualKey uint16
		Scan       uint16
		Char       uint16
		Control    uint32
	}
	chars := utf16.Encode([]rune(text))
	records := make([]keyRecord, 0, len(chars)*2)
	dll := windows.NewLazySystemDLL("user32.dll")
	keyScan := dll.NewProc("VkKeyScanW")
	mapKey := dll.NewProc("MapVirtualKeyW")
	for _, char := range chars {
		key, _, _ := keyScan.Call(uintptr(char))
		record := keyRecord{EventType: 1, KeyDown: 1, Repeat: 1, Char: char}
		if int16(key) != -1 {
			record.VirtualKey = uint16(key & 255)
			scan, _, _ := mapKey.Call(uintptr(record.VirtualKey), 0)
			record.Scan = uint16(scan)
			if key&0x100 != 0 {
				record.Control |= 0x10 // SHIFT_PRESSED
			}
			if key&0x200 != 0 {
				record.Control |= 0x8 // LEFT_CTRL_PRESSED
			}
			if key&0x400 != 0 {
				record.Control |= 0x2 // LEFT_ALT_PRESSED
			}
		}
		records = append(records, record)
		record.KeyDown = 0
		records = append(records, record)
	}
	var count uint32
	write := windows.NewLazySystemDLL("kernel32.dll").NewProc("WriteConsoleInputW")
	if ok, _, err := write.Call(uintptr(handle), uintptr(unsafe.Pointer(&records[0])), uintptr(len(records)), uintptr(unsafe.Pointer(&count))); ok == 0 || count != uint32(len(records)) {
		t.Fatalf("write CLI console input: %d records, %v", count, err)
	}
}
