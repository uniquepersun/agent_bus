package hook

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbus/internal/bus"
	"agentbus/internal/client"
)

func TestAgyWorkerHooks(t *testing.T) {
	url, newHook := setup(t)
	ctx := context.Background()
	be, fe := client.New(url, backend), client.New(url, frontend)
	worker := newHook(frontend)
	run := func(event, reason string) map[string]any {
		t.Helper()
		out, err := worker.HandleAgy(ctx, event, AgyInput{ConversationID: "c1", TerminationReason: reason})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	if out := run("PreInvocation", ""); len(out) != 0 {
		t.Fatalf("nothing pending: %v", out)
	}
	if _, err := be.Act(ctx, "users-shape", "propose", bus.ProposeReq{Title: "t", Contract: "openapi.yaml", Edits: renameEdits, Reason: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fe.Act(ctx, "users-shape", "accept", bus.AcceptReq{}); err != nil {
		t.Fatal(err)
	}

	out := run("PreInvocation", "")
	steps, _ := out["injectSteps"].([]any)
	if len(steps) != 1 {
		t.Fatalf("injectSteps: %v", out)
	}
	text := steps[0].(map[string]any)["systemMessage"].(map[string]any)["systemMessage"].(string)
	if !strings.Contains(text, "DECIDED [users-shape]") || !strings.Contains(text, "[agent-bus]") {
		t.Fatalf("injected: %s", text)
	}
	if out := run("PreInvocation", ""); len(out) != 0 {
		t.Fatalf("injected twice: %v", out)
	}

	if out := run("Stop", "USER_CANCELED"); len(out) != 0 {
		t.Fatalf("must not hold a user cancel: %v", out)
	}
	if out := run("Stop", "NO_TOOL_CALL"); out["decision"] != "continue" || !strings.Contains(out["reason"].(string), "not applied") {
		t.Fatalf("Stop: %v", out)
	}
}

func TestLiaisonGate(t *testing.T) {
	call := func(name, server string) AgyInput {
		raw, _ := json.Marshal(map[string]any{"name": name, "args": map[string]any{"ServerName": server}})
		return AgyInput{ToolCall: raw}
	}
	for _, tc := range []struct {
		in   AgyInput
		want string
	}{
		{call("call_mcp_tool", "agent-bus_agent-bus"), "allow"},
		{call("read_file", ""), "allow"},
		{call("call_mcp_tool", "github"), "deny"},
		{call("write_file", ""), "deny"},
		{call("run_command", ""), "deny"},
		{AgyInput{}, "deny"},
	} {
		if got := LiaisonGate(tc.in)["decision"]; got != tc.want {
			t.Errorf("%s: got %v, want %s", tc.in.ToolCall, got, tc.want)
		}
	}
}

func TestAgyPluginAndPermissions(t *testing.T) {
	root := t.TempDir()
	dir, err := WriteAgyPlugin(root, AgyPlugin{MCPCommand: "/bin/bus-mcp", MCPArgs: []string{"-role", "worker"},
		Hooks: map[string]any{"Stop": []any{AgyHook("bus-hook -format agy -event Stop")}}, Rules: WorkerRules(frontend)})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"plugin.json", "mcp_config.json", "hooks.json", "rules/AGENTS.md"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "mcp_config.json"))
	if !strings.Contains(string(raw), `"agent-bus"`) || !strings.Contains(string(raw), "/bin/bus-mcp") {
		t.Fatalf("mcp_config.json: %s", raw)
	}

	settings := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(settings, []byte(`{"model":"x","permissions":{"allow":["command(npm test)"]}}`), 0o644)
	added, err := AddAgyPermissions(settings, AgyMCPRule, "write_file(/p)")
	if err != nil || len(added) != 2 {
		t.Fatalf("added %v, %v", added, err)
	}
	if added, _ := AddAgyPermissions(settings, AgyMCPRule); len(added) != 0 {
		t.Fatalf("rule added twice: %v", added)
	}
	raw, _ = os.ReadFile(settings)
	var got struct {
		Model       string
		Permissions struct{ Allow []string }
	}
	json.Unmarshal(raw, &got)
	if got.Model != "x" || len(got.Permissions.Allow) != 3 || got.Permissions.Allow[1] != "mcp(agent-bus_agent-bus/*)" {
		t.Fatalf("settings: %s", raw)
	}
}
