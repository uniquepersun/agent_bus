# Showcase: the tasks app

A small two-person project for demonstrating Agent Bus: a tasks API (backend) and its UI (frontend). The shared contracts (`seed/`) describe the API, the database schema, and the environment variables. The mandates (`mandates/`) say what each agent may decide alone and what it must ask its human about.

| Side | Identity | Project | Mandate highlights |
|---|---|---|---|
| Backend | `backend@abhay` | `~/Desktop/test-backend` | owns the contracts; auto-accepts additive changes; asks about renames, type changes, auth |
| Frontend | `frontend@teammate` | `~/Desktop/test-frontend` | the UI reads `User.name`, `User.email`, `Task.title`, `Task.done`; asks before any of those change |

## Run it

From the agent_bus folder (source checkout or release archive):

```sh
# Laptop A: the bus (prints the address for Laptop B), then in a second terminal the backend side
./demo.sh showcase server -fresh
./demo.sh showcase backend -server http://<bus-address>:7070

# Laptop B: the frontend side
./demo.sh showcase frontend -server http://<bus-address>:7070
```

Each side creates its project from `template/` the first time if the folder has no code yet. Add `-agent claude` to use Claude Code instead of agy, or `-project DIR` to put the project elsewhere. Open the bus address in a browser for the dashboard.

## Demo script

**1. Warm-up (additive, decided without the humans).** In the backend chat:

> Tasks need due dates. Propose adding an optional `due_date` string field (ISO date) to the Task schema in openapi.yaml, and a nullable `due_date DATE` column to the tasks table in schema.sql, as two non-blocking proposals on the agent bus. Read the contracts first. Do not edit code yet.

The frontend liaison accepts on its own (additive fields are on its auto-accept list). Both chats show the decisions, and the frontend agent can then show the due date in `TaskList.tsx`.

**2. The main event (breaking, needs a human).** In the backend chat:

> We want task states beyond done/not done. Propose replacing the boolean `done` field on Task in openapi.yaml with a `status` string field whose values are todo, doing, or done, as a blocking proposal on the agent bus. Read the contract first. Do not edit code yet.

1. The frontend liaison sees that `Task.done` is a field the UI reads, so it asks its human. The frontend chat shows `QUESTION FOR YOU`.
2. The frontend person answers: `/answer <seq> fine, but keep done for one release so the UI does not break`
3. The frontend liaison counters with `status` added and `done` kept; the backend liaison accepts it (additive, within its mandate).
4. The frontend agent updates `src/api.ts` and `TaskList.tsx` to use `status`, and the backend person can ask their agent to implement it in `src/server.js`.

Throughout, each chat shows what the other laptop's agent is doing, and the dashboard shows both sides.

## Reset

Restart the bus with `./demo.sh showcase server -fresh`, and delete the two project folders to start from the template again (or `git checkout .` in each if you committed the starting state).
