// Package mcptools exposes the Agent Bus protocol as MCP tools. One binary
// serves both sides; identity and role come from configuration, never from
// tool arguments.
package mcptools

import (
	"context"
	"fmt"
	"time"

	"agentbus/internal/bus"
	"agentbus/internal/client"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxWaitSeconds = 50

type edit struct {
	Old string `json:"old" jsonschema:"exact text currently in the contract, copied verbatim with indentation; must occur exactly once (include surrounding lines to make it unique). Empty string appends new to the end."`
	New string `json:"new" jsonschema:"replacement text"`
}

func toEdits(es []edit) []bus.Edit {
	out := make([]bus.Edit, len(es))
	for i, e := range es {
		out[i] = bus.Edit{Old: e.Old, New: e.New}
	}
	return out
}

type proposeIn struct {
	Thread     string `json:"thread" jsonschema:"id for a new thread: a lowercase slug such as users-endpoint-shape"`
	Title      string `json:"title" jsonschema:"one-line topic, e.g. 'Shape of /users response'"`
	Contract   string `json:"contract" jsonschema:"contract to change"`
	BaseRev    int    `json:"base_rev" jsonschema:"rev of the contract your edits are against; get it from get_contract"`
	Edits      []edit `json:"edits" jsonschema:"the concrete change, as find/replace edits applied in order"`
	Reason     string `json:"reason" jsonschema:"why this change is needed"`
	Blocking   bool   `json:"blocking" jsonschema:"true if you cannot continue until this is decided; false to continue assuming it will be accepted (you are told if it isn't)"`
	MandateRef string `json:"mandate_ref,omitempty" jsonschema:"the line of your mandate that justifies this, if any"`
}

type counterIn struct {
	Thread     string `json:"thread"`
	BaseRev    int    `json:"base_rev" jsonschema:"current rev of the thread's contract; edits apply to that rev, not on top of the proposal you are countering"`
	Edits      []edit `json:"edits" jsonschema:"your alternative, as find/replace edits against base_rev"`
	Reason     string `json:"reason" jsonschema:"why your alternative is better"`
	MandateRef string `json:"mandate_ref,omitempty"`
}

type acceptIn struct {
	Thread string `json:"thread"`
	Reason string `json:"reason,omitempty"`
}

type askIn struct {
	Thread   string `json:"thread" jsonschema:"existing thread, or a new slug to open one"`
	Title    string `json:"title,omitempty" jsonschema:"required only when opening a new thread"`
	Question string `json:"question"`
	To       string `json:"to" jsonschema:"'peer' for the other agent, 'human' for your own human (required for anything your mandate says to ask about)"`
	Blocking bool   `json:"blocking"`
}

type answerIn struct {
	Thread  string `json:"thread"`
	ReplyTo int64  `json:"reply_to" jsonschema:"seq of the ASK you are answering"`
	Answer  string `json:"answer"`
}

type escalateIn struct {
	Thread  string   `json:"thread"`
	Reason  string   `json:"reason" jsonschema:"what the humans need to decide"`
	Options []string `json:"options,omitempty" jsonschema:"the choices, briefly"`
}

type notifyIn struct {
	Topic    string `json:"topic" jsonschema:"one-line summary"`
	Body     string `json:"body,omitempty"`
	Contract string `json:"contract,omitempty" jsonschema:"contract this relates to, if any"`
}

type threadIn struct {
	Thread string `json:"thread"`
}

type contractIn struct {
	Name string `json:"name"`
	Rev  *int   `json:"rev,omitempty" jsonschema:"omit for the current head"`
}

type decisionsIn struct {
	Contract string `json:"contract,omitempty" jsonschema:"omit for all contracts"`
	SinceSeq int64  `json:"since_seq,omitempty" jsonschema:"only decisions after this seq"`
}

type ackIn struct {
	Seq int64 `json:"seq" jsonschema:"seq of the last inbox event you handled; everything up to it is marked read"`
}

type waitIn struct {
	Thread   string `json:"thread"`
	After    int64  `json:"after,omitempty" jsonschema:"return events after this seq; omit to wait for anything newer than your own latest message in the thread"`
	TimeoutS int    `json:"timeout_s,omitempty" jsonschema:"seconds to wait, at most 50 (default 45); call again to keep waiting"`
}

type Config struct {
	Client    *client.Client
	Role      bus.Role
	Contracts []string // contract names, used as the enum for contract arguments
}

// NewServer builds the MCP server for one agent and role. Workers get the
// tools to read contracts, propose and hear outcomes; liaisons also get the
// negotiation tools.
func NewServer(cfg Config) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "agent-bus", Version: "0.1.0"},
		&mcp.ServerOptions{Instructions: instructions(cfg)})
	c := cfg.Client
	contractEnum := func(field string) func(*jsonschema.Schema) {
		return func(sc *jsonschema.Schema) {
			if p := sc.Properties[field]; p != nil && len(cfg.Contracts) > 0 {
				for _, n := range cfg.Contracts {
					p.Enum = append(p.Enum, n)
				}
			}
		}
	}
	nonEmptyEdits := func(sc *jsonschema.Schema) {
		one := 1
		sc.Properties["edits"].MinItems = &one
	}
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}

	addTool(s, &mcp.Tool{Name: "propose", Description: "Open a thread proposing a concrete change to a shared contract. Read the contract with get_contract first; edits must match its text exactly."},
		func(ctx context.Context, in proposeIn) (any, error) {
			return result(c.Act(ctx, in.Thread, "propose", bus.ProposeReq{Title: in.Title, Contract: in.Contract,
				BaseRev: in.BaseRev, Edits: toEdits(in.Edits), Reason: in.Reason, Blocking: in.Blocking, MandateRef: in.MandateRef}))
		}, contractEnum("contract"), nonEmptyEdits)

	addTool(s, &mcp.Tool{Name: "notify", Description: "Tell the other side about a decision inside your own scope. Opens no thread and needs no reply."},
		func(ctx context.Context, in notifyIn) (any, error) {
			return result(c.Notify(ctx, bus.NotifyReq{Topic: in.Topic, Body: in.Body, Contract: in.Contract}))
		}, contractEnum("contract"))

	addTool(s, &mcp.Tool{Name: "get_thread", Description: "A thread's state and full message history.", Annotations: readOnly},
		func(ctx context.Context, in threadIn) (any, error) {
			th, evs, err := c.Thread(ctx, in.Thread)
			if err != nil {
				return nil, err
			}
			return map[string]any{"thread": th, "events": compact(evs)}, nil
		})

	addTool(s, &mcp.Tool{Name: "get_contract", Description: "Current text and rev of a shared contract (or an older rev). Contracts change only through decided threads; never edit the files directly.", Annotations: readOnly},
		func(ctx context.Context, in contractIn) (any, error) {
			rev := -1
			if in.Rev != nil {
				rev = *in.Rev
			}
			return c.Contract(ctx, in.Name, rev)
		}, contractEnum("name"))

	addTool(s, &mcp.Tool{Name: "get_decisions", Description: "Decided changes to contracts, oldest first, each with its diff.", Annotations: readOnly},
		func(ctx context.Context, in decisionsIn) (any, error) {
			evs, err := c.Decisions(ctx, in.Contract, in.SinceSeq)
			if err != nil {
				return nil, err
			}
			return map[string]any{"decisions": compact(evs)}, nil
		}, contractEnum("contract"))

	addTool(s, &mcp.Tool{Name: "inbox", Description: inboxDescription(cfg.Role), Annotations: readOnly},
		func(ctx context.Context, _ struct{}) (any, error) {
			evs, err := c.Inbox(ctx, cfg.Role)
			if err != nil {
				return nil, err
			}
			return map[string]any{"events": compact(evs)}, nil
		})

	addTool(s, &mcp.Tool{Name: "ack", Description: "Mark inbox events up to seq as handled so they stop being delivered."},
		func(ctx context.Context, in ackIn) (any, error) {
			if err := c.Ack(ctx, cfg.Role, in.Seq); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true}, nil
		})

	addTool(s, &mcp.Tool{Name: "wait_for_reply", Description: "Block until someone else posts in a thread, or the timeout passes ({\"timeout\": true}). Use when you sent something blocking and have nothing else to do.", Annotations: readOnly},
		func(ctx context.Context, in waitIn) (any, error) {
			secs := in.TimeoutS
			if secs <= 0 {
				secs = 45
			}
			secs = min(secs, maxWaitSeconds)
			evs, err := c.Wait(ctx, in.Thread, in.After, time.Duration(secs)*time.Second)
			if err != nil {
				return nil, err
			}
			if len(evs) == 0 {
				return map[string]any{"timeout": true, "events": []bus.Event{}}, nil
			}
			return map[string]any{"timeout": false, "events": compact(evs)}, nil
		})

	if cfg.Role != bus.RoleLiaison {
		return s
	}

	addTool(s, &mcp.Tool{Name: "counter", Description: fmt.Sprintf("Reject the proposal on the table with a concrete alternative. A thread allows %d counters; the next one escalates to the humans.", bus.MaxCounters)},
		func(ctx context.Context, in counterIn) (any, error) {
			return result(c.Act(ctx, in.Thread, "counter", bus.CounterReq{BaseRev: in.BaseRev,
				Edits: toEdits(in.Edits), Reason: in.Reason, MandateRef: in.MandateRef}))
		}, nonEmptyEdits)

	addTool(s, &mcp.Tool{Name: "accept", Description: "Agree to the proposal on the table. This decides the thread and applies the change to the contract."},
		func(ctx context.Context, in acceptIn) (any, error) {
			return result(c.Act(ctx, in.Thread, "accept", bus.AcceptReq{Reason: in.Reason}))
		})

	addTool(s, &mcp.Tool{Name: "ask", Description: "Ask the peer agent or your own human a question. Asking your human is how you get a decision on anything outside your mandate."},
		func(ctx context.Context, in askIn) (any, error) {
			return result(c.Act(ctx, in.Thread, "ask", bus.AskReq{Title: in.Title, Question: in.Question, To: in.To, Blocking: in.Blocking}))
		}, func(sc *jsonschema.Schema) { sc.Properties["to"].Enum = []any{"peer", "human"} })

	addTool(s, &mcp.Tool{Name: "answer", Description: "Answer an ASK from the peer agent."},
		func(ctx context.Context, in answerIn) (any, error) {
			return result(c.Act(ctx, in.Thread, "answer", bus.AnswerReq{ReplyTo: in.ReplyTo, Answer: in.Answer}))
		})

	addTool(s, &mcp.Tool{Name: "escalate", Description: "Hand the thread to the humans when you can't agree or it is beyond both mandates."},
		func(ctx context.Context, in escalateIn) (any, error) {
			return result(c.Act(ctx, in.Thread, "escalate", bus.EscalateReq{Reason: in.Reason, Options: in.Options}))
		})

	return s
}

// addTool registers a handler with a schema inferred from In, adjusted by
// patches (enums, minItems) the struct tags can't express.
func addTool[In any](s *mcp.Server, t *mcp.Tool, h func(context.Context, In) (any, error), patches ...func(*jsonschema.Schema)) {
	sc, err := jsonschema.For[In](nil)
	if err != nil {
		panic(fmt.Sprintf("tool %s: %v", t.Name, err))
	}
	for _, p := range patches {
		p(sc)
	}
	t.InputSchema = sc
	mcp.AddTool(s, t, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		out, err := h(ctx, in)
		return nil, out, err
	})
}

// compact drops full contract snapshots from events; the diff carries the
// change and get_contract has the full text.
func compact(evs []bus.Event) []bus.Event {
	out := make([]bus.Event, len(evs))
	for i, e := range evs {
		e.ProposedContent = ""
		out[i] = e
	}
	return out
}

func result(res *bus.Result, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return map[string]any{"thread": res.Thread, "events": compact(res.Events)}, nil
}

func inboxDescription(role bus.Role) string {
	if role == bus.RoleWorker {
		return "Outcomes you must act on: DECIDED contract changes, ASSUMPTION_BROKEN (a non-blocking proposal of yours lost), and NOTIFY from the other side. Update your code, then ack."
	}
	return "Unhandled negotiation messages addressed to you. Handle them, then ack the last seq."
}

func instructions(cfg Config) string {
	common := fmt.Sprintf(`You are %s on the Agent Bus, which coordinates shared contracts (%v) between two agents on one project.
Contracts change only through decided threads; never edit contract files directly. Every proposal is a concrete find/replace edit against the contract's current rev.`,
		cfg.Client.From, cfg.Contracts)
	if cfg.Role == bus.RoleWorker {
		return common + `
You are the worker: you write code. When your work needs a change to a contract you don't own, call propose (blocking only if you truly cannot continue).
Agent-bus updates are lower priority than your user: finish your current step, then fold them in. After applying a DECIDED change or fixing an ASSUMPTION_BROKEN, ack it.`
	}
	return common + `
You are the liaison: you negotiate for your human under their mandate. Check inbox, act on each message, ack, then stop.
- Inside your mandate: accept, or counter with a concrete alternative.
- Outside it (the mandate's "ask me" list): ask your human (to: "human") before replying to the peer, then stop; you are restarted with their answer.
- Deadlocked or beyond both mandates: escalate.
Be brief. One action per message is usually enough.`
}
