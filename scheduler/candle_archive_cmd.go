package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"time"
)

func runCandleArchiveCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "verify" {
		fmt.Fprintln(stderr, "usage: go-trader candle-archive verify --dir <archive_dir> [--generation <id>] [--max-bytes <n>]")
		return 2
	}
	fs := flag.NewFlagSet("candle-archive verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "Archive directory that holds CURRENT and generations/")
	genFlag := fs.String("generation", "", "Generation id to verify instead of the one CURRENT names")
	maxBytes := fs.Int64("max-bytes", candleArchiveDefaultMaxBytes, "Generation byte limit, as dashboard_candles.max_generation_bytes")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 || *dir == "" {
		fmt.Fprintln(stderr, "usage: go-trader candle-archive verify --dir <archive_dir> [--generation <id>] [--max-bytes <n>]")
		return 2
	}
	root, err := filepath.Abs(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "candle-archive verify: %v\n", err)
		return 2
	}
	id := *genFlag
	if id == "" {
		id, err = readCandleArchiveCurrent(root)
		if err != nil {
			fmt.Fprintf(stderr, "candle-archive verify: refused: read %s: %v\n", candleArchiveCurrentName, err)
			return 1
		}
	}
	gen, err := loadCandleArchiveGeneration(context.Background(), root, id, candleArchiveLimits{MaxBytes: *maxBytes, Deadline: candleArchiveLoadDeadline}, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "candle-archive verify: refused: generation %s: %v\n", id, err)
		return 1
	}
	defer gen.retire()
	fmt.Fprintf(stdout, "generation %s admitted: %d series, %d bytes, publisher %s (%s %s, egress %s), published %s, expires %s\n",
		gen.ID, len(gen.series), gen.Bytes, gen.Publisher.ID, gen.Publisher.Tool, gen.Publisher.Version, gen.Publisher.EgressID,
		gen.PublishedAt.Format(time.RFC3339), gen.ExpiresAt.Format(time.RFC3339))
	if gen.expired(time.Now()) {
		fmt.Fprintf(stdout, "generation %s is past expires_at: display shows it as stale; preview and trade review refuse it\n", gen.ID)
	}
	for _, sid := range gen.identities() {
		s := gen.series[sid]
		fmt.Fprintf(stdout, "  %s bars=%d segments=%d first=%s finalized_through=%s history_start_known=%t source=%s\n",
			sid, s.Bars, len(s.Segments), time.Unix(s.firstOpen(), 0).UTC().Format(time.RFC3339),
			time.Unix(s.FinalizedThrough, 0).UTC().Format(time.RFC3339), s.HistoryStartKnown, s.SourceKind)
	}
	return 0
}
