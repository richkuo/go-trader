package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type storageRole string

const (
	storageRolePrimary storageRole = "primary"
	storageRolePaper   storageRole = "paper"
)

var storageRoleOrder = []storageRole{storageRolePrimary, storageRolePaper}

type storageFileSpec struct {
	Role      storageRole
	Path      string
	Canonical string
	InMemory  bool
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

func (l storageLayout) roleForScope(scope PortfolioScope) storageRole {
	if l.Split && scope == ScopePaper {
		return storageRolePaper
	}
	return storageRolePrimary
}

func (l storageLayout) scopesForRole(role storageRole) []PortfolioScope {
	if !l.Split {
		return []PortfolioScope{ScopeLive, ScopePaper}
	}
	if role == storageRolePaper {
		return []PortfolioScope{ScopePaper}
	}
	return []PortfolioScope{ScopeLive}
}

func (l storageLayout) ownsAllScopes(role storageRole) bool {
	return !l.Split && role == storageRolePrimary
}

func (l storageLayout) describe() string {
	primary, _ := l.spec(storageRolePrimary)
	if !l.Split {
		return fmt.Sprintf("single-file (primary=%s)", primary.Path)
	}
	paper, _ := l.spec(storageRolePaper)
	return fmt.Sprintf("split (primary=%s, paper=%s)", primary.Path, paper.Path)
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

func newStorageFileSpec(role storageRole, path string) (storageFileSpec, error) {
	spec := storageFileSpec{Role: role, Path: path, InMemory: isInMemoryDBPath(path)}
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
	primary, err := newStorageFileSpec(storageRolePrimary, primaryPath)
	if err != nil {
		return storageLayout{}, err
	}
	paperPath := strings.TrimSpace(cfg.PaperDBFile)
	if paperPath == "" {
		return storageLayout{Files: []storageFileSpec{primary}}, nil
	}
	paper, err := newStorageFileSpec(storageRolePaper, paperPath)
	if err != nil {
		return storageLayout{}, err
	}
	same, err := sameStorageFile(primary, paper)
	if err != nil {
		return storageLayout{}, err
	}
	if same {
		return storageLayout{}, fmt.Errorf("paper_db_file %q and db_file %q resolve to the same physical file (%s); the two scopes need distinct files",
			paper.Path, primary.Path, primary.Canonical)
	}
	return storageLayout{Split: true, Files: []storageFileSpec{primary, paper}}, nil
}

func storageRoleForScopeInConfig(cfg *Config, scope PortfolioScope) storageRole {
	if cfg == nil || strings.TrimSpace(cfg.PaperDBFile) == "" {
		return storageRolePrimary
	}
	if scope == ScopePaper {
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
		role := storageRoleForScopeInConfig(cfg, portfolioScopeFor(sc))
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
