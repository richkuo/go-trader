package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	ledgerFixtureRegenerateEnv     = "GO_TRADER_REGENERATE_LEDGER_EXPORT_FIXTURE"
	ledgerFixtureRegenerateCommand = "GO_TRADER_REGENERATE_LEDGER_EXPORT_FIXTURE=1 go -C scheduler test -run '^TestLedgerExportFixture$' -count=1 ."
	ledgerFixtureRecaptureDoc      = "backtest/testdata/ledger_export/README.md (Linux snapshot re-capture)"
	ledgerFixtureProductionSchema  = "go-trader.ledger-export-fixture-production"
)

var ledgerFixtureDir = filepath.Join("..", "backtest", "testdata", "ledger_export")

type ledgerFixtureCapture struct {
	CaptureManifestSHA256 string `json:"capture_manifest_sha256"`
	SeedSQLSHA256         string `json:"seed_sql_sha256"`
	SourceDataChanged     bool   `json:"source_data_changed"`
	CapturedWith          string `json:"captured_with"`
	Note                  string `json:"note"`
}

type ledgerFixtureExport struct {
	File      string `json:"file"`
	Partition string `json:"partition"`
	Strategy  string `json:"strategy"`
}

type ledgerFixtureProduction struct {
	Schema        string                 `json:"schema"`
	SchemaVersion int                    `json:"schema_version"`
	Captures      []ledgerFixtureCapture `json:"captures"`
	Exports       []ledgerFixtureExport  `json:"exports"`
}

var ledgerFixtureSections = []string{
	"schema", "schema_version", "inspected_revision", "capture_manifest_sha256", "time_basis",
	"timestamp_meanings", "selection", "capture", "snapshot_files", "current_effective_configuration",
	"events", "wallet_orphan_context",
}

func ledgerFixtureSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func loadLedgerFixtureProduction(t *testing.T) ledgerFixtureProduction {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ledgerFixtureDir, "fixture_production.json"))
	if err != nil {
		t.Fatalf("read fixture production record: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var prod ledgerFixtureProduction
	if err := dec.Decode(&prod); err != nil {
		t.Fatalf("parse fixture production record: %v", err)
	}
	if prod.Schema != ledgerFixtureProductionSchema || prod.SchemaVersion != 1 {
		t.Fatalf("fixture production record is %s version %d, want %s version 1", prod.Schema, prod.SchemaVersion, ledgerFixtureProductionSchema)
	}
	if len(prod.Captures) == 0 || len(prod.Exports) == 0 {
		t.Fatalf("fixture production record lists no capture or no export")
	}
	for i, c := range prod.Captures {
		want := i == 0 || c.SeedSQLSHA256 != prod.Captures[i-1].SeedSQLSHA256
		if c.SourceDataChanged != want {
			t.Fatalf("fixture production capture %d records source_data_changed=%t, but its seed_sql_sha256 says %t", i, c.SourceDataChanged, want)
		}
		if strings.TrimSpace(c.CapturedWith) == "" || strings.TrimSpace(c.Note) == "" {
			t.Fatalf("fixture production capture %d does not document how and why it was captured", i)
		}
	}
	return prod
}

func ledgerFixtureSplit(data []byte) (map[string][]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(raw))
	for k, v := range raw {
		var buf bytes.Buffer
		if err := json.Compact(&buf, v); err != nil {
			return nil, err
		}
		out[k] = buf.Bytes()
	}
	return out, nil
}

func ledgerFixtureChangedSections(oldDoc, newDoc map[string][]byte) []string {
	keys := make(map[string]bool)
	for k := range oldDoc {
		keys[k] = true
	}
	for k := range newDoc {
		keys[k] = true
	}
	var changed []string
	for k := range keys {
		ov, okOld := oldDoc[k]
		nv, okNew := newDoc[k]
		if okOld != okNew || !bytes.Equal(ov, nv) {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	return changed
}

func ledgerFixtureSchemaVersion(doc map[string][]byte) (string, int, error) {
	var schema string
	var version int
	if err := json.Unmarshal(doc["schema"], &schema); err != nil {
		return "", 0, fmt.Errorf("schema: %w", err)
	}
	if err := json.Unmarshal(doc["schema_version"], &version); err != nil {
		return "", 0, fmt.Errorf("schema_version: %w", err)
	}
	return schema, version, nil
}

func ledgerFixtureRegenerationRefusal(oldDoc, newDoc map[string][]byte, latest ledgerFixtureCapture) error {
	_, oldVersion, err := ledgerFixtureSchemaVersion(oldDoc)
	if err != nil {
		return fmt.Errorf("decode the committed export: %w", err)
	}
	_, newVersion, err := ledgerFixtureSchemaVersion(newDoc)
	if err != nil {
		return fmt.Errorf("decode the new export: %w", err)
	}
	if oldVersion != newVersion {
		return nil
	}
	recaptured := !bytes.Equal(oldDoc["capture_manifest_sha256"], newDoc["capture_manifest_sha256"])
	allowed := map[string]bool{"current_effective_configuration": true}
	if recaptured {
		allowed["capture_manifest_sha256"] = true
		allowed["capture"] = true
		allowed["snapshot_files"] = true
		if latest.SourceDataChanged {
			allowed["events"] = true
			allowed["wallet_orphan_context"] = true
		}
	}
	var refused []string
	for _, k := range ledgerFixtureChangedSections(oldDoc, newDoc) {
		if !allowed[k] {
			refused = append(refused, k)
		}
	}
	if len(refused) == 0 {
		return nil
	}
	return fmt.Errorf("refusing to regenerate: sections %s changed without an export schema-version change (recaptured=%t, latest capture source_data_changed=%t); booked accounting may change only with a schema-version change or a documented change to source/seed.sql recorded in fixture_production.json, see %s",
		strings.Join(refused, ", "), recaptured, latest.SourceDataChanged, ledgerFixtureRecaptureDoc)
}

func TestLedgerExportFixture(t *testing.T) {
	if SourceCommit != "" {
		t.Fatalf("SourceCommit is %q; the fixture export needs an unstamped test binary so inspected_revision stays null", SourceCommit)
	}
	regenerate := os.Getenv(ledgerFixtureRegenerateEnv) == "1"
	prod := loadLedgerFixtureProduction(t)
	latest := prod.Captures[len(prod.Captures)-1]
	manifest := filepath.Join(ledgerFixtureDir, "snapshot", ledgerManifestName)
	if got := ledgerFixtureSHA256(t, manifest); got != latest.CaptureManifestSHA256 {
		t.Fatalf("snapshot/capture.json sha256 is %s; fixture_production.json documents %s as the latest capture. Record a new capture as described in %s, then run %s", got, latest.CaptureManifestSHA256, ledgerFixtureRecaptureDoc, ledgerFixtureRegenerateCommand)
	}
	if got := ledgerFixtureSHA256(t, filepath.Join(ledgerFixtureDir, "source", "seed.sql")); got != latest.SeedSQLSHA256 {
		t.Fatalf("source/seed.sql sha256 is %s; the latest documented capture used %s. Re-capture the snapshot on Linux as described in %s, record it in fixture_production.json, then run %s", got, latest.SeedSQLSHA256, ledgerFixtureRecaptureDoc, ledgerFixtureRegenerateCommand)
	}
	outDir := t.TempDir()
	for _, exp := range prod.Exports {
		t.Run(exp.Strategy, func(t *testing.T) {
			out := filepath.Join(outDir, exp.Strategy+".json")
			args := []string{"--manifest", manifest, "--partition", exp.Partition, "--strategy", exp.Strategy, "--output", out}
			if rc := runLedgerExport(args); rc != 0 {
				t.Fatalf("runLedgerExport %v exited %d", args, rc)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("read export output: %v", err)
			}
			newDoc, err := ledgerFixtureSplit(got)
			if err != nil {
				t.Fatalf("decode export output: %v", err)
			}
			committedPath := filepath.Join(ledgerFixtureDir, exp.File)
			want, readErr := os.ReadFile(committedPath)
			if readErr != nil && !(regenerate && errors.Is(readErr, fs.ErrNotExist)) {
				t.Fatalf("read committed fixture %s: %v (create it with %s)", exp.File, readErr, ledgerFixtureRegenerateCommand)
			}
			if regenerate {
				if readErr == nil {
					oldDoc, err := ledgerFixtureSplit(want)
					if err != nil {
						t.Fatalf("decode committed fixture %s: %v", exp.File, err)
					}
					if err := ledgerFixtureRegenerationRefusal(oldDoc, newDoc, latest); err != nil {
						t.Fatalf("%s: %v", exp.File, err)
					}
				}
				if err := os.WriteFile(committedPath, got, 0o644); err != nil {
					t.Fatalf("write %s: %v", exp.File, err)
				}
				t.Logf("regenerated %s (%d bytes)", exp.File, len(got))
				return
			}
			oldDoc, err := ledgerFixtureSplit(want)
			if err != nil {
				t.Fatalf("decode committed fixture %s: %v; regenerate with %s", exp.File, err, ledgerFixtureRegenerateCommand)
			}
			schema, version, err := ledgerFixtureSchemaVersion(oldDoc)
			if err != nil || schema != LedgerExportSchema || version != LedgerExportSchemaVersion {
				t.Fatalf("committed fixture %s is schema %q version %d (%v); the exporter writes %q version %d; regenerate with %s", exp.File, schema, version, err, LedgerExportSchema, LedgerExportSchemaVersion, ledgerFixtureRegenerateCommand)
			}
			for _, k := range ledgerFixtureSections {
				if _, ok := newDoc[k]; !ok {
					t.Fatalf("export output lacks top-level section %q", k)
				}
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("export of %s differs from committed %s; changed top-level sections: %s. If the exporter output changed by design, regenerate with %s and review the diff",
					exp.Strategy, exp.File, strings.Join(ledgerFixtureChangedSections(oldDoc, newDoc), ", "), ledgerFixtureRegenerateCommand)
			}
		})
	}
}
