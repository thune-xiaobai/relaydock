package config

import "testing"

func TestTLSBoundary(t *testing.T) {
	for _, url := range []string{"ws://10.1.2.3/ws", "http://localhost/ws", "ws://example.com/ws"} {
		if _, e := ClientTLS(Config{Hub: url}); e == nil {
			t.Errorf("accepted %s", url)
		}
	}
	for _, url := range []string{"ws://127.0.0.1:7331/ws", "wss://internal.example/ws"} {
		if _, e := ClientTLS(Config{Hub: url}); e != nil {
			t.Fatal(e)
		}
	}
	if e := (Config{Listen: "0.0.0.0:7331"}).CheckHub(); e == nil {
		t.Fatal("accepted LAN plaintext")
	}
}
