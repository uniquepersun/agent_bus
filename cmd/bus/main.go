// Command bus is the human's CLI for the Agent Bus.
//
//	bus threads                          list threads and their states
//	bus show <thread>                    a thread's history with diffs
//	bus answer <thread> <ask-seq> <text> answer your liaison's question
//	bus resolve <thread> <seq> [note]    pick a proposal on an escalated thread
//	bus contract <name> [rev]            print a contract
//	bus tail                             follow the live event feed
//
// Set AGENT_BUS_URL and AGENT_BUS_HUMAN (your name, e.g. abhay) or pass
// -server and -human.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"agentbus/internal/bus"
	"agentbus/internal/client"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func main() {
	server := flag.String("server", env("AGENT_BUS_URL", "http://localhost:7070"), "busd base URL")
	human := flag.String("human", os.Getenv("AGENT_BUS_HUMAN"), "your name; you act as human:<name>")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: bus [-server URL] [-human NAME] threads|show|answer|resolve|contract|tail ...")
		flag.PrintDefaults()
	}
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	c := client.New(*server, "human:"+*human)
	ctx := context.Background()
	needHuman := func() {
		if *human == "" {
			fail(fmt.Errorf("set AGENT_BUS_HUMAN or -human to act as a human"))
		}
	}

	switch cmd, rest := args[0], args[1:]; cmd {
	case "threads":
		ths, err := c.Threads(ctx)
		fail(err)
		for _, th := range ths {
			extra := ""
			if th.AwaitingHuman != "" {
				extra = "  (waiting on " + th.AwaitingHuman + "'s human)"
			}
			fmt.Printf("%-10s %-24s %-14s %s%s\n", th.State, th.ID, th.Contract, th.Title, extra)
		}
	case "show":
		need(rest, 1, "show <thread>")
		th, evs, err := c.Thread(ctx, rest[0])
		fail(err)
		fmt.Printf("%s — %s (%s, counters %d/%d)\n\n", th.ID, th.Title, th.State, th.CounterCount, bus.MaxCounters)
		for _, e := range evs {
			fmt.Println(client.FormatEvent(e))
			if e.Diff != "" {
				fmt.Println(indent(e.Diff))
			}
		}
	case "answer":
		need(rest, 3, "answer <thread> <ask-seq> <text>")
		needHuman()
		seq := parseSeq(rest[1])
		res, err := c.Act(ctx, rest[0], "answer", bus.AnswerReq{ReplyTo: seq, Answer: strings.Join(rest[2:], " ")})
		fail(err)
		printResult(res)
	case "resolve":
		need(rest, 2, "resolve <thread> <proposal-seq> [note]")
		needHuman()
		res, err := c.Act(ctx, rest[0], "resolve", bus.ResolveReq{Pick: parseSeq(rest[1]), Note: strings.Join(rest[2:], " ")})
		fail(err)
		printResult(res)
	case "contract":
		need(rest, 1, "contract <name> [rev]")
		rev := -1
		if len(rest) > 1 {
			rev = int(parseSeq(rest[1]))
		}
		ct, err := c.Contract(ctx, rest[0], rev)
		fail(err)
		fmt.Printf("# %s rev %d (owner %s)\n%s", ct.Name, ct.Rev, ct.Owner, ct.Content)
	case "tail":
		conn, _, err := websocket.Dial(ctx, c.FeedURL(0), nil)
		fail(err)
		conn.SetReadLimit(4 << 20)
		for {
			var e bus.Event
			fail(wsjson.Read(ctx, conn, &e))
			fmt.Println(client.FormatEvent(e))
		}
	default:
		flag.Usage()
		os.Exit(2)
	}
}

func printResult(res *bus.Result) {
	for _, e := range res.Events {
		fmt.Println(client.FormatEvent(e))
	}
	if res.Thread != nil {
		fmt.Printf("thread %s is now %s\n", res.Thread.ID, res.Thread.State)
	}
}

func need(args []string, n int, usage string) {
	if len(args) < n {
		fail(fmt.Errorf("usage: bus %s", usage))
	}
}

func parseSeq(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
	if err != nil {
		fail(fmt.Errorf("%q is not a number", s))
	}
	return n
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}

func fail(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "bus:", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
