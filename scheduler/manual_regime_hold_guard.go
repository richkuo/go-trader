package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
)

const (
	manualRegimeHoldGuardInfix    = ".manual-regime-hold-"
	manualRegimeHoldGuardExt      = ".json"
	manualRegimeHoldGuardVersion  = 1
	manualRegimeHoldGuardMaxBytes = 4096
)

type manualRegimeHoldGuardMarker struct {
	Version    int    `json:"version"`
	OwnerNonce string `json:"owner_nonce"`
	Partition  string `json:"partition"`
}

func manualRegimeHoldGuardPath(dbPath string, p RiskPartition) string {
	return canonicalDBPath(dbPath) + manualRegimeHoldGuardInfix + strings.ReplaceAll(p.String(), ":", "-") + manualRegimeHoldGuardExt
}

type manualRegimeHoldGuard struct {
	path    string
	part    RiskPartition
	f       *os.File
	clear   bool
	witness *manualRegimeWitnessFile
	offset  int64
}

type manualRegimeHoldGuards struct {
	mu           sync.Mutex
	nonce        string
	guards       map[RiskPartition]*manualRegimeHoldGuard
	order        []RiskPartition
	draining     bool
	witnesses    map[string]*manualRegimeWitnessFile
	witnessOrder []string
}

const (
	manualRegimeOwnershipWitnessOffset = 0
	manualRegimeWitnessSuffix          = ".manual-regime-hold-witness.json"
)

func manualRegimeWitnessPath(dbPath string) string {
	return canonicalDBPath(dbPath) + manualRegimeWitnessSuffix
}

func openManualRegimeWitnessFile(path string) (*os.File, error) {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		f, cerr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
		if cerr == nil || !errors.Is(cerr, fs.ErrExist) {
			return f, cerr
		}
	} else if err != nil {
		return nil, err
	}
	return openManualRegimeHoldGuard(path, os.O_RDWR)
}

var errManualRegimeNoWitness = errors.New("no witness record lock")

type manualRegimeWitnessFile struct {
	path string
	f    *os.File
	held map[int64]bool
}

func manualRegimeRecordLock(f *os.File, offset int64, typ int16) error {
	lk := syscall.Flock_t{Type: typ, Whence: io.SeekStart, Start: offset, Len: 1}
	return syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lk)
}

func (w *manualRegimeWitnessFile) set(offset int64) error {
	if w == nil || w.f == nil {
		return fmt.Errorf("witness file is closed")
	}
	if err := manualRegimeRecordLock(w.f, offset, syscall.F_WRLCK); err != nil {
		return fmt.Errorf("witness %s at byte %d: %w", w.path, offset, err)
	}
	w.held[offset] = true
	return nil
}

func (w *manualRegimeWitnessFile) drop(offset int64) {
	if w == nil || w.f == nil || !w.held[offset] {
		return
	}
	if err := manualRegimeRecordLock(w.f, offset, syscall.F_UNLCK); err != nil {
		fmt.Printf("[manual-regime] CRITICAL: could not drop the witness %s at byte %d (%v); closing the file withdraws every witness it carried\n", w.path, offset, err)
		w.close()
		return
	}
	delete(w.held, offset)
}

func (w *manualRegimeWitnessFile) close() {
	if w == nil {
		return
	}
	if w.f != nil {
		w.f.Close()
		w.f = nil
	}
	w.held = map[int64]bool{}
}

func manualRegimeWitnessSlot(layout storageLayout, p RiskPartition) (storageRole, int64, bool) {
	role := layout.roleForPartition(p)
	names := make([]string, 0, 2)
	for _, q := range layout.partitionsForRole(role) {
		names = append(names, q.String())
	}
	sort.Strings(names)
	for i, name := range names {
		if name == p.String() {
			return role, int64(1 + i), true
		}
	}
	return role, 0, false
}

func (g *manualRegimeHoldGuards) armWitnesses(layout storageLayout) error {
	if g == nil {
		return fmt.Errorf("no persistence guards")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.witnesses = make(map[string]*manualRegimeWitnessFile)
	for _, spec := range layout.Files {
		if spec.InMemory {
			continue
		}
		path := manualRegimeWitnessPath(spec.Path)
		if _, dup := g.witnesses[path]; dup {
			continue
		}
		f, err := openManualRegimeWitnessFile(path)
		if err != nil {
			return fmt.Errorf("witness file %s: %w", path, err)
		}
		w := &manualRegimeWitnessFile{path: path, f: f, held: map[int64]bool{}}
		g.witnesses[path] = w
		g.witnessOrder = append(g.witnessOrder, path)
	}
	for _, p := range g.order {
		guard := g.guards[p]
		role, offset, ok := manualRegimeWitnessSlot(layout, p)
		spec, found := layout.spec(role)
		if !ok || !found || spec.InMemory {
			return fmt.Errorf("no on-disk state file carries the %s partition witness", partitionLabel(p))
		}
		w := g.witnesses[manualRegimeWitnessPath(spec.Path)]
		if w == nil {
			return fmt.Errorf("no witness file carries the %s partition witness", partitionLabel(p))
		}
		guard.witness, guard.offset = w, offset
		if guard.clear {
			if err := w.set(offset); err != nil {
				return err
			}
		}
	}
	for _, path := range g.witnessOrder {
		if err := g.witnesses[path].set(manualRegimeOwnershipWitnessOffset); err != nil {
			return err
		}
	}
	return nil
}

func (g *manualRegimeHoldGuards) witnessHeld(path string, offset int64) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	w := g.witnesses[path]
	return w != nil && w.f != nil && w.held[offset]
}

func (g *manualRegimeHoldGuard) holdLocked() {
	if !g.clear {
		return
	}
	g.witness.drop(g.offset)
	syscall.Flock(int(g.f.Fd()), syscall.LOCK_UN)
	g.clear = false
}

func readManualRegimeHoldGuardMarker(f *os.File) (manualRegimeHoldGuardMarker, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return manualRegimeHoldGuardMarker{}, err
	}
	blob, err := io.ReadAll(io.LimitReader(f, manualRegimeHoldGuardMaxBytes+1))
	if err != nil {
		return manualRegimeHoldGuardMarker{}, err
	}
	if len(blob) > manualRegimeHoldGuardMaxBytes {
		return manualRegimeHoldGuardMarker{}, fmt.Errorf("exceeds %d bytes", manualRegimeHoldGuardMaxBytes)
	}
	var marker manualRegimeHoldGuardMarker
	if err := decodeManualRegimeStrict(blob, &marker); err != nil {
		return manualRegimeHoldGuardMarker{}, err
	}
	if marker.Version != manualRegimeHoldGuardVersion || !validManualRegimeNonce(marker.OwnerNonce) || marker.Partition == "" {
		return manualRegimeHoldGuardMarker{}, fmt.Errorf("malformed")
	}
	return marker, nil
}

func openManualRegimeHoldGuard(path string, flag int) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (%s)", path, info.Mode().Type())
	}
	f, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	held, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !held.Mode().IsRegular() || !os.SameFile(info, held) {
		f.Close()
		return nil, fmt.Errorf("%s was replaced while it was opened", path)
	}
	return f, nil
}

func prepareManualRegimeHoldGuards(layout storageLayout, nonce string) (*manualRegimeHoldGuards, error) {
	g := &manualRegimeHoldGuards{nonce: nonce, guards: make(map[RiskPartition]*manualRegimeHoldGuard)}
	for _, spec := range layout.Files {
		if spec.InMemory {
			continue
		}
		for _, part := range layout.partitionsForRole(spec.Role) {
			if _, dup := g.guards[part]; dup {
				continue
			}
			path := manualRegimeHoldGuardPath(spec.Path, part)
			if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
				g.release()
				return nil, fmt.Errorf("persistence guard %s is not a regular file (%s)", path, info.Mode().Type())
			} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
				g.release()
				return nil, fmt.Errorf("persistence guard %s unreadable: %w", path, err)
			}
			if err := writeManualRegimeJSONFile(path, manualRegimeHoldGuardMarker{Version: manualRegimeHoldGuardVersion, OwnerNonce: nonce, Partition: part.String()}); err != nil {
				g.release()
				return nil, fmt.Errorf("persistence guard %s: %w", path, err)
			}
			f, err := openManualRegimeHoldGuard(path, os.O_RDWR)
			if err != nil {
				g.release()
				return nil, fmt.Errorf("persistence guard %s: %w", path, err)
			}
			guard := &manualRegimeHoldGuard{path: path, part: part, f: f}
			g.guards[part] = guard
			g.order = append(g.order, part)
			if marker, err := readManualRegimeHoldGuardMarker(f); err != nil || marker.OwnerNonce != nonce || marker.Partition != part.String() {
				g.release()
				return nil, fmt.Errorf("persistence guard %s did not read back after publication", path)
			}
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				g.release()
				return nil, fmt.Errorf("persistence guard %s: lock: %w", path, err)
			}
			guard.clear = true
		}
	}
	sort.Slice(g.order, func(i, j int) bool { return g.order[i].String() < g.order[j].String() })
	return g, nil
}

func (g *manualRegimeHoldGuards) signal(p RiskPartition, held bool) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	guard := g.guards[p]
	if guard == nil || guard.f == nil {
		return
	}
	if held || g.draining {
		guard.holdLocked()
		return
	}
	if guard.clear {
		return
	}
	if err := syscall.Flock(int(guard.f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Printf("[manual-regime] persistence guard %s stays held until the next successful %s save: %v\n", guard.path, partitionLabel(p), err)
		return
	}
	if err := guard.witness.set(guard.offset); err != nil {
		syscall.Flock(int(guard.f.Fd()), syscall.LOCK_UN)
		fmt.Printf("[manual-regime] persistence guard %s stays held until the next successful %s save: %v\n", guard.path, partitionLabel(p), err)
		return
	}
	guard.clear = true
}

func (g *manualRegimeHoldGuards) drain() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.draining = true
	for _, p := range g.order {
		if guard := g.guards[p]; guard != nil && guard.f != nil {
			guard.holdLocked()
		}
	}
}

func (g *manualRegimeHoldGuards) release() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.draining = true
	for _, guard := range g.guards {
		if guard.f == nil {
			continue
		}
		guard.holdLocked()
		if marker, err := readManualRegimeHoldGuardMarker(guard.f); err == nil && marker.OwnerNonce == g.nonce {
			if info, lerr := os.Lstat(guard.path); lerr == nil {
				if held, serr := guard.f.Stat(); serr == nil && os.SameFile(info, held) {
					os.Remove(guard.path)
				}
			}
		}
		guard.f.Close()
		guard.f = nil
	}
	for _, path := range g.witnessOrder {
		w := g.witnesses[path]
		var held os.FileInfo
		if w.f != nil {
			held, _ = w.f.Stat()
		}
		w.close()
		if held != nil {
			if current, err := os.Lstat(path); err == nil && os.SameFile(current, held) {
				os.Remove(path)
			}
		}
	}
}

func checkManualRegimeHoldGuard(layout storageLayout, p RiskPartition, nonce string) error {
	role := layout.roleForPartition(p)
	spec, ok := layout.spec(role)
	if !ok || spec.InMemory {
		return manualRegimeRefused("no on-disk state file owns the %s partition", partitionLabel(p))
	}
	path := manualRegimeHoldGuardPath(spec.Path, p)
	f, err := openManualRegimeHoldGuard(path, os.O_RDONLY)
	if errors.Is(err, fs.ErrNotExist) {
		return manualRegimeRefused("the owning scheduler published no persistence guard %s", path)
	}
	if err != nil {
		return manualRegimeRefused("persistence guard unreadable: %v", err)
	}
	defer f.Close()
	marker, err := readManualRegimeHoldGuardMarker(f)
	if err != nil {
		return manualRegimeRefused("persistence guard %s unreadable: %v", path, err)
	}
	if marker.OwnerNonce != nonce || marker.Partition != p.String() {
		return manualRegimeRefused("persistence guard %s belongs to a different scheduler process or partition", path)
	}
	flockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if flockErr == nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return manualRegimeRefused("the scheduler holds the %s partition: a state save failed and no later save succeeded, or the scheduler is draining", partitionLabel(p))
	}
	if !errors.Is(flockErr, syscall.EWOULDBLOCK) {
		return manualRegimeRefused("persistence guard %s unreadable: %v", path, flockErr)
	}
	after, err := os.Lstat(path)
	held, serr := f.Stat()
	if err != nil || serr != nil || !os.SameFile(after, held) {
		return manualRegimeRefused("persistence guard %s was replaced during the check", path)
	}
	return nil
}

func manualRegimeWitnessHolder(path string, want os.FileInfo, offset int64, nonce string) (int, os.FileInfo, error) {
	f, err := openManualRegimeHoldGuard(path, os.O_RDONLY)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil, errManualRegimeNoWitness
	}
	if err != nil {
		return 0, nil, fmt.Errorf("witness file %s unreadable: %v", path, err)
	}
	defer f.Close()
	held, err := f.Stat()
	if err != nil {
		return 0, nil, fmt.Errorf("witness file %s unreadable: %v", path, err)
	}
	if want != nil && !os.SameFile(held, want) {
		return 0, nil, fmt.Errorf("witness file %s was replaced", path)
	}
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart, Start: offset, Len: 1}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &lk); err != nil {
		return 0, nil, fmt.Errorf("witness file %s unreadable: %v", path, err)
	}
	if lk.Type == syscall.F_UNLCK {
		if globalManualRegimeOwner.localWitness(path, nonce, offset) {
			return os.Getpid(), held, nil
		}
		return 0, held, errManualRegimeNoWitness
	}
	if lk.Pid <= 0 {
		return 0, nil, fmt.Errorf("the process holding the witness on %s is not visible from this process", path)
	}
	return int(lk.Pid), held, nil
}

func checkManualRegimeHoldGuardWitness(pin manualRegimePin, p RiskPartition) error {
	role, offset, ok := manualRegimeWitnessSlot(pin.layout, p)
	spec, found := pin.layout.spec(role)
	if !ok || !found || spec.InMemory {
		return manualRegimeRefused("no on-disk state file owns the %s partition", partitionLabel(p))
	}
	want := pin.witnesses[role]
	if want == nil {
		return manualRegimeRefused("the pinned owner has no witness file for the %s partition", partitionLabel(p))
	}
	pid, _, err := manualRegimeWitnessHolder(manualRegimeWitnessPath(spec.Path), want, offset, pin.owner.Nonce)
	if errors.Is(err, errManualRegimeNoWitness) {
		return manualRegimeRefused("the scheduler holds the %s partition: it attests no clear persistence guard (a state save failed and no later save succeeded, the scheduler is draining, or the guard could not be locked again)", partitionLabel(p))
	}
	if err != nil {
		return manualRegimeRefused("%v", err)
	}
	if pid != pin.witness {
		return manualRegimeRefused("the %s persistence guard witness is held by process %d, not by the owner of the state files (process %d)", partitionLabel(p), pid, pin.witness)
	}
	return checkManualRegimeHoldGuard(pin.layout, p, pin.owner.Nonce)
}
