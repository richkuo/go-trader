//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type fpArrival struct {
	at     time.Time
	weight int
	typ    string
	owner  string
}

type fpPark struct {
	owner    string
	taken    bool
	entered  chan struct{}
	release  chan struct{}
	recorded chan int
}

type fpVenue struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	arrivals []fpArrival
	hold     chan struct{}
	entered  chan struct{}
	status   map[string]int
	park     *fpPark
}

func newFPVenue(t *testing.T) *fpVenue {
	v := &fpVenue{t: t, status: map[string]int{}}
	v.srv = httptest.NewServer(http.HandlerFunc(v.handle))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *fpVenue) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		Type string `json:"type"`
		Req  struct {
			Coin      string `json:"coin"`
			Interval  string `json:"interval"`
			StartTime int64  `json:"startTime"`
			EndTime   int64  `json:"endTime"`
		} `json:"req"`
	}
	json.Unmarshal(raw, &body)
	now := time.Now()
	var payload []byte
	rows := 0
	switch body.Type {
	case "candleSnapshot":
		step, ok := hlCandleIntervalMs(body.Req.Interval)
		if !ok || step <= 0 {
			step = 60_000
		}
		out := []map[string]any{}
		first := (body.Req.StartTime + step - 1) / step * step
		for open := first; open <= body.Req.EndTime && len(out) < hlCandleRowCap; open += step {
			out = append(out, map[string]any{"t": open, "T": open + step - 1, "o": "100", "h": "102", "l": "99", "c": "101", "v": "5", "s": body.Req.Coin, "i": body.Req.Interval, "n": 1})
		}
		rows = len(out)
		payload, _ = json.Marshal(out)
	case "allMids":
		payload = []byte(`{"BTC":"101.5"}`)
	case "metaAndAssetCtxs":
		payload = []byte(`[{"universe":[{"name":"BTC"}]},[{"funding":"0.0001"}]]`)
	case "meta":
		payload = []byte(hlMetaBody(`{"name":"BTC","szDecimals":5}`))
	default:
		payload = []byte(`[]`)
	}
	weight, err := hlInfoWeight(body.Type, rows)
	if err != nil {
		weight = 20
	}
	owner := r.Header.Get(feedWeightTestOwnerHdr)
	v.mu.Lock()
	park := v.park
	if park != nil && (park.taken || park.owner != owner || body.Type != "candleSnapshot") {
		park = nil
	}
	if park != nil {
		park.taken = true
	}
	v.mu.Unlock()
	if park != nil {
		close(park.entered)
		<-park.release
	}
	v.mu.Lock()
	v.arrivals = append(v.arrivals, fpArrival{at: now, weight: weight, typ: body.Type, owner: owner})
	index := len(v.arrivals) - 1
	hold, entered := v.hold, v.entered
	st := v.status[body.Type]
	v.mu.Unlock()
	if park != nil {
		park.recorded <- index
	}
	if hold != nil && body.Type == "candleSnapshot" {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	if st != 0 {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(st)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(payload)
}

func (v *fpVenue) snapshot() []fpArrival {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]fpArrival(nil), v.arrivals...)
}

func (v *fpVenue) sentBy(owner string) int {
	v.t.Helper()
	n := 0
	for i, a := range v.snapshot() {
		if a.owner == "" {
			v.t.Fatalf("venue arrival %d (%s) carries no owner tag, so it cannot be attributed to a process", i, a.typ)
		}
		if a.owner == owner {
			n++
		}
	}
	return n
}

func (v *fpVenue) maxWindow() int {
	arr := v.snapshot()
	sort.Slice(arr, func(i, j int) bool { return arr[i].at.Before(arr[j].at) })
	best := 0
	for i := range arr {
		sum := 0
		for j := i; j < len(arr) && arr[j].at.Sub(arr[i].at) < feedBudgetWindow; j++ {
			sum += arr[j].weight
		}
		best = max(best, sum)
	}
	return best
}

type fpEnv struct {
	t     *testing.T
	bin   string
	root  string
	dir   string
	auth  string
	clock string
	venue *fpVenue
}

func newFPEnv(t *testing.T) *fpEnv {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	dir, err := os.MkdirTemp("", "fpw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	e := &fpEnv{t: t, bin: pyintegrationBinary(t), root: filepath.Dir(filepath.Dir(thisFile)), dir: dir,
		auth: filepath.Join(dir, "feed-weight"), clock: filepath.Join(dir, "clock.json"), venue: newFPVenue(t)}
	e.setOffset(0)
	return e
}

func (e *fpEnv) setOffset(d time.Duration) {
	e.t.Helper()
	blob, _ := json.Marshal(map[string]any{"offset_ns": int64(d)})
	tmp := e.clock + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Rename(tmp, e.clock); err != nil {
		e.t.Fatal(err)
	}
}

func (e *fpEnv) feedConfig(name string, coins int, weightPerMinute, weightStartup, reserve int) string {
	e.t.Helper()
	sock := filepath.Join(e.dir, name+".sock")
	strategies := []any{}
	for i := 0; i < coins; i++ {
		coin := fmt.Sprintf("%s%02d", strings.ToUpper(name), i)
		strategies = append(strategies, map[string]any{"id": "hl-" + strings.ToLower(coin), "type": "perps", "platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py", "args": []any{"sma_crossover", coin, "1h", "--mode=paper"}, "capital": 1000, "max_drawdown_pct": 50})
	}
	consumer := filepath.Join(e.dir, name+"-consumer.json")
	blob, _ := json.Marshal(map[string]any{"config_version": CurrentConfigVersion, "market_feed": "shared",
		"db_file": filepath.Join(e.dir, name+"-state.db"), "log_dir": filepath.Join(e.dir, "logs"),
		"shared_market_feed": map[string]any{"primary_socket": sock}, "strategies": strategies})
	if err := os.WriteFile(consumer, blob, 0o600); err != nil {
		e.t.Fatal(err)
	}
	cfg := filepath.Join(e.dir, name+"-feed.json")
	blob, _ = json.Marshal(map[string]any{"config_version": CurrentConfigVersion, "role": "feed", "status_port": htfITFreePort(e.t),
		"feed": map[string]any{"source": "rest", "socket_path": sock, "consumer_configs": []string{consumer},
			"request_budget": map[string]any{"per_minute": 1000, "startup": 1000, "weight_per_minute": weightPerMinute, "weight_startup": weightStartup, "weight_reserve": reserve}}})
	if err := os.WriteFile(cfg, blob, 0o600); err != nil {
		e.t.Fatal(err)
	}
	return cfg
}

func (e *fpEnv) env(extra ...string) []string {
	env := append(os.Environ(),
		manualRegimeTestHLInfoEnv+"="+e.venue.srv.URL,
		feedWeightTestDirEnv+"="+e.auth,
		feedWeightTestClockEnv+"="+e.clock,
	)
	return append(env, extra...)
}

func (e *fpEnv) processEnv(name string, extra ...string) []string {
	env := []string{}
	for _, kv := range e.env(extra...) {
		if !strings.HasPrefix(kv, feedWeightTestOwnerEnv+"=") {
			env = append(env, kv)
		}
	}
	return append(env, feedWeightTestOwnerEnv+"="+name)
}

type fpProc struct {
	t      *testing.T
	name   string
	cmd    *exec.Cmd
	mu     sync.Mutex
	out    bytes.Buffer
	exited chan struct{}
	code   int
}

func (p *fpProc) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.Write(b)
}

func (p *fpProc) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.String()
}

func (p *fpProc) waitFor(what string, timeout time.Duration) {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for !strings.Contains(p.output(), what) {
		select {
		case <-p.exited:
			if !strings.Contains(p.output(), what) {
				p.t.Fatalf("%s exited (code %d) before %q:\n%s", p.name, p.code, what, p.output())
			}
		default:
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("%s: timed out waiting for %q:\n%s", p.name, what, p.output())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (p *fpProc) waitExit(timeout time.Duration) int {
	p.t.Helper()
	select {
	case <-p.exited:
		return p.code
	case <-time.After(timeout):
		p.t.Fatalf("%s did not exit:\n%s", p.name, p.output())
	}
	return 0
}

func (p *fpProc) stop() {
	select {
	case <-p.exited:
		return
	default:
	}
	p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(20 * time.Second):
		p.cmd.Process.Kill()
		<-p.exited
	}
}

func (p *fpProc) kill() {
	p.cmd.Process.Kill()
	<-p.exited
}

func (e *fpEnv) start(name, cfg string, extra ...string) *fpProc {
	e.t.Helper()
	p := &fpProc{t: e.t, name: name, exited: make(chan struct{})}
	p.cmd = exec.Command(e.bin, "--config", cfg)
	p.cmd.Dir = e.root
	p.cmd.Env = e.processEnv(name, extra...)
	p.cmd.Stdout = p
	p.cmd.Stderr = p
	if err := p.cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	go func() {
		err := p.cmd.Wait()
		if ee, ok := err.(*exec.ExitError); ok {
			p.code = ee.ExitCode()
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				p.code = 128 + int(ws.Signal())
			}
		}
		close(p.exited)
	}()
	e.t.Cleanup(p.stop)
	return p
}

func (e *fpEnv) record() *feedAuthRecord {
	e.t.Helper()
	rec, state, detail, r := loadFeedAuthRecord(filepath.Join(e.auth, feedAuthorityRecordName), true)
	if r != nil || state != feedAuthLoadOK {
		e.t.Fatalf("record: state %d %s %v", state, detail, r)
	}
	return rec
}

func (e *fpEnv) ownerKey(cfg string) string { return feedAuthorityOwnerKey(feedAuthorityRoleFeed, cfg) }

func (e *fpEnv) install(t *testing.T, cfg string) {
	t.Helper()
	p := e.start("install", cfg)
	p.waitFor("[feed-weight-shared] registered owner=", 60*time.Second)
	e.setOffset(feedBudgetWindow + time.Second)
	p.waitFor("reason=initial_install ended", 30*time.Second)
	p.stop()
}

func (e *fpEnv) installLeavingALateArrival(t *testing.T, cfg string) func() int {
	t.Helper()
	park := &fpPark{owner: "install", entered: make(chan struct{}), release: make(chan struct{}), recorded: make(chan int, 1)}
	var once sync.Once
	release := func() { once.Do(func() { close(park.release) }) }
	t.Cleanup(release)
	p := e.start("install", cfg)
	p.waitFor("[feed-weight-shared] registered owner=", 60*time.Second)
	e.venue.mu.Lock()
	e.venue.park = park
	e.venue.mu.Unlock()
	e.setOffset(feedBudgetWindow + time.Second)
	p.waitFor("reason=initial_install ended", 30*time.Second)
	select {
	case <-park.entered:
	case <-time.After(60 * time.Second):
		t.Fatalf("the install process sent no candle request to hold in the venue handler:\n%s", p.output())
	}
	p.stop()
	return func() int {
		release()
		select {
		case i := <-park.recorded:
			return i
		case <-time.After(30 * time.Second):
			t.Fatalf("the held install request was never recorded")
		}
		return -1
	}
}

func TestFeedWeightSharedProcesses(t *testing.T) {
	t.Run("TwoOwnersAndADeadOwnersDebtStayWithinTheSharedCapacity", func(t *testing.T) {
		e := newFPEnv(t)
		cCfg := e.feedConfig("c", 40, 800, 200, 200)
		e.install(t, cCfg)
		c := e.start("c", cCfg)
		c.waitFor("[feed] serving:", 120*time.Second)
		c.kill()
		aCfg := e.feedConfig("a", 30, 500, 100, 200)
		bCfg := e.feedConfig("b", 30, 300, 100, 100)
		a := e.start("a", aCfg)
		b := e.start("b", bCfg)
		a.waitFor("[feed-weight-shared] registered owner=", 60*time.Second)
		rec := e.record()
		dead := rec.Owners[e.ownerKey(cCfg)]
		if dead == nil || dead.Active || len(dead.Charges) == 0 {
			t.Fatalf("the killed owner is still active or its debt vanished: %+v", dead)
		}
		if rec.usedOnHost(feedHostOf(e.venue.srv.URL)) == 0 {
			t.Fatalf("the killed owner's debt does not count against the shared host capacity")
		}
		a.waitFor("[feed] serving:", 120*time.Second)
		b.waitFor("[feed] serving:", 120*time.Second)
		if got := e.venue.maxWindow(); got > hlRESTWeightCeiling-200 {
			t.Fatalf("the largest rolling 60s venue window is %d, above the shared capacity %d", got, hlRESTWeightCeiling-200)
		}
		t.Logf("venue arrivals %d, largest window %d", len(e.venue.snapshot()), e.venue.maxWindow())
	})

	t.Run("CrashPointsKeepFullLiabilityAndNeverSendUnreserved", func(t *testing.T) {
		for _, tc := range []struct {
			point     string
			wantState string
			sent      bool
		}{
			{"reserve:before", "", false},
			{"reserve:after_commit", feedPermitStatePending, false},
			{"send:after_commit", feedPermitStateSending, false},
			{"settle:before_commit", feedPermitStateSending, true},
			{"rename@2", "", false},
		} {
			t.Run(strings.ReplaceAll(tc.point, ":", "_"), func(t *testing.T) {
				e := newFPEnv(t)
				cfg := e.feedConfig("x", 2, 600, 100, 100)
				recordLate := e.installLeavingALateArrival(t, cfg)
				before := len(e.venue.snapshot())
				recBefore := e.record()
				x := e.start("x", cfg, feedWeightTestKillEnv+"="+tc.point)
				if code := x.waitExit(120 * time.Second); code != 128+int(syscall.SIGKILL) {
					t.Fatalf("exit %d, want SIGKILL:\n%s", code, x.output())
				}
				late := recordLate()
				if late < before {
					t.Fatalf("the held install request was recorded at arrival %d, before the count of %d taken after the install exited", late, before)
				}
				rec := e.record()
				me := rec.Owners[e.ownerKey(cfg)]
				var states []string
				permitWeight := 0
				for _, p := range me.Permits {
					states = append(states, p.State)
					permitWeight += p.Cost
				}
				sent := e.venue.sentBy("x") > 0
				if sent != tc.sent {
					t.Fatalf("venue arrivals from the crashed incarnation: sent=%t, want %t", sent, tc.sent)
				}
				t.Logf("held install arrival recorded at index %d after the post-install count %d; arrivals after that count %d; arrivals from x %d", late, before, len(e.venue.snapshot())-before, e.venue.sentBy("x"))
				if tc.wantState == "" && len(me.Permits) != 0 {
					t.Fatalf("a crash before any commit left permits %v", states)
				}
				if tc.wantState != "" && !containsString(states, tc.wantState) {
					t.Fatalf("permits after a crash at %s = %v, want a %s permit", tc.point, states, tc.wantState)
				}
				for _, st := range states {
					if st != feedPermitStatePending && st != feedPermitStateSending {
						t.Fatalf("unexpected permit state %q", st)
					}
				}
				if tc.point == "rename@2" && rec.Seq != recBefore.Seq+1 {
					t.Fatalf("a crash before rename committed the reservation: seq %d -> %d", recBefore.Seq, rec.Seq)
				}
				charged := func(r *feedAuthRecord) int {
					n := 0
					for _, c := range r.Owners[e.ownerKey(cfg)].Charges {
						n += c.Weight
					}
					return n
				}
				chargedBefore := charged(rec)
				y := e.start("x-restart", cfg)
				y.waitFor("[feed-weight-shared] registered owner=", 60*time.Second)
				y.kill()
				rec = e.record()
				if got := charged(rec) - chargedBefore; got < permitWeight {
					t.Fatalf("restart converted %d of %d unresolved weight", got, permitWeight)
				}
				for _, p := range rec.Owners[e.ownerKey(cfg)].Permits {
					if p.Incarnation == me.Incarnation {
						t.Fatalf("an unresolved permit of the dead incarnation survived the restart: %+v", p)
					}
				}
			})
		}
	})

	t.Run("CrashDuringADelayedResponseKeepsTheSendingPermit", func(t *testing.T) {
		e := newFPEnv(t)
		cfg := e.feedConfig("x", 2, 600, 100, 100)
		e.install(t, cfg)
		e.venue.mu.Lock()
		e.venue.hold, e.venue.entered = make(chan struct{}), make(chan struct{}, 1)
		entered := e.venue.entered
		e.venue.mu.Unlock()
		x := e.start("x", cfg)
		select {
		case <-entered:
		case <-time.After(60 * time.Second):
			t.Fatalf("no delayed request:\n%s", x.output())
		}
		x.kill()
		e.venue.mu.Lock()
		close(e.venue.hold)
		e.venue.hold = nil
		e.venue.mu.Unlock()
		me := e.record().Owners[e.ownerKey(cfg)]
		sending := 0
		for _, p := range me.Permits {
			if p.State == feedPermitStateSending {
				sending += p.Cost
			}
		}
		if sending == 0 {
			t.Fatalf("a crash during a delayed response left no sending liability: %+v", me.Permits)
		}
		y := e.start("x-restart", cfg)
		y.waitFor("[feed-weight-shared] registered owner=", 60*time.Second)
		y.kill()
		y2 := e.start("x-restart-2", cfg)
		y2.waitFor("[feed-weight-shared] registered owner=", 60*time.Second)
		y2.kill()
		total := 0
		for _, c := range e.record().Owners[e.ownerKey(cfg)].Charges {
			total += c.Weight
		}
		if total < sending {
			t.Fatalf("restarts kept %d of the %d unresolved weight", total, sending)
		}
	})

	t.Run("RestartKeepsAPauseAndTheProbeIsReadOnly", func(t *testing.T) {
		e := newFPEnv(t)
		aCfg := e.feedConfig("a", 2, 600, 100, 100)
		bCfg := e.feedConfig("b", 2, 300, 100, 100)
		e.install(t, aCfg)
		e.venue.mu.Lock()
		e.venue.status["candleSnapshot"] = 429
		e.venue.mu.Unlock()
		a := e.start("a", aCfg)
		a.waitFor("[feed-ratelimit] host=", 60*time.Second)
		pausedAt := time.Now()
		e.venue.mu.Lock()
		e.venue.status["candleSnapshot"] = 0
		e.venue.mu.Unlock()
		a.kill()
		rec := e.record()
		if len(rec.Hosts) != 1 {
			t.Fatalf("the committed pause is missing: %+v", rec.Hosts)
		}
		b := e.start("b", bCfg)
		b.waitFor("[feed-weight-shared] registered owner=", 60*time.Second)
		time.Sleep(3 * time.Second)
		if n := e.venue.sentBy("b"); time.Since(pausedAt) < feedRateLimitPause && n != 0 {
			t.Fatalf("a peer started after the pause sent %d request(s) inside the pause", n)
		}
		if !strings.Contains(b.output(), "paused") {
			t.Fatalf("the peer shows no paused host:\n%s", b.output())
		}

		staged := filepath.Join(e.dir, "staged.json")
		raw, _ := os.ReadFile(aCfg)
		os.WriteFile(staged, raw, 0o600)
		before := faTree(t, e.auth)
		cmd := exec.Command(e.bin, "probe", "--config", staged, "--deployment-config", aCfg)
		cmd.Dir, cmd.Env = e.root, e.env()
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "owner="+e.ownerKey(aCfg)) || !strings.Contains(string(out), "combined_allocation=1100") {
			t.Fatalf("staged probe: %v\n%s", err, out)
		}
		if after := faTree(t, e.auth); after != before {
			t.Fatalf("the probe changed the state directory")
		}
		big := e.feedConfig("big", 2, 900, 100, 100)
		cmd = exec.Command(e.bin, "probe", "--config", big)
		cmd.Dir, cmd.Env = e.root, e.env()
		out, err = cmd.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != ExitProbeFailure {
			t.Fatalf("an unsafe candidate probe: %v\n%s", err, out)
		}
		dup := e.start("b-dup", bCfg)
		if code := dup.waitExit(60 * time.Second); code != ExitSingletonLock {
			t.Fatalf("a duplicate owner exited %d, want %d:\n%s", code, ExitSingletonLock, dup.output())
		}
	})

	t.Run("AHeldAuthorityLockNeverBlocksStatus", func(t *testing.T) {
		e := newFPEnv(t)
		cfg := e.feedConfig("a", 2, 600, 100, 100)
		e.install(t, cfg)
		a := e.start("a", cfg)
		a.waitFor("[feed] serving:", 120*time.Second)
		var port int
		raw, _ := os.ReadFile(cfg)
		var doc struct {
			StatusPort int `json:"status_port"`
		}
		json.Unmarshal(raw, &doc)
		port = doc.StatusPort
		f, err := os.OpenFile(filepath.Join(e.auth, feedAuthorityLockName), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		release := time.AfterFunc(1500*time.Millisecond, func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN) })
		defer release.Stop()
		for _, path := range []string{"/health", "/status", "/health"} {
			started := time.Now()
			resp, err := http.Get("http://localhost:" + strconv.Itoa(port) + path)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if d := time.Since(started); d > 200*time.Millisecond {
				t.Fatalf("%s took %s while the authority lock was held", path, d)
			}
		}
	})
}
