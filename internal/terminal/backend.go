// Package terminal provides transient interactive terminals. It does not own
// tmux sessions, persist input, or reconnect/replay a terminal stream.
package terminal

import (
	"errors"
	"io"

	"relaydock/internal/protocol"
)

var ErrUnsupported = errors.New("interactive terminals are implemented only on Linux/macOS; Windows backend is pending")

// Process is the platform boundary for the future Windows ConPTY backend.
// Wait is called exactly once. Hangup must unblock Read/Write and hang up this
// terminal; Kill only targets its outer shell, never detached mux servers.
type Process interface {
	io.Reader
	io.Writer
	Resize(protocol.TerminalSize) error
	Hangup()
	Kill()
	Wait() protocol.TerminalExit
}
