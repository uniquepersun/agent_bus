package daemon

import (
	"context"
	"io"
	"log"
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

type runnerFunc func(ctx context.Context, prompt string) error

func (f runnerFunc) Run(ctx context.Context, prompt string) error { return f(ctx, prompt) }

// scripted builds a fake liaison: it reads its inbox, calls react for each
// event, and acks, like a well-behaved LLM liaison would.
func scripted(t *testing.T, c *client.Client, react func(ctx context.Context, e bus.Event) error) Runner {
	return runnerFunc(func(ctx context.Context, prompt string) error {
		inbox, err := c.Inbox(ctx, bus.RoleLiaison)
		if err != nil || len(inbox) == 0 {
			return err
		}
		for _, e := range inbox {
			if !strings.Contains(prompt, client.FormatEvent(e)) {
				t.Errorf("prompt is missing %s", client.FormatEvent(e))
			}
			if err := react(ctx, e); err != nil {
				return err
			}
		}
		return c.Ack(ctx, bus.RoleLiaison, inbox[len(inbox)-1].Seq)
	})
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The demo trace with both laptops' daemons, scripted liaisons, and a human
// answering through the API.
func TestDaemonsRunDemoTrace(t *testing.T) {
	b, err := bus.Open(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	seed, _ := os.ReadFile("../../seed/openapi.yaml")
	b.UpsertAgent(backend, "abhay", "")
	b.UpsertAgent(frontend, "teammate", "")
	b.SeedContract("openapi.yaml", backend, string(seed))
	ts := httptest.NewServer(server.New(b))
	defer ts.Close()

	nameField := "        name:\n          type: string"
	required := bus.Edit{Old: "required: [id, name, email]", New: "required: [id, display_name, email]"}

	be, fe := client.New(ts.URL, backend), client.New(ts.URL, frontend)
	backendLiaison := scripted(t, be, func(ctx context.Context, e bus.Event) error {
		if e.Type == bus.Counter {
			_, err := be.Act(ctx, e.Thread, "accept", bus.AcceptReq{Reason: "additive, within mandate"})
			return err
		}
		return nil
	})
	frontendLiaison := scripted(t, fe, func(ctx context.Context, e bus.Event) error {
		switch e.Type {
		case bus.Propose: // renames are "ask me"
			_, err := fe.Act(ctx, e.Thread, "ask", bus.AskReq{Question: "OK to rename name?", To: "human", Blocking: true})
			return err
		case bus.Answer:
			_, err := fe.Act(ctx, e.Thread, "counter", bus.CounterReq{BaseRev: 0, Reason: "keep name for one release",
				Edits: []bus.Edit{{Old: nameField, New: "        display_name:\n          type: string\n" + nameField + "\n          deprecated: true"}, required}})
			return err
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var notesMu sync.Mutex
	var notes []string // what the frontend human's game was told
	repos := map[string]string{}
	for id, r := range map[string]Runner{backend: backendLiaison, frontend: frontendLiaison} {
		repos[id] = t.TempDir()
		d := &Daemon{Client: client.New(ts.URL, id), Repo: repos[id], Liaison: r, Log: log.New(io.Discard, "", 0)}
		if id == frontend {
			d.Notify = func(kind, _ string) { notesMu.Lock(); notes = append(notes, kind); notesMu.Unlock() }
		}
		go d.Run(ctx)
	}
	// Both daemons render the seed contract on start.
	for id, repo := range repos {
		eventually(t, id+" initial render", func() bool {
			_, err := os.Stat(filepath.Join(repo, "contracts", "openapi.yaml"))
			return err == nil
		})
	}

	// The backend worker proposes the rename.
	if _, err := be.Act(ctx, "users-shape", "propose", bus.ProposeReq{Title: "Shape of /users response",
		Contract: "openapi.yaml", Reason: "DB column renamed for i18n", Blocking: true,
		Edits: []bus.Edit{{Old: nameField, New: "        display_name:\n          type: string"}, required}}); err != nil {
		t.Fatal(err)
	}

	var askSeq int64
	eventually(t, "frontend liaison to ask its human", func() bool {
		_, evs, _ := be.Thread(ctx, "users-shape")
		for _, e := range evs {
			if e.Type == bus.Ask && e.To == "human:teammate" {
				askSeq = e.Seq
			}
		}
		return askSeq != 0
	})
	if _, err := client.New(ts.URL, "human:teammate").Act(ctx, "users-shape", "answer",
		bus.AnswerReq{ReplyTo: askSeq, Answer: "fine, but keep name for one release"}); err != nil {
		t.Fatal(err)
	}

	eventually(t, "thread to be decided", func() bool {
		th, _, _ := be.Thread(ctx, "users-shape")
		return th.State == bus.StateDecided
	})
	eventually(t, "the game to be told about the question and the decision", func() bool {
		notesMu.Lock()
		defer notesMu.Unlock()
		got := strings.Join(notes, ",")
		return strings.Contains(got, "working") && strings.Contains(got, "attention") && strings.Contains(got, "done")
	})
	for id, repo := range repos {
		path := filepath.Join(repo, "contracts", "openapi.yaml")
		eventually(t, id+" to receive the new contract", func() bool {
			got, _ := os.ReadFile(path)
			return strings.Contains(string(got), "deprecated: true")
		})
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o444 {
			t.Errorf("%s mode %v, want read-only", path, fi.Mode().Perm())
		}
		eventually(t, id+" DECISIONS.md", func() bool {
			got, _ := os.ReadFile(filepath.Join(repo, "DECISIONS.md"))
			return strings.Contains(string(got), "### Shape of /users response") && strings.Contains(string(got), "+          deprecated: true")
		})
	}
}

// A liaison that never acks is retried once, then not restarted until
// something new arrives.
func TestUnackedLiaisonDoesNotLoop(t *testing.T) {
	b, _ := bus.Open(filepath.Join(t.TempDir(), "bus.db"))
	defer b.Close()
	b.UpsertAgent(backend, "abhay", "")
	b.UpsertAgent(frontend, "teammate", "")
	ts := httptest.NewServer(server.New(b))
	defer ts.Close()

	runs := make(chan string, 10)
	d := &Daemon{Client: client.New(ts.URL, frontend), Repo: t.TempDir(), Log: log.New(io.Discard, "", 0),
		Liaison: runnerFunc(func(_ context.Context, prompt string) error { runs <- prompt; return nil })}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	ask := func(thread string) {
		if _, err := client.New(ts.URL, backend).Act(ctx, thread, "ask", bus.AskReq{Title: thread, Question: "?", To: "peer"}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond) // let the daemon connect
	ask("first-question")
	for i := 0; i < LiaisonAttempts; i++ { // the first run plus its retry
		select {
		case <-runs:
		case <-time.After(5 * time.Second):
			t.Fatalf("liaison attempt %d never started", i+1)
		}
	}
	select {
	case p := <-runs:
		t.Fatalf("liaison restarted beyond its retry with nothing new:\n%s", p)
	case <-time.After(300 * time.Millisecond):
	}
	ask("second-question")
	select {
	case p := <-runs:
		if !strings.Contains(p, "first-question") || !strings.Contains(p, "second-question") {
			t.Fatalf("second run should see both unacked messages:\n%s", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("liaison not restarted for new message")
	}
}
