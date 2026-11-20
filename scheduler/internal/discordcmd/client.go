package discordcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	CodeMaxApplicationCommands = 30032

	maxResponseBytes = 4 << 20
	globalBucket     = "global"
)

type Error struct {
	Op          string
	HTTPStatus  int
	Code        int
	RateLimited bool
	RetryAfter  time.Duration
	Global      bool
	Message     string
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("discord ")
	b.WriteString(e.Op)
	if e.HTTPStatus != 0 {
		fmt.Fprintf(&b, ": HTTP %d", e.HTTPStatus)
	}
	if e.RateLimited {
		if e.Global {
			b.WriteString(": global rate limit")
		} else {
			b.WriteString(": rate limited")
		}
		fmt.Fprintf(&b, ", retry after %s", e.RetryAfter.Round(time.Millisecond))
	}
	if e.Code != 0 {
		fmt.Fprintf(&b, ": code %d", e.Code)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) && e != nil {
		return e, true
	}
	return nil, false
}

func eachError(err error, visit func(*Error)) {
	if err == nil {
		return
	}
	if e, ok := err.(*Error); ok && e != nil {
		visit(e)
	}
	switch u := err.(type) {
	case interface{ Unwrap() []error }:
		for _, inner := range u.Unwrap() {
			eachError(inner, visit)
		}
	case interface{ Unwrap() error }:
		eachError(u.Unwrap(), visit)
	}
}

func RateLimit(err error) (*Error, bool) {
	var found *Error
	eachError(err, func(e *Error) {
		if e.RateLimited && (found == nil || e.RetryAfter > found.RetryAfter) {
			found = e
		}
	})
	return found, found != nil
}

func IsRateLimited(err error) bool {
	_, ok := RateLimit(err)
	return ok
}

func IsCapacity(err error) bool {
	capacity := false
	eachError(err, func(e *Error) {
		if e.Code == CodeMaxApplicationCommands {
			capacity = true
		}
	})
	return capacity
}

type Client struct {
	HTTP      *http.Client
	Token     string
	UserAgent string

	mu      sync.Mutex
	resetAt map[string]time.Time
}

func New(httpClient *http.Client, token, userAgent string) *Client {
	return &Client{HTTP: httpClient, Token: token, UserAgent: userAgent}
}

func (c *Client) Self(ctx context.Context) (*discordgo.User, error) {
	var u discordgo.User
	if err := c.do(ctx, "read identity", http.MethodGet, discordgo.EndpointUser("@me"), "users/@me", nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

func (c *Client) List(ctx context.Context, appID, guildID string) ([]*discordgo.ApplicationCommand, error) {
	var out []*discordgo.ApplicationCommand
	err := c.do(ctx, "list commands", http.MethodGet, commandsURL(appID, guildID)+"?with_localizations=true", routeKey("list", guildID), nil, &out)
	return out, err
}

func (c *Client) Get(ctx context.Context, appID, guildID, cmdID string) (*discordgo.ApplicationCommand, error) {
	var out discordgo.ApplicationCommand
	if err := c.do(ctx, "read command", http.MethodGet, commandURL(appID, guildID, cmdID), routeKey("get", guildID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Create(ctx context.Context, appID, guildID string, cmd *discordgo.ApplicationCommand) (*discordgo.ApplicationCommand, error) {
	var out discordgo.ApplicationCommand
	if err := c.do(ctx, "create command", http.MethodPost, commandsURL(appID, guildID), routeKey("create", guildID), cmd, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Edit(ctx context.Context, appID, guildID, cmdID string, cmd *discordgo.ApplicationCommand) (*discordgo.ApplicationCommand, error) {
	var out discordgo.ApplicationCommand
	if err := c.do(ctx, "edit command", http.MethodPatch, commandURL(appID, guildID, cmdID), routeKey("edit", guildID), cmd, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Delete(ctx context.Context, appID, guildID, cmdID string) error {
	return c.do(ctx, "delete command", http.MethodDelete, commandURL(appID, guildID, cmdID), routeKey("delete", guildID), nil, nil)
}

func commandsURL(appID, guildID string) string {
	if guildID == "" {
		return discordgo.EndpointApplicationGlobalCommands(appID)
	}
	return discordgo.EndpointApplicationGuildCommands(appID, guildID)
}

func commandURL(appID, guildID, cmdID string) string {
	if guildID == "" {
		return discordgo.EndpointApplicationGlobalCommand(appID, cmdID)
	}
	return discordgo.EndpointApplicationGuildCommand(appID, guildID, cmdID)
}

func routeKey(op, guildID string) string {
	if guildID == "" {
		return "commands/global/" + op
	}
	return "commands/guild/" + guildID + "/" + op
}

func (c *Client) admit(ctx context.Context, op, route string) error {
	for {
		c.mu.Lock()
		at := c.resetAt[route]
		if g := c.resetAt[globalBucket]; g.After(at) {
			at = g
		}
		c.mu.Unlock()
		wait := time.Until(at)
		if wait <= 0 {
			return nil
		}
		if dl, ok := ctx.Deadline(); ok && at.After(dl) {
			return &Error{Op: op, RateLimited: true, RetryAfter: wait, Message: "the rate-limit bucket resets after this operation's deadline; nothing was sent"}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) note(route string, h http.Header, now time.Time) {
	if h.Get("X-RateLimit-Remaining") != "0" {
		return
	}
	after, ok := parseSeconds(h.Get("X-RateLimit-Reset-After"))
	if !ok {
		return
	}
	c.hold(route, now.Add(after))
}

func (c *Client) hold(route string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resetAt == nil {
		c.resetAt = map[string]time.Time{}
	}
	if at.After(c.resetAt[route]) {
		c.resetAt[route] = at
	}
}

func parseSeconds(raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 7*24*3600 {
		return 0, false
	}
	return time.Duration(v * float64(time.Second)), true
}

func (c *Client) do(ctx context.Context, op, method, url, route string, body any, out any) error {
	if c == nil || c.HTTP == nil {
		return &Error{Op: op, Message: "no Discord HTTP client"}
	}
	if err := c.admit(ctx, op, route); err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return &Error{Op: op, Message: "encode request: " + err.Error()}
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return &Error{Op: op, Message: "build request"}
	}
	if c.Token != "" {
		req.Header.Set("Authorization", c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return &Error{Op: op, Message: c.scrub(transportMessage(err))}
	}
	defer resp.Body.Close()
	now := time.Now()
	c.note(route, resp.Header, now)
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if readErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return &Error{Op: op, HTTPStatus: resp.StatusCode, Message: "read response"}
	}
	if len(data) > maxResponseBytes {
		return &Error{Op: op, HTTPStatus: resp.StatusCode, Message: "response is larger than 4 MiB"}
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		e := rateLimitError(op, resp.Header, data)
		if e.Global {
			c.hold(globalBucket, now.Add(e.RetryAfter))
		} else {
			c.hold(route, now.Add(e.RetryAfter))
		}
		return e
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return c.statusError(op, resp.StatusCode, data)
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &Error{Op: op, HTTPStatus: resp.StatusCode, Message: "decode response: " + err.Error()}
	}
	return nil
}

func transportMessage(err error) string {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) {
		if inner := ue.Unwrap(); inner != nil {
			return inner.Error()
		}
	}
	return err.Error()
}

func (c *Client) scrub(s string) string {
	if c.Token != "" {
		s = strings.ReplaceAll(s, c.Token, "[redacted]")
		s = strings.ReplaceAll(s, strings.TrimPrefix(c.Token, "Bot "), "[redacted]")
	}
	return s
}

type errorBody struct {
	Message    string   `json:"message"`
	Code       int      `json:"code"`
	RetryAfter *float64 `json:"retry_after"`
	Global     bool     `json:"global"`
}

func rateLimitError(op string, h http.Header, data []byte) *Error {
	e := &Error{Op: op, HTTPStatus: http.StatusTooManyRequests, RateLimited: true}
	var body errorBody
	parsed := json.Unmarshal(data, &body) == nil
	if parsed {
		e.Code = body.Code
		e.Message = clip(body.Message)
		e.Global = body.Global
		if body.RetryAfter != nil {
			if d, ok := parseSeconds(strconv.FormatFloat(*body.RetryAfter, 'f', -1, 64)); ok {
				e.RetryAfter = d
			}
		}
	}
	if strings.EqualFold(h.Get("X-RateLimit-Global"), "true") || strings.EqualFold(h.Get("X-RateLimit-Scope"), "global") {
		e.Global = true
	}
	if e.RetryAfter == 0 {
		if d, ok := parseSeconds(h.Get("Retry-After")); ok {
			e.RetryAfter = d
		} else if d, ok := parseSeconds(h.Get("X-RateLimit-Reset-After")); ok {
			e.RetryAfter = d
		}
	}
	if e.RetryAfter <= 0 {
		e.RetryAfter = time.Second
	}
	return e
}

func (c *Client) statusError(op string, status int, data []byte) *Error {
	e := &Error{Op: op, HTTPStatus: status}
	var body errorBody
	if json.Unmarshal(data, &body) == nil {
		e.Code = body.Code
		e.Message = c.scrub(clip(body.Message))
	}
	return e
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}
