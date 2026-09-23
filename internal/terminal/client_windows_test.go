package terminal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Give the helper a private, hidden native console so this test neither changes
// the invoking terminal nor opens an interactive window on the desktop.
func TestWindowsConsole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWindowsConsoleHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "RELAYDOCK_CONSOLE_HELPER=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE, HideWindow: true}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hidden console helper: %v\n%s", err, output)
	}
	t.Logf("%s", output)
}

func TestWindowsConsoleHelper(t *testing.T) {
	if os.Getenv("RELAYDOCK_CONSOLE_HELPER") != "1" {
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
	inHandle, outHandle := windows.Handle(in.Fd()), windows.Handle(out.Fd())
	// Start with legacy code pages to prove that session setup and teardown
	// do not silently leave the user's console configured for UTF-8.
	if err := windows.SetConsoleCP(437); err != nil {
		t.Fatal(err)
	}
	if err := windows.SetConsoleOutputCP(437); err != nil {
		t.Fatal(err)
	}
	var oldInput, oldOutput uint32
	if err := windows.GetConsoleMode(inHandle, &oldInput); err != nil {
		t.Fatal(err)
	}
	if err := windows.GetConsoleMode(outHandle, &oldOutput); err != nil {
		t.Fatal(err)
	}
	checkRestored := func() {
		t.Helper()
		var inputMode, outputMode uint32
		if err := windows.GetConsoleMode(inHandle, &inputMode); err != nil {
			t.Fatal("caller input closed:", err)
		}
		if err := windows.GetConsoleMode(outHandle, &outputMode); err != nil {
			t.Fatal("caller output closed:", err)
		}
		inputCP, inErr := windows.GetConsoleCP()
		outputCP, outErr := windows.GetConsoleOutputCP()
		if inputMode != oldInput || outputMode != oldOutput || inputCP != 437 || outputCP != 437 || inErr != nil || outErr != nil {
			t.Fatalf("console not restored: modes %#x/%#x (want %#x/%#x), code pages %d/%d, errors %v/%v",
				inputMode, outputMode, oldInput, oldOutput, inputCP, outputCP, inErr, outErr)
		}
	}
	con, err := prepareConsole(in, out)
	if err != nil {
		t.Fatal(err)
	}
	defer con.restore()
	var mode uint32
	if err := windows.GetConsoleMode(inHandle, &mode); err != nil {
		t.Fatal(err)
	}
	if mode&(windows.ENABLE_ECHO_INPUT|windows.ENABLE_LINE_INPUT|windows.ENABLE_PROCESSED_INPUT|windows.ENABLE_QUICK_EDIT_MODE) != 0 || mode&windows.ENABLE_VIRTUAL_TERMINAL_INPUT == 0 {
		t.Fatalf("input is not raw VT mode: %#x", mode)
	}
	if err := windows.GetConsoleMode(outHandle, &mode); err != nil || mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING == 0 {
		t.Fatalf("output is not VT mode: %#x, %v", mode, err)
	}

	t.Run("control and Unicode input", func(t *testing.T) {
		want := []byte("\x03\t\x1b[A中文😀\x00\x1a\b\rz")
		injectConsoleText(t, inHandle, string(want))
		got := make([]byte, len(want))
		// One-byte reads exercise leftovers of multibyte UTF-8 characters.
		for i := range got {
			if _, err := io.ReadFull(con.input, got[i:i+1]); err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("input %q, want %q", got, want)
		}
	})
	t.Run("arrow key translation", func(t *testing.T) {
		injectConsoleRecords(t, inHandle, []consoleKeyRecord{{EventType: 1, KeyDown: 1, Repeat: 1, VirtualKey: 0x26}})
		var got [3]byte
		if _, err := io.ReadFull(con.input, got[:]); err != nil {
			t.Fatal(err)
		}
		if string(got[:]) != "\x1b[A" {
			t.Fatalf("Up arrow: %q", got)
		}
	})
	t.Run("surrogate across native reads", func(t *testing.T) {
		injectConsoleRecords(t, inHandle, []consoleKeyRecord{{EventType: 1, KeyDown: 1, Repeat: 1, Char: 0xd83d}})
		time.Sleep(20 * time.Millisecond)
		injectConsoleRecords(t, inHandle, []consoleKeyRecord{{EventType: 1, KeyDown: 1, Repeat: 1, Char: 0xde00}})
		var got [4]byte
		if _, err := io.ReadFull(con.input, got[:]); err != nil {
			t.Fatal(err)
		}
		if string(got[:]) != "😀" {
			t.Fatalf("split surrogate: %q", got)
		}
	})
	t.Run("ANSI and split Unicode output", func(t *testing.T) {
		if _, err := con.output.Write([]byte("\x1b[2J\x1b[H\x1b[31m")); err != nil {
			t.Fatal(err)
		}
		for _, b := range []byte("中文red") {
			if _, err := con.output.Write([]byte{b}); err != nil {
				t.Fatal(err)
			}
		}
		// Console buffer reads count cells; each Chinese character uses two.
		var chars [7]uint16
		var count uint32
		proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("ReadConsoleOutputCharacterW")
		ok, _, err := proc.Call(uintptr(outHandle), uintptr(unsafe.Pointer(&chars[0])), uintptr(len(chars)), 0, uintptr(unsafe.Pointer(&count)))
		if ok == 0 {
			t.Fatal(err)
		}
		if got := string(utf16.Decode(chars[:count])); got != "中文red" {
			t.Fatalf("screen output %q, want Chinese and ANSI text", got)
		}
	})
	t.Run("resize while input idle", func(t *testing.T) {
		var info windows.ConsoleScreenBufferInfo
		if err := windows.GetConsoleScreenBufferInfo(outHandle, &info); err != nil {
			t.Fatal(err)
		}
		rect := info.Window
		rect.Right -= 5
		proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleWindowInfo")
		ok, _, err := proc.Call(uintptr(outHandle), 1, uintptr(unsafe.Pointer(&rect)))
		if ok == 0 {
			t.Fatal(err)
		}
		select {
		case <-con.resized:
			size, err := con.size()
			if err != nil || size.Cols != uint16(rect.Right-rect.Left+1) || size.Rows != uint16(rect.Bottom-rect.Top+1) {
				t.Fatalf("resize size %+v: %v", size, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no idle resize notification")
		}
	})
	t.Run("close unblocks idle read", func(t *testing.T) {
		done := make(chan error, 1)
		go func() { _, err := con.input.Read(make([]byte, 1)); done <- err }()
		if err := con.input.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if !errors.Is(err, os.ErrClosed) {
				t.Fatalf("closed read: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Close did not wake Read")
		}
		if err := con.restore(); err != nil {
			t.Fatal(err)
		}
		if err := con.restore(); err != nil {
			t.Fatal(err)
		}
		checkRestored()
	})
	t.Run("private input mode restoration", func(t *testing.T) {
		// Also restore the shared fixture when this subtest is selected alone.
		if err := con.restore(); err != nil {
			t.Fatal(err)
		}
		changed, err := prepareConsole(in, out)
		if err != nil {
			t.Fatal(err)
		}
		defer changed.restore()
		// ConPTY enables this private input mode in the local terminal. It
		// must not leak through teardown into a subsequent console reader.
		if _, err := changed.output.Write([]byte("\x1b[?9001h")); err != nil {
			t.Fatal(err)
		}
		if err := changed.restore(); err != nil {
			t.Fatal(err)
		}
		checkRestored()
		con, err := prepareConsole(in, out)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := con.restore(); err != nil {
				t.Error(err)
			}
			checkRestored()
		}()
		got := make(chan byte, 1)
		go func() {
			var b [1]byte
			_, _ = io.ReadFull(con.input, b[:])
			got <- b[0]
		}()
		for range 10 {
			injectConsoleRecords(t, inHandle, []consoleKeyRecord{{EventType: 1, KeyDown: 1, Repeat: 1, VirtualKey: 'Z', Scan: 44, Char: 'z'}, {EventType: 1, Repeat: 1, VirtualKey: 'Z', Scan: 44, Char: 'z'}})
			select {
			case b := <-got:
				if b != 'z' {
					t.Fatalf("Win32 input mode leaked into restored console: %q", b)
				}
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
		t.Fatal("restored console not reading native keys")
	})
	t.Run("repeated immediate close", func(t *testing.T) {
		for range 20 {
			con, err := prepareConsole(in, out)
			if err != nil {
				t.Fatal(err)
			}
			if err := con.restore(); err != nil {
				t.Fatal(err)
			}
			checkRestored()
		}
	})
	t.Run("setup failure keeps input intact", func(t *testing.T) {
		file, err := os.CreateTemp(t.TempDir(), "not-a-console")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if _, err := prepareConsole(in, file); err == nil {
			t.Fatal("accepted non-console output")
		}
		checkRestored()
	})
}

type consoleKeyRecord struct {
	EventType  uint16
	Padding    uint16
	KeyDown    int32
	Repeat     uint16
	VirtualKey uint16
	Scan       uint16
	Char       uint16
	Control    uint32
}

func injectConsoleText(t *testing.T, handle windows.Handle, text string) {
	t.Helper()
	chars := utf16.Encode([]rune(text))
	records := make([]consoleKeyRecord, len(chars))
	for i, c := range chars {
		records[i] = consoleKeyRecord{EventType: 1, KeyDown: 1, Repeat: 1, Char: c}
		if c == 0 {
			records[i].VirtualKey = 0x20
			records[i].Control = 0x8 // LEFT_CTRL_PRESSED: Ctrl+Space sends NUL.
		}
		if c == 3 {
			records[i].VirtualKey = 'C'
			records[i].Control = 0x8
		}
	}
	injectConsoleRecords(t, handle, records)
}

func injectConsoleRecords(t *testing.T, handle windows.Handle, records []consoleKeyRecord) {
	t.Helper()
	var written uint32
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("WriteConsoleInputW")
	ok, _, err := proc.Call(uintptr(handle), uintptr(unsafe.Pointer(&records[0])), uintptr(len(records)), uintptr(unsafe.Pointer(&written)))
	if ok == 0 || written != uint32(len(records)) {
		t.Fatalf("write console input: %d records, %v", written, err)
	}
}
