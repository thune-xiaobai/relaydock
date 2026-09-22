package terminal

import "testing"

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
