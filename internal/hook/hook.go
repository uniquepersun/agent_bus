// Package hook implements the Claude Code hooks that bring bus outcomes
// into a busy worker session:
//
//   - PostToolUse and UserPromptSubmit inject new DECIDED / ASSUMPTION_BROKEN
//     / NOTIFY events as additionalContext, between steps, without stopping
//     the worker. Each event is injected once per session.
//   - Stop keeps the worker going while contract changes are unacked or its
//     own blocking proposal is still being negotiated.
//
// Hooks must never break the worker: any error means "no output".
package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agentbus/internal/bus"
	"agentbus/internal/client"
)

// MaxStopBlocks is how many times in a row Stop holds the worker for the
// same unchanged situation before letting it finish.
const MaxStopBlocks = 3

const maxDiffLines = 60

// Input is the subset of Claude Code's hook input we use.
type Input struct {
	SessionID      string `json:"session_id"`
	HookEventName  string `json:"hook_event_name"`
	StopHookActive bool   `json:"stop_hook_active"`
}

type Hook struct {
	Client   *client.Client
	StateDir string // per-session state lives here
}

type state struct {
	Injected        int64  `json:"injected"` // highest seq already injected into context
	StopFingerprint string `json:"stop_fingerprint"`
	StopBlocks      int    `json:"stop_blocks"`
}

// Handle returns the hook's JSON output, or nil for none.
func (h *Hook) Handle(ctx context.Context, in Input) (map[string]any, error) {
	st := h.load(in.SessionID)
	defer h.save(in.SessionID, st)
	switch in.HookEventName {
	case "PostToolUse", "UserPromptSubmit":
		text, err := h.inject(ctx, st)
		if err != nil || text == "" {
			return nil, err
		}
		return map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName":     in.HookEventName,
			"additionalContext": text,
		}}, nil
	case "Stop":
		reason, err := h.stop(ctx, st)
		if err != nil || reason == "" {
			return nil, err
		}
		return map[string]any{"decision": "block", "reason": reason}, nil
	}
	return nil, nil
}

func (h *Hook) inject(ctx context.Context, st *state) (string, error) {
	inbox, err := h.Client.Inbox(ctx, bus.RoleWorker)
	if err != nil {
		return "", err
	}
	var fresh []bus.Event
	for _, e := range inbox {
		if e.Seq > st.Injected {
			fresh = append(fresh, e)
		}
	}
	if len(fresh) == 0 {
		return "", nil
	}
	st.Injected = fresh[len(fresh)-1].Seq
	return Describe(fresh), nil
}

// Describe tells the worker what changed and what to do about it.
func Describe(evs []bus.Event) string {
	var sb strings.Builder
	sb.WriteString("[agent-bus] Contract update. This is lower priority than your user: finish your current step, then handle it.\n")
	needsAck := false
	for _, e := range evs {
		sb.WriteString("\n" + client.FormatEvent(e) + "\n")
		switch e.Type {
		case bus.Decided:
			needsAck = true
			fmt.Fprintf(&sb, "contracts/%s in the repo is now rev %d (read-only; never edit it). Change:\n```diff\n%s\n```\n",
				e.Contract, *e.Rev, truncate(e.Diff))
		case bus.AssumptionBroken:
			needsAck = true
			sb.WriteString("Code you wrote assuming your proposal would be accepted is now wrong. Re-read the contract and fix it.\n")
		}
	}
	last := evs[len(evs)-1].Seq
	if needsAck {
		fmt.Fprintf(&sb, "\nUpdate your code to match, then call the agent-bus ack tool with seq %d.\n", last)
	} else {
		fmt.Fprintf(&sb, "\nFYI only; call the agent-bus ack tool with seq %d once you've read it.\n", last)
	}
	return sb.String()
}

func (h *Hook) stop(ctx context.Context, st *state) (string, error) {
	inbox, err := h.Client.Inbox(ctx, bus.RoleWorker)
	if err != nil {
		return "", err
	}
	threads, err := h.Client.Threads(ctx)
	if err != nil {
		return "", err
	}
	var unapplied []bus.Event
	for _, e := range inbox {
		if e.Type == bus.Decided || e.Type == bus.AssumptionBroken {
			unapplied = append(unapplied, e)
		}
	}
	var waiting []bus.Thread
	for _, th := range threads {
		if th.OpenedBy == h.Client.From && th.Blocking && th.State != bus.StateDecided {
			waiting = append(waiting, th)
		}
	}
	if len(unapplied) == 0 && len(waiting) == 0 {
		st.StopFingerprint, st.StopBlocks = "", 0
		return "", nil
	}

	fp := fingerprint(unapplied, waiting)
	if fp != st.StopFingerprint {
		st.StopFingerprint, st.StopBlocks = fp, 0
	}
	if st.StopBlocks >= MaxStopBlocks {
		return "", nil // nothing changed after repeated nudges; let the worker stop
	}
	st.StopBlocks++

	var sb strings.Builder
	sb.WriteString("agent-bus: not done yet.\n")
	if len(unapplied) > 0 {
		last := unapplied[len(unapplied)-1].Seq
		fmt.Fprintf(&sb, "\n%d contract change(s) are not applied to your code yet:\n", len(unapplied))
		for _, e := range unapplied {
			sb.WriteString("- " + client.FormatEvent(e) + "\n")
		}
		fmt.Fprintf(&sb, "Read contracts/ (or get_decisions), update your code, then call the agent-bus ack tool with seq %d.\n", last)
		st.Injected = max(st.Injected, last)
	}
	if len(waiting) > 0 {
		sb.WriteString("\nYour blocking proposal(s) are still being negotiated:\n")
		for _, th := range waiting {
			fmt.Fprintf(&sb, "- %s (%s): %s\n", th.ID, th.State, th.Title)
		}
		sb.WriteString("Call the agent-bus wait_for_reply tool on the thread to wait for the decision, then apply it. If there is truly nothing to do until then, say so and stop.\n")
	}
	return sb.String(), nil
}

func fingerprint(unapplied []bus.Event, waiting []bus.Thread) string {
	var parts []string
	for _, e := range unapplied {
		parts = append(parts, fmt.Sprint(e.Seq))
	}
	for _, th := range waiting {
		parts = append(parts, fmt.Sprintf("%s@%d", th.ID, th.UpdatedSeq))
	}
	return strings.Join(parts, ",")
}

func truncate(diff string) string {
	lines := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	if len(lines) <= maxDiffLines {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:maxDiffLines], "\n") + fmt.Sprintf("\n... (%d more lines; see get_decisions)", len(lines)-maxDiffLines)
}

func (h *Hook) path(session string) string {
	if session == "" {
		session = "default"
	}
	return filepath.Join(h.StateDir, filepath.Base(session)+".json")
}

func (h *Hook) load(session string) *state {
	var st state
	if raw, err := os.ReadFile(h.path(session)); err == nil {
		json.Unmarshal(raw, &st)
	}
	return &st
}

func (h *Hook) save(session string, st *state) {
	if err := os.MkdirAll(h.StateDir, 0o755); err != nil {
		return
	}
	raw, _ := json.Marshal(st)
	os.WriteFile(h.path(session), raw, 0o644)
}
