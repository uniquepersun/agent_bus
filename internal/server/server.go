// Package server exposes the bus over REST and a WebSocket event feed.
//
// Callers identify themselves with the X-Bus-From header: an agent id
// ("backend@abhay") or a human ("human:abhay"). There is no auth; this is a
// hackathon LAN service.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"agentbus/internal/bus"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const fromHeader = "X-Bus-From"

// MaxWait caps long-polls below typical MCP tool-call timeouts.
const MaxWait = 50 * time.Second

type Server struct {
	bus *bus.Bus
	mux *http.ServeMux
}

func New(b *bus.Bus) *Server {
	s := &Server{bus: b, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /api/state", s.state)
	s.mux.HandleFunc("GET /api/threads", s.threads)
	s.mux.HandleFunc("GET /api/threads/{id}", s.thread)
	s.mux.HandleFunc("GET /api/threads/{id}/wait", s.wait)
	s.mux.HandleFunc("POST /api/threads/{id}/{action}", s.threadAction)
	s.mux.HandleFunc("POST /api/notify", s.notify)
	s.mux.HandleFunc("GET /api/contracts", s.contracts)
	s.mux.HandleFunc("GET /api/contracts/{name}", s.contract)
	s.mux.HandleFunc("GET /api/decisions", s.decisions)
	s.mux.HandleFunc("GET /api/inbox", s.inbox)
	s.mux.HandleFunc("POST /api/ack", s.ack)
	s.mux.HandleFunc("PUT /api/agents/{id}/mandate", s.setMandate)
	s.mux.HandleFunc("GET /ws", s.ws)
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "agent bus is running; dashboard coming soon. Feed: /ws, state: /api/state\n")
	})
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	agents, err := s.bus.Agents()
	if err != nil {
		writeErr(w, err)
		return
	}
	contracts, err := s.bus.Contracts()
	if err != nil {
		writeErr(w, err)
		return
	}
	threads, err := s.bus.Threads()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"agents": agents, "contracts": contracts, "threads": threads})
}

func (s *Server) threads(w http.ResponseWriter, r *http.Request) {
	threads, err := s.bus.Threads()
	reply(w, threads, err)
}

func (s *Server) thread(w http.ResponseWriter, r *http.Request) {
	th, evs, err := s.bus.Thread(r.PathValue("id"))
	reply(w, map[string]any{"thread": th, "events": evs}, err)
}

// wait long-polls: GET /api/threads/{id}/wait?after=<seq>&timeout=<seconds>
func (s *Server) wait(w http.ResponseWriter, r *http.Request) {
	from, ok := identity(w, r)
	if !ok {
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	timeout := MaxWait
	if secs, err := strconv.Atoi(r.URL.Query().Get("timeout")); err == nil && secs > 0 && time.Duration(secs)*time.Second < MaxWait {
		timeout = time.Duration(secs) * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	evs, err := s.bus.WaitForReply(ctx, from, r.PathValue("id"), after)
	reply(w, map[string]any{"events": evs, "timeout": err == nil && len(evs) == 0}, err)
}

func (s *Server) threadAction(w http.ResponseWriter, r *http.Request) {
	from, ok := identity(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var res *bus.Result
	var err error
	switch r.PathValue("action") {
	case "propose":
		res, err = call(r, func(req bus.ProposeReq) (*bus.Result, error) { return s.bus.Propose(from, id, req) })
	case "counter":
		res, err = call(r, func(req bus.CounterReq) (*bus.Result, error) { return s.bus.Counter(from, id, req) })
	case "accept":
		res, err = call(r, func(req bus.AcceptReq) (*bus.Result, error) { return s.bus.Accept(from, id, req) })
	case "ask":
		res, err = call(r, func(req bus.AskReq) (*bus.Result, error) { return s.bus.Ask(from, id, req) })
	case "answer":
		res, err = call(r, func(req bus.AnswerReq) (*bus.Result, error) { return s.bus.Answer(from, id, req) })
	case "escalate":
		res, err = call(r, func(req bus.EscalateReq) (*bus.Result, error) { return s.bus.Escalate(from, id, req) })
	case "resolve":
		res, err = call(r, func(req bus.ResolveReq) (*bus.Result, error) { return s.bus.Resolve(from, id, req) })
	default:
		http.NotFound(w, r)
		return
	}
	reply(w, res, err)
}

func (s *Server) notify(w http.ResponseWriter, r *http.Request) {
	from, ok := identity(w, r)
	if !ok {
		return
	}
	res, err := call(r, func(req bus.NotifyReq) (*bus.Result, error) { return s.bus.Notify(from, req) })
	reply(w, res, err)
}

func (s *Server) contracts(w http.ResponseWriter, r *http.Request) {
	cs, err := s.bus.Contracts()
	reply(w, cs, err)
}

// contract returns a contract at head, or at ?rev=N.
func (s *Server) contract(w http.ResponseWriter, r *http.Request) {
	rev := -1
	if v := r.URL.Query().Get("rev"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, &bus.Error{Status: http.StatusBadRequest, Msg: "rev must be a non-negative integer"})
			return
		}
		rev = n
	}
	c, err := s.bus.Contract(r.PathValue("name"), rev)
	reply(w, c, err)
}

func (s *Server) decisions(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	evs, err := s.bus.Decisions(r.URL.Query().Get("contract"), after)
	reply(w, evs, err)
}

// inbox: GET /api/inbox?role=liaison|worker
func (s *Server) inbox(w http.ResponseWriter, r *http.Request) {
	from, ok := identity(w, r)
	if !ok {
		return
	}
	evs, err := s.bus.Inbox(from, bus.Role(r.URL.Query().Get("role")))
	reply(w, map[string]any{"events": evs}, err)
}

func (s *Server) ack(w http.ResponseWriter, r *http.Request) {
	from, ok := identity(w, r)
	if !ok {
		return
	}
	var req struct {
		Role bus.Role `json:"role"`
		Seq  int64    `json:"seq"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, &bus.Error{Status: http.StatusBadRequest, Msg: "invalid JSON: " + err.Error()})
		return
	}
	reply(w, map[string]bool{"ok": true}, s.bus.Ack(from, req.Role, req.Seq))
}

// setMandate takes the raw mandate.md text as the body.
func (s *Server) setMandate(w http.ResponseWriter, r *http.Request) {
	text, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeErr(w, err)
		return
	}
	reply(w, map[string]bool{"ok": true}, s.bus.SetMandate(r.PathValue("id"), string(text)))
}

// ws streams ledger events as JSON, starting after ?since=<seq>. Clients
// reconnect with the last seq they saw to resume without gaps.
func (s *Server) ws(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := c.CloseRead(r.Context())

	// Subscribe before reading the backlog so nothing falls in the gap.
	ch, cancel := s.bus.Subscribe()
	defer cancel()
	backlog, err := s.bus.Events(since)
	if err != nil {
		c.Close(websocket.StatusInternalError, "reading ledger failed")
		return
	}
	last := since
	for _, e := range backlog {
		if err := wsjson.Write(ctx, c, e); err != nil {
			return
		}
		last = e.Seq
	}
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				c.Close(websocket.StatusTryAgainLater, "fell behind; reconnect with ?since=last seq")
				return
			}
			if e.Seq <= last {
				continue
			}
			if err := wsjson.Write(ctx, c, e); err != nil {
				return
			}
			last = e.Seq
		}
	}
}

// ---- helpers ----

func identity(w http.ResponseWriter, r *http.Request) (string, bool) {
	from := r.Header.Get(fromHeader)
	if from == "" {
		writeErr(w, &bus.Error{Status: http.StatusUnauthorized, Msg: fromHeader + " header is required"})
		return "", false
	}
	return from, true
}

func call[T any](r *http.Request, fn func(T) (*bus.Result, error)) (*bus.Result, error) {
	var req T
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		return nil, &bus.Error{Status: http.StatusBadRequest, Msg: "invalid JSON: " + err.Error()}
	}
	return fn(req)
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, v)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var be *bus.Error
	if errors.As(err, &be) {
		status = be.Status
	} else {
		log.Printf("internal error: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
