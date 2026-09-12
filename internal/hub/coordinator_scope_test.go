package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"relaydock/internal/config"
)

func TestRealPiCoordinatorHistoryScope(t *testing.T) {
	if os.Getenv("RELAYDOCK_TEST_PI") != "1" {
		t.Skip("set RELAYDOCK_TEST_PI=1 to test installed pi history")
	}
	observed := make(chan string, 50)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []fixtureMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		b, _ := json.Marshal(request)
		observed <- string(b)
		coordinatorFixture(w, request.Messages)
	}))
	defer model.Close()
	p := PiCoordinator{Config: config.Config{StateDir: t.TempDir(), Model: config.Model{URL: model.URL + "/v1", Model: "coordinator"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, tc := range []struct {
		owner, input, data string
		nodes              []string
		wantPrivate        bool
	}{
		{"u", "first", "PRIVATE_NODE_N", []string{"n", "m"}, true},
		{"u", "same authority", "public", []string{"m", "n"}, true},
		{"u", "after revocation", "public", []string{"m"}, false},
		{"other", "other channel", "public", []string{"n", "m"}, false},
	} {
		if _, err := p.Run(ctx, Turn{Owner: tc.owner, Input: tc.input, Context: map[string]any{}, Nodes: tc.nodes}, func(context.Context, string, json.RawMessage) ToolReply {
			return ToolReply{OK: true, Data: map[string]string{"result": tc.data}}
		}); err != nil {
			t.Fatal(err)
		}
		hasPrivate, requests := false, 0
		for len(observed) > 0 {
			requests++
			if strings.Contains(<-observed, "PRIVATE_NODE_N") {
				hasPrivate = true
			}
		}
		if requests == 0 || hasPrivate != tc.wantPrivate {
			t.Fatalf("%s: requests=%d private=%v", tc.input, requests, hasPrivate)
		}
	}
}
