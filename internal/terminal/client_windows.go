package terminal

import "os"

// TODO(Windows): raw console input, resize events, and reliable console restore.
func prepareConsole(in, out *os.File) (*console, error) { return nil, ErrUnsupported }
