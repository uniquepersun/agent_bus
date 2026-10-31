// Command busd is the central Agent Bus server: SQLite ledger, REST API and
// WebSocket event feed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"agentbus/internal/bus"
	"agentbus/internal/server"
)

type config struct {
	EscalationTimeout string `json:"escalation_timeout"`
	Agents            []struct {
		ID          string `json:"id"`
		Human       string `json:"human"`
		MandateFile string `json:"mandate_file"`
	} `json:"agents"`
	Contracts []struct {
		Name     string `json:"name"`
		Owner    string `json:"owner"`
		SeedFile string `json:"seed_file"`
	} `json:"contracts"`
}

func main() {
	addr := flag.String("addr", ":7070", "listen address")
	dbPath := flag.String("db", "bus.db", "SQLite ledger path")
	cfgPath := flag.String("config", "bus.json", "agents and contracts config")
	flag.Parse()

	b, err := bus.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer b.Close()
	if err := seed(b, *cfgPath); err != nil {
		log.Fatalf("seed from %s: %v", *cfgPath, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				evs, err := b.SweepEscalations()
				if err != nil {
					log.Printf("escalation sweep: %v", err)
				}
				for _, e := range evs {
					log.Printf("escalation timeout: #%d %s %s", e.Seq, e.Type, e.Thread)
				}
			}
		}
	}()

	srv := &http.Server{Addr: *addr, Handler: server.New(b)}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("busd listening on %s (ledger %s, escalation timeout %s)", *addr, *dbPath, b.EscalationTimeout)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// seed registers agents and contracts from the config. Paths in the config
// are relative to the config file. Existing contracts keep their history.
func seed(b *bus.Bus, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	if cfg.EscalationTimeout != "" {
		if b.EscalationTimeout, err = time.ParseDuration(cfg.EscalationTimeout); err != nil {
			return err
		}
	}
	dir := filepath.Dir(path)
	read := func(rel string) (string, error) {
		if rel == "" {
			return "", nil
		}
		data, err := os.ReadFile(filepath.Join(dir, rel))
		return string(data), err
	}
	for _, a := range cfg.Agents {
		mandate, err := read(a.MandateFile)
		if err != nil {
			return err
		}
		if err := b.UpsertAgent(a.ID, a.Human, mandate); err != nil {
			return err
		}
	}
	for _, c := range cfg.Contracts {
		content, err := read(c.SeedFile)
		if err != nil {
			return err
		}
		if err := b.SeedContract(c.Name, c.Owner, content); err != nil {
			return err
		}
	}
	return nil
}
