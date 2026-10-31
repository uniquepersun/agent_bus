// Command bus-mcp is the Agent Bus MCP server (stdio). Both sides run the
// same binary, configured with their identity and role:
//
//	bus-mcp -server http://10.0.0.5:7070 -as backend@abhay -role liaison -mandate mandate.md
//
// Flags fall back to AGENT_BUS_URL, AGENT_BUS_ID, AGENT_BUS_ROLE and
// AGENT_BUS_MANDATE. Logs go to stderr; stdout belongs to MCP.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"time"

	"agentbus/internal/bus"
	"agentbus/internal/client"
	"agentbus/internal/mcptools"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetPrefix("bus-mcp: ")

	server := flag.String("server", env("AGENT_BUS_URL", "http://localhost:7070"), "busd base URL")
	as := flag.String("as", os.Getenv("AGENT_BUS_ID"), "agent identity, e.g. backend@abhay")
	role := flag.String("role", env("AGENT_BUS_ROLE", string(bus.RoleWorker)), "worker or liaison")
	mandate := flag.String("mandate", os.Getenv("AGENT_BUS_MANDATE"), "mandate.md to upload on start (optional)")
	flag.Parse()

	if *as == "" {
		log.Fatal("-as (or AGENT_BUS_ID) is required")
	}
	r := bus.Role(*role)
	if r != bus.RoleWorker && r != bus.RoleLiaison {
		log.Fatalf("-role must be %q or %q", bus.RoleWorker, bus.RoleLiaison)
	}
	c := client.New(*server, *as)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if *mandate != "" {
		text, err := os.ReadFile(*mandate)
		if err != nil {
			log.Fatal(err)
		}
		if err := c.SetMandate(ctx, *as, string(text)); err != nil {
			log.Printf("uploading mandate: %v", err)
		}
	}
	// Contract names become enums in the tool schemas. If the server is down,
	// start anyway; tool calls will report it.
	var names []string
	if cs, err := c.Contracts(ctx); err != nil {
		log.Printf("listing contracts: %v", err)
	} else {
		for _, ct := range cs {
			names = append(names, ct.Name)
		}
	}
	cancel()

	s := mcptools.NewServer(mcptools.Config{Client: c, Role: r, Contracts: names})
	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
