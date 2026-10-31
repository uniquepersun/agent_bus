-- Agent Bus ledger. `events` is the source of truth and is append-only.
-- `threads` and `contracts.head_rev` are projections updated in the same
-- transaction as the events that change them.

CREATE TABLE IF NOT EXISTS agents (
  id      TEXT PRIMARY KEY,           -- 'backend@abhay'
  human   TEXT NOT NULL,              -- 'abhay'; that human acts as 'human:abhay'
  mandate TEXT NOT NULL DEFAULT ''    -- mandate.md snapshot (dashboard + liaison prompt)
);

CREATE TABLE IF NOT EXISTS contracts (
  name     TEXT PRIMARY KEY,          -- 'openapi.yaml' | 'schema.sql' | 'env.md'
  owner    TEXT NOT NULL REFERENCES agents(id),  -- wins when an escalation times out
  head_rev INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS threads (
  id             TEXT PRIMARY KEY,    -- slug: 'users-endpoint-shape'
  title          TEXT NOT NULL,
  contract       TEXT REFERENCES contracts(name),  -- NULL until the first proposal
  opened_by      TEXT NOT NULL,
  state          TEXT NOT NULL CHECK (state IN ('OPEN','PROPOSED','COUNTERED','ESCALATED','DECIDED')),
  blocking       INTEGER NOT NULL DEFAULT 0,
  counter_count  INTEGER NOT NULL DEFAULT 0,
  live_seq       INTEGER,             -- PROPOSE/COUNTER currently on the table
  decided_seq    INTEGER,             -- the proposal that won
  resolved_by    TEXT,                -- 'accept' | 'human:abhay' | 'timeout:owner'
  awaiting_human TEXT,                -- agent with an open ASK to its human
  escalated_at   INTEGER,             -- unix ms
  updated_seq    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS events (
  seq              INTEGER PRIMARY KEY AUTOINCREMENT,  -- global order; WS resume cursor
  ts               INTEGER NOT NULL,                   -- unix ms
  thread_id        TEXT REFERENCES threads(id),        -- NULL only for NOTIFY
  type             TEXT NOT NULL CHECK (type IN (
                     'PROPOSE','COUNTER','ACCEPT','ASK','ANSWER','NOTIFY','ESCALATE',
                     'DECIDED','RESOLVE','ASSUMPTION_BROKEN')),
  from_id          TEXT NOT NULL,     -- agent id | 'human:<name>' | 'server'
  to_id            TEXT,              -- agent id | 'human:<name>' | NULL = everyone else
  blocking         INTEGER NOT NULL DEFAULT 0,
  reply_to         INTEGER REFERENCES events(seq),
  contract         TEXT REFERENCES contracts(name),
  rev              INTEGER,           -- PROPOSE/COUNTER: base rev; DECIDED: new rev
  edits_json       TEXT,              -- [{"old","new"}] exactly as submitted
  proposed_content TEXT,              -- full contract after edits (PROPOSE/COUNTER)
  diff             TEXT,              -- server-computed unified diff
  body             TEXT NOT NULL DEFAULT '',  -- reason / question / answer
  mandate_ref      TEXT
);
CREATE INDEX IF NOT EXISTS events_thread ON events(thread_id, seq);
CREATE INDEX IF NOT EXISTS events_to ON events(to_id, seq);

CREATE TRIGGER IF NOT EXISTS events_no_update BEFORE UPDATE ON events
  BEGIN SELECT RAISE(ABORT, 'ledger is append-only'); END;
CREATE TRIGGER IF NOT EXISTS events_no_delete BEFORE DELETE ON events
  BEGIN SELECT RAISE(ABORT, 'ledger is append-only'); END;

CREATE TABLE IF NOT EXISTS contract_revs (
  contract   TEXT NOT NULL REFERENCES contracts(name),
  rev        INTEGER NOT NULL,
  content    TEXT NOT NULL,
  decided_by INTEGER REFERENCES events(seq),  -- NULL for the seed rev 0
  PRIMARY KEY (contract, rev)
);
CREATE TRIGGER IF NOT EXISTS contract_revs_no_update BEFORE UPDATE ON contract_revs
  BEGIN SELECT RAISE(ABORT, 'contract revisions are append-only'); END;

CREATE TABLE IF NOT EXISTS cursors (
  consumer  TEXT PRIMARY KEY,         -- 'backend@abhay/liaison' | 'backend@abhay/worker'
  acked_seq INTEGER NOT NULL DEFAULT 0
);
