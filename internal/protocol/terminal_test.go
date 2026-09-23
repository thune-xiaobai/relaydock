package protocol

import "testing"

func TestTerminalExitStatusRange(t *testing.T) {
	for _, code := range []int64{-1, 0, 7, 255, 256, 3010, 0xc000013a, 1<<32 - 1} {
		m := Wrap("shell_exit", "", TerminalExit{Code: code})
		if err := TerminalOutput(m); err != nil {
			t.Errorf("valid exit status %d rejected: %v", code, err)
		}
		got, err := Decode[TerminalExit](m)
		if err != nil || got.Code != code {
			t.Errorf("exit status %d lost in transit: %+v, %v", code, got, err)
		}
	}
	for _, code := range []int64{-2, 1 << 32, 1<<63 - 1} {
		if err := TerminalOutput(Wrap("shell_exit", "", TerminalExit{Code: code})); err == nil {
			t.Errorf("invalid exit status %d accepted", code)
		}
	}
}
