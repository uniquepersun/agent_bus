package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgyRunner(t *testing.T) {
	tmp := t.TempDir()
	// A fake agy that records its working directory and arguments.
	fake := filepath.Join(tmp, "agy")
	record := filepath.Join(tmp, "record")
	os.WriteFile(fake, []byte("#!/bin/sh\npwd > "+record+"\nfor a in \"$@\"; do echo \"$a\" >> "+record+"; done\necho handled\n"), 0o755)

	r := AgyRunner{Bin: fake, MCPBin: "/bin/bus-mcp", HookBin: "/bin/bus-hook", Server: "http://s", ID: frontend,
		Workspace: filepath.Join(tmp, "ws"), LogDir: filepath.Join(tmp, "logs")}
	if err := r.Prepare(); err != nil {
		t.Fatal(err)
	}
	if err := r.Prepare(); err != nil { // idempotent
		t.Fatal(err)
	}
	plugin := filepath.Join(r.Workspace, ".agents", "plugins", "agent-bus")
	mcp, _ := os.ReadFile(filepath.Join(plugin, "mcp_config.json"))
	hooks, _ := os.ReadFile(filepath.Join(plugin, "hooks.json"))
	if !strings.Contains(string(mcp), `"liaison"`) || !strings.Contains(string(hooks), "-liaison-gate") {
		t.Fatalf("plugin:\n%s\n%s", mcp, hooks)
	}
	if _, err := os.Stat(filepath.Join(r.Workspace, ".git")); err != nil {
		t.Fatal("workspace should be a git root")
	}

	if err := r.Run(context.Background(), "handle seq 4"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(record)
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	wd, _ := filepath.EvalSymlinks(r.Workspace)
	if gotWD, _ := filepath.EvalSymlinks(lines[0]); gotWD != wd {
		t.Errorf("ran in %s, want the liaison workspace %s", lines[0], wd)
	}
	if strings.Join(lines[1:], " ") != "--print=handle seq 4" {
		t.Errorf("args: %v", lines[1:])
	}
}
