package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	feedAuthorityRecordCapability = "go-trader-capability:feed-weight-record-v2"
	feedAuthorityLegacyVersion    = 1
	feedAuthorityPoliciesDir      = "policies"
	feedAuthorityPolicyVersion    = 2
	feedAuthorityPolicyMaxBytes   = 64 << 10
	feedAuthorityPolicyMaxFiles   = 4096
)

type feedAuthIdentity struct {
	Role        string `json:"role,omitempty"`
	ConfigPath  string `json:"config_path,omitempty"`
	Incarnation string `json:"incarnation,omitempty"`
	PID         int    `json:"pid,omitempty"`
	StartedWall string `json:"started_wall,omitempty"`
}

type feedAuthPolicyStage struct {
	Tx      string         `json:"tx"`
	Policy  feedAuthPolicy `json:"policy"`
	Release bool           `json:"release,omitempty"`
}

type feedAuthPolicyEvidence struct {
	Version int    `json:"version"`
	Key     string `json:"key"`
	feedAuthIdentity
	Committed feedAuthPolicy       `json:"committed"`
	Tx        string               `json:"tx,omitempty"`
	Released  bool                 `json:"released,omitempty"`
	Staged    *feedAuthPolicyStage `json:"staged,omitempty"`
}

func feedMaxPolicy(a, b feedAuthPolicy) feedAuthPolicy {
	return feedAuthPolicy{PerMinute: max(a.PerMinute, b.PerMinute), Startup: max(a.Startup, b.Startup), Reserve: max(a.Reserve, b.Reserve)}
}

func (e *feedAuthPolicyEvidence) held() feedAuthPolicy {
	p := e.Committed
	if e.Staged != nil {
		p = feedMaxPolicy(p, e.Staged.Policy)
	}
	return p
}

func (o *feedAuthOwner) identity() feedAuthIdentity {
	return feedAuthIdentity{Role: o.Role, ConfigPath: o.ConfigPath, Incarnation: o.Incarnation, PID: o.PID, StartedWall: o.StartedWall}
}

func (o *feedAuthOwner) applyIdentity(id feedAuthIdentity) {
	o.Role, o.ConfigPath, o.Incarnation, o.PID, o.StartedWall = id.Role, id.ConfigPath, id.Incarnation, id.PID, id.StartedWall
}

func (a *feedWeightAuthority) identity() feedAuthIdentity {
	return feedAuthIdentity{Role: a.role, ConfigPath: a.configPath, Incarnation: a.incarnation, PID: a.pid, StartedWall: feedWallString(a.startedWall)}
}

func feedValidOwnerKey(key string) bool {
	if len(key) != 2*feedAuthorityKeyBytes {
		return false
	}
	for _, c := range key {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func feedAuthorityPolicyDir(dir string) string { return filepath.Join(dir, feedAuthorityPoliciesDir) }

func feedAuthorityPolicyPath(dir, key string) string {
	return filepath.Join(feedAuthorityPolicyDir(dir), key+".json")
}

func feedValidPolicy(p feedAuthPolicy) bool {
	return feedWeightInRange(p.PerMinute, hlRESTWeightCeiling) && feedWeightInRange(p.Startup, hlRESTWeightCeiling) && feedWeightInRange(p.Reserve, hlRESTWeightCeiling)
}

func feedDecodePolicyEvidence(key string, raw []byte) *feedAuthPolicyEvidence {
	if len(raw) > feedAuthorityPolicyMaxBytes {
		return nil
	}
	var ev feedAuthPolicyEvidence
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ev); err != nil {
		return nil
	}
	if ev.Version != feedAuthorityPolicyVersion || ev.Key != key || !feedValidPolicy(ev.Committed) {
		return nil
	}
	if ev.Staged != nil && (ev.Staged.Tx == "" || !feedValidPolicy(ev.Staged.Policy)) {
		return nil
	}
	return &ev
}

func feedReadOnePolicyEvidence(dir, key string) (*feedAuthPolicyEvidence, bool, error) {
	f, err := os.Open(feedAuthorityPolicyPath(dir, key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, feedAuthorityPolicyMaxBytes+1))
	if err != nil {
		return nil, false, err
	}
	return feedDecodePolicyEvidence(key, raw), true, nil
}

type feedEvidenceSet map[string]*feedAuthPolicyEvidence

func feedReadPolicyEvidenceSet(dir string) (feedEvidenceSet, error) {
	entries, err := os.ReadDir(feedAuthorityPolicyDir(dir))
	if errors.Is(err, os.ErrNotExist) {
		return feedEvidenceSet{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := feedEvidenceSet{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.Contains(name, ".tmp-") {
			continue
		}
		key := strings.TrimSuffix(name, ".json")
		if !feedValidOwnerKey(key) {
			continue
		}
		if len(out) >= feedAuthorityPolicyMaxFiles {
			return nil, fmt.Errorf("%s holds more than %d owner policy files", feedAuthorityPolicyDir(dir), feedAuthorityPolicyMaxFiles)
		}
		ev, _, err := feedReadOnePolicyEvidence(dir, key)
		if err != nil {
			return nil, err
		}
		out[key] = ev
	}
	return out, nil
}

func feedWritePolicyEvidence(dir string, ev *feedAuthPolicyEvidence) error {
	ev.Version = feedAuthorityPolicyVersion
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return feedAuthorityWriteFileAtomic(feedAuthorityPolicyDir(dir), ev.Key+".json", data, false)
}

func feedHeldOwnerRow(id feedAuthIdentity, p feedAuthPolicy, tx string, stoppedNs int64) *feedAuthOwner {
	o := &feedAuthOwner{Policy: p, PolicyTx: tx, StoppedNs: stoppedNs, Bounds: map[string]*feedAuthBound{}, Permits: []*feedAuthPermit{}, Charges: []*feedAuthCharge{}}
	o.applyIdentity(id)
	return o
}

func feedUnprovenOwnerRow(id feedAuthIdentity, stoppedNs int64) *feedAuthOwner {
	o := feedHeldOwnerRow(id, feedAuthPolicy{}, "", stoppedNs)
	o.Unproven = true
	return o
}

func (rec *feedAuthRecord) unprovenOwners() []string {
	var out []string
	for _, k := range rec.sortedOwnerKeys() {
		if rec.Owners[k].Unproven {
			out = append(out, k)
		}
	}
	return out
}

type feedAdmission struct {
	Combined     int64
	Reserve      int
	ReserveOwner string
	Capacity     int64
	AllLive      bool
	Owners       []string
}

func feedAdmissionOf(rec *feedAuthRecord, key string, cand feedAuthPolicy, proveLive func(string, *feedAuthOwner) bool) feedAdmission {
	res := feedAdmission{Combined: cand.allocation(), Reserve: cand.Reserve, ReserveOwner: key, AllLive: true, Owners: []string{key}}
	for _, k := range rec.sortedOwnerKeys() {
		if k == key {
			continue
		}
		o := rec.Owners[k]
		if o.Policy.Reserve > res.Reserve {
			res.Reserve, res.ReserveOwner = o.Policy.Reserve, k
		}
		if !o.Active {
			continue
		}
		res.Combined += o.Policy.allocation()
		res.Owners = append(res.Owners, k)
		if proveLive != nil && !proveLive(k, o) {
			res.AllLive = false
		}
	}
	res.Capacity = int64(hlRESTWeightCeiling) - int64(res.Reserve)
	sort.Strings(res.Owners)
	return res
}

func feedSortedKeys(sets ...map[string]bool) []string {
	all := map[string]bool{}
	for _, s := range sets {
		for k := range s {
			all[k] = true
		}
	}
	out := make([]string, 0, len(all))
	for k := range all {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func feedKeySet[V any](m map[string]V) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

type feedRebuild struct {
	Rows     map[string]*feedAuthOwner
	Pending  []string
	Unknown  []string
	Unproven []string
	Evidence bool
	Legacy   bool
}

func feedRebuildRows(dir, skip string, readOnly bool, stoppedNs int64) (*feedRebuild, error) {
	set, err := feedReadPolicyEvidenceSet(dir)
	if err != nil {
		return nil, fmt.Errorf("owner policy evidence: %w", err)
	}
	out := &feedRebuild{Rows: map[string]*feedAuthOwner{}, Evidence: len(set) > 0}
	legacy := map[string]*feedAuthDeclaration{}
	if len(set) == 0 {
		if legacy, err = feedReadDeclarations(dir); err != nil {
			return nil, fmt.Errorf("owner declarations: %w", err)
		}
		out.Legacy = len(legacy) > 0
	}
	locks := map[string]bool{}
	for _, k := range feedDeclaredLockKeys(dir) {
		locks[k] = true
	}
	for _, key := range feedSortedKeys(feedKeySet(set), feedKeySet(legacy), locks) {
		if key == skip {
			continue
		}
		state, _ := feedProbeOwnerLock(dir, key, readOnly)
		var pol feedAuthPolicy
		var id feedAuthIdentity
		tx := ""
		known, corrupt := false, false
		if ev, ok := set[key]; ok {
			if ev == nil {
				corrupt = true
			} else {
				pol, id, tx, known = ev.held(), ev.feedAuthIdentity, ev.Tx, true
			}
		} else if d, ok := legacy[key]; ok {
			if d == nil {
				corrupt = true
			} else {
				pol, known = d.Policy, true
				id = feedAuthIdentity{Role: d.Role, ConfigPath: d.ConfigPath, Incarnation: d.Incarnation, PID: d.PID, StartedWall: d.StartedWall}
			}
		}
		if state == feedOwnerDead {
			switch {
			case corrupt:
				out.Rows[key] = feedUnprovenOwnerRow(id, stoppedNs)
				out.Unproven = append(out.Unproven, key)
			case known && pol.Reserve > 0:
				out.Rows[key] = feedHeldOwnerRow(id, pol, tx, stoppedNs)
			}
			continue
		}
		o := feedHeldOwnerRow(id, pol, tx, 0)
		o.Active, o.Reconstructed = true, true
		if !known {
			state = feedOwnerUnknown
			o.Policy = feedAuthPolicy{PerMinute: hlRESTWeightCeiling}
		}
		out.Rows[key] = o
		if state == feedOwnerLive {
			out.Pending = append(out.Pending, key)
		} else {
			out.Unknown = append(out.Unknown, key)
		}
	}
	if len(out.Rows) > feedAuthorityMaxOwners {
		return nil, fmt.Errorf("the recovery evidence names %d owners, above the %d cap; release stopped owners with go-trader feed-weight-release", len(out.Rows), feedAuthorityMaxOwners)
	}
	return out, nil
}

type feedMigration struct {
	Unproven []string
	Seeded   []string
	Raised   []string
}

func feedMigrateRows(rec *feedAuthRecord, dir string, readOnly bool, stoppedNs int64) (*feedMigration, error) {
	set, err := feedReadPolicyEvidenceSet(dir)
	if err != nil {
		return nil, fmt.Errorf("owner policy evidence: %w", err)
	}
	legacy, err := feedReadDeclarations(dir)
	if err != nil {
		return nil, fmt.Errorf("owner declarations: %w", err)
	}
	out := &feedMigration{}
	for _, key := range feedSortedKeys(feedKeySet(rec.Owners), feedKeySet(set), feedKeySet(legacy)) {
		row := rec.Owners[key]
		ev, hasEv := set[key]
		if row != nil {
			if hasEv && ev != nil {
				held := ev.held()
				if ev.Released && ev.Staged == nil {
					held = feedAuthPolicy{}
				}
				if held.Reserve > row.Policy.Reserve {
					row.Policy.Reserve = held.Reserve
					out.Raised = append(out.Raised, key)
				}
			}
			continue
		}
		if hasEv {
			if ev == nil {
				rec.Owners[key] = feedUnprovenOwnerRow(feedAuthIdentity{}, stoppedNs)
				out.Unproven = append(out.Unproven, key)
			} else if p := ev.held(); p.Reserve > 0 {
				rec.Owners[key] = feedHeldOwnerRow(ev.feedAuthIdentity, p, ev.Tx, stoppedNs)
				out.Seeded = append(out.Seeded, key)
			}
			continue
		}
		d := legacy[key]
		if d == nil {
			if state, _ := feedProbeOwnerLock(dir, key, readOnly); state != feedOwnerLive {
				rec.Owners[key] = feedUnprovenOwnerRow(feedAuthIdentity{}, stoppedNs)
				out.Unproven = append(out.Unproven, key)
			}
			continue
		}
		if d.Policy.Reserve > 0 {
			id := feedAuthIdentity{Role: d.Role, ConfigPath: d.ConfigPath, Incarnation: d.Incarnation, PID: d.PID, StartedWall: d.StartedWall}
			rec.Owners[key] = feedHeldOwnerRow(id, d.Policy, "", stoppedNs)
			out.Seeded = append(out.Seeded, key)
		}
	}
	if len(rec.Owners) > feedAuthorityMaxOwners {
		return nil, fmt.Errorf("the migrated record names %d owners, above the %d cap; release stopped owners with go-trader feed-weight-release", len(rec.Owners), feedAuthorityMaxOwners)
	}
	rec.Version = feedAuthorityVersion
	return out, nil
}

func feedSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (t *feedAuthTx) migrate(sum string) *feedWeightRefusal {
	m, err := feedMigrateRows(t.rec, t.a.dir, false, t.now.Ns)
	if err != nil {
		return feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("migrate %s from version %d: %v", t.a.recordPath(), feedAuthorityLegacyVersion, err))
	}
	if err := feedAuthorityHookAt("migrate:evidence"); err != nil {
		return feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("migrate %s: %v", t.a.recordPath(), err))
	}
	for _, key := range t.rec.sortedOwnerKeys() {
		o := t.rec.Owners[key]
		if o.Unproven {
			continue
		}
		if o.PolicyTx == "" {
			o.PolicyTx = "migrated-" + sum[:16]
		}
		ev := &feedAuthPolicyEvidence{Key: key, feedAuthIdentity: o.identity(), Committed: o.Policy, Tx: o.PolicyTx}
		if err := feedWritePolicyEvidence(t.a.dir, ev); err != nil {
			return feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("migrate %s: write owner policy evidence %s: %v", t.a.recordPath(), key, err))
		}
	}
	t.rec.MigratedFrom = sum
	t.dirty = true
	held := []string{}
	for _, key := range t.rec.sortedOwnerKeys() {
		if o := t.rec.Owners[key]; o.Policy.Reserve > 0 {
			held = append(held, fmt.Sprintf("%s:%d", key, o.Policy.Reserve))
		}
	}
	t.logLine("[feed-weight-shared] migrated %s from version %d (sha256 %s) to version %d: owners=%d held_reserves=%s seeded=%s raised=%s unproven=%s",
		t.a.recordPath(), feedAuthorityLegacyVersion, sum, feedAuthorityVersion, len(t.rec.Owners), joinFeedNames(held), joinFeedNames(m.Seeded), joinFeedNames(m.Raised), joinFeedNames(m.Unproven))
	return nil
}

func (t *feedAuthTx) stagePolicy(key string, id feedAuthIdentity, next feedAuthPolicy, release bool) (string, *feedWeightRefusal) {
	dir := t.a.dir
	row := t.rec.Owners[key]
	ev, exists, err := feedReadOnePolicyEvidence(dir, key)
	if err != nil {
		return "", feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("read owner policy evidence %s: %v", feedAuthorityPolicyPath(dir, key), err))
	}
	clean := exists && ev != nil && ev.Staged == nil && ev.Committed == next && ev.Released == release
	if clean && row != nil && row.Policy == next && !row.Unproven && row.PolicyTx != "" {
		return row.PolicyTx, nil
	}
	prev := feedAuthPolicy{}
	prevTx := ""
	if exists && ev != nil {
		prev, prevTx = ev.held(), ev.Tx
	}
	if row != nil {
		prev = feedMaxPolicy(prev, row.Policy)
	}
	tx := feedRandomID(8)
	stage := &feedAuthPolicyEvidence{Key: key, feedAuthIdentity: id, Committed: prev, Tx: prevTx, Staged: &feedAuthPolicyStage{Tx: tx, Policy: next, Release: release}}
	if err := feedAuthorityHookAt("policy:stage"); err != nil {
		return "", feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("stage owner policy evidence %s: %v", key, err))
	}
	if err := feedWritePolicyEvidence(dir, stage); err != nil {
		return "", feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("stage owner policy evidence %s: %v", key, err))
	}
	if err := feedAuthorityHookAt("policy:staged"); err != nil {
		return "", feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("stage owner policy evidence %s: %v", key, err))
	}
	if t.staged == nil {
		t.staged = map[string]bool{}
	}
	t.staged[key] = true
	final := &feedAuthPolicyEvidence{Key: key, feedAuthIdentity: id, Committed: next, Tx: tx, Released: release}
	t.afterWrite = append(t.afterWrite, func() error {
		if err := feedAuthorityHookAt("policy:finalize"); err != nil {
			return err
		}
		return feedWritePolicyEvidence(dir, final)
	})
	t.dirty = true
	return tx, nil
}

func (t *feedAuthTx) syncEvidence() {
	dir := t.a.dir
	set, err := feedReadPolicyEvidenceSet(dir)
	if err != nil {
		t.logLine("[feed-weight-shared] owner policy evidence cannot be read (%v); the record keeps every held reserve and a rebuild would hold sends until it can be read", err)
		return
	}
	for _, key := range feedSortedKeys(feedKeySet(t.rec.Owners), feedKeySet(set)) {
		row := t.rec.Owners[key]
		ev, has := set[key]
		if t.staged[key] || row != nil && row.Unproven {
			continue
		}
		if row == nil && has && ev != nil && ev.Committed.Reserve > 0 {
			if len(t.rec.Owners) >= feedAuthorityMaxOwners {
				t.logLine("[feed-weight-shared] owner=%s holds reserve %d in its committed policy evidence, but the record already holds %d owners; release stopped owners with go-trader feed-weight-release", key, ev.Committed.Reserve, feedAuthorityMaxOwners)
				continue
			}
			row = feedHeldOwnerRow(ev.feedAuthIdentity, ev.Committed, ev.Tx, t.now.Ns)
			t.rec.Owners[key] = row
			t.dirty = true
			t.logLine("[feed-weight-shared] owner=%s holds reserve %d in its committed policy evidence but not in the record; the record keeps it held", key, ev.Committed.Reserve)
		}
		if has && ev != nil && ev.Staged == nil {
			if row == nil || ev.Committed == row.Policy {
				continue
			}
			if ev.Committed.Reserve > row.Policy.Reserve {
				t.logLine("[feed-weight-shared] owner=%s record reserve %d is below its committed policy evidence %d; the record keeps %d", key, row.Policy.Reserve, ev.Committed.Reserve, ev.Committed.Reserve)
				row.Policy.Reserve = ev.Committed.Reserve
				t.dirty = true
				if ev.Committed == row.Policy {
					continue
				}
			}
		}
		var final *feedAuthPolicyEvidence
		switch {
		case row == nil && has && ev != nil && ev.Staged != nil:
			final = &feedAuthPolicyEvidence{Key: key, feedAuthIdentity: ev.feedAuthIdentity, Released: ev.Staged.Release}
		case row == nil:
			continue
		case !has && row.Policy.Reserve == 0:
			continue
		default:
			released := row.Policy.Reserve == 0 && !row.Active && has && ev != nil && (ev.Released || ev.Staged != nil && ev.Staged.Release && ev.Staged.Tx == row.PolicyTx)
			tx := row.PolicyTx
			if tx == "" {
				tx = feedRandomID(8)
				row.PolicyTx = tx
			}
			final = &feedAuthPolicyEvidence{Key: key, feedAuthIdentity: row.identity(), Committed: row.Policy, Tx: tx, Released: released}
		}
		t.dirty = true
		f := final
		t.afterWrite = append(t.afterWrite, func() error { return feedWritePolicyEvidence(dir, f) })
	}
}

func (t *feedAuthTx) noteReserveChange(before int) {
	after := t.rec.commonReserve()
	if after == before {
		return
	}
	capacity := t.rec.capacity()
	var over []string
	if after > before {
		for _, h := range t.rec.sortedHosts() {
			if used := t.rec.usedOnHost(h); used > capacity {
				over = append(over, fmt.Sprintf("%s:used=%d:shortfall=%d", h, used, used-capacity))
			}
		}
	}
	line := fmt.Sprintf("[feed-weight-shared] owner=%s common reserve %d -> %d capacity=%d hosts_over_capacity=%s", t.a.key, before, after, capacity, joinFeedNames(over))
	t.onCommit = append(t.onCommit, func() { t.logs = append(t.logs, line) })
}

func (rec *feedAuthRecord) sortedHosts() []string {
	hosts := map[string]bool{}
	for _, o := range rec.Owners {
		for _, p := range o.Permits {
			hosts[p.Host] = true
		}
		for _, c := range o.Charges {
			hosts[c.Host] = true
		}
	}
	return feedSortedKeys(hosts)
}
