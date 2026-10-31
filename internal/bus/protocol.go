package bus

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,63}$`)

type ProposeReq struct {
	Title      string `json:"title"`
	Contract   string `json:"contract"`
	BaseRev    int    `json:"base_rev"`
	Edits      []Edit `json:"edits"`
	Reason     string `json:"reason"`
	Blocking   bool   `json:"blocking"`
	MandateRef string `json:"mandate_ref,omitempty"`
}

type CounterReq struct {
	BaseRev    int    `json:"base_rev"`
	Edits      []Edit `json:"edits"`
	Reason     string `json:"reason"`
	MandateRef string `json:"mandate_ref,omitempty"`
}

type AcceptReq struct {
	Reason string `json:"reason,omitempty"`
}

type AskReq struct {
	Title    string `json:"title,omitempty"` // required when the ASK opens the thread
	Question string `json:"question"`
	To       string `json:"to"` // "peer" (default) or "human"
	Blocking bool   `json:"blocking"`
}

type AnswerReq struct {
	ReplyTo int64  `json:"reply_to"`
	Answer  string `json:"answer"`
}

type EscalateReq struct {
	Reason  string   `json:"reason"`
	Options []string `json:"options,omitempty"`
}

type ResolveReq struct {
	Pick int64  `json:"pick"` // seq of the PROPOSE/COUNTER the human chose
	Note string `json:"note,omitempty"`
}

type NotifyReq struct {
	Topic    string `json:"topic"`
	Body     string `json:"body"`
	Contract string `json:"contract,omitempty"`
}

// Propose opens a thread with a concrete change to a contract. A thread
// previously opened by an ASK takes its first proposal here too.
func (b *Bus) Propose(from, thread string, r ProposeReq) (*Result, error) {
	if !slugRe.MatchString(thread) {
		return nil, badRequest("thread id must be a lowercase slug like 'users-endpoint-shape'")
	}
	if strings.TrimSpace(r.Reason) == "" {
		return nil, badRequest("reason is required")
	}
	return b.do(func(t *txn) (*Thread, error) {
		if _, err := t.agent(from); err != nil {
			return nil, err
		}
		th, err := loadThread(t.tx, thread)
		if err != nil {
			return nil, err
		}
		switch {
		case th == nil:
			if strings.TrimSpace(r.Title) == "" {
				return nil, badRequest("title is required to open a thread")
			}
			th = &Thread{ID: thread, Title: r.Title, OpenedBy: from, State: StateOpen}
			if err := t.saveThread(th); err != nil {
				return nil, err
			}
		case th.State == StateOpen:
		default:
			return nil, conflict("thread %q is %s; reply with counter/accept, or open a new thread", thread, th.State)
		}
		content, diff, err := t.buildProposal(r.Contract, r.BaseRev, r.Edits)
		if err != nil {
			return nil, err
		}
		ev, err := t.append(Event{Thread: thread, Type: Propose, From: from, Blocking: r.Blocking,
			Contract: r.Contract, Rev: &r.BaseRev, Edits: r.Edits, ProposedContent: content,
			Diff: diff, Body: r.Reason, MandateRef: r.MandateRef})
		if err != nil {
			return nil, err
		}
		th.Contract, th.State, th.Blocking, th.LiveSeq, th.UpdatedSeq = r.Contract, StateProposed, r.Blocking, ev.Seq, ev.Seq
		return th, t.saveThread(th)
	})
}

// Counter replaces the proposal on the table with an alternative. The
// counter after MaxCounters escalates the thread to the humans.
func (b *Bus) Counter(from, thread string, r CounterReq) (*Result, error) {
	if strings.TrimSpace(r.Reason) == "" {
		return nil, badRequest("reason is required")
	}
	return b.do(func(t *txn) (*Thread, error) {
		th, live, err := t.negotiable(from, thread, "counter")
		if err != nil {
			return nil, err
		}
		content, diff, err := t.buildProposal(th.Contract, r.BaseRev, r.Edits)
		if err != nil {
			return nil, err
		}
		ev, err := t.append(Event{Thread: thread, Type: Counter, From: from, Blocking: th.Blocking,
			ReplyTo: live.Seq, Contract: th.Contract, Rev: &r.BaseRev, Edits: r.Edits,
			ProposedContent: content, Diff: diff, Body: r.Reason, MandateRef: r.MandateRef})
		if err != nil {
			return nil, err
		}
		th.CounterCount++
		th.State, th.LiveSeq, th.UpdatedSeq = StateCountered, ev.Seq, ev.Seq
		if th.CounterCount > MaxCounters {
			esc, err := t.append(Event{Thread: thread, Type: Escalate, From: ServerID,
				Body: fmt.Sprintf("auto-escalated: %d counters exceeds the limit of %d; humans pick a proposal", th.CounterCount, MaxCounters)})
			if err != nil {
				return nil, err
			}
			th.State, th.EscalatedAt, th.UpdatedSeq = StateEscalated, t.now, esc.Seq
		}
		return th, t.saveThread(th)
	})
}

// Accept agrees to the proposal on the table, which decides the thread and
// applies the proposal to its contract.
func (b *Bus) Accept(from, thread string, r AcceptReq) (*Result, error) {
	return b.do(func(t *txn) (*Thread, error) {
		th, live, err := t.negotiable(from, thread, "accept")
		if err != nil {
			return nil, err
		}
		if _, err := t.append(Event{Thread: thread, Type: Accept, From: from, ReplyTo: live.Seq,
			Contract: th.Contract, Body: r.Reason}); err != nil {
			return nil, err
		}
		return th, t.decide(th, live, "accept")
	})
}

// Ask asks the peer, or the asking agent's own human, for information.
func (b *Bus) Ask(from, thread string, r AskReq) (*Result, error) {
	if strings.TrimSpace(r.Question) == "" {
		return nil, badRequest("question is required")
	}
	return b.do(func(t *txn) (*Thread, error) {
		agent, err := t.agent(from)
		if err != nil {
			return nil, err
		}
		th, err := loadThread(t.tx, thread)
		if err != nil {
			return nil, err
		}
		switch {
		case th == nil:
			if !slugRe.MatchString(thread) {
				return nil, badRequest("thread id must be a lowercase slug like 'users-endpoint-shape'")
			}
			if strings.TrimSpace(r.Title) == "" {
				return nil, badRequest("title is required to open a thread")
			}
			th = &Thread{ID: thread, Title: r.Title, OpenedBy: from, State: StateOpen, Blocking: r.Blocking}
			if err := t.saveThread(th); err != nil {
				return nil, err
			}
		case th.State == StateDecided:
			return nil, conflict("thread %q is already DECIDED; open a new thread", thread)
		}
		var to string
		switch r.To {
		case "", "peer":
		case "human":
			to = "human:" + agent.Human
			th.AwaitingHuman = from
		default:
			return nil, badRequest(`to must be "peer" or "human"`)
		}
		ev, err := t.append(Event{Thread: thread, Type: Ask, From: from, To: to, Blocking: r.Blocking, Body: r.Question})
		if err != nil {
			return nil, err
		}
		th.UpdatedSeq = ev.Seq
		return th, t.saveThread(th)
	})
}

// Answer replies to an ASK. An ASK to a human can only be answered by that
// human; an ASK to the peer by anyone but the asker.
func (b *Bus) Answer(from, thread string, r AnswerReq) (*Result, error) {
	if strings.TrimSpace(r.Answer) == "" {
		return nil, badRequest("answer is required")
	}
	return b.do(func(t *txn) (*Thread, error) {
		if err := t.participant(from); err != nil {
			return nil, err
		}
		th, err := t.thread(thread)
		if err != nil {
			return nil, err
		}
		ask, err := loadEvent(t.tx, r.ReplyTo)
		if err != nil {
			return nil, err
		}
		if ask == nil || ask.Type != Ask || ask.Thread != thread {
			return nil, badRequest("reply_to must be the seq of an ASK in thread %q", thread)
		}
		toHuman := strings.HasPrefix(ask.To, "human:")
		switch {
		case toHuman && from != ask.To:
			return nil, forbidden("ASK #%d is for %s", ask.Seq, ask.To)
		case !toHuman && from == ask.From:
			return nil, forbidden("you can't answer your own ASK")
		}
		ev, err := t.append(Event{Thread: thread, Type: Answer, From: from, To: ask.From, ReplyTo: ask.Seq, Body: r.Answer})
		if err != nil {
			return nil, err
		}
		if toHuman && th.AwaitingHuman == ask.From {
			th.AwaitingHuman = ""
		}
		th.UpdatedSeq = ev.Seq
		return th, t.saveThread(th)
	})
}

// Escalate hands the thread to the humans.
func (b *Bus) Escalate(from, thread string, r EscalateReq) (*Result, error) {
	if strings.TrimSpace(r.Reason) == "" {
		return nil, badRequest("reason is required")
	}
	return b.do(func(t *txn) (*Thread, error) {
		if _, err := t.agent(from); err != nil {
			return nil, err
		}
		th, err := t.thread(thread)
		if err != nil {
			return nil, err
		}
		if th.State == StateDecided || th.State == StateEscalated {
			return nil, conflict("thread %q is already %s", thread, th.State)
		}
		body := r.Reason
		if len(r.Options) > 0 {
			body += "\n\nOptions:\n- " + strings.Join(r.Options, "\n- ")
		}
		ev, err := t.append(Event{Thread: thread, Type: Escalate, From: from, Body: body})
		if err != nil {
			return nil, err
		}
		th.State, th.EscalatedAt, th.UpdatedSeq = StateEscalated, t.now, ev.Seq
		return th, t.saveThread(th)
	})
}

// Resolve is a human picking a proposal on an escalated thread.
func (b *Bus) Resolve(from, thread string, r ResolveReq) (*Result, error) {
	if !strings.HasPrefix(from, "human:") {
		return nil, forbidden("only humans resolve escalations")
	}
	return b.do(func(t *txn) (*Thread, error) {
		if err := t.participant(from); err != nil {
			return nil, err
		}
		th, err := t.thread(thread)
		if err != nil {
			return nil, err
		}
		if th.State != StateEscalated {
			return nil, conflict("thread %q is %s, not ESCALATED", thread, th.State)
		}
		pick, err := loadEvent(t.tx, r.Pick)
		if err != nil {
			return nil, err
		}
		if pick == nil || pick.Thread != thread || (pick.Type != Propose && pick.Type != Counter) {
			return nil, badRequest("pick must be the seq of a PROPOSE or COUNTER in thread %q", thread)
		}
		if _, err := t.append(Event{Thread: thread, Type: Resolve, From: from, ReplyTo: pick.Seq, Body: r.Note}); err != nil {
			return nil, err
		}
		return th, t.decide(th, pick, from)
	})
}

// Notify is an FYI. It never opens a thread and needs no reply.
func (b *Bus) Notify(from string, r NotifyReq) (*Result, error) {
	if strings.TrimSpace(r.Topic) == "" {
		return nil, badRequest("topic is required")
	}
	return b.do(func(t *txn) (*Thread, error) {
		if _, err := t.agent(from); err != nil {
			return nil, err
		}
		if r.Contract != "" {
			if _, err := loadContract(t.tx, r.Contract, -1); err != nil {
				return nil, err
			}
		}
		body := r.Topic
		if r.Body != "" {
			body += "\n\n" + r.Body
		}
		_, err := t.append(Event{Type: Notify, From: from, Contract: r.Contract, Body: body})
		return nil, err
	})
}

// SweepEscalations decides escalated threads the humans left alone for
// longer than EscalationTimeout, in favor of the contract owner's latest
// proposal. Threads where the owner never proposed stay escalated.
func (b *Bus) SweepEscalations() ([]Event, error) {
	cutoff := b.Now().Add(-b.EscalationTimeout).UnixMilli()
	rows, err := b.db.Query(`SELECT id FROM threads WHERE state = 'ESCALATED' AND contract IS NOT NULL AND escalated_at <= ?`, cutoff)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()

	var out []Event
	var errs []error
	for _, id := range ids {
		res, err := b.do(func(t *txn) (*Thread, error) {
			th, err := t.thread(id)
			if err != nil || th.State != StateEscalated {
				return th, err
			}
			c, err := loadContract(t.tx, th.Contract, -1)
			if err != nil {
				return nil, err
			}
			picks, err := queryEvents(t.tx, `thread_id = ? AND from_id = ?
				AND type IN ('PROPOSE','COUNTER') ORDER BY seq DESC LIMIT 1`, id, c.Owner)
			if err != nil || len(picks) == 0 {
				return th, err
			}
			return th, t.decide(th, &picks[0], "timeout:owner")
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("thread %s: %w", id, err))
			continue
		}
		out = append(out, res.Events...)
	}
	return out, errors.Join(errs...)
}

// ---- txn helpers ----

func (t *txn) agent(id string) (*Agent, error) {
	var a Agent
	err := t.tx.QueryRow(`SELECT id, human, mandate FROM agents WHERE id = ?`, id).Scan(&a.ID, &a.Human, &a.Mandate)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, forbidden("unknown agent %q", id)
	}
	return &a, err
}

// participant accepts an agent id or "human:<name>" for a registered human.
func (t *txn) participant(id string) error {
	if human, ok := strings.CutPrefix(id, "human:"); ok {
		var n int
		if err := t.tx.QueryRow(`SELECT COUNT(*) FROM agents WHERE human = ?`, human).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return forbidden("unknown human %q", human)
		}
		return nil
	}
	_, err := t.agent(id)
	return err
}

func (t *txn) thread(id string) (*Thread, error) {
	th, err := loadThread(t.tx, id)
	if err == nil && th == nil {
		err = notFound("unknown thread %q", id)
	}
	return th, err
}

// negotiable loads a thread with a proposal on the table that `from` may
// respond to.
func (t *txn) negotiable(from, thread, verb string) (*Thread, *Event, error) {
	if _, err := t.agent(from); err != nil {
		return nil, nil, err
	}
	th, err := t.thread(thread)
	if err != nil {
		return nil, nil, err
	}
	if th.State != StateProposed && th.State != StateCountered {
		return nil, nil, conflict("cannot %s: thread %q is %s", verb, thread, th.State)
	}
	live, err := loadEvent(t.tx, th.LiveSeq)
	if err != nil {
		return nil, nil, err
	}
	if live.From == from {
		return nil, nil, conflict("cannot %s your own proposal #%d; wait for the peer to respond", verb, live.Seq)
	}
	return th, live, nil
}

// buildProposal applies edits to a contract's head and returns the result
// and its diff. Proposals must be made against the current head.
func (t *txn) buildProposal(contract string, baseRev int, edits []Edit) (content, diff string, err error) {
	c, err := loadContract(t.tx, contract, -1)
	if err != nil {
		return "", "", err
	}
	if baseRev != c.Rev {
		return "", "", conflict("base_rev %d is stale: %s is at rev %d; re-read it with get_contract", baseRev, contract, c.Rev)
	}
	if len(edits) == 0 {
		return "", "", badRequest("at least one edit is required; proposals must be concrete changes")
	}
	if content, err = ApplyEdits(c.Content, edits); err != nil {
		return "", "", badRequest("%v", err)
	}
	if err := Validate(contract, content); err != nil {
		return "", "", badRequest("%v", err)
	}
	if diff = Diff(contract, c.Content, content); diff == "" {
		return "", "", badRequest("edits produce no change")
	}
	return content, diff, nil
}

// decide applies the chosen proposal to its contract, marks the thread
// DECIDED, and flags the opener if they proceeded on an assumption that lost.
func (t *txn) decide(th *Thread, chosen *Event, resolvedBy string) error {
	c, err := loadContract(t.tx, th.Contract, -1)
	if err != nil {
		return err
	}
	content := chosen.ProposedContent
	if chosen.Rev == nil || *chosen.Rev != c.Rev {
		// The contract moved since this proposal; replay its edits on the new head.
		if content, err = ApplyEdits(c.Content, chosen.Edits); err == nil {
			err = Validate(c.Name, content)
		}
		if err != nil {
			return conflict("proposal #%d no longer applies to %s rev %d (%v); counter with edits against rev %d",
				chosen.Seq, c.Name, c.Rev, err, c.Rev)
		}
	}
	newRev := c.Rev + 1
	dec, err := t.append(Event{Thread: th.ID, Type: Decided, From: ServerID, ReplyTo: chosen.Seq,
		Contract: c.Name, Rev: &newRev, Diff: Diff(c.Name, c.Content, content),
		Body: fmt.Sprintf("%s → %s rev %d (proposal #%d by %s, resolved by %s): %s",
			th.Title, c.Name, newRev, chosen.Seq, chosen.From, resolvedBy, chosen.Body)})
	if err != nil {
		return err
	}
	if _, err := t.tx.Exec(`INSERT INTO contract_revs (contract, rev, content, decided_by) VALUES (?, ?, ?, ?)`,
		c.Name, newRev, content, dec.Seq); err != nil {
		return err
	}
	if _, err := t.tx.Exec(`UPDATE contracts SET head_rev = ? WHERE name = ?`, newRev, c.Name); err != nil {
		return err
	}
	th.State, th.DecidedSeq, th.ResolvedBy, th.AwaitingHuman, th.UpdatedSeq =
		StateDecided, chosen.Seq, resolvedBy, "", dec.Seq

	opening, err := queryEvents(t.tx, `thread_id = ? AND type = 'PROPOSE' ORDER BY seq LIMIT 1`, th.ID)
	if err != nil {
		return err
	}
	if len(opening) == 1 && !opening[0].Blocking && opening[0].Seq != chosen.Seq {
		p := opening[0]
		ab, err := t.append(Event{Thread: th.ID, Type: AssumptionBroken, From: ServerID, To: p.From,
			ReplyTo: p.Seq, Contract: c.Name, Rev: &newRev,
			Body: fmt.Sprintf("You proceeded assuming proposal #%d, but %q decided on #%d. Re-read %s rev %d and fix code built on the assumption.",
				p.Seq, th.Title, chosen.Seq, c.Name, newRev)})
		if err != nil {
			return err
		}
		th.UpdatedSeq = ab.Seq
	}
	return t.saveThread(th)
}
