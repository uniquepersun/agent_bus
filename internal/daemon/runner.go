package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"agentbus/internal/hook"
)

// ClaudeRunner starts `claude -p` with only the agent-bus MCP server and
// read-only file tools.
type ClaudeRunner struct {
	Bin    string // claude executable
	MCPBin string // bus-mcp executable
	Server string // busd URL
	ID     string // agent identity
	Dir    string // working directory (the repo)
	LogDir string // full transcripts go here
	Out    io.Writer
}

func (r ClaudeRunner) Run(ctx context.Context, prompt string) error {
	cfg, err := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"agent-bus": map[string]any{
			"command": r.MCPBin,
			"args":    []string{"-server", r.Server, "-as", r.ID, "-role", "liaison"},
		},
	}})
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, r.Bin, "-p",
		"--mcp-config", string(cfg), "--strict-mcp-config",
		"--allowedTools", "mcp__agent-bus", "Read", "Grep", "Glob")
	cmd.Stdin = strings.NewReader(prompt) // keeps the prompt clear of variadic flags
	return runLogged(cmd, r.Dir, r.LogDir, r.Out, prompt)
}

// AgyRunner starts the Antigravity CLI headless (`agy --print`). It runs from
// its own workspace outside the repo, so it loads only the liaison plugin
// (liaison-role MCP server plus a PreToolUse gate that allows reading and
// agent-bus tools only), and sees the repo through --add-dir.
type AgyRunner struct {
	Bin       string // agy executable
	MCPBin    string // bus-mcp executable
	HookBin   string // bus-hook executable, for the read-only gate
	Server    string
	ID        string
	Repo      string // the project, added read-only
	Workspace string // the liaison's own workspace directory
	LogDir    string
	Out       io.Writer
}

// Prepare creates the liaison workspace. A .git directory bounds agy's
// customization discovery to it.
func (r AgyRunner) Prepare() error {
	if err := os.MkdirAll(r.Workspace, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(r.Workspace, ".git")); os.IsNotExist(err) {
		if out, err := exec.Command("git", "init", "-q", r.Workspace).CombinedOutput(); err != nil {
			return fmt.Errorf("git init %s: %v: %s", r.Workspace, err, out)
		}
	}
	gate := hook.ShellQuote(r.HookBin) + " -format agy -liaison-gate"
	_, err := hook.WriteAgyPlugin(r.Workspace, hook.AgyPlugin{
		MCPCommand: r.MCPBin,
		MCPArgs:    []string{"-server", r.Server, "-as", r.ID, "-role", "liaison"},
		Hooks: map[string]any{
			"PreToolUse": []any{map[string]any{"matcher": "*", "hooks": []any{hook.AgyHook(gate)}}},
		},
	})
	return err
}

func (r AgyRunner) Run(ctx context.Context, prompt string) error {
	// --print=<prompt> keeps the prompt attached to its flag.
	cmd := exec.CommandContext(ctx, r.Bin, "--print="+prompt, "--add-dir", r.Repo)
	return runLogged(cmd, r.Workspace, r.LogDir, r.Out, prompt)
}

// CommandRunner runs any headless agent CLI with the prompt as its last
// argument, e.g. ["codex", "exec"]. The CLI must already have the agent-bus
// MCP server configured with -role liaison.
type CommandRunner struct {
	Argv   []string
	Dir    string
	LogDir string
	Out    io.Writer
}

func (r CommandRunner) Run(ctx context.Context, prompt string) error {
	args := append(append([]string{}, r.Argv[1:]...), prompt)
	return runLogged(exec.CommandContext(ctx, r.Argv[0], args...), r.Dir, r.LogDir, r.Out, prompt)
}

// runLogged runs cmd, saving the prompt and full output to a log file and
// echoing the agent's final reply to out.
func runLogged(cmd *exec.Cmd, dir, logDir string, out io.Writer, prompt string) error {
	cmd.Dir = dir
	var logFile io.Writer = io.Discard
	if logDir != "" {
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			return err
		}
		f, err := os.Create(filepath.Join(logDir, "liaison-"+time.Now().Format("20060102-150405")+".log"))
		if err != nil {
			return err
		}
		defer f.Close()
		fmt.Fprintf(f, "$ %s\n--- prompt ---\n%s\n--- output ---\n", strings.Join(cmd.Args, " "), prompt)
		logFile = f
	}
	var reply strings.Builder
	cmd.Stdout = io.MultiWriter(logFile, &reply)
	cmd.Stderr = logFile
	err := cmd.Run()
	if text := strings.TrimSpace(reply.String()); text != "" && out != nil {
		fmt.Fprintf(out, "    liaison: %s\n", strings.ReplaceAll(text, "\n", "\n    "))
	}
	return err
}
