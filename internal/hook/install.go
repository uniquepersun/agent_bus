package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Events are the Claude Code hook events bus-hook handles.
var Events = []string{"PostToolUse", "UserPromptSubmit", "Stop"}

// Install merges bus-hook into <project>/.claude/settings.local.json (local
// to this person, since it carries their identity). Earlier bus-hook entries
// are replaced; everything else in the file is kept.
func Install(project, command string) (string, error) {
	path := filepath.Join(project, ".claude", "settings.local.json")
	settings := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &settings); err != nil {
			return "", fmt.Errorf("%s is not valid JSON: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for _, event := range Events {
		groups, _ := hooks[event].([]any)
		kept := []any{}
		for _, g := range groups {
			if !mentionsBusHook(g) {
				kept = append(kept, g)
			}
		}
		group := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 10}}}
		if event == "PostToolUse" {
			group["matcher"] = "*"
		}
		hooks[event] = append(kept, group)
	}
	settings["hooks"] = hooks

	raw, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, append(raw, '\n'), 0o644)
}

func mentionsBusHook(group any) bool {
	raw, _ := json.Marshal(group)
	return strings.Contains(string(raw), "bus-hook")
}

// ShellQuote quotes s for a POSIX shell.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
