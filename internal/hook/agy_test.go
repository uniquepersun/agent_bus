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

	// Unlike Claude Code, an agy worker is not held for its own open proposal.
	inbox, _ := fe.Inbox(ctx, bus.RoleWorker)
	fe.Ack(ctx, bus.RoleWorker, inbox[len(inbox)-1].Seq)
	if _, err := fe.Act(ctx, "avatar", "propose", bus.ProposeReq{Title: "a", Contract: "openapi.yaml", BaseRev: 1,
		Edits: []bus.Edit{{Old: "", New: "# a\n"}}, Reason: "r", Blocking: true}); err != nil {
		t.Fatal(err)
	}
	if out := run("Stop", "NO_TOOL_CALL"); len(out) != 0 {
		t.Fatalf("agy Stop held for an open proposal: %v", out)
	}
}

func TestLiaisonGate(t *testing.T) {
	call := func(name string, args map[string]any) AgyInput {
		raw, _ := json.Marshal(map[string]any{"name": name, "args": args})
		return AgyInput{ToolCall: raw, WorkspacePaths: []string{"/cache/liaison"}}
	}
	for _, tc := range []struct {
		in   AgyInput
		want string
	}{
		{call("call_mcp_tool", map[string]any{"ServerName": "agent-bus_agent-bus"}), "allow"},
		{call("read_file", map[string]any{"Path": "/home/.gemini/antigravity-cli/mcp/agent-bus_agent-bus/inbox.json"}), "allow"},
		{call("list_dir", map[string]any{"DirectoryPath": "/cache/liaison/.agents"}), "allow"},
		{call("read_file", map[string]any{"Path": "/home/code/app/src/UserCard.tsx"}), "deny"},
		{call("read_file", map[string]any{"Path": "/home/.gemini/antigravity-cli/mcp_config.json"}), "deny"},
		{call("read_file", map[string]any{"Path": "/cache/liaison-other/x"}), "deny"},
		{call("call_mcp_tool", map[string]any{"ServerName": "github"}), "deny"},
		{call("write_file", map[string]any{"Path": "/cache/liaison/x"}), "deny"},
		{call("run_command", nil), "deny"},
		{AgyInput{}, "deny"},
	} {
		if got := LiaisonGate(tc.in, "/home/.gemini/antigravity-cli/mcp")["decision"]; got != tc.want {
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

func TestWorkerGate(t *testing.T) {
	cmd := func(line string) AgyInput {
		raw, _ := json.Marshal(map[string]any{"name": "run_command", "args": map[string]any{"CommandLine": line}})
		return AgyInput{ToolCall: raw}
	}
	read, _ := json.Marshal(map[string]any{"name": "write_file", "args": map[string]any{}})
	allowed := []string{"npm test", "grep"}
	for _, tc := range []struct {
		in   AgyInput
		want string
	}{
		{AgyInput{ToolCall: read}, "allow"},
		{cmd("npm test"), "allow"},
		{cmd("grep -rn name src/"), "allow"},
		{cmd("npm testing"), "deny"},
		{cmd("git status; ls -la"), "deny"},
		{cmd("grep x | rm -rf /"), "deny"},
		{cmd("ls -la"), "deny"},
	} {
		out := WorkerGate(tc.in, allowed)
		if out["decision"] != tc.want {
			t.Errorf("%s: got %v, want %s", tc.in.ToolCall, out["decision"], tc.want)
		}
		if tc.want == "deny" && !strings.Contains(out["reason"].(string), "npm test, grep") {
			t.Errorf("deny reason should list allowed commands: %v", out["reason"])
		}
	}
}
