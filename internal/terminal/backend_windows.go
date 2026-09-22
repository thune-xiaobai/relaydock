package terminal

import "relaydock/internal/protocol"

// TODO(Windows): implement Process using ConPTY. Keep detached psmux servers
// outside terminal teardown; do not reuse the batch shell's kill-tree policy.
func Supported() bool                { return false }
func SupportsShell(kind string) bool { return false }
func Start(executable, cwd, term string, size protocol.TerminalSize) (Process, error) {
	return nil, ErrUnsupported
}
