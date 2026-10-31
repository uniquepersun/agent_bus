// Package client is a typed HTTP client for the busd REST API, shared by
// bus-mcp, the daemon and the hooks.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"agentbus/internal/bus"
)

type Client struct {
	BaseURL string // e.g. http://192.168.1.20:7070
	From    string // identity sent as X-Bus-From
	HTTP    *http.Client
}

func New(baseURL, from string) *Client {
	// Long enough for a maxed-out wait long-poll.
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), From: from, HTTP: &http.Client{Timeout: 70 * time.Second}}
}

// do sends a JSON request. Server errors come back as *bus.Error so callers
// can show agents the server's message as-is.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(in); err != nil {
			return err
		}
		body = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-Bus-From", c.From)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("agent bus unreachable at %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		raw, _ := io.ReadAll(resp.Body)
		if json.Unmarshal(raw, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(raw))
		}
		return &bus.Error{Status: resp.StatusCode, Msg: e.Error}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Act runs a thread action: propose, counter, accept, ask, answer, escalate
// or resolve.
func (c *Client) Act(ctx context.Context, thread, action string, req any) (*bus.Result, error) {
	var res bus.Result
	err := c.do(ctx, "POST", "/api/threads/"+url.PathEscape(thread)+"/"+action, req, &res)
	return &res, err
}

func (c *Client) Notify(ctx context.Context, req bus.NotifyReq) (*bus.Result, error) {
	var res bus.Result
	err := c.do(ctx, "POST", "/api/notify", req, &res)
	return &res, err
}

func (c *Client) Thread(ctx context.Context, id string) (*bus.Thread, []bus.Event, error) {
	var out struct {
		Thread *bus.Thread
		Events []bus.Event
	}
	err := c.do(ctx, "GET", "/api/threads/"+url.PathEscape(id), nil, &out)
	return out.Thread, out.Events, err
}

// Contract fetches a contract at rev, or at head when rev < 0.
func (c *Client) Contract(ctx context.Context, name string, rev int) (*bus.Contract, error) {
	path := "/api/contracts/" + url.PathEscape(name)
	if rev >= 0 {
		path += "?rev=" + strconv.Itoa(rev)
	}
	var out bus.Contract
	err := c.do(ctx, "GET", path, nil, &out)
	return &out, err
}

func (c *Client) Contracts(ctx context.Context) ([]bus.Contract, error) {
	var out []bus.Contract
	err := c.do(ctx, "GET", "/api/contracts", nil, &out)
	return out, err
}

func (c *Client) Decisions(ctx context.Context, contract string, since int64) ([]bus.Event, error) {
	q := url.Values{}
	if contract != "" {
		q.Set("contract", contract)
	}
	if since > 0 {
		q.Set("since", strconv.FormatInt(since, 10))
	}
	var out []bus.Event
	err := c.do(ctx, "GET", "/api/decisions?"+q.Encode(), nil, &out)
	return out, err
}

func (c *Client) Inbox(ctx context.Context, role bus.Role) ([]bus.Event, error) {
	var out struct{ Events []bus.Event }
	err := c.do(ctx, "GET", "/api/inbox?role="+url.QueryEscape(string(role)), nil, &out)
	return out.Events, err
}

func (c *Client) Ack(ctx context.Context, role bus.Role, seq int64) error {
	return c.do(ctx, "POST", "/api/ack", map[string]any{"role": role, "seq": seq}, nil)
}

// Wait long-polls a thread for replies from others after seq `after` (or
// after the caller's own latest event when after <= 0). It returns no
// events on timeout.
func (c *Client) Wait(ctx context.Context, thread string, after int64, timeout time.Duration) ([]bus.Event, error) {
	q := url.Values{"timeout": {strconv.Itoa(int(timeout.Seconds()))}}
	if after > 0 {
		q.Set("after", strconv.FormatInt(after, 10))
	}
	var out struct{ Events []bus.Event }
	err := c.do(ctx, "GET", "/api/threads/"+url.PathEscape(thread)+"/wait?"+q.Encode(), nil, &out)
	return out.Events, err
}

// SetMandate uploads mandate text for agent id.
func (c *Client) SetMandate(ctx context.Context, id, mandate string) error {
	req, err := http.NewRequestWithContext(ctx, "PUT", c.BaseURL+"/api/agents/"+url.PathEscape(id)+"/mandate", strings.NewReader(mandate))
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return &bus.Error{Status: resp.StatusCode, Msg: strings.TrimSpace(string(raw))}
	}
	return nil
}
