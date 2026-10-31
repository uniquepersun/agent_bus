// Command bus-hook is the Claude Code hook for agent-bus workers. Claude
// Code runs it with the hook input on stdin; it dispatches on
// hook_event_name (PostToolUse, UserPromptSubmit, Stop).
//
// Set it up in a project (writes .claude/settings.local.json):
//
//	bus-hook install -server http://10.0.0.5:7070 -as frontend@teammate -project ~/code/app
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
	flag.Parse()

	var in hook.Input
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil || *as == "" {
		return // never break the worker
	}
	c := client.New(*server, *as)
	c.HTTP.Timeout = 3 * time.Second
	stateDir, err := os.UserCacheDir()
	if err != nil {
		stateDir = os.TempDir()
	}
	h := &hook.Hook{Client: c, StateDir: filepath.Join(stateDir, "agent-bus", "hooks", sanitize(*as))}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := h.Handle(ctx, in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bus-hook:", err)
		return
	}
	if out != nil {
		json.NewEncoder(os.Stdout).Encode(out)
	}
}

func install(args []string) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	server := fs.String("server", env("AGENT_BUS_URL", "http://localhost:7070"), "busd base URL")
	as := fs.String("as", os.Getenv("AGENT_BUS_ID"), "agent identity")
	project := fs.String("project", ".", "project whose .claude/settings.local.json gets the hooks")
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

	command := strings.Join([]string{hook.ShellQuote(self), "-server", hook.ShellQuote(*server), "-as", hook.ShellQuote(*as)}, " ")
	path, err := hook.Install(dir, command)
	fatal(err)
	fmt.Printf("Installed %s hooks in %s\n\n", strings.Join(hook.Events, ", "), path)
	fmt.Printf("Give the worker the agent-bus tools too (run in %s):\n  claude mcp add agent-bus -- %s -server %s -as %s -role worker\n",
		dir, hook.ShellQuote(filepath.Join(filepath.Dir(self), "bus-mcp")), hook.ShellQuote(*server), hook.ShellQuote(*as))
	fmt.Println("\nRestart Claude Code (or run /hooks) to pick them up.")
}

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
