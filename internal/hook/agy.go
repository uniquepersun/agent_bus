package hook

import (
	"context"
	"encoding/json"
	"strings"
)

// AgyInput is the subset of Antigravity CLI (agy) hook input we use. agy
// does not include the event name, so the hook command line carries it.
type AgyInput struct {
	ConversationID    string          `json:"conversationId"`
	TerminationReason string          `json:"terminationReason"`
	ToolCall          json.RawMessage `json:"toolCall"`
}

// HandleAgy handles one agy hook event for a worker:
//
//   - PreInvocation (before every model call) injects new bus outcomes as a
//     system message, once per conversation.
//   - Stop keeps the worker going on a normal finish while changes are
//     unapplied or its blocking proposal is open; never on a user cancel.
func (h *Hook) HandleAgy(ctx context.Context, event string, in AgyInput) (map[string]any, error) {
	st := h.load(in.ConversationID)
	defer h.save(in.ConversationID, st)
	switch event {
	case "PreInvocation":
		text, err := h.inject(ctx, st)
		if err != nil || text == "" {
			return map[string]any{}, err
		}
		return map[string]any{"injectSteps": []any{
			map[string]any{"systemMessage": map[string]any{"systemMessage": text}},
		}}, nil
	case "Stop":
		if in.TerminationReason != "NO_TOOL_CALL" {
			return map[string]any{}, nil // the user canceled, or it errored: let it stop
		}
		reason, err := h.stop(ctx, st)
		if err != nil || reason == "" {
			return map[string]any{}, err
		}
		return map[string]any{"decision": "continue", "reason": reason}, nil
	}
	return map[string]any{}, nil
}

// readOnlyTools are agy tools a liaison may use: reading the repo and
// calling the agent-bus MCP server.
var readOnlyTools = map[string]bool{
	"read_file": true, "list_dir": true, "grep_search": true, "find_by_name": true,
	"view_file": true, "view_file_outline": true, "codebase_search": true,
	"list_resources": true, "read_resource": true,
}

// LiaisonGate is the PreToolUse decision for an agy liaison: read the repo
// and talk to the bus, nothing else. agy treats a missing decision as deny.
func LiaisonGate(in AgyInput) map[string]any {
	var call struct {
		Name string `json:"name"`
		Args struct {
			ServerName string `json:"ServerName"`
		} `json:"args"`
	}
	json.Unmarshal(in.ToolCall, &call)
	switch {
	case call.Name == "call_mcp_tool" && strings.HasPrefix(call.Args.ServerName, "agent-bus"):
		return map[string]any{"decision": "allow"}
	case readOnlyTools[call.Name]:
		return map[string]any{"decision": "allow"}
	}
	return map[string]any{"decision": "deny",
		"reason": "The agent-bus liaison is read-only: it may read files and use the agent-bus tools, nothing else."}
}
