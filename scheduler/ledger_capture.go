package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	CaptureManifestVersion  = 1
	captureManifestKind     = "go-trader.ledger-capture"
	captureMethodVacuumInto = "vacuum_into"
	captureSourceDSNQuery   = "mode=ro&cache=private&readonly_shm=1"
	captureWorkerTarget     = "__capture-worker"
	captureConfigCopyName   = "config.json"
	captureStateDirName     = "state"
	captureMechanism        = "linux_private_mount_namespace_readonly_bind"
)

type captureConfinement struct {
	Mechanism            string   `json:"mechanism"`
	UserNamespace        bool     `json:"user_namespace"`
	ProtectedDirectories []string `json:"protected_directories"`
	SourceDSNParameters  string   `json:"source_dsn_parameters"`
}

type captureSourceConfig struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type captureFileRef struct {
	RelativePath string `json:"relative_path"`
	SHA256       string `json:"sha256"`
}

type captureManifestFile struct {
	SourceRole          string   `json:"source_role"`
	Partitions          []string `json:"partitions"`
	ConfigKey           string   `json:"config_key"`
	SourcePath          string   `json:"source_path"`
	SourceCanonicalPath string   `json:"source_canonical_path"`
	SourceJournalMode   string   `json:"source_journal_mode"`
	RelativePath        string   `json:"relative_path"`
	SHA256              string   `json:"sha256"`
	Size                int64    `json:"size"`
	CaptureStartedAt    string   `json:"capture_started_at"`
	CaptureCompletedAt  string   `json:"capture_completed_at"`
	IntegrityCheck      string   `json:"integrity_check"`
	JournalMode         string   `json:"journal_mode"`
	TradesRows          int64    `json:"trades_rows"`
	Attempts            int      `json:"attempts"`
}

type captureManifest struct {
	Manifest        string                `json:"manifest"`
	ManifestVersion int                   `json:"manifest_version"`
	StartedAt       string                `json:"started_at"`
	CompletedAt     string                `json:"completed_at"`
	SourceRevision  *string               `json:"source_revision"`
	CaptureMethod   string                `json:"capture_method"`
	Consistency     string                `json:"consistency"`
	Driver          string                `json:"driver"`
	SQLiteVersion   string                `json:"sqlite_version"`
	Confinement     captureConfinement    `json:"confinement"`
	SourceConfig    captureSourceConfig   `json:"source_config"`
	ConfigCopy      captureFileRef        `json:"config_copy"`
	Files           []captureManifestFile `json:"files"`
}

type captureWorkerFile struct {
	Role   string `json:"role"`
	Source string `json:"source"`
	Dev    uint64 `json:"dev"`
	Ino    uint64 `json:"ino"`
	Dest   string `json:"dest"`
}

type captureWorkerPlan struct {
	Protect        []string            `json:"protect"`
	DestinationDir string              `json:"destination_dir"`
	Files          []captureWorkerFile `json:"files"`
}

type captureWorkerResult struct {
	SQLiteVersion string               `json:"sqlite_version"`
	Driver        string               `json:"driver"`
	Files         []captureFileOutcome `json:"files"`
}

const ledgerCaptureUsage = "Usage: go-trader export capture --config <source-config> --output-dir <new-snapshot-directory>"

func runLedgerCapture(args []string) int {
	vals, err := parseLedgerOnceFlags("export capture", args, "config", "output-dir")
	if err != nil {
		fmt.Fprintf(os.Stderr, "export capture: %v\n%s\n", err, ledgerCaptureUsage)
		return 2
	}
	manifestPath, err := captureLedgerSnapshot(vals["config"], vals["output-dir"])
	if err != nil {
		fmt.Fprintf(os.Stderr, "export capture refused: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "Captured snapshot set; manifest %s\n", manifestPath)
	return 0
}

func runLedgerCaptureWorker(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "capture worker takes no arguments")
		return 2
	}
	res, err := ledgerCaptureWorkerMain(io.LimitReader(os.Stdin, 4<<20))
	if err != nil {
		fmt.Fprintf(os.Stderr, "capture worker refused: %v\n", err)
		return 1
	}
	if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
		fmt.Fprintf(os.Stderr, "capture worker: write result: %v\n", err)
		return 1
	}
	return 0
}

func captureFileName(role storageRole) string {
	switch role {
	case storageRolePrimary:
		return "primary.db"
	case storageRolePaper:
		return "paper.db"
	}
	id := strings.TrimPrefix(string(role), string(ScopePaper)+paperSourceSeparator)
	return "paper-source-" + id + ".db"
}

func captureConfigKey(role storageRole) string {
	switch role {
	case storageRolePrimary:
		return "db_file"
	case storageRolePaper:
		return "paper_db_file"
	}
	id := strings.TrimPrefix(string(role), string(ScopePaper)+paperSourceSeparator)
	return "paper_sources[" + id + "].db_file"
}

func readSourceConfigBytes(path string) (string, []byte, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, fmt.Errorf("resolve configuration %q: %w", path, err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", nil, fmt.Errorf("resolve configuration %q: %w", path, err)
	}
	f, err := os.OpenFile(canonical, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", nil, fmt.Errorf("open configuration %q: %w", canonical, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", nil, fmt.Errorf("stat configuration %q: %w", canonical, err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("configuration %q is not a regular file", canonical)
	}
	if info.Size() > ledgerMaxManifestBytes {
		return "", nil, fmt.Errorf("configuration %q is larger than %d bytes", canonical, ledgerMaxManifestBytes)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", nil, fmt.Errorf("read configuration %q: %w", canonical, err)
	}
	return canonical, data, nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type captureSourceFile struct {
	Spec  storageFileSpec
	Stamp ledgerFileStamp
}

func inspectCaptureSource(spec storageFileSpec) (captureSourceFile, error) {
	if spec.InMemory {
		return captureSourceFile{}, fmt.Errorf("%s state file %q is in memory; there is nothing on disk to capture", spec.Role, spec.Path)
	}
	info, err := os.Lstat(spec.Canonical)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return captureSourceFile{}, fmt.Errorf("configured %s state file %q is missing", spec.Role, spec.Canonical)
		}
		return captureSourceFile{}, fmt.Errorf("stat %s state file %q: %w", spec.Role, spec.Canonical, err)
	}
	if !info.Mode().IsRegular() {
		return captureSourceFile{}, fmt.Errorf("%s state file %q is not a regular file", spec.Role, spec.Canonical)
	}
	stamp, err := ledgerStampOf(info)
	if err != nil {
		return captureSourceFile{}, err
	}
	if stamp.Nlink != 1 {
		return captureSourceFile{}, fmt.Errorf("%s state file %q has %d hard links; an aliased source is refused", spec.Role, spec.Canonical, stamp.Nlink)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		side, err := os.Lstat(spec.Canonical + suffix)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return captureSourceFile{}, fmt.Errorf("stat %s sidecar %q: %w", spec.Role, spec.Canonical+suffix, err)
		}
		if !side.Mode().IsRegular() {
			return captureSourceFile{}, fmt.Errorf("%s sidecar %q is not a regular file", spec.Role, spec.Canonical+suffix)
		}
		sideStamp, err := ledgerStampOf(side)
		if err != nil {
			return captureSourceFile{}, err
		}
		if sideStamp.Nlink != 1 {
			return captureSourceFile{}, fmt.Errorf("%s sidecar %q has %d hard links", spec.Role, spec.Canonical+suffix, sideStamp.Nlink)
		}
	}
	return captureSourceFile{Spec: spec, Stamp: stamp}, nil
}

var captureConfigCredentialKeys = map[string][]string{
	"discord":  {"token", "report_github_token"},
	"telegram": {"bot_token"},
}

func withoutCaptureConfigCredentials(cfg Config) Config {
	cfg.Discord.Token = ""
	cfg.Discord.ReportGitHubToken = ""
	cfg.Telegram.BotToken = ""
	return cfg
}

func rewriteCaptureConfigStorage(data []byte, rel map[storageRole]string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("parse configuration for the copy: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("parse configuration for the copy: trailing data")
	}
	for section, keys := range captureConfigCredentialKeys {
		raw, ok := root[section]
		if !ok || raw == nil {
			continue
		}
		obj, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s is not an object", section)
		}
		for _, key := range keys {
			delete(obj, key)
		}
	}
	assigned := make(map[storageRole]bool)
	p, ok := rel[storageRolePrimary]
	if !ok {
		return nil, fmt.Errorf("no primary snapshot path")
	}
	root["db_file"] = p
	assigned[storageRolePrimary] = true
	if p, ok := rel[storageRolePaper]; ok {
		root["paper_db_file"] = p
		assigned[storageRolePaper] = true
	}
	if raw, ok := root["paper_sources"]; ok && raw != nil {
		arr, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("paper_sources is not an array")
		}
		for i, item := range arr {
			obj, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("paper_sources[%d] is not an object", i)
			}
			id, _ := obj["id"].(string)
			role := paperSourceRole(strings.TrimSpace(id))
			p, ok := rel[role]
			if !ok {
				return nil, fmt.Errorf("paper_sources[%d] (%q) has no snapshot file", i, id)
			}
			obj["db_file"] = p
			assigned[role] = true
		}
	}
	for role := range rel {
		if !assigned[role] {
			return nil, fmt.Errorf("the %s snapshot has no configuration key to point at it", role)
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("encode configuration copy: %w", err)
	}
	return buf.Bytes(), nil
}

func verifyCaptureConfigCopy(orig *Config, copyData []byte) error {
	copyCfg, err := LoadConfigForLedgerExport(copyData)
	if err != nil {
		return fmt.Errorf("the configuration copy does not load: %w", err)
	}
	copyCfg.DBFile = orig.DBFile
	copyCfg.PaperDBFile = orig.PaperDBFile
	if len(copyCfg.PaperSources) != len(orig.PaperSources) {
		return fmt.Errorf("the configuration copy changed paper_sources")
	}
	for i := range copyCfg.PaperSources {
		copyCfg.PaperSources[i].DBFile = orig.PaperSources[i].DBFile
	}
	a, err := json.Marshal(withoutCaptureConfigCredentials(*orig))
	if err != nil {
		return fmt.Errorf("encode source configuration: %w", err)
	}
	b, err := json.Marshal(withoutCaptureConfigCredentials(*copyCfg))
	if err != nil {
		return fmt.Errorf("encode configuration copy: %w", err)
	}
	if !bytes.Equal(a, b) {
		return fmt.Errorf("the configuration copy differs from the source configuration beyond storage paths")
	}
	return nil
}

func writeRootFileExclusive(root *os.Root, name string, data []byte) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

type captureCleanup struct {
	parent  *os.Root
	out     *os.Root
	name    string
	created []string
	dirs    []string
	madeDir bool
}

func (c *captureCleanup) run() {
	if c.out != nil {
		for i := len(c.created) - 1; i >= 0; i-- {
			for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
				c.out.Remove(c.created[i] + suffix)
			}
		}
		for i := len(c.dirs) - 1; i >= 0; i-- {
			c.out.Remove(c.dirs[i])
		}
		c.out.Close()
		c.out = nil
	}
	if c.parent != nil {
		if c.madeDir {
			if err := c.parent.Remove(c.name); err != nil && !errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "export capture: left %s in place after cleanup: %v\n", c.name, err)
			}
		}
		c.parent.Close()
		c.parent = nil
	}
}

func captureLedgerSnapshot(configPath, outputDir string) (string, error) {
	if err := ledgerCaptureConfinementAvailable(); err != nil {
		return "", err
	}
	startedAt := formatLedgerTimestamp(time.Now())
	configCanonical, configData, err := readSourceConfigBytes(configPath)
	if err != nil {
		return "", err
	}
	configSHA := sha256Hex(configData)
	cfg, err := LoadConfigForLedgerExport(configData)
	if err != nil {
		return "", fmt.Errorf("load source configuration: %w", err)
	}
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		return "", fmt.Errorf("resolve storage layout: %w", err)
	}
	if _, err := buildStorageIdentityMap(cfg, layout); err != nil {
		return "", fmt.Errorf("build storage identity map: %w", err)
	}
	sources := make([]captureSourceFile, 0, len(layout.Files))
	dirSet := make(map[string]bool)
	for _, spec := range layout.Files {
		src, err := inspectCaptureSource(spec)
		if err != nil {
			return "", err
		}
		sources = append(sources, src)
		dirSet[filepath.Dir(spec.Canonical)] = true
	}
	protect := make([]string, 0, len(dirSet))
	for d := range dirSet {
		protect = append(protect, d)
	}
	sort.Strings(protect)
	target, err := resolveLedgerOutputTarget(outputDir, append(append([]string{}, protect...), filepath.Dir(configCanonical)), "output directory")
	if err != nil {
		return "", err
	}

	cleanup := &captureCleanup{name: target.Name}
	success := false
	defer func() {
		if !success {
			cleanup.run()
		}
	}()
	cleanup.parent, err = target.openParent()
	if err != nil {
		return "", err
	}
	if err := cleanup.parent.Mkdir(target.Name, 0o700); err != nil {
		return "", fmt.Errorf("create output directory %q: %w", target.Final, err)
	}
	cleanup.madeDir = true
	cleanup.out, err = cleanup.parent.OpenRoot(target.Name)
	if err != nil {
		return "", fmt.Errorf("open output directory %q: %w", target.Final, err)
	}
	if err := cleanup.out.Mkdir(captureStateDirName, 0o700); err != nil {
		return "", fmt.Errorf("create %s/%s: %w", target.Final, captureStateDirName, err)
	}
	cleanup.dirs = append(cleanup.dirs, captureStateDirName)

	plan := captureWorkerPlan{Protect: protect, DestinationDir: filepath.Join(target.Final, captureStateDirName)}
	relPaths := make(map[storageRole]string, len(sources))
	for _, src := range sources {
		rel := captureStateDirName + "/" + captureFileName(src.Spec.Role)
		relPaths[src.Spec.Role] = rel
		cleanup.created = append(cleanup.created, rel)
		plan.Files = append(plan.Files, captureWorkerFile{
			Role:   string(src.Spec.Role),
			Source: src.Spec.Canonical,
			Dev:    src.Stamp.Dev,
			Ino:    src.Stamp.Ino,
			Dest:   filepath.Join(target.Final, filepath.FromSlash(rel)),
		})
	}
	result, userNS, err := runCaptureWorker(plan)
	if err != nil {
		return "", err
	}
	if len(result.Files) != len(sources) {
		return "", fmt.Errorf("capture worker reported %d files, want %d", len(result.Files), len(sources))
	}

	expected := map[string]bool{captureStateDirName + "/": true}
	for _, rel := range relPaths {
		expected[rel] = true
	}
	if err := ledgerSnapshotInventory(cleanup.out, expected); err != nil {
		return "", err
	}
	manifest := captureManifest{
		Manifest:        captureManifestKind,
		ManifestVersion: CaptureManifestVersion,
		StartedAt:       startedAt,
		SourceRevision:  ledgerVerifiedRevision(),
		CaptureMethod:   captureMethodVacuumInto,
		Consistency:     ledgerCaptureConsistency,
		Driver:          result.Driver,
		SQLiteVersion:   result.SQLiteVersion,
		Confinement: captureConfinement{
			Mechanism:            captureMechanism,
			UserNamespace:        userNS,
			ProtectedDirectories: protect,
			SourceDSNParameters:  captureSourceDSNQuery,
		},
		SourceConfig: captureSourceConfig{Path: configCanonical, SHA256: configSHA},
	}
	for i, src := range sources {
		outcome := result.Files[i]
		if outcome.Role != string(src.Spec.Role) {
			return "", fmt.Errorf("capture worker reported role %q at position %d, want %q", outcome.Role, i, src.Spec.Role)
		}
		rel := relPaths[src.Spec.Role]
		sum, stamp, header, err := hashRootFileVerified(cleanup.out, rel)
		if err != nil {
			return "", fmt.Errorf("read snapshot %s: %w", rel, err)
		}
		if err := checkRollbackSQLiteHeader(rel, header); err != nil {
			return "", err
		}
		manifest.Files = append(manifest.Files, captureManifestFile{
			SourceRole:          string(src.Spec.Role),
			Partitions:          partitionTextList(layout.partitionsForRole(src.Spec.Role)),
			ConfigKey:           captureConfigKey(src.Spec.Role),
			SourcePath:          src.Spec.Path,
			SourceCanonicalPath: src.Spec.Canonical,
			SourceJournalMode:   outcome.SourceJournalMode,
			RelativePath:        rel,
			SHA256:              sum,
			Size:                stamp.Size,
			CaptureStartedAt:    outcome.StartedAt,
			CaptureCompletedAt:  outcome.CompletedAt,
			IntegrityCheck:      outcome.IntegrityCheck,
			JournalMode:         outcome.JournalMode,
			TradesRows:          outcome.TradesRows,
			Attempts:            outcome.Attempts,
		})
	}

	copyData, err := rewriteCaptureConfigStorage(configData, relPaths)
	if err != nil {
		return "", err
	}
	if err := verifyCaptureConfigCopy(cfg, copyData); err != nil {
		return "", err
	}
	cleanup.created = append(cleanup.created, captureConfigCopyName)
	if err := writeRootFileExclusive(cleanup.out, captureConfigCopyName, copyData); err != nil {
		return "", fmt.Errorf("write configuration copy: %w", err)
	}
	manifest.ConfigCopy = captureFileRef{RelativePath: captureConfigCopyName, SHA256: sha256Hex(copyData)}

	_, after, err := readSourceConfigBytes(configCanonical)
	if err != nil {
		return "", fmt.Errorf("re-read source configuration: %w", err)
	}
	if sha256Hex(after) != configSHA {
		return "", fmt.Errorf("source configuration %q changed during capture", configCanonical)
	}

	manifest.CompletedAt = formatLedgerTimestamp(time.Now())
	if err := manifest.validate(); err != nil {
		return "", fmt.Errorf("assembled manifest is invalid: %w", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		return "", fmt.Errorf("encode manifest: %w", err)
	}
	expected[captureConfigCopyName] = true
	if err := ledgerSnapshotInventory(cleanup.out, expected); err != nil {
		return "", err
	}
	cleanup.created = append(cleanup.created, ledgerManifestName)
	if err := writeRootFileExclusive(cleanup.out, ledgerManifestName, buf.Bytes()); err != nil {
		return "", fmt.Errorf("write manifest: %w", err)
	}
	if err := syncRootDir(cleanup.out); err != nil {
		return "", fmt.Errorf("sync snapshot directory: %w", err)
	}
	if err := syncRootDir(cleanup.parent); err != nil {
		return "", fmt.Errorf("sync output parent directory: %w", err)
	}
	success = true
	cleanup.out.Close()
	cleanup.parent.Close()
	return filepath.Join(target.Final, ledgerManifestName), nil
}
