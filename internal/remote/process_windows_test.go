package remote

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"relaydock/internal/config"
)

// Run these on the actual Windows account/policy; cross-compilation alone does
// not establish PowerShell or Job Object compatibility in that environment.
func TestPowerShellNativeExitAndTerminatingError(t *testing.T) {
	m, _, _ := testManager(t, config.Shell{})
	for _, tc := range []struct {
		id, cmd string
		code    int
	}{
		{"native_exit", "cmd.exe /d /c exit 9", 9},
		{"ps_error", "throw 'expected failure'", 1},
	} {
		startJob(t, m, tc.id, tc.cmd, 5000)
		s := waitJob(t, m, tc.id)
		if s.Job.Status != "failed" || s.Job.ExitCode == nil || *s.Job.ExitCode != tc.code {
			t.Fatal(s)
		}
	}
}

func TestWindowsJobCancelsChild(t *testing.T) {
	m, _, c := testManager(t, config.Shell{})
	child := filepath.Join(c.Workspaces["project"], "child.ps1")
	if err := os.WriteFile(child, []byte("\ufeffStart-Sleep -Seconds 2\n[IO.File]::WriteAllText((Join-Path (Get-Location).Path 'leaked'), 'child survived')\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := `Start-Process -FilePath (Get-Process -Id $PID).Path -WorkingDirectory (Get-Location).Path -ArgumentList @('-NoProfile', '-File', 'child.ps1'); [Console]::Out.Write('ready'); Start-Sleep -Seconds 30`
	startJob(t, m, "tree", cmd, 10000)
	until := time.Now().Add(5 * time.Second)
	for {
		s, err := m.Status("tree", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		if s.Stdout == "ready" {
			break
		}
		if time.Now().After(until) {
			t.Fatal("child not launched", s)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := m.Cancel("tree"); err != nil {
		t.Fatal(err)
	}
	if s := waitJob(t, m, "tree"); s.Job.Status != "cancelled" {
		t.Fatal(s)
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(c.Workspaces["project"], "leaked")); !os.IsNotExist(err) {
		t.Fatal("Windows child survived cancellation")
	}
}
