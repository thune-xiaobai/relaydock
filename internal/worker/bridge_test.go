package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestRealPiBridgeReceipts(t *testing.T) {
	if os.Getenv("RELAYDOCK_TEST_PI") != "1" {
		t.Skip("set RELAYDOCK_TEST_PI=1 to test the installed pi SDK")
	}
	pi, err := exec.LookPath("pi")
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--test", filepath.Join(root, "extensions", "relaydock.test.mjs"))
	cmd.Env = append(os.Environ(), "RELAYDOCK_PI_ANCHOR="+pi)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("real SDK bridge: %v\n%s", err, b)
	} else {
		t.Log(string(b))
	}
}
