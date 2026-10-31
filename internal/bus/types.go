package bus

import (
	"fmt"
	"net/http"
)

type EventType string

const (
	Propose          EventType = "PROPOSE"
	Counter          EventType = "COUNTER"
	Accept           EventType = "ACCEPT"
	Ask              EventType = "ASK"
	Answer           EventType = "ANSWER"
	Notify           EventType = "NOTIFY"
	Escalate         EventType = "ESCALATE"
	Decided          EventType = "DECIDED"           // server: a proposal was applied to a contract
	Resolve          EventType = "RESOLVE"           // human: picked a proposal on an escalated thread
	AssumptionBroken EventType = "ASSUMPTION_BROKEN" // server: a non-blocking proposal lost
)

type State string

const (
	StateOpen      State = "OPEN"
	StateProposed  State = "PROPOSED"
	StateCountered State = "COUNTERED"
	StateEscalated State = "ESCALATED"
	StateDecided   State = "DECIDED"
)

// MaxCounters is how many COUNTERs a thread allows; the next one escalates.
const MaxCounters = 2

// ServerID is the sender of events the server emits on its own.
const ServerID = "server"

// Edit replaces Old with New in a contract. Old must occur exactly once,
// or be empty to append New to the end.
type Edit struct {
	Old string `json:"old"`
	New string `json:"new"`
}

type Event struct {
	Seq             int64     `json:"seq"`
	TS              int64     `json:"ts"`
	Thread          string    `json:"thread,omitempty"`
	Type            EventType `json:"type"`
	From            string    `json:"from"`
	To              string    `json:"to,omitempty"`
	Blocking        bool      `json:"blocking"`
	ReplyTo         int64     `json:"reply_to,omitempty"`
	Contract        string    `json:"contract,omitempty"`
	Rev             *int      `json:"rev,omitempty"`
	Edits           []Edit    `json:"edits,omitempty"`
	ProposedContent string    `json:"proposed_content,omitempty"`
	Diff            string    `json:"diff,omitempty"`
	Body            string    `json:"body,omitempty"`
	MandateRef      string    `json:"mandate_ref,omitempty"`
}

type Thread struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Contract      string `json:"contract,omitempty"`
	OpenedBy      string `json:"opened_by"`
	State         State  `json:"state"`
	Blocking      bool   `json:"blocking"`
	CounterCount  int    `json:"counter_count"`
	LiveSeq       int64  `json:"live_seq,omitempty"`
	DecidedSeq    int64  `json:"decided_seq,omitempty"`
	ResolvedBy    string `json:"resolved_by,omitempty"`
	AwaitingHuman string `json:"awaiting_human,omitempty"`
	EscalatedAt   int64  `json:"escalated_at,omitempty"`
	UpdatedSeq    int64  `json:"updated_seq"`
}

type Agent struct {
	ID      string `json:"id"`
	Human   string `json:"human"`
	Mandate string `json:"mandate"`
}

type Contract struct {
	Name    string `json:"name"`
	Owner   string `json:"owner"`
	Rev     int    `json:"rev"`
	Content string `json:"content,omitempty"`
}

// Result is what every write returns: the thread after the write and the
// events it appended, in order.
type Result struct {
	Thread *Thread `json:"thread,omitempty"`
	Events []Event `json:"events"`
}

// Role selects which events an agent's inbox shows. The liaison negotiates;
// the worker only hears outcomes.
type Role string

const (
	RoleLiaison Role = "liaison"
	RoleWorker  Role = "worker"
)

var roleTypes = map[Role][]EventType{
	RoleLiaison: {Propose, Counter, Accept, Ask, Answer, Escalate, Resolve},
	RoleWorker:  {Decided, AssumptionBroken, Notify},
}

// Error carries the HTTP status the API should answer with. Messages are
// written for the agent reading them, so they say what to do next.
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func errf(status int, format string, a ...any) error {
	return &Error{Status: status, Msg: fmt.Sprintf(format, a...)}
}

func badRequest(format string, a ...any) error { return errf(http.StatusBadRequest, format, a...) }
func conflict(format string, a ...any) error   { return errf(http.StatusConflict, format, a...) }
func forbidden(format string, a ...any) error  { return errf(http.StatusForbidden, format, a...) }
func notFound(format string, a ...any) error   { return errf(http.StatusNotFound, format, a...) }
