package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type faClock struct {
	mu   sync.Mutex
	boot string
	ns   int64
	wall time.Time
	fail bool
}

func (c *faClock) BootID() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return "", errors.New("fixture boot clock unreadable")
	}
	return c.boot, nil
}

func (c *faClock) Elapsed() (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return 0, errors.New("fixture boot clock unreadable")
	}
	return c.ns, nil
}

func (c *faClock) Wall() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}

func (c *faClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ns += int64(d)
	c.wall = c.wall.Add(d)
}

func (c *faClock) stepWall(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(d)
}

func (c *faClock) set(fn func(c *faClock)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c)
}

type faEnv struct {
	t      *testing.T
	root   string
	dir    string
	clock  *faClock
	logMu  sync.Mutex
	logs   []string
	owners []*faOwner
}

func newFAEnv(t *testing.T) *faEnv {
	t.Helper()
	root := t.TempDir()
	e := &faEnv{t: t, root: root, dir: filepath.Join(root, "feed-weight"),
		clock: &faClock{boot: "boot-a", ns: int64(1000 * time.Hour), wall: time.Unix(1890000000, 0).UTC()}}
	prevDir, prevClock, prevHook := feedAuthorityDirOverride, newFeedBootClock, feedAuthorityHook
	feedAuthorityDirOverride = e.dir
	newFeedBootClock = func() feedBootClock { return e.clock }
	t.Cleanup(func() {
		feedAuthorityDirOverride, newFeedBootClock, feedAuthorityHook = prevDir, prevClock, prevHook
		e.owners = nil
	})
	return e
}

func (e *faEnv) logf(format string, a ...any) {
	e.logMu.Lock()
	defer e.logMu.Unlock()
	e.logs = append(e.logs, fmt.Sprintf(format, a...))
}

func (e *faEnv) logged(sub string) int {
	e.logMu.Lock()
	defer e.logMu.Unlock()
	n := 0
	for _, l := range e.logs {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

type faOwner struct {
	name   string
	a      *feedWeightAuthority
	l      *feedRequestLedger
	policy *feedBudgetConfig
}

func (e *faEnv) newOwner(name string, policy *feedBudgetConfig) *faOwner {
	l := newFeedRequestLedger(policy, true, nil)
	a := newFeedWeightAuthority(feedAuthorityRoleFeed, filepath.Join(e.root, "cfg-"+name+".json"), e.logf)
	a.attach(l)
	o := &faOwner{name: name, a: a, l: l, policy: policy}
	e.owners = append(e.owners, o)
	return o
}

func (e *faEnv) owner(name string, policy *feedBudgetConfig) *faOwner {
	e.t.Helper()
	o := e.newOwner(name, policy)
	if err := o.a.register(context.Background(), policy.weightPolicy()); err != nil {
		e.t.Fatalf("register %s: %v", name, err)
	}
	o.l.openStartup()
	return o
}

func (e *faEnv) settle(owners ...*faOwner) {
	e.t.Helper()
	e.clock.advance(feedBudgetWindow + time.Second)
	for _, o := range owners {
		o.a.heartbeatOnce(context.Background())
	}
	for _, o := range owners {
		o.a.heartbeatOnce(context.Background())
	}
	if rec := e.record(); rec.Recovery != nil {
		e.t.Fatalf("recovery still holds after the window: %+v", rec.Recovery)
	}
}

func (e *faEnv) record() *feedAuthRecord {
	e.t.Helper()
	rec, state, detail, r := loadFeedAuthRecord(filepath.Join(e.dir, feedAuthorityRecordName), true)
	if r != nil || state != feedAuthLoadOK {
		e.t.Fatalf("record state %d %s %v", state, detail, r)
	}
	return rec
}

func (e *faEnv) recordBytes() []byte {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, feedAuthorityRecordName))
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func faCharged(rec *feedAuthRecord, key string) (charged, debt, permits int) {
	o := rec.Owners[key]
	if o == nil {
		return 0, 0, 0
	}
	for _, c := range o.Charges {
		charged += c.Weight
		if c.Debt {
			debt += c.Weight
		}
	}
	for _, p := range o.Permits {
		permits += p.Cost
	}
	return
}

func faCandleCtx(o *faOwner, reason, input string) context.Context {
	return wtCtx(o.l, reason, input)
}

func faFetch(e *faEnv, o *faOwner, reason string) error {
	end := e.clock.Wall()
	ctx, cancel := context.WithTimeout(faCandleCtx(o, reason, "BTC|1m"), 3*time.Second)
	defer cancel()
	_, err := fetchHyperliquidCandleSnapshot(ctx, "BTC", "1m", end.Add(-59*time.Minute).UnixMilli(), end.UnixMilli())
	return err
}

func faRows(r wtRequest) int {
	const step = int64(60_000)
	first := (r.StartTime + step - 1) / step * step
	if r.EndTime < first {
		return 0
	}
	return int((r.EndTime-first)/step) + 1
}

func faVenueWeight(t *testing.T, r wtRequest) int {
	t.Helper()
	switch r.Type {
	case "candleSnapshot":
		w, err := hlInfoWeight("candleSnapshot", faRows(r))
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	w, err := hlInfoWeight(r.Type, 0)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func faMaxWindow(t *testing.T, reqs []wtRequest) int {
	t.Helper()
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].At.Before(reqs[j].At) })
	best := 0
	for i := range reqs {
		sum := 0
		for j := i; j < len(reqs) && reqs[j].At.Sub(reqs[i].At) < feedBudgetWindow; j++ {
			sum += faVenueWeight(t, reqs[j])
		}
		if sum > best {
			best = sum
		}
	}
	return best
}

func faAllRequests(v *wtVenue) []wtRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]wtRequest(nil), v.reqs...)
}

func faTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			fmt.Fprintf(&b, "%s err=%v\n", path, err)
			return nil
		}
		fi, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		fmt.Fprintf(&b, "%s %o %d %d\n", path, fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
		if !d.IsDir() {
			if raw, rerr := os.ReadFile(path); rerr == nil {
				fmt.Fprintf(&b, "  %x\n", raw)
			}
		}
		return nil
	})
	return b.String()
}

func TestFeedWeightSharedAuthorityStorage(t *testing.T) {
	for _, point := range []string{"dir", "lock", "read", "write", "sync", "rename"} {
		t.Run("Fault_"+point, func(t *testing.T) {
			e := newFAEnv(t)
			newWTVenue(t, e.clock.Wall)
			o := e.owner("a", wtPolicy(600, 100, 100))
			e.settle(o)
			before := e.recordBytes()
			feedAuthorityHook = func(p string) error {
				if p == point {
					return fmt.Errorf("injected %s failure", point)
				}
				return nil
			}
			err := faFetch(e, o, string(feedRestRefresh))
			if cause := feedWeightRefusalCause(err); cause != feedWeightCauseSharedStorage {
				t.Fatalf("a %s failure returned %v (cause %q), want %s", point, err, cause, feedWeightCauseSharedStorage)
			}
			if !errors.Is(err, errFeedWeightRefused) || !errors.Is(err, errFeedBudgetExhausted) {
				t.Fatalf("a shared refusal must classify as a weight hold: %v", err)
			}
			if after := e.recordBytes(); !bytes.Equal(before, after) {
				t.Fatalf("a %s failure changed authority.json", point)
			}
			if st := o.l.weightStatus(); st.Pending != 0 || st.InFlight != 0 {
				t.Fatalf("a refused reservation left local weight open: %+v", st)
			}
			if o.l.status().Totals.Total != 0 {
				t.Fatalf("a refused reservation was counted as a request: %+v", o.l.status().Totals)
			}
			feedAuthorityHook = nil
			time.Sleep(feedAuthorityFaultRetry + 50*time.Millisecond)
			if err := faFetch(e, o, string(feedRestRefresh)); err != nil {
				t.Fatalf("a read after the fault cleared: %v", err)
			}
		})
	}

	t.Run("DirectorySyncFailureKeepsTheOrphanAsAFullCharge", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		o := e.owner("a", wtPolicy(600, 100, 100))
		e.settle(o)
		feedAuthorityHook = func(p string) error {
			if p == "dirsync" {
				return errors.New("injected dirsync failure")
			}
			return nil
		}
		if err := faFetch(e, o, string(feedRestRefresh)); feedWeightRefusalCause(err) != feedWeightCauseSharedStorage {
			t.Fatalf("dirsync failure: %v", err)
		}
		feedAuthorityHook = nil
		time.Sleep(feedAuthorityFaultRetry + 50*time.Millisecond)
		o.a.heartbeatOnce(context.Background())
		rec := e.record()
		charged, _, permits := faCharged(rec, o.a.key)
		cost, _ := hlInfoWeight("candleSnapshot", 61)
		if permits != 0 || charged != cost {
			t.Fatalf("after an ambiguous commit the record holds permits %d charges %d, want the full %d as a charge", permits, charged, cost)
		}
	})

	t.Run("UnknownVersionRefusesAndIsNeverOverwritten", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		o := e.owner("a", wtPolicy(600, 100, 100))
		e.settle(o)
		future := []byte(`{"version":2,"anything":"else"}`)
		if err := os.WriteFile(filepath.Join(e.dir, feedAuthorityRecordName), future, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := faFetch(e, o, string(feedRestRefresh)); feedWeightRefusalCause(err) != feedWeightCauseSharedVersion {
			t.Fatalf("an unknown version returned %v", err)
		}
		o.a.heartbeatOnce(context.Background())
		if got := e.recordBytes(); !bytes.Equal(got, future) {
			t.Fatalf("an unknown version was overwritten: %s", got)
		}
		if st := o.l.weightStatus().Shared; st.Registry != "unsupported_version" {
			t.Fatalf("registry = %q, want unsupported_version", st.Registry)
		}
	})

	t.Run("PermissionsAndReplacedLockRefuse", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		o := e.owner("a", wtPolicy(600, 100, 100))
		e.settle(o)
		if err := os.Chmod(e.dir, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := faFetch(e, o, string(feedRestRefresh)); feedWeightRefusalCause(err) != feedWeightCauseSharedStorage || !strings.Contains(err.Error(), "must not write") {
			t.Fatalf("a group-writable directory returned %v", err)
		}
		if err := os.Chmod(e.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		time.Sleep(feedAuthorityFaultRetry + 50*time.Millisecond)
		feedAuthorityHook = func(p string) error {
			if p != "lock:held" {
				return nil
			}
			tmp := filepath.Join(e.dir, "replacement")
			os.WriteFile(tmp, nil, 0o600)
			return os.Rename(tmp, filepath.Join(e.dir, feedAuthorityLockName))
		}
		if err := faFetch(e, o, string(feedRestRefresh)); feedWeightRefusalCause(err) != feedWeightCauseSharedStorage || !strings.Contains(err.Error(), "replaced") {
			t.Fatalf("a replaced lock returned %v", err)
		}
	})

	t.Run("LockTimeoutRefusesWithoutAHeapFallback", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		o := e.owner("a", wtPolicy(600, 100, 100))
		e.settle(o)
		f, err := os.OpenFile(filepath.Join(e.dir, feedAuthorityLockName), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		err = faFetch(e, o, string(feedRestRefresh))
		if feedWeightRefusalCause(err) != feedWeightCauseSharedStorage || time.Since(started) > feedAuthorityLockWait+time.Second {
			t.Fatalf("a held lock returned %v after %s", err, time.Since(started))
		}
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	})
}

func TestFeedWeightSharedOwners(t *testing.T) {
	t.Run("CombinedAllocationIncludesStartupAndUsesTheLargestReserve", func(t *testing.T) {
		e := newFAEnv(t)
		e.owner("a", wtPolicy(500, 100, 200))
		e.owner("b", wtPolicy(300, 100, 50))
		c := e.newOwner("c", wtPolicy(1, 0, 0))
		err := c.a.register(context.Background(), c.policy.weightPolicy())
		var unsafe *feedAuthorityUnsafeError
		if !errors.As(err, &unsafe) || !unsafe.AllLive || unsafe.Combined != 1001 || unsafe.Capacity != 1000 || unsafe.Reserve != 200 {
			t.Fatalf("an unsafe third owner returned %v", err)
		}
		d := e.newOwner("d", wtPolicy(0, 0, 300))
		if err := d.a.register(context.Background(), d.policy.weightPolicy()); !errors.As(err, &unsafe) || unsafe.Capacity != 900 {
			t.Fatalf("a larger reserve must shrink the capacity to 900: %v", err)
		}
		rec := e.record()
		if rec.combinedAllocation() != 1000 || rec.commonReserve() != 200 || rec.Owners[c.a.key] != nil || rec.Owners[d.a.key] != nil {
			t.Fatalf("refused owners changed the record: combined %d reserve %d", rec.combinedAllocation(), rec.commonReserve())
		}
	})

	t.Run("BoundsDoNotOverflow", func(t *testing.T) {
		e := newFAEnv(t)
		e.owner("a", wtPolicy(1200, 0, 0))
		huge := e.newOwner("b", wtPolicy(1200, 1200, 1200))
		var unsafe *feedAuthorityUnsafeError
		if err := huge.a.register(context.Background(), huge.policy.weightPolicy()); !errors.As(err, &unsafe) || unsafe.Combined != 3600 || unsafe.Capacity != 0 {
			t.Fatalf("bound values: %v", err)
		}
	})

	t.Run("DuplicateOwnerIsRefusedAndAStoppedOwnersDebtStays", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		a := e.owner("a", wtPolicy(600, 100, 100))
		e.settle(a)
		dup := e.newOwner("a", wtPolicy(600, 100, 100))
		var de *feedAuthorityDuplicateError
		if err := dup.a.register(context.Background(), dup.policy.weightPolicy()); !errors.As(err, &de) {
			t.Fatalf("a second incarnation of the same owner returned %v", err)
		}
		for i := 0; i < 5; i++ {
			if err := faFetch(e, a, string(feedRestRefresh)); err != nil {
				t.Fatal(err)
			}
		}
		spent, _, _ := faCharged(e.record(), a.a.key)
		a.a.lifetime.Close()
		a.a.lifetime = nil
		b := e.owner("b", wtPolicy(900, 0, 100))
		rec := e.record()
		if rec.Owners[a.a.key].Active || rec.combinedAllocation() != 900 {
			t.Fatalf("a dead owner's allocation is still active: %+v", rec.Owners[a.a.key])
		}
		if got := rec.usedOnHost(feedHostOf(hlMainnetURL)); got != int64(spent) {
			t.Fatalf("shared use after the owner died = %d, want its retained %d", got, spent)
		}
		if st := b.l.weightStatus().Shared; st.StoppedOwnerDebt != int64(spent) || st.Capacity != 1100 {
			t.Fatalf("status: %+v", st)
		}
		e.clock.advance(feedBudgetWindow)
		b.a.heartbeatOnce(context.Background())
		if rec := e.record(); rec.Owners[a.a.key] != nil {
			t.Fatalf("an expired stopped owner stays in the record: %+v", rec.Owners[a.a.key])
		}
	})

	t.Run("MissingLifetimeLockKeepsTheAllocationAndLatchesTheOwner", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		a := e.owner("a", wtPolicy(600, 100, 100))
		b := e.owner("b", wtPolicy(300, 0, 100))
		e.settle(a, b)
		if err := os.Remove(feedAuthorityOwnerLockPath(e.dir, a.a.key)); err != nil {
			t.Fatal(err)
		}
		b.a.heartbeatOnce(context.Background())
		if rec := e.record(); !rec.Owners[a.a.key].Active || rec.combinedAllocation() != 1000 {
			t.Fatalf("an owner with unknown liveness lost its allocation: %+v", rec.Owners[a.a.key])
		}
		a.a.heartbeatOnce(context.Background())
		if err := faFetch(e, a, string(feedRestRefresh)); feedWeightRefusalCause(err) != feedWeightCauseSharedOwner {
			t.Fatalf("an owner whose lifetime lock vanished still sends: %v", err)
		}
		if e.logged("no longer names the held lock file") != 1 {
			t.Fatalf("missing CRITICAL line: %v", e.logs)
		}
	})
}

func TestFeedWeightSharedPermitLifecycle(t *testing.T) {
	t.Run("TwoOwnersAndStoppedOwnerDebtStayWithinTheSharedCapacity", func(t *testing.T) {
		e := newFAEnv(t)
		venue := newWTVenue(t, e.clock.Wall)
		c := e.owner("c", wtPolicy(1000, 0, 200))
		e.settle(c)
		for i := 0; i < 20; i++ {
			if err := faFetch(e, c, string(feedRestRefresh)); err != nil {
				t.Fatal(err)
			}
		}
		c.a.stop()
		a := e.owner("a", wtPolicy(500, 100, 200))
		b := e.owner("b", wtPolicy(300, 100, 100))
		var shared int
		var mu sync.Mutex
		for round := 0; round < 18; round++ {
			var wg sync.WaitGroup
			for _, o := range []*faOwner{a, b} {
				for i := 0; i < 5; i++ {
					wg.Add(1)
					go func(o *faOwner, reason string) {
						defer wg.Done()
						err := faFetch(e, o, reason)
						if feedSharedHeldErr(err) {
							mu.Lock()
							shared++
							mu.Unlock()
						} else if err != nil && !errors.Is(err, errFeedWeightRefused) {
							t.Errorf("fetch: %v", err)
						}
					}(o, []string{string(feedRestRefresh), string(feedRestBootstrap)}[i%2])
				}
			}
			wg.Wait()
			e.clock.advance(5 * time.Second)
			a.a.heartbeatOnce(context.Background())
		}
		reqs := faAllRequests(venue)
		if got := faMaxWindow(t, reqs); got > 1000 {
			t.Fatalf("the largest rolling 60s arrival window carries %d weight, above the shared capacity 1000", got)
		}
		if shared == 0 {
			t.Fatalf("no read was refused by the shared record; the fixture does not exercise shared admission")
		}
		t.Logf("arrivals %d, largest window %d, shared refusals %d", len(reqs), faMaxWindow(t, reqs), shared)
	})

	t.Run("HeldSendAndSlowResponseStayCountedPastOneWindow", func(t *testing.T) {
		e := newFAEnv(t)
		venue := newWTVenue(t, e.clock.Wall)
		a := e.owner("a", wtPolicy(600, 100, 100))
		b := e.owner("b", wtPolicy(400, 0, 100))
		e.settle(a, b)
		spec := feedAcquireSpec{host: feedHostOf(venue.srv.URL), reason: string(feedRestRefresh), reqType: "allMids", input: "mids", rows: -1}
		held, err := a.l.acquireShared(context.Background(), spec, false)
		if err != nil {
			t.Fatal(err)
		}
		entered, release := venue.hold("candleSnapshot")
		done := make(chan error, 1)
		go func() { done <- faFetch(e, a, string(feedRestRefresh)) }()
		<-entered
		e.clock.advance(2 * feedBudgetWindow)
		b.a.heartbeatOnce(context.Background())
		rec := e.record()
		var pending, sending int
		for _, p := range rec.Owners[a.a.key].Permits {
			if p.State == feedPermitStatePending {
				pending += p.Cost
			} else {
				sending += p.Cost
			}
		}
		mids, _ := hlInfoWeight("allMids", 0)
		candle, _ := hlInfoWeight("candleSnapshot", 61)
		if pending != mids || sending != candle {
			t.Fatalf("after two windows pending %d sending %d, want %d and %d still counted", pending, sending, mids, candle)
		}
		release()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if err := held.beginSend(); err != nil {
			t.Fatalf("a pending permit past one window: %v", err)
		}
		held.finish(200, "")
		held.settle(0)
		before, _, _ := faCharged(e.record(), a.a.key)
		held.settle(0)
		held.closeOpen()
		held.retain()
		if after, _, _ := faCharged(e.record(), a.a.key); after != before {
			t.Fatalf("a repeated settle, closeOpen or retain changed the record: %d -> %d", before, after)
		}
	})

	t.Run("ExcessRowsLeaveDurableDebtAndAFailedSettleKeepsTheFullReservation", func(t *testing.T) {
		e := newFAEnv(t)
		venue := newWTVenue(t, e.clock.Wall)
		a := e.owner("a", wtPolicy(1000, 0, 100))
		e.settle(a)
		venue.set(func(v *wtVenue) { v.candleExtra = 200 })
		if err := faFetch(e, a, string(feedRestRefresh)); err == nil {
			t.Fatal("an over-bound response was accepted")
		}
		venue.set(func(v *wtVenue) { v.candleExtra = 0 })
		_, debt, _ := faCharged(e.record(), a.a.key)
		if debt <= 0 {
			t.Fatalf("no durable debt after a bound violation")
		}
		if b := e.record().Owners[a.a.key].Bounds["candleSnapshot"]; b == nil || !b.AtCap {
			t.Fatalf("the bound state is not durable: %+v", b)
		}
		before, _, _ := faCharged(e.record(), a.a.key)
		armed := false
		feedAuthorityHook = func(p string) error {
			if p == "settle:before_commit" {
				armed = true
			}
			if armed && p == "write" {
				return errors.New("injected write failure during settle")
			}
			return nil
		}
		if err := faFetch(e, a, string(feedRestRefresh)); err != nil {
			t.Fatal(err)
		}
		feedAuthorityHook = nil
		time.Sleep(feedAuthorityFaultRetry + 50*time.Millisecond)
		a.a.heartbeatOnce(context.Background())
		after, _, permits := faCharged(e.record(), a.a.key)
		atCap, _ := hlInfoWeightAtCap("candleSnapshot")
		if permits != 0 || after-before != atCap {
			t.Fatalf("a failed settle commit charged %d (permits %d), want the full reservation %d with no refund", after-before, permits, atCap)
		}
	})
}

func TestFeedWeightSharedRecovery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(e *faEnv)
		reason string
	}{
		{"Absent", func(e *faEnv) { os.Remove(filepath.Join(e.dir, feedAuthorityRecordName)) }, feedAuthorityRecoveryMissing},
		{"Corrupt", func(e *faEnv) {
			os.WriteFile(filepath.Join(e.dir, feedAuthorityRecordName), []byte(`{"version":1,"epoch_id":`), 0o600)
		}, feedAuthorityRecoveryCorrupt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFAEnv(t)
			newWTVenue(t, e.clock.Wall)
			a := e.owner("a", wtPolicy(600, 100, 100))
			b := e.owner("b", wtPolicy(300, 0, 100))
			if rec := e.record(); rec.Recovery == nil || rec.Recovery.Reason != feedAuthorityRecoveryInitial || rec.Generation != 1 {
				t.Fatalf("first installation: %+v", rec.Recovery)
			}
			if err := faFetch(e, a, string(feedRestRefresh)); feedWeightRefusalCause(err) != feedWeightCauseSharedHold {
				t.Fatalf("a read during the initial hold: %v", err)
			}
			e.settle(a, b)
			for i := 0; i < 3; i++ {
				if err := faFetch(e, a, string(feedRestRefresh)); err != nil {
					t.Fatal(err)
				}
			}
			known, _, _ := faCharged(e.record(), a.a.key)
			a.a.sharedReport()
			b.a.sharedReport()
			tc.damage(e)
			a.a.heartbeatOnce(context.Background())
			rec := e.record()
			if rec.Recovery == nil || rec.Recovery.Reason != tc.reason || rec.Generation != 2 {
				t.Fatalf("after damage: generation %d recovery %+v", rec.Generation, rec.Recovery)
			}
			hold := rec.Recovery.HoldUntilNs
			for i := 0; i < 10; i++ {
				b.a.heartbeatOnce(context.Background())
				a.a.heartbeatOnce(context.Background())
				faFetch(e, b, string(feedRestRefresh))
			}
			rec = e.record()
			if rec.Generation != 2 || rec.Recovery == nil || rec.Recovery.HoldUntilNs != hold {
				t.Fatalf("repeated reads moved the hold: generation %d %+v", rec.Generation, rec.Recovery)
			}
			if got, _, _ := faCharged(rec, a.a.key); got != known {
				t.Fatalf("a live owner's known charges were not re-adopted: %d, want %d", got, known)
			}
			evA := a.a.sharedReport()
			if len(evA) != 1 || evA[0].Kind != "recovery_hold" || evA[0].Generation != 2 {
				t.Fatalf("owner summary at hold start: %+v", evA)
			}
			if again := a.a.sharedReport(); len(again) != 0 {
				t.Fatalf("a second report in the same episode: %+v", again)
			}
			e.settle(a, b)
			evA = a.a.sharedReport()
			if len(evA) != 1 || evA[0].Kind != "recovery_ended" {
				t.Fatalf("owner summary at hold end: %+v", evA)
			}
			if err := faFetch(e, a, string(feedRestRefresh)); err != nil {
				t.Fatalf("a read after recovery: %v", err)
			}
		})
	}

	t.Run("DeadPendingOwnerReleasesTheHoldOnlyAfterAFullWindow", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		a := e.owner("a", wtPolicy(600, 100, 100))
		b := e.owner("b", wtPolicy(300, 0, 100))
		e.settle(a, b)
		os.Remove(filepath.Join(e.dir, feedAuthorityRecordName))
		a.a.heartbeatOnce(context.Background())
		e.clock.advance(50 * time.Second)
		b.a.lifetime.Close()
		b.a.lifetime = nil
		b.a.stopped = true
		a.a.heartbeatOnce(context.Background())
		e.clock.advance(15 * time.Second)
		a.a.heartbeatOnce(context.Background())
		if rec := e.record(); rec.Recovery == nil {
			t.Fatalf("the hold ended before the dead owner was dead for a full window")
		}
		e.clock.advance(feedBudgetWindow)
		a.a.heartbeatOnce(context.Background())
		if rec := e.record(); rec.Recovery != nil {
			t.Fatalf("the hold did not end: %+v", rec.Recovery)
		}
	})
}

func TestFeedWeightSharedClock(t *testing.T) {
	t.Run("WallStepsAndIdleTimeStartNoHoldAndReleaseNothingEarly", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		a := e.owner("a", wtPolicy(600, 100, 100))
		e.settle(a)
		gen := e.record().Generation
		if err := faFetch(e, a, string(feedRestRefresh)); err != nil {
			t.Fatal(err)
		}
		charged, _, _ := faCharged(e.record(), a.a.key)
		for _, step := range []time.Duration{2 * time.Hour, -4 * time.Hour} {
			e.clock.stepWall(step)
			a.a.heartbeatOnce(context.Background())
			if got, _, _ := faCharged(e.record(), a.a.key); got != charged {
				t.Fatalf("a wall step of %s changed the retained charge %d -> %d", step, charged, got)
			}
			if st := a.l.weightStatus(); st.Charged != charged {
				t.Fatalf("a wall step of %s changed the local charge to %d", step, st.Charged)
			}
		}
		e.clock.advance(30 * time.Minute)
		a.a.heartbeatOnce(context.Background())
		rec := e.record()
		if rec.Generation != gen || rec.Recovery != nil {
			t.Fatalf("idle time started a recovery: generation %d -> %d", gen, rec.Generation)
		}
		if got, _, _ := faCharged(rec, a.a.key); got != 0 {
			t.Fatalf("an idle window did not expire the charge: %d", got)
		}
		if err := faFetch(e, a, string(feedRestRefresh)); err != nil {
			t.Fatalf("a read after idle time: %v", err)
		}
	})

	for _, tc := range []struct {
		name   string
		change func(c *faClock)
		reason string
	}{
		{"BootChanged", func(c *faClock) { c.boot = "boot-b"; c.ns = int64(time.Minute) }, feedAuthorityRecoveryBoot},
		{"ElapsedRegressed", func(c *faClock) { c.ns -= int64(time.Hour) }, feedAuthorityRecoveryRegressed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFAEnv(t)
			venue := newWTVenue(t, e.clock.Wall)
			a := e.owner("a", wtPolicy(600, 100, 100))
			e.settle(a)
			gen := e.record().Generation
			if err := faFetch(e, a, string(feedRestRefresh)); err != nil {
				t.Fatal(err)
			}
			host := feedHostOf(venue.srv.URL)
			if err := a.a.tx(context.Background(), "test", func(t *feedAuthTx) error {
				t.commitPause(host, t.now.Ns+int64(feedRateLimitPause), t.now.Wall.Add(feedRateLimitPause), "")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			e.clock.set(tc.change)
			a.a.heartbeatOnce(context.Background())
			rec := e.record()
			if rec.Generation != gen+1 || rec.Recovery == nil || rec.Recovery.Reason != tc.reason {
				t.Fatalf("generation %d -> %d recovery %+v", gen, rec.Generation, rec.Recovery)
			}
			if h := rec.Hosts[host]; h == nil || h.PauseUntilNs-e.clock.ns != int64(feedRateLimitPause) {
				t.Fatalf("an unexpired pause was not re-armed for a full pause: %+v", h)
			}
			if got, _, _ := faCharged(rec, a.a.key); got == 0 {
				t.Fatalf("charges were dropped by the clock recovery")
			}
			for i := 0; i < 5; i++ {
				a.a.heartbeatOnce(context.Background())
			}
			if e.record().Generation != gen+1 {
				t.Fatalf("repeated reads started another recovery")
			}
			e.settle(a)
			if e.record().Generation != gen+1 {
				t.Fatalf("the recovery repeated after it ended")
			}
		})
	}

	t.Run("UnreadableClockRefusesThenStartsOneHold", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		a := e.owner("a", wtPolicy(600, 100, 100))
		e.settle(a)
		gen := e.record().Generation
		before := e.recordBytes()
		e.clock.set(func(c *faClock) { c.fail = true })
		if err := faFetch(e, a, string(feedRestRefresh)); feedWeightRefusalCause(err) != feedWeightCauseSharedClock {
			t.Fatalf("an unreadable clock returned %v", err)
		}
		if !bytes.Equal(before, e.recordBytes()) {
			t.Fatalf("an unreadable clock changed the record")
		}
		e.clock.set(func(c *faClock) { c.fail = false })
		a.a.heartbeatOnce(context.Background())
		a.a.heartbeatOnce(context.Background())
		rec := e.record()
		if rec.Generation != gen+1 || rec.Recovery == nil || rec.Recovery.Reason != feedAuthorityRecoveryClockFault {
			t.Fatalf("after the clock returned: generation %d %+v", rec.Generation, rec.Recovery)
		}
	})
}

type faRateVenue struct {
	srv    *httptest.Server
	mu     sync.Mutex
	status int
	count  int
}

func newFARateVenue(t *testing.T) *faRateVenue {
	v := &faRateVenue{status: 200}
	v.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		v.count++
		st := v.status
		v.mu.Unlock()
		if st != 200 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(st)
			return
		}
		w.Write([]byte(`{"BTC":"101.5"}`))
	}))
	t.Cleanup(v.srv.Close)
	return v
}

func faMids(o *faOwner, base string) error {
	return hlInfoPost(wtCtx(o.l, string(feedRestMids), "mids"), hlInfoCall{BaseURL: base, Type: "allMids", Body: []byte(`{"type":"allMids"}`), Timeout: 5 * time.Second,
		Decode: func([]byte) (int, error) { return 0, nil }})
}

func TestFeedWeightSharedPause(t *testing.T) {
	t.Run("ACommittedRateLimitStopsEveryLaterSendForThatHostOnly", func(t *testing.T) {
		e := newFAEnv(t)
		main := newFARateVenue(t)
		test := newFARateVenue(t)
		a := e.owner("a", wtPolicy(600, 100, 100))
		b := e.owner("b", wtPolicy(400, 0, 100))
		e.settle(a, b)
		spec := feedAcquireSpec{host: feedHostOf(main.srv.URL), reason: string(feedRestRefresh), reqType: "allMids", input: "mids", rows: -1}
		admitted, err := b.l.acquireShared(context.Background(), spec, false)
		if err != nil {
			t.Fatal(err)
		}
		early, err := b.l.acquireShared(context.Background(), spec, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := early.beginSend(); err != nil {
			t.Fatal(err)
		}
		test.mu.Lock()
		test.status = 429
		test.mu.Unlock()
		if err := faMids(a, test.srv.URL); !errors.Is(err, errHLInfoRateLimited) {
			t.Fatalf("testnet 429: %v", err)
		}
		if err := faMids(b, main.srv.URL); err != nil {
			t.Fatalf("a testnet pause closed mainnet: %v", err)
		}
		main.mu.Lock()
		main.status = 429
		main.mu.Unlock()
		if err := faMids(a, main.srv.URL); !errors.Is(err, errHLInfoRateLimited) {
			t.Fatalf("mainnet 429: %v", err)
		}
		first := e.record().Hosts[feedHostOf(main.srv.URL)].PauseUntilNs
		if err := admitted.beginSend(); !errors.Is(err, errFeedHostPaused) {
			t.Fatalf("a previously admitted permit of the peer sent after the committed pause: %v", err)
		}
		early.finish(200, "")
		early.settle(0)
		if st := b.l.rateLimitStatus(); len(st) == 0 || st[0].State != feedRateLimitStatePaused {
			t.Fatalf("the peer's local status does not show the shared pause: %+v", st)
		}
		main.mu.Lock()
		count := main.count
		main.mu.Unlock()
		if err := faMids(b, main.srv.URL); !errors.Is(err, errFeedHostPaused) {
			t.Fatalf("a later peer read: %v", err)
		}
		main.mu.Lock()
		if main.count != count {
			t.Fatalf("a paused host received a request")
		}
		main.mu.Unlock()
		e.clock.advance(10 * time.Second)
		a.a.tx(context.Background(), "test", func(t *feedAuthTx) error {
			t.commitPause(feedHostOf(main.srv.URL), first-int64(time.Second), t.now.Wall, "")
			return nil
		})
		if got := e.record().Hosts[feedHostOf(main.srv.URL)].PauseUntilNs; got != first {
			t.Fatalf("a pause was shortened: %d -> %d", first, got)
		}
		restarted := e.newOwner("b2", wtPolicy(0, 0, 100))
		if err := restarted.a.register(context.Background(), restarted.policy.weightPolicy()); err != nil {
			t.Fatal(err)
		}
		if err := faMids(restarted, main.srv.URL); !errors.Is(err, errFeedHostPaused) {
			t.Fatalf("a newly started owner ignored the stored pause: %v", err)
		}
	})

	t.Run("AFailedPauseWriteClosesTheOwnerUntilTheRetryCommits", func(t *testing.T) {
		e := newFAEnv(t)
		v := newFARateVenue(t)
		a := e.owner("a", wtPolicy(600, 100, 100))
		e.settle(a)
		v.mu.Lock()
		v.status = 429
		v.mu.Unlock()
		armed := false
		feedAuthorityHook = func(p string) error {
			if p == "send:after_commit" {
				armed = true
			}
			if armed && p == "write" {
				return errors.New("injected pause write failure")
			}
			return nil
		}
		if err := faMids(a, v.srv.URL); !errors.Is(err, errHLInfoRateLimited) {
			t.Fatalf("429: %v", err)
		}
		feedAuthorityHook = nil
		st := a.l.weightStatus().Shared
		if st.Registry != "pause_unavailable" || len(st.Hosts) == 0 || st.Hosts[0].Pause != "unavailable" {
			t.Fatalf("status after a failed pause write: %+v", st)
		}
		if rec := e.record(); rec.Hosts[feedHostOf(v.srv.URL)] != nil {
			t.Fatalf("a pause was reported in the record before its write succeeded")
		}
		e.clock.advance(feedRateLimitPause + time.Second)
		v.mu.Lock()
		v.status = 200
		v.mu.Unlock()
		if err := faMids(a, v.srv.URL); feedWeightRefusalCause(err) != feedWeightCauseSharedStorage {
			t.Fatalf("a send while the pause is uncommitted: %v", err)
		}
		time.Sleep(feedAuthorityFaultRetry + 50*time.Millisecond)
		a.a.heartbeatOnce(context.Background())
		if st := a.l.weightStatus().Shared; st.Registry == "pause_unavailable" {
			t.Fatalf("the heartbeat did not commit the pause: %+v", st)
		}
		if rec := e.record(); rec.Hosts[feedHostOf(v.srv.URL)] == nil {
			t.Fatalf("the retried pause is missing from the record")
		}
	})
}

func TestFeedWeightSharedReloadAndProbe(t *testing.T) {
	t.Run("ProbeForecastIsReadOnlyInEveryState", func(t *testing.T) {
		e := newFAEnv(t)
		identity := filepath.Join(e.root, "cfg-a.json")
		pol := &feedWeightPolicy{PerMinute: 600, Startup: 100, Reserve: 100}
		check := func(name string, wantRefuse bool, wantState string) {
			t.Helper()
			before := faTree(t, e.root)
			fc := forecastFeedAuthority(feedAuthorityRoleFeed, identity, pol)
			if after := faTree(t, e.root); after != before {
				t.Fatalf("%s: the probe changed the state directory:\nbefore\n%s\nafter\n%s", name, before, after)
			}
			if (fc.Refuse != "") != wantRefuse || !strings.Contains(strings.Join(fc.Lines, "\n"), wantState) {
				t.Fatalf("%s: forecast %+v", name, fc)
			}
		}
		check("absent", false, "state=initialization required")
		a := e.owner("a", wtPolicy(600, 100, 100))
		e.settle(a)
		check("healthy, same owner replaced", false, "combined_allocation=700")
		peer := e.owner("peer", wtPolicy(300, 0, 200))
		check("live peer", false, "combined_allocation=1000")
		big := &feedWeightPolicy{PerMinute: 800, Startup: 100, Reserve: 100}
		if fc := forecastFeedAuthority(feedAuthorityRoleFeed, identity, big); fc.Refuse == "" {
			t.Fatalf("an unsafe candidate passed: %+v", fc)
		}
		os.WriteFile(filepath.Join(e.dir, feedAuthorityRecordName), []byte("{"), 0o600)
		check("corrupt", false, "recovery required (record corrupt")
		os.Remove(feedAuthorityOwnerLockPath(e.dir, peer.a.key))
		before := faTree(t, e.root)
		if fc := forecastFeedAuthority(feedAuthorityRoleFeed, identity, pol); fc.Refuse == "" {
			t.Fatalf("unreadable liveness passed: %+v", fc)
		}
		if faTree(t, e.root) != before {
			t.Fatalf("a refused forecast changed the directory")
		}
	})

	t.Run("AnUnsafeFeedReloadKeepsThePreviousRegistrationAndLimits", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		dir := t.TempDir()
		consumer := filepath.Join(dir, "consumer.json")
		os.WriteFile(consumer, []byte(`{"market_feed":"shared","shared_market_feed":{"primary_socket":"/run/a/feed.sock","backup_socket":"/run/b/feed.sock"},
"strategies":[{"id":"hl-btc","type":"perps","platform":"hyperliquid","script":"shared_scripts/check_hyperliquid.py","args":["sma_crossover","BTC","1h","--mode=paper"],"capital":1000,"max_drawdown_pct":50}]}`), 0o600)
		cfgPath := filepath.Join(dir, "feed.json")
		write := func(budget string) {
			body := fmt.Sprintf(`{"role":"feed","status_port":8191,"feed":{"source":"rest","socket_path":"/run/b/feed.sock","consumer_configs":[%q],"request_budget":%s},"strategies":[]}`, consumer, budget)
			os.WriteFile(cfgPath, []byte(body), 0o600)
		}
		write(`{"per_minute":120,"startup":60,"weight_per_minute":500,"weight_startup":100,"weight_reserve":200}`)
		cfg, err := loadFeedRoleConfig(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		peer := e.owner("peer", wtPolicy(300, 0, 100))
		ledger := newFeedRequestLedger(cfg.Feed.RequestBudget, true, nil)
		a := newFeedWeightAuthority(feedAuthorityRoleFeed, cfgPath, e.logf)
		a.attach(ledger)
		if err := a.register(context.Background(), cfg.Feed.RequestBudget.weightPolicy()); err != nil {
			t.Fatal(err)
		}
		ledger.openStartup()
		e.settle(peer)
		a.heartbeatOnce(context.Background())
		owner := newMarketFeedOwner(nil, nil)
		sealer := newFeedSealer(owner, feedSourceREST, "fixture", nil, nil)
		sealer.ledger = ledger
		ctx := withFeedLedger(context.Background(), ledger)
		rt := &feedRuntime{configPath: cfgPath, cfg: cfg, owner: owner, sealer: sealer, ledger: ledger, authority: a, ctx: ctx}
		before := ledger.status()
		recBefore := e.record().Owners[a.key].Policy
		write(`{"per_minute":120,"startup":60,"weight_per_minute":700,"weight_startup":100,"weight_reserve":200}`)
		rt.reload()
		after := ledger.status()
		if after.Weight.PerMinute != before.Weight.PerMinute || after.Weight.StartupLeft != before.Weight.StartupLeft || e.record().Owners[a.key].Policy != recBefore {
			t.Fatalf("an unsafe reload changed state: before %+v after %+v record %+v", before.Weight, after.Weight, e.record().Owners[a.key].Policy)
		}
		write(`{"per_minute":120,"startup":60,"weight_per_minute":550,"weight_startup":100,"weight_reserve":200}`)
		rt.reload()
		if got := ledger.status().Weight.PerMinute; got != 550 || e.record().Owners[a.key].Policy.PerMinute != 550 {
			t.Fatalf("a safe reload was not applied: ledger %d record %+v", got, e.record().Owners[a.key].Policy)
		}
	})
}

func TestFeedWeightSharedStatus(t *testing.T) {
	e := newFAEnv(t)
	newWTVenue(t, e.clock.Wall)
	a := e.owner("a", wtPolicy(600, 100, 100))
	b := e.owner("b", wtPolicy(300, 0, 200))
	e.settle(a, b)
	if err := faFetch(e, a, string(feedRestRefresh)); err != nil {
		t.Fatal(err)
	}
	owner := newMarketFeedOwner(nil, nil)
	owner.SetRequestLedger(a.l)
	h := owner.Health("")
	st := a.l.status().Weight.Shared
	if h.Weight == nil || fmt.Sprintf("%+v", h.Weight.Shared) != fmt.Sprintf("%+v", st) {
		t.Fatalf("health and ledger status disagree:\n%+v\n%+v", h.Weight, st)
	}
	if st.Coverage != feedSharedCoverageShared || st.Registry != "ok" || st.CombinedAllocation != 1000 || st.CommonReserve != 200 || st.Capacity != 1000 || len(st.Owners) != 2 || st.Charged <= 0 {
		t.Fatalf("shared status: %+v", st)
	}
	if !sort.SliceIsSorted(st.Owners, func(i, j int) bool { return st.Owners[i].Key < st.Owners[j].Key }) {
		t.Fatalf("owners are not sorted: %+v", st.Owners)
	}
	if line := formatFeedSharedFields(st); !strings.Contains(line, "shared_registry=ok") || !strings.Contains(line, "shared_capacity=1000") {
		t.Fatalf("budget line fields: %s", line)
	}
	local := hlLotMetadataLedgerContext(context.Background())
	if cov := feedLedgerFrom(local).weightStatus().Shared.Coverage; cov != feedSharedCoverageLotLocal {
		t.Fatalf("lot metadata coverage = %q", cov)
	}
	if cov := newFeedRequestLedger(nil, false, nil).weightStatus().Shared.Coverage; cov != feedSharedCoverageNoPolicy {
		t.Fatalf("no-policy coverage = %q", cov)
	}
	lot := withFeedLedger(context.Background(), a.l)
	if feedLedgerFrom(hlLotMetadataLedgerContext(lot)) != a.l {
		t.Fatalf("embedded lot metadata must use the participating ledger")
	}
}

func TestFeedWeightSharedBootstrapWait(t *testing.T) {
	t.Run("ASharedRefusalWaitsForTheHintWhilePeersKeepCommitting", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		a := e.owner("a", wtPolicy(300, 100, 100))
		b := e.owner("b", wtPolicy(300, 100, 100))
		e.settle(a, b)
		e.clock.set(func(c *faClock) { c.boot = "boot-b"; c.ns = int64(time.Minute) })
		a.a.heartbeatOnce(context.Background())
		rec := e.record()
		if rec.Recovery == nil || rec.Recovery.Reason != feedAuthorityRecoveryBoot {
			t.Fatalf("boot change did not start a hold: %+v", rec.Recovery)
		}
		c := e.owner("c", wtPolicy(200, 100, 100))
		const keys = 10
		const deadline = 2 * time.Second
		hint := c.a.waitHint(c.l.clock().UTC())
		end := e.clock.Wall()
		var wg sync.WaitGroup
		errs := make([]error, keys)
		for i := 0; i < keys; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(faCandleCtx(c, string(feedRestBootstrap), fmt.Sprintf("K%02d|1m", i)), deadline)
				defer cancel()
				_, errs[i] = fetchHyperliquidCandleSnapshot(ctx, fmt.Sprintf("K%02d", i), "1m", end.Add(-59*time.Minute).UnixMilli(), end.UnixMilli())
			}(i)
		}
		stop := make(chan struct{})
		beats := 0
		var hb sync.WaitGroup
		hb.Add(1)
		go func() {
			defer hb.Done()
			for {
				select {
				case <-stop:
					return
				case <-time.After(100 * time.Millisecond):
				}
				b.a.heartbeatOnce(context.Background())
				beats++
			}
		}()
		wg.Wait()
		close(stop)
		hb.Wait()
		for i, err := range errs {
			if feedWeightRefusalCause(err) != feedWeightCauseSharedHold {
				t.Fatalf("key %d during the hold: %v", i, err)
			}
		}
		passes := c.l.weightSnapshot().RefusedByCause[feedWeightCauseSharedHold]
		perKey := int(deadline/hint) + 1
		t.Logf("wait hint %s, %d bootstrap keys, %d reserve transaction(s) refused by the hold in %s, %d peer heartbeat(s)", hint, keys, passes, deadline, beats)
		if passes < keys || passes > keys*perKey {
			t.Fatalf("%d reserve transactions for %d keys in %s with a %s hint, want between %d and %d", passes, keys, deadline, hint, keys, keys*perKey)
		}
		if n := e.logged("owner=" + b.a.key + " heartbeat:"); n != 0 || beats == 0 {
			t.Fatalf("peer heartbeats failed %d time(s) of %d during the hold: %v", n, beats, e.logs)
		}
		e.clock.advance(55 * time.Second)
		for _, o := range []*faOwner{a, b, c} {
			o.a.heartbeatOnce(context.Background())
		}
		if rec := e.record(); rec.Recovery == nil {
			t.Fatalf("the hold ended before 60s")
		}
		e.clock.advance(6 * time.Second)
		for _, o := range []*faOwner{a, b, c} {
			o.a.heartbeatOnce(context.Background())
		}
		if rec := e.record(); rec.Recovery != nil {
			t.Fatalf("the hold did not end at about 60s: %+v", rec.Recovery)
		}
	})

	t.Run("ALocalExhaustedRefusalStillWakesOnASettle", func(t *testing.T) {
		clock := &hlLotTestClock{now: time.Unix(1890000000, 0).UTC()}
		venue := newWTVenue(t, clock.Now)
		small, err := hlInfoWeight("candleSnapshot", 0)
		if err != nil {
			t.Fatal(err)
		}
		rule, _ := hlInfoWeightRuleFor("candleSnapshot")
		rows := rule.RowCap
		big, err := hlInfoWeight("candleSnapshot", rows)
		if err != nil || big < 2*small {
			t.Fatalf("candle weights %d and %d do not leave room for the fixture", small, big)
		}
		l := newFeedRequestLedger(wtPolicy(big, 0, 0), true, clock.Now)
		l.openStartup()
		first, err := feedBudgetAcquireBounded(wtCtx(l, string(feedRestRefresh), "A|1m"), venue.srv.URL, "candleSnapshot", rows)
		if err != nil || first == nil {
			t.Fatalf("first reservation: %v", err)
		}
		ctx, cancel := context.WithTimeout(wtCtx(l, string(feedRestBootstrap), "B|1m"), feedWeightWaitTick*4/5)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			p, err := feedBudgetAcquireBounded(ctx, venue.srv.URL, "candleSnapshot", 0)
			if p != nil {
				p.release()
			}
			done <- err
		}()
		time.Sleep(feedWeightWaitTick / 5)
		first.settle(0)
		start := time.Now()
		if err := <-done; err != nil {
			t.Fatalf("a waiting bootstrap did not wake on the settle: %v", err)
		}
		t.Logf("waiter woke %s after the settle; the timer tick is %s", time.Since(start), feedWeightWaitTick)
	})
}

func TestFeedWeightSharedLatchAndFaultAlerts(t *testing.T) {
	t.Run("ARemovedDirectoryLatchesTheOwnerWithOneAlertAndNoResumeText", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		a := e.owner("a", wtPolicy(600, 100, 100))
		e.settle(a)
		a.a.sharedReport()
		if err := os.RemoveAll(e.dir); err != nil {
			t.Fatal(err)
		}
		a.a.heartbeatOnce(context.Background())
		evs := a.a.sharedReport()
		kinds := []string{}
		for _, ev := range evs {
			kinds = append(kinds, ev.Kind)
		}
		if len(evs) != 2 || evs[0].Kind != "owner_latched" || evs[1].Kind != "recovery_hold" {
			t.Fatalf("events after the directory was removed: %v", kinds)
		}
		if msg := formatFeedSharedAlert(evs[0], "fixture", "i", a.a.key); !strings.Contains(msg, "OWNER LATCHED") || !strings.Contains(msg, "until it restarts") {
			t.Fatalf("latch alert: %s", msg)
		}
		for i := 0; i < 5; i++ {
			a.a.heartbeatOnce(context.Background())
			if again := a.a.sharedReport(); len(again) != 0 {
				t.Fatalf("a repeated latch alert: %+v", again)
			}
		}
		err := faFetch(e, a, string(feedRestRefresh))
		if feedWeightRefusalCause(err) != feedWeightCauseSharedOwner || feedSharedHeldErr(err) {
			t.Fatalf("a latched read: %v (shared held %v)", err, feedSharedHeldErr(err))
		}
		e.clock.advance(feedBudgetWindow + time.Second)
		a.a.heartbeatOnce(context.Background())
		evs = a.a.sharedReport()
		if len(evs) != 1 || evs[0].Kind != "recovery_ended" || evs[0].Detail == "" {
			t.Fatalf("events at the hold end: %+v", evs)
		}
		if msg := formatFeedSharedAlert(evs[0], "fixture", "i", a.a.key); strings.Contains(msg, "resume") || !strings.Contains(msg, "stays latched closed") {
			t.Fatalf("recovered alert for a latched owner: %s", msg)
		}
		if err := faFetch(e, a, string(feedRestRefresh)); feedWeightRefusalCause(err) != feedWeightCauseSharedOwner {
			t.Fatalf("a latched owner read after the hold: %v", err)
		}
	})

	t.Run("ALatchedOwnerKeepsTheKeyUnreadyAlertPath", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		a := e.owner("a", wtPolicy(600, 100, 100))
		e.settle(a)
		if err := os.Remove(feedAuthorityOwnerLockPath(e.dir, a.a.key)); err != nil {
			t.Fatal(err)
		}
		a.a.heartbeatOnce(context.Background())
		owner := newMarketFeedOwner(e.clock.Wall, nil)
		owner.SetRequestLedger(a.l)
		ctx, cancel := context.WithTimeout(withFeedLedger(context.Background(), a.l), 5*time.Second)
		defer cancel()
		<-owner.ApplyGeneration(ctx, rlRequirements(5, 60, "BTC"))
		if held := owner.sharedHeldKeys(); len(held) != 0 {
			t.Fatalf("a latched owner marked keys shared-held, which silences the UNREADY alert: %v", held)
		}
		for _, r := range owner.Readiness() {
			if r.Ready {
				t.Fatalf("%s is ready while the owner is latched", r.Key)
			}
		}
	})

	t.Run("AnUnreadableClockSendsOneFaultAndOneRestore", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		a := e.owner("a", wtPolicy(600, 100, 100))
		e.settle(a)
		a.a.sharedReport()
		e.clock.set(func(c *faClock) { c.fail = true })
		var faults []feedSharedEvent
		for i := 0; i < 60; i++ {
			a.a.heartbeatOnce(context.Background())
			faults = append(faults, a.a.sharedReport()...)
		}
		if len(faults) != 1 || faults[0].Kind != "storage_outage" || faults[0].Reason != feedWeightCauseSharedClock {
			t.Fatalf("events over five minutes of an unreadable clock: %+v", faults)
		}
		if msg := formatFeedSharedAlert(faults[0], "fixture", "i", a.a.key); !strings.Contains(msg, "CLOCK FAULT") {
			t.Fatalf("clock fault alert: %s", msg)
		}
		if err := faFetch(e, a, string(feedRestRefresh)); feedWeightRefusalCause(err) != feedWeightCauseSharedClock {
			t.Fatalf("a read with an unreadable clock: %v", err)
		}
		if st := a.l.weightStatus().Shared; st.Registry != "clock_unavailable" {
			t.Fatalf("registry = %q, want clock_unavailable", st.Registry)
		}
		e.clock.set(func(c *faClock) { c.fail = false })
		a.a.heartbeatOnce(context.Background())
		restored := 0
		for _, ev := range a.a.sharedReport() {
			if ev.Kind == "storage_restored" {
				restored++
			}
		}
		if restored != 1 {
			t.Fatalf("restore alerts after the clock returned: %d", restored)
		}
	})

	t.Run("ANormalHoldSendsOnlyHoldAndRecoveredAndHoldsKeyAlerts", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		a := e.owner("a", wtPolicy(600, 100, 100))
		e.settle(a)
		a.a.sharedReport()
		os.Remove(filepath.Join(e.dir, feedAuthorityRecordName))
		a.a.heartbeatOnce(context.Background())
		err := faFetch(e, a, string(feedRestRefresh))
		if feedWeightRefusalCause(err) != feedWeightCauseSharedHold || !feedSharedHeldErr(err) {
			t.Fatalf("a read during the hold: %v", err)
		}
		var evs []feedSharedEvent
		for i := 0; i < 12; i++ {
			e.clock.advance(5 * time.Second)
			a.a.heartbeatOnce(context.Background())
			evs = append(evs, a.a.sharedReport()...)
		}
		evs = append(evs, a.a.sharedReport()...)
		if len(evs) != 2 || evs[0].Kind != "recovery_hold" || evs[1].Kind != "recovery_ended" || evs[1].Detail != "" {
			t.Fatalf("events over a normal hold: %+v", evs)
		}
		if msg := formatFeedSharedAlert(evs[1], "fixture", "i", a.a.key); !strings.Contains(msg, "resume") {
			t.Fatalf("recovered alert: %s", msg)
		}
	})
}

func TestFeedWeightSharedEmbeddedAdmission(t *testing.T) {
	e := newFAEnv(t)
	newWTVenue(t, e.clock.Wall)
	a := e.owner("a", wtPolicy(600, 100, 100))
	e.settle(a)
	policy := wtPolicy(400, 100, 100).weightPolicy()
	feed := newFeedWeightAuthority(feedAuthorityRoleFeed, filepath.Join(e.root, "cfg-feed.json"), e.logf)
	feed.attach(newFeedRequestLedger(wtPolicy(400, 100, 100), true, nil))
	if code := startFeedAuthority(feed, policy, nil, "[fixture]"); code != ExitProbeFailure {
		t.Fatalf("a feed owner with an unsafe admission returned %d, want %d", code, ExitProbeFailure)
	}
	emb := &faOwner{name: "embedded", policy: wtPolicy(400, 100, 100)}
	emb.l = newFeedRequestLedger(emb.policy, false, nil)
	emb.a = newFeedWeightAuthority(feedAuthorityRoleEmbedded, filepath.Join(e.root, "cfg-embedded.json"), e.logf)
	emb.a.attach(emb.l)
	if code := startFeedAuthority(emb.a, policy, nil, "[fixture]"); code != 0 {
		t.Fatalf("the embedded scheduler with an unsafe admission returned %d, want 0 so protection keeps running", code)
	}
	emb.l.openStartup()
	if err := faFetch(e, emb, string(feedRestRefresh)); !feedSharedHeldErr(err) {
		t.Fatalf("a held embedded owner read: %v", err)
	}
	emb.a.heartbeatOnce(context.Background())
	if emb.a.registered {
		t.Fatalf("the embedded owner registered while the declarations still do not fit")
	}
	a.a.stop()
	emb.a.heartbeatOnce(context.Background())
	if !emb.a.registered {
		t.Fatalf("the embedded owner did not register on the heartbeat after the peer left: %v", emb.a.registerErr)
	}
}
