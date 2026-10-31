// Package daemon is the per-laptop process: it follows the bus feed, keeps
// the rendered files in the repo up to date, starts a headless liaison when
// messages arrive for this agent, and tells the human when their liaison
// asks them something.
package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"agentbus/internal/bus"
	"agentbus/internal/client"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Runner runs one liaison session to completion.
type Runner interface {
	Run(ctx context.Context, prompt string) error
}

type Daemon struct {
	Client  *client.Client
	Repo    string // rendered files are written here
	Mandate string // mandate.md text, included in every liaison prompt
	Liaison Runner
	Log     *log.Logger

	// LiaisonTimeout bounds one liaison session.
	LiaisonTimeout time.Duration

	// Notify, when set, reports activity to the human outside the terminal
	// (for example the blob game): kind is "working", "attention" or "done".
	Notify func(kind, message string)

	human string
	kicks chan struct{}
}

// Run follows the feed until ctx ends, reconnecting on errors.
func (d *Daemon) Run(ctx context.Context) error {
	if d.LiaisonTimeout == 0 {
		d.LiaisonTimeout = 5 * time.Minute
	}
	d.kicks = make(chan struct{}, 1)
	agents, err := d.Client.Agents(ctx)
	if err != nil {
		return err
	}
	for _, a := range agents {
		if a.ID == d.Client.From {
			d.human = "human:" + a.Human
		}
	}
	if d.human == "" {
		return fmt.Errorf("agent %q is not registered on the bus; add it to bus.json", d.Client.From)
	}
	go d.liaisonLoop(ctx)

	since := int64(-1)
	for {
		// Catch up on anything missed while disconnected.
		d.sync(ctx)
		d.kick()
		err := d.follow(ctx, &since)
		if ctx.Err() != nil {
			return nil
		}
		d.Log.Printf("feed disconnected (%v); reconnecting in 2s", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

func (d *Daemon) follow(ctx context.Context, since *int64) error {
	conn, _, err := websocket.Dial(ctx, d.Client.FeedURL(*since), nil)
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(4 << 20) // events carry full contract snapshots
	d.Log.Printf("following %s as %s", d.Client.BaseURL, d.Client.From)
	for {
		var e bus.Event
		if err := wsjson.Read(ctx, conn, &e); err != nil {
			return err
		}
		*since = e.Seq
		d.handle(ctx, e)
	}
}

func (d *Daemon) handle(ctx context.Context, e bus.Event) {
	me := d.Client.From
	d.Log.Print(client.FormatEvent(e))
	switch {
	case e.Type == bus.Decided:
		d.sync(ctx)
		d.notify("done", "agent-bus: "+e.Thread+" decided")
	case e.Type == bus.Ask && e.To == d.human:
		d.Log.Printf("QUESTION FOR YOU: %s asks (thread %s): %s\n    answer with: bus answer %s %d \"...\"",
			e.From, e.Thread, e.Body, e.Thread, e.Seq)
		d.notify("attention", "agent-bus: your liaison has a question on "+e.Thread)
	case e.Type == bus.Escalate:
		d.Log.Printf("ESCALATED: %s needs the humans. Review: bus show %s; decide: bus resolve %s <proposal-seq>",
			e.Thread, e.Thread, e.Thread)
		d.notify("attention", "agent-bus: "+e.Thread+" needs the humans")
	case e.Type == bus.AssumptionBroken && e.To == me:
		d.Log.Printf("ASSUMPTION BROKEN on %s; your worker will be told to fix its code", e.Thread)
	}
	if bus.RoleLiaison.Sees(me, e) {
		d.kick()
	}
}

func (d *Daemon) notify(kind, message string) {
	if d.Notify != nil {
		d.Notify(kind, message)
	}
}

func (d *Daemon) kick() {
	select {
	case d.kicks <- struct{}{}:
	default: // a check is already pending
	}
}

// LiaisonAttempts is how many times a liaison is started for the same
// unacked messages. Headless agents occasionally end a run without acting,
// so one retry is allowed; after that the daemon waits for new activity, so a
// confused liaison can't loop.
const LiaisonAttempts = 2

// liaisonLoop runs one liaison at a time for whatever is in the inbox.
func (d *Daemon) liaisonLoop(ctx context.Context) {
	var lastHandled int64
	attempts := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.kicks:
		}
		inbox, err := d.Client.Inbox(ctx, bus.RoleLiaison)
		if err != nil {
			d.Log.Printf("liaison inbox: %v", err)
			continue
		}
		if len(inbox) == 0 {
			continue
		}
		last := inbox[len(inbox)-1].Seq
		if last != lastHandled {
			lastHandled, attempts = last, 0
		}
		if attempts >= LiaisonAttempts {
			d.Log.Printf("liaison left %d message(s) unacked after %d attempts; waiting for new activity", len(inbox), attempts)
			continue
		}
		attempts++
		if attempts > 1 {
			d.Log.Printf("liaison left %d message(s) unacked; retrying", len(inbox))
		} else {
			d.Log.Printf("%d message(s) for the liaison; starting it", len(inbox))
		}
		d.notify("working", "agent-bus: liaison negotiating")
		start := time.Now()
		runCtx, cancel := context.WithTimeout(ctx, d.LiaisonTimeout)
		err = d.Liaison.Run(runCtx, d.prompt(inbox))
		cancel()
		if err != nil {
			d.Log.Printf("liaison failed after %s: %v", time.Since(start).Round(time.Second), err)
		} else {
			d.Log.Printf("liaison finished in %s", time.Since(start).Round(time.Second))
		}
		d.kick() // pick up anything that arrived meanwhile
	}
}

func (d *Daemon) prompt(inbox []bus.Event) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "You are the agent-bus liaison for %s.\n\n", d.Client.From)
	if d.Mandate != "" {
		fmt.Fprintf(&b, "Your human's mandate:\n---\n%s\n---\n\n", d.Mandate)
	}
	fmt.Fprintf(&b, "New messages for you:\n")
	for _, e := range inbox {
		fmt.Fprintf(&b, "- %s\n", client.FormatEvent(e))
	}
	fmt.Fprintf(&b, `
Handle them now with the agent-bus tools. Use get_thread and get_contract for context; the repo is read-only context.
Then accept, counter, answer, ask your human (for anything on your mandate's "ask me" list), or escalate.
When done, ack seq %d and reply with one line saying what you did.
`, inbox[len(inbox)-1].Seq)
	return b.String()
}

// sync writes the rendered files into the repo as read-only files.
func (d *Daemon) sync(ctx context.Context) {
	files, err := d.Client.Render(ctx)
	if err != nil {
		d.Log.Printf("render: %v", err)
		return
	}
	for path, content := range files {
		if !filepath.IsLocal(path) {
			d.Log.Printf("render: refusing non-local path %q", path)
			continue
		}
		full := filepath.Join(d.Repo, path)
		changed, err := writeReadOnly(full, content)
		if err != nil {
			d.Log.Printf("render %s: %v", path, err)
		} else if changed {
			d.Log.Printf("updated %s", path)
		}
	}
}

// writeReadOnly replaces path with content (mode 0444) unless it already
// has that content.
func writeReadOnly(path, content string) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && string(old) == content {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp := path + ".tmp"
	os.Remove(tmp)
	if err := os.WriteFile(tmp, []byte(content), 0o444); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}
