//go:build !linux && !darwin && !windows

package terminal

import "os"

func prepareConsole(in, out *os.File) (*console, error) { return nil, ErrUnsupported }
