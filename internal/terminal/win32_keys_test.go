package terminal

import (
	"fmt"
	"strings"
	"testing"
)

func TestEscapeWin32Records(t *testing.T) {
	key := func(char, down int) string { return fmt.Sprintf("\x1b[0;0;%d;%d;0;1_", char, down) }
	enter, tilde, dot := key(13, 1), key('~', 1), key('.', 1)
	release := key('~', 0)
	shift := "\x1b[16;42;0;0;0;1_"
	for _, tc := range []struct {
		name, input, output string
		quit                bool
	}{
		{"disconnect", enter + tilde + release + shift + dot, enter + strings.TrimSuffix(tilde, "_"), true},
		{"literal", enter + tilde + release + shift + tilde + key('~', 0) + key('x', 1), enter + tilde + release + shift + key('~', 0) + key('x', 1), false},
		{"non escape", tilde + release + key('x', 1), tilde + release + key('x', 1), false},
		{"middle of line", key('x', 1) + tilde + dot, key('x', 1) + tilde + dot, false},
		{"controls unchanged", key(3, 1) + key(9, 1) + key(0x4e2d, 1) + "\x1b[38;72;0;1;256;1_", key(3, 1) + key(9, 1) + key(0x4e2d, 1) + "\x1b[38;72;0;1;256;1_", false},
		{"optional fields", "\x1b[;;126;1_\x1b[;;126_\x1b[;;46;1_", "\x1b[;;126;1", true},
		{"modified tilde", "\x1b[192;41;126;1;2;1_" + dot, "\x1b[192;41;126;1;2;1_" + dot, false},
		{"repeated tilde", "\x1b[192;41;126;1;16;2_" + dot, "\x1b[192;41;126;1;16;2_" + dot, false},
		{"ordinary VT", "\x1b[A\x1b[1;5D\x1b[200~abc\x1b[201~", "\x1b[A\x1b[1;5D\x1b[200~abc\x1b[201~", false},
		{"malformed following tilde", tilde + "\x1b[1;2;99999999;1;0;1_", tilde + "\x1b[1;2;99999999;1;0;1_", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for chunk := 1; chunk <= len(tc.input); chunk++ {
				f := escapeFilter{lineStart: true}
				var got []byte
				var quit bool
				for remaining := []byte(tc.input); len(remaining) != 0 && !quit; {
					n := min(chunk, len(remaining))
					var output []byte
					output, quit = f.feed(remaining[:n])
					got = append(got, output...)
					remaining = remaining[n:]
				}
				if string(got) != tc.output || quit != tc.quit {
					t.Fatalf("chunk %d: got %q quit %v, want %q quit %v", chunk, got, quit, tc.output, tc.quit)
				}
			}
		})
	}
}

func TestEscapeWin32BoundedAndImmediate(t *testing.T) {
	f := escapeFilter{lineStart: true}
	if got, quit := f.feed([]byte("\x1b")); string(got) != "\x1b" || quit {
		t.Fatalf("standalone Escape held: %q %v", got, quit)
	}
	for _, input := range []string{
		"\x1b[" + strings.Repeat("1", 10000) + "_",
		"\x1b[;;126;1_" + strings.Repeat("\x1b[16;42;0;0;0;1_", 1000) + "\x1b[;;120;1_",
	} {
		f := escapeFilter{lineStart: true}
		var got []byte
		for _, b := range []byte(input) {
			out, quit := f.feed([]byte{b})
			if quit {
				t.Fatal("unexpected disconnect")
			}
			got = append(got, out...)
			if len(f.pendingBytes) > 4096 || len(f.sequence) >= 96 {
				t.Fatal("unbounded escape buffer")
			}
		}
		if string(got) != input {
			t.Fatal("bounded parser changed keyboard bytes")
		}
	}
}
