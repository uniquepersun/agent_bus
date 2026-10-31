// Command bus-daemon runs on each laptop. It follows the bus, keeps
// DECISIONS.md and contracts/ in the repo up to date (read-only), and starts
// a headless liaison whenever messages arrive for this agent.
//
//	bus-daemon -server http://10.0.0.5:7070 -as frontend@teammate -mandate mandate.md -repo ~/code/app
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"agentbus/internal/client"
	"agentbus/internal/daemon"
	"agentbus/internal/hook"
)

func main() {
	server := flag.String("server", env("AGENT_BUS_URL", "http://localhost:7070"), "busd base URL")
	as := flag.String("as", os.Getenv("AGENT_BUS_ID"), "agent identity, e.g. backend@abhay")
	mandatePath := flag.String("mandate", os.Getenv("AGENT_BUS_MANDATE"), "mandate.md for this agent")
	repo := flag.String("repo", ".", "repo to write DECISIONS.md and contracts/ into; liaisons run here")
	liaisonCmd := flag.String("liaison", "claude", `"claude", "agy", or a command that takes the prompt as its last argument (e.g. "codex exec")`)
	mcpBin := flag.String("mcp-bin", "", "bus-mcp executable for claude and agy liaisons (default: next to this binary)")
	timeout := flag.Duration("liaison-timeout", 5*time.Minute, "max duration of one liaison session")
	blob := flag.Bool("blob", false, "tell a running blob game (github.com/uniquepersun/blob) when your liaison is negotiating, needs you, or a decision lands")
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
	// The liaison's final reply goes to this terminal and the activity stream.
	liaisonOut := io.MultiWriter(os.Stderr, activityWriter{c})
	var runner daemon.Runner
	switch *liaisonCmd {
	case "claude":
		runner = daemon.ClaudeRunner{Bin: "claude", MCPBin: *mcpBin, HookBin: sibling("bus-hook"), Server: *server, ID: *as,
			Dir: dir, LogDir: logDir, Out: liaisonOut}
	case "agy":
		cache, err := os.UserCacheDir()
		if err != nil {
			log.Fatal(err)
		}
		r := daemon.AgyRunner{Bin: "agy", MCPBin: *mcpBin, HookBin: sibling("bus-hook"), Server: *server, ID: *as,
			Workspace: filepath.Join(cache, "agent-bus", "liaison", strings.ReplaceAll(*as, "/", "_")), LogDir: logDir, Out: liaisonOut}
		if err := r.Prepare(); err != nil {
			log.Fatal(err)
		}
		runner = r
	default:
		runner = daemon.CommandRunner{Argv: strings.Fields(*liaisonCmd), Dir: dir, LogDir: logDir, Out: liaisonOut}
	}

	var notify func(kind, message string)
	if *blob {
		notify = blobNotifier()
	}

	d := &daemon.Daemon{
		Client:         c,
		Repo:           dir,
		Mandate:        mandate,
		Liaison:        runner,
		Log:            log.New(os.Stderr, "", log.Ltime),
		LiaisonTimeout: *timeout,
		Notify:         notify,
		Activity: func(kind, text string) {
			go hook.Report(c, "liaison", kind, text)
		},
	}
	if err := d.Run(ctx); err != nil {
		log.Fatal(err)
	}
}

// activityWriter shares the liaison's reply on the bus's activity stream.
type activityWriter struct{ c *client.Client }

func (w activityWriter) Write(p []byte) (int, error) {
	text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(p)), "liaison:"))
	if text != "" {
		go hook.Report(w.c, "liaison", "message", strings.Join(strings.Fields(text), " "))
	}
	return len(p), nil
}

// blobNotifier forwards activity to the blob game with `blob send`. With no
// game running, blob exits with an error, which is ignored.
func blobNotifier() func(kind, message string) {
	bin, err := exec.LookPath("blob")
	if err != nil {
		log.Fatal("-blob: blob is not on PATH; install it from https://github.com/uniquepersun/blob")
	}
	return func(kind, message string) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			exec.CommandContext(ctx, bin, "send", kind, message).Run()
		}()
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
