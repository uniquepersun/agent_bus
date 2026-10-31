package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// AgyPluginName is the plugin (and MCP server) name. agy namespaces the
// server as "<plugin>_<server>", which the permission rule must match.
const AgyPluginName = "agent-bus"

// AgyMCPRule allows agy's headless mode to call the agent-bus MCP tools.
var AgyMCPRule = fmt.Sprintf("mcp(%s_%s/*)", AgyPluginName, AgyPluginName)

// AgyPlugin describes a workspace plugin at <root>/.agents/plugins/agent-bus.
type AgyPlugin struct {
	MCPCommand string
	MCPArgs    []string
	Hooks      map[string]any // agy hooks.json event map, e.g. {"PreInvocation": [...]}
	Rules      string         // rules/AGENTS.md; empty for none
}

// WriteAgyPlugin writes the plugin into root and returns its directory.
func WriteAgyPlugin(root string, p AgyPlugin) (string, error) {
	dir := filepath.Join(root, ".agents", "plugins", AgyPluginName)
	files := map[string]any{
		"plugin.json": map[string]any{"name": AgyPluginName},
		"mcp_config.json": map[string]any{"mcpServers": map[string]any{
			AgyPluginName: map[string]any{"command": p.MCPCommand, "args": p.MCPArgs},
		}},
		"hooks.json": map[string]any{AgyPluginName: p.Hooks},
	}
	for name, v := range files {
		raw, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return "", err
		}
		if err := writeFile(filepath.Join(dir, name), append(raw, '\n')); err != nil {
			return "", err
		}
	}
	if p.Rules != "" {
		if err := writeFile(filepath.Join(dir, "rules", "AGENTS.md"), []byte(p.Rules)); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// AgyHook is one command handler in agy's hooks.json.
func AgyHook(command string) map[string]any {
	return map[string]any{"type": "command", "command": command, "timeout": 10}
}

// AgySettingsPath is agy's global settings file, the only place headless
// permission rules are read from.
func AgySettingsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
}

// AddAgyPermissions adds allow rules to agy's settings, keeping everything
// else. It returns the rules that were newly added.
func AddAgyPermissions(settingsPath string, rules ...string) ([]string, error) {
	settings := map[string]any{}
	if raw, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(raw, &settings); err != nil {
			return nil, fmt.Errorf("%s is not valid JSON: %w", settingsPath, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	perms, _ := settings["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	var allow []string
	if list, ok := perms["allow"].([]any); ok {
		for _, r := range list {
			if s, ok := r.(string); ok {
				allow = append(allow, s)
			}
		}
	}
	var added []string
	for _, r := range rules {
		if !slices.Contains(allow, r) {
			allow = append(allow, r)
			added = append(added, r)
		}
	}
	if len(added) == 0 {
		return nil, nil
	}
	perms["allow"] = allow
	settings["permissions"] = perms
	raw, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, err
	}
	return added, writeFile(settingsPath, append(raw, '\n'))
}

// WorkerRules is the AGENTS.md shipped with the worker plugin.
func WorkerRules(id string) string {
	return strings.TrimSpace(fmt.Sprintf(`
# Agent Bus

You are %s, one of two coding agents on this project, coordinated through the Agent Bus.

- Shared contracts (API, schema, env) live in contracts/ and DECISIONS.md. They are read-only: never edit them.
- When your work needs a change to a shared contract, propose it with the agent-bus propose tool: read the contract with get_contract first and send exact find/replace edits against its current rev. Use blocking only if you cannot continue without the decision.
- Shell commands are restricted in this session. Use the file tools (read_file, list_dir, grep_search, write_file) and the agent-bus tools; if a tool is refused, continue another way.
- Messages marked [agent-bus] are contract updates. They are lower priority than your user: finish your current step, then update the code to match and call the agent-bus ack tool with the seq given.
`, id)) + "\n"
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
