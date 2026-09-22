//go:build !linux && !darwin && !windows

package terminal

import "relaydock/internal/protocol"

func Supported() bool                { return false }
func SupportsShell(kind string) bool { return false }
func Start(executable, cwd, term string, size protocol.TerminalSize) (Process, error) {
	return nil, ErrUnsupported
}
