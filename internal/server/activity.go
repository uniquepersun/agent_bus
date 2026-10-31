package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"agentbus/internal/bus"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Activity is a live, unrecorded report of what an agent is doing: a tool
// call, a liaison starting or finishing. It is not part of the ledger.
type Activity struct {
	TS   int64  `json:"ts"`
	From string `json:"from"` // agent id
	Role string `json:"role"` // "worker" or "liaison"
	Kind string `json:"kind"` // "tool", "message" or "status"
	Text string `json:"text"`
}

// activityHub keeps the most recent activity for late joiners and fans new
// activity out to subscribers.
type activityHub struct {
	mu     sync.Mutex
	recent []Activity
	subs   map[chan Activity]struct{}
}

const activityKeep = 200

func newActivityHub() *activityHub {
	return &activityHub{subs: map[chan Activity]struct{}{}}
}

func (h *activityHub) publish(a Activity) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recent = append(h.recent, a)
	if len(h.recent) > activityKeep {
		h.recent = h.recent[len(h.recent)-activityKeep:]
	}
	for ch := range h.subs {
		select {
		case ch <- a:
		default: // slow subscriber: drop rather than block agents
		}
	}
}

func (h *activityHub) subscribe(recent int) ([]Activity, chan Activity, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan Activity, 256)
	h.subs[ch] = struct{}{}
	start := max(0, len(h.recent)-recent)
	backlog := append([]Activity(nil), h.recent[start:]...)
	return backlog, ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs, ch)
	}
}

// postActivity: POST /api/activity {role, kind, text} from a registered agent.
func (s *Server) postActivity(w http.ResponseWriter, r *http.Request) {
	from, ok := identity(w, r)
	if !ok {
		return
	}
	var a Activity
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&a); err != nil {
		writeErr(w, &bus.Error{Status: http.StatusBadRequest, Msg: "invalid JSON: " + err.Error()})
		return
	}
	agents, err := s.bus.Agents()
	if err != nil {
		writeErr(w, err)
		return
	}
	known := false
	for _, ag := range agents {
		known = known || ag.ID == from
	}
	if !known {
		writeErr(w, &bus.Error{Status: http.StatusForbidden, Msg: "unknown agent " + from})
		return
	}
	if len(a.Text) > 500 {
		a.Text = a.Text[:497] + "..."
	}
	a.From, a.TS = from, time.Now().UnixMilli()
	s.activity.publish(a)
	writeJSON(w, map[string]bool{"ok": true})
}

// activityFeed streams activity: the last ?recent=N entries (default 50),
// then live.
func (s *Server) activityFeed(w http.ResponseWriter, r *http.Request) {
	recent := 50
	if n, err := strconv.Atoi(r.URL.Query().Get("recent")); err == nil && n >= 0 {
		recent = n
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := c.CloseRead(r.Context())
	backlog, ch, cancel := s.activity.subscribe(recent)
	defer cancel()
	for _, a := range backlog {
		if wsjson.Write(ctx, c, a) != nil {
			return
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case a := <-ch:
			if wsjson.Write(ctx, c, a) != nil {
				return
			}
		}
	}
}
