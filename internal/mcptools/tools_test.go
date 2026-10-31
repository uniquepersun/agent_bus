package mcptools

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agentbus/internal/bus"
	"agentbus/internal/client"
	"agentbus/internal/server"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	backend  = "backend@abhay"
	frontend = "frontend@teammate"
)

func startBus(t *testing.T) string {
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
	ts := httptest.NewServer(server.New(b))
	t.Cleanup(ts.Close)
	return ts.URL
}

type session struct {
	t  *testing.T
	cs *mcp.ClientSession
}

func connect(t *testing.T, url, as string, role bus.Role) *session {
	t.Helper()
	ctx := context.Background()
	srv := NewServer(Config{Client: client.New(url, as), Role: role, Contracts: []string{"openapi.yaml"}})
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return &session{t, cs}
}

// call invokes a tool and decodes its JSON output. wantErr says whether the
// tool should report an error; the error text is returned in that case.
func (s *session) call(name string, args map[string]any, out any, wantErr bool) string {
	s.t.Helper()
	res, err := s.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		s.t.Fatalf("%s: %v", name, err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if res.IsError != wantErr {
		s.t.Fatalf("%s: isError=%v: %s", name, res.IsError, text)
	}
	if out != nil && !wantErr {
		if err := json.Unmarshal([]byte(text), out); err != nil {
			s.t.Fatalf("%s: decode %q: %v", name, text, err)
		}
	}
	return text
}

type eventsOut struct {
	Thread *bus.Thread
	Events []bus.Event
}

func TestToolsPerRole(t *testing.T) {
	url := startBus(t)
	names := func(s *session) []string {
		res, err := s.cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, tool := range res.Tools {
			out = append(out, tool.Name)
		}
		slices.Sort(out)
		return out
	}
	worker := names(connect(t, url, backend, bus.RoleWorker))
	liaison := names(connect(t, url, backend, bus.RoleLiaison))
	if want := "ack get_contract get_decisions get_thread inbox notify propose wait_for_reply"; strings.Join(worker, " ") != want {
		t.Fatalf("worker tools: %v", worker)
	}
	if len(liaison) != len(worker)+5 || !slices.Contains(liaison, "counter") || !slices.Contains(liaison, "accept") {
		t.Fatalf("liaison tools: %v", liaison)
	}

	res, _ := connect(t, url, backend, bus.RoleLiaison).cs.ListTools(context.Background(), nil)
	for _, tool := range res.Tools {
		if tool.Name != "propose" {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		schema := string(raw)
		for _, want := range []string{`"enum":["openapi.yaml"]`, `"minItems":1`, `"blocking"`} {
			if !strings.Contains(schema, want) {
				t.Errorf("propose schema missing %s: %s", want, schema)
			}
		}
		var parsed struct{ Required []string }
		json.Unmarshal(raw, &parsed)
		for _, f := range []string{"thread", "title", "contract", "base_rev", "edits", "reason", "blocking"} {
			if !slices.Contains(parsed.Required, f) {
				t.Errorf("propose: %s should be required, required=%v", f, parsed.Required)
			}
		}
	}
}

// The demo trace from SPEC.md, driven through MCP tools by both liaisons and
// observed by the frontend worker.
func TestDemoTraceOverMCP(t *testing.T) {
	url := startBus(t)
	be := connect(t, url, backend, bus.RoleLiaison)
	fe := connect(t, url, frontend, bus.RoleLiaison)
	feWorker := connect(t, url, frontend, bus.RoleWorker)

	var c bus.Contract
	be.call("get_contract", map[string]any{"name": "openapi.yaml"}, &c, false)

	nameField := "        name:\n          type: string"
	required := map[string]any{"old": "required: [id, name, email]", "new": "required: [id, display_name, email]"}

	// Vague proposals are rejected by the schema before reaching the server.
	be.call("propose", map[string]any{"thread": "users-shape", "title": "t", "contract": "openapi.yaml",
		"base_rev": c.Rev, "edits": []any{}, "reason": "r", "blocking": true}, nil, true)

	var out eventsOut
	be.call("propose", map[string]any{"thread": "users-shape", "title": "Shape of /users response",
		"contract": "openapi.yaml", "base_rev": c.Rev, "reason": "DB column renamed for i18n", "blocking": true,
		"edits": []any{map[string]any{"old": nameField, "new": "        display_name:\n          type: string"}, required},
	}, &out, false)
	if out.Thread.State != bus.StateProposed || out.Events[0].ProposedContent != "" || out.Events[0].Diff == "" {
		t.Fatalf("propose: %+v", out)
	}

	var inbox eventsOut
	fe.call("inbox", nil, &inbox, false)
	if len(inbox.Events) != 1 || inbox.Events[0].Type != bus.Propose {
		t.Fatalf("frontend inbox: %+v", inbox.Events)
	}
	fe.call("ack", map[string]any{"seq": inbox.Events[0].Seq}, nil, false)

	fe.call("ask", map[string]any{"thread": "users-shape", "question": "Rename name→display_name?", "to": "human", "blocking": true}, &out, false)
	ask := out.Events[0]
	if _, err := client.New(url, "human:teammate").Act(context.Background(), "users-shape", "answer",
		bus.AnswerReq{ReplyTo: ask.Seq, Answer: "fine, but keep name for one release"}); err != nil {
		t.Fatal(err)
	}

	fe.call("counter", map[string]any{"thread": "users-shape", "base_rev": c.Rev, "reason": "keep name, deprecated, for one release",
		"edits": []any{map[string]any{"old": nameField, "new": "        display_name:\n          type: string\n" + nameField + "\n          deprecated: true"}, required},
	}, &out, false)

	// Server errors come back as tool errors the agent can read.
	if msg := fe.call("accept", map[string]any{"thread": "users-shape"}, nil, true); !strings.Contains(msg, "your own proposal") {
		t.Fatalf("self-accept error: %s", msg)
	}

	var waited struct {
		Timeout bool
		Events  []bus.Event
	}
	be.call("wait_for_reply", map[string]any{"thread": "users-shape", "timeout_s": 1}, &waited, false)
	if waited.Timeout || waited.Events[len(waited.Events)-1].Type != bus.Counter {
		t.Fatalf("wait: %+v", waited)
	}

	be.call("accept", map[string]any{"thread": "users-shape", "reason": "additive, within mandate"}, &out, false)
	if out.Thread.State != bus.StateDecided {
		t.Fatalf("accept: %+v", out.Thread)
	}

	feWorker.call("inbox", nil, &inbox, false)
	if len(inbox.Events) != 1 || inbox.Events[0].Type != bus.Decided || !strings.Contains(inbox.Events[0].Diff, "+          deprecated: true") {
		t.Fatalf("frontend worker inbox: %+v", inbox.Events)
	}
	var decisions struct{ Decisions []bus.Event }
	feWorker.call("get_decisions", map[string]any{"contract": "openapi.yaml"}, &decisions, false)
	if len(decisions.Decisions) != 1 {
		t.Fatalf("decisions: %+v", decisions)
	}
}
