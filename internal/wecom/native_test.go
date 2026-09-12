package wecom

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// A separate local process exercises the actual JSON-lines client. It neither
// loads the Windows helper nor performs desktop actions.
func TestMain(m *testing.M) {
	if os.Getenv("RELAYDOCK_NATIVE_FIXTURE") == "1" {
		s := bufio.NewScanner(os.Stdin)
		enc := json.NewEncoder(os.Stdout)
		for s.Scan() {
			var r struct {
				Version int `json:"protocolVersion"`
				ID, Cmd string
				Args    json.RawMessage
			}
			if json.Unmarshal(s.Bytes(), &r) != nil || r.Version != 3 {
				os.Exit(2)
			}
			if r.Cmd == "stall" {
				time.Sleep(time.Hour)
			}
			id := r.ID
			if r.Cmd == "wrong-id" {
				id = "different"
			}
			result := any(r.Args)
			if r.Cmd == "diagnostics" {
				result = map[string]int{"protocolVersion": 3}
			}
			_ = enc.Encode(map[string]any{"protocolVersion": 3, "id": id, "ok": true, "result": result})
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func TestHelperProtocolAndInterruptedCall(t *testing.T) {
	t.Setenv("RELAYDOCK_NATIVE_FIXTURE", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"wrong-id", "stall"} {
		t.Run(command, func(t *testing.T) {
			h, err := StartHelper(context.Background(), exe)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			var got map[string]string
			if err = h.Call(context.Background(), "echo", map[string]string{"text": "中文\n多行"}, &got); err != nil || got["text"] != "中文\n多行" {
				t.Fatal(got, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if err = h.Call(ctx, command, map[string]any{}, &got); err == nil {
				t.Fatal("expected protocol/timeout failure")
			}
			if err = h.Call(context.Background(), "echo", map[string]any{}, &got); err == nil {
				t.Fatal("reused uncertain native connection")
			}
		})
	}
}
