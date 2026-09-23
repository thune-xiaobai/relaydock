package terminal

import (
	"errors"
	"runtime"
	"testing"

	"relaydock/internal/protocol"
)

func TestRemoteExitStatus(t *testing.T) {
	for _, code := range []int64{7, 255, 256, 3010, 0xc000013a, 1<<32 - 1} {
		err := exitResult(protocol.Wrap("shell_exit", "", protocol.TerminalExit{Code: code}))
		var exit *ExitError
		if !errors.As(err, &exit) || exit.Code != code {
			t.Fatalf("remote status %d lost: %v", code, err)
		}
		want := int(code & 255)
		if want == 0 {
			want = 1
		}
		if runtime.GOOS == "windows" {
			want = int(uint32(code))
		}
		if got := exit.ExitStatus(); got != want {
			t.Errorf("remote status %d: local status %d, want %d", code, got, want)
		}
	}
	if err := exitResult(protocol.Wrap("shell_exit", "", protocol.TerminalExit{})); err != nil {
		t.Fatal(err)
	}
}

func TestEscapeAcrossReadBoundaries(t *testing.T) {
	f := escapeFilter{lineStart: true}
	for _, tc := range []struct {
		in, out string
		quit    bool
	}{
		{"~", "", false}, {"~pwd\r", "~pwd\r", false}, {"~", "", false}, {"x\n", "~x\n", false},
		{"echo ~/.config\r", "echo ~/.config\r", false}, {"~", "", false}, {".", "", true},
	} {
		got, quit := f.feed([]byte(tc.in))
		if string(got) != tc.out || quit != tc.quit {
			t.Fatalf("%q => %q %v", tc.in, got, quit)
		}
	}
}
