// Package chat is a terminal chat with your coding agent (the worker) that
// also shows Agent Bus activity inline and lets the human answer their
// liaison and resolve escalations without leaving the conversation.
//
// The agent runs as one persistent headless session (agy stream-json), so
// the conversation keeps its context across messages. Contract decisions
// still reach it mid-task through the worker hooks.
package chat

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentbus/internal/bus"
	"agentbus/internal/client"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// AgyArgv runs agy as a persistent stream-json session that may edit files.
var AgyArgv = []string{"agy", "--mode", "accept-edits", "--input-format", "stream-json", "--output-format", "stream-json", "--print="}

type Chat struct {
	Client *client.Client // identity: the worker agent
	Human  string         // "human:<name>", for answers and picks
	Argv   []string       // agent command
	Dir    string         // project directory the agent works in
	In     io.Reader
	Out    io.Writer
	Color  bool

	mu        sync.Mutex
	agent     io.WriteCloser
	busy      bool // a turn is running
	streaming bool // agent text is mid-line
	seqThread map[int64]string
	nudged    int64 // last decision the idle agent was woken up for
	quiet     bool  // hide other agents' activity
}

// Run chats until the input ends, the user types /quit, or ctx ends.
func (c *Chat) Run(ctx context.Context) error {
	c.seqThread = map[int64]string{}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Dir = c.Dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", c.Argv[0], err)
	}
	c.agent = stdin
	agentDone := make(chan struct{})
	go func() { c.readAgent(stdout); close(agentDone) }()
	go c.readStderr(stderr)
	go c.followBus(ctx)
	go c.followActivity(ctx)

	c.banner(ctx)
	c.prompt()
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(c.In)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-agentDone:
			c.println(c.paint(red, "The agent session ended."))
			return cmd.Wait()
		case line, ok := <-lines:
			if !ok || strings.TrimSpace(line) == "/quit" {
				stdin.Close()
				cancel()
				return nil
			}
			c.input(ctx, strings.TrimSpace(line))
		}
	}
}

// ---- user input ----

func (c *Chat) input(ctx context.Context, line string) {
	if line == "" {
		c.prompt()
		return
	}
	if strings.HasPrefix(line, "/") {
		c.command(ctx, line)
		c.prompt()
		return
	}
	msg, _ := json.Marshal(map[string]any{"event": "user", "message": map[string]any{"content": line}})
	c.mu.Lock()
	queued := c.busy
	c.busy = true
	c.mu.Unlock()
	if _, err := c.agent.Write(append(msg, '\n')); err != nil {
		c.println(c.paint(red, "could not send to the agent: "+err.Error()))
		return
	}
	if queued {
		c.println(c.paint(dim, "(queued; the agent will read it after its current turn)"))
	}
}

func (c *Chat) command(ctx context.Context, line string) {
	fields := strings.Fields(line)
	switch fields[0] {
	case "/help":
		c.println(helpText)
	case "/answer":
		if len(fields) < 3 {
			c.println("usage: /answer <question-seq> <your answer>")
			return
		}
		seq, thread, err := c.lookup(ctx, fields[1])
		if err != nil {
			c.println(c.paint(red, err.Error()))
			return
		}
		res, err := c.as(c.Human).Act(ctx, thread, "answer", bus.AnswerReq{ReplyTo: seq, Answer: strings.Join(fields[2:], " ")})
		c.report(res, err, "Answer sent; your liaison takes it from here.")
	case "/pick":
		if len(fields) < 2 {
			c.println("usage: /pick <proposal-seq> [note]")
			return
		}
		seq, thread, err := c.lookup(ctx, fields[1])
		if err != nil {
			c.println(c.paint(red, err.Error()))
			return
		}
		res, err := c.as(c.Human).Act(ctx, thread, "resolve", bus.ResolveReq{Pick: seq, Note: strings.Join(fields[2:], " ")})
		c.report(res, err, "Escalation resolved.")
	case "/activity":
		c.mu.Lock()
		c.quiet = len(fields) > 1 && fields[1] == "off"
		quiet := c.quiet
		c.mu.Unlock()
		if quiet {
			c.println("Other agents' activity hidden. /activity on to show it again.")
		} else {
			c.println("Showing other agents' activity.")
		}
	case "/threads":
		ths, err := c.Client.Threads(ctx)
		if err != nil {
			c.println(c.paint(red, err.Error()))
			return
		}
		if len(ths) == 0 {
			c.println("No threads yet.")
		}
		for _, th := range ths {
			c.println(fmt.Sprintf("  %-10s %-22s %s", th.State, th.ID, th.Title))
		}
	case "/show":
		if len(fields) < 2 {
			c.println("usage: /show <thread>")
			return
		}
		th, evs, err := c.Client.Thread(ctx, fields[1])
		if err != nil {
			c.println(c.paint(red, err.Error()))
			return
		}
		c.println(c.paint(bold, fmt.Sprintf("%s (%s)", th.Title, th.State)))
		for _, e := range evs {
			c.remember(e)
			c.println("  " + client.FormatEvent(e))
			if e.Diff != "" {
				c.println(c.diff(e.Diff))
			}
		}
	default:
		c.println("Unknown command. " + helpText)
	}
}

const helpText = `Commands:
  /answer <seq> <text>   answer a question your liaison asked you
  /pick <seq> [note]     decide an escalated thread with that proposal
  /activity on|off       show or hide what the other agents are doing
  /threads               list threads
  /show <thread>         a thread's history with diffs
  /quit                  leave
Anything else is sent to your agent.`

func (c *Chat) as(id string) *client.Client {
	cl := *c.Client
	cl.From = id
	return &cl
}

func (c *Chat) report(res *bus.Result, err error, ok string) {
	if err != nil {
		c.println(c.paint(red, err.Error()))
		return
	}
	for _, e := range res.Events {
		c.remember(e)
	}
	c.println(c.paint(green, ok))
}

// lookup resolves an event seq to its thread, from the feed or the API.
func (c *Chat) lookup(ctx context.Context, s string) (int64, string, error) {
	seq, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("%q is not a message number", s)
	}
	c.mu.Lock()
	thread := c.seqThread[seq]
	c.mu.Unlock()
	if thread != "" {
		return seq, thread, nil
	}
	ths, err := c.Client.Threads(ctx)
	if err != nil {
		return 0, "", err
	}
	for _, th := range ths {
		_, evs, err := c.Client.Thread(ctx, th.ID)
		if err != nil {
			return 0, "", err
		}
		for _, e := range evs {
			c.remember(e)
			if e.Seq == seq {
				return seq, th.ID, nil
			}
		}
	}
	return 0, "", fmt.Errorf("no message #%d", seq)
}

func (c *Chat) remember(e bus.Event) {
	if e.Thread == "" {
		return
	}
	c.mu.Lock()
	c.seqThread[e.Seq] = e.Thread
	c.mu.Unlock()
}

// ---- agent output ----

type streamEvent struct {
	Event      string `json:"event"`
	StepUpdate struct {
		State     string `json:"state"`
		StepType  string `json:"step_type"`
		TextDelta string `json:"text_delta"`
		ToolName  string `json:"tool_name"`
		ToolInfo  struct {
			Parameters map[string]any `json:"parameters"`
		} `json:"tool_info"`
	} `json:"step_update"`
	Result struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	} `json:"result"`
}

// toolDetailKeys are the tool parameters worth showing, most specific first.
var toolDetailKeys = []string{"ToolName", "CommandLine", "TargetFile", "AbsolutePath", "Path"}

func (c *Chat) readAgent(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var ev streamEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Event {
		case "step_update":
			c.step(ev)
		case "result":
			c.mu.Lock()
			c.busy = false
			c.mu.Unlock()
			c.endStream()
			if ev.Result.Status != "SUCCESS" && ev.Result.Error != "" {
				c.println(c.paint(red, "agent error: "+ev.Result.Error))
			}
			c.prompt()
		}
	}
}

func (c *Chat) step(ev streamEvent) {
	u := ev.StepUpdate
	switch u.StepType {
	case "agent_response":
		if u.TextDelta == "" {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if !c.streaming {
			fmt.Fprint(c.Out, c.clearLine()+c.paint(cyan, "agent> "))
			c.streaming = true
		}
		fmt.Fprint(c.Out, u.TextDelta)
		if strings.HasSuffix(u.TextDelta, "\n") {
			c.streaming = false
		}
	case "system_message":
		// The worker hook injected an agent-bus update into the conversation.
		if u.State == "DONE" {
			c.println(c.paint(green, "  [bus] update delivered to the agent"))
		}
	case "tool":
		detail := u.ToolName
		for _, k := range toolDetailKeys {
			if v, ok := u.ToolInfo.Parameters[k].(string); ok && v != "" {
				detail += " " + v
				break
			}
		}
		switch u.State {
		case "ACTIVE":
			c.println(c.paint(dim, "  [tool] "+detail))
		case "ERROR":
			c.println(c.paint(yellow, "  [tool] "+detail+" failed"))
		}
	}
}

func (c *Chat) endStream() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.streaming {
		fmt.Fprintln(c.Out)
		c.streaming = false
	}
}

func (c *Chat) readStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			c.println(c.paint(dim, "  [agent] "+line))
		}
	}
}

// ---- bus feed ----

func (c *Chat) banner(ctx context.Context) {
	c.println(c.paint(bold, fmt.Sprintf("Agent Bus chat as %s (human %s). Type /help for commands.", c.Client.From, c.Human)))
	ths, err := c.Client.Threads(ctx)
	if err != nil {
		c.println(c.paint(red, "bus unreachable: "+err.Error()))
		return
	}
	for _, th := range ths {
		if th.AwaitingHuman == c.Client.From || th.State == bus.StateEscalated {
			_, evs, err := c.Client.Thread(ctx, th.ID)
			if err != nil {
				continue
			}
			for _, e := range evs {
				c.remember(e)
				if c.needsMe(e, evs) {
					c.busEvent(e)
				}
			}
		}
	}
}

// needsMe reports an unanswered question to this human, or an escalation.
func (c *Chat) needsMe(e bus.Event, thread []bus.Event) bool {
	if e.Type == bus.Escalate {
		return true
	}
	if e.Type != bus.Ask || e.To != c.Human {
		return false
	}
	for _, a := range thread {
		if a.Type == bus.Answer && a.ReplyTo == e.Seq {
			return false
		}
	}
	return true
}

func (c *Chat) followBus(ctx context.Context) {
	since := int64(-1)
	for ctx.Err() == nil {
		conn, _, err := websocket.Dial(ctx, c.Client.FeedURL(since), nil)
		if err == nil {
			conn.SetReadLimit(4 << 20)
			for {
				var e bus.Event
				if err := wsjson.Read(ctx, conn, &e); err != nil {
					break
				}
				since = e.Seq
				c.remember(e)
				c.busEvent(e)
				c.wake(e)
			}
			conn.CloseNow()
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

// wake starts a turn when a decision the worker must act on arrives while
// it is idle. Mid-turn, the worker hook delivers it instead; either way the
// hook injects the details on the agent's next model call.
func (c *Chat) wake(e bus.Event) {
	if (e.Type != bus.Decided && e.Type != bus.AssumptionBroken) || !bus.RoleWorker.Sees(c.Client.From, e) {
		return
	}
	c.mu.Lock()
	if c.busy || e.Seq <= c.nudged {
		c.mu.Unlock()
		return
	}
	c.busy, c.nudged = true, e.Seq
	c.mu.Unlock()
	c.println(c.paint(green, "  [bus] your agent is idle; asking it to apply the update"))
	msg, _ := json.Marshal(map[string]any{"event": "user", "message": map[string]any{
		"content": fmt.Sprintf("[agent-bus] Contract update #%d arrived. Apply it to the code as described in the agent-bus update, then ack it.", e.Seq)}})
	if _, err := c.agent.Write(append(msg, '\n')); err != nil {
		c.println(c.paint(red, "could not send to the agent: "+err.Error()))
	}
}

// followActivity shows what the other agents (and this person's own
// liaison) are doing as it happens. This worker's own tool calls are already
// shown from its session.
func (c *Chat) followActivity(ctx context.Context) {
	for ctx.Err() == nil {
		conn, _, err := websocket.Dial(ctx, c.Client.ActivityURL(0), nil)
		if err == nil {
			for {
				var a client.Activity
				if err := wsjson.Read(ctx, conn, &a); err != nil {
					break
				}
				c.mu.Lock()
				quiet := c.quiet
				c.mu.Unlock()
				if quiet || (a.From == c.Client.From && a.Role == "worker") {
					continue
				}
				color := blue
				if a.Role == "liaison" {
					color = yellow
				}
				c.println(c.paint(dim, "  ") + c.paint(color, fmt.Sprintf("[%s %s]", a.From, a.Role)) + " " + c.paint(dim, a.Text))
				c.prompt()
			}
			conn.CloseNow()
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *Chat) busEvent(e bus.Event) {
	line := c.paint(dim, "  [bus] ") + c.paint(typeColor(e.Type), client.FormatEvent(e))
	switch {
	case e.Type == bus.Ask && e.To == c.Human:
		line = c.paint(yellow+bold, fmt.Sprintf("  QUESTION FOR YOU (#%d) from %s:", e.Seq, e.From)) + "\n    " + e.Body +
			"\n" + c.paint(dim, fmt.Sprintf("    reply with: /answer %d <your answer>", e.Seq))
	case e.Type == bus.Escalate:
		line += "\n" + c.paint(dim, fmt.Sprintf("    review with /show %s, then /pick <proposal-seq>", e.Thread))
	case e.Type == bus.Decided && e.Rev != nil:
		line += "\n" + c.paint(dim, fmt.Sprintf("    contracts/%s is now rev %d", e.Contract, *e.Rev))
	}
	c.println(line)
	c.prompt()
}

// ---- terminal output ----

const (
	bold   = "\033[1m"
	dim    = "\033[2m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	blue   = "\033[34m"
	cyan   = "\033[36m"
)

func typeColor(t bus.EventType) string {
	switch t {
	case bus.Propose:
		return blue
	case bus.Counter:
		return yellow
	case bus.Accept, bus.Decided, bus.Resolve:
		return green
	case bus.Escalate, bus.AssumptionBroken:
		return red
	}
	return ""
}

func (c *Chat) paint(code, s string) string {
	if !c.Color || code == "" {
		return s
	}
	return code + s + "\033[0m"
}

func (c *Chat) diff(d string) string {
	var out []string
	for _, l := range strings.Split(strings.TrimRight(d, "\n"), "\n") {
		if strings.HasPrefix(l, "---") || strings.HasPrefix(l, "+++") {
			continue
		}
		switch {
		case strings.HasPrefix(l, "+"):
			l = c.paint(green, l)
		case strings.HasPrefix(l, "-"):
			l = c.paint(red, l)
		}
		out = append(out, "      "+l)
	}
	return strings.Join(out, "\n")
}

// clearLine erases a pending prompt so async output starts on a clean line.
func (c *Chat) clearLine() string {
	if !c.Color {
		return ""
	}
	return "\r\033[K"
}

// println prints a full line, breaking out of any agent text mid-line.
func (c *Chat) println(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.streaming {
		fmt.Fprintln(c.Out)
		c.streaming = false
	}
	fmt.Fprint(c.Out, c.clearLine()+s+"\n")
}

// prompt shows the input prompt when the agent isn't mid-turn.
func (c *Chat) prompt() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.busy && !c.streaming {
		fmt.Fprint(c.Out, c.clearLine()+c.paint(bold, "you> "))
	}
}
