//go:build linux || darwin

package terminal

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
	"relaydock/internal/protocol"
)

func prepareConsole(in, out *os.File) (*console, error) {
	inFD, outFD := int(in.Fd()), int(out.Fd())
	// A separate open file description avoids setting O_NONBLOCK on stdout
	// when the parent's stdin/stdout were both dup'ed from the same tty.
	reader, err := openConsoleInput()
	if err != nil {
		return nil, err
	}
	old, err := term.MakeRaw(inFD)
	if err != nil {
		reader.Close()
		return nil, err
	}
	changes := make(chan os.Signal, 1)
	signal.Notify(changes, syscall.SIGWINCH)
	return &console{input: reader, output: out, resized: changes, size: func() (protocol.TerminalSize, error) {
		cols, rows, err := term.GetSize(outFD)
		return protocol.TerminalSize{Cols: uint16(cols), Rows: uint16(rows)}, err
	}, restore: func() error {
		signal.Stop(changes)
		return term.Restore(inFD, old)
	}}, nil
}
