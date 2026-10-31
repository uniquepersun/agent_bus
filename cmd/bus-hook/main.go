// Command bus-hook brings agent-bus outcomes into a worker session. It runs
// as a hook for Claude Code (default) or the Antigravity CLI (-format agy),
// reading the hook input on stdin.
//
// Set it up in a project:
//
//	bus-hook install -server http://10.0.0.5:7070 -as frontend@teammate -project ~/code/app
//	bus-hook install -agent agy -server http://10.0.0.5:7070 -as frontend@teammate -project ~/code/app
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentbus/internal/client"
	"agentbus/internal/hook"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "install" {
		install(os.Args[2:])
		return
	}
	server := flag.String("server", env("AGENT_BUS_URL", "http://localhost:7070"), "busd base URL")
	as := flag.String("as", os.Getenv("AGENT_BUS_ID"), "agent identity")
	format := flag.String("format", "claude", "hook protocol: claude or agy")
	event := flag.String("event", "", "agy only: the hook event (PreInvocation, Stop, PreToolUse)")
	gate := flag.Bool("liaison-gate", false, "agy PreToolUse: allow only read-only and agent-bus tools")
	workerGate := flag.Bool("worker-gate", false, "agy PreToolUse: refuse shell commands outside -allow-command")
	var commands multiFlag
	flag.Var(&commands, "allow-command", "with -worker-gate: an allowed command prefix (repeatable)")
	flag.Parse()

	if *format == "agy" {
		runAgy(*server, *as, *event, *gate, *workerGate, commands)
		return
	}
	var in hook.Input
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil || *as == "" {
		return // never break the worker
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := newHook(*server, *as).Handle(ctx, in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bus-hook:", err)
		return
	}
	if out != nil {
		json.NewEncoder(os.Stdout).Encode(out)
	}
}

// runAgy always prints a JSON object: agy expects one, and treats a missing
// PreToolUse decision as a denial.
func runAgy(server, as, event string, gate, workerGate bool, commands []string) {
	var in hook.AgyInput
	json.NewDecoder(os.Stdin).Decode(&in)
	out := map[string]any{}
	defer func() { json.NewEncoder(os.Stdout).Encode(out) }()
	switch {
	case gate:
		// agy's own MCP tool schemas are the only files headless agy lets
		// it read without a rule.
		out = hook.LiaisonGate(in, filepath.Join(filepath.Dir(hook.AgySettingsPath()), "mcp"))
		return
	case workerGate:
		out = hook.WorkerGate(in, commands)
		return
	}
	if as == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := newHook(server, as).HandleAgy(ctx, event, in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bus-hook:", err)
		return
	}
	out = res
}

func newHook(server, as string) *hook.Hook {
	c := client.New(server, as)
	c.HTTP.Timeout = 3 * time.Second
	stateDir, err := os.UserCacheDir()
	if err != nil {
		stateDir = os.TempDir()
	}
	return &hook.Hook{Client: c, StateDir: filepath.Join(stateDir, "agent-bus", "hooks", sanitize(as))}
}

func install(args []string) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	server := fs.String("server", env("AGENT_BUS_URL", "http://localhost:7070"), "busd base URL")
	as := fs.String("as", os.Getenv("AGENT_BUS_ID"), "agent identity")
	project := fs.String("project", ".", "project to install into")
	agent := fs.String("agent", "claude", "worker agent: claude or agy")
	agySettings := fs.String("agy-settings", hook.AgySettingsPath(), "agy settings.json that receives the permission rules")
	var commands multiFlag
	fs.Var(&commands, "allow-command", `agy: a command prefix the worker may run headless, e.g. "npm test" (repeatable)`)
	fs.Parse(args)
	if *as == "" {
		fatal(fmt.Errorf("-as is required"))
	}
	self, err := os.Executable()
	fatal(err)
	self, err = filepath.EvalSymlinks(self)
	fatal(err)
	dir, err := filepath.Abs(*project)
	fatal(err)
	mcpBin := filepath.Join(filepath.Dir(self), "bus-mcp")
	base := []string{hook.ShellQuote(self), "-server", hook.ShellQuote(*server), "-as", hook.ShellQuote(*as)}

	switch *agent {
	case "claude":
		path, err := hook.Install(dir, strings.Join(base, " "))
		fatal(err)
		fmt.Printf("Installed %s hooks in %s\n\n", strings.Join(hook.Events, ", "), path)
		fmt.Printf("Give the worker the agent-bus tools too (run in %s):\n  claude mcp add agent-bus -- %s -server %s -as %s -role worker\n",
			dir, hook.ShellQuote(mcpBin), hook.ShellQuote(*server), hook.ShellQuote(*as))
		fmt.Println("\nRestart Claude Code (or run /hooks) to pick them up.")

	case "agy":
		cmd := func(event string) string {
			return strings.Join(append(base, "-format", "agy", "-event", event), " ")
		}
		gateArgs := []string{hook.ShellQuote(self), "-format", "agy", "-worker-gate"}
		for _, c := range commands {
			gateArgs = append(gateArgs, "-allow-command", hook.ShellQuote(c))
		}
		gateCmd := strings.Join(gateArgs, " ")
		pluginDir, err := hook.WriteAgyPlugin(dir, hook.AgyPlugin{
			MCPCommand: mcpBin,
			MCPArgs:    []string{"-server", *server, "-as", *as, "-role", "worker"},
			Hooks: map[string]any{
				"PreInvocation": []any{hook.AgyHook(cmd("PreInvocation"))},
				"Stop":          []any{hook.AgyHook(cmd("Stop"))},
				"PreToolUse":    []any{map[string]any{"matcher": "*", "hooks": []any{hook.AgyHook(gateCmd)}}},
			},
			Rules: hook.WorkerRules(*as),
		})
		fatal(err)
		rules := []string{hook.AgyMCPRule, fmt.Sprintf("write_file(%s)", dir)}
		for _, c := range commands {
			rules = append(rules, fmt.Sprintf("command(%s)", c))
		}
		added, err := hook.AddAgyPermissions(*agySettings, rules...)
		fatal(err)
		fmt.Printf("Installed the agent-bus plugin (MCP tools, PreInvocation and Stop hooks, rules) in %s\n", pluginDir)
		if len(added) > 0 {
			fmt.Printf("Added to %s permissions.allow (needed by headless agy):\n  %s\n", *agySettings, strings.Join(added, "\n  "))
		}
		fmt.Printf("\nAdd .agents/plugins/%s/ to the project's .gitignore; it carries your identity.\n", hook.AgyPluginName)
		fmt.Println("Headless agy ends a turn when it is denied a tool; grant the commands your worker needs with -allow-command.")

	default:
		fatal(fmt.Errorf("-agent must be claude or agy"))
	}
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' {
			return '_'
		}
		return r
	}, s)
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "bus-hook:", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
