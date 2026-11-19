package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func faEvidence(t *testing.T, e *faEnv, key string) *feedAuthPolicyEvidence {
	t.Helper()
	ev, exists, err := feedReadOnePolicyEvidence(e.dir, key)
	if err != nil || !exists || ev == nil {
		t.Fatalf("policy evidence of %s: exists %t valid %t err %v", key, exists, ev != nil, err)
	}
	return ev
}

func faOwnerStatus(st feedSharedStatus, key string) *feedSharedOwnerStatus {
	for i := range st.Owners {
		if st.Owners[i].Key == key {
			return &st.Owners[i]
		}
	}
	return nil
}

func faHeld(t *testing.T, e *faEnv, key string, reserve int, stage string) *feedAuthRecord {
	t.Helper()
	rec := e.record()
	o := rec.Owners[key]
	if o == nil || o.Active || o.Policy.Reserve != reserve {
		t.Fatalf("%s: owner %s is not a stopped owner holding reserve %d: %+v", stage, key, reserve, o)
	}
	return rec
}

func faEmbedded(e *faEnv, name string, policy *feedBudgetConfig) *faOwner {
	o := &faOwner{name: name, policy: policy}
	o.l = newFeedRequestLedger(policy, false, nil)
	o.a = newFeedWeightAuthority(feedAuthorityRoleEmbedded, filepath.Join(e.root, "cfg-"+name+".json"), e.logf)
	o.a.attach(o.l)
	return o
}

func TestFeedWeightSharedHeldReserve(t *testing.T) {
	t.Run("AStoppedOwnersReserveStaysHeldThroughPruningRestartsAndRecovery", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		c := e.owner("c", wtPolicy(300, 0, 200))
		b := e.owner("b", wtPolicy(400, 100, 100))
		e.settle(c, b)
		if err := faFetch(e, c, string(feedRestRefresh)); err != nil {
			t.Fatal(err)
		}
		c.a.stop()
		e.clock.advance(feedBudgetWindow + time.Second)
		b.a.heartbeatOnce(context.Background())
		rec := faHeld(t, e, c.a.key, 200, "after pruning")
		if charged, _, _ := faCharged(rec, c.a.key); charged != 0 || rec.commonReserve() != 200 || rec.capacity() != 1000 || rec.combinedAllocation() != 500 {
			t.Fatalf("after pruning: charged %d reserve %d capacity %d combined %d", charged, rec.commonReserve(), rec.capacity(), rec.combinedAllocation())
		}
		st := b.l.weightStatus().Shared
		if ost := faOwnerStatus(st, c.a.key); ost == nil || ost.Active || ost.Reserve != 200 || ost.Allocation != 0 || st.CommonReserve != 200 || st.Capacity != 1000 {
			t.Fatalf("status of the stopped owner: %+v common %d capacity %d", ost, st.CommonReserve, st.Capacity)
		}
		b.a.stop()
		b = e.owner("b", wtPolicy(400, 100, 100))
		faHeld(t, e, c.a.key, 200, "after a peer restart")
		for _, tc := range []struct {
			name   string
			damage func()
			reason string
		}{
			{"boot", func() { e.clock.set(func(c *faClock) { c.boot = "boot-b"; c.ns = int64(time.Hour) }) }, feedAuthorityRecoveryBoot},
			{"regressed", func() { e.clock.set(func(c *faClock) { c.ns -= int64(10 * time.Minute) }) }, feedAuthorityRecoveryRegressed},
			{"missing", func() { os.Remove(filepath.Join(e.dir, feedAuthorityRecordName)) }, feedAuthorityRecoveryMissing},
			{"corrupt", func() { os.WriteFile(filepath.Join(e.dir, feedAuthorityRecordName), []byte("{"), 0o600) }, feedAuthorityRecoveryCorrupt},
		} {
			tc.damage()
			b.a.heartbeatOnce(context.Background())
			rec := faHeld(t, e, c.a.key, 200, tc.name)
			if rec.Recovery == nil || rec.Recovery.Reason != tc.reason || rec.capacity() != 1000 {
				t.Fatalf("%s: recovery %+v capacity %d", tc.name, rec.Recovery, rec.capacity())
			}
			e.settle(b)
			faHeld(t, e, c.a.key, 200, tc.name+" after the hold")
		}
	})

	t.Run("AnInactiveReserveRefusesACombinedAllocationThatFitsOnlyWithoutIt", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		c := e.owner("c", wtPolicy(300, 0, 200))
		b := e.owner("b", wtPolicy(400, 100, 100))
		e.settle(c, b)
		c.a.stop()
		d := wtPolicy(500, 50, 100)
		for _, role := range []string{feedAuthorityRoleFeed, feedAuthorityRoleEmbedded} {
			before := faTree(t, e.root)
			fc := forecastFeedAuthority(role, filepath.Join(e.root, "cfg-d.json"), d.weightPolicy())
			if fc.Refuse == "" {
				t.Fatalf("%s probe of combined 1050 under a held reserve 200 passed: %+v", role, fc)
			}
			if faTree(t, e.root) != before {
				t.Fatalf("the %s probe changed the state directory", role)
			}
		}
		feed := e.newOwner("d", d)
		if code := startFeedAuthority(feed.a, d.weightPolicy(), nil, "[fixture]"); code != ExitProbeFailure {
			t.Fatalf("feed startup returned %d, want %d", code, ExitProbeFailure)
		}
		emb := faEmbedded(e, "embedded", d)
		if code := startFeedAuthority(emb.a, d.weightPolicy(), nil, "[fixture]"); code != 0 || emb.a.registered {
			t.Fatalf("the embedded owner returned %d registered %t, want 0 and a held registration", code, emb.a.registered)
		}
		emb.l.openStartup()
		if err := faFetch(e, emb, string(feedRestRefresh)); !feedSharedHeldErr(err) {
			t.Fatalf("a held embedded owner read: %v", err)
		}
		emb.a.heartbeatOnce(context.Background())
		if emb.a.registered {
			t.Fatalf("the embedded owner registered on a retry while the held reserve still refuses it")
		}
		applied := false
		err := b.a.redeclare(context.Background(), wtPolicy(900, 150, 100).weightPolicy(), func() { applied = true })
		var unsafe *feedAuthorityUnsafeError
		if !errors.As(err, &unsafe) || unsafe.Reserve != 200 || unsafe.ReserveOwner != c.a.key || unsafe.Capacity != 1000 || applied {
			t.Fatalf("a feed reload to combined 1050 under the held reserve: %v applied %t", err, applied)
		}
		if rec := e.record(); rec.Owners[b.a.key].Policy.PerMinute != 400 {
			t.Fatalf("a refused reload changed the record: %+v", rec.Owners[b.a.key].Policy)
		}
	})

	t.Run("SameKeyRegistrationAndReloadReplaceTheHeldReserve", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		c := e.owner("c", wtPolicy(300, 0, 200))
		b := e.owner("b", wtPolicy(400, 100, 150))
		x := e.owner("x", wtPolicy(100, 0, 100))
		e.settle(c, b, x)
		c.a.stop()
		tooBig := e.newOwner("c", wtPolicy(600, 0, 50))
		var unsafe *feedAuthorityUnsafeError
		if err := tooBig.a.register(context.Background(), tooBig.policy.weightPolicy()); !errors.As(err, &unsafe) {
			t.Fatalf("an oversize restart of c: %v", err)
		}
		faHeld(t, e, c.a.key, 200, "after a refused restart")
		if ev := faEvidence(t, e, c.a.key); ev.Staged != nil || ev.Committed.Reserve != 200 {
			t.Fatalf("a refused admission changed the policy evidence: %+v", ev)
		}
		c2 := e.owner("c", wtPolicy(300, 0, 50))
		if rec := e.record(); rec.commonReserve() != 150 || rec.Owners[c2.a.key].Policy.Reserve != 50 {
			t.Fatalf("a same-key restart did not lower the common reserve to the next held value: %d", rec.commonReserve())
		}
		applied := false
		if err := b.a.redeclare(context.Background(), wtPolicy(400, 100, 0).weightPolicy(), func() { applied = true }); err != nil || !applied {
			t.Fatalf("an explicit zero reserve reload: %v applied %t", err, applied)
		}
		if rec := e.record(); rec.commonReserve() != 100 || rec.Owners[b.a.key].Policy.Reserve != 0 {
			t.Fatalf("explicit zero: common reserve %d", rec.commonReserve())
		}
		if ev := faEvidence(t, e, b.a.key); ev.Staged != nil || ev.Committed.Reserve != 0 {
			t.Fatalf("explicit zero evidence: %+v", ev)
		}
		before := e.record().Owners[b.a.key].Policy
		applied = false
		var refusal *feedWeightRefusal
		if err := b.a.redeclare(context.Background(), nil, func() { applied = true }); !errors.As(err, &refusal) || applied {
			t.Fatalf("a reload that removes the policy of a registered owner: %v applied %t", err, applied)
		}
		if got := e.record().Owners[b.a.key].Policy; got != before {
			t.Fatalf("a rejected policy removal changed the record: %+v -> %+v", before, got)
		}

		ledger := newFeedRequestLedger(wtPolicy(400, 100, 0), true, nil)
		rt := &feedRuntime{cfg: &Config{Feed: &FeedRoleConfig{RequestBudget: wtPolicy(400, 100, 0)}}, ledger: ledger, authority: b.a, ctx: context.Background()}
		removed := &Config{Feed: &FeedRoleConfig{RequestBudget: &feedBudgetConfig{PerMinute: 1000, Startup: 1000}}}
		if err := rt.applyBudget(removed); !errors.As(err, &refusal) {
			t.Fatalf("a feed reload without a weight policy for a registered owner: %v", err)
		}
	})

	t.Run("SameKeyReplacementAtTheOwnerCap", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		a := e.owner("a", wtPolicy(300, 0, 100))
		c := e.owner("c", wtPolicy(300, 0, 200))
		e.settle(a, c)
		c.a.stop()
		host := feedHostOf(hlMainnetURL)
		if err := a.a.tx(context.Background(), "test", func(t *feedAuthTx) error {
			for i := 0; len(t.rec.Owners) < feedAuthorityMaxOwners; i++ {
				o := feedHeldOwnerRow(feedAuthIdentity{Role: feedAuthorityRoleFeed}, feedAuthPolicy{}, "", t.now.Ns)
				o.Charges = append(o.Charges, &feedAuthCharge{ID: fmt.Sprintf("fixture-%d", i), Host: host, AtNs: t.now.Ns, AtWall: feedWallString(t.now.Wall)})
				t.rec.Owners[fmt.Sprintf("%032x", i+1)] = o
			}
			t.dirty = true
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if n := len(e.record().Owners); n != feedAuthorityMaxOwners {
			t.Fatalf("fixture holds %d owners, want %d", n, feedAuthorityMaxOwners)
		}
		fresh := e.newOwner("fresh", wtPolicy(100, 0, 0))
		var refusal *feedWeightRefusal
		if err := fresh.a.register(context.Background(), fresh.policy.weightPolicy()); !errors.As(err, &refusal) || refusal.Cause != feedWeightCauseSharedOwner {
			t.Fatalf("a new key above the owner cap: %v", err)
		}
		again := e.owner("c", wtPolicy(300, 0, 150))
		if rec := e.record(); len(rec.Owners) != feedAuthorityMaxOwners || rec.Owners[again.a.key].Policy.Reserve != 150 || !rec.Owners[again.a.key].Active {
			t.Fatalf("same-key replacement at the cap: %d owners %+v", len(rec.Owners), rec.Owners[again.a.key])
		}
	})

	t.Run("InterruptedPolicyTransitionsKeepTheLargerReserve", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		a := e.owner("a", wtPolicy(300, 0, 200))
		b := e.owner("b", wtPolicy(400, 100, 100))
		e.settle(a, b)
		failAt := func(point string) {
			armed := false
			feedAuthorityHook = func(p string) error {
				if p == "policy:staged" {
					armed = true
				}
				if p == point && (armed || point == "policy:stage") {
					return fmt.Errorf("injected %s failure", point)
				}
				return nil
			}
		}
		clear := func() {
			feedAuthorityHook = nil
			time.Sleep(feedAuthorityFaultRetry + 50*time.Millisecond)
		}

		failAt("policy:stage")
		if err := a.a.redeclare(context.Background(), wtPolicy(300, 0, 50).weightPolicy(), func() {}); feedWeightRefusalCause(err) != feedWeightCauseSharedStorage {
			t.Fatalf("a failed stage write: %v", err)
		}
		clear()
		if ev := faEvidence(t, e, a.a.key); ev.Staged != nil || ev.Committed.Reserve != 200 || e.record().Owners[a.a.key].Policy.Reserve != 200 {
			t.Fatalf("a failed stage changed the evidence or the record: %+v", ev)
		}

		failAt("write")
		if err := a.a.redeclare(context.Background(), wtPolicy(300, 0, 50).weightPolicy(), func() {}); feedWeightRefusalCause(err) != feedWeightCauseSharedStorage {
			t.Fatalf("a lowering reload whose record write fails: %v", err)
		}
		clear()
		if ev := faEvidence(t, e, a.a.key); ev.Staged == nil || ev.held().Reserve != 200 {
			t.Fatalf("an aborted lowering left evidence %+v, want the staged pair holding 200", ev)
		}
		if a.a.policy.Reserve != 200 || e.record().Owners[a.a.key].Policy.Reserve != 200 {
			t.Fatalf("an aborted lowering changed the owner or the record")
		}
		os.Remove(filepath.Join(e.dir, feedAuthorityRecordName))
		b.a.heartbeatOnce(context.Background())
		if rec := e.record(); rec.commonReserve() != 200 {
			t.Fatalf("a rebuild between the stage and the record write lowered the reserve to %d", rec.commonReserve())
		}
		e.settle(a, b)
		if ev := faEvidence(t, e, a.a.key); ev.Staged != nil || ev.Committed.Reserve != 200 {
			t.Fatalf("the next transactions did not settle the aborted stage to the record: %+v", ev)
		}

		failAt("policy:finalize")
		if err := a.a.redeclare(context.Background(), wtPolicy(300, 0, 300).weightPolicy(), func() {}); err != nil {
			t.Fatalf("a raise whose evidence finalize fails still commits in the record: %v", err)
		}
		clear()
		if ev := faEvidence(t, e, a.a.key); ev.Staged == nil || ev.held().Reserve != 300 {
			t.Fatalf("a raise with a failed finalize left evidence %+v", ev)
		}
		os.WriteFile(filepath.Join(e.dir, feedAuthorityRecordName), []byte("{"), 0o600)
		b.a.heartbeatOnce(context.Background())
		if rec := e.record(); rec.commonReserve() != 300 {
			t.Fatalf("a rebuild after a raise with a failed finalize holds %d, want 300", rec.commonReserve())
		}
		e.settle(a, b)
		if ev := faEvidence(t, e, a.a.key); ev.Staged != nil || ev.Committed.Reserve != 300 {
			t.Fatalf("the raise evidence was not finalized: %+v", ev)
		}
	})
}

func TestFeedWeightSharedRebuildUsesDeclarationsPerOwner(t *testing.T) {
	writeDeclaration := func(t *testing.T, e *faEnv, o *faOwner, raw []byte) {
		t.Helper()
		if raw == nil {
			raw, _ = json.Marshal(feedAuthDeclaration{Role: o.a.role, ConfigPath: o.a.configPath, Incarnation: o.a.incarnation, PID: o.a.pid, Policy: feedAuthPolicyOf(o.policy.weightPolicy())})
		}
		if err := os.WriteFile(feedAuthorityOwnerDeclPath(e.dir, o.a.key), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("PartialEvidenceKeepsAStoppedDeclarationInRebuildAndForecast", func(t *testing.T) {
		e := newFAEnv(t)
		c := e.owner("c", wtPolicy(300, 0, 200))
		c.a.stop()
		if err := os.Remove(filepath.Join(e.dir, feedAuthorityRecordName)); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(feedAuthorityPolicyDir(e.dir)); err != nil {
			t.Fatal(err)
		}
		writeDeclaration(t, e, c, nil)
		b := e.newOwner("b", wtPolicy(400, 100, 100))
		feedAuthorityHook = func(point string) error {
			if point == "policy:staged" {
				return fmt.Errorf("injected kill after policy staging")
			}
			return nil
		}
		if err := b.a.register(context.Background(), b.policy.weightPolicy()); err == nil {
			t.Fatal("registration unexpectedly passed the injected policy staging failure")
		}
		feedAuthorityHook = nil
		fc := forecastFeedAuthority(feedAuthorityRoleFeed, filepath.Join(e.root, "cfg-new.json"), wtPolicy(100, 0, 0).weightPolicy())
		if lines := strings.Join(fc.Lines, "\n"); fc.Refuse != "" || !strings.Contains(lines, "common_reserve=200") || !strings.Contains(lines, "capacity=1000") {
			t.Fatalf("forecast with partial evidence: %+v", fc)
		}
		e.owner("b", wtPolicy(400, 100, 100))
		if rec := faHeld(t, e, c.a.key, 200, "rebuild with partial evidence"); rec.capacity() != 1000 {
			t.Fatalf("rebuild capacity %d, want 1000", rec.capacity())
		}
	})

	t.Run("ReleasedEvidenceWinsOverAnOlderDeclaration", func(t *testing.T) {
		e := newFAEnv(t)
		c := e.owner("c", wtPolicy(300, 0, 200))
		c.a.stop()
		if out, err := newFeedWeightReleaser(e.logf).releaseOwner(context.Background(), c.a.key); err != nil || !out.Released {
			t.Fatalf("release fixture: %+v %v", out, err)
		}
		writeDeclaration(t, e, c, nil)
		rb, err := feedRebuildRows(e.dir, "", false, 0)
		if err != nil {
			t.Fatal(err)
		}
		if rb.Rows[c.a.key] != nil {
			t.Fatalf("released owner returned through its older declaration: %+v", rb.Rows[c.a.key])
		}
	})

	t.Run("CorruptEvidenceWinsOverAValidDeclaration", func(t *testing.T) {
		e := newFAEnv(t)
		c := e.owner("c", wtPolicy(300, 0, 200))
		c.a.stop()
		if err := os.WriteFile(feedAuthorityPolicyPath(e.dir, c.a.key), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		writeDeclaration(t, e, c, nil)
		rb, err := feedRebuildRows(e.dir, "", false, 0)
		if err != nil {
			t.Fatal(err)
		}
		if o := rb.Rows[c.a.key]; o == nil || !o.Unproven || len(rb.Unproven) != 1 || rb.Unproven[0] != c.a.key {
			t.Fatalf("corrupt evidence with a valid declaration: owner %+v unproven %v", o, rb.Unproven)
		}
	})
}

func TestFeedWeightSharedReserveEvidence(t *testing.T) {
	t.Run("UnprovableEvidenceHoldsSendsPastTheRecoveryUntilARelease", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		c := e.owner("c", wtPolicy(300, 0, 200))
		b := e.owner("b", wtPolicy(400, 100, 100))
		e.settle(c, b)
		c.a.stop()
		b.a.sharedReport()
		if err := os.WriteFile(feedAuthorityPolicyPath(e.dir, c.a.key), []byte(`{"version":2,"key":`), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Remove(filepath.Join(e.dir, feedAuthorityRecordName))
		b.a.heartbeatOnce(context.Background())
		e.settle(b)
		rec := e.record()
		if o := rec.Owners[c.a.key]; o == nil || !o.Unproven || o.Active {
			t.Fatalf("a stopped owner with corrupt evidence is not unproven after the rebuild: %+v", o)
		}
		err := faFetch(e, b, string(feedRestRefresh))
		if feedWeightRefusalCause(err) != feedWeightCauseSharedHold || !feedSharedHeldErr(err) {
			t.Fatalf("a read past the recovery hold with an unproven reserve: %v", err)
		}
		st := b.l.weightStatus().Shared
		if st.Registry != "reserve_unproven" || len(st.UnprovenOwners) != 1 || st.UnprovenOwners[0] != c.a.key {
			t.Fatalf("status with an unproven reserve: registry %q unproven %v", st.Registry, st.UnprovenOwners)
		}
		kinds := map[string]int{}
		for _, ev := range b.a.sharedReport() {
			kinds[ev.Kind]++
		}
		if kinds["reserve_unproven"] != 1 {
			t.Fatalf("events with an unproven reserve: %v", kinds)
		}
		if fc := forecastFeedAuthority(feedAuthorityRoleFeed, filepath.Join(e.root, "cfg-x.json"), wtPolicy(100, 0, 0).weightPolicy()); fc.Refuse == "" {
			t.Fatalf("a probe beside an unproven reserve passed: %+v", fc)
		}
		out, err := newFeedWeightReleaser(e.logf).releaseOwner(context.Background(), c.a.key)
		if err != nil || !out.Released {
			t.Fatalf("release of the stopped unproven owner: %+v %v", out, err)
		}
		if err := faFetch(e, b, string(feedRestRefresh)); err != nil {
			t.Fatalf("a read after the release: %v", err)
		}
		os.Remove(filepath.Join(e.dir, feedAuthorityRecordName))
		b.a.heartbeatOnce(context.Background())
		e.settle(b)
		if rec := e.record(); rec.Owners[c.a.key] != nil || rec.commonReserve() != 100 {
			t.Fatalf("a committed release reappeared on rebuild: %+v", rec.Owners[c.a.key])
		}
	})

	t.Run("UnreadableEvidenceRefusesTheRebuild", func(t *testing.T) {
		e := newFAEnv(t)
		newWTVenue(t, e.clock.Wall)
		b := e.owner("b", wtPolicy(400, 100, 100))
		e.settle(b)
		os.Remove(filepath.Join(e.dir, feedAuthorityRecordName))
		if err := os.Chmod(feedAuthorityPolicyDir(e.dir), 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(feedAuthorityPolicyDir(e.dir), 0o700) })
		time.Sleep(feedAuthorityFaultRetry + 50*time.Millisecond)
		if err := faFetch(e, b, string(feedRestRefresh)); feedWeightRefusalCause(err) != feedWeightCauseSharedStorage {
			t.Fatalf("a read while the evidence cannot be read: %v", err)
		}
		if _, err := os.Stat(filepath.Join(e.dir, feedAuthorityRecordName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a rebuild without readable evidence wrote a record: %v", err)
		}
	})

	t.Run("ReleaseRefusesLiveAndUnprovableOwnersAndKeepsLiabilities", func(t *testing.T) {
		e := newFAEnv(t)
		venue := newWTVenue(t, e.clock.Wall)
		c := e.owner("c", wtPolicy(800, 0, 200))
		b := e.owner("b", wtPolicy(100, 0, 100))
		y := e.owner("y", wtPolicy(100, 0, 150))
		e.settle(c, b, y)
		for i := 0; i < 3; i++ {
			if err := faFetch(e, c, string(feedRestRefresh)); err != nil {
				t.Fatal(err)
			}
		}
		venue.set(func(v *wtVenue) { v.candleExtra = 200 })
		if err := faFetch(e, c, string(feedRestRefresh)); err == nil {
			t.Fatal("an over-bound response was accepted")
		}
		venue.set(func(v *wtVenue) { v.candleExtra = 0 })
		c.a.stop()
		y.a.stop()
		rel := newFeedWeightReleaser(e.logf)
		recBefore := e.record()
		chargedBefore, debtBefore, _ := faCharged(recBefore, c.a.key)
		boundsBefore := len(recBefore.Owners[c.a.key].Bounds)
		if debtBefore == 0 || boundsBefore == 0 {
			t.Fatalf("fixture: debt %d bounds %d", debtBefore, boundsBefore)
		}

		var refusal *feedWeightRefusal
		if _, err := rel.releaseOwner(context.Background(), b.a.key); !errors.As(err, &refusal) || refusal.Cause != feedWeightCauseSharedOwner {
			t.Fatalf("release of a live owner: %v", err)
		}
		if o := e.record().Owners[b.a.key]; !o.Active || o.Policy.Reserve != 100 {
			t.Fatalf("a refused release changed the live owner: %+v", o)
		}
		if err := os.Remove(feedAuthorityOwnerLockPath(e.dir, y.a.key)); err != nil {
			t.Fatal(err)
		}
		if _, err := rel.releaseOwner(context.Background(), y.a.key); !errors.As(err, &refusal) {
			t.Fatalf("release of an owner whose lifetime lock is missing: %v", err)
		}
		faHeld(t, e, y.a.key, 150, "after a refused unprovable release")
		if _, err := rel.releaseOwner(context.Background(), "not-a-key"); err == nil {
			t.Fatalf("an invalid owner key was accepted")
		}

		out, err := rel.releaseOwner(context.Background(), c.a.key)
		if err != nil || !out.Released {
			t.Fatalf("release of the stopped owner: %+v %v", out, err)
		}
		rec := e.record()
		charged, debt, _ := faCharged(rec, c.a.key)
		if o := rec.Owners[c.a.key]; o == nil || o.Policy.Reserve != 0 || charged != chargedBefore || debt != debtBefore || len(o.Bounds) != boundsBefore || rec.commonReserve() != 150 {
			t.Fatalf("release: owner %+v charged %d/%d debt %d/%d common reserve %d", o, charged, chargedBefore, debt, debtBefore, rec.commonReserve())
		}
		if ev := faEvidence(t, e, c.a.key); !ev.Released || ev.Staged != nil || ev.Committed.Reserve != 0 {
			t.Fatalf("release evidence: %+v", ev)
		}
		before := e.recordBytes()
		again, err := rel.releaseOwner(context.Background(), c.a.key)
		if err != nil || again.Released {
			t.Fatalf("a repeated release: %+v %v", again, err)
		}
		if !bytes.Equal(before, e.recordBytes()) {
			t.Fatalf("a repeated release changed the record")
		}
	})
}

func TestFeedWeightSharedReserveShortfall(t *testing.T) {
	e := newFAEnv(t)
	hot := newFARateVenue(t)
	cool := newFARateVenue(t)
	hotHost, coolHost := feedHostOf(hot.srv.URL), feedHostOf(cool.srv.URL)
	a := e.owner("a", wtPolicy(400, 0, 100))
	e.settle(a)
	if err := a.a.tx(context.Background(), "test", func(t *feedAuthTx) error {
		me := t.owner()
		me.Charges = append(me.Charges, &feedAuthCharge{ID: "fixture-hot", Host: hotHost, Weight: 1000, AtNs: t.now.Ns, AtWall: feedWallString(t.now.Wall)})
		t.dirty = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	spec := feedAcquireSpec{host: hotHost, reason: string(feedRestRefresh), reqType: "allMids", input: "mids", rows: -1}
	pending, err := a.l.acquireShared(context.Background(), spec, false)
	if err != nil {
		t.Fatal(err)
	}
	sending, err := a.l.acquireShared(context.Background(), spec, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := sending.beginSend(); err != nil {
		t.Fatal(err)
	}
	b := e.owner("b", wtPolicy(100, 0, 300))
	if e.logged("common reserve 100 -> 300 capacity=900 hosts_over_capacity="+hotHost) != 1 {
		t.Fatalf("no commit line names the over-capacity host: %v", e.logs)
	}
	if err := pending.beginSend(); feedWeightRefusalCause(err) != feedWeightCauseSharedExhausted {
		t.Fatalf("a pending permit on the over-capacity host moved to sending: %v", err)
	}
	if err := faMids(a, hot.srv.URL); feedWeightRefusalCause(err) != feedWeightCauseSharedExhausted {
		t.Fatalf("a new reservation on the over-capacity host: %v", err)
	}
	if err := faMids(b, cool.srv.URL); err != nil {
		t.Fatalf("a host with room refused a read: %v", err)
	}
	rec := e.record()
	inFlight := 0
	for _, p := range rec.Owners[a.a.key].Permits {
		if p.State == feedPermitStateSending && p.Host == hotHost {
			inFlight += p.Cost
		}
	}
	if inFlight == 0 {
		t.Fatalf("the request already marked sending is no longer charged: %+v", rec.Owners[a.a.key].Permits)
	}
	sending.finish(200, "")
	sending.settle(0)
	used := e.record().usedOnHost(hotHost)
	st := a.l.weightStatus().Shared
	var hs *feedSharedHostStatus
	for i := range st.Hosts {
		if st.Hosts[i].Host == hotHost {
			hs = &st.Hosts[i]
		}
	}
	if hs == nil || hs.Shortfall != used-900 || st.Capacity != 900 {
		t.Fatalf("cached status shortfall for %s: %+v (used %d, capacity %d)", hotHost, hs, used, st.Capacity)
	}
	if e.logged("host="+hotHost+" reserve shortfall=") == 0 {
		t.Fatalf("no shortfall line for the host: %v", e.logs)
	}
	if err := b.a.tx(context.Background(), "test", func(t *feedAuthTx) error {
		t.commitPause(coolHost, t.now.Ns+int64(feedRateLimitPause), t.now.Wall.Add(feedRateLimitPause), "")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := faMids(b, cool.srv.URL); !errors.Is(err, errFeedHostPaused) {
		t.Fatalf("a committed pause no longer applies to its host: %v", err)
	}
	e.clock.advance(feedBudgetWindow + time.Second)
	a.a.heartbeatOnce(context.Background())
	st = a.l.weightStatus().Shared
	for _, h := range st.Hosts {
		if h.Shortfall != 0 {
			t.Fatalf("the shortfall did not clear after use fell: %+v", h)
		}
	}
	if e.logged("host="+hotHost+" reserve shortfall cleared") == 0 {
		t.Fatalf("no cleared line for the host: %v", e.logs)
	}
	if err := faMids(a, hot.srv.URL); err != nil {
		t.Fatalf("a read after the shortfall cleared: %v", err)
	}
}

func TestFeedWeightSharedMigrationRecoveryInstructions(t *testing.T) {
	e := newFAEnv(t)
	newWTVenue(t, e.clock.Wall)
	live := e.owner("live", wtPolicy(400, 100, 100))
	broken := e.owner("broken", wtPolicy(100, 0, 50))
	e.settle(live, broken)
	broken.a.stop()
	rec := e.record()
	delete(rec.Owners, broken.a.key)
	rec.Version = feedAuthorityLegacyVersion
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, feedAuthorityRecordName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(feedAuthorityPolicyDir(e.dir)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{broken.a.key, "notes"} {
		if err := os.WriteFile(filepath.Join(e.dir, feedAuthorityOwnersDir, name+".json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lockPath := feedAuthorityOwnerLockPath(e.dir, broken.a.key)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	live.a.heartbeatOnce(context.Background())
	rec = e.record()
	if o := rec.Owners[broken.a.key]; o == nil || !o.Unproven {
		t.Fatalf("missing-lock declaration did not become unproven: %+v", o)
	}
	if rec.Owners["notes"] != nil {
		t.Fatalf("invalid declaration name created an owner: %+v", rec.Owners["notes"])
	}
	rel := newFeedWeightReleaser(e.logf)
	if _, err := rel.releaseOwner(context.Background(), broken.a.key); err == nil || !strings.Contains(err.Error(), "create "+lockPath+" with mode 0600") {
		t.Fatalf("missing-lock release instructions: %v", err)
	}
	if alert := formatFeedSharedAlert(feedSharedEvent{Kind: "reserve_unproven", Detail: broken.a.key}, "feed", "test", live.a.key); !strings.Contains(alert, "create owners/<key>.lock with mode 0600") {
		t.Fatalf("unproven alert instructions: %s", alert)
	}
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := rel.releaseOwner(context.Background(), broken.a.key); err != nil || !out.Released {
		t.Fatalf("release after following the missing-lock instructions: %+v %v", out, err)
	}
	e.settle(live)
	live.a.heartbeatOnce(context.Background())
	if err := faFetch(e, live, string(feedRestRefresh)); err != nil {
		t.Fatalf("read after following the missing-lock instructions: %v", err)
	}
}

func TestFeedWeightSharedRecordUpgrade(t *testing.T) {
	e := newFAEnv(t)
	newWTVenue(t, e.clock.Wall)
	live := e.owner("live", wtPolicy(400, 100, 100))
	gone := e.owner("gone", wtPolicy(200, 0, 250))
	broken := e.owner("broken", wtPolicy(100, 0, 50))
	e.settle(live, gone, broken)
	for i := 0; i < 2; i++ {
		if err := faFetch(e, live, string(feedRestRefresh)); err != nil {
			t.Fatal(err)
		}
	}
	gone.a.stop()
	broken.a.stop()
	e.clock.advance(feedBudgetWindow + time.Second)
	live.a.heartbeatOnce(context.Background())
	if err := faFetch(e, live, string(feedRestRefresh)); err != nil {
		t.Fatal(err)
	}
	rec := e.record()
	charged, _, _ := faCharged(rec, live.a.key)
	rec.Version = feedAuthorityLegacyVersion
	for key, o := range rec.Owners {
		o.PolicyTx = ""
		if !o.Active {
			delete(rec.Owners, key)
		}
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, feedAuthorityRecordName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(feedAuthorityPolicyDir(e.dir)); err != nil {
		t.Fatal(err)
	}
	for _, o := range []*faOwner{live, gone, broken} {
		decl, _ := json.Marshal(feedAuthDeclaration{Role: o.a.role, ConfigPath: o.a.configPath, Incarnation: o.a.incarnation, PID: o.a.pid, Policy: feedAuthPolicyOf(o.policy.weightPolicy())})
		if o == broken {
			decl = []byte("{")
		}
		if err := os.WriteFile(filepath.Join(e.dir, feedAuthorityOwnersDir, o.a.key+".json"), decl, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := faTree(t, e.root)
	fc := forecastFeedAuthority(feedAuthorityRoleFeed, filepath.Join(e.root, "cfg-new.json"), wtPolicy(100, 0, 0).weightPolicy())
	if faTree(t, e.root) != before || fc.Refuse == "" {
		t.Fatalf("a read-only forecast of a version 1 record with an unprovable stopped owner: %+v", fc)
	}
	live.a.heartbeatOnce(context.Background())
	rec = e.record()
	if rec.Version != feedAuthorityVersion || rec.MigratedFrom != feedSHA256(raw) {
		t.Fatalf("migration: version %d from %q", rec.Version, rec.MigratedFrom)
	}
	if got, _, _ := faCharged(rec, live.a.key); got != charged {
		t.Fatalf("migration changed the live owner's accounting: %d -> %d", charged, got)
	}
	faHeld(t, e, gone.a.key, 250, "after the upgrade")
	if o := rec.Owners[broken.a.key]; o == nil || !o.Unproven {
		t.Fatalf("a stopped owner with an unreadable version 1 declaration is not unproven: %+v", o)
	}
	if ev := faEvidence(t, e, gone.a.key); ev.Committed.Reserve != 250 {
		t.Fatalf("migration evidence of the stopped owner: %+v", ev)
	}
	if err := faFetch(e, live, string(feedRestRefresh)); feedWeightRefusalCause(err) != feedWeightCauseSharedHold {
		t.Fatalf("a read beside an unproven migrated reserve: %v", err)
	}
	out, err := newFeedWeightReleaser(e.logf).releaseOwner(context.Background(), broken.a.key)
	if err != nil || !out.Released {
		t.Fatalf("release of the unprovable migrated owner: %+v %v", out, err)
	}
	if err := faFetch(e, live, string(feedRestRefresh)); err != nil {
		t.Fatalf("a read after the reconciliation: %v", err)
	}
	future := []byte(`{"version":3}`)
	os.WriteFile(filepath.Join(e.dir, feedAuthorityRecordName), future, 0o600)
	time.Sleep(feedAuthorityFaultRetry + 50*time.Millisecond)
	live.a.heartbeatOnce(context.Background())
	if got := e.recordBytes(); !bytes.Equal(got, future) {
		t.Fatalf("an unsupported version was overwritten: %s", got)
	}
}
