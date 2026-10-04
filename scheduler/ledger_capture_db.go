package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"math"
	"net/url"
	"os"
	"strings"
	"time"
)

const captureMaxAttempts = 3

var captureDigestTables = []string{"trades", "wallet_transfers", "trade_diagnostics"}

type captureDigest struct {
	Sum        string
	TradesRows int64
}

type captureFileOutcome struct {
	Role              string `json:"role"`
	SourceJournalMode string `json:"source_journal_mode"`
	StartedAt         string `json:"started_at"`
	CompletedAt       string `json:"completed_at"`
	IntegrityCheck    string `json:"integrity_check"`
	JournalMode       string `json:"journal_mode"`
	TradesRows        int64  `json:"trades_rows"`
	Attempts          int    `json:"attempts"`
}

func captureSourceDSN(path string) string {
	return (&url.URL{Scheme: "file", Path: path, RawQuery: captureSourceDSNQuery}).String()
}

func captureHashValue(h hash.Hash, v any) error {
	var tag [1]byte
	var n [8]byte
	switch t := v.(type) {
	case nil:
		tag[0] = 'n'
		h.Write(tag[:])
	case int64:
		tag[0] = 'i'
		h.Write(tag[:])
		binary.BigEndian.PutUint64(n[:], uint64(t))
		h.Write(n[:])
	case float64:
		tag[0] = 'f'
		h.Write(tag[:])
		binary.BigEndian.PutUint64(n[:], math.Float64bits(t))
		h.Write(n[:])
	case string:
		tag[0] = 's'
		h.Write(tag[:])
		binary.BigEndian.PutUint64(n[:], uint64(len(t)))
		h.Write(n[:])
		h.Write([]byte(t))
	case []byte:
		tag[0] = 'b'
		h.Write(tag[:])
		binary.BigEndian.PutUint64(n[:], uint64(len(t)))
		h.Write(n[:])
		h.Write(t)
	case bool:
		tag[0] = 'o'
		h.Write(tag[:])
		if t {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
	case time.Time:
		tag[0] = 't'
		h.Write(tag[:])
		s := t.UTC().Format(time.RFC3339Nano)
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	default:
		return fmt.Errorf("unsupported stored value type %T", v)
	}
	return nil
}

func captureTableDigest(ctx context.Context, conn *sql.Conn) (captureDigest, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return captureDigest{}, fmt.Errorf("begin digest read: %w", err)
	}
	defer tx.Rollback()
	h := sha256.New()
	var out captureDigest
	for _, table := range captureDigestTables {
		cols, ok, err := ledgerTableColumns(tx, table)
		if err != nil {
			return captureDigest{}, err
		}
		if !ok {
			h.Write([]byte("absent:" + table + "\x00"))
			continue
		}
		if !ledgerHasRowIDKey(cols) {
			if table == "trades" {
				return captureDigest{}, fmt.Errorf("the trades table has no explicit INTEGER PRIMARY KEY rowid column; row identity would not survive capture")
			}
			h.Write([]byte("unkeyed:" + table + "\x00"))
			continue
		}
		rows, err := tx.Query("SELECT * FROM " + table + " ORDER BY rowid")
		if err != nil {
			return captureDigest{}, fmt.Errorf("read %s for digest: %w", table, err)
		}
		names, err := rows.Columns()
		if err != nil {
			rows.Close()
			return captureDigest{}, err
		}
		h.Write([]byte("table:" + table + "\x00" + strings.Join(names, "\x00") + "\x00"))
		for rows.Next() {
			vals := make([]any, len(names))
			ptrs := make([]any, len(names))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return captureDigest{}, fmt.Errorf("scan %s for digest: %w", table, err)
			}
			h.Write([]byte{'r'})
			for _, v := range vals {
				if err := captureHashValue(h, v); err != nil {
					rows.Close()
					return captureDigest{}, fmt.Errorf("%s: %w", table, err)
				}
			}
			if table == "trades" {
				out.TradesRows++
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return captureDigest{}, fmt.Errorf("iterate %s for digest: %w", table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return captureDigest{}, fmt.Errorf("end digest read: %w", err)
	}
	out.Sum = hex.EncodeToString(h.Sum(nil))
	return out, nil
}

func removeCaptureDestination(dst string) {
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		os.Remove(dst + suffix)
	}
}

func captureStateFileVacuumInto(role, src, dst string) (captureFileOutcome, string, error) {
	out := captureFileOutcome{Role: role, StartedAt: formatLedgerTimestamp(time.Now())}
	ctx := context.Background()
	db, err := sql.Open("sqlite", captureSourceDSN(src))
	if err != nil {
		return out, "", fmt.Errorf("open %s source read-only: %w", role, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return out, "", fmt.Errorf("open %s source read-only: %w", role, err)
	}
	defer conn.Close()
	for _, p := range []string{"PRAGMA busy_timeout=5000", "PRAGMA temp_store=MEMORY"} {
		if _, err := conn.ExecContext(ctx, p); err != nil {
			return out, "", fmt.Errorf("%s source %s: %w", role, p, err)
		}
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&out.SourceJournalMode); err != nil {
		return out, "", fmt.Errorf("read %s source journal mode: %w", role, err)
	}
	out.SourceJournalMode = strings.ToLower(out.SourceJournalMode)
	var sqliteVersion string
	if err := conn.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&sqliteVersion); err != nil {
		return out, "", fmt.Errorf("read SQLite version: %w", err)
	}
	var before captureDigest
	for attempt := 1; ; attempt++ {
		out.Attempts = attempt
		if _, err := os.Lstat(dst); err == nil {
			return out, "", fmt.Errorf("%s destination %q already exists", role, dst)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return out, "", fmt.Errorf("check %s destination: %w", role, err)
		}
		before, err = captureTableDigest(ctx, conn)
		if err != nil {
			return out, "", fmt.Errorf("%s source: %w", role, err)
		}
		if _, err := conn.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
			removeCaptureDestination(dst)
			return out, "", fmt.Errorf("%s VACUUM INTO: %w", role, err)
		}
		after, err := captureTableDigest(ctx, conn)
		if err != nil {
			removeCaptureDestination(dst)
			return out, "", fmt.Errorf("%s source: %w", role, err)
		}
		if after.Sum == before.Sum {
			break
		}
		removeCaptureDestination(dst)
		if attempt >= captureMaxAttempts {
			return out, "", fmt.Errorf("%s ledger tables kept changing across %d capture attempts; retry when the scheduler is between cycles", role, attempt)
		}
	}
	if err := conn.Close(); err != nil {
		removeCaptureDestination(dst)
		return out, "", fmt.Errorf("close %s source: %w", role, err)
	}
	if err := db.Close(); err != nil {
		removeCaptureDestination(dst)
		return out, "", fmt.Errorf("close %s source: %w", role, err)
	}
	if err := verifyCaptureDestination(role, dst, before, &out); err != nil {
		removeCaptureDestination(dst)
		return out, "", err
	}
	out.TradesRows = before.TradesRows
	out.CompletedAt = formatLedgerTimestamp(time.Now())
	return out, sqliteVersion, nil
}

func verifyCaptureDestination(role, dst string, want captureDigest, out *captureFileOutcome) error {
	ctx := context.Background()
	dsn := (&url.URL{Scheme: "file", Path: dst, RawQuery: "mode=rw&cache=private"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open %s snapshot: %w", role, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open %s snapshot: %w", role, err)
	}
	defer conn.Close()
	var mode string
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("read %s snapshot journal mode: %w", role, err)
	}
	if strings.ToLower(mode) != "delete" {
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
			return fmt.Errorf("normalize %s snapshot journal mode: %w", role, err)
		}
	}
	out.JournalMode = strings.ToLower(mode)
	if out.JournalMode != "delete" {
		return fmt.Errorf("%s snapshot journal mode is %q, want delete", role, mode)
	}
	rows, err := conn.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("%s snapshot integrity check: %w", role, err)
	}
	var results []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			return fmt.Errorf("%s snapshot integrity check: %w", role, err)
		}
		results = append(results, line)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%s snapshot integrity check: %w", role, err)
	}
	if len(results) != 1 || results[0] != "ok" {
		return fmt.Errorf("%s snapshot integrity check returned %q", role, strings.Join(results, "; "))
	}
	out.IntegrityCheck = results[0]
	got, err := captureTableDigest(ctx, conn)
	if err != nil {
		return fmt.Errorf("%s snapshot: %w", role, err)
	}
	if got.Sum != want.Sum || got.TradesRows != want.TradesRows {
		return fmt.Errorf("%s snapshot ledger tables differ from the source snapshot (row identity or content not preserved)", role)
	}
	if err := conn.Close(); err != nil {
		return fmt.Errorf("close %s snapshot: %w", role, err)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("close %s snapshot: %w", role, err)
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if _, err := os.Lstat(dst + suffix); err == nil {
			return fmt.Errorf("%s snapshot left a %s sidecar", role, suffix)
		}
	}
	return nil
}
