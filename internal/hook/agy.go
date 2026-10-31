package hook

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
)

// AgyInput is the subset of Antigravity CLI (agy) hook input we use. agy
// does not include the event name, so the hook command line carries it.
type AgyInput struct {
	ConversationID    string          `json:"conversationId"`
	WorkspacePaths    []string        `json:"workspacePaths"`
	TerminationReason string          `json:"terminationReason"`
	ToolCall          json.RawMessage `json:"toolCall"`
}

// HandleAgy handles one agy hook event for a worker:
//
//   - PreInvocation (before every model call) injects new bus outcomes as a
//     system message, once per conversation.
//   - Stop keeps the worker going on a normal finish while decided changes
//     are unapplied; never on a user cancel.
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
		// No hold for open proposals: agy treats a long hold as something to
		// investigate, and bus chat wakes an idle agent when the decision lands.
		reason, err := h.stop(ctx, st, false)
		if err != nil || reason == "" {
			return map[string]any{}, err
		}
		return map[string]any{"decision": "continue", "reason": reason}, nil
	}
	return map[string]any{}, nil
}

// WorkerGate is the PreToolUse decision for an agy worker. Headless agy ends
// the whole turn when it auto-denies a tool, but a hook denial is returned to
// the model, which then tries another way. So shell commands outside the
// allowed prefixes are refused here, with directions, before agy's own
// check can end the turn. Everything else goes on to agy's normal checks.
func WorkerGate(in AgyInput, allowed []string) map[string]any {
	var call struct {
		Name string `json:"name"`
		Args struct {
			CommandLine string `json:"CommandLine"`
		} `json:"args"`
	}
	json.Unmarshal(in.ToolCall, &call)
	if call.Name != "run_command" || commandAllowed(call.Args.CommandLine, allowed) {
		return map[string]any{"decision": "allow"}
	}
	reason := "Shell commands are not available in this session"
	if len(allowed) > 0 {
		reason += " except: " + strings.Join(allowed, ", ")
	}
	return map[string]any{"decision": "deny", "reason": reason +
		". Use the file tools (read_file, list_dir, grep_search, write_file) and the agent-bus tools instead, and carry on with the task."}
}

// commandAllowed reports whether a single command starts with an allowed
// prefix. Compound commands are never allowed.
func commandAllowed(cmd string, allowed []string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" || strings.ContainsAny(cmd, ";|&`$\n<>") {
		return false
	}
	for _, p := range allowed {
		if cmd == p || strings.HasPrefix(cmd, p+" ") {
			return true
		}
	}
	return false
}

// readOnlyTools are agy tools a liaison may use: reading the repo and
// calling the agent-bus MCP server.
var readOnlyTools = map[string]bool{
	"read_file": true, "list_dir": true, "grep_search": true, "find_by_name": true,
	"view_file": true, "view_file_outline": true, "codebase_search": true,
	"list_resources": true, "read_resource": true,
}

// LiaisonGate is the PreToolUse decision for an agy liaison: talk to the bus
// and read within its own workspace and schemaDir (agy's MCP tool schemas),
// nothing else. Headless agy would end the turn on any read it
// has no rule for, so such reads are refused here with directions instead.
// agy treats a missing decision as deny.
func LiaisonGate(in AgyInput, schemaDir string) map[string]any {
	var call struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	}
	json.Unmarshal(in.ToolCall, &call)
	if call.Name == "call_mcp_tool" {
		if server, _ := call.Args["ServerName"].(string); strings.HasPrefix(server, "agent-bus") {
			return map[string]any{"decision": "allow"}
		}
	}
	if readOnlyTools[call.Name] && pathsWithin(call.Args, append([]string{schemaDir}, in.WorkspacePaths...)) {
		return map[string]any{"decision": "allow"}
	}
	return map[string]any{"decision": "deny", "reason": "The agent-bus liaison can only use the agent-bus tools. " +
		"Read contracts with get_contract and history with get_thread; do not read or change project files."}
}

// pathsWithin reports whether every absolute path among a tool's arguments
// is inside one of the roots.
func pathsWithin(args map[string]any, roots []string) bool {
	for _, v := range args {
		p, ok := v.(string)
		if !ok || !filepath.IsAbs(p) {
			continue
		}
		inside := false
		for _, r := range roots {
			if r != "" && (p == r || strings.HasPrefix(p, strings.TrimSuffix(r, "/")+"/")) {
				inside = true
			}
		}
		if !inside {
			return false
		}
	}
	return true
}
