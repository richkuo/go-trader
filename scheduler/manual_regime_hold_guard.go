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
	path  string
	part  RiskPartition
	f     *os.File
	clear bool
}

type manualRegimeHoldGuards struct {
	mu       sync.Mutex
	nonce    string
	guards   map[RiskPartition]*manualRegimeHoldGuard
	order    []RiskPartition
	draining bool
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
		if guard.clear {
			syscall.Flock(int(guard.f.Fd()), syscall.LOCK_UN)
			guard.clear = false
		}
		return
	}
	if guard.clear {
		return
	}
	if err := syscall.Flock(int(guard.f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
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
		if guard := g.guards[p]; guard != nil && guard.f != nil && guard.clear {
			syscall.Flock(int(guard.f.Fd()), syscall.LOCK_UN)
			guard.clear = false
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
		if guard.clear {
			syscall.Flock(int(guard.f.Fd()), syscall.LOCK_UN)
			guard.clear = false
		}
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
