package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"agentbus/internal/client"
)

// summaryKeys are the tool arguments worth showing, most telling first, for
// Claude Code (snake_case) and agy (PascalCase) tools.
var summaryKeys = []string{
	"command", "CommandLine", "file_path", "TargetFile", "AbsolutePath", "Path", "path",
	"DirectoryPath", "SearchPath", "pattern", "Query", "query", "url",
}

// Summarize describes a tool call in one line, e.g. "Edit src/UserCard.tsx"
// or "agent-bus propose".
func Summarize(tool string, args map[string]any) string {
	if tool == "call_mcp_tool" { // agy wraps every MCP call
		server, _ := args["ServerName"].(string)
		name, _ := args["ToolName"].(string)
		return strings.TrimSpace(strings.Split(server, "_")[0] + " " + name)
	}
	if server, name, ok := strings.Cut(strings.TrimPrefix(tool, "mcp__"), "__"); ok && strings.HasPrefix(tool, "mcp__") {
		return server + " " + name // Claude Code's mcp__<server>__<tool>
	}
	for _, k := range summaryKeys {
		if v, ok := args[k].(string); ok && v != "" {
			if filepath.IsAbs(v) && !strings.Contains(v, " ") {
				v = shortPath(v)
			}
			v = strings.Join(strings.Fields(v), " ")
			if len(v) > 100 {
				v = v[:97] + "..."
			}
			return tool + " " + v
		}
	}
	return tool
}

// shortPath keeps the last three path elements.
func shortPath(p string) string {
	parts := strings.Split(filepath.ToSlash(p), "/")
	if len(parts) > 3 {
		return ".../" + strings.Join(parts[len(parts)-3:], "/")
	}
	return p
}

// Report posts one line of activity, best effort, within a second.
func Report(c *client.Client, role, kind, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c.PostActivity(ctx, role, kind, text)
}

// AgyCall parses an agy tool call into its name and arguments.
func AgyCall(in AgyInput) (string, map[string]any) {
	var call struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	}
	json.Unmarshal(in.ToolCall, &call)
	return call.Name, call.Args
}

// GateSummary describes a gated agy tool call, noting a refusal.
func GateSummary(in AgyInput, decision map[string]any) string {
	name, args := AgyCall(in)
	s := Summarize(name, args)
	if decision["decision"] == "deny" {
		s = fmt.Sprintf("%s (refused)", s)
	}
	return s
}
