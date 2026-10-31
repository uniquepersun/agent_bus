package bus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	backend  = "backend@abhay"
	frontend = "frontend@teammate"
)

// The two edits that turn User.name into display_name in seed/openapi.yaml.
var (
	nameField   = "        name:\n          type: string"
	requiredOld = "required: [id, name, email]"
	requiredNew = "required: [id, display_name, email]"
	renameEdits = []Edit{
		{Old: nameField, New: "        display_name:\n          type: string"},
		{Old: requiredOld, New: requiredNew},
	}
	keepDeprecatedEdits = []Edit{
		{Old: nameField, New: "        display_name:\n          type: string\n" +
			"        name:\n          type: string\n          deprecated: true"},
		{Old: requiredOld, New: requiredNew},
	}
)

func newBus(t *testing.T) *Bus {
	t.Helper()
	b, err := Open(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	must(t, b.UpsertAgent(backend, "abhay", "Auto-accept: additive fields"))
	must(t, b.UpsertAgent(frontend, "teammate", "Ask me: renames"))
	seed, err := os.ReadFile("../../seed/openapi.yaml")
	must(t, err)
	must(t, b.SeedContract("openapi.yaml", backend, string(seed)))
	return b
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantStatus(t *testing.T, err error, status int) {
	t.Helper()
	var be *Error
	if !errors.As(err, &be) || be.Status != status {
		t.Fatalf("want status %d, got %v", status, err)
	}
}

func types(evs []Event) string {
	var s []string
	for _, e := range evs {
		s = append(s, string(e.Type))
	}
	return strings.Join(s, ",")
}

// The demo trace from SPEC.md, end to end.
func TestDemoTrace(t *testing.T) {
	b := newBus(t)
	const th = "users-shape"

	res, err := b.Propose(backend, th, ProposeReq{Title: "Shape of /users response", Contract: "openapi.yaml",
		BaseRev: 0, Edits: renameEdits, Reason: "DB column renamed for i18n", Blocking: true})
	must(t, err)
	if res.Thread.State != StateProposed || !strings.Contains(res.Events[0].Diff, "+        display_name:") {
		t.Fatalf("propose: state %s diff:\n%s", res.Thread.State, res.Events[0].Diff)
	}

	inbox, err := b.Inbox(frontend, RoleLiaison)
	must(t, err)
	if types(inbox) != "PROPOSE" {
		t.Fatalf("frontend liaison inbox = %s", types(inbox))
	}
	must(t, b.Ack(frontend, RoleLiaison, inbox[0].Seq))

	// Renames are "ask me" in the frontend mandate.
	res, err = b.Ask(frontend, th, AskReq{Question: "Backend wants to rename name→display_name. OK?", To: "human", Blocking: true})
	must(t, err)
	ask := res.Events[0]
	if ask.To != "human:teammate" || res.Thread.AwaitingHuman != frontend {
		t.Fatalf("ask to=%q awaiting=%q", ask.To, res.Thread.AwaitingHuman)
	}
	if inbox, _ := b.Inbox(backend, RoleLiaison); len(inbox) != 0 {
		t.Fatalf("backend should not see frontend's question to its human, got %s", types(inbox))
	}

	_, err = b.Answer(backend, th, AnswerReq{ReplyTo: ask.Seq, Answer: "sure"})
	wantStatus(t, err, 403)
	res, err = b.Answer("human:teammate", th, AnswerReq{ReplyTo: ask.Seq, Answer: "fine, but keep name for one release"})
	must(t, err)
	if res.Thread.AwaitingHuman != "" {
		t.Fatal("answer should clear awaiting_human")
	}
	inbox, _ = b.Inbox(frontend, RoleLiaison)
	if types(inbox) != "ANSWER" {
		t.Fatalf("frontend liaison inbox = %s", types(inbox))
	}

	res, err = b.Counter(frontend, th, CounterReq{BaseRev: 0, Edits: keepDeprecatedEdits,
		Reason: "add display_name, keep name as deprecated for one release"})
	must(t, err)
	counter := res.Events[0]
	if res.Thread.State != StateCountered {
		t.Fatalf("state = %s", res.Thread.State)
	}

	_, err = b.Accept(frontend, th, AcceptReq{})
	wantStatus(t, err, 409) // can't accept your own counter

	res, err = b.Accept(backend, th, AcceptReq{Reason: "additive, within mandate"})
	must(t, err)
	if types(res.Events) != "ACCEPT,DECIDED" || res.Thread.State != StateDecided || res.Thread.DecidedSeq != counter.Seq {
		t.Fatalf("accept: events %s thread %+v", types(res.Events), res.Thread)
	}

	c, err := b.Contract("openapi.yaml", -1)
	must(t, err)
	if c.Rev != 1 || !strings.Contains(c.Content, "display_name:") || !strings.Contains(c.Content, "deprecated: true") {
		t.Fatalf("contract rev %d:\n%s", c.Rev, c.Content)
	}
	for _, agent := range []string{backend, frontend} {
		inbox, _ := b.Inbox(agent, RoleWorker)
		if types(inbox) != "DECIDED" {
			t.Fatalf("%s worker inbox = %s", agent, types(inbox))
		}
	}
	if _, err := b.Counter(frontend, th, CounterReq{BaseRev: 1, Edits: renameEdits, Reason: "x"}); err == nil {
		t.Fatal("decided thread accepted a counter")
	}
}

func TestProposalsMustBeConcrete(t *testing.T) {
	b := newBus(t)
	req := ProposeReq{Title: "t", Contract: "openapi.yaml", Reason: "r"}

	_, err := b.Propose(backend, "t1", req)
	wantStatus(t, err, 400) // no edits

	req.Edits = []Edit{{Old: "type: string", New: "type: integer"}}
	_, err = b.Propose(backend, "t1", req)
	wantStatus(t, err, 400) // ambiguous

	req.Edits = []Edit{{Old: requiredOld, New: "required: [id, name"}}
	_, err = b.Propose(backend, "t1", req)
	wantStatus(t, err, 400) // invalid YAML

	req.Edits, req.BaseRev = renameEdits, 3
	_, err = b.Propose(backend, "t1", req)
	wantStatus(t, err, 409) // stale base

	_, err = b.Propose("mallory@nowhere", "t1", ProposeReq{Title: "t", Contract: "openapi.yaml", Edits: renameEdits, Reason: "r"})
	wantStatus(t, err, 403)
}

func TestCounterLimitEscalatesThenOwnerWinsOnTimeout(t *testing.T) {
	b := newBus(t)
	now := time.Unix(1_000_000, 0)
	b.Now = func() time.Time { return now }
	const th = "users-shape"

	res, err := b.Propose(frontend, th, ProposeReq{Title: "t", Contract: "openapi.yaml", Edits: keepDeprecatedEdits, Reason: "r"})
	must(t, err)
	_, err = b.Counter(backend, th, CounterReq{Edits: renameEdits, Reason: "r"})
	must(t, err)
	res, err = b.Counter(frontend, th, CounterReq{Edits: keepDeprecatedEdits, Reason: "r"})
	must(t, err)
	if res.Thread.State != StateCountered {
		t.Fatalf("after 2 counters: %s", res.Thread.State)
	}
	res, err = b.Counter(backend, th, CounterReq{Edits: renameEdits, Reason: "r"})
	must(t, err)
	ownerPick := res.Events[0]
	if res.Thread.State != StateEscalated || types(res.Events) != "COUNTER,ESCALATE" {
		t.Fatalf("3rd counter: state %s events %s", res.Thread.State, types(res.Events))
	}
	_, err = b.Accept(frontend, th, AcceptReq{})
	wantStatus(t, err, 409)

	now = now.Add(b.EscalationTimeout - time.Second)
	evs, err := b.SweepEscalations()
	must(t, err)
	if len(evs) != 0 {
		t.Fatalf("decided before timeout: %s", types(evs))
	}

	now = now.Add(2 * time.Second)
	evs, err = b.SweepEscalations()
	must(t, err)
	// Frontend's opening proposal was non-blocking and lost, so it hears about it.
	if types(evs) != "DECIDED,ASSUMPTION_BROKEN" || evs[0].ReplyTo != ownerPick.Seq || evs[1].To != frontend {
		t.Fatalf("sweep: %s", types(evs))
	}
	got, _, err := b.Thread(th)
	must(t, err)
	if got.ResolvedBy != "timeout:owner" {
		t.Fatalf("resolved_by = %q", got.ResolvedBy)
	}
}

func TestHumanResolvesEscalation(t *testing.T) {
	b := newBus(t)
	const th = "users-shape"
	res, err := b.Propose(backend, th, ProposeReq{Title: "t", Contract: "openapi.yaml", Edits: renameEdits, Reason: "r"})
	must(t, err)
	pick := res.Events[0].Seq
	_, err = b.Escalate(frontend, th, EscalateReq{Reason: "breaking change", Options: []string{"rename", "keep both"}})
	must(t, err)

	_, err = b.Resolve(backend, th, ResolveReq{Pick: pick})
	wantStatus(t, err, 403)
	res, err = b.Resolve("human:teammate", th, ResolveReq{Pick: pick, Note: "ship it"})
	must(t, err)
	if types(res.Events) != "RESOLVE,DECIDED" || res.Thread.ResolvedBy != "human:teammate" {
		t.Fatalf("resolve: %s %+v", types(res.Events), res.Thread)
	}
}

func TestBrokenAssumptionAndRebase(t *testing.T) {
	b := newBus(t)

	// Backend proceeds on a non-blocking proposal; frontend counters and wins.
	res, err := b.Propose(backend, "users-shape", ProposeReq{Title: "t", Contract: "openapi.yaml", Edits: renameEdits, Reason: "r"})
	must(t, err)
	assumed := res.Events[0].Seq

	// Meanwhile another thread moves the contract to rev 1.
	_, err = b.Propose(frontend, "add-avatar", ProposeReq{Title: "avatar", Contract: "openapi.yaml",
		Edits: []Edit{{Old: "        email:\n          type: string", New: "        email:\n          type: string\n        avatar_url:\n          type: string"}}, Reason: "r"})
	must(t, err)
	_, err = b.Accept(backend, "add-avatar", AcceptReq{})
	must(t, err)

	_, err = b.Counter(frontend, "users-shape", CounterReq{BaseRev: 1, Edits: keepDeprecatedEdits, Reason: "r"})
	must(t, err)
	res, err = b.Accept(backend, "users-shape", AcceptReq{})
	must(t, err)
	if types(res.Events) != "ACCEPT,DECIDED,ASSUMPTION_BROKEN" {
		t.Fatalf("events %s", types(res.Events))
	}
	if ab := res.Events[2]; ab.To != backend || ab.ReplyTo != assumed {
		t.Fatalf("assumption_broken %+v", ab)
	}
	inbox, _ := b.Inbox(backend, RoleWorker)
	if types(inbox) != "DECIDED,DECIDED,ASSUMPTION_BROKEN" { // add-avatar, users-shape
		t.Fatalf("backend worker inbox = %s", types(inbox))
	}

	// A proposal made at rev 1 is replayed onto rev 2 when accepted later.
	_, err = b.Propose(frontend, "add-bio", ProposeReq{Title: "bio", Contract: "openapi.yaml", BaseRev: 2,
		Edits: []Edit{{Old: "        avatar_url:\n          type: string", New: "        avatar_url:\n          type: string\n        bio:\n          type: string"}}, Reason: "r"})
	must(t, err)
	_, err = b.Propose(backend, "add-created", ProposeReq{Title: "created", Contract: "openapi.yaml", BaseRev: 2,
		Edits: []Edit{{Old: "      type: object", New: "      type: object\n      description: A user"}}, Reason: "r"})
	must(t, err)
	_, err = b.Accept(frontend, "add-created", AcceptReq{})
	must(t, err)
	_, err = b.Accept(backend, "add-bio", AcceptReq{})
	must(t, err)
	c, _ := b.Contract("openapi.yaml", -1)
	if c.Rev != 4 || !strings.Contains(c.Content, "bio:") || !strings.Contains(c.Content, "description: A user") {
		t.Fatalf("rebase lost an edit, rev %d:\n%s", c.Rev, c.Content)
	}
}

func TestLedgerIsAppendOnly(t *testing.T) {
	b := newBus(t)
	_, err := b.Notify(backend, NotifyReq{Topic: "added /health"})
	must(t, err)
	if _, err := b.db.Exec(`UPDATE events SET body = 'x'`); err == nil {
		t.Fatal("update succeeded")
	}
	if _, err := b.db.Exec(`DELETE FROM events`); err == nil {
		t.Fatal("delete succeeded")
	}
}

func TestWaitForReply(t *testing.T) {
	b := newBus(t)
	const th = "users-shape"
	_, err := b.Propose(backend, th, ProposeReq{Title: "t", Contract: "openapi.yaml", Edits: renameEdits, Reason: "r", Blocking: true})
	must(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	evs, err := b.WaitForReply(ctx, backend, th, 0)
	must(t, err)
	if len(evs) != 0 {
		t.Fatalf("got %s before any reply", types(evs))
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		b.Accept(frontend, th, AcceptReq{})
	}()
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	evs, err = b.WaitForReply(ctx, backend, th, 0)
	must(t, err)
	if types(evs) != "ACCEPT,DECIDED" {
		t.Fatalf("wait returned %s", types(evs))
	}
}
