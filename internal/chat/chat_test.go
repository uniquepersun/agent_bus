package chat

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentbus/internal/bus"
	"agentbus/internal/client"
	"agentbus/internal/server"
)

const (
	backend  = "backend@abhay"
	frontend = "frontend@teammate"
)

// TestMain doubles as a fake agy stream-json session when asked to.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_AGY") == "1" {
		fakeAgy()
		return
	}
	os.Exit(m.Run())
}

func fakeAgy() {
	out := json.NewEncoder(os.Stdout)
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var in struct {
			Event   string
			Message struct{ Content string }
		}
		json.Unmarshal(sc.Bytes(), &in)
		step := func(u map[string]any) { out.Encode(map[string]any{"event": "step_update", "step_update": u}) }
		step(map[string]any{"state": "DONE", "step_type": "user_input"})
		step(map[string]any{"state": "ACTIVE", "step_type": "tool", "tool_name": "read_file",
			"tool_info": map[string]any{"name": "read_file", "parameters": map[string]any{"Path": "/repo/src/UserCard.tsx"}}})
		step(map[string]any{"state": "ACTIVE", "step_type": "agent_response", "text_delta": "echo: " + in.Message.Content})
		step(map[string]any{"state": "DONE", "step_type": "agent_response", "text_delta": "\n"})
		out.Encode(map[string]any{"event": "result", "result": map[string]any{"status": "SUCCESS"}})
	}
}

// syncBuffer is a concurrency-safe output sink.
type syncBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

func TestChat(t *testing.T) {
	b, err := bus.Open(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.UpsertAgent(backend, "abhay", "")
	b.UpsertAgent(frontend, "teammate", "")
	b.SeedContract("openapi.yaml", backend, "openapi: 3.0.3\n")
	ts := httptest.NewServer(server.New(b))
	defer ts.Close()

	self, _ := os.Executable()
	t.Setenv("FAKE_AGY", "1")
	in, typeLine := io.Pipe()
	out := &syncBuffer{}
	ch := &Chat{Client: client.New(ts.URL, frontend), Human: "human:teammate", Argv: []string{self},
		Dir: t.TempDir(), In: in, Out: out}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error)
	go func() { done <- ch.Run(ctx) }()

	waitFor := func(want string) {
		t.Helper()
		for !strings.Contains(out.String(), want) {
			if ctx.Err() != nil {
				t.Fatalf("never saw %q in:\n%s", want, out.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	say := func(line string) { fmt.Fprintln(typeLine, line) }

	waitFor("Agent Bus chat as frontend@teammate")
	say("add an email label to UserCard")
	waitFor("agent> echo: add an email label to UserCard")
	waitFor("[tool] read_file /repo/src/UserCard.tsx")

	time.Sleep(200 * time.Millisecond) // let the feed connect
	// The frontend liaison asks its human; the question appears inline.
	res, err := client.New(ts.URL, frontend).Act(ctx, "users-shape", "ask",
		bus.AskReq{Title: "Shape of /users", Question: "Rename name to display_name?", To: "human", Blocking: true})
	if err != nil {
		t.Fatal(err)
	}
	ask := res.Events[0].Seq
	waitFor(fmt.Sprintf("QUESTION FOR YOU (#%d) from frontend@teammate", ask))
	waitFor(fmt.Sprintf("/answer %d", ask))

	say(fmt.Sprintf("/answer %d fine, but keep name for one release", ask))
	waitFor("Answer sent")
	_, evs, err := client.New(ts.URL, frontend).Thread(ctx, "users-shape")
	if err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1]
	if last.Type != bus.Answer || last.From != "human:teammate" || last.ReplyTo != ask {
		t.Fatalf("answer not recorded: %+v", last)
	}

	// A decision arriving while the agent is idle starts a turn for it.
	if _, err := client.New(ts.URL, frontend).Act(ctx, "avatar", "propose", bus.ProposeReq{Title: "Avatar",
		Contract: "openapi.yaml", Edits: []bus.Edit{{Old: "", New: "# avatar\n"}}, Reason: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.New(ts.URL, backend).Act(ctx, "avatar", "accept", bus.AcceptReq{}); err != nil {
		t.Fatal(err)
	}
	waitFor("asking it to apply the update")
	waitFor("agent> echo: [agent-bus] Contract update")

	say("/threads")
	waitFor("users-shape")
	say("/quit")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("chat did not quit")
	}
}
