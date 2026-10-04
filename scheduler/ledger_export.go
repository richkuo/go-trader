package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
)

const ledgerManifestName = "capture.json"

const ledgerMaxManifestBytes = 16 << 20

type ledgerOnceFlag struct {
	name  string
	value string
	set   bool
}

func (f *ledgerOnceFlag) String() string { return f.value }

func (f *ledgerOnceFlag) Set(v string) error {
	if f.set {
		return fmt.Errorf("--%s given more than once", f.name)
	}
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("--%s needs a non-empty value", f.name)
	}
	f.value = v
	f.set = true
	return nil
}

func parseLedgerOnceFlags(name string, args []string, names ...string) (map[string]string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	flags := make(map[string]*ledgerOnceFlag, len(names))
	for _, n := range names {
		f := &ledgerOnceFlag{name: n}
		flags[n] = f
		fs.Var(f, n, "")
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	out := make(map[string]string, len(names))
	for _, n := range names {
		if !flags[n].set {
			return nil, fmt.Errorf("--%s is required", n)
		}
		out[n] = flags[n].value
	}
	return out, nil
}

const ledgerExportUsage = "Usage: go-trader export ledger --manifest <snapshot-dir>/capture.json --partition <live|paper|paper:<source-id>> --strategy <process-strategy-id> --output <new-json-file>"

type ledgerExportOptions struct {
	ManifestPath string
	Partition    RiskPartition
	StrategyID   string
	OutputPath   string
}

func runLedgerExport(args []string) int {
	vals, err := parseLedgerOnceFlags("export ledger", args, "manifest", "partition", "strategy", "output")
	if err != nil {
		fmt.Fprintf(os.Stderr, "export ledger: %v\n%s\n", err, ledgerExportUsage)
		return 2
	}
	part, err := parseRiskPartition(vals["partition"])
	if err != nil || part == unassignedPartition {
		fmt.Fprintf(os.Stderr, "export ledger: --partition %q is not live, paper or paper:<source-id>\n%s\n", vals["partition"], ledgerExportUsage)
		return 2
	}
	opts := ledgerExportOptions{
		ManifestPath: vals["manifest"],
		Partition:    part,
		StrategyID:   strings.TrimSpace(vals["strategy"]),
		OutputPath:   vals["output"],
	}
	n, err := exportLedger(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "export ledger refused: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "Exported %d ledger events for %s (%s) to %s\n", n, opts.StrategyID, opts.Partition.String(), opts.OutputPath)
	return 0
}

type ledgerFileStamp struct {
	Dev   uint64
	Ino   uint64
	Size  int64
	Mtime int64
	Nlink uint64
}

func ledgerStampOf(info fs.FileInfo) (ledgerFileStamp, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ledgerFileStamp{}, fmt.Errorf("file identity is unavailable on this platform")
	}
	return ledgerFileStamp{
		Dev:   uint64(st.Dev),
		Ino:   uint64(st.Ino),
		Size:  info.Size(),
		Mtime: info.ModTime().UnixNano(),
		Nlink: uint64(st.Nlink),
	}, nil
}

func isPathWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func ledgerAliasDirs(dirs []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, d := range dirs {
		if d == "" {
			continue
		}
		for _, cand := range []string{filepath.Clean(d), canonicalOrSelf(d)} {
			if !seen[cand] {
				seen[cand] = true
				out = append(out, cand)
			}
		}
	}
	sort.Strings(out)
	return out
}

func canonicalOrSelf(p string) string {
	if c, err := filepath.EvalSymlinks(p); err == nil {
		return c
	}
	return filepath.Clean(p)
}

type ledgerOutputTarget struct {
	ParentCanonical string
	Name            string
	Final           string
	parentInfo      fs.FileInfo
}

func resolveLedgerOutputTarget(path string, protected []string, label string) (ledgerOutputTarget, error) {
	if strings.TrimSpace(path) == "" {
		return ledgerOutputTarget{}, fmt.Errorf("%s path is empty", label)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ledgerOutputTarget{}, fmt.Errorf("resolve %s %q: %w", label, path, err)
	}
	name := filepath.Base(abs)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return ledgerOutputTarget{}, fmt.Errorf("%s %q does not name a new entry", label, path)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return ledgerOutputTarget{}, fmt.Errorf("resolve the parent directory of %s %q: %w", label, path, err)
	}
	info, err := os.Stat(parent)
	if err != nil {
		return ledgerOutputTarget{}, fmt.Errorf("stat the parent directory of %s %q: %w", label, path, err)
	}
	if !info.IsDir() {
		return ledgerOutputTarget{}, fmt.Errorf("the parent of %s %q is not a directory", label, path)
	}
	final := filepath.Join(parent, name)
	if _, err := os.Lstat(final); err == nil {
		return ledgerOutputTarget{}, fmt.Errorf("%s %q already exists; refusing to replace or reuse it", label, final)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return ledgerOutputTarget{}, fmt.Errorf("check %s %q: %w", label, final, err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if strings.HasSuffix(name, suffix) {
			return ledgerOutputTarget{}, fmt.Errorf("%s %q uses a reserved SQLite sidecar name", label, final)
		}
	}
	for _, dir := range ledgerAliasDirs(protected) {
		if isPathWithin(final, dir) {
			return ledgerOutputTarget{}, fmt.Errorf("%s %q is inside protected directory %q", label, final, dir)
		}
	}
	return ledgerOutputTarget{ParentCanonical: parent, Name: name, Final: final, parentInfo: info}, nil
}

func (t ledgerOutputTarget) openParent() (*os.Root, error) {
	root, err := os.OpenRoot(t.ParentCanonical)
	if err != nil {
		return nil, fmt.Errorf("open output directory %q: %w", t.ParentCanonical, err)
	}
	info, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, fmt.Errorf("stat output directory %q: %w", t.ParentCanonical, err)
	}
	if !os.SameFile(info, t.parentInfo) {
		root.Close()
		return nil, fmt.Errorf("output directory %q changed after validation", t.ParentCanonical)
	}
	return root, nil
}

func ledgerRandomSuffix() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func syncRootDir(root *os.Root) error {
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func publishLedgerFile(t ledgerOutputTarget, data []byte, beforeLink func() error) error {
	root, err := t.openParent()
	if err != nil {
		return err
	}
	defer root.Close()
	suffix, err := ledgerRandomSuffix()
	if err != nil {
		return fmt.Errorf("staging name: %w", err)
	}
	staging := "." + t.Name + "." + suffix + ".staging"
	f, err := root.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create staging file in %q: %w", t.ParentCanonical, err)
	}
	cleanup := func() { root.Remove(staging) }
	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("write staging file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("sync staging file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close staging file: %w", err)
	}
	if beforeLink != nil {
		if err := beforeLink(); err != nil {
			cleanup()
			return err
		}
	}
	if err := root.Link(staging, t.Name); err != nil {
		cleanup()
		return fmt.Errorf("publish %q without replacement: %w", t.Final, err)
	}
	if err := root.Remove(staging); err != nil {
		fmt.Fprintf(os.Stderr, "[WARN] published %s but could not remove staging file %s: %v\n", t.Final, staging, err)
	}
	if err := syncRootDir(root); err != nil {
		fmt.Fprintf(os.Stderr, "[WARN] published %s but could not sync its directory: %v\n", t.Final, err)
	}
	return nil
}

func ledgerVerifiedRevision() *string {
	rev := strings.TrimSpace(SourceCommit)
	if len(rev) != 40 && len(rev) != 64 {
		return nil
	}
	if _, err := hex.DecodeString(rev); err != nil || strings.ToLower(rev) != rev {
		return nil
	}
	return &rev
}

func ledgerSQLiteDriverVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "modernc.org/sqlite (unknown version)"
	}
	for _, d := range bi.Deps {
		if d.Path == "modernc.org/sqlite" {
			if d.Replace != nil {
				return "modernc.org/sqlite " + d.Version + " => " + d.Replace.Path + " " + d.Replace.Version
			}
			return "modernc.org/sqlite " + d.Version
		}
	}
	return "modernc.org/sqlite (unknown version)"
}

type verifiedSnapshotFile struct {
	Role         storageRole
	RelativePath string
	AbsPath      string
	SHA256       string
	Stamp        ledgerFileStamp
}

type verifiedLedgerSnapshot struct {
	Dir           string
	ManifestPath  string
	ManifestSHA   string
	ManifestStamp ledgerFileStamp
	Manifest      captureManifest
	ConfigBytes   []byte
	ConfigStamp   ledgerFileStamp
	Files         []verifiedSnapshotFile
	dirInfo       fs.FileInfo
}

func readRootFileVerified(root *os.Root, rel string, limit int64) ([]byte, ledgerFileStamp, error) {
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ledgerFileStamp{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, ledgerFileStamp{}, err
	}
	if !info.Mode().IsRegular() {
		return nil, ledgerFileStamp{}, fmt.Errorf("%s is not a regular file", rel)
	}
	stamp, err := ledgerStampOf(info)
	if err != nil {
		return nil, ledgerFileStamp{}, err
	}
	if stamp.Nlink != 1 {
		return nil, ledgerFileStamp{}, fmt.Errorf("%s has %d hard links; a snapshot input must have exactly one", rel, stamp.Nlink)
	}
	if limit > 0 && info.Size() > limit {
		return nil, ledgerFileStamp{}, fmt.Errorf("%s is larger than %d bytes", rel, limit)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, ledgerFileStamp{}, err
	}
	if int64(len(data)) != info.Size() {
		return nil, ledgerFileStamp{}, fmt.Errorf("%s changed size while being read", rel)
	}
	return data, stamp, nil
}

func hashRootFileVerified(root *os.Root, rel string) (string, ledgerFileStamp, []byte, error) {
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", ledgerFileStamp{}, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", ledgerFileStamp{}, nil, err
	}
	if !info.Mode().IsRegular() {
		return "", ledgerFileStamp{}, nil, fmt.Errorf("%s is not a regular file", rel)
	}
	stamp, err := ledgerStampOf(info)
	if err != nil {
		return "", ledgerFileStamp{}, nil, err
	}
	if stamp.Nlink != 1 {
		return "", ledgerFileStamp{}, nil, fmt.Errorf("%s has %d hard links; a snapshot input must have exactly one", rel, stamp.Nlink)
	}
	h := sha256.New()
	header := make([]byte, 100)
	n, err := io.ReadFull(f, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", ledgerFileStamp{}, nil, err
	}
	header = header[:n]
	h.Write(header)
	copied, err := io.Copy(h, f)
	if err != nil {
		return "", ledgerFileStamp{}, nil, err
	}
	if int64(n)+copied != info.Size() {
		return "", ledgerFileStamp{}, nil, fmt.Errorf("%s changed size while being hashed", rel)
	}
	return hex.EncodeToString(h.Sum(nil)), stamp, header, nil
}

func checkRollbackSQLiteHeader(rel string, header []byte) error {
	if len(header) < 100 || !bytes.Equal(header[:16], []byte("SQLite format 3\x00")) {
		return fmt.Errorf("%s is not an SQLite database file", rel)
	}
	if header[18] != 1 || header[19] != 1 {
		return fmt.Errorf("%s is not in rollback-journal mode (header write/read versions %d/%d; 2 means WAL)", rel, header[18], header[19])
	}
	return nil
}

func ledgerSnapshotInventory(root *os.Root, expected map[string]bool) error {
	var extra []string
	err := fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			extra = append(extra, p+" (symlink)")
			return nil
		}
		if d.IsDir() {
			if !expected[p+"/"] {
				extra = append(extra, p+"/")
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			extra = append(extra, p+" (not a regular file)")
			return nil
		}
		if !expected[p] {
			extra = append(extra, p)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("list snapshot directory: %w", err)
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return fmt.Errorf("snapshot directory holds unexpected entries (journals, sidecars and extra files are refused): %s", strings.Join(extra, ", "))
	}
	return nil
}

func validateManifestRelPath(rel string) error {
	if rel == "" || filepath.IsAbs(rel) || !filepath.IsLocal(rel) || filepath.Clean(rel) != rel || strings.Contains(rel, "\\") {
		return fmt.Errorf("manifest path %q is not a clean relative path inside the snapshot directory", rel)
	}
	return nil
}

func (m captureManifest) expectedEntries() (map[string]bool, error) {
	out := map[string]bool{ledgerManifestName: true}
	add := func(rel string) error {
		if err := validateManifestRelPath(rel); err != nil {
			return err
		}
		if out[rel] {
			return fmt.Errorf("manifest names %q twice", rel)
		}
		out[rel] = true
		for dir := filepath.Dir(rel); dir != "."; dir = filepath.Dir(dir) {
			out[filepath.ToSlash(dir)+"/"] = true
		}
		return nil
	}
	if err := add(m.ConfigCopy.RelativePath); err != nil {
		return nil, err
	}
	for _, f := range m.Files {
		if err := add(f.RelativePath); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (m captureManifest) validate() error {
	if m.Manifest != captureManifestKind {
		return fmt.Errorf("manifest kind %q is not %q", m.Manifest, captureManifestKind)
	}
	if m.ManifestVersion != CaptureManifestVersion {
		return fmt.Errorf("unsupported capture manifest version %d (want %d)", m.ManifestVersion, CaptureManifestVersion)
	}
	if m.CaptureMethod != captureMethodVacuumInto {
		return fmt.Errorf("unsupported capture method %q", m.CaptureMethod)
	}
	if m.Consistency != ledgerCaptureConsistency {
		return fmt.Errorf("unsupported capture consistency %q", m.Consistency)
	}
	if _, err := parseLedgerTimestamp(m.StartedAt); err != nil {
		return fmt.Errorf("manifest started_at: %v", err)
	}
	if _, err := parseLedgerTimestamp(m.CompletedAt); err != nil {
		return fmt.Errorf("manifest completed_at: %v", err)
	}
	if len(m.Files) == 0 {
		return fmt.Errorf("manifest lists no state files")
	}
	if !isLowerHexSHA256(m.ConfigCopy.SHA256) || !isLowerHexSHA256(m.SourceConfig.SHA256) {
		return fmt.Errorf("manifest configuration hashes are malformed")
	}
	if !filepath.IsAbs(m.SourceConfig.Path) {
		return fmt.Errorf("manifest source configuration path %q is not absolute", m.SourceConfig.Path)
	}
	roles := make(map[string]bool)
	for _, f := range m.Files {
		if roles[f.SourceRole] {
			return fmt.Errorf("manifest lists state role %q twice", f.SourceRole)
		}
		roles[f.SourceRole] = true
		if !isLowerHexSHA256(f.SHA256) {
			return fmt.Errorf("manifest hash for %s is malformed", f.SourceRole)
		}
		if !filepath.IsAbs(f.SourceCanonicalPath) {
			return fmt.Errorf("manifest source path for %s is not absolute", f.SourceRole)
		}
		if f.IntegrityCheck != "ok" || f.JournalMode != "delete" {
			return fmt.Errorf("manifest records %s as integrity %q, journal mode %q", f.SourceRole, f.IntegrityCheck, f.JournalMode)
		}
		if _, err := parseLedgerTimestamp(f.CaptureStartedAt); err != nil {
			return fmt.Errorf("manifest capture_started_at for %s: %v", f.SourceRole, err)
		}
		if _, err := parseLedgerTimestamp(f.CaptureCompletedAt); err != nil {
			return fmt.Errorf("manifest capture_completed_at for %s: %v", f.SourceRole, err)
		}
	}
	if !roles[string(storageRolePrimary)] {
		return fmt.Errorf("manifest has no primary state file")
	}
	return nil
}

func isLowerHexSHA256(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func (v *verifiedLedgerSnapshot) protectedDirs() []string {
	dirs := []string{v.Dir, filepath.Dir(v.Manifest.SourceConfig.Path)}
	for _, f := range v.Manifest.Files {
		dirs = append(dirs, filepath.Dir(f.SourceCanonicalPath))
	}
	return dirs
}

func loadVerifiedLedgerSnapshot(manifestPath string) (*verifiedLedgerSnapshot, error) {
	abs, err := filepath.Abs(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("resolve manifest %q: %w", manifestPath, err)
	}
	if filepath.Base(abs) != ledgerManifestName {
		return nil, fmt.Errorf("manifest %q must be named %s", manifestPath, ledgerManifestName)
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, fmt.Errorf("resolve snapshot directory of %q: %w", manifestPath, err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("stat snapshot directory %q: %w", dir, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open snapshot directory %q: %w", dir, err)
	}
	defer root.Close()
	if info, err := root.Stat("."); err != nil || !os.SameFile(info, dirInfo) {
		return nil, fmt.Errorf("snapshot directory %q changed while opening", dir)
	}
	data, stamp, err := readRootFileVerified(root, ledgerManifestName, ledgerMaxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	sum := sha256.Sum256(data)
	v := &verifiedLedgerSnapshot{
		Dir:           dir,
		ManifestPath:  filepath.Join(dir, ledgerManifestName),
		ManifestSHA:   hex.EncodeToString(sum[:]),
		ManifestStamp: stamp,
		dirInfo:       dirInfo,
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v.Manifest); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("parse manifest: trailing data after the manifest object")
	}
	if err := v.Manifest.validate(); err != nil {
		return nil, err
	}
	if err := v.verifyContents(root); err != nil {
		return nil, err
	}
	return v, nil
}

func (v *verifiedLedgerSnapshot) verifyContents(root *os.Root) error {
	expected, err := v.Manifest.expectedEntries()
	if err != nil {
		return err
	}
	if err := ledgerSnapshotInventory(root, expected); err != nil {
		return err
	}
	cfgData, cfgStamp, err := readRootFileVerified(root, v.Manifest.ConfigCopy.RelativePath, ledgerMaxManifestBytes)
	if err != nil {
		return fmt.Errorf("read configuration copy: %w", err)
	}
	cfgSum := sha256.Sum256(cfgData)
	if hex.EncodeToString(cfgSum[:]) != v.Manifest.ConfigCopy.SHA256 {
		return fmt.Errorf("configuration copy %s hash does not match the manifest", v.Manifest.ConfigCopy.RelativePath)
	}
	files := make([]verifiedSnapshotFile, 0, len(v.Manifest.Files))
	for _, f := range v.Manifest.Files {
		sum, stamp, header, err := hashRootFileVerified(root, f.RelativePath)
		if err != nil {
			return fmt.Errorf("read snapshot %s: %w", f.RelativePath, err)
		}
		if sum != f.SHA256 {
			return fmt.Errorf("snapshot %s hash does not match the manifest", f.RelativePath)
		}
		if err := checkRollbackSQLiteHeader(f.RelativePath, header); err != nil {
			return err
		}
		files = append(files, verifiedSnapshotFile{
			Role:         storageRole(f.SourceRole),
			RelativePath: f.RelativePath,
			AbsPath:      filepath.Join(v.Dir, filepath.FromSlash(f.RelativePath)),
			SHA256:       sum,
			Stamp:        stamp,
		})
	}
	if v.Files != nil {
		if !bytes.Equal(cfgData, v.ConfigBytes) || cfgStamp != v.ConfigStamp {
			return fmt.Errorf("configuration copy changed during export")
		}
		for i := range files {
			if files[i] != v.Files[i] {
				return fmt.Errorf("snapshot %s changed during export", files[i].RelativePath)
			}
		}
		return nil
	}
	v.ConfigBytes = cfgData
	v.ConfigStamp = cfgStamp
	v.Files = files
	return nil
}

func (v *verifiedLedgerSnapshot) reverify() error {
	root, err := os.OpenRoot(v.Dir)
	if err != nil {
		return fmt.Errorf("reopen snapshot directory: %w", err)
	}
	defer root.Close()
	if info, err := root.Stat("."); err != nil || !os.SameFile(info, v.dirInfo) {
		return fmt.Errorf("snapshot directory %q was replaced during export", v.Dir)
	}
	data, stamp, err := readRootFileVerified(root, ledgerManifestName, ledgerMaxManifestBytes)
	if err != nil {
		return fmt.Errorf("re-read manifest: %w", err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != v.ManifestSHA || stamp != v.ManifestStamp {
		return fmt.Errorf("manifest changed during export")
	}
	return v.verifyContents(root)
}

func (v *verifiedLedgerSnapshot) file(role storageRole) (verifiedSnapshotFile, bool) {
	for _, f := range v.Files {
		if f.Role == role {
			return f, true
		}
	}
	return verifiedSnapshotFile{}, false
}

func (v *verifiedLedgerSnapshot) bindConfigStorage(cfg *Config) error {
	assigned := make(map[storageRole]bool)
	bind := func(role storageRole, key string, value *string) error {
		vf, ok := v.file(role)
		if !ok {
			return fmt.Errorf("configuration copy %s names a %s state file the manifest does not list", key, role)
		}
		rel := strings.TrimSpace(*value)
		if filepath.IsAbs(rel) || filepath.Clean(rel) != rel || filepath.ToSlash(rel) != vf.RelativePath {
			return fmt.Errorf("configuration copy %s is %q; the manifest maps %s to %q", key, *value, role, vf.RelativePath)
		}
		*value = vf.AbsPath
		assigned[role] = true
		return nil
	}
	if err := bind(storageRolePrimary, "db_file", &cfg.DBFile); err != nil {
		return err
	}
	if cfg.PaperDBFile != "" {
		if err := bind(storageRolePaper, "paper_db_file", &cfg.PaperDBFile); err != nil {
			return err
		}
	}
	for i := range cfg.PaperSources {
		src := &cfg.PaperSources[i]
		if err := bind(paperSourceRole(src.ID), fmt.Sprintf("paper_sources[%s].db_file", src.ID), &src.DBFile); err != nil {
			return err
		}
	}
	for _, f := range v.Files {
		if !assigned[f.Role] {
			return fmt.Errorf("manifest lists a %s state file the configuration copy does not use", f.Role)
		}
	}
	return nil
}

func selectLedgerStrategy(cfg *Config, ident storageIdentityMap, part RiskPartition, id string) (StrategyConfig, storageIdentity, error) {
	var matches []StrategyConfig
	for _, sc := range cfg.Strategies {
		if sc.ID == id {
			matches = append(matches, sc)
		}
	}
	if len(matches) == 0 {
		return StrategyConfig{}, storageIdentity{}, fmt.Errorf("strategy %q is not in the captured configuration", id)
	}
	if len(matches) > 1 {
		return StrategyConfig{}, storageIdentity{}, fmt.Errorf("strategy %q appears %d times in the captured configuration", id, len(matches))
	}
	sc := matches[0]
	if got := partitionFor(sc); got != part {
		return StrategyConfig{}, storageIdentity{}, fmt.Errorf("strategy %q belongs to partition %s, not %s", id, got.String(), part.String())
	}
	si, ok := ident.storageFor(id)
	if !ok {
		return StrategyConfig{}, storageIdentity{}, fmt.Errorf("strategy %q has no storage identity", id)
	}
	if si.Partition != part {
		return StrategyConfig{}, storageIdentity{}, fmt.Errorf("strategy %q storage identity names partition %s, not %s", id, si.Partition.String(), part.String())
	}
	if !strings.EqualFold(strings.TrimSpace(sc.Platform), ledgerExportPlatform) || (sc.Type != "perps" && sc.Type != "manual") {
		return StrategyConfig{}, storageIdentity{}, fmt.Errorf("strategy %q is platform %q type %q; the ledger export supports Hyperliquid perps and Hyperliquid manual owners only", id, sc.Platform, sc.Type)
	}
	return sc, si, nil
}

func ledgerRawJSON(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

func exportLedger(opts ledgerExportOptions) (int, error) {
	snap, err := loadVerifiedLedgerSnapshot(opts.ManifestPath)
	if err != nil {
		return 0, err
	}
	target, err := resolveLedgerOutputTarget(opts.OutputPath, snap.protectedDirs(), "output")
	if err != nil {
		return 0, err
	}
	cfg, err := LoadConfigForLedgerExport(snap.ConfigBytes)
	if err != nil {
		return 0, fmt.Errorf("load the configuration copy: %w", err)
	}
	if err := snap.bindConfigStorage(cfg); err != nil {
		return 0, err
	}
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		return 0, fmt.Errorf("resolve snapshot storage layout: %w", err)
	}
	if len(layout.Files) != len(snap.Files) {
		return 0, fmt.Errorf("snapshot layout has %d files, the manifest lists %d", len(layout.Files), len(snap.Files))
	}
	for i, spec := range layout.Files {
		vf := snap.Files[i]
		if spec.Role != vf.Role || spec.Canonical != vf.AbsPath {
			return 0, fmt.Errorf("snapshot layout file %d resolves to %s %q, the manifest lists %s %q", i, spec.Role, spec.Canonical, vf.Role, vf.AbsPath)
		}
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		return 0, fmt.Errorf("build storage identity map: %w", err)
	}
	sc, si, err := selectLedgerStrategy(cfg, ident, opts.Partition, opts.StrategyID)
	if err != nil {
		return 0, err
	}
	inspection, err := inspectStorageOwnershipWith(layout, ident, cfg, false, openStateDBSnapshot)
	if err != nil {
		return 0, fmt.Errorf("ownership inspection: %w", err)
	}
	if !inspection.OK() {
		return 0, fmt.Errorf("ownership inspection rejected the snapshot set:\n  - %s", strings.Join(inspection.Rejections, "\n  - "))
	}
	for _, fi := range inspection.Files {
		if !fi.Present {
			return 0, fmt.Errorf("snapshot %s state file %q is missing", fi.Role, fi.Path)
		}
	}

	store, err := openLedgerSnapshotStore(layout, ident)
	if err != nil {
		return 0, err
	}
	for _, vf := range snap.Files {
		info, statErr := os.Lstat(vf.AbsPath)
		if statErr != nil {
			store.Close()
			return 0, fmt.Errorf("stat opened snapshot %s: %w", vf.RelativePath, statErr)
		}
		stamp, stampErr := ledgerStampOf(info)
		if stampErr != nil || stamp.Dev != vf.Stamp.Dev || stamp.Ino != vf.Stamp.Ino {
			store.Close()
			return 0, fmt.Errorf("snapshot %s was replaced between verification and open", vf.RelativePath)
		}
	}
	read, readIdent, readErr := store.ReadLedgerSnapshot(sc.ID, ledgerExportPageSize)
	var wallet *ledgerWalletRead
	if readErr == nil && opts.Partition.IsLive() {
		wallet, readErr = store.ReadLedgerWalletOrphans(ledgerExportPageSize)
	}
	closeErr := store.Close()
	if readErr != nil {
		return 0, readErr
	}
	if closeErr != nil {
		return 0, fmt.Errorf("close snapshot files: %w", closeErr)
	}
	if readIdent != si {
		return 0, fmt.Errorf("strategy %q resolved to %s/%q at read time, %s/%q at selection", sc.ID, readIdent.Role, readIdent.StorageID, si.Role, si.StorageID)
	}

	events, err := buildLedgerEvents(read, opts.Partition, sc.ID)
	if err != nil {
		return 0, err
	}
	walletCtx, err := buildLedgerWalletContext(wallet, opts.Partition)
	if err != nil {
		return 0, err
	}
	strategyJSON, err := ledgerRawJSON(sc)
	if err != nil {
		return 0, fmt.Errorf("encode effective strategy: %w", err)
	}
	regimeJSON, err := ledgerRawJSON(cfg.Regime)
	if err != nil {
		return 0, fmt.Errorf("encode effective regime: %w", err)
	}
	riskJSON, err := ledgerRawJSON(partitionRiskConfig(cfg, opts.Partition))
	if err != nil {
		return 0, fmt.Errorf("encode effective portfolio risk: %w", err)
	}
	snapFiles := make([]ledgerSnapshotFile, 0, len(snap.Files))
	for _, f := range snap.Files {
		snapFiles = append(snapFiles, ledgerSnapshotFile{SourceRole: string(f.Role), RelativePath: f.RelativePath, SHA256: f.SHA256})
	}
	doc := ledgerExportDocument{
		Schema:                LedgerExportSchema,
		SchemaVersion:         LedgerExportSchemaVersion,
		InspectedRevision:     ledgerVerifiedRevision(),
		CaptureManifestSHA256: snap.ManifestSHA,
		TimeBasis:             "UTC",
		TimestampMeanings: ledgerTimestampMeanings{
			Trades:          ledgerTradeTimestampMeaning,
			WalletTransfers: ledgerWalletTimestampMeaning,
		},
		Selection: ledgerSelection{
			Partition:         opts.Partition.String(),
			ProcessStrategyID: sc.ID,
			StorageStrategyID: si.StorageID,
			SourceRole:        string(si.Role),
			Platform:          ledgerExportPlatform,
		},
		Capture: ledgerCaptureInfo{
			StartedAt:      snap.Manifest.StartedAt,
			CompletedAt:    snap.Manifest.CompletedAt,
			SourceRevision: snap.Manifest.SourceRevision,
			Consistency:    ledgerCaptureConsistency,
		},
		SnapshotFiles: snapFiles,
		CurrentEffectiveConfiguration: ledgerEffectiveConfig{
			Basis:         ledgerConfigBasis,
			ConfigVersion: cfg.ConfigVersion,
			Strategy:      strategyJSON,
			Regime:        regimeJSON,
			PortfolioRisk: riskJSON,
		},
		Events:              events,
		WalletOrphanContext: walletCtx,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return 0, fmt.Errorf("encode export: %w", err)
	}
	if err := snap.reverify(); err != nil {
		return 0, err
	}
	if err := publishLedgerFile(target, buf.Bytes(), snap.reverify); err != nil {
		return 0, err
	}
	return len(events), nil
}
