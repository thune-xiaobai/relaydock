//go:build !windows

package remote

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"relaydock/internal/config"
)

func TestCancelKillsOrdinaryDescendants(t *testing.T) {
	m, _, c := testManager(t, config.Shell{})
	startJob(t, m, "tree", "(sleep 1; printf leaked > leaked) & printf ready; wait", 5000)
	until := time.Now().Add(3 * time.Second)
	for {
		s, err := m.Status("tree", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		if s.Stdout == "ready" {
			break
		}
		if time.Now().After(until) {
			t.Fatal("child not launched")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := m.Cancel("tree"); err != nil {
		t.Fatal(err)
	}
	if s := waitJob(t, m, "tree"); s.Job.Status != "cancelled" {
		t.Fatal(s)
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(c.Workspaces["project"], "leaked")); !os.IsNotExist(err) {
		t.Fatal("descendant survived cancellation")
	}
}
