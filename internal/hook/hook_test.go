package hook

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbus/internal/bus"
	"agentbus/internal/client"
	"agentbus/internal/server"
)

const (
	backend  = "backend@abhay"
	frontend = "frontend@teammate"
)

var renameEdits = []bus.Edit{
	{Old: "        name:\n          type: string", New: "        display_name:\n          type: string"},
	{Old: "required: [id, name, email]", New: "required: [id, display_name, email]"},
}

func setup(t *testing.T) (url string, h func(id string) *Hook) {
	t.Helper()
	b, err := bus.Open(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	seed, _ := os.ReadFile("../../seed/openapi.yaml")
	b.UpsertAgent(backend, "abhay", "")
	b.UpsertAgent(frontend, "teammate", "")
	b.SeedContract("openapi.yaml", backend, string(seed))
	ts := httptest.NewServer(server.New(b))
	t.Cleanup(ts.Close)
	stateDir := t.TempDir()
	return ts.URL, func(id string) *Hook {
		return &Hook{Client: client.New(ts.URL, id), StateDir: filepath.Join(stateDir, id)}
	}
}

func run(t *testing.T, h *Hook, event string) map[string]any {
	t.Helper()
	out, err := h.Handle(context.Background(), Input{SessionID: "s1", HookEventName: event})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func contextText(out map[string]any) string {
	if out == nil {
		return ""
	}
	return out["hookSpecificOutput"].(map[string]any)["additionalContext"].(string)
}

func TestWorkerHooks(t *testing.T) {
	url, newHook := setup(t)
	ctx := context.Background()
	be, fe := client.New(url, backend), client.New(url, frontend)
	worker := newHook(frontend)

	if out := run(t, worker, "PostToolUse"); out != nil {
		t.Fatalf("nothing pending, got %v", out)
	}
	if out := run(t, worker, "Stop"); out != nil {
		t.Fatalf("Stop with nothing pending: %v", out)
	}

	// Frontend's worker proposes (blocking); Stop holds it until decided.
	if _, err := fe.Act(ctx, "users-shape", "propose", bus.ProposeReq{Title: "Shape of /users",
		Contract: "openapi.yaml", Edits: renameEdits, Reason: "r", Blocking: true}); err != nil {
		t.Fatal(err)
	}
	out := run(t, worker, "Stop")
	if out["decision"] != "block" || !strings.Contains(out["reason"].(string), "wait_for_reply") {
		t.Fatalf("Stop should hold a worker with a blocking proposal: %v", out)
	}

	// Backend accepts: the decision is injected mid-task, once.
	if _, err := be.Act(ctx, "users-shape", "accept", bus.AcceptReq{}); err != nil {
		t.Fatal(err)
	}
	text := contextText(run(t, worker, "PostToolUse"))
	for _, want := range []string{"DECIDED [users-shape]", "contracts/openapi.yaml", "+        display_name:", "lower priority than your user", "ack tool with seq"} {
		if !strings.Contains(text, want) {
			t.Errorf("injected context missing %q:\n%s", want, text)
		}
	}
	if out := run(t, worker, "PostToolUse"); out != nil {
		t.Fatalf("same event injected twice: %v", out)
	}
	if out := run(t, worker, "UserPromptSubmit"); out != nil {
		t.Fatalf("UserPromptSubmit re-injected: %v", out)
	}

	// Unacked decision: Stop holds the worker, but gives up if nothing changes.
	for i := 0; i < MaxStopBlocks; i++ {
		out := run(t, worker, "Stop")
		if out["decision"] != "block" || !strings.Contains(out["reason"].(string), "not applied to your code") {
			t.Fatalf("Stop #%d: %v", i, out)
		}
	}
	if out := run(t, worker, "Stop"); out != nil {
		t.Fatalf("Stop should give up after %d unchanged blocks: %v", MaxStopBlocks, out)
	}

	// Acking clears it.
	inbox, _ := fe.Inbox(ctx, bus.RoleWorker)
	fe.Ack(ctx, bus.RoleWorker, inbox[len(inbox)-1].Seq)
	if out := run(t, worker, "Stop"); out != nil {
		t.Fatalf("Stop after ack: %v", out)
	}

	// A different session sees the event even though s1 already did... until acked.
	other, _ := newHook(backend).Handle(ctx, Input{SessionID: "s2", HookEventName: "UserPromptSubmit"})
	if !strings.Contains(contextText(other), "DECIDED") {
		t.Fatalf("backend worker should hear about the decision: %v", other)
	}
}

func TestInstallMergesSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.local.json")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(`{
  "permissions": {"allow": ["Bash(go test:*)"]},
  "hooks": {
    "PostToolUse": [
      {"matcher": "Edit", "hooks": [{"type": "command", "command": "gofmt-hook"}]},
      {"matcher": "*", "hooks": [{"type": "command", "command": "/old/bus-hook -as x"}]}
    ]
  }
}`), 0o644)

	if _, err := Install(dir, "/new/bus-hook -as frontend@teammate"); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(dir, "/new/bus-hook -as frontend@teammate"); err != nil { // idempotent
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var got struct {
		Permissions map[string]any
		Hooks       map[string][]struct {
			Matcher string
			Hooks   []struct{ Command string }
		}
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Permissions == nil {
		t.Fatal("dropped unrelated settings")
	}
	post := got.Hooks["PostToolUse"]
	if len(post) != 2 || post[0].Hooks[0].Command != "gofmt-hook" || post[1].Hooks[0].Command != "/new/bus-hook -as frontend@teammate" {
		t.Fatalf("PostToolUse groups: %+v", post)
	}
	for _, e := range []string{"UserPromptSubmit", "Stop"} {
		if len(got.Hooks[e]) != 1 {
			t.Fatalf("%s groups: %+v", e, got.Hooks[e])
		}
	}
}
