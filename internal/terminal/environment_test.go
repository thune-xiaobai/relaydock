package terminal

import (
	"runtime"
	"slices"
	"testing"
)

func TestShellEnvironmentSeparatesMuxContextFromConfiguration(t *testing.T) {
	keep := []string{
		"PATH=tools", "HOME=home", "USERPROFILE=profile", "=C:=C:\\workspace",
		"APP_VALUE=中文🙂=value", "PSMUX_CONFIG_FILE=custom.conf",
		"PSMUX_ALLOW_NESTING=1", "PSMUX_NO_WARM=1", "PSMUX_CUSTOM_SETTING=keep",
	}
	parent := append(slices.Clone(keep),
		"TERM=old", "TERM=duplicate", "TMUX=/tmp/parent,1234,0", "TMUX_PANE=%3",
		"PSMUX_SESSION=parent", "PSMUX_ACTIVE=1", "PSMUX_SESSION_NAME=parent",
		"PSMUX_REMOTE_ATTACH=1", "PSMUX_TARGET_SESSION=parent", "PSMUX_TARGET_FULL=parent:0.3",
	)
	before := slices.Clone(parent)
	got := shellEnvironment(parent, "xterm-256color")
	want := append(slices.Clone(keep), "TERM=xterm-256color")
	if !slices.Equal(got, want) {
		t.Fatalf("child environment: got %q, want %q", got, want)
	}
	if !slices.Equal(parent, before) {
		t.Fatal("changed parent environment")
	}
	got[0] = "PATH=changed-child"
	if !slices.Equal(parent, before) {
		t.Fatal("child environment aliases parent storage")
	}
}

func TestShellEnvironmentPlatformCaseRules(t *testing.T) {
	parent := []string{"Path=tools", "tErM=old", "tMuX=parent", "pSmUx_SeSsIoN=parent", "pSmUx_AcTiVe=1", "pSmUx_CoNfIg_FiLe=custom.conf"}
	want := append(slices.Clone(parent), "TERM=vt100")
	if runtime.GOOS == "windows" {
		want = []string{"Path=tools", "pSmUx_CoNfIg_FiLe=custom.conf", "TERM=vt100"}
	}
	if got := shellEnvironment(parent, "vt100"); !slices.Equal(got, want) {
		t.Fatalf("case handling on %s: got %q, want %q", runtime.GOOS, got, want)
	}
}
