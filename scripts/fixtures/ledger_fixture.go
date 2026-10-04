//go:build linux

package main

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"

	_ "modernc.org/sqlite"
)

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ledger_fixture: "+format+"\n", args...)
	os.Exit(1)
}

func driverVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, d := range bi.Deps {
		if d.Path == "modernc.org/sqlite" {
			return d.Version
		}
	}
	return "unknown"
}

func openRW(path string) *sql.DB {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		fail("open %s: %v", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		fail("busy_timeout %s: %v", path, err)
	}
	return db
}

func cmdExec(path string, stmts []string) {
	db := openRW(path)
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			fail("exec on %s: %v\n  %s", path, err, s)
		}
	}
}

func cmdQuery(path, query string) {
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		fail("open %s: %v", path, err)
	}
	defer db.Close()
	rows, err := db.Query(query)
	if err != nil {
		fail("query %s: %v", path, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			fail("scan: %v", err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			switch t := v.(type) {
			case []byte:
				parts[i] = string(t)
			case nil:
				parts[i] = "NULL"
			default:
				parts[i] = fmt.Sprint(t)
			}
		}
		fmt.Println(strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		fail("rows: %v", err)
	}
}

func cmdWriter(paths []string) {
	dbs := make([]*sql.DB, len(paths))
	for i, p := range paths {
		db := openRW(p)
		for _, s := range []string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0"} {
			if _, err := db.Exec(s); err != nil {
				fail("%s on %s: %v", s, p, err)
			}
		}
		var a, b, c int
		if err := db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&a, &b, &c); err != nil {
			fail("checkpoint %s: %v", p, err)
		}
		dbs[i] = db
	}
	fmt.Println("ready")
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		cmd, rest, _ := strings.Cut(line, " ")
		switch cmd {
		case "exec":
			idxText, stmt, _ := strings.Cut(rest, " ")
			idx, err := strconv.Atoi(idxText)
			if err != nil || idx < 0 || idx >= len(dbs) {
				fail("bad db index %q", idxText)
			}
			if _, err := dbs[idx].Exec(stmt); err != nil {
				fail("writer exec: %v\n  %s", err, stmt)
			}
			fmt.Println("ok")
		case "close":
			for _, db := range dbs {
				db.Close()
			}
			fmt.Println("closed")
			return
		case "crash":
			fmt.Println("crashing")
			os.Exit(0)
		case "ping":
			fmt.Println("ok")
		default:
			fail("unknown writer command %q", cmd)
		}
	}
}

type fpEntry struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Size   int64  `json:"size"`
	Inode  uint64 `json:"inode"`
	Mode   string `json:"mode"`
	UID    uint32 `json:"uid"`
	GID    uint32 `json:"gid"`
	Nlink  uint64 `json:"nlink"`
	Mtime  int64  `json:"mtime_ns"`
	Ctime  int64  `json:"ctime_ns"`
	SHA256 string `json:"sha256,omitempty"`
	Link   string `json:"link,omitempty"`
}

func fingerprintDir(root string) ([]fpEntry, error) {
	var out []fpEntry
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		st := info.Sys().(*syscall.Stat_t)
		e := fpEntry{
			Path:  p,
			Size:  info.Size(),
			Inode: st.Ino,
			Mode:  info.Mode().String(),
			UID:   st.Uid,
			GID:   st.Gid,
			Nlink: uint64(st.Nlink),
			Mtime: st.Mtim.Sec*1e9 + st.Mtim.Nsec,
			Ctime: st.Ctim.Sec*1e9 + st.Ctim.Nsec,
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			e.Type = "symlink"
			e.Link, _ = os.Readlink(p)
		case info.IsDir():
			e.Type = "dir"
		case info.Mode().IsRegular():
			e.Type = "file"
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			h := sha256.New()
			_, err = io.Copy(h, f)
			f.Close()
			if err != nil {
				return err
			}
			e.SHA256 = hex.EncodeToString(h.Sum(nil))
		default:
			e.Type = "other"
		}
		out = append(out, e)
		return nil
	})
	return out, err
}

func cmdFingerprint(out string, dirs []string) {
	all := map[string][]fpEntry{}
	keys := append([]string{}, dirs...)
	sort.Strings(keys)
	for _, d := range keys {
		entries, err := fingerprintDir(d)
		if err != nil {
			fail("fingerprint %s: %v", d, err)
		}
		all[d] = entries
	}
	b, err := json.MarshalIndent(all, "", " ")
	if err != nil {
		fail("encode fingerprint: %v", err)
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		fail("write fingerprint: %v", err)
	}
}

var traceCall = regexp.MustCompile(`^(?:\[pid\s+\d+\]\s+|\d+\s+)?([a-z0-9_]+)\(`)

var mutatingCalls = map[string]bool{
	"openat": true, "open": true, "creat": true, "unlink": true, "unlinkat": true, "rename": true, "renameat": true,
	"renameat2": true, "truncate": true, "ftruncate": true, "fchown": true, "fchownat": true, "chown": true, "lchown": true,
	"fchmod": true, "fchmodat": true, "chmod": true, "mkdir": true, "mkdirat": true, "rmdir": true, "link": true, "linkat": true,
	"symlink": true, "symlinkat": true, "write": true, "pwrite64": true, "writev": true, "pwritev": true, "pwritev2": true,
	"fallocate": true, "utimensat": true, "mmap": true, "setxattr": true, "fsetxattr": true, "removexattr": true, "fremovexattr": true,
}

func cmdTraceCheck(trace string, dirs []string) {
	f, err := os.Open(trace)
	if err != nil {
		fail("open trace: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var mutations, refused []string
	failed := regexp.MustCompile(`= -1 E[A-Z]+`)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		m := traceCall.FindStringSubmatch(line)
		if m == nil || !mutatingCalls[m[1]] {
			continue
		}
		touches := false
		for _, d := range dirs {
			if strings.Contains(line, d+"/") || strings.Contains(line, "\""+d+"\"") || strings.Contains(line, "<"+d+">") {
				touches = true
			}
		}
		if !touches {
			continue
		}
		call := m[1]
		if call == "openat" || call == "open" {
			if !strings.Contains(line, "O_WRONLY") && !strings.Contains(line, "O_RDWR") && !strings.Contains(line, "O_CREAT") && !strings.Contains(line, "O_TRUNC") {
				continue
			}
		}
		if call == "mmap" && (!strings.Contains(line, "PROT_WRITE") || !strings.Contains(line, "MAP_SHARED")) {
			continue
		}
		if failed.MatchString(line) {
			refused = append(refused, line)
		} else {
			mutations = append(mutations, line)
		}
	}
	if err := sc.Err(); err != nil {
		fail("read trace: %v", err)
	}
	fmt.Printf("trace: %d successful source mutation calls, %d refused source mutation attempts\n", len(mutations), len(refused))
	for _, l := range refused {
		if len(l) > 240 {
			l = l[:240]
		}
		fmt.Println("  refused:", l)
	}
	for _, l := range mutations {
		if len(l) > 240 {
			l = l[:240]
		}
		fmt.Println("  MUTATION:", l)
	}
	if len(mutations) > 0 {
		os.Exit(1)
	}
}

func lookup(doc any, path string) (any, error) {
	cur := doc
	if path == "" || path == "." {
		return cur, nil
	}
	for _, part := range strings.Split(path, ".") {
		switch t := cur.(type) {
		case map[string]any:
			v, ok := t[part]
			if !ok {
				return nil, fmt.Errorf("no key %q", part)
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(t) {
				return nil, fmt.Errorf("bad index %q", part)
			}
			cur = t[i]
		default:
			return nil, fmt.Errorf("cannot descend into %T at %q", cur, part)
		}
	}
	return cur, nil
}

func loadJSON(path string) any {
	b, err := os.ReadFile(path)
	if err != nil {
		fail("read %s: %v", path, err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		fail("parse %s: %v", path, err)
	}
	return doc
}

func render(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	case []any:
		return "array:" + strconv.Itoa(len(t))
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return "object:" + strings.Join(keys, ",")
	}
	return fmt.Sprint(v)
}

func cmdGet(path string, keys []string) {
	doc := loadJSON(path)
	for _, k := range keys {
		v, err := lookup(doc, k)
		if err != nil {
			fail("%s: %v", k, err)
		}
		fmt.Println(render(v))
	}
}

func cmdEvents(path string, fields []string) {
	doc := loadJSON(path)
	evs, err := lookup(doc, "events")
	if err != nil {
		fail("events: %v", err)
	}
	for _, ev := range evs.([]any) {
		parts := make([]string, len(fields))
		for i, f := range fields {
			v, err := lookup(ev, f)
			if err != nil {
				fail("%s: %v", f, err)
			}
			parts[i] = render(v)
		}
		fmt.Println(strings.Join(parts, "|"))
	}
}

func cmdSchemaKeys(path string) {
	doc := loadJSON(path)
	evs, err := lookup(doc, "events")
	if err != nil {
		fail("events: %v", err)
	}
	evidence := []string{"value", "raw_value", "status", "reason", "provenance"}
	for i, ev := range evs.([]any) {
		obj := ev.(map[string]any)
		for k, v := range obj {
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			for _, want := range evidence {
				if _, ok := m[want]; !ok {
					fail("event %d field %s lacks %s", i, k, want)
				}
			}
			if len(m) != len(evidence) {
				fail("event %d field %s has extra keys", i, k)
			}
			prov, ok := m["provenance"].([]any)
			if !ok {
				fail("event %d field %s provenance is not an array", i, k)
			}
			for _, p := range prov {
				pm := p.(map[string]any)
				for _, want := range []string{"kind", "source_role", "source_table", "source_row_id", "source_field"} {
					if _, ok := pm[want].(string); !ok {
						fail("event %d field %s provenance lacks string %s", i, k, want)
					}
				}
			}
		}
	}
	fmt.Println("evidence-shape-ok")
}

func setPath(doc any, path string, value any) error {
	parts := strings.Split(path, ".")
	parent, err := lookup(doc, strings.Join(parts[:len(parts)-1], "."))
	if len(parts) == 1 {
		parent, err = doc, nil
	}
	if err != nil {
		return err
	}
	last := parts[len(parts)-1]
	switch t := parent.(type) {
	case map[string]any:
		t[last] = value
	case []any:
		i, err := strconv.Atoi(last)
		if err != nil || i < 0 || i >= len(t) {
			return fmt.Errorf("bad index %q", last)
		}
		t[i] = value
	default:
		return fmt.Errorf("cannot set into %T", parent)
	}
	return nil
}

func writeJSON(path string, doc any) {
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		fail("encode %s: %v", path, err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		fail("write %s: %v", path, err)
	}
}

func cmdJSONSet(path, key, literal string) {
	doc := loadJSON(path)
	dec := json.NewDecoder(strings.NewReader(literal))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		fail("value %q: %v", literal, err)
	}
	if err := setPath(doc, key, value); err != nil {
		fail("%s: %v", key, err)
	}
	writeJSON(path, doc)
}

func fileSHA(path string) string {
	f, err := os.Open(path)
	if err != nil {
		fail("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		fail("read %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func cmdRehash(manifest string) {
	doc := loadJSON(manifest)
	dir := filepath.Dir(manifest)
	m := doc.(map[string]any)
	cc := m["config_copy"].(map[string]any)
	cc["sha256"] = fileSHA(filepath.Join(dir, cc["relative_path"].(string)))
	for _, f := range m["files"].([]any) {
		fm := f.(map[string]any)
		fm["sha256"] = fileSHA(filepath.Join(dir, fm["relative_path"].(string)))
	}
	writeJSON(manifest, doc)
}

func main() {
	if len(os.Args) < 2 {
		fail("usage: ledger_fixture <command> ...")
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "version":
		fmt.Println("modernc.org/sqlite " + driverVersion())
	case "exec":
		if len(args) < 2 {
			fail("exec <db> <sql>...")
		}
		cmdExec(args[0], args[1:])
	case "query":
		if len(args) != 2 {
			fail("query <db> <sql>")
		}
		cmdQuery(args[0], args[1])
	case "writer":
		cmdWriter(args)
	case "fingerprint":
		if len(args) < 2 {
			fail("fingerprint <out> <dir>...")
		}
		cmdFingerprint(args[0], args[1:])
	case "trace-check":
		if len(args) < 2 {
			fail("trace-check <trace> <dir>...")
		}
		cmdTraceCheck(args[0], args[1:])
	case "get":
		if len(args) < 2 {
			fail("get <json> <path>...")
		}
		cmdGet(args[0], args[1:])
	case "events":
		if len(args) < 2 {
			fail("events <json> <field>...")
		}
		cmdEvents(args[0], args[1:])
	case "json-set":
		if len(args) != 3 {
			fail("json-set <file> <path> <json>")
		}
		cmdJSONSet(args[0], args[1], args[2])
	case "rehash":
		if len(args) != 1 {
			fail("rehash <manifest>")
		}
		cmdRehash(args[0])
	case "sha256":
		if len(args) != 1 {
			fail("sha256 <file>")
		}
		fmt.Println(fileSHA(args[0]))
	case "evidence-shape":
		if len(args) != 1 {
			fail("evidence-shape <json>")
		}
		cmdSchemaKeys(args[0])
	default:
		fail("unknown command %q", os.Args[1])
	}
}
