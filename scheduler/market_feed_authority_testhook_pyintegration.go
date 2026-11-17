//go:build pyintegration

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"trading-scheduler/internal/egresspolicy"
)

const (
	feedWeightTestDirEnv   = "GO_TRADER_TEST_FEED_WEIGHT_DIR"
	feedWeightTestClockEnv = "GO_TRADER_TEST_FEED_WEIGHT_CLOCK"
	feedWeightTestKillEnv  = "GO_TRADER_TEST_FEED_WEIGHT_KILL"
	feedWeightTestFailEnv  = "GO_TRADER_TEST_FEED_WEIGHT_FAIL"
	feedWeightTestOwnerEnv = "GO_TRADER_TEST_FEED_WEIGHT_OWNER"
	feedWeightTestOwnerHdr = "X-Test-Owner"
)

type feedTestOwnerTransport struct {
	owner string
	base  http.RoundTripper
}

func (t feedTestOwnerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(feedWeightTestOwnerHdr, t.owner)
	return t.base.RoundTrip(r)
}

type feedFileClockState struct {
	Boot         string `json:"boot"`
	OffsetNs     int64  `json:"offset_ns"`
	WallOffsetNs int64  `json:"wall_offset_ns"`
	Fail         bool   `json:"fail"`
}

type feedFileBootClock struct{ path string }

func (c feedFileBootClock) state() (feedFileClockState, error) {
	var st feedFileClockState
	raw, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, err
	}
	if st.Fail {
		return st, fmt.Errorf("fixture clock %s is set to fail", c.path)
	}
	return st, nil
}

func (c feedFileBootClock) BootID() (string, error) {
	st, err := c.state()
	if err != nil {
		return "", err
	}
	if st.Boot != "" {
		return st.Boot, nil
	}
	return readFeedBootIdentity()
}

func (c feedFileBootClock) Elapsed() (int64, error) {
	st, err := c.state()
	if err != nil {
		return 0, err
	}
	ns, err := readFeedBootElapsed()
	if err != nil {
		return 0, err
	}
	return ns + st.OffsetNs, nil
}

func (c feedFileBootClock) Wall() time.Time {
	st, _ := c.state()
	return time.Now().UTC().Add(time.Duration(st.OffsetNs + st.WallOffsetNs))
}

type feedTestPoint struct {
	name  string
	nth   int
	count int
}

func parseFeedTestPoint(raw string) *feedTestPoint {
	if raw == "" {
		return nil
	}
	p := &feedTestPoint{name: raw, nth: 1}
	if i := strings.LastIndex(raw, "@"); i > 0 {
		if n, err := strconv.Atoi(raw[i+1:]); err == nil && n > 0 {
			p.name, p.nth = raw[:i], n
		}
	}
	return p
}

func init() {
	if d := os.Getenv(feedWeightTestDirEnv); d != "" {
		feedAuthorityDirOverride = d
	}
	if p := os.Getenv(feedWeightTestClockEnv); p != "" {
		newFeedBootClock = func() feedBootClock { return feedFileBootClock{path: p} }
	}
	if o := os.Getenv(feedWeightTestOwnerEnv); o != "" {
		if os.Getenv(egresspolicy.EnvProxy) != "" {
			fmt.Fprintf(os.Stderr, "%s cannot run with %s\n", feedWeightTestOwnerEnv, egresspolicy.EnvProxy)
			os.Exit(1)
		}
		http.DefaultTransport = feedTestOwnerTransport{owner: o, base: http.DefaultTransport}
	}
	kill := parseFeedTestPoint(os.Getenv(feedWeightTestKillEnv))
	fail := parseFeedTestPoint(os.Getenv(feedWeightTestFailEnv))
	if kill == nil && fail == nil {
		return
	}
	var mu sync.Mutex
	feedAuthorityHook = func(point string) error {
		mu.Lock()
		defer mu.Unlock()
		if kill != nil && point == kill.name {
			kill.count++
			if kill.count == kill.nth {
				fmt.Fprintf(os.Stderr, "[feed-weight-test] SIGKILL at %s (occurrence %d)\n", point, kill.count)
				syscall.Kill(os.Getpid(), syscall.SIGKILL)
				select {}
			}
		}
		if fail != nil && point == fail.name {
			fail.count++
			if fail.count >= fail.nth {
				return fmt.Errorf("injected failure at %s", point)
			}
		}
		return nil
	}
}
