package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbus/internal/bus"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	backend  = "backend@abhay"
	frontend = "frontend@teammate"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	b, err := bus.Open(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	seed, err := os.ReadFile("../../seed/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		b.UpsertAgent(backend, "abhay", ""),
		b.UpsertAgent(frontend, "teammate", ""),
		b.SeedContract("openapi.yaml", backend, string(seed)),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewServer(New(b))
	t.Cleanup(ts.Close)
	return ts
}

func do(t *testing.T, ts *httptest.Server, method, path, from string, body any, out any) int {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, ts.URL+path, &buf)
	if from != "" {
		req.Header.Set(fromHeader, from)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func TestNegotiationOverHTTP(t *testing.T) {
	ts := newServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	feed, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws?since=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer feed.CloseNow()

	if code := do(t, ts, "POST", "/api/threads/users-shape/propose", "", nil, nil); code != 401 {
		t.Fatalf("no identity: %d", code)
	}

	var res bus.Result
	code := do(t, ts, "POST", "/api/threads/users-shape/propose", backend, bus.ProposeReq{
		Title: "Shape of /users", Contract: "openapi.yaml", Blocking: true, Reason: "i18n",
		Edits: []bus.Edit{{Old: "        name:\n          type: string", New: "        display_name:\n          type: string"}},
	}, &res)
	if code != 200 || res.Thread.State != bus.StateProposed {
		t.Fatalf("propose: %d %+v", code, res.Thread)
	}

	// Backend blocks on a reply while frontend accepts.
	waited := make(chan []bus.Event)
	go func() {
		var out struct{ Events []bus.Event }
		do(t, ts, "GET", "/api/threads/users-shape/wait?timeout=5", backend, nil, &out)
		waited <- out.Events
	}()

	var inbox struct{ Events []bus.Event }
	do(t, ts, "GET", "/api/inbox?role=liaison", frontend, nil, &inbox)
	if len(inbox.Events) != 1 || inbox.Events[0].Type != bus.Propose {
		t.Fatalf("frontend inbox: %+v", inbox.Events)
	}

	var errBody map[string]string
	if code := do(t, ts, "POST", "/api/threads/users-shape/accept", backend, bus.AcceptReq{}, &errBody); code != 409 {
		t.Fatalf("self-accept: %d %v", code, errBody)
	}
	if code := do(t, ts, "POST", "/api/threads/users-shape/accept", frontend, bus.AcceptReq{Reason: "ok"}, &res); code != 200 {
		t.Fatalf("accept: %d", code)
	}

	select {
	case evs := <-waited:
		if len(evs) != 2 || evs[0].Type != bus.Accept || evs[1].Type != bus.Decided {
			t.Fatalf("wait returned %+v", evs)
		}
	case <-ctx.Done():
		t.Fatal("wait never returned")
	}

	var c bus.Contract
	do(t, ts, "GET", "/api/contracts/openapi.yaml", "", nil, &c)
	if c.Rev != 1 || !strings.Contains(c.Content, "display_name") {
		t.Fatalf("contract: rev %d", c.Rev)
	}

	var got []bus.EventType
	for len(got) < 3 {
		var e bus.Event
		if err := wsjson.Read(ctx, feed, &e); err != nil {
			t.Fatal(err)
		}
		got = append(got, e.Type)
	}
	if got[0] != bus.Propose || got[1] != bus.Accept || got[2] != bus.Decided {
		t.Fatalf("feed: %v", got)
	}
}
