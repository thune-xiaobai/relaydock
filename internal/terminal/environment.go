package terminal

import (
	"runtime"
	"strings"
)

// A new PTY is independent of the Worker's own terminal. Carrying the parent's
// mux identity into it both triggers nesting guards and routes untargeted mux
// commands to the wrong session. Keep configuration, including user-selected
// nesting policy, and only discard terminal/session context in the child copy.
func shellEnvironment(parent []string, term string) []string {
	env := make([]string, 0, len(parent)+1)
	for _, entry := range parent {
		name, _, _ := strings.Cut(entry, "=")
		if runtime.GOOS == "windows" {
			name = strings.ToUpper(name)
		}
		switch name {
		case "TERM", "TMUX", "TMUX_PANE", "PSMUX_SESSION", "PSMUX_ACTIVE",
			"PSMUX_SESSION_NAME", "PSMUX_REMOTE_ATTACH", "PSMUX_TARGET_SESSION", "PSMUX_TARGET_FULL":
			continue
		}
		env = append(env, entry)
	}
	return append(env, "TERM="+term)
}
