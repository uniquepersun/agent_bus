// Package bus is the ledger and thread state machine behind the Agent Bus
// server. Every write goes through one transaction that appends events,
// updates projections, and then publishes the events to subscribers.
package bus

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

type Bus struct {
	db *sql.DB
	mu sync.Mutex // serializes writes so state checks and appends are atomic

	Now               func() time.Time
	EscalationTimeout time.Duration

	subMu sync.Mutex
	subs  map[chan Event]struct{}
}

func Open(path string) (*Bus, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Bus{
		db:                db,
		Now:               time.Now,
		EscalationTimeout: 2 * time.Minute,
		subs:              map[chan Event]struct{}{},
	}, nil
}

func (b *Bus) Close() error { return b.db.Close() }

// UpsertAgent registers an agent. An empty mandate keeps the stored one.
func (b *Bus) UpsertAgent(id, human, mandate string) error {
	_, err := b.db.Exec(`INSERT INTO agents (id, human, mandate) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET human = excluded.human,
			mandate = CASE WHEN excluded.mandate != '' THEN excluded.mandate ELSE agents.mandate END`,
		id, human, mandate)
	return err
}

func (b *Bus) SetMandate(id, mandate string) error {
	res, err := b.db.Exec(`UPDATE agents SET mandate = ? WHERE id = ?`, mandate, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound("unknown agent %q", id)
	}
	return nil
}

// SeedContract creates a contract at rev 0 if it doesn't exist yet, and
// always updates its owner.
func (b *Bus) SeedContract(name, owner, content string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO contracts (name, owner) VALUES (?, ?) ON CONFLICT(name) DO NOTHING`, name, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		if _, err := tx.Exec(`INSERT INTO contract_revs (contract, rev, content) VALUES (?, 0, ?)`, name, content); err != nil {
			return err
		}
	} else if _, err := tx.Exec(`UPDATE contracts SET owner = ? WHERE name = ?`, owner, name); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- subscriptions ----

// Subscribe returns a channel of every event appended from now on. If the
// subscriber falls behind, the channel is closed and it should resubscribe
// and catch up from the ledger.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	b.subMu.Lock()
	b.subs[ch] = struct{}{}
	b.subMu.Unlock()
	return ch, func() {
		b.subMu.Lock()
		defer b.subMu.Unlock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
	}
}

func (b *Bus) publish(evs []Event) {
	b.subMu.Lock()
	defer b.subMu.Unlock()
next:
	for ch := range b.subs {
		for _, e := range evs {
			select {
			case ch <- e:
			default:
				delete(b.subs, ch)
				close(ch)
				continue next
			}
		}
	}
}

// ---- transactions ----

type txn struct {
	tx     *sql.Tx
	now    int64
	events []Event
}

// do runs fn in a write transaction and publishes the appended events after
// commit.
func (b *Bus) do(fn func(t *txn) (*Thread, error)) (*Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	tx, err := b.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	t := &txn{tx: tx, now: b.Now().UnixMilli()}
	th, err := fn(t)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	b.publish(t.events)
	return &Result{Thread: th, Events: t.events}, nil
}

func (t *txn) append(e Event) (Event, error) {
	e.TS = t.now
	var edits any
	if len(e.Edits) > 0 {
		j, err := json.Marshal(e.Edits)
		if err != nil {
			return e, err
		}
		edits = string(j)
	}
	var rev any
	if e.Rev != nil {
		rev = *e.Rev
	}
	res, err := t.tx.Exec(`INSERT INTO events
		(ts, thread_id, type, from_id, to_id, blocking, reply_to, contract, rev,
		 edits_json, proposed_content, diff, body, mandate_ref)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TS, nullStr(e.Thread), e.Type, e.From, nullStr(e.To), e.Blocking, nullInt(e.ReplyTo),
		nullStr(e.Contract), rev, edits, nullStr(e.ProposedContent), nullStr(e.Diff), e.Body,
		nullStr(e.MandateRef))
	if err != nil {
		return e, err
	}
	if e.Seq, err = res.LastInsertId(); err != nil {
		return e, err
	}
	t.events = append(t.events, e)
	return e, nil
}

func (t *txn) saveThread(th *Thread) error {
	_, err := t.tx.Exec(`INSERT INTO threads
		(id, title, contract, opened_by, state, blocking, counter_count, live_seq, decided_seq,
		 resolved_by, awaiting_human, escalated_at, updated_seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			title = excluded.title, contract = excluded.contract, state = excluded.state,
			blocking = excluded.blocking, counter_count = excluded.counter_count,
			live_seq = excluded.live_seq, decided_seq = excluded.decided_seq,
			resolved_by = excluded.resolved_by, awaiting_human = excluded.awaiting_human,
			escalated_at = excluded.escalated_at, updated_seq = excluded.updated_seq`,
		th.ID, th.Title, nullStr(th.Contract), th.OpenedBy, th.State, th.Blocking, th.CounterCount,
		nullInt(th.LiveSeq), nullInt(th.DecidedSeq), nullStr(th.ResolvedBy),
		nullStr(th.AwaitingHuman), nullInt(th.EscalatedAt), th.UpdatedSeq)
	return err
}

// ---- row loading (shared by txn and read paths) ----

type querier interface {
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

const threadCols = `id, title, contract, opened_by, state, blocking, counter_count, live_seq,
	decided_seq, resolved_by, awaiting_human, escalated_at, updated_seq`

func scanThread(s interface{ Scan(...any) error }) (*Thread, error) {
	var th Thread
	var contract, resolvedBy, awaiting sql.NullString
	var live, decided, escalated sql.NullInt64
	err := s.Scan(&th.ID, &th.Title, &contract, &th.OpenedBy, &th.State, &th.Blocking,
		&th.CounterCount, &live, &decided, &resolvedBy, &awaiting, &escalated, &th.UpdatedSeq)
	if err != nil {
		return nil, err
	}
	th.Contract, th.ResolvedBy, th.AwaitingHuman = contract.String, resolvedBy.String, awaiting.String
	th.LiveSeq, th.DecidedSeq, th.EscalatedAt = live.Int64, decided.Int64, escalated.Int64
	return &th, nil
}

// loadThread returns nil, nil when the thread doesn't exist.
func loadThread(q querier, id string) (*Thread, error) {
	th, err := scanThread(q.QueryRow(`SELECT `+threadCols+` FROM threads WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return th, err
}

const eventCols = `seq, ts, thread_id, type, from_id, to_id, blocking, reply_to, contract, rev,
	edits_json, proposed_content, diff, body, mandate_ref`

func scanEvent(s interface{ Scan(...any) error }) (Event, error) {
	var e Event
	var thread, to, contract, edits, content, diff, mandateRef sql.NullString
	var replyTo, rev sql.NullInt64
	err := s.Scan(&e.Seq, &e.TS, &thread, &e.Type, &e.From, &to, &e.Blocking, &replyTo,
		&contract, &rev, &edits, &content, &diff, &e.Body, &mandateRef)
	if err != nil {
		return e, err
	}
	e.Thread, e.To, e.Contract = thread.String, to.String, contract.String
	e.ProposedContent, e.Diff, e.MandateRef = content.String, diff.String, mandateRef.String
	e.ReplyTo = replyTo.Int64
	if rev.Valid {
		r := int(rev.Int64)
		e.Rev = &r
	}
	if edits.Valid {
		if err := json.Unmarshal([]byte(edits.String), &e.Edits); err != nil {
			return e, err
		}
	}
	return e, nil
}

func queryEvents(q querier, where string, args ...any) ([]Event, error) {
	rows, err := q.Query(`SELECT `+eventCols+` FROM events WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	evs := []Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		evs = append(evs, e)
	}
	return evs, rows.Err()
}

func loadEvent(q querier, seq int64) (*Event, error) {
	e, err := scanEvent(q.QueryRow(`SELECT `+eventCols+` FROM events WHERE seq = ?`, seq))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &e, err
}

func loadContract(q querier, name string, rev int) (*Contract, error) {
	var c Contract
	query := `SELECT c.name, c.owner, r.rev, r.content FROM contracts c
		JOIN contract_revs r ON r.contract = c.name AND r.rev = c.head_rev WHERE c.name = ?`
	args := []any{name}
	if rev >= 0 {
		query = `SELECT c.name, c.owner, r.rev, r.content FROM contracts c
			JOIN contract_revs r ON r.contract = c.name AND r.rev = ? WHERE c.name = ?`
		args = []any{rev, name}
	}
	err := q.QueryRow(query, args...).Scan(&c.Name, &c.Owner, &c.Rev, &c.Content)
	if errors.Is(err, sql.ErrNoRows) {
		if rev >= 0 {
			return nil, notFound("contract %q has no rev %d", name, rev)
		}
		return nil, notFound("unknown contract %q", name)
	}
	return &c, err
}

// ---- reads ----

func (b *Bus) Agents() ([]Agent, error) {
	rows, err := b.db.Query(`SELECT id, human, mandate FROM agents ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Agent{}
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.ID, &a.Human, &a.Mandate); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Contracts lists contracts at their head revision, without content.
func (b *Bus) Contracts() ([]Contract, error) {
	rows, err := b.db.Query(`SELECT name, owner, head_rev FROM contracts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Contract{}
	for rows.Next() {
		var c Contract
		if err := rows.Scan(&c.Name, &c.Owner, &c.Rev); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Contract returns a contract at rev, or at head when rev < 0.
func (b *Bus) Contract(name string, rev int) (*Contract, error) {
	return loadContract(b.db, name, rev)
}

func (b *Bus) Threads() ([]Thread, error) {
	rows, err := b.db.Query(`SELECT ` + threadCols + ` FROM threads ORDER BY updated_seq DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Thread{}
	for rows.Next() {
		th, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *th)
	}
	return out, rows.Err()
}

func (b *Bus) Thread(id string) (*Thread, []Event, error) {
	th, err := loadThread(b.db, id)
	if err != nil {
		return nil, nil, err
	}
	if th == nil {
		return nil, nil, notFound("unknown thread %q", id)
	}
	evs, err := queryEvents(b.db, `thread_id = ? ORDER BY seq`, id)
	return th, evs, err
}

// Events returns ledger events after seq, oldest first.
func (b *Bus) Events(after int64) ([]Event, error) {
	return queryEvents(b.db, `seq > ? ORDER BY seq`, after)
}

// Decisions returns DECIDED events, optionally for one contract.
func (b *Bus) Decisions(contract string, after int64) ([]Event, error) {
	if contract != "" {
		return queryEvents(b.db, `type = 'DECIDED' AND contract = ? AND seq > ? ORDER BY seq`, contract, after)
	}
	return queryEvents(b.db, `type = 'DECIDED' AND seq > ? ORDER BY seq`, after)
}

func consumer(agent string, role Role) string { return agent + "/" + string(role) }

// Inbox returns unacked events for an agent's role: events from others that
// are addressed to it or to everyone.
func (b *Bus) Inbox(agent string, role Role) ([]Event, error) {
	types, ok := roleTypes[role]
	if !ok {
		return nil, badRequest("role must be %q or %q", RoleLiaison, RoleWorker)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(types)), ",")
	args := []any{consumer(agent, role), agent, agent}
	for _, t := range types {
		args = append(args, t)
	}
	return queryEvents(b.db, `seq > COALESCE((SELECT acked_seq FROM cursors WHERE consumer = ?), 0)
		AND from_id != ? AND (to_id IS NULL OR to_id = ?)
		AND type IN (`+placeholders+`) ORDER BY seq LIMIT 100`, args...)
}

// Ack marks everything up to seq as read for an agent's role.
func (b *Bus) Ack(agent string, role Role, seq int64) error {
	if _, ok := roleTypes[role]; !ok {
		return badRequest("role must be %q or %q", RoleLiaison, RoleWorker)
	}
	_, err := b.db.Exec(`INSERT INTO cursors (consumer, acked_seq) VALUES (?, ?)
		ON CONFLICT(consumer) DO UPDATE SET acked_seq = MAX(acked_seq, excluded.acked_seq)`,
		consumer(agent, role), seq)
	return err
}

// WaitForReply blocks until someone other than who appends to the thread
// after seq `after`, or ctx ends (returning no events). With after <= 0 it
// waits for anything newer than who's own latest event in the thread.
func (b *Bus) WaitForReply(ctx context.Context, who, thread string, after int64) ([]Event, error) {
	ch, cancel := b.Subscribe()
	defer cancel()
	if after <= 0 {
		if err := b.db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM events WHERE thread_id = ? AND from_id = ?`,
			thread, who).Scan(&after); err != nil {
			return nil, err
		}
	}
	replies := func() ([]Event, error) {
		return queryEvents(b.db, `thread_id = ? AND seq > ? AND from_id != ? ORDER BY seq`, thread, after, who)
	}
	if evs, err := replies(); err != nil || len(evs) > 0 {
		return evs, err
	}
	for {
		select {
		case <-ctx.Done():
			return []Event{}, nil
		case e, ok := <-ch:
			if !ok {
				return replies()
			}
			if e.Thread == thread && e.From != who && e.Seq > after {
				// Re-read so events committed in the same transaction come back together.
				return replies()
			}
		}
	}
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(i int64) any {
	if i == 0 {
		return nil
	}
	return i
}
