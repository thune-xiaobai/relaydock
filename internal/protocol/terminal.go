package protocol

import (
	"errors"
	"regexp"
	"strings"
)

const TerminalChunk = 16 << 10

type TerminalSize struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

func (s TerminalSize) Validate() error {
	if s.Cols < 1 || s.Rows < 1 || s.Cols > 1000 || s.Rows > 1000 {
		return errors.New("terminal size must be 1..1000 columns/rows")
	}
	return nil
}

// PeerID is checked against the authenticated Channel. The Hub generates the
// ephemeral stream ID and delivers this request to exactly one Worker.
type TerminalOpen struct {
	PeerID string `json:"peer_id"`
	Node   string `json:"node"`
	CWD    string `json:"cwd"`
	Term   string `json:"term"`
	TerminalSize
}

var termName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]{0,63}$`)

func (o TerminalOpen) Validate() error {
	if !ValidID(o.PeerID) || !ValidID(o.Node) || strings.TrimSpace(o.CWD) == "" || len(o.CWD) > MaxText || strings.ContainsRune(o.CWD, 0) || !termName.MatchString(o.Term) {
		return errors.New("invalid terminal peer, node, cwd or TERM")
	}
	return o.TerminalSize.Validate()
}

// JSON encodes bytes as base64: ANSI and split/non-UTF8 sequences stay intact.
type TerminalData struct {
	Bytes []byte `json:"bytes"`
}
type TerminalExit struct {
	Code  int64  `json:"code"` // -1 for transport/startup errors, otherwise an OS exit status (Windows DWORD).
	Error string `json:"error,omitempty"`
}

func TerminalInput(m Message) error {
	if m.ID != "" {
		return errors.New("terminal stream frames must not have IDs")
	}
	switch m.Type {
	case "shell_input":
		b, e := Decode[TerminalData](m)
		if e != nil || len(b.Bytes) == 0 || len(b.Bytes) > TerminalChunk {
			return errors.New("invalid terminal input")
		}
		return nil
	case "shell_resize":
		s, e := Decode[TerminalSize](m)
		if e != nil {
			return e
		}
		return s.Validate()
	}
	return errors.New("unexpected terminal input frame")
}

func TerminalOutput(m Message) error {
	if m.ID != "" {
		return errors.New("terminal stream frames must not have IDs")
	}
	switch m.Type {
	case "shell_output":
		b, e := Decode[TerminalData](m)
		if e != nil || len(b.Bytes) == 0 || len(b.Bytes) > TerminalChunk {
			return errors.New("invalid terminal output")
		}
		return nil
	case "shell_ready":
		if m.Version == Version {
			return nil
		}
	case "shell_exit":
		e, err := Decode[TerminalExit](m)
		if err == nil && e.Code >= -1 && e.Code <= 1<<32-1 && len(e.Error) <= MaxText {
			return nil
		}
	}
	return errors.New("unexpected terminal output frame")
}
