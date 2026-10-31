# Agent Bus

Coding agents on different machines negotiate shared contracts (`openapi.yaml`, `schema.sql`, `env.md`) under their humans' mandates. See [SPEC.md](SPEC.md) for the design.

| Binary | Runs on | Does |
|---|---|---|
| `busd` | the server | SQLite ledger, thread state machine, REST + WebSocket feed, renderer |
| `bus-daemon` | each laptop | follows the feed, writes `DECISIONS.md` + `contracts/` (read-only) into the repo, starts a headless liaison when messages arrive |
| `bus-mcp` | each laptop | MCP tools for the worker (your Claude Code session) and the liaison |
| `bus-hook` | each laptop | Claude Code hooks for the worker: decisions arrive mid-task, Stop holds unapplied changes |
| `bus` | each laptop | CLI for humans: answer your liaison, resolve escalations, inspect threads |

```sh
go build -o bin/ ./cmd/...
```

## Test

### 1. Automated tests (no LLM, no tokens)

```sh
go test -race ./...
```

`TestDaemonsRunDemoTrace` runs the SPEC demo trace with two daemons and scripted liaisons; `TestDemoTraceOverMCP` runs it through the MCP tools.

### 2. One laptop, real liaisons

Four terminals in this repo. Each daemon gets its own scratch repo directory.

```sh
# 1: server (rm bus.db for a fresh ledger)
./bin/busd

# 2: frontend daemon
mkdir -p /tmp/fe && ./bin/bus-daemon -as frontend@teammate -mandate mandates/frontend.md -repo /tmp/fe

# 3: backend daemon
mkdir -p /tmp/be && ./bin/bus-daemon -as backend@abhay -mandate mandates/backend.md -repo /tmp/be

# 4: play the backend worker: propose the rename
curl -X POST localhost:7070/api/threads/users-shape/propose -H 'X-Bus-From: backend@abhay' -d '{
  "title": "Shape of /users response", "contract": "openapi.yaml", "base_rev": 0, "blocking": true,
  "reason": "DB column renamed for i18n",
  "edits": [{"old": "        name:\n          type: string", "new": "        display_name:\n          type: string"},
            {"old": "required: [id, name, email]", "new": "required: [id, display_name, email]"}]}'
```

Expected: within ~30s terminal 2 prints `❓ frontend@teammate asks you ... answer with: bus answer users-shape 2 "..."`. Answer as the frontend human:

```sh
./bin/bus -human teammate answer users-shape 2 "fine, but keep name for one release"
```

The frontend liaison counters, the backend liaison accepts, both daemons print `📄 updated contracts/openapi.yaml`, and `/tmp/fe/DECISIONS.md` shows the decision. Inspect with `./bin/bus threads`, `./bin/bus show users-shape`, `./bin/bus tail`. Full liaison transcripts: `<repo>/.agentbus/logs/`.

To exercise escalation, have both sides counter until the third counter (or call `escalate`), then `./bin/bus -human abhay resolve <thread> <proposal-seq>`. If nobody resolves within `escalation_timeout` (bus.json, default 2m), the contract owner's latest proposal wins.

### 3. Worker hooks on one laptop

With `busd` running, install the hooks into any scratch project (`bus-hook install -as frontend@teammate -project /tmp/app`), start Claude Code there with the MCP server added, give it a multi-step task, and decide a change mid-task (the `curl` propose above as backend, then `accept` as frontend). The worker picks it up after its next tool call. Hook state is per session under `~/Library/Caches/agent-bus/hooks/`.

### 4. Two laptops

- **Server laptop:** `./bin/busd -addr 0.0.0.0:7070`. Find its LAN IP (`ipconfig getifaddr en0`).
- **Each laptop:** `export AGENT_BUS_URL=http://<server-ip>:7070`, then run its daemon in the real project repo:
  `./bin/bus-daemon -as backend@abhay -mandate mandates/backend.md -repo ~/code/app`
- **Each worker** (your normal Claude Code session), from the project repo:
  ```sh
  /abs/path/bin/bus-hook install -server http://<server-ip>:7070 -as backend@abhay   # writes .claude/settings.local.json
  claude mcp add agent-bus -- /abs/path/bin/bus-mcp -server http://<server-ip>:7070 -as backend@abhay -role worker
  ```
  Restart Claude Code. Decisions now reach the worker between tool calls (PostToolUse) and with your next prompt (UserPromptSubmit), each once; the Stop hook keeps it working while a decided change is unacked or its own blocking proposal is open (up to 3 nudges per unchanged situation). Your messages and Esc still take priority.
  Then ask it to make a change that needs a contract change, e.g. *"rename the users.name column to display_name and propose the API change on the agent bus"*.

Identities and contract owners live in `bus.json`. Add `.agentbus/` to the project repo's `.gitignore`.
