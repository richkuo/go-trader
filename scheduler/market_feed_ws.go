package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	feedSocketPingEvery    = 30 * time.Second
	feedSocketReadDeadline = 90 * time.Second
	feedSocketBackoffMin   = time.Second
	feedSocketBackoffMax   = 30 * time.Second
	feedSocketDialTimeout  = 15 * time.Second
)

var hlFeedWebsocketURLOverride string

func hlFeedWebsocketURL() string {
	if hlFeedWebsocketURLOverride != "" {
		return hlFeedWebsocketURLOverride
	}
	base := hlMainnetURL
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	}
	return strings.TrimRight(base, "/") + "/ws"
}

type feedSocketEnvelope struct {
	Channel string          `json:"channel"`
	Data    json.RawMessage `json:"data"`
}

type feedSocketCandle struct {
	Coin     string `json:"s"`
	Interval string `json:"i"`
}

type feedSocketMids struct {
	Mids map[string]string `json:"mids"`
}

func feedNextBackoff(current time.Duration) time.Duration {
	if current <= 0 {
		return feedSocketBackoffMin
	}
	next := current * 2
	if next > feedSocketBackoffMax {
		next = feedSocketBackoffMax
	}
	return next
}

func feedJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + time.Duration(rand.Int63n(int64(d/2)+1))
}

func (o *marketFeedOwner) Run(ctx context.Context) {
	backoff := time.Duration(0)
	for {
		if ctx.Err() != nil {
			return
		}
		err := o.runOneSocketSession(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			o.logf("[feed] socket session ended: %v", err)
		}
		backoff = feedNextBackoff(backoff)
		wait := feedJitter(backoff)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (o *marketFeedOwner) runOneSocketSession(ctx context.Context) error {
	dialer := &websocket.Dialer{HandshakeTimeout: feedSocketDialTimeout}
	dialCtx, cancelDial := context.WithTimeout(ctx, feedSocketDialTimeout)
	conn, _, err := dialer.DialContext(dialCtx, hlFeedWebsocketURL(), nil)
	cancelDial()
	if err != nil {
		return fmt.Errorf("dial %s: %w", hlFeedWebsocketURL(), err)
	}

	sessionCtx, cancelSession := context.WithCancel(ctx)
	defer cancelSession()

	var closeOnce sync.Once
	closeConn := func() { closeOnce.Do(func() { conn.Close() }) }
	defer closeConn()

	go func() {
		<-sessionCtx.Done()
		closeConn()
	}()

	var writeMu sync.Mutex
	writeJSON := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteJSON(v)
	}

	subscribed := make(map[marketFeedKey]bool)
	midsSubscribed := false
	syncSubscriptions := func() error {
		keys, coins, _ := o.Subscriptions()
		if len(coins) > 0 && !midsSubscribed {
			if err := writeJSON(map[string]any{
				"method":       "subscribe",
				"subscription": map[string]any{"type": "allMids"},
			}); err != nil {
				return err
			}
			midsSubscribed = true
		}
		for _, key := range keys {
			if subscribed[key] {
				continue
			}
			if err := writeJSON(map[string]any{
				"method": "subscribe",
				"subscription": map[string]any{
					"type":     "candle",
					"coin":     key.Symbol,
					"interval": key.Timeframe,
				},
			}); err != nil {
				return err
			}
			subscribed[key] = true
		}
		return nil
	}

	if err := syncSubscriptions(); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	o.SetConnected(true)
	defer o.SetConnected(false)
	go o.repairAfterConnect(sessionCtx)

	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(feedSocketReadDeadline))
	})

	go func() {
		ticker := time.NewTicker(feedSocketPingEvery)
		defer ticker.Stop()
		for {
			select {
			case <-sessionCtx.Done():
				return
			case <-ticker.C:
				if err := writeJSON(map[string]any{"method": "ping"}); err != nil {
					cancelSession()
					return
				}
				writeMu.Lock()
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
				writeMu.Unlock()
				if serr := syncSubscriptions(); serr != nil {
					cancelSession()
					return
				}
			}
		}
	}()

	for {
		if err := conn.SetReadDeadline(time.Now().Add(feedSocketReadDeadline)); err != nil {
			return err
		}
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		o.handleSocketMessage(payload)
	}
}

func (o *marketFeedOwner) handleSocketMessage(payload []byte) {
	var env feedSocketEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return
	}
	switch env.Channel {
	case "candle":
		var head feedSocketCandle
		if err := json.Unmarshal(env.Data, &head); err != nil {
			return
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(env.Data, &fields); err != nil {
			return
		}
		raw, err := hlCandleRawFromFields(fields)
		if err != nil {
			o.logf("[feed] %s/%s: dropped a malformed candle message: %v", head.Coin, head.Interval, err)
			return
		}
		o.IngestSocketCandle(feedKeyFor(head.Coin, head.Interval), raw, o.now())
	case "allMids":
		var mids feedSocketMids
		if err := json.Unmarshal(env.Data, &mids); err != nil {
			return
		}
		parsed := make(map[string]float64, len(mids.Mids))
		for coin, pxStr := range mids.Mids {
			px, perr := parseFeedMidPrice(pxStr)
			if perr != nil {
				continue
			}
			parsed[coin] = px
		}
		o.IngestMids(parsed, o.now(), string(feedBarSourceSocket))
	}
}

func parseFeedMidPrice(raw string) (float64, error) {
	return strconv.ParseFloat(strings.TrimSpace(raw), 64)
}

func (o *marketFeedOwner) repairAfterConnect(ctx context.Context) {
	for _, key := range o.keysAwaitingRepair() {
		if ctx.Err() != nil {
			return
		}
		if err := o.fetchAndMerge(ctx, key, feedRestRepair); err != nil {
			o.logf("[feed] %s: reconnect repair failed: %v", key, err)
			o.raiseAlert(key, "repair_failed", err.Error())
			continue
		}
		o.MarkKeyRecovered(key)
	}
}
