package terminal

import (
	"errors"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
	"relaydock/internal/protocol"
)

type consoleResizeSignal struct{}

func (consoleResizeSignal) Signal()        {}
func (consoleResizeSignal) String() string { return "console resized" }

func prepareConsole(in, out *os.File) (_ *console, err error) {
	inHandle, outHandle := windows.Handle(in.Fd()), windows.Handle(out.Fd())
	var inputMode, outputMode uint32
	if err = windows.GetConsoleMode(inHandle, &inputMode); err != nil {
		return nil, err
	}
	if err = windows.GetConsoleMode(outHandle, &outputMode); err != nil {
		return nil, err
	}
	inputCP, err := windows.GetConsoleCP()
	if err != nil {
		return nil, err
	}
	outputCP, err := windows.GetConsoleOutputCP()
	if err != nil {
		return nil, err
	}
	restoreModes := func() error {
		return errors.Join(
			windows.SetConsoleMode(inHandle, inputMode),
			windows.SetConsoleMode(outHandle, outputMode),
			windows.SetConsoleCP(inputCP),
			windows.SetConsoleOutputCP(outputCP),
		)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, restoreModes())
		}
	}()
	// Disabling processed input keeps Ctrl+C in the byte stream instead of
	// delivering a local console control event. VT mode supplies escape
	// sequences for keys such as arrows, Home, and function keys.
	raw := inputMode &^ (windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT |
		windows.ENABLE_PROCESSED_INPUT | windows.ENABLE_QUICK_EDIT_MODE |
		windows.ENABLE_MOUSE_INPUT | windows.ENABLE_WINDOW_INPUT)
	raw |= windows.ENABLE_EXTENDED_FLAGS | windows.ENABLE_VIRTUAL_TERMINAL_INPUT
	if err = windows.SetConsoleMode(inHandle, raw); err != nil {
		return nil, err
	}
	if err = windows.SetConsoleMode(outHandle, outputMode|windows.ENABLE_PROCESSED_OUTPUT|
		windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.DISABLE_NEWLINE_AUTO_RETURN); err != nil {
		return nil, err
	}
	if err = windows.SetConsoleCP(65001); err != nil {
		return nil, err
	}
	if err = windows.SetConsoleOutputCP(65001); err != nil {
		return nil, err
	}
	reader, err := openConsoleInput(inHandle)
	if err != nil {
		return nil, err
	}
	size := func() (protocol.TerminalSize, error) {
		cols, rows, e := term.GetSize(int(out.Fd()))
		return protocol.TerminalSize{Cols: uint16(cols), Rows: uint16(rows)}, e
	}
	changes := make(chan os.Signal, 1)
	stop, done := make(chan struct{}), make(chan struct{})
	initialSize, _ := size()
	go func() {
		defer close(done)
		// ReadConsole discards window events. Polling the visible dimensions
		// also detects changes while an idle keyboard read is blocked.
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		last := initialSize
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				current, e := size()
				if e == nil && current.Validate() == nil && current != last {
					last = current
					select {
					case changes <- consoleResizeSignal{}:
					default:
					}
				}
			}
		}
	}()
	var once sync.Once
	var restoreErr error
	return &console{input: reader, output: out, size: size, resized: changes, restore: func() error {
		once.Do(func() {
			close(stop)
			reader.Close()
			<-done
			// ConPTY negotiates this private mode through output. Restoring
			// GetConsoleMode flags alone leaves future readers receiving Win32
			// key packets, so reset it while VT output processing is enabled.
			_, resetErr := out.Write([]byte("\x1b[?9001l"))
			restoreErr = errors.Join(resetErr, restoreModes())
		})
		return restoreErr
	}}, nil
}
