# Agent Bus

Agent Bus is a coordination layer for AI coding agents that work on the same project from different machines. Each agent owns a scope, such as the frontend or the backend. When a change crosses that boundary (an API response shape, a database column, an environment variable), the agents negotiate it through a structured protocol. They negotiate only within limits their humans have set, and every agreed outcome is recorded in shared files that both sides build against.

Build, run, and test steps are in [INSTRUCTIONS.md](INSTRUCTIONS.md). The original design brief is in [SPEC.md](SPEC.md).

## Quick start

Download the archive for your platform from the [latest release](https://github.com/uniquepersun/agent_bus/releases/latest) (macOS and Linux, arm64 and amd64), unpack it, and use `demo.sh`. No Go toolchain is needed.

```sh
./demo.sh server                                                    # on the machine that hosts the bus and dashboard
./demo.sh laptop -server http://<bus-ip>:7070 -as frontend@teammate -project ~/code/app   # on each laptop
./demo.sh rehearse                                                  # both sides on one machine, for practice
```

Each laptop needs a signed-in coding agent CLI: the Antigravity CLI (`agy`, the default) or Claude Code (`-agent claude`). Agents can be mixed: one person can use `agy` while the other uses Claude Code.

## The problem

Two developers who each pair with a coding agent quickly hit the same friction: one agent changes an API or a schema, and the other agent's code breaks. Letting the agents chat freely does not solve it. Unstructured conversation drifts, loops, and consumes tokens without converging, and nothing guarantees the humans stay in charge of decisions that matter to them.

## The approach

Agent Bus treats the conversation as a negotiation over shared artifacts, not as a chat.

- **Every proposal is a concrete change.** A proposal or counter-proposal must carry an exact edit to a named contract file. "We should probably change the user object" is rejected; "replace `name: string` with `display_name: string` in `openapi.yaml` at revision 3" is accepted.
- **Threads have a state machine that guarantees convergence.** A thread allows two counter-proposals. The next one escalates to the humans, and an escalation that the humans leave alone is decided in favor of the contract's owner after a timeout.
- **Humans stay in control through mandates.** Each human writes a short mandate for their agent: its scope, what it may accept on its own, and what it must ask about. Anything outside the mandate goes to that human before the agent replies.
- **Agents are never interrupted mid-task by negotiation.** A separate, short-lived liaison session negotiates in the background. The working session only hears outcomes, and hears them between steps.

## How it works

```
                 +------------------------------------------+
                 |                  busd                    |
                 |  append-only ledger (SQLite)             |
                 |  thread state machine                    |
                 |  REST API + WebSocket event feed         |
                 |  renderer: DECISIONS.md, contracts/      |
                 |  live dashboard                          |
                 +-------------------+----------------------+
                                     |
            +------------------------+------------------------+
            |                                                 |
   Laptop A (backend)                                Laptop B (frontend)
   bus-daemon                                        bus-daemon
     - follows the feed                                - follows the feed
     - writes DECISIONS.md, contracts/ (read-only)     - writes DECISIONS.md, contracts/
     - starts a liaison when messages arrive           - starts a liaison when messages arrive
   liaison: claude -p + bus-mcp (liaison tools)      liaison: claude -p + bus-mcp
   worker:  Claude Code + bus-mcp + bus-hook         worker:  Claude Code + bus-mcp + bus-hook
```

### Ledger and rendered views

The central server keeps an append-only ledger of events in SQLite. The ledger is the source of truth, and database triggers reject any update or deletion. Threads and contract revisions are projections maintained in the same transaction as the events that change them.

From the ledger the server renders `DECISIONS.md` (each decision with its winning proposal, the mandate that justified it, how it was reached, and its diff) and a `contracts/` folder with the current revision of `openapi.yaml`, `schema.sql`, and `env.md`. Each laptop's daemon writes these into the project repository as read-only files. Agents and humans read them but never edit them; all changes go through the protocol, so two agents can never overwrite each other's edits.

### Messages

| Type | Meaning |
|---|---|
| `PROPOSE` | Opens a thread with a concrete edit to a contract |
| `COUNTER` | Rejects the proposal on the table with a concrete alternative |
| `ACCEPT` | Agrees; decides the thread and applies the change |
| `ASK` | Asks the other agent, or the agent's own human, for information |
| `ANSWER` | Replies to an `ASK` |
| `NOTIFY` | Announces an in-scope decision; opens no thread and needs no reply |
| `ESCALATE` | Hands the thread to the humans |

The server also records `DECIDED` when a change is applied, `RESOLVE` when a human picks a proposal, and `ASSUMPTION_BROKEN` when a non-blocking proposal that an agent built on is not the one that won.

### Thread states

```
OPEN --> PROPOSED --> COUNTERED (up to 2) --> DECIDED
             |              |                    ^
             |              +--> ESCALATED ------+  (human picks, or the owner's
             +-----------------------------------+   proposal wins on timeout)
```

The server enforces the rules: an agent cannot accept or counter its own proposal, only a human can resolve an escalation or answer a question addressed to them, and proposals must be made against the contract's current revision.

### Edits

Proposals carry find-and-replace edits rather than unified diffs, because language models produce exact text reliably and diff hunk headers unreliably. The server applies the edits, rejects any whose search text is missing or ambiguous, checks that YAML contracts still parse, and generates the display diff itself. If a contract has moved on by the time a proposal is accepted, the server replays the proposal's edits onto the new revision and only rejects it if they no longer apply.

### Blocking and non-blocking proposals

A blocking proposal means the proposer cannot continue until the thread is decided. A non-blocking proposal lets the proposer continue on the assumption that it will be accepted; if the thread is decided differently, the server sends that agent an `ASSUMPTION_BROKEN` event so it can correct its code.

### Worker and liaison

Each developer runs two kinds of agent session:

- **Worker:** the developer's normal coding agent session (Claude Code or the Antigravity CLI, `agy`). It writes code, reads the rendered contracts, and can propose changes. It is never asked to negotiate.
- **Liaison:** a headless session (`claude -p`, `agy --print`, or any CLI with a headless mode such as `codex exec`) that the daemon starts when a message arrives. It receives the mandate and the new messages, has the negotiation tools and read-only access to the repository, takes its actions, and exits.

### Delivering outcomes to a busy worker

Coding agents act only when called, so the worker needs a way to learn about decisions without being stopped. For Claude Code, `bus-hook` provides three hooks:

- **PostToolUse** adds new decisions to the worker's context after its next tool call, the same point at which Claude Code delivers a message the user types mid-task. Each event is delivered once, framed as lower priority than the user.
- **UserPromptSubmit** attaches pending decisions to the user's next prompt.
- **Stop** keeps the worker working while a decided change is not yet applied and acknowledged, or while its own blocking proposal is still being negotiated. It gives up after three nudges with no change, and a user interrupt always takes precedence.

For the Antigravity CLI, `bus-hook install -agent agy` installs a workspace plugin (`.agents/plugins/agent-bus/`) that bundles the worker's MCP server, a `PreInvocation` hook that injects new decisions before each model call, a `Stop` hook with the same behavior as above (never on a user cancel), and a rules file explaining the protocol to the agent.

### Terminal chat

`bus chat` is a single terminal per developer for live demonstrations. The developer chats with their `agy` worker as a persistent session, and Agent Bus activity appears inline in the same conversation: proposals, counters, and decisions as they happen, the moment an update is delivered to the agent, and questions from the developer's own liaison. The developer answers those questions with `/answer` and resolves escalations with `/pick` without leaving the chat.

## Components

| Path | Description |
|---|---|
| `cmd/busd` | Central server: ledger, state machine, REST API, WebSocket feed, renderer, dashboard |
| `cmd/bus-daemon` | Per-laptop process: follows the feed, syncs rendered files, starts liaisons |
| `cmd/bus-mcp` | MCP server over stdio, configured per agent with an identity and a role (`worker` or `liaison`) |
| `cmd/bus-hook` | Claude Code hook handler and installer |
| `cmd/bus` | Command-line interface for humans: list and inspect threads, answer questions, resolve escalations, follow the feed, and `bus chat` |
| `internal/bus` | Ledger schema, state machine, edit application and validation |
| `internal/server` | HTTP handlers, WebSocket feed, embedded dashboard |
| `internal/render` | Rendering of `DECISIONS.md` and `contracts/` |
| `internal/mcptools` | MCP tool definitions and schemas |
| `internal/daemon` | Feed follower, file sync, liaison scheduling and runners |
| `internal/hook` | Hook logic for Claude Code and agy, settings and plugin installers, liaison tool gate |
| `internal/chat` | Terminal chat with an agy worker, with bus activity inline |
| `internal/client` | HTTP client shared by the daemon, MCP server, hooks, and CLI |
| `bus.json`, `mandates/`, `seed/` | Example configuration, mandates, and initial contracts |

## MCP tools

| Tool | Worker | Liaison | Purpose |
|---|---|---|---|
| `propose` | yes | yes | Open a thread with a concrete contract edit |
| `notify` | yes | yes | Announce an in-scope decision |
| `get_contract` | yes | yes | Read a contract at its current or a past revision |
| `get_decisions` | yes | yes | List decided changes with diffs |
| `get_thread` | yes | yes | Read a thread's state and history |
| `inbox` / `ack` | yes | yes | Read and acknowledge pending events for the session's role |
| `wait_for_reply` | yes | yes | Long-poll a thread for a reply |
| `counter`, `accept` | | yes | Respond to the proposal on the table |
| `ask`, `answer` | | yes | Ask or answer questions, including asking the agent's own human |
| `escalate` | | yes | Hand a thread to the humans |

Identity and role come from the MCP server's configuration, never from tool arguments, so an agent cannot act as the other side. Contract names are enumerated in the tool schemas, and proposals with no edits are rejected by the schema before they reach the server.

## Technology

- Go, with the standard library HTTP server
- SQLite through `modernc.org/sqlite` (pure Go, no cgo)
- `github.com/coder/websocket` for the event feed
- `github.com/modelcontextprotocol/go-sdk` for the MCP server
- A single embedded HTML page for the dashboard, with no build step

## Scope and limitations

Agent Bus was built for a hackathon and deliberately limits its scope:

- Two parties and one central server. Voting and multi-party negotiation are out of scope.
- No authentication. Callers identify themselves with a header, so the server should run only on a trusted network.
- Agents never edit contract files directly; all changes go through the protocol.
- Worker hooks are implemented for Claude Code and the Antigravity CLI. Other agents can use the MCP tools and check their inbox between tasks, with less immediate delivery.
- Headless agy reads permission rules only from its global settings, so the installer adds narrowly scoped rules there (the agent-bus MCP server and writes to the project directory). A tool that headless agy is not permitted to use ends the agent's turn.
