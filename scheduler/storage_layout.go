package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type storageRole string

const (
	storageRolePrimary storageRole = "primary"
	storageRolePaper   storageRole = "paper"
)

// paperSourceRole names the physical file of one folded paper deployment. The
// separator is outside the source id pattern, so a source role can never spell
// the primary or the default paper role.
func paperSourceRole(id string) storageRole {
	return storageRole(string(ScopePaper) + paperSourceSeparator + id)
}

type storageFileSpec struct {
	Role      storageRole
	Path      string
	Canonical string
	InMemory  bool
	// Partitions lists every risk partition this physical file owns. One file
	// owns at most one partition per scope, so the stored scope column stays
	// 'live' or 'paper' exactly as a standalone deployment wrote it.
	Partitions []RiskPartition
}

type storageLayout struct {
	Split bool
	Files []storageFileSpec
}

func (l storageLayout) spec(role storageRole) (storageFileSpec, bool) {
	for _, f := range l.Files {
		if f.Role == role {
			return f, true
		}
	}
	return storageFileSpec{}, false
}

func (l storageLayout) roles() []storageRole {
	out := make([]storageRole, 0, len(l.Files))
	for _, f := range l.Files {
		out = append(out, f.Role)
	}
	return out
}

// roleForPartition resolves the file that owns one partition. An unowned
// partition falls back to the primary, which is the single-file behavior.
func (l storageLayout) roleForPartition(p RiskPartition) storageRole {
	for _, f := range l.Files {
		for _, owned := range f.Partitions {
			if owned == p {
				return f.Role
			}
		}
	}
	return storageRolePrimary
}

func (l storageLayout) partitionsForRole(role storageRole) []RiskPartition {
	if spec, ok := l.spec(role); ok && len(spec.Partitions) > 0 {
		return spec.Partitions
	}
	if !l.Split && role == storageRolePrimary {
		return []RiskPartition{livePartition, defaultPaperPartition}
	}
	return nil
}

// scopesForRole is the per-file SQL filter: the distinct execution modes the
// file's partitions cover, in the stable order live then paper.
func (l storageLayout) scopesForRole(role storageRole) []PortfolioScope {
	parts := l.partitionsForRole(role)
	if len(parts) == 0 {
		return []PortfolioScope{ScopeLive, ScopePaper}
	}
	hasLive := false
	hasPaper := false
	for _, p := range parts {
		if p.Scope == ScopeLive {
			hasLive = true
		} else if p.Scope == ScopePaper {
			hasPaper = true
		}
	}
	out := make([]PortfolioScope, 0, 2)
	if hasLive {
		out = append(out, ScopeLive)
	}
	if hasPaper {
		out = append(out, ScopePaper)
	}
	return out
}

// partitionForRoleScope maps one loaded row back onto its partition. A file
// owns at most one partition per scope, so the answer is unambiguous.
func (l storageLayout) partitionForRoleScope(role storageRole, scope PortfolioScope) (RiskPartition, bool) {
	for _, p := range l.partitionsForRole(role) {
		if p.Scope == scope {
			return p, true
		}
	}
	return unassignedPartition, false
}

func (l storageLayout) ownsAllScopes(role storageRole) bool {
	return !l.Split && role == storageRolePrimary
}

func (l storageLayout) describe() string {
	primary, _ := l.spec(storageRolePrimary)
	if !l.Split {
		return fmt.Sprintf("single-file (primary=%s)", primary.Path)
	}
	parts := make([]string, 0, len(l.Files))
	for _, f := range l.Files {
		parts = append(parts, fmt.Sprintf("%s=%s", f.Role, f.Path))
	}
	return fmt.Sprintf("split (%s)", strings.Join(parts, ", "))
}

func canonicalStoragePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", path, err)
	}
	cur := abs
	rest := ""
	for {
		eval, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if rest == "" {
				return eval, nil
			}
			return filepath.Join(eval, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("resolve %q: %w", path, err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

func validateStorageParentDir(spec storageFileSpec) error {
	if spec.InMemory {
		return nil
	}
	if info, err := os.Stat(spec.Canonical); err == nil {
		if info.IsDir() {
			return fmt.Errorf("%s state file %q is a directory", spec.Role, spec.Path)
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat %s state file %q: %w", spec.Role, spec.Path, err)
	}
	dir := filepath.Dir(spec.Canonical)
	for {
		info, err := os.Stat(dir)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s state file %q cannot be created: %q is not a directory", spec.Role, spec.Path, dir)
			}
			return nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("stat parent of %s state file %q: %w", spec.Role, spec.Path, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

func newStorageFileSpec(role storageRole, path string, partitions ...RiskPartition) (storageFileSpec, error) {
	spec := storageFileSpec{Role: role, Path: path, InMemory: isInMemoryDBPath(path), Partitions: partitions}
	if spec.InMemory {
		spec.Canonical = path
		return spec, nil
	}
	canonical, err := canonicalStoragePath(path)
	if err != nil {
		return storageFileSpec{}, err
	}
	spec.Canonical = canonical
	if err := validateStorageParentDir(spec); err != nil {
		return storageFileSpec{}, err
	}
	return spec, nil
}

func sameStorageFile(a, b storageFileSpec) (bool, error) {
	if a.InMemory || b.InMemory {
		return false, nil
	}
	if a.Canonical == b.Canonical {
		return true, nil
	}
	ai, err := os.Stat(a.Canonical)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat %q: %w", a.Path, err)
	}
	bi, err := os.Stat(b.Canonical)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat %q: %w", b.Path, err)
	}
	return os.SameFile(ai, bi), nil
}

func resolveStorageLayout(cfg *Config) (storageLayout, error) {
	if cfg == nil {
		return storageLayout{}, fmt.Errorf("nil config")
	}
	primaryPath := strings.TrimSpace(cfg.DBFile)
	if primaryPath == "" {
		return storageLayout{}, fmt.Errorf("db_file is empty")
	}
	paperPath := strings.TrimSpace(cfg.PaperDBFile)
	primaryPartitions := []RiskPartition{livePartition}
	if paperPath == "" {
		primaryPartitions = append(primaryPartitions, defaultPaperPartition)
	}
	primary, err := newStorageFileSpec(storageRolePrimary, primaryPath, primaryPartitions...)
	if err != nil {
		return storageLayout{}, err
	}
	files := []storageFileSpec{primary}
	if paperPath != "" {
		paper, err := newStorageFileSpec(storageRolePaper, paperPath, defaultPaperPartition)
		if err != nil {
			return storageLayout{}, err
		}
		files = append(files, paper)
	}
	for _, src := range sortedPaperSources(cfg.PaperSources) {
		path := strings.TrimSpace(src.DBFile)
		if path == "" {
			return storageLayout{}, fmt.Errorf("paper_sources[%s].db_file is empty", src.ID)
		}
		spec, err := newStorageFileSpec(paperSourceRole(src.ID), path, paperSourcePartition(src.ID))
		if err != nil {
			return storageLayout{}, err
		}
		files = append(files, spec)
	}
	// Every pair must be a distinct physical file: two partitions sharing one
	// file would give one scope column two owners and silently merge books.
	for i := 0; i < len(files); i++ {
		for j := i + 1; j < len(files); j++ {
			same, err := sameStorageFile(files[i], files[j])
			if err != nil {
				return storageLayout{}, err
			}
			if same {
				return storageLayout{}, fmt.Errorf("%s state file %q and %s state file %q resolve to the same physical file (%s); every partition needs its own file",
					files[j].Role, files[j].Path, files[i].Role, files[i].Path, files[i].Canonical)
			}
		}
	}
	if len(files) == 1 {
		return storageLayout{Files: files}, nil
	}
	return storageLayout{Split: true, Files: files}, nil
}

func sortedPaperSources(sources []PaperSourceConfig) []PaperSourceConfig {
	out := append([]PaperSourceConfig(nil), sources...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// storageRoleForPartitionInConfig answers from config alone, before any file is
// resolved, so identity validation runs without touching the filesystem.
func storageRoleForPartitionInConfig(cfg *Config, p RiskPartition) storageRole {
	if p.Source != "" {
		return paperSourceRole(p.Source)
	}
	if cfg == nil || strings.TrimSpace(cfg.PaperDBFile) == "" {
		return storageRolePrimary
	}
	if p.Scope == ScopePaper {
		return storageRolePaper
	}
	return storageRolePrimary
}

func effectiveStorageStrategyID(sc StrategyConfig) string {
	if id := strings.TrimSpace(sc.StorageStrategyID); id != "" {
		return id
	}
	return sc.ID
}

func validateStorageIdentityConfig(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	seen := make(map[storageKey]string, len(cfg.Strategies))
	for i, sc := range cfg.Strategies {
		prefix := fmt.Sprintf("strategy[%d]", i)
		if sc.ID != "" {
			prefix = fmt.Sprintf("strategy[%s]", sc.ID)
		}
		if sc.StorageStrategyID != "" && strings.TrimSpace(sc.StorageStrategyID) == "" {
			errs = append(errs, fmt.Sprintf("%s: storage_strategy_id is blank; omit it to default to id", prefix))
			continue
		}
		storageID := effectiveStorageStrategyID(sc)
		if storageID == "" {
			continue
		}
		role := storageRoleForPartitionInConfig(cfg, partitionFor(sc))
		key := storageKey{Role: role, ID: storageID}
		if prev, ok := seen[key]; ok {
			errs = append(errs, fmt.Sprintf("%s: storage_strategy_id %q already used by strategy %q in the %s state file; storage identity must be unique per file",
				prefix, storageID, prev, role))
			continue
		}
		seen[key] = sc.ID
	}
	return errs
}

func storageIdentityReloadErrors(cfg, next *Config) []string {
	if cfg == nil || next == nil {
		return nil
	}
	prev := make(map[string]string, len(cfg.Strategies))
	for _, sc := range cfg.Strategies {
		if sc.ID != "" {
			prev[sc.ID] = effectiveStorageStrategyID(sc)
		}
	}
	var errs []string
	for _, sc := range next.Strategies {
		if sc.ID == "" {
			continue
		}
		before, ok := prev[sc.ID]
		if !ok {
			continue
		}
		after := effectiveStorageStrategyID(sc)
		if before != after {
			errs = append(errs, fmt.Sprintf("strategy[%s]: storage_strategy_id changed (%q -> %q; restart required)", sc.ID, before, after))
		}
	}
	return errs
}
