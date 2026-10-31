# Agent Bus: Hackathon Spec

Coordination layer that lets coding agents on different machines (Claude Code, Codex, Antigravity) work on one project, each owning a scope (frontend, backend, DB), while sharing context and negotiating cross-boundary decisions. Decisions come from each agent's human; agents negotiate within those limits and record outcomes in shared files.

Hackathon scope: **2 agents, 2 laptops, 1 central server.** Business concerns are out of scope.

---

## Core principle

The conversation is a **negotiation over shared artifacts, not a chat**. Free-form agent chat drifts, loops and burns tokens. Every message is structured and tied to a concrete thing (an endpoint, a table, an env var).

## Components

1. **Central server** (FastAPI or Axum): SQLite ledger, REST API and a WebSocket event feed.
2. **MCP server**: a single binary or script used by both agents, configured per side with identity (`backend@abhay`, `frontend@teammate`).
3. **Daemon per laptop**: listens on the WebSocket and spawns liaison sessions when messages arrive.
4. **Renderer**: turns the ledger into `DECISIONS.md` and the `contracts/` folder.
5. **Claude Code hooks**: Stop and UserPromptSubmit (plus PostToolUse) for inbox injection.
6. **Dashboard**: live view of threads moving through states. This is the demo screen.

## Source of truth

- **Ledger** (append-only, in the server DB) is the truth.
- **Rendered views:** `DECISIONS.md` and `contracts/` (`openapi.yaml`, `schema.sql`, `env.md`).
- Agents and humans **read** the rendered files but **never edit them directly**. All changes go through the protocol, which prevents agents overwriting each other.

## Threads

- A thread is one topic (e.g. "shape of `/users` response").
- Every message belongs to a thread.

## Message types

| Type | Meaning |
|---|---|
| `PROPOSE` | "I want X" plus a concrete contract diff |
| `ACCEPT` | Agree; closes the thread as a decision |
| `COUNTER` | Disagree; **must** include an alternative diff |
| `ASK` | Question needing info, not agreement |
| `ANSWER` | Reply to an ASK |
| `NOTIFY` | FYI about an in-scope decision; never opens a thread, no reply needed |
| `ESCALATE` | Needs the humans |

**Rule:** every PROPOSE or COUNTER carries a diff. Vague proposals are rejected.

Example:
```json
{
  "thread": "users-endpoint-shape",
  "from": "backend@abhay",
  "type": "PROPOSE",
  "blocking": true,
  "contract": "openapi.yaml",
  "diff": "- name: string\n+ display_name: string",
  "reason": "DB column renamed for i18n",
  "mandate_ref": "human said: no breaking changes without notice"
}
```

## Thread state machine

```
OPEN → PROPOSED → ACCEPTED → DECIDED (written to DECISIONS.md)
            ↓
        COUNTERED (max 2 rounds) → ESCALATED → humans pick
```

- Max 2 counters, then auto-escalate.
- The scope owner breaks ties if the humans don't respond within N minutes.

## Mandates (how humans stay in control)

Each human writes `mandate.md` for their agent:

```
Scope: backend/, schema.sql, openapi.yaml
I care about: no breaking API changes without a migration path, Postgres only
Auto-accept: additive fields, new endpoints
Ask me: removing/renaming fields, new dependencies, auth changes
```

- The agent negotiates freely **inside** its mandate.
- Anything outside it requires an `ASK` to its own human before replying to the other agent.

## Worker / liaison split (chosen design)

Two agent sessions per teammate:

- **Worker:** the human's normal Claude Code or Codex session. It writes code, reads `DECISIONS.md` and contracts, and is never interrupted mid-task.
- **Liaison:** a headless session (`claude -p` / `codex exec`) spawned by the daemon whenever a message arrives. It has the mandate, read-only repo access and the protocol tools. It negotiates, decides or escalates, then exits.

When a decision lands, the daemon nudges the worker through hooks, e.g. "Contract changed: `/users.name` → `display_name`. Update your code."

## Delivery into agents

- **Claude Code:**
  - `UserPromptSubmit` / `PostToolUse` hooks inject pending messages and decision updates into context.
  - A `Stop` hook blocks the agent from finishing while a `blocking: true` message is unanswered.
- **Codex / others:** an AGENTS.md section tells the agent to check the inbox before and after each task.

## MCP tools

- `inbox()`
- `send(thread, type, diff?, reason, blocking)`
- `get_decisions(scope)`
- `get_contract(name)`
- `wait_for_reply(thread, timeout)`: long-poll for when the agent is blocked

## Blocking vs non-blocking

- `blocking: true`: the sender waits (or the Stop hook holds it) until a reply.
- `blocking: false`: the sender continues on an **assumption** logged as `ASSUMED`. If the thread resolves differently, the server flags the broken assumption and the agent fixes its code.

## Demo trace

```
[backend liaison]  PROPOSE  users-shape: rename name→display_name (blocking)
[frontend liaison] checks mandate → renames are "ask me" → ASK its human
[frontend human]   "fine, but keep name for one release"
[frontend liaison] COUNTER: add display_name, keep name as deprecated
[backend liaison]  within mandate (additive) → ACCEPT
[server]           DECIDED → updates openapi.yaml + DECISIONS.md
[hooks]            both workers notified; frontend worker updates UserCard.tsx
```

## Build order

1. Server: ledger schema, thread state machine, REST + WebSocket
2. MCP server with the tools above
3. Renderer (ledger → DECISIONS.md + contracts/)
4. Daemon + liaison spawning
5. Claude Code hooks
6. Dashboard
7. End-to-end run of the demo trace on two machines

## Out of scope

- Agents editing contracts directly
- More than two parties
- Voting
- Business, auth or multi-tenant concerns

## Next step

Design the ledger table schema and exact MCP tool schemas, then scaffold the server.
