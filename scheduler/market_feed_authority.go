package main

import (
	"bytes"
	"context"
	"crypto/rand"
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
	"sync"
	"syscall"
	"time"
)

const (
	feedAuthorityCapability   = "go-trader-capability:feed-weight-authority-v1"
	feedAuthorityVersion      = 2
	feedAuthorityHostDir      = "/var/lib/go-trader/shared/feed-weight"
	feedAuthorityRecordName   = "authority.json"
	feedAuthorityLockName     = "authority.lock"
	feedAuthorityOwnersDir    = "owners"
	feedAuthorityTempPrefix   = "authority.json.tmp-"
	feedAuthorityLockWait     = 2 * time.Second
	feedAuthorityLockPoll     = 10 * time.Millisecond
	feedAuthorityHeartbeat    = 5 * time.Second
	feedAuthorityFaultRetry   = time.Second
	feedAuthorityStopBound    = 3 * time.Second
	feedAuthorityMaxBytes     = 8 << 20
	feedAuthorityMaxOwners    = 256
	feedAuthorityMaxEntries   = 100000
	feedAuthorityMaxDebt      = 1 << 31
	feedAuthorityOutageAfter  = 3
	feedAuthorityKeyBytes     = 16
	feedAuthorityRoleFeed     = "feed"
	feedAuthorityRoleEmbedded = "scheduler"

	feedWeightCauseSharedExhausted = "shared_exhausted"
	feedWeightCauseSharedHold      = "shared_recovery_hold"
	feedWeightCauseSharedStorage   = "shared_storage"
	feedWeightCauseSharedClock     = "shared_clock"
	feedWeightCauseSharedVersion   = "shared_version"
	feedWeightCauseSharedOwner     = "shared_owner"

	feedSharedCoverageShared   = "shared"
	feedSharedCoverageNoPolicy = "excluded: no weight policy"
	feedSharedCoverageLotLocal = "excluded: local lot-metadata ledger"

	feedSharedCoverageKillSwitchLocal = "excluded: local kill-switch position ledger"

	feedAuthorityRecoveryInitial    = "initial_install"
	feedAuthorityRecoveryMissing    = "record_missing"
	feedAuthorityRecoveryCorrupt    = "record_corrupt"
	feedAuthorityRecoveryBoot       = "boot_changed"
	feedAuthorityRecoveryRegressed  = "clock_regressed"
	feedAuthorityRecoveryClockFault = "clock_unreadable"

	feedPermitStatePending = "pending"
	feedPermitStateSending = "sending"
)

var feedAuthorityDirOverride string

var feedAuthorityHook func(point string) error

var newFeedBootClock = func() feedBootClock { return feedSystemBootClock{} }

func feedSharedCause(cause string) bool {
	switch cause {
	case feedWeightCauseSharedExhausted, feedWeightCauseSharedHold, feedWeightCauseSharedStorage,
		feedWeightCauseSharedClock, feedWeightCauseSharedVersion, feedWeightCauseSharedOwner:
		return true
	}
	return false
}

func feedSharedHeldErr(err error) bool {
	cause := feedWeightRefusalCause(err)
	return feedSharedCause(cause) && cause != feedWeightCauseSharedOwner
}

func feedAuthorityDir() string {
	if feedAuthorityDirOverride != "" {
		return feedAuthorityDirOverride
	}
	if inContainerRuntime() {
		return containerFeedWeightDir
	}
	return feedAuthorityHostDir
}

func feedAuthorityHookAt(point string) error {
	if h := feedAuthorityHook; h != nil {
		return h(point)
	}
	return nil
}

type feedBootClock interface {
	BootID() (string, error)
	Elapsed() (int64, error)
	Wall() time.Time
}

type feedSystemBootClock struct{}

func (feedSystemBootClock) BootID() (string, error) { return readFeedBootIdentity() }
func (feedSystemBootClock) Elapsed() (int64, error) { return readFeedBootElapsed() }
func (feedSystemBootClock) Wall() time.Time         { return time.Now().UTC() }

type feedBootReading struct {
	Boot string
	Ns   int64
	Wall time.Time
}

func readFeedBoot(c feedBootClock) (feedBootReading, error) {
	id, err := c.BootID()
	if err != nil {
		return feedBootReading{}, err
	}
	ns, err := c.Elapsed()
	if err != nil {
		return feedBootReading{}, err
	}
	if ns < 0 {
		return feedBootReading{}, fmt.Errorf("boot clock returned a negative elapsed time %d", ns)
	}
	return feedBootReading{Boot: id, Ns: ns, Wall: c.Wall().UTC()}, nil
}

type feedAnchoredClock struct {
	mu         sync.Mutex
	src        feedBootClock
	anchorWall time.Time
	anchorNs   int64
	anchored   bool
	mono       time.Time
	last       time.Time
}

func newFeedAnchoredClock(src feedBootClock) *feedAnchoredClock {
	c := &feedAnchoredClock{src: src, mono: time.Now(), anchorWall: src.Wall().UTC()}
	if ns, err := src.Elapsed(); err == nil && ns >= 0 {
		c.anchorNs, c.anchored = ns, true
	}
	c.last = c.anchorWall
	return c
}

func (c *feedAnchoredClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	var t time.Time
	if ns, err := c.src.Elapsed(); err == nil && c.anchored && ns >= c.anchorNs {
		t = c.anchorWall.Add(time.Duration(ns - c.anchorNs))
	} else {
		t = c.anchorWall.Add(time.Since(c.mono))
	}
	if t.Before(c.last) {
		t = c.last
	}
	c.last = t
	return t
}

type feedAuthPolicy struct {
	PerMinute int `json:"weight_per_minute"`
	Startup   int `json:"weight_startup"`
	Reserve   int `json:"weight_reserve"`
}

func (p feedAuthPolicy) allocation() int64 { return int64(p.PerMinute) + int64(p.Startup) }

type feedAuthPermit struct {
	ID          string `json:"id"`
	Host        string `json:"host"`
	ReqType     string `json:"req_type"`
	Reason      string `json:"reason"`
	Input       string `json:"input"`
	Cost        int    `json:"cost"`
	StartupPart int    `json:"startup_part"`
	State       string `json:"state"`
	Incarnation string `json:"incarnation"`
	EpochID     string `json:"epoch_id"`
	CreatedNs   int64  `json:"created_boot_ns"`
}

type feedAuthCharge struct {
	ID      string `json:"id"`
	Host    string `json:"host"`
	Weight  int    `json:"weight"`
	Startup int    `json:"startup"`
	Debt    bool   `json:"debt"`
	AtNs    int64  `json:"at_boot_ns"`
	AtWall  string `json:"at_wall"`
}

type feedAuthBound struct {
	AtCap    bool   `json:"at_cap"`
	Blocked  bool   `json:"blocked"`
	Bound    int    `json:"bound"`
	Observed int    `json:"observed"`
	AtNs     int64  `json:"at_boot_ns"`
	AtWall   string `json:"at_wall"`
}

type feedAuthOwner struct {
	Role          string                    `json:"role"`
	ConfigPath    string                    `json:"config_path"`
	Incarnation   string                    `json:"incarnation"`
	PID           int                       `json:"pid"`
	StartedWall   string                    `json:"started_wall"`
	Active        bool                      `json:"active"`
	EpochID       string                    `json:"epoch_id"`
	Reconstructed bool                      `json:"reconstructed,omitempty"`
	Policy        feedAuthPolicy            `json:"policy"`
	StartupLeft   int                       `json:"startup_left"`
	StoppedNs     int64                     `json:"stopped_boot_ns,omitempty"`
	PolicyTx      string                    `json:"policy_tx,omitempty"`
	Unproven      bool                      `json:"policy_unproven,omitempty"`
	Bounds        map[string]*feedAuthBound `json:"bounds"`
	Permits       []*feedAuthPermit         `json:"permits"`
	Charges       []*feedAuthCharge         `json:"charges"`
}

type feedAuthHost struct {
	PauseUntilNs   int64  `json:"pause_until_boot_ns"`
	PauseWall      string `json:"pause_wall"`
	LastRetryAfter string `json:"last_retry_after,omitempty"`
}

type feedAuthRecovery struct {
	EpochID       string           `json:"epoch_id"`
	Generation    int64            `json:"generation"`
	Reason        string           `json:"reason"`
	Started       string           `json:"started"`
	StartedNs     int64            `json:"started_boot_ns"`
	HoldUntilNs   int64            `json:"hold_until_boot_ns"`
	PendingOwners []string         `json:"pending_owners"`
	UnknownOwners []string         `json:"unknown_owners"`
	DeadAtNs      map[string]int64 `json:"dead_at_boot_ns,omitempty"`
}

type feedAuthRecord struct {
	Version      int                       `json:"version"`
	EpochID      string                    `json:"epoch_id"`
	Generation   int64                     `json:"generation"`
	Seq          int64                     `json:"seq"`
	BootID       string                    `json:"boot_id"`
	LastBootNs   int64                     `json:"last_boot_ns"`
	WrittenWall  string                    `json:"written_wall"`
	Recovery     *feedAuthRecovery         `json:"recovery,omitempty"`
	LastRecovery *feedAuthRecovery         `json:"last_recovery,omitempty"`
	Hosts        map[string]*feedAuthHost  `json:"hosts"`
	Owners       map[string]*feedAuthOwner `json:"owners"`
	MigratedFrom string                    `json:"migrated_from_sha256,omitempty"`
}

type feedAuthDeclaration struct {
	Role        string         `json:"role"`
	ConfigPath  string         `json:"config_path"`
	Incarnation string         `json:"incarnation"`
	PID         int            `json:"pid"`
	StartedWall string         `json:"started_wall"`
	Policy      feedAuthPolicy `json:"policy"`
}

func (rec *feedAuthRecord) commonReserve() int {
	r := 0
	for _, o := range rec.Owners {
		if o.Policy.Reserve > r {
			r = o.Policy.Reserve
		}
	}
	return r
}

func (rec *feedAuthRecord) combinedAllocation() int64 {
	var sum int64
	for _, o := range rec.Owners {
		if o.Active {
			sum += o.Policy.allocation()
		}
	}
	return sum
}

func (rec *feedAuthRecord) capacity() int64 {
	return int64(hlRESTWeightCeiling) - int64(rec.commonReserve())
}

func (rec *feedAuthRecord) usedOnHost(host string) int64 {
	var used int64
	for _, o := range rec.Owners {
		for _, p := range o.Permits {
			if p.Host == host {
				used += int64(p.Cost)
			}
		}
		for _, c := range o.Charges {
			if c.Host == host {
				used += int64(c.Weight)
			}
		}
	}
	return used
}

func (rec *feedAuthRecord) sortedOwnerKeys() []string {
	keys := make([]string, 0, len(rec.Owners))
	for k := range rec.Owners {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func newFeedAuthRecord() *feedAuthRecord {
	return &feedAuthRecord{Version: feedAuthorityVersion, Hosts: map[string]*feedAuthHost{}, Owners: map[string]*feedAuthOwner{}}
}

func feedRandomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func feedAuthorityOwnerKey(role, configPath string) string {
	sum := sha256.Sum256([]byte(role + "\x00" + resolvedConfigPath(configPath)))
	return hex.EncodeToString(sum[:feedAuthorityKeyBytes])
}

func feedAuthPolicyOf(p *feedWeightPolicy) feedAuthPolicy {
	if p == nil {
		return feedAuthPolicy{}
	}
	return feedAuthPolicy{PerMinute: p.PerMinute, Startup: p.Startup, Reserve: p.Reserve}
}

func feedWallString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func feedSharedRefusal(cause, detail string) *feedWeightRefusal {
	return &feedWeightRefusal{Cause: cause, Detail: detail}
}

type feedAuthorityUnsafeError struct {
	Combined     int64
	Capacity     int64
	Reserve      int
	ReserveOwner string
	AllLive      bool
	Owners       []string
}

func (e *feedAuthorityUnsafeError) Error() string {
	proof := "every counted owner is proved live"
	if !e.AllLive {
		proof = "some counted owners have unknown liveness, so the owner waits for their state"
	}
	return fmt.Sprintf("combined weight allocation %d of active owners %s exceeds the %d venue weight ceiling less the common reserve %d held by owner %s (at most %d); %s",
		e.Combined, strings.Join(e.Owners, ","), hlRESTWeightCeiling, e.Reserve, e.ReserveOwner, e.Capacity, proof)
}

type feedAuthorityDuplicateError struct {
	Key  string
	Path string
}

func (e *feedAuthorityDuplicateError) Error() string {
	return fmt.Sprintf("another process already holds the shared weight lifetime lock %s for owner %s; a second active incarnation of the same owner is refused", e.Path, e.Key)
}

type feedFileID struct {
	dev uint64
	ino uint64
}

func feedFileIDOf(fi os.FileInfo) (feedFileID, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return feedFileID{}, false
	}
	return feedFileID{dev: uint64(st.Dev), ino: uint64(st.Ino)}, true
}

type feedPendingPause struct {
	UntilNs    int64
	Wall       time.Time
	RetryAfter string
}

type feedSharedHostStatus struct {
	Host        string `json:"host"`
	Used        int64  `json:"used"`
	Shortfall   int64  `json:"shortfall"`
	Pause       string `json:"pause"`
	PausedUntil string `json:"paused_until,omitempty"`
}

type feedSharedRecoveryStatus struct {
	Generation    int64    `json:"generation"`
	Reason        string   `json:"reason"`
	Started       string   `json:"started"`
	HoldUntil     string   `json:"hold_until"`
	PendingOwners []string `json:"pending_owners"`
	UnknownOwners []string `json:"unknown_owners"`
}

type feedSharedOwnerStatus struct {
	Key        string `json:"key"`
	Role       string `json:"role"`
	ConfigPath string `json:"config_path"`
	Active     bool   `json:"active"`
	Allocation int64  `json:"allocation"`
	Reserve    int    `json:"reserve"`
	Unproven   bool   `json:"unproven,omitempty"`
	Pending    int64  `json:"pending"`
	InFlight   int64  `json:"in_flight"`
	Charged    int64  `json:"charged"`
	Debt       int64  `json:"debt"`
}

type feedSharedStatus struct {
	Coverage           string                    `json:"coverage"`
	Registry           string                    `json:"registry,omitempty"`
	Directory          string                    `json:"directory,omitempty"`
	OwnerKey           string                    `json:"owner_key,omitempty"`
	Role               string                    `json:"role,omitempty"`
	ConfigPath         string                    `json:"config_path,omitempty"`
	Incarnation        string                    `json:"incarnation,omitempty"`
	Allocation         int64                     `json:"allocation"`
	CommonReserve      int                       `json:"common_reserve"`
	CombinedAllocation int64                     `json:"combined_allocation"`
	Capacity           int64                     `json:"capacity"`
	Pending            int64                     `json:"pending"`
	InFlight           int64                     `json:"in_flight"`
	Charged            int64                     `json:"charged"`
	Debt               int64                     `json:"debt"`
	StoppedOwnerDebt   int64                     `json:"stopped_owner_debt"`
	Hosts              []feedSharedHostStatus    `json:"hosts"`
	Recovery           *feedSharedRecoveryStatus `json:"recovery,omitempty"`
	LastRecovery       *feedSharedRecoveryStatus `json:"last_recovery,omitempty"`
	UnprovenOwners     []string                  `json:"unproven_owners,omitempty"`
	LastRefusal        string                    `json:"last_refusal,omitempty"`
	ViewAt             string                    `json:"view_at,omitempty"`
	Owners             []feedSharedOwnerStatus   `json:"owners"`
}

type feedSharedView struct {
	status        feedSharedStatus
	shortfall     map[string]int64
	holdUntil     time.Time
	nextExpiry    time.Time
	recovery      *feedAuthRecovery
	lastRecovery  *feedAuthRecovery
	maxGeneration int64
}

type feedWeightAuthority struct {
	dir         string
	clock       feedBootClock
	key         string
	role        string
	configPath  string
	incarnation string
	pid         int
	startedWall time.Time
	logf        func(string, ...any)
	gate        chan struct{}

	policy            feedAuthPolicy
	registered        bool
	registerErr       error
	seeded            bool
	lifetime          *os.File
	lifetimeID        feedFileID
	ledger            *feedRequestLedger
	clockFaulted      bool
	faultUntil        time.Time
	faultErr          *feedWeightRefusal
	consecutiveFaults int
	outageOpen        bool
	stopped           bool

	stateMu      sync.Mutex
	durableOpen  map[string]*feedRequestPermit
	pendingPause map[string]feedPendingPause
	latched      string
	latchDetail  string

	viewMu sync.Mutex
	view   feedSharedView

	reportMu       sync.Mutex
	reportedStart  string
	reportedEnd    string
	outageReported bool
	outageCleared  bool
	outageDetail   string
	outageCause    string
	latchReported  bool
	unprovenSeen   string
}

func newFeedWeightAuthority(role, configPath string, logf func(string, ...any)) *feedWeightAuthority {
	if logf == nil {
		logf = func(format string, a ...any) { fmt.Printf(format+"\n", a...) }
	}
	resolved := resolvedConfigPath(configPath)
	a := &feedWeightAuthority{
		dir:          feedAuthorityDir(),
		clock:        newFeedBootClock(),
		key:          feedAuthorityOwnerKey(role, configPath),
		role:         role,
		configPath:   resolved,
		incarnation:  feedRandomID(8),
		pid:          os.Getpid(),
		startedWall:  time.Now().UTC(),
		logf:         logf,
		gate:         make(chan struct{}, 1),
		durableOpen:  map[string]*feedRequestPermit{},
		pendingPause: map[string]feedPendingPause{},
	}
	a.view.status = feedSharedStatus{Coverage: feedSharedCoverageShared, Registry: "unregistered", Directory: a.dir, OwnerKey: a.key,
		Role: role, ConfigPath: resolved, Incarnation: a.incarnation, Hosts: []feedSharedHostStatus{}, Owners: []feedSharedOwnerStatus{}}
	return a
}

func (a *feedWeightAuthority) openSnapshot() map[string]*feedRequestPermit {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	out := make(map[string]*feedRequestPermit, len(a.durableOpen))
	for k, v := range a.durableOpen {
		out[k] = v
	}
	return out
}

func (a *feedWeightAuthority) forget(id string) {
	a.stateMu.Lock()
	delete(a.durableOpen, id)
	a.stateMu.Unlock()
}

func (a *feedWeightAuthority) remember(p *feedRequestPermit) {
	a.stateMu.Lock()
	a.durableOpen[p.id] = p
	a.stateMu.Unlock()
}

func (a *feedWeightAuthority) latch(cause, detail string) bool {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.latched != "" {
		return false
	}
	a.latched = cause
	a.latchDetail = detail
	return true
}

func (a *feedWeightAuthority) latchState() (cause, detail string) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	return a.latched, a.latchDetail
}

func (a *feedWeightAuthority) latchedCause() string {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	return a.latched
}

func (a *feedWeightAuthority) pendingPauses() map[string]feedPendingPause {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	out := make(map[string]feedPendingPause, len(a.pendingPause))
	for k, v := range a.pendingPause {
		out[k] = v
	}
	return out
}

func (a *feedWeightAuthority) lockPath() string { return filepath.Join(a.dir, feedAuthorityLockName) }
func (a *feedWeightAuthority) recordPath() string {
	return filepath.Join(a.dir, feedAuthorityRecordName)
}
func feedAuthorityOwnerLockPath(dir, key string) string {
	return filepath.Join(dir, feedAuthorityOwnersDir, key+".lock")
}
func feedAuthorityOwnerDeclPath(dir, key string) string {
	return filepath.Join(dir, feedAuthorityOwnersDir, key+".json")
}

func feedAuthorityTrustIssue(path string) string {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Sprintf("%s: %v", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Sprintf("%s is a symlink; the shared weight directory must be a real directory", path)
	}
	if !fi.IsDir() {
		return fmt.Sprintf("%s is not a directory", path)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Sprintf("%s has mode %o; group and other must not write the shared weight directory", path, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Sprintf("%s is owned by uid %d, not the process user %d; participants under two accounts cannot share one authority", path, st.Uid, os.Geteuid())
	}
	return ""
}

func (a *feedWeightAuthority) ensureDir() *feedWeightRefusal {
	if err := feedAuthorityHookAt("dir"); err != nil {
		return feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("shared weight directory %s: %v", a.dir, err))
	}
	for _, d := range []string{a.dir, filepath.Join(a.dir, feedAuthorityOwnersDir), feedAuthorityPolicyDir(a.dir)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("shared weight directory %s: %v", d, err))
		}
		if issue := feedAuthorityTrustIssue(d); issue != "" {
			return feedSharedRefusal(feedWeightCauseSharedStorage, issue)
		}
	}
	return nil
}

type feedAuthTx struct {
	op         string
	a          *feedWeightAuthority
	rec        *feedAuthRecord
	now        feedBootReading
	dirty      bool
	afterWrite []func() error
	staged     map[string]bool
	onCommit   []func()
	onFinally  []func()
	logs       []string
}

func (t *feedAuthTx) owner() *feedAuthOwner { return t.rec.Owners[t.a.key] }

func (t *feedAuthTx) logLine(format string, args ...any) {
	t.logs = append(t.logs, fmt.Sprintf(format, args...))
}

func (a *feedWeightAuthority) acquireGate(ctx context.Context) *feedWeightRefusal {
	timer := time.NewTimer(feedAuthorityLockWait)
	defer timer.Stop()
	select {
	case a.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("waiting for the shared weight transaction gate: %v", ctx.Err()))
	case <-timer.C:
		return feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("the shared weight transaction gate stayed busy for %s", feedAuthorityLockWait))
	}
}

func (a *feedWeightAuthority) releaseGate() { <-a.gate }

func feedFlockPoll(ctx context.Context, f *os.File, how int, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(feedAuthorityLockPoll):
		}
	}
}

func (a *feedWeightAuthority) openAuthorityLock(ctx context.Context) (*os.File, *feedWeightRefusal) {
	if err := feedAuthorityHookAt("lock"); err != nil {
		return nil, feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("shared weight lock %s: %v", a.lockPath(), err))
	}
	f, err := os.OpenFile(a.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("open shared weight lock %s: %v", a.lockPath(), err))
	}
	if err := feedFlockPoll(ctx, f, syscall.LOCK_EX, feedAuthorityLockWait); err != nil {
		f.Close()
		return nil, feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("shared weight lock %s was not acquired within %s: %v", a.lockPath(), feedAuthorityLockWait, err))
	}
	if err := feedAuthorityHookAt("lock:held"); err != nil {
		f.Close()
		return nil, feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("shared weight lock %s: %v", a.lockPath(), err))
	}
	held, herr := f.Stat()
	cur, cerr := os.Lstat(a.lockPath())
	if herr != nil || cerr != nil {
		f.Close()
		return nil, feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("shared weight lock %s cannot be checked: %v %v", a.lockPath(), herr, cerr))
	}
	hid, ok1 := feedFileIDOf(held)
	cid, ok2 := feedFileIDOf(cur)
	if !ok1 || !ok2 || hid != cid {
		f.Close()
		return nil, feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("shared weight lock %s was replaced while held; lock files are never replaced", a.lockPath()))
	}
	return f, nil
}

type feedAuthLoad int

const (
	feedAuthLoadOK feedAuthLoad = iota
	feedAuthLoadAbsent
	feedAuthLoadCorrupt
	feedAuthLoadLegacy
)

func loadFeedAuthRecord(path string, readOnly bool) (*feedAuthRecord, feedAuthLoad, string, *feedWeightRefusal) {
	if !readOnly {
		if err := feedAuthorityHookAt("read"); err != nil {
			return nil, 0, "", feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("read %s: %v", path, err))
		}
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, feedAuthLoadAbsent, "absent", nil
	}
	if err != nil {
		return nil, 0, "", feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("read %s: %v", path, err))
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, feedAuthorityMaxBytes+1))
	if err != nil {
		return nil, 0, "", feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("read %s: %v", path, err))
	}
	if len(raw) > feedAuthorityMaxBytes {
		return nil, feedAuthLoadCorrupt, fmt.Sprintf("larger than %d bytes", feedAuthorityMaxBytes), nil
	}
	var head struct {
		Version json.RawMessage `json:"version"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, feedAuthLoadCorrupt, fmt.Sprintf("not JSON: %v", err), nil
	}
	var version int
	if err := json.Unmarshal(head.Version, &version); err != nil {
		return nil, feedAuthLoadCorrupt, "version is not an integer", nil
	}
	if version != feedAuthorityVersion && version != feedAuthorityLegacyVersion {
		return nil, 0, "", feedSharedRefusal(feedWeightCauseSharedVersion, fmt.Sprintf("%s has version %d; this binary reads versions %d and %d and never overwrites another version (run a binary that knows version %d on every participant; to go back to this binary, stop every participant, move %s aside and start them: the held reserves in %s stay, and one recovery hold follows)", path, version, feedAuthorityLegacyVersion, feedAuthorityVersion, version, path, feedAuthorityPolicyDir(filepath.Dir(path))))
	}
	rec := newFeedAuthRecord()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(rec); err != nil {
		return nil, feedAuthLoadCorrupt, fmt.Sprintf("does not decode: %v", err), nil
	}
	if issue := validateFeedAuthRecord(rec); issue != "" {
		return nil, feedAuthLoadCorrupt, issue, nil
	}
	if rec.Version == feedAuthorityLegacyVersion {
		return rec, feedAuthLoadLegacy, feedSHA256(raw), nil
	}
	return rec, feedAuthLoadOK, "", nil
}

func feedWeightInRange(v, hi int) bool { return v >= 0 && v <= hi }

func validateFeedAuthRecord(rec *feedAuthRecord) string {
	if rec.EpochID == "" || rec.BootID == "" || rec.LastBootNs < 0 {
		return "missing epoch, boot identity or boot time"
	}
	if rec.Hosts == nil {
		rec.Hosts = map[string]*feedAuthHost{}
	}
	if rec.Owners == nil {
		rec.Owners = map[string]*feedAuthOwner{}
	}
	if len(rec.Owners) > feedAuthorityMaxOwners {
		return fmt.Sprintf("%d owners, above the %d cap", len(rec.Owners), feedAuthorityMaxOwners)
	}
	for host, h := range rec.Hosts {
		if h == nil || h.PauseUntilNs < 0 {
			return fmt.Sprintf("host %s has an invalid pause", host)
		}
	}
	for key, o := range rec.Owners {
		if o == nil || key == "" {
			return "an owner entry is empty"
		}
		p := o.Policy
		if !feedWeightInRange(p.PerMinute, hlRESTWeightCeiling) || !feedWeightInRange(p.Startup, hlRESTWeightCeiling) || !feedWeightInRange(p.Reserve, hlRESTWeightCeiling) {
			return fmt.Sprintf("owner %s policy is outside 0..%d", key, hlRESTWeightCeiling)
		}
		if !feedWeightInRange(o.StartupLeft, hlRESTWeightCeiling) {
			return fmt.Sprintf("owner %s startup_left is outside 0..%d", key, hlRESTWeightCeiling)
		}
		if len(o.Permits)+len(o.Charges) > feedAuthorityMaxEntries {
			return fmt.Sprintf("owner %s has more than %d permits and charges", key, feedAuthorityMaxEntries)
		}
		if o.Bounds == nil {
			o.Bounds = map[string]*feedAuthBound{}
		}
		for t, b := range o.Bounds {
			if b == nil || b.AtNs < 0 {
				return fmt.Sprintf("owner %s bound %s is invalid", key, t)
			}
		}
		if o.Unproven && (o.Active || o.Policy != feedAuthPolicy{}) {
			return fmt.Sprintf("owner %s is marked unproven but is active or holds a policy", key)
		}
		for _, pm := range o.Permits {
			if pm == nil || pm.ID == "" || !feedWeightInRange(pm.Cost, hlRESTWeightCeiling) || !feedWeightInRange(pm.StartupPart, pm.Cost) ||
				(pm.State != feedPermitStatePending && pm.State != feedPermitStateSending) || pm.CreatedNs < 0 {
				return fmt.Sprintf("owner %s has an invalid permit", key)
			}
		}
		for _, c := range o.Charges {
			if c == nil || !feedWeightInRange(c.Weight, feedAuthorityMaxDebt) || !feedWeightInRange(c.Startup, c.Weight) || c.AtNs < 0 {
				return fmt.Sprintf("owner %s has an invalid charge", key)
			}
		}
	}
	return ""
}

func feedAuthorityWriteFileAtomic(dir, name string, data []byte, hooks bool) error {
	hook := func(point string) error {
		if !hooks {
			return nil
		}
		return feedAuthorityHookAt(point)
	}
	if err := hook("write"); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, name+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := hook("sync"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := hook("rename"); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		return err
	}
	cleanup = false
	if err := hook("dirsync"); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (a *feedWeightAuthority) cleanStaleTemps() {
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), feedAuthorityTempPrefix) {
			os.Remove(filepath.Join(a.dir, e.Name()))
		}
	}
	for _, sub := range []string{feedAuthorityOwnersDir, feedAuthorityPoliciesDir} {
		entries, err := os.ReadDir(filepath.Join(a.dir, sub))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), ".json.tmp-") {
				os.Remove(filepath.Join(a.dir, sub, e.Name()))
			}
		}
	}
}

func (a *feedWeightAuthority) writeRecord(t *feedAuthTx) error {
	t.rec.Seq++
	t.rec.BootID = t.now.Boot
	t.rec.LastBootNs = t.now.Ns
	t.rec.WrittenWall = feedWallString(t.now.Wall)
	data, err := json.Marshal(t.rec)
	if err != nil {
		return err
	}
	return feedAuthorityWriteFileAtomic(a.dir, feedAuthorityRecordName, data, true)
}

type feedLockProbe int

const (
	feedOwnerLive feedLockProbe = iota
	feedOwnerDead
	feedOwnerUnknown
)

func feedProbeOwnerLock(dir, key string, readOnly bool) (feedLockProbe, string) {
	path := feedAuthorityOwnerLockPath(dir, key)
	flags, how := os.O_RDWR, syscall.LOCK_EX
	if readOnly {
		flags, how = os.O_RDONLY, syscall.LOCK_SH
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return feedOwnerUnknown, fmt.Sprintf("lifetime lock %s: %v", path, err)
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
	if err == nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return feedOwnerDead, ""
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return feedOwnerLive, ""
	}
	return feedOwnerUnknown, fmt.Sprintf("lifetime lock %s: %v", path, err)
}

func feedReadDeclarations(dir string) (map[string]*feedAuthDeclaration, error) {
	entries, err := os.ReadDir(filepath.Join(dir, feedAuthorityOwnersDir))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]*feedAuthDeclaration{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]*feedAuthDeclaration{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.Contains(name, ".tmp-") {
			continue
		}
		key := strings.TrimSuffix(name, ".json")
		if !feedValidOwnerKey(key) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, feedAuthorityOwnersDir, name))
		if err != nil {
			return nil, err
		}
		var d feedAuthDeclaration
		if err := json.Unmarshal(raw, &d); err != nil {
			out[key] = nil
			continue
		}
		out[key] = &d
	}
	return out, nil
}

func feedDeclaredLockKeys(dir string) []string {
	entries, err := os.ReadDir(filepath.Join(dir, feedAuthorityOwnersDir))
	if err != nil {
		return nil
	}
	var keys []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".lock") {
			key := strings.TrimSuffix(e.Name(), ".lock")
			if feedValidOwnerKey(key) {
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

func feedAddUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	out := append(list, v)
	sort.Strings(out)
	return out
}

func feedRemoveString(list []string, v string) []string {
	out := list[:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func (t *feedAuthTx) chargeFromPermit(p *feedAuthPermit) *feedAuthCharge {
	return &feedAuthCharge{ID: p.ID, Host: p.Host, Weight: p.Cost, Startup: p.StartupPart, AtNs: t.now.Ns, AtWall: feedWallString(t.now.Wall)}
}

func (t *feedAuthTx) convertPermits(o *feedAuthOwner, keep func(*feedAuthPermit) bool) int {
	kept := o.Permits[:0]
	converted := 0
	for _, p := range o.Permits {
		if keep != nil && keep(p) {
			kept = append(kept, p)
			continue
		}
		if p.Cost > 0 {
			o.Charges = append(o.Charges, t.chargeFromPermit(p))
		}
		converted++
	}
	o.Permits = kept
	if converted > 0 {
		t.dirty = true
	}
	return converted
}

func (t *feedAuthTx) ownerDied(key string, o *feedAuthOwner, why string) {
	n := t.convertPermits(o, nil)
	o.Active = false
	o.StoppedNs = t.now.Ns
	t.dirty = true
	if r := t.rec.Recovery; r != nil {
		if r.DeadAtNs == nil {
			r.DeadAtNs = map[string]int64{}
		}
		if _, ok := r.DeadAtNs[key]; !ok {
			r.DeadAtNs[key] = t.now.Ns
		}
		r.UnknownOwners = feedRemoveString(r.UnknownOwners, key)
	}
	t.logLine("[feed-weight-shared] owner=%s role=%s stopped (%s): allocation removed, reserve %d held, %d unresolved permit(s) kept as full charges, unexpired debt kept", key, o.Role, why, o.Policy.Reserve, n)
}

func (t *feedAuthTx) sweep() {
	for _, key := range t.rec.sortedOwnerKeys() {
		if key == t.a.key {
			continue
		}
		o := t.rec.Owners[key]
		if !o.Active {
			continue
		}
		state, detail := feedProbeOwnerLock(t.a.dir, key, false)
		switch state {
		case feedOwnerDead:
			t.ownerDied(key, o, "lifetime lock free")
		case feedOwnerLive:
			if r := t.rec.Recovery; r != nil && len(r.UnknownOwners) > 0 {
				before := len(r.UnknownOwners)
				r.UnknownOwners = feedRemoveString(r.UnknownOwners, key)
				if len(r.UnknownOwners) != before {
					r.PendingOwners = feedAddUnique(r.PendingOwners, key)
					t.dirty = true
				}
			}
		case feedOwnerUnknown:
			if r := t.rec.Recovery; r != nil {
				before := len(r.UnknownOwners)
				r.UnknownOwners = feedAddUnique(r.UnknownOwners, key)
				if len(r.UnknownOwners) != before {
					t.dirty = true
				}
			} else if t.op == "heartbeat" {
				t.logLine("[feed-weight-shared] owner=%s liveness unknown (%s); its allocation stays counted", key, detail)
			}
		}
	}
}

func (t *feedAuthTx) prune() {
	cut := t.now.Ns - int64(feedBudgetWindow)
	for key, o := range t.rec.Owners {
		kept := o.Charges[:0]
		for _, c := range o.Charges {
			if c.AtNs > cut {
				kept = append(kept, c)
			}
		}
		if len(kept) != len(o.Charges) {
			t.dirty = true
		}
		o.Charges = kept
		for typ, b := range o.Bounds {
			if !o.Active && b.AtNs <= cut {
				delete(o.Bounds, typ)
				t.dirty = true
			}
		}
		pending := t.rec.Recovery != nil && (containsString(t.rec.Recovery.PendingOwners, key) || containsString(t.rec.Recovery.UnknownOwners, key))
		if !o.Active && len(o.Permits) == 0 && len(o.Charges) == 0 && len(o.Bounds) == 0 && !pending && o.Policy.Reserve == 0 && !o.Unproven {
			delete(t.rec.Owners, key)
			t.dirty = true
		}
	}
	for host, h := range t.rec.Hosts {
		if h.PauseUntilNs < cut {
			delete(t.rec.Hosts, host)
			t.dirty = true
		}
	}
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (t *feedAuthTx) rebase(prevLast int64) {
	for _, o := range t.rec.Owners {
		for _, c := range o.Charges {
			c.AtNs = t.now.Ns
		}
		for _, p := range o.Permits {
			p.CreatedNs = t.now.Ns
		}
		for _, b := range o.Bounds {
			b.AtNs = t.now.Ns
		}
		if o.StoppedNs != 0 {
			o.StoppedNs = t.now.Ns
		}
	}
	for _, h := range t.rec.Hosts {
		if h.PauseUntilNs > prevLast {
			h.PauseUntilNs = t.now.Ns + int64(feedRateLimitPause)
			h.PauseWall = feedWallString(t.now.Wall.Add(feedRateLimitPause))
		} else {
			h.PauseUntilNs = 0
		}
	}
	t.rec.Recovery = nil
}

func (t *feedAuthTx) startRecovery(reason string, rebuild bool) *feedWeightRefusal {
	a := t.a
	gen := t.rec.Generation
	if seen := a.maxGeneration(); seen > gen {
		gen = seen
	}
	gen++
	r := &feedAuthRecovery{Generation: gen, Reason: reason, Started: feedWallString(t.now.Wall), StartedNs: t.now.Ns,
		HoldUntilNs: t.now.Ns + int64(feedBudgetWindow), PendingOwners: []string{}, UnknownOwners: []string{}}
	if rebuild {
		skip := ""
		if a.registered {
			skip = a.key
		}
		rb, err := feedRebuildRows(a.dir, skip, false, t.now.Ns)
		if err != nil {
			return feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("rebuild %s: %v; participating sends stay held until the recovery evidence can be read", a.recordPath(), err))
		}
		for key, o := range rb.Rows {
			t.rec.Owners[key] = o
		}
		r.PendingOwners = append(r.PendingOwners, rb.Pending...)
		r.UnknownOwners = append(r.UnknownOwners, rb.Unknown...)
		source := "none"
		switch {
		case rb.Evidence:
			source = "policy evidence"
		case rb.Legacy:
			source = "version 1 declarations"
		}
		held := []string{}
		for _, key := range t.rec.sortedOwnerKeys() {
			if o := t.rec.Owners[key]; !o.Active && o.Policy.Reserve > 0 {
				held = append(held, fmt.Sprintf("%s:%d", key, o.Policy.Reserve))
			}
		}
		t.logLine("[feed-weight-shared] rebuild from %s: stopped owners with held reserves %s; unproven reserves %s",
			source, joinFeedNames(held), joinFeedNames(rb.Unproven))
	} else {
		for _, key := range t.rec.sortedOwnerKeys() {
			o := t.rec.Owners[key]
			if !o.Active || key == a.key {
				continue
			}
			state, _ := feedProbeOwnerLock(a.dir, key, false)
			switch state {
			case feedOwnerDead:
				t.convertPermits(o, nil)
				o.Active = false
				o.StoppedNs = t.now.Ns
			case feedOwnerLive:
				r.PendingOwners = append(r.PendingOwners, key)
			default:
				r.UnknownOwners = append(r.UnknownOwners, key)
			}
		}
	}
	if a.registered {
		r.PendingOwners = feedAddUnique(r.PendingOwners, a.key)
	}
	sort.Strings(r.PendingOwners)
	sort.Strings(r.UnknownOwners)
	t.rec.Generation = gen
	t.rec.EpochID = feedRandomID(8)
	r.EpochID = t.rec.EpochID
	t.rec.Recovery = r
	t.dirty = true
	t.logLine("[feed-weight-shared] recovery generation=%d reason=%s epoch=%s hold_until=%s pending_owners=%s unknown_owners=%s",
		gen, reason, t.rec.EpochID, feedWallString(t.now.Wall.Add(feedBudgetWindow)), joinFeedNames(r.PendingOwners), joinFeedNames(r.UnknownOwners))
	return nil
}

func (t *feedAuthTx) maybeEndRecovery() {
	r := t.rec.Recovery
	if r == nil || t.now.Ns < r.HoldUntilNs || len(r.UnknownOwners) > 0 {
		return
	}
	for _, key := range r.PendingOwners {
		o := t.rec.Owners[key]
		if o != nil && o.Active && o.EpochID == t.rec.EpochID {
			continue
		}
		dead, ok := r.DeadAtNs[key]
		if (o == nil || !o.Active) && ok && t.now.Ns-dead >= int64(feedBudgetWindow) {
			continue
		}
		if o == nil && !ok {
			continue
		}
		return
	}
	ended := *r
	t.rec.LastRecovery = &ended
	t.rec.Recovery = nil
	t.dirty = true
	t.logLine("[feed-weight-shared] recovery generation=%d reason=%s ended", r.Generation, r.Reason)
}

func (t *feedAuthTx) heapOpenPermits() map[string]*feedRequestPermit {
	return t.a.openSnapshot()
}

func (t *feedAuthTx) reconcile() {
	a := t.a
	if !a.registered {
		return
	}
	me := t.owner()
	if me == nil {
		me = &feedAuthOwner{Bounds: map[string]*feedAuthBound{}, Permits: []*feedAuthPermit{}, Charges: []*feedAuthCharge{}, Reconstructed: true}
		t.rec.Owners[a.key] = me
		t.dirty = true
	}
	if me.Incarnation != "" && me.Incarnation != a.incarnation && !me.Reconstructed {
		detail := fmt.Sprintf("incarnation %s is no longer the registered incarnation (%s)", a.incarnation, me.Incarnation)
		if a.latch(feedWeightCauseSharedOwner, detail) {
			t.logLine("[feed-weight-shared] CRITICAL: owner=%s %s; this process refuses every shared send", a.key, detail)
		}
		return
	}
	if me.Reconstructed || me.EpochID != t.rec.EpochID || !me.Active {
		me.applyIdentity(a.identity())
		me.Active = true
		if me.Reconstructed {
			t.readoptHeap(me)
		}
		me.Reconstructed = false
		me.EpochID = t.rec.EpochID
		me.StoppedNs = 0
		if r := t.rec.Recovery; r != nil {
			r.UnknownOwners = feedRemoveString(r.UnknownOwners, a.key)
		}
		t.dirty = true
	}
	if me.Policy != a.policy || me.Unproven || me.PolicyTx == "" {
		before := t.rec.commonReserve()
		if tx, r := t.stagePolicy(a.key, a.identity(), a.policy, false); r != nil {
			t.logLine("[feed-weight-shared] owner=%s record policy %+v differs from this owner's policy %+v and the correction is not staged yet (%s); the next transaction retries", a.key, me.Policy, a.policy, r.Detail)
		} else {
			me.Policy, me.PolicyTx, me.Unproven = a.policy, tx, false
			t.noteReserveChange(before)
		}
	}
	open := t.heapOpenPermits()
	inRecord := map[string]bool{}
	kept := me.Permits[:0]
	converted := 0
	for _, p := range me.Permits {
		if p.Incarnation == a.incarnation && open[p.ID] != nil {
			inRecord[p.ID] = true
			kept = append(kept, p)
			continue
		}
		if p.Cost > 0 {
			me.Charges = append(me.Charges, t.chargeFromPermit(p))
		}
		converted++
	}
	me.Permits = kept
	if converted > 0 {
		t.dirty = true
	}
	ids := make([]string, 0, len(open))
	for id := range open {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if inRecord[id] {
			continue
		}
		hp := open[id]
		me.Permits = append(me.Permits, t.permitRecord(hp, hp.durableEpoch))
		t.dirty = true
	}
}

func (t *feedAuthTx) permitRecord(p *feedRequestPermit, epoch string) *feedAuthPermit {
	state := feedPermitStatePending
	if p.durableState == feedPermitStateSending {
		state = feedPermitStateSending
	}
	return &feedAuthPermit{ID: p.id, Host: p.host, ReqType: p.reqType, Reason: p.reason, Input: p.input, Cost: p.cost,
		StartupPart: p.startupPart, State: state, Incarnation: t.a.incarnation, EpochID: epoch, CreatedNs: t.now.Ns}
}

func (t *feedAuthTx) readoptHeap(me *feedAuthOwner) {
	l := t.a.ledger
	if l == nil {
		return
	}
	l.mu.Lock()
	heapNow := l.clock().UTC()
	l.pruneLocked(heapNow)
	charges := append([]feedWeightCharge(nil), l.wCharges...)
	bounds := map[string]feedWeightBound{}
	for k, b := range l.wBounds {
		bounds[k] = *b
	}
	l.mu.Unlock()
	for i, c := range charges {
		host := c.host
		if host == "" {
			host = feedHostOf(hlMainnetURL)
		}
		age := heapNow.Sub(c.at)
		if age < 0 {
			age = 0
		}
		me.Charges = append(me.Charges, &feedAuthCharge{ID: fmt.Sprintf("readopt-%s-%d", t.rec.EpochID, i), Host: host, Weight: c.weight,
			Startup: c.startup, Debt: c.debt, AtNs: t.now.Ns - int64(age), AtWall: feedWallString(t.now.Wall.Add(-age))})
	}
	for typ, b := range bounds {
		if b.atCap || b.blocked {
			age := heapNow.Sub(b.at)
			me.Bounds[typ] = &feedAuthBound{AtCap: b.atCap, Blocked: b.blocked, Bound: b.bound, Observed: b.observed,
				AtNs: t.now.Ns - int64(age), AtWall: feedWallString(b.at)}
		}
	}
}

func (t *feedAuthTx) retryPendingPauses() {
	a := t.a
	pending := a.pendingPauses()
	if len(pending) == 0 {
		return
	}
	hosts := make([]string, 0, len(pending))
	for h := range pending {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, host := range hosts {
		pp := pending[host]
		t.commitPause(host, pp.UntilNs, pp.Wall, pp.RetryAfter)
	}
	t.onCommit = append(t.onCommit, func() {
		a.stateMu.Lock()
		for _, h := range hosts {
			delete(a.pendingPause, h)
		}
		a.stateMu.Unlock()
		t.logs = append(t.logs, fmt.Sprintf("[feed-weight-shared] owner=%s shared pause committed after an earlier write failure: hosts=%s", a.key, strings.Join(hosts, ",")))
	})
}

func (t *feedAuthTx) commitPause(host string, untilNs int64, wall time.Time, retryAfter string) {
	h := t.rec.Hosts[host]
	if h == nil {
		h = &feedAuthHost{}
		t.rec.Hosts[host] = h
	}
	if untilNs > h.PauseUntilNs {
		h.PauseUntilNs = untilNs
		h.PauseWall = feedWallString(wall)
	}
	if retryAfter != "" {
		h.LastRetryAfter = retryAfter
	}
	t.dirty = true
}

func (a *feedWeightAuthority) refuseFast(op string) *feedWeightRefusal {
	if op == "heartbeat" || op == "register" || op == "stop" || op == "redeclare" {
		return nil
	}
	if a.faultErr != nil && time.Now().Before(a.faultUntil) {
		return a.faultErr
	}
	return nil
}

func (a *feedWeightAuthority) noteFault(r *feedWeightRefusal) {
	if r == nil {
		return
	}
	switch r.Cause {
	case feedWeightCauseSharedStorage, feedWeightCauseSharedClock, feedWeightCauseSharedVersion:
	default:
		return
	}
	a.faultErr = r
	a.faultUntil = time.Now().Add(feedAuthorityFaultRetry)
	a.consecutiveFaults++
	a.viewMu.Lock()
	if r.Cause == feedWeightCauseSharedStorage {
		a.view.status.Registry = "unavailable"
	}
	a.view.status.LastRefusal = r.Detail
	a.viewMu.Unlock()
	a.reportMu.Lock()
	if a.consecutiveFaults >= feedAuthorityOutageAfter && !a.outageReported {
		a.outageReported = true
		a.outageDetail = r.Detail
		a.outageCause = r.Cause
	}
	a.reportMu.Unlock()
}

func (a *feedWeightAuthority) noteHealthy() {
	a.faultErr = nil
	a.consecutiveFaults = 0
	a.reportMu.Lock()
	if a.outageReported {
		a.outageReported = false
		a.outageCleared = true
	}
	a.reportMu.Unlock()
}

func (a *feedWeightAuthority) tx(ctx context.Context, op string, fn func(t *feedAuthTx) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if r := a.acquireGate(ctx); r != nil {
		return r
	}
	defer a.releaseGate()
	return a.txLocked(ctx, op, fn)
}

func (a *feedWeightAuthority) txLocked(ctx context.Context, op string, fn func(t *feedAuthTx) error) (result error) {
	if a.stopped {
		return feedSharedRefusal(feedWeightCauseSharedOwner, fmt.Sprintf("owner %s stopped", a.key))
	}
	if r := a.refuseFast(op); r != nil {
		return r
	}
	t := &feedAuthTx{a: a, op: op}
	defer func() {
		for _, f := range t.onFinally {
			f()
		}
		for _, line := range t.logs {
			a.logf("%s", line)
		}
	}()
	fail := func(r *feedWeightRefusal) error {
		a.noteFault(r)
		return r
	}
	if r := a.ensureDir(); r != nil {
		return fail(r)
	}
	lock, r := a.openAuthorityLock(ctx)
	if r != nil {
		return fail(r)
	}
	defer func() {
		syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		lock.Close()
	}()
	now, err := readFeedBoot(a.clock)
	if err != nil {
		a.clockFaulted = true
		ref := feedSharedRefusal(feedWeightCauseSharedClock, fmt.Sprintf("boot clock (%s): %v", feedBootClockSource, err))
		a.viewMu.Lock()
		a.view.status.Registry = "clock_unavailable"
		a.view.status.LastRefusal = ref.Detail
		a.viewMu.Unlock()
		a.noteFault(ref)
		return ref
	}
	t.now = now
	a.cleanStaleTemps()
	rec, state, detail, lr := loadFeedAuthRecord(a.recordPath(), false)
	if lr != nil {
		if lr.Cause == feedWeightCauseSharedVersion {
			a.viewMu.Lock()
			a.view.status.Registry = "unsupported_version"
			a.view.status.LastRefusal = lr.Detail
			a.viewMu.Unlock()
			a.noteFault(lr)
			return lr
		}
		return fail(lr)
	}
	clockRecovered := false
	switch state {
	case feedAuthLoadAbsent:
		t.rec = newFeedAuthRecord()
		reason := feedAuthorityRecoveryMissing
		if feedAuthorityDirEmpty(a.dir) {
			reason = feedAuthorityRecoveryInitial
		}
		if r := t.startRecovery(reason, true); r != nil {
			return fail(r)
		}
	case feedAuthLoadCorrupt:
		t.rec = newFeedAuthRecord()
		t.logLine("[feed-weight-shared] %s is corrupt (%s); rebuilding from owner policy evidence", a.recordPath(), detail)
		if r := t.startRecovery(feedAuthorityRecoveryCorrupt, true); r != nil {
			return fail(r)
		}
	default:
		t.rec = rec
		if state == feedAuthLoadLegacy {
			if r := t.migrate(detail); r != nil {
				return fail(r)
			}
		}
		switch {
		case rec.BootID != now.Boot:
			prev := rec.LastBootNs
			t.rebase(prev)
			t.startRecovery(feedAuthorityRecoveryBoot, false)
		case now.Ns < rec.LastBootNs:
			t.rebase(rec.LastBootNs)
			t.startRecovery(feedAuthorityRecoveryRegressed, false)
		case a.clockFaulted:
			t.startRecovery(feedAuthorityRecoveryClockFault, false)
			clockRecovered = true
		}
	}
	if a.clockFaulted && state != feedAuthLoadOK && state != feedAuthLoadLegacy {
		clockRecovered = true
	}
	t.sweep()
	t.reconcile()
	t.prune()
	t.retryPendingPauses()
	t.maybeEndRecovery()
	var fnErr error
	if fn != nil {
		fnErr = fn(t)
	}
	if state == feedAuthLoadOK && feedAuthoritySyncOp(op) {
		t.syncEvidence()
	}
	if t.dirty {
		if err := a.writeRecord(t); err != nil {
			return fail(feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("commit %s (%s): %v", a.recordPath(), op, err)))
		}
		for _, f := range t.afterWrite {
			if err := f(); err != nil {
				t.logLine("[feed-weight-shared] owner policy evidence stays staged after %s (%v); the staged file keeps the larger policy until the next transaction finalizes it", op, err)
			}
		}
	}
	if clockRecovered {
		a.clockFaulted = false
	}
	a.noteHealthy()
	for _, f := range t.onCommit {
		f()
	}
	a.refreshView(t.rec, t.now)
	return fnErr
}

func feedAuthoritySyncOp(op string) bool {
	switch op {
	case "heartbeat", "register", "redeclare", "stop", "release":
		return true
	}
	return false
}

func feedAuthorityDirEmpty(dir string) bool {
	if set, err := feedReadPolicyEvidenceSet(dir); err != nil || len(set) > 0 {
		return false
	}
	if decls, err := feedReadDeclarations(dir); err != nil || len(decls) > 0 {
		return false
	}
	return len(feedDeclaredLockKeys(dir)) == 0
}

func (a *feedWeightAuthority) ownerStatus(key string, o *feedAuthOwner) feedSharedOwnerStatus {
	st := feedSharedOwnerStatus{Key: key, Role: o.Role, ConfigPath: o.ConfigPath, Active: o.Active, Reserve: o.Policy.Reserve, Unproven: o.Unproven}
	if o.Active {
		st.Allocation = o.Policy.allocation()
	}
	for _, p := range o.Permits {
		if p.State == feedPermitStateSending {
			st.InFlight += int64(p.Cost)
		} else {
			st.Pending += int64(p.Cost)
		}
	}
	for _, c := range o.Charges {
		st.Charged += int64(c.Weight)
		if c.Debt {
			st.Debt += int64(c.Weight)
		}
	}
	return st
}

func feedRecoveryStatus(r *feedAuthRecovery, now feedBootReading) *feedSharedRecoveryStatus {
	if r == nil {
		return nil
	}
	return &feedSharedRecoveryStatus{Generation: r.Generation, Reason: r.Reason, Started: r.Started,
		HoldUntil:     feedWallString(now.Wall.Add(time.Duration(r.HoldUntilNs - now.Ns))),
		PendingOwners: append([]string{}, r.PendingOwners...), UnknownOwners: append([]string{}, r.UnknownOwners...)}
}

func (a *feedWeightAuthority) heapTimeOf(untilNs int64, now feedBootReading) time.Time {
	var heapNow time.Time
	if a.ledger != nil {
		heapNow = a.ledger.clock().UTC()
	} else {
		heapNow = now.Wall
	}
	return heapNow.Add(time.Duration(untilNs - now.Ns))
}

func (a *feedWeightAuthority) refreshView(rec *feedAuthRecord, now feedBootReading) {
	st := feedSharedStatus{Coverage: feedSharedCoverageShared, Registry: "ok", Directory: a.dir, OwnerKey: a.key, Role: a.role,
		ConfigPath: a.configPath, Incarnation: a.incarnation, CommonReserve: rec.commonReserve(), CombinedAllocation: rec.combinedAllocation(),
		Capacity: rec.capacity(), Hosts: []feedSharedHostStatus{}, Owners: []feedSharedOwnerStatus{}}
	if !a.registered {
		st.Registry = "unregistered"
		if a.registerErr != nil {
			st.LastRefusal = a.registerErr.Error()
		}
	}
	pending := a.pendingPauses()
	if c := a.latchedCause(); c != "" {
		st.Registry = "latched_" + c
	}
	if len(pending) > 0 {
		st.Registry = "pause_unavailable"
	}
	if me := rec.Owners[a.key]; me != nil && me.Active {
		st.Allocation = me.Policy.allocation()
	}
	var nextExpiry time.Time
	hosts := map[string]bool{}
	for _, key := range rec.sortedOwnerKeys() {
		o := rec.Owners[key]
		ost := a.ownerStatus(key, o)
		st.Owners = append(st.Owners, ost)
		if key == a.key {
			st.Pending, st.InFlight, st.Charged, st.Debt = ost.Pending, ost.InFlight, ost.Charged, ost.Debt
		} else if !o.Active {
			st.StoppedOwnerDebt += ost.Charged
		}
		for _, c := range o.Charges {
			hosts[c.Host] = true
			exp := a.heapTimeOf(c.AtNs+int64(feedBudgetWindow), now)
			if nextExpiry.IsZero() || exp.Before(nextExpiry) {
				nextExpiry = exp
			}
		}
		for _, p := range o.Permits {
			hosts[p.Host] = true
		}
	}
	for h := range rec.Hosts {
		hosts[h] = true
	}
	for h := range pending {
		hosts[h] = true
	}
	hostList := make([]string, 0, len(hosts))
	for h := range hosts {
		hostList = append(hostList, h)
	}
	sort.Strings(hostList)
	shortfall := map[string]int64{}
	for _, h := range hostList {
		hs := feedSharedHostStatus{Host: h, Used: rec.usedOnHost(h), Pause: "clear"}
		if hs.Used > st.Capacity {
			hs.Shortfall = hs.Used - st.Capacity
			shortfall[h] = hs.Shortfall
		}
		if hp := rec.Hosts[h]; hp != nil && hp.PauseUntilNs > now.Ns {
			hs.Pause = "confirmed"
			hs.PausedUntil = feedWallString(now.Wall.Add(time.Duration(hp.PauseUntilNs - now.Ns)))
		}
		if pp, ok := pending[h]; ok {
			hs.Pause = "unavailable"
			hs.PausedUntil = feedWallString(pp.Wall)
		}
		st.Hosts = append(st.Hosts, hs)
	}
	st.Recovery = feedRecoveryStatus(rec.Recovery, now)
	st.LastRecovery = feedRecoveryStatus(rec.LastRecovery, now)
	st.UnprovenOwners = rec.unprovenOwners()
	if len(st.UnprovenOwners) > 0 {
		st.Registry = "reserve_unproven"
	}
	var holdUntil time.Time
	if rec.Recovery != nil {
		st.Registry = "recovery_hold"
		holdUntil = a.heapTimeOf(rec.Recovery.HoldUntilNs, now)
	}
	st.ViewAt = feedWallString(now.Wall)
	a.mergePauses(rec, now)
	a.viewMu.Lock()
	prevRefusal := a.view.status.LastRefusal
	if st.LastRefusal == "" {
		st.LastRefusal = prevRefusal
	}
	prevShortfall := a.view.shortfall
	a.view.shortfall = shortfall
	a.view.status = st
	a.view.holdUntil = holdUntil
	a.view.nextExpiry = nextExpiry
	a.view.recovery, a.view.lastRecovery = nil, nil
	if rec.Recovery != nil {
		r := *rec.Recovery
		a.view.recovery = &r
	}
	if rec.LastRecovery != nil {
		r := *rec.LastRecovery
		a.view.lastRecovery = &r
	}
	if rec.Generation > a.view.maxGeneration {
		a.view.maxGeneration = rec.Generation
	}
	a.viewMu.Unlock()
	for _, h := range hostList {
		cur, was := shortfall[h], prevShortfall[h]
		switch {
		case cur > 0 && was == 0:
			a.logf("[feed-weight-shared] owner=%s host=%s reserve shortfall=%d: used %d is above capacity %d (common reserve %d); no new participating reservation or pending send fits on this host until use falls", a.key, h, cur, rec.usedOnHost(h), st.Capacity, st.CommonReserve)
		case cur == 0 && was > 0:
			a.logf("[feed-weight-shared] owner=%s host=%s reserve shortfall cleared: used %d fits capacity %d", a.key, h, rec.usedOnHost(h), st.Capacity)
		}
	}
	for _, h := range feedSortedKeys(feedKeySet(prevShortfall)) {
		if !containsString(hostList, h) {
			a.logf("[feed-weight-shared] owner=%s host=%s reserve shortfall cleared: no recorded use", a.key, h)
		}
	}
}

func (a *feedWeightAuthority) mergePauses(rec *feedAuthRecord, now feedBootReading) {
	l := a.ledger
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	heapNow := l.clock().UTC()
	for host, h := range rec.Hosts {
		if h.PauseUntilNs <= now.Ns {
			continue
		}
		until := heapNow.Add(time.Duration(h.PauseUntilNs - now.Ns))
		hp := l.hostLocked(host)
		if !until.After(hp.until) {
			continue
		}
		hp.until = until
		if !hp.episode {
			hp.episode = true
			hp.openedAt = heapNow
			hp.extensions = 0
			hp.refused = 0
			hp.refusedInputs = map[string]bool{}
			hp.pauseAlerted = false
			hp.origin = "peer"
		}
		hp.successAfter = false
	}
}

func (a *feedWeightAuthority) maxGeneration() int64 {
	a.viewMu.Lock()
	defer a.viewMu.Unlock()
	return a.view.maxGeneration
}

func (a *feedWeightAuthority) markPauseUnavailable(host string, until time.Time) {
	a.viewMu.Lock()
	defer a.viewMu.Unlock()
	a.view.status.Registry = "pause_unavailable"
	hosts := append([]feedSharedHostStatus{}, a.view.status.Hosts...)
	found := false
	for i := range hosts {
		if hosts[i].Host == host {
			hosts[i].Pause, hosts[i].PausedUntil, found = "unavailable", feedWallString(until), true
		}
	}
	if !found {
		hosts = append(hosts, feedSharedHostStatus{Host: host, Pause: "unavailable", PausedUntil: feedWallString(until)})
		sort.Slice(hosts, func(i, j int) bool { return hosts[i].Host < hosts[j].Host })
	}
	a.view.status.Hosts = hosts
}

func (a *feedWeightAuthority) statusView() feedSharedStatus {
	a.viewMu.Lock()
	defer a.viewMu.Unlock()
	st := a.view.status
	st.Hosts = append([]feedSharedHostStatus{}, st.Hosts...)
	st.Owners = append([]feedSharedOwnerStatus{}, st.Owners...)
	return st
}

func (a *feedWeightAuthority) sharedRoom(host string) int {
	st := a.statusView()
	if st.Registry != "ok" || st.Recovery != nil {
		return 0
	}
	room := st.Capacity
	for _, h := range st.Hosts {
		if h.Host == host {
			if h.Pause != "clear" {
				return 0
			}
			room -= h.Used
		}
	}
	return int(max(room, 0))
}

func (a *feedWeightAuthority) noteRefusal(r *feedWeightRefusal) {
	if r == nil {
		return
	}
	a.viewMu.Lock()
	a.view.status.LastRefusal = r.Cause + ": " + r.Detail
	a.viewMu.Unlock()
}

func (a *feedWeightAuthority) waitHint(now time.Time) time.Duration {
	a.viewMu.Lock()
	defer a.viewMu.Unlock()
	wait := feedAuthorityHeartbeat
	for _, t := range []time.Time{a.view.holdUntil, a.view.nextExpiry} {
		if !t.IsZero() && t.After(now) && t.Sub(now) < wait {
			wait = t.Sub(now)
		}
	}
	if wait < 100*time.Millisecond {
		wait = 100 * time.Millisecond
	}
	return wait
}

func (t *feedAuthTx) aggregateCheck(policy feedAuthPolicy) *feedAuthorityUnsafeError {
	unknown := map[string]bool{}
	if r := t.rec.Recovery; r != nil {
		for _, k := range r.UnknownOwners {
			unknown[k] = true
		}
	}
	res := feedAdmissionOf(t.rec, t.a.key, policy, func(key string, o *feedAuthOwner) bool {
		if !unknown[key] && !o.Reconstructed {
			return true
		}
		state, _ := feedProbeOwnerLock(t.a.dir, key, false)
		return state == feedOwnerLive
	})
	if res.Combined <= res.Capacity {
		return nil
	}
	return &feedAuthorityUnsafeError{Combined: res.Combined, Capacity: res.Capacity, Reserve: res.Reserve, ReserveOwner: res.ReserveOwner, AllLive: res.AllLive, Owners: res.Owners}
}

func (a *feedWeightAuthority) register(ctx context.Context, policy *feedWeightPolicy) error {
	if r := a.acquireGate(ctx); r != nil {
		return r
	}
	defer a.releaseGate()
	return a.registerLocked(ctx, policy)
}

func (a *feedWeightAuthority) registerLocked(ctx context.Context, policy *feedWeightPolicy) error {
	if a.registered {
		return nil
	}
	a.policy = feedAuthPolicyOf(policy)
	var lifetime *os.File
	var lifetimeID feedFileID
	err := a.txLocked(ctx, "register", func(t *feedAuthTx) error {
		if t.rec.Owners[a.key] == nil && len(t.rec.Owners) >= feedAuthorityMaxOwners {
			return feedSharedRefusal(feedWeightCauseSharedOwner, fmt.Sprintf("the shared weight record already holds %d owners; release stopped owners with go-trader feed-weight-release", feedAuthorityMaxOwners))
		}
		path := feedAuthorityOwnerLockPath(a.dir, a.key)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("open lifetime lock %s: %v", path, err))
		}
		if err := flockWithRetry(f); err != nil {
			f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return &feedAuthorityDuplicateError{Key: a.key, Path: path}
			}
			return feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("lifetime lock %s: %v", path, err))
		}
		fi, err := f.Stat()
		id, ok := feedFileID{}, false
		if err == nil {
			id, ok = feedFileIDOf(fi)
		}
		if !ok {
			f.Close()
			return feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("lifetime lock %s cannot be identified", path))
		}
		if unsafe := t.aggregateCheck(a.policy); unsafe != nil {
			f.Close()
			return unsafe
		}
		before := t.rec.commonReserve()
		tx, sr := t.stagePolicy(a.key, a.identity(), a.policy, false)
		if sr != nil {
			f.Close()
			return sr
		}
		prev := t.rec.Owners[a.key]
		me := &feedAuthOwner{Bounds: map[string]*feedAuthBound{}, Permits: []*feedAuthPermit{}, Charges: []*feedAuthCharge{}}
		if prev != nil {
			if prev.Incarnation != a.incarnation && len(prev.Permits) > 0 {
				n := t.convertPermits(prev, nil)
				t.logLine("[feed-weight-shared] owner=%s previous incarnation %s left %d unresolved permit(s); kept as full charges", a.key, prev.Incarnation, n)
			}
			me.Charges = prev.Charges
			cut := t.now.Ns - int64(feedBudgetWindow)
			for typ, b := range prev.Bounds {
				if b.AtNs > cut {
					me.Bounds[typ] = b
				}
			}
		}
		spent := 0
		for _, c := range me.Charges {
			spent += c.Startup
		}
		left := a.policy.Startup - spent
		if left < 0 {
			left = 0
		}
		me.Role, me.ConfigPath, me.Incarnation, me.PID = a.role, a.configPath, a.incarnation, a.pid
		me.StartedWall, me.Active, me.EpochID, me.Policy, me.StartupLeft = feedWallString(a.startedWall), true, t.rec.EpochID, a.policy, left
		me.PolicyTx = tx
		t.rec.Owners[a.key] = me
		t.noteReserveChange(before)
		if r := t.rec.Recovery; r != nil {
			r.UnknownOwners = feedRemoveString(r.UnknownOwners, a.key)
			r.PendingOwners = feedAddUnique(r.PendingOwners, a.key)
		}
		t.dirty = true
		seed := *me
		seedNow := t.now
		pauses := map[string]int64{}
		for h, hp := range t.rec.Hosts {
			pauses[h] = hp.PauseUntilNs
		}
		t.onCommit = append(t.onCommit, func() {
			lifetime, lifetimeID = f, id
			a.registered = true
			a.registerErr = nil
			a.seedLedger(&seed, seedNow, left)
		})
		t.onFinally = append(t.onFinally, func() {
			if lifetime == nil {
				f.Close()
			}
		})
		t.logLine("[feed-weight-shared] registered owner=%s role=%s config=%s incarnation=%s allocation=%d reserve=%d combined=%d capacity=%d startup_left=%d directory=%s clock=%s",
			a.key, a.role, a.configPath, a.incarnation, a.policy.allocation(), a.policy.Reserve, t.rec.combinedAllocation(), t.rec.capacity(), left, a.dir, feedBootClockSource)
		return nil
	})
	if err != nil {
		a.registerErr = err
		a.viewMu.Lock()
		a.view.status.Registry = "unregistered"
		a.view.status.LastRefusal = err.Error()
		a.viewMu.Unlock()
		return err
	}
	a.lifetime, a.lifetimeID = lifetime, lifetimeID
	return nil
}

func (a *feedWeightAuthority) seedLedger(me *feedAuthOwner, now feedBootReading, startupLeft int) {
	l := a.ledger
	if l == nil || a.seeded {
		return
	}
	a.seeded = true
	l.mu.Lock()
	defer l.mu.Unlock()
	heapNow := l.clock().UTC()
	for _, c := range me.Charges {
		at := heapNow.Add(-time.Duration(now.Ns - c.AtNs))
		l.wCharges = append(l.wCharges, feedWeightCharge{at: at, weight: c.Weight, startup: c.Startup, debt: c.Debt})
	}
	sort.SliceStable(l.wCharges, func(i, j int) bool { return l.wCharges[i].at.Before(l.wCharges[j].at) })
	types := make([]string, 0, len(me.Bounds))
	for t := range me.Bounds {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, typ := range types {
		b := me.Bounds[typ]
		hb := l.wBounds[typ]
		if hb == nil {
			hb = &feedWeightBound{}
			l.wBounds[typ] = hb
		}
		hb.atCap, hb.blocked, hb.bound, hb.observed = b.AtCap, b.Blocked, b.Bound, b.Observed
		hb.at, hb.alerted = heapNow.Add(-time.Duration(now.Ns-b.AtNs)), true
		hb.epoch++
	}
	l.seededStartup = true
	l.seededStartupLeft = startupLeft
	l.wStartupLeft = startupLeft
	l.signalLocked()
}

func (a *feedWeightAuthority) redeclare(ctx context.Context, policy *feedWeightPolicy, apply func()) error {
	if r := a.acquireGate(ctx); r != nil {
		return r
	}
	defer a.releaseGate()
	if !a.registered {
		if policy == nil {
			a.policy = feedAuthPolicy{}
			apply()
			return nil
		}
		if err := a.registerLocked(ctx, policy); err != nil {
			return err
		}
		apply()
		return nil
	}
	if policy == nil {
		return feedSharedRefusal(feedWeightCauseSharedOwner, fmt.Sprintf("owner %s is registered in the shared weight record; a reload cannot remove its complete weight policy (set an explicit policy, for example weight_reserve 0, or restart without it: the held reserve stays until go-trader feed-weight-release)", a.key))
	}
	next := feedAuthPolicyOf(policy)
	return a.txLocked(ctx, "redeclare", func(t *feedAuthTx) error {
		if unsafe := t.aggregateCheck(next); unsafe != nil {
			return unsafe
		}
		me := t.owner()
		if me == nil || me.Incarnation != a.incarnation || !me.Active {
			return feedSharedRefusal(feedWeightCauseSharedOwner, fmt.Sprintf("owner %s incarnation %s is not the active registered incarnation", a.key, a.incarnation))
		}
		before := t.rec.commonReserve()
		tx, sr := t.stagePolicy(a.key, a.identity(), next, false)
		if sr != nil {
			return sr
		}
		me.Policy, me.PolicyTx = next, tx
		t.dirty = true
		t.noteReserveChange(before)
		t.onCommit = append(t.onCommit, func() {
			a.policy = next
			apply()
		})
		t.logLine("[feed-weight-shared] owner=%s redeclared allocation=%d reserve=%d", a.key, next.allocation(), next.Reserve)
		return nil
	})
}

func (a *feedWeightAuthority) sendRefusalLocked(t *feedAuthTx) *feedWeightRefusal {
	if c := a.latchedCause(); c != "" {
		return feedSharedRefusal(c, fmt.Sprintf("owner %s latched its shared sends closed", a.key))
	}
	if len(a.pendingPauses()) > 0 {
		return feedSharedRefusal(feedWeightCauseSharedStorage, "a venue rate-limit pause could not be committed to the shared record; sends stay closed until it is")
	}
	if !a.registered {
		detail := "this owner is not registered in the shared weight record"
		if a.registerErr != nil {
			detail += ": " + a.registerErr.Error()
		}
		return feedSharedRefusal(feedWeightCauseSharedStorage, detail)
	}
	me := t.owner()
	if me == nil || me.Incarnation != a.incarnation || !me.Active {
		return feedSharedRefusal(feedWeightCauseSharedOwner, fmt.Sprintf("owner %s incarnation %s is not the active registered incarnation", a.key, a.incarnation))
	}
	if r := t.rec.Recovery; r != nil {
		return feedSharedRefusal(feedWeightCauseSharedHold, fmt.Sprintf("shared recovery generation %d (%s) holds every participating send until %s", r.Generation, r.Reason,
			feedWallString(t.now.Wall.Add(time.Duration(r.HoldUntilNs-t.now.Ns)))))
	}
	if keys := t.rec.unprovenOwners(); len(keys) > 0 {
		return feedSharedRefusal(feedWeightCauseSharedHold, fmt.Sprintf("the held weight reserve of owner(s) %s cannot be proved from the recovery evidence; participating sends stay held until each one restarts with its config or, once it is confirmed stopped, an operator runs go-trader feed-weight-release --owner <key>", strings.Join(keys, ",")))
	}
	return nil
}

func (a *feedWeightAuthority) hostPausedLocked(t *feedAuthTx, host string) error {
	if h := t.rec.Hosts[host]; h != nil && h.PauseUntilNs > t.now.Ns {
		return &feedHostPausedError{Host: host, Until: a.heapTimeOf(h.PauseUntilNs, t.now)}
	}
	return nil
}

func (a *feedWeightAuthority) reserve(ctx context.Context, p *feedRequestPermit) error {
	if err := feedAuthorityHookAt("reserve:before"); err != nil {
		return feedSharedRefusal(feedWeightCauseSharedStorage, err.Error())
	}
	return a.tx(ctx, "reserve", func(t *feedAuthTx) error {
		if r := a.sendRefusalLocked(t); r != nil {
			return r
		}
		if err := a.hostPausedLocked(t, p.host); err != nil {
			return err
		}
		used := t.rec.usedOnHost(p.host)
		capacity := t.rec.capacity()
		if used+int64(p.cost) > capacity {
			return feedSharedRefusal(feedWeightCauseSharedExhausted, fmt.Sprintf("cost %d does not fit %d of %d shared weight used on %s by every owner in the last %s (common reserve %d)",
				p.cost, used, capacity, p.host, feedBudgetWindow, t.rec.commonReserve()))
		}
		me := t.owner()
		p.id = feedRandomID(8)
		p.durableEpoch = t.rec.EpochID
		p.durableState = feedPermitStatePending
		me.Permits = append(me.Permits, t.permitRecord(p, t.rec.EpochID))
		t.dirty = true
		t.onCommit = append(t.onCommit, func() {
			a.remember(p)
		})
		return nil
	})
}

func (t *feedAuthTx) findPermit(id string) (*feedAuthOwner, int) {
	me := t.owner()
	if me == nil {
		return nil, -1
	}
	for i, p := range me.Permits {
		if p.ID == id {
			return me, i
		}
	}
	return me, -1
}

func (t *feedAuthTx) dropPermit(me *feedAuthOwner, i int) {
	me.Permits = append(me.Permits[:i], me.Permits[i+1:]...)
	t.dirty = true
}

func (a *feedWeightAuthority) markSending(ctx context.Context, p *feedRequestPermit) error {
	return a.tx(ctx, "send", func(t *feedAuthTx) error {
		me, i := t.findPermit(p.id)
		if i < 0 {
			return feedSharedRefusal(feedWeightCauseSharedOwner, fmt.Sprintf("permit %s is not in the shared record", p.id))
		}
		rp := me.Permits[i]
		refuse := func(err error) error {
			if rp.State == feedPermitStatePending {
				t.dropPermit(me, i)
			}
			return err
		}
		if r := a.sendRefusalLocked(t); r != nil {
			return refuse(r)
		}
		if rp.EpochID != t.rec.EpochID || rp.Incarnation != a.incarnation {
			return refuse(feedSharedRefusal(feedWeightCauseSharedHold, fmt.Sprintf("permit %s was reserved in an earlier shared generation and is fenced", p.id)))
		}
		if err := a.hostPausedLocked(t, p.host); err != nil {
			return refuse(err)
		}
		used := t.rec.usedOnHost(p.host)
		if capacity := t.rec.capacity(); used > capacity {
			return refuse(feedSharedRefusal(feedWeightCauseSharedExhausted, fmt.Sprintf("%d of %d shared weight is used on %s after the common reserve grew to %d", used, capacity, p.host, t.rec.commonReserve())))
		}
		rp.State = feedPermitStateSending
		t.dirty = true
		t.onCommit = append(t.onCommit, func() { p.durableState = feedPermitStateSending })
		return nil
	})
}

type feedAuthClose struct {
	charge     int
	startup    int
	debt       int
	bound      *feedAuthBound
	boundType  string
	pause      bool
	retryAfter string
}

func (a *feedWeightAuthority) closePermit(ctx context.Context, p *feedRequestPermit, c feedAuthClose) {
	if ctx == nil {
		ctx = context.Background()
	}
	defer a.forget(p.id)
	var pauseUntil int64
	var pauseWall time.Time
	err := a.tx(ctx, "close", func(t *feedAuthTx) error {
		if c.pause {
			pauseUntil, pauseWall = t.now.Ns+int64(feedRateLimitPause), t.now.Wall.Add(feedRateLimitPause)
			t.commitPause(p.host, pauseUntil, pauseWall, c.retryAfter)
		}
		me, i := t.findPermit(p.id)
		if i < 0 {
			return nil
		}
		t.dropPermit(me, i)
		feedAuthorityHookAt("close:before_commit")
		if c.charge > 0 {
			me.Charges = append(me.Charges, &feedAuthCharge{ID: p.id, Host: p.host, Weight: c.charge, Startup: c.startup, AtNs: t.now.Ns, AtWall: feedWallString(t.now.Wall)})
		}
		if c.debt > 0 {
			me.Charges = append(me.Charges, &feedAuthCharge{ID: p.id + "-debt", Host: p.host, Weight: c.debt, Debt: true, AtNs: t.now.Ns, AtWall: feedWallString(t.now.Wall)})
		}
		if c.bound != nil {
			b := *c.bound
			b.AtNs, b.AtWall = t.now.Ns, feedWallString(t.now.Wall)
			me.Bounds[c.boundType] = &b
		}
		return nil
	})
	if err != nil && c.pause {
		if pauseUntil == 0 {
			if now, cerr := readFeedBoot(a.clock); cerr == nil {
				pauseUntil, pauseWall = now.Ns+int64(feedRateLimitPause), now.Wall.Add(feedRateLimitPause)
			} else {
				pauseWall = time.Now().UTC().Add(feedRateLimitPause)
			}
		}
		a.stateMu.Lock()
		a.pendingPause[p.host] = feedPendingPause{UntilNs: pauseUntil, Wall: pauseWall, RetryAfter: c.retryAfter}
		a.stateMu.Unlock()
		a.markPauseUnavailable(p.host, pauseWall)
		a.logf("[feed-weight-shared] CRITICAL: owner=%s could not commit the rate-limit pause for %s: %v; this owner refuses every later send until the pause is committed", a.key, p.host, err)
	} else if err != nil {
		a.logf("[feed-weight-shared] owner=%s could not commit the outcome of permit %s (%v); the next transaction keeps its full reservation as a charge", a.key, p.id, err)
	}
}

func (a *feedWeightAuthority) releasePermit(ctx context.Context, p *feedRequestPermit) {
	if ctx == nil {
		ctx = context.Background()
	}
	defer a.forget(p.id)
	err := a.tx(ctx, "release", func(t *feedAuthTx) error {
		me, i := t.findPermit(p.id)
		if i < 0 {
			return nil
		}
		rp := me.Permits[i]
		t.dropPermit(me, i)
		if rp.State != feedPermitStatePending && rp.Cost > 0 {
			me.Charges = append(me.Charges, t.chargeFromPermit(rp))
		}
		return nil
	})
	if err != nil {
		a.logf("[feed-weight-shared] owner=%s could not release permit %s (%v); the next transaction keeps it as a full charge", a.key, p.id, err)
	}
}

func (a *feedWeightAuthority) heartbeatOnce(ctx context.Context) {
	if r := a.acquireGate(ctx); r != nil {
		return
	}
	defer a.releaseGate()
	if a.stopped {
		return
	}
	if !a.registered {
		var dup *feedAuthorityDuplicateError
		if errors.As(a.registerErr, &dup) {
			return
		}
		pol := &feedWeightPolicy{PerMinute: a.policy.PerMinute, Startup: a.policy.Startup, Reserve: a.policy.Reserve}
		if err := a.registerLocked(ctx, pol); err != nil {
			a.logf("[feed-weight-shared] owner=%s registration still refused: %v", a.key, err)
		}
		return
	}
	if a.lifetime != nil {
		cur, err := os.Lstat(feedAuthorityOwnerLockPath(a.dir, a.key))
		id, ok := feedFileID{}, false
		if err == nil {
			id, ok = feedFileIDOf(cur)
		}
		detail := fmt.Sprintf("lifetime lock %s no longer names the held lock file", feedAuthorityOwnerLockPath(a.dir, a.key))
		if (!ok || id != a.lifetimeID) && a.latch(feedWeightCauseSharedOwner, detail) {
			a.logf("[feed-weight-shared] CRITICAL: owner=%s %s; this owner refuses every shared send", a.key, detail)
		}
	}
	if err := a.txLocked(ctx, "heartbeat", nil); err != nil {
		a.logf("[feed-weight-shared] owner=%s heartbeat: %v", a.key, err)
	}
	if l := a.ledger; l != nil {
		l.mu.Lock()
		l.signalLocked()
		l.mu.Unlock()
	}
}

func (a *feedWeightAuthority) runHeartbeat(ctx context.Context) {
	ticker := time.NewTicker(feedAuthorityHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.heartbeatOnce(ctx)
		}
	}
}

func (a *feedWeightAuthority) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), feedAuthorityStopBound)
	defer cancel()
	if r := a.acquireGate(ctx); r != nil {
		return
	}
	defer a.releaseGate()
	if !a.registered || a.stopped {
		return
	}
	err := a.txLocked(ctx, "stop", func(t *feedAuthTx) error {
		me := t.owner()
		if me == nil || me.Incarnation != a.incarnation {
			return nil
		}
		n := t.convertPermits(me, nil)
		me.Active = false
		me.StoppedNs = t.now.Ns
		t.dirty = true
		t.logLine("[feed-weight-shared] owner=%s stopped: allocation removed, reserve %d held, %d open permit(s) kept as full charges, unexpired charges kept", a.key, me.Policy.Reserve, n)
		return nil
	})
	if err != nil {
		a.logf("[feed-weight-shared] owner=%s stop transaction failed (%v); peers remove the allocation once the lifetime lock is free", a.key, err)
	}
	a.stopped = true
	if a.lifetime != nil {
		a.lifetime.Close()
		a.lifetime = nil
	}
}

func (a *feedWeightAuthority) attach(l *feedRequestLedger) {
	a.ledger = l
	l.mu.Lock()
	l.authority = a
	l.clock = newFeedAnchoredClock(a.clock).Now
	l.mu.Unlock()
}

func startFeedAuthority(a *feedWeightAuthority, policy *feedWeightPolicy, notifier *MultiNotifier, prefix string) int {
	err := a.register(context.Background(), policy)
	if err == nil {
		return 0
	}
	var unsafe *feedAuthorityUnsafeError
	var dup *feedAuthorityDuplicateError
	switch {
	case errors.As(err, &dup):
		msg := fmt.Sprintf("shared weight owner refused: %v", err)
		fmt.Fprintf(os.Stderr, "%s CRITICAL: %s (exit %d)\n", prefix, msg, ExitSingletonLock)
		sendStartupRefusalDM(notifier, "Shared weight owner", msg)
		return ExitSingletonLock
	case errors.As(err, &unsafe) && unsafe.AllLive && a.role == feedAuthorityRoleFeed:
		msg := fmt.Sprintf("shared weight admission refused: %v", err)
		fmt.Fprintf(os.Stderr, "%s CRITICAL: %s (exit %d)\n", prefix, msg, ExitProbeFailure)
		sendStartupRefusalDM(notifier, "Shared weight admission", msg)
		return ExitProbeFailure
	case errors.As(err, &unsafe) && unsafe.AllLive:
		msg := fmt.Sprintf("shared weight admission refused (%v); the trading scheduler keeps running so protection, the kill switch and circuit breakers stay active, refuses every participating venue read so entries on those inputs hold, and retries the admission every %s", err, feedAuthorityHeartbeat)
		fmt.Fprintf(os.Stderr, "%s CRITICAL: %s\n", prefix, msg)
		sendStartupRefusalDM(notifier, "Shared weight admission", msg)
		return 0
	}
	msg := fmt.Sprintf("shared weight registration is held (%v); this owner keeps running, refuses every participating venue read and retries every %s", err, feedAuthorityHeartbeat)
	fmt.Fprintf(os.Stderr, "%s WARN: %s\n", prefix, msg)
	sendStartupRefusalDM(notifier, "Shared weight hold", msg)
	return 0
}

func feedAuthorityStartupLine(a *feedWeightAuthority) string {
	return fmt.Sprintf("shared weight: owner=%s role=%s directory=%s clock=%s capability=%s", a.key, a.role, a.dir, feedBootClockSource, feedAuthorityCapability)
}

type feedSharedEvent struct {
	Kind       string
	Generation int64
	Reason     string
	Detail     string
}

func (a *feedWeightAuthority) sharedReport() []feedSharedEvent {
	if a == nil {
		return nil
	}
	a.viewMu.Lock()
	cur, last := a.view.recovery, a.view.lastRecovery
	unproven := strings.Join(a.view.status.UnprovenOwners, ",")
	a.viewMu.Unlock()
	latchCause, latchDetail := a.latchState()
	latched := ""
	if latchCause != "" {
		latched = latchCause + ": " + latchDetail
	}
	a.reportMu.Lock()
	defer a.reportMu.Unlock()
	var out []feedSharedEvent
	if latchCause != "" && !a.latchReported {
		a.latchReported = true
		out = append(out, feedSharedEvent{Kind: "owner_latched", Reason: latchCause, Detail: latchDetail})
	}
	if unproven != a.unprovenSeen {
		a.unprovenSeen = unproven
		if unproven != "" {
			out = append(out, feedSharedEvent{Kind: "reserve_unproven", Detail: unproven})
		}
	}
	if cur != nil && cur.EpochID != a.reportedStart {
		if a.reportedStart != "" && a.reportedStart != a.reportedEnd {
			a.reportedEnd = a.reportedStart
			out = append(out, feedSharedEvent{Kind: "recovery_ended", Reason: "superseded by a later recovery", Detail: latched})
		}
		a.reportedStart = cur.EpochID
		out = append(out, feedSharedEvent{Kind: "recovery_hold", Generation: cur.Generation, Reason: cur.Reason})
	}
	if cur == nil && a.reportedStart != "" && a.reportedStart != a.reportedEnd {
		a.reportedEnd = a.reportedStart
		ev := feedSharedEvent{Kind: "recovery_ended", Detail: latched}
		if last != nil {
			ev.Generation, ev.Reason = last.Generation, last.Reason
		}
		out = append(out, ev)
	}
	if a.outageReported && a.outageDetail != "" {
		out = append(out, feedSharedEvent{Kind: "storage_outage", Reason: a.outageCause, Detail: a.outageDetail})
		a.outageDetail = ""
	}
	if a.outageCleared {
		a.outageCleared = false
		out = append(out, feedSharedEvent{Kind: "storage_restored"})
	}
	return out
}

func (ev feedSharedEvent) logLine() string {
	return fmt.Sprintf("status=%s generation=%d reason=%s detail=%q", ev.Kind, ev.Generation, ev.Reason, ev.Detail)
}

func formatFeedSharedAlert(ev feedSharedEvent, source, instance, key string) string {
	switch ev.Kind {
	case "recovery_hold":
		return fmt.Sprintf("**MARKET FEED SHARED WEIGHT HOLD** [%s] shared weight recovery generation %d (%s) holds every participating venue read on this host for at least %s; consumers hold entries on unready inputs, protection continues on verified inputs, and reads recover by themselves (owner %s, instance %s).",
			source, ev.Generation, ev.Reason, feedBudgetWindow, key, instance)
	case "recovery_ended":
		if ev.Detail != "" {
			return fmt.Sprintf("**MARKET FEED SHARED WEIGHT RECOVERED** [%s] shared weight recovery generation %d (%s) ended for the host, but this owner stays latched closed (%s) and refuses every participating venue read until it restarts (owner %s, instance %s).",
				source, ev.Generation, ev.Reason, ev.Detail, key, instance)
		}
		return fmt.Sprintf("**MARKET FEED SHARED WEIGHT RECOVERED** [%s] shared weight recovery generation %d (%s) ended; participating reads resume through the normal eligible fetches (owner %s, instance %s).",
			source, ev.Generation, ev.Reason, key, instance)
	case "reserve_unproven":
		return fmt.Sprintf("**MARKET FEED SHARED WEIGHT RESERVE UNPROVEN** [%s] the held weight reserve of owner(s) %s cannot be proved from the recovery evidence, so every participating venue read on this host is held; restart each such owner with its config or, once it is confirmed stopped, run go-trader feed-weight-release --owner <key> as the service user; if the release reports a missing lifetime lock, first confirm no process runs with that key, create owners/<key>.lock with mode 0600 as the service user, then rerun the release (owner %s, instance %s).",
			source, ev.Detail, key, instance)
	case "owner_latched":
		return fmt.Sprintf("**MARKET FEED SHARED WEIGHT OWNER LATCHED** [%s] this owner latched its shared sends closed (%s: %s); every participating venue read of this owner is refused until it restarts, consumers hold entries on unready inputs and protection continues on verified inputs; check the shared weight directory, then restart this owner (owner %s, instance %s).",
			source, ev.Reason, ev.Detail, key, instance)
	case "storage_outage":
		switch ev.Reason {
		case feedWeightCauseSharedClock:
			return fmt.Sprintf("**MARKET FEED SHARED WEIGHT CLOCK FAULT** [%s] the boot clock cannot be read (%s); every participating venue read of this owner is refused until the clock reads again (owner %s, instance %s).",
				source, ev.Detail, key, instance)
		case feedWeightCauseSharedVersion:
			return fmt.Sprintf("**MARKET FEED SHARED WEIGHT UNSUPPORTED RECORD** [%s] the shared weight record has a version this binary does not know (%s); every participating venue read of this owner is refused until a binary that knows that version runs (owner %s, instance %s).",
				source, ev.Detail, key, instance)
		}
		return fmt.Sprintf("**MARKET FEED SHARED WEIGHT STORAGE FAULT** [%s] the shared weight record cannot be used (%s); every participating venue read of this owner is refused until storage works again (owner %s, instance %s).",
			source, ev.Detail, key, instance)
	}
	return fmt.Sprintf("**MARKET FEED SHARED WEIGHT STORAGE RESTORED** [%s] the shared weight record works again (owner %s, instance %s).", source, key, instance)
}

type feedAuthorityForecast struct {
	Lines  []string
	Refuse string
}

func forecastFeedAuthority(role, identityPath string, policy *feedWeightPolicy) feedAuthorityForecast {
	dir := feedAuthorityDir()
	key := feedAuthorityOwnerKey(role, identityPath)
	cand := feedAuthPolicyOf(policy)
	out := feedAuthorityForecast{}
	add := func(format string, a ...any) { out.Lines = append(out.Lines, fmt.Sprintf(format, a...)) }
	add("shared weight: owner=%s role=%s identity=%s directory=%s capability=%s record_version=%d", key, role, resolvedConfigPath(identityPath), dir, feedAuthorityCapability, feedAuthorityVersion)
	if _, err := readFeedBoot(newFeedBootClock()); err != nil {
		out.Refuse = fmt.Sprintf("the boot clock is unavailable (%s): %v", feedBootClockSource, err)
		return out
	}
	view := newFeedAuthRecord()
	state := "ok"
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		state = "initialization required"
	case err != nil:
		out.Refuse = fmt.Sprintf("shared weight directory %s: %v", dir, err)
		return out
	case !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0:
		out.Refuse = fmt.Sprintf("%s is not a real directory", dir)
		return out
	default:
		rec, load, detail, lr := loadFeedAuthRecord(filepath.Join(dir, feedAuthorityRecordName), true)
		if lr != nil {
			out.Refuse = lr.Detail
			return out
		}
		switch load {
		case feedAuthLoadOK:
			view = rec
			if rec.Recovery != nil {
				state = fmt.Sprintf("recovery generation %d (%s) in progress", rec.Recovery.Generation, rec.Recovery.Reason)
			}
		case feedAuthLoadLegacy:
			view = rec
			if _, merr := feedMigrateRows(view, dir, true, 0); merr != nil {
				out.Refuse = fmt.Sprintf("the version %d record cannot be migrated: %v", feedAuthorityLegacyVersion, merr)
				return out
			}
			state = fmt.Sprintf("migration required (record version %d)", feedAuthorityLegacyVersion)
		default:
			if load == feedAuthLoadAbsent {
				state = "recovery required (record missing)"
			} else {
				state = fmt.Sprintf("recovery required (record corrupt: %s)", detail)
			}
			if load == feedAuthLoadAbsent && feedAuthorityDirEmpty(dir) {
				state = "initialization required"
			}
			rb, rerr := feedRebuildRows(dir, key, true, 0)
			if rerr != nil {
				out.Refuse = fmt.Sprintf("the recovery evidence in %s cannot be read: %v", dir, rerr)
				return out
			}
			view.Owners = rb.Rows
			for _, k := range rb.Unknown {
				out.Refuse = fmt.Sprintf("the liveness or policy of owner %s cannot be read from the recovery evidence", k)
				return out
			}
		}
	}
	peerState := map[string]string{}
	for _, k := range view.sortedOwnerKeys() {
		o := view.Owners[k]
		if k == key {
			continue
		}
		if o.Unproven {
			out.Refuse = fmt.Sprintf("the held weight reserve of owner %s cannot be proved from the recovery evidence; restart that owner with its config or, once it is confirmed stopped, run go-trader feed-weight-release --owner %s", k, k)
			return out
		}
		if !o.Active {
			peerState[k] = "stopped (reserve held)"
			continue
		}
		live, why := feedProbeOwnerLock(dir, k, true)
		switch live {
		case feedOwnerDead:
			o.Active = false
			peerState[k] = "stopped (allocation removed at startup, reserve held)"
		case feedOwnerUnknown:
			out.Refuse = fmt.Sprintf("the liveness of owner %s cannot be read: %s", k, why)
			return out
		default:
			peerState[k] = "live"
		}
	}
	res := feedAdmissionOf(view, key, cand, nil)
	add("shared weight: state=%s candidate_allocation=%d candidate_reserve=%d combined_allocation=%d common_reserve=%d reserve_owner=%s capacity=%d", state, cand.allocation(), cand.Reserve, res.Combined, res.Reserve, res.ReserveOwner, res.Capacity)
	for _, k := range feedSortedKeys(feedKeySet(peerState)) {
		o := view.Owners[k]
		alloc := int64(0)
		if o.Active {
			alloc = o.Policy.allocation()
		}
		add("shared weight: peer owner=%s state=%s allocation=%d reserve=%d", k, peerState[k], alloc, o.Policy.Reserve)
	}
	if res.Combined > res.Capacity {
		out.Refuse = fmt.Sprintf("combined weight allocation %d exceeds the %d venue weight ceiling less the common reserve %d held by owner %s (at most %d)", res.Combined, hlRESTWeightCeiling, res.Reserve, res.ReserveOwner, res.Capacity)
	}
	return out
}

func formatFeedSharedFields(sh feedSharedStatus) string {
	if sh.Coverage != feedSharedCoverageShared {
		return fmt.Sprintf("shared_coverage=%q", sh.Coverage)
	}
	recovery := "none"
	if sh.Recovery != nil {
		recovery = fmt.Sprintf("%d:%s:until=%s", sh.Recovery.Generation, sh.Recovery.Reason, sh.Recovery.HoldUntil)
	}
	hosts := make([]string, 0, len(sh.Hosts))
	for _, h := range sh.Hosts {
		part := fmt.Sprintf("%s:used=%d:pause=%s", h.Host, h.Used, h.Pause)
		if h.Shortfall > 0 {
			part += fmt.Sprintf(":shortfall=%d", h.Shortfall)
		}
		if h.PausedUntil != "" {
			part += ":until=" + h.PausedUntil
		}
		hosts = append(hosts, part)
	}
	return fmt.Sprintf("shared_coverage=shared shared_registry=%s shared_owner=%s shared_allocation=%d shared_common_reserve=%d shared_combined_allocation=%d shared_capacity=%d shared_pending=%d shared_in_flight=%d shared_charged=%d shared_debt=%d shared_stopped_owner_debt=%d shared_hosts=%s shared_recovery=%s shared_owners=%d shared_unproven=%s shared_last_refusal=%q",
		sh.Registry, sh.OwnerKey, sh.Allocation, sh.CommonReserve, sh.CombinedAllocation, sh.Capacity, sh.Pending, sh.InFlight, sh.Charged, sh.Debt,
		sh.StoppedOwnerDebt, joinFeedNames(hosts), recovery, len(sh.Owners), joinFeedNames(sh.UnprovenOwners), sh.LastRefusal)
}

func feedSharedCycleLine(l *feedRequestLedger) string {
	if l == nil || l.sharedAuthority() == nil {
		return ""
	}
	return formatFeedSharedFields(l.weightStatus().Shared)
}

func printFeedAuthorityForecast(role, identity string, policy *feedWeightPolicy) bool {
	fc := forecastFeedAuthority(role, identity, policy)
	for _, line := range fc.Lines {
		fmt.Printf("probe: %s\n", line)
	}
	if fc.Refuse != "" {
		fmt.Fprintf(os.Stderr, "probe: shared weight refused: %s\n", fc.Refuse)
		return false
	}
	return true
}
