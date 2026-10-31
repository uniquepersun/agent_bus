# Instructions

How to build, run, and test Agent Bus. For what the project is and how it works, see [README.md](README.md).

## Requirements

- Go 1.27 or newer, as declared in `go.mod` (Go 1.21+ downloads the right toolchain automatically)
- A coding agent CLI for workers and liaisons: Claude Code (`claude`) or the Antigravity CLI (`agy`), signed in
- Optional: Codex CLI, used as a liaison via `-liaison "codex exec"`

## One-command setup

`demo.sh` wraps everything below. It builds the binaries when run from source and uses the prebuilt ones in a release archive.

| Command | What it does |
|---|---|
| `./demo.sh server [-port 7070] [-fresh]` | Starts the bus and dashboard and prints the address to give each laptop |
| `./demo.sh laptop -as ID -project DIR [-server URL] [-agent agy\|claude] [-mandate FILE] [-allow-command CMD]` | Installs the worker integration into the project, starts the daemon in the background, then opens `bus chat` (agy) or Claude Code |
| `./demo.sh rehearse` | Runs the bus and both sides on one machine in two Terminal windows, with a sample frontend project in `~/agentbus-demo` |

The first time it installs for agy, the script adds the permission rules headless agy needs to `~/.gemini/antigravity-cli/settings.json` (see section 6).

## Build

```sh
go build -o bin/ ./cmd/...
```

This produces five binaries in `bin/`:

| Binary | Runs on | Purpose |
|---|---|---|
| `busd` | the server | Ledger, state machine, REST API, WebSocket feed, renderer, dashboard |
| `bus-daemon` | each laptop | Follows the feed, writes rendered files into the repo, starts liaisons |
| `bus-mcp` | each laptop | MCP server for the worker and the liaison |
| `bus-hook` | each laptop | Claude Code hooks for the worker |
| `bus` | each laptop | Command-line interface for humans |

## Configuration

`bus.json` (read by `busd`) defines the agents, their humans, the contracts and their owners, and the escalation timeout:

```json
{
  "escalation_timeout": "2m",
  "agents": [
    {"id": "backend@abhay", "human": "abhay", "mandate_file": "mandates/backend.md"},
    {"id": "frontend@teammate", "human": "teammate", "mandate_file": "mandates/frontend.md"}
  ],
  "contracts": [
    {"name": "openapi.yaml", "owner": "backend@abhay", "seed_file": "seed/openapi.yaml"}
  ]
}
```

Contracts are seeded at revision 0 from `seed_file` on first start; later starts keep the ledger's history. Delete `bus.db` for a fresh ledger.

The other binaries take `-server` and `-as`, or the environment variables `AGENT_BUS_URL` and `AGENT_BUS_ID`. The `bus` CLI takes `-human` or `AGENT_BUS_HUMAN`.

## Test

### 1. Automated tests

No LLM calls and no tokens:

```sh
go test -race ./...
```

Notable tests:

- `TestDemoTrace` (bus): the full negotiation from SPEC.md against the state machine.
- `TestDemoTraceOverMCP` (mcptools): the same trace through real MCP sessions.
- `TestDaemonsRunDemoTrace` (daemon): two daemons with scripted liaisons, including rendered files.
- `TestWorkerHooks` (hook): mid-task injection, once-only delivery, and the Stop hook's limits.

### 2. Dashboard

Start the server and open http://localhost:7070:

```sh
./bin/busd
```

The dashboard shows threads in columns by state, a live event feed, agents with their mandates, and contracts with their current revision. Select a thread to see its timeline and diffs. Choose your identity under "Acting as" to answer a question addressed to you or to pick a proposal on an escalated thread. A thread can be linked directly with `http://localhost:7070/#thread=<thread-id>`.

### 3. Full negotiation on one laptop

Use four terminals in this repository. Each daemon gets its own scratch directory standing in for a project repo.

```sh
# Terminal 1: server, then open http://localhost:7070
./bin/busd

# Terminal 2: frontend daemon
mkdir -p /tmp/fe && ./bin/bus-daemon -as frontend@teammate -mandate mandates/frontend.md -repo /tmp/fe

# Terminal 3: backend daemon
mkdir -p /tmp/be && ./bin/bus-daemon -as backend@abhay -mandate mandates/backend.md -repo /tmp/be

# Terminal 4: act as the backend worker and propose a rename
curl -X POST localhost:7070/api/threads/users-shape/propose -H 'X-Bus-From: backend@abhay' -d '{
  "title": "Shape of /users response", "contract": "openapi.yaml", "base_rev": 0, "blocking": true,
  "reason": "DB column renamed for i18n",
  "edits": [{"old": "        name:\n          type: string", "new": "        display_name:\n          type: string"},
            {"old": "required: [id, name, email]", "new": "required: [id, display_name, email]"}]}'
```

Expected sequence:

1. Within about 30 seconds the frontend liaison decides that a rename needs its human's approval. Terminal 2 prints `QUESTION FOR YOU: frontend@teammate asks ...` with the answer command, and the dashboard shows "1 thread needs a human".
2. Answer from the dashboard (acting as `teammate`) or the CLI:
   ```sh
   ./bin/bus -human teammate answer users-shape 2 "fine, but keep name for one release"
   ```
3. The frontend liaison counters with an additive change, and the backend liaison accepts it under its mandate.
4. Both daemons print `updated contracts/openapi.yaml`, and `/tmp/fe/DECISIONS.md` records the decision.

Useful commands: `./bin/bus threads`, `./bin/bus show users-shape`, `./bin/bus tail`. Full liaison transcripts are written to `<repo>/.agentbus/logs/`.

To test escalation, counter back and forth until a third counter (or call `escalate`), then pick a proposal on the dashboard or run `./bin/bus -human abhay resolve <thread> <proposal-seq>`. If no human resolves it within `escalation_timeout`, the contract owner's latest proposal wins.

### 4. Worker hooks on one laptop

With `busd` running, set up a scratch project as the frontend worker:

```sh
mkdir -p /tmp/app && cd /tmp/app
/abs/path/bin/bus-hook install -as frontend@teammate -project .
claude mcp add agent-bus -- /abs/path/bin/bus-mcp -as frontend@teammate -role worker
claude
```

Give Claude Code a multi-step task. While it works, propose and accept a contract change (the `curl` above as backend, then `accept` as frontend). After its next tool call the worker receives the change, finishes its current step, updates its code, and acknowledges the change. Hook state is kept per session under `~/Library/Caches/agent-bus/hooks/`.

### 5. Two laptops

1. **Server laptop:** run `./bin/busd -addr 0.0.0.0:7070` and find its LAN address with `ipconfig getifaddr en0`. Open the dashboard on this machine for the demo screen.
2. **Each laptop:** set `export AGENT_BUS_URL=http://<server-ip>:7070`, then run the daemon in the real project repository:
   ```sh
   ./bin/bus-daemon -as backend@abhay -mandate mandates/backend.md -repo ~/code/app
   ```
3. **Each worker** (the person's normal Claude Code session), from the project repository:
   ```sh
   /abs/path/bin/bus-hook install -server http://<server-ip>:7070 -as backend@abhay
   claude mcp add agent-bus -- /abs/path/bin/bus-mcp -server http://<server-ip>:7070 -as backend@abhay -role worker
   ```
   Restart Claude Code, then ask for a change that needs a contract change, for example: "Rename the users.name column to display_name and propose the API change on the agent bus."

Add `.agentbus/` to the project repository's `.gitignore`. The hooks are written to `.claude/settings.local.json`, which is personal and not committed.

### 6. Antigravity CLI (agy) and the terminal chat

Each laptop can use `agy` instead of Claude Code for both the worker and the liaison. This is also the setup for the terminal chat demo, where each person works in a single window.

1. **Install the worker plugin** in the project repository (run once per laptop):
   ```sh
   /abs/path/bin/bus-hook install -agent agy -server http://<server-ip>:7070 -as frontend@teammate -project ~/code/app
   ```
   This writes `.agents/plugins/agent-bus/` into the project (MCP tools, hooks, rules, and a gate that refuses shell commands outside the `-allow-command` list with directions, so the agent carries on instead of its turn ending) and adds two permission rules to `~/.gemini/antigravity-cli/settings.json`, which headless agy requires: `mcp(agent-bus_agent-bus/*)` and `write_file(<project>)`. Headless agy ends a turn when it is denied a tool, so also grant the commands your worker needs, for example `-allow-command "npm test" -allow-command "grep"`. Add `.agents/plugins/agent-bus/` to the project's `.gitignore`.

2. **Run the daemon with agy liaisons:**
   ```sh
   ./bin/bus-daemon -liaison agy -as frontend@teammate -mandate mandates/frontend.md -repo ~/code/app
   ```
   Each liaison runs from its own workspace under `~/Library/Caches/agent-bus/liaison/`, so it loads only the liaison plugin. A `PreToolUse` hook limits it to the agent-bus tools; it reads contracts and history through the bus, not the repository. If a liaison run ends without handling its messages, the daemon retries it once. Expect one to two minutes per agy liaison turn.

3. **Open the chat** (the demo window):
   ```sh
   ./bin/bus chat -server http://<server-ip>:7070 -as frontend@teammate -dir ~/code/app
   ```
   Type to talk to your agent. Agent Bus activity appears inline; `[bus] update delivered to the agent` marks the moment a decision reaches it. If the agent is idle when a decision lands, the chat starts a turn so it applies the change right away. Commands: `/answer <seq> <text>`, `/pick <seq>`, `/threads`, `/show <thread>`, `/help`, `/quit`.

A demonstration with two laptops: the backend person asks their agent in `bus chat` to rename `users.name` to `display_name` and propose the API change. The frontend person's chat shows the proposal, then `QUESTION FOR YOU` from their liaison; they reply with `/answer`. The liaisons settle the counter-proposal, both chats show `DECIDED`, and the frontend agent updates its code to match. Keep the dashboard on the projector throughout.

### 7. Playing blob while the agents work

Install [blob](https://github.com/uniquepersun/blob) (`brew tap uniquepersun/blob https://github.com/uniquepersun/blob && brew install uniquepersun/blob/blob`) and start the game in its own terminal tab with `blob`. Run the daemon with `-blob` (`demo.sh laptop` adds it when `blob` is on the PATH):

```sh
./bin/bus-daemon -blob -liaison agy -as frontend@teammate -mandate mandates/frontend.md -repo ~/code/app
```

The daemon sends the game `working` while your liaison negotiates, `attention` when your liaison has a question for you or a thread escalates (the game pauses with a card naming the thread), and `done` when a decision lands. Answer in `bus chat` with `/answer`, then go back to the game. With no game running, the notifications are skipped. For pauses when your own worker agent needs permission, use blob's own wrapper or hooks (`blob agy`, `blob install claude`).

## Troubleshooting

- **A liaison never starts:** check that the daemon's `-as` identity is listed in `bus.json` and that the daemon prints `following <url>`.
- **A liaison leaves messages unhandled:** the daemon does not restart it for the same messages; it waits for new activity. Read the transcript in `.agentbus/logs/`.
- **The worker does not see updates:** run `/hooks` in Claude Code to confirm the three hooks are loaded, and check that `bus-mcp` is listed under `/mcp`.
- **agy turns end with no output:** headless agy was denied a tool. The chat shows the notice as `[agent] ...`; add the rule it names, for example with `bus-hook install -agent agy -allow-command <prefix>`.
- **Proposal rejected as stale:** the contract moved on. Read it again with `get_contract` and propose against the new revision.
