package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
)

var processPaperOnlyWrites atomic.Bool

func applyProcessVenueWritePolicy(cfg *Config) {
	if cfg != nil && cfg.PaperOnly {
		processPaperOnlyWrites.Store(true)
	}
}

type venueWriteRefusedError struct {
	script string
	detail string
}

func (e *venueWriteRefusedError) Error() string {
	return fmt.Sprintf("venue write via %s refused before launch: %s; no order was sent", e.script, e.detail)
}

func isVenueWriteRefused(err error) bool {
	var refused *venueWriteRefusedError
	return errors.As(err, &refused)
}

var venueWriteDispatchFlags = map[string]bool{
	"--execute":          true,
	"--sync-protection":  true,
	"--update-stop-loss": true,
	"--quote-place":      true,
	"--quote-cancel":     true,
	"--limit-open":       true,
	"--cancel-order":     true,
	"--resolve-order":    true,
}

func venueCloseScript(script string) bool {
	return strings.HasPrefix(filepath.Base(strings.TrimSpace(script)), "close_")
}

func venueArgvDispatchesWrite(script string, args []string) bool {
	if venueCloseScript(script) {
		return true
	}
	for _, a := range args {
		if venueWriteDispatchFlags[a] {
			return true
		}
	}
	return false
}

func venueSideEffectArgvIsRead(script string, args []string) bool {
	if venueCloseScript(script) || len(args) == 0 {
		return false
	}
	has := func(flag string) bool {
		for _, a := range args {
			if a == flag {
				return true
			}
		}
		return false
	}
	switch {
	case has("--list-open-order-oids"), has("--limit-status"):
		for _, a := range args {
			if venueWriteDispatchFlags[a] {
				return false
			}
		}
		return true
	case has("--resolve-order"):
		for _, a := range args {
			if a == "--cancel-open" || (a != "--resolve-order" && venueWriteDispatchFlags[a]) {
				return false
			}
		}
		return true
	}
	return false
}

func venueSideEffectLaunchRefusal(script string, args []string) error {
	if !processPaperOnlyWrites.Load() || venueSideEffectArgvIsRead(script, args) {
		return nil
	}
	return &venueWriteRefusedError{script: script, detail: "this process runs with paper_only, which permits no live venue write"}
}

func venueReadOnlyLaunchRefusal(script string, args []string) error {
	if !venueArgvDispatchesWrite(script, args) {
		return nil
	}
	return &venueWriteRefusedError{script: script, detail: "a read-only launch carried a venue write operation in its arguments"}
}
