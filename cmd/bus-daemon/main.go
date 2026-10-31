// Command bus-daemon runs on each laptop. It follows the bus, keeps
// DECISIONS.md and contracts/ in the repo up to date (read-only), and starts
// a headless liaison whenever messages arrive for this agent.
//
//	bus-daemon -server http://10.0.0.5:7070 -as frontend@teammate -mandate mandate.md -repo ~/code/app
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"agentbus/internal/client"
	"agentbus/internal/daemon"
)

func main() {
	server := flag.String("server", env("AGENT_BUS_URL", "http://localhost:7070"), "busd base URL")
	as := flag.String("as", os.Getenv("AGENT_BUS_ID"), "agent identity, e.g. backend@abhay")
	mandatePath := flag.String("mandate", os.Getenv("AGENT_BUS_MANDATE"), "mandate.md for this agent")
	repo := flag.String("repo", ".", "repo to write DECISIONS.md and contracts/ into; liaisons run here")
	liaisonCmd := flag.String("liaison", "claude", `"claude", "agy", or a command that takes the prompt as its last argument (e.g. "codex exec")`)
	mcpBin := flag.String("mcp-bin", "", "bus-mcp executable for claude and agy liaisons (default: next to this binary)")
	timeout := flag.Duration("liaison-timeout", 5*time.Minute, "max duration of one liaison session")
	flag.Parse()

	if *as == "" {
		log.Fatal("-as (or AGENT_BUS_ID) is required")
	}
	dir, err := filepath.Abs(*repo)
	if err != nil {
		log.Fatal(err)
	}
	c := client.New(*server, *as)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var mandate string
	if *mandatePath != "" {
		raw, err := os.ReadFile(*mandatePath)
		if err != nil {
			log.Fatal(err)
		}
		mandate = string(raw)
		if err := c.SetMandate(ctx, *as, mandate); err != nil {
			log.Fatalf("uploading mandate: %v", err)
		}
	}

	logDir := filepath.Join(dir, ".agentbus", "logs")
	// sibling finds another agent-bus binary next to this one.
	sibling := func(name string) string {
		self, err := os.Executable()
		if err != nil {
			log.Fatal(err)
		}
		bin := filepath.Join(filepath.Dir(self), name)
		if _, err := os.Stat(bin); err != nil {
			log.Fatalf("%s not found at %s; build it with go build -o bin/ ./cmd/...", name, bin)
		}
		return bin
	}
	if *mcpBin == "" && (*liaisonCmd == "claude" || *liaisonCmd == "agy") {
		*mcpBin = sibling("bus-mcp")
	}
	var runner daemon.Runner
	switch *liaisonCmd {
	case "claude":
		runner = daemon.ClaudeRunner{Bin: "claude", MCPBin: *mcpBin, Server: *server, ID: *as, Dir: dir, LogDir: logDir, Out: os.Stderr}
	case "agy":
		cache, err := os.UserCacheDir()
		if err != nil {
			log.Fatal(err)
		}
		r := daemon.AgyRunner{Bin: "agy", MCPBin: *mcpBin, HookBin: sibling("bus-hook"), Server: *server, ID: *as,
			Workspace: filepath.Join(cache, "agent-bus", "liaison", strings.ReplaceAll(*as, "/", "_")), LogDir: logDir, Out: os.Stderr}
		if err := r.Prepare(); err != nil {
			log.Fatal(err)
		}
		runner = r
	default:
		runner = daemon.CommandRunner{Argv: strings.Fields(*liaisonCmd), Dir: dir, LogDir: logDir, Out: os.Stderr}
	}

	d := &daemon.Daemon{
		Client:         c,
		Repo:           dir,
		Mandate:        mandate,
		Liaison:        runner,
		Log:            log.New(os.Stderr, "", log.Ltime),
		LiaisonTimeout: *timeout,
	}
	if err := d.Run(ctx); err != nil {
		log.Fatal(err)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
