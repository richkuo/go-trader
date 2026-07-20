package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

const (
	tuningPromotionJournalFile = "promotions.json"
	tuningApplyBodyLimit       = 4 << 10 // 4 KiB — identity triple only
	tuningResultsSchemaV2      = 2

	tuningPromoPending      = "pending"
	tuningPromoApplied      = "applied"
	tuningPromoManualReview = "manual_review"

	tuningEligEligible          = "eligible"
	tuningEligAlreadyApplied    = "already_applied"
	tuningEligBaselineDrifted   = "baseline_drifted"
	tuningEligLegacyArtifact    = "legacy_artifact"
	tuningEligNotSurvivor       = "not_survivor"
	tuningEligConfigUnavailable = "config_unavailable"

	tuningReasonApplied        = "applied"
	tuningReasonAlreadyApplied = "already_applied"
	tuningReasonBaselineDrift  = "baseline_drifted"
	tuningReasonLegacy         = "legacy_artifact"
	tuningReasonNotSurvivor    = "not_survivor"
	tuningReasonManualReview   = "manual_review"
	tuningReasonRunMissing     = "run_not_found"
	tuningReasonNotCompleted   = "run_not_completed"
	tuningReasonBadRequest     = "bad_request"
	tuningReasonConflict       = "conflict"
)

var (
	errTuningBaselineDrifted = errors.New("tuning promotion baseline drifted")
	errTuningStrategyMissing = errors.New("tuning promotion strategy not found")
	errTuningStrategyDup     = errors.New("tuning promotion strategy id is ambiguous")
)

type tuningApplyRequest struct {
	RunID         string `json:"run_id"`
	StrategyID    string `json:"strategy_id"`
	SuggestionKey string `json:"suggestion_key"`
}

type tuningPromotionBaseline struct {
	OpenStrategy             json.RawMessage `json:"open_strategy"`
	OpenStrategyPresent      bool            `json:"open_strategy_present"`
	UserDefaults             json.RawMessage `json:"user_defaults"`
	UserDefaultsPresent      bool            `json:"user_defaults_present"`
	UserCloseDefaults        json.RawMessage `json:"user_close_defaults"`
	UserCloseDefaultsPresent bool            `json:"user_close_defaults_present"`
}

type tuningPromotionRecord struct {
	RunID         string     `json:"run_id"`
	StrategyID    string     `json:"strategy_id"`
	SuggestionKey string     `json:"suggestion_key"`
	State         string     `json:"state"`
	PatchHash     string     `json:"patch_hash"`
	CreatedAt     time.Time  `json:"created_at"`
	AppliedAt     *time.Time `json:"applied_at,omitempty"`
	Reason        string     `json:"reason,omitempty"`
}

type tuningPromotionJournal struct {
	Promotions []tuningPromotionRecord `json:"promotions"`
}

type tuningApplyTarget struct {
	RunID         string
	StrategyID    string
	SuggestionKey string
	Patch         json.RawMessage
	PatchHash     string
	Baseline      tuningPromotionBaseline
}

func (m *tuningRunManager) promotionsPath() string {
	return filepath.Join(m.rootDir, tuningPromotionJournalFile)
}

func tuningPromotionKey(runID, strategyID, suggestionKey string) string {
	return runID + "\x00" + strategyID + "\x00" + suggestionKey
}

func (m *tuningRunManager) beginApplyInflight(key string) (func(), error) {
	m.journalMu.Lock()
	for {
		if ch, ok := m.applyInflight[key]; ok {
			m.journalMu.Unlock()
			<-ch
			m.journalMu.Lock()
			continue
		}
		ch := make(chan struct{})
		m.applyInflight[key] = ch
		m.journalMu.Unlock()
		return func() {
			m.journalMu.Lock()
			delete(m.applyInflight, key)
			close(ch)
			m.journalMu.Unlock()
		}, nil
	}
}

func (m *tuningRunManager) loadPromotionJournal() (tuningPromotionJournal, error) {
	var journal tuningPromotionJournal
	err := readTuningJSON(m.promotionsPath(), &journal)
	if errors.Is(err, os.ErrNotExist) {
		return tuningPromotionJournal{Promotions: nil}, nil
	}
	if err != nil {
		return tuningPromotionJournal{}, err
	}
	if journal.Promotions == nil {
		journal.Promotions = []tuningPromotionRecord{}
	}
	return journal, nil
}

func (m *tuningRunManager) storePromotionJournal(journal tuningPromotionJournal) error {
	if journal.Promotions == nil {
		journal.Promotions = []tuningPromotionRecord{}
	}
	return writeTuningJSON(m.promotionsPath(), journal)
}

func (m *tuningRunManager) findPromotionLocked(journal *tuningPromotionJournal, runID, strategyID, suggestionKey string) int {
	for i := range journal.Promotions {
		rec := &journal.Promotions[i]
		if rec.RunID == runID && rec.StrategyID == strategyID && rec.SuggestionKey == suggestionKey {
			return i
		}
	}
	return -1
}

func (m *tuningRunManager) upsertPromotion(rec tuningPromotionRecord) error {
	m.journalMu.Lock()
	defer m.journalMu.Unlock()
	journal, err := m.loadPromotionJournal()
	if err != nil {
		return err
	}
	if idx := m.findPromotionLocked(&journal, rec.RunID, rec.StrategyID, rec.SuggestionKey); idx >= 0 {
		journal.Promotions[idx] = rec
	} else {
		journal.Promotions = append(journal.Promotions, rec)
	}
	return m.storePromotionJournal(journal)
}

func (m *tuningRunManager) getPromotion(runID, strategyID, suggestionKey string) (tuningPromotionRecord, bool, error) {
	m.journalMu.Lock()
	defer m.journalMu.Unlock()
	journal, err := m.loadPromotionJournal()
	if err != nil {
		return tuningPromotionRecord{}, false, err
	}
	idx := m.findPromotionLocked(&journal, runID, strategyID, suggestionKey)
	if idx < 0 {
		return tuningPromotionRecord{}, false, nil
	}
	return journal.Promotions[idx], true, nil
}

func tuningResultsSchemaVersion(results map[string]any) (int, error) {
	raw, ok := results["schema_version"]
	if !ok {
		return 0, errors.New("missing schema_version")
	}
	switch v := raw.(type) {
	case float64:
		return int(v), nil
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, fmt.Errorf("schema_version: %w", err)
		}
		return int(n), nil
	case int:
		return v, nil
	case int64:
		return int(v), nil
	default:
		return 0, fmt.Errorf("schema_version has unsupported type %T", raw)
	}
}

func findTuningStrategyEntry(results map[string]any, strategyID string) (map[string]any, error) {
	list, ok := results["strategies"].([]any)
	if !ok {
		return nil, errors.New("results missing strategies array")
	}
	var found map[string]any
	for _, raw := range list {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := entry["strategy_id"].(string)
		if id != strategyID {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%w: %s", errTuningStrategyDup, strategyID)
		}
		found = entry
	}
	if found == nil {
		return nil, fmt.Errorf("%w: %s", errTuningStrategyMissing, strategyID)
	}
	return found, nil
}

func findTuningRankedRow(strategy map[string]any, suggestionKey string) (map[string]any, error) {
	ranked, ok := strategy["ranked"].([]any)
	if !ok {
		return nil, errors.New("strategy missing ranked array")
	}
	var found map[string]any
	for _, raw := range ranked {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		key, _ := row["key"].(string)
		if key != suggestionKey {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("ambiguous suggestion key %q", suggestionKey)
		}
		found = row
	}
	if found == nil {
		return nil, fmt.Errorf("suggestion key %q not found", suggestionKey)
	}
	return found, nil
}

func decodePromotionBaseline(raw any) (tuningPromotionBaseline, error) {
	var baseline tuningPromotionBaseline
	if raw == nil {
		return baseline, errors.New("promotion_baseline missing")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return baseline, err
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &probe); err != nil {
		return baseline, fmt.Errorf("promotion_baseline: %w", err)
	}
	required := []string{
		"open_strategy_present", "user_defaults_present", "user_close_defaults_present",
		"open_strategy", "user_defaults", "user_close_defaults",
	}
	for _, key := range required {
		if _, ok := probe[key]; !ok {
			return baseline, fmt.Errorf("promotion_baseline missing %s", key)
		}
	}
	if err := json.Unmarshal(encoded, &baseline); err != nil {
		return baseline, fmt.Errorf("promotion_baseline: %w", err)
	}
	return baseline, nil
}

func extractOpenStrategyPatch(row map[string]any) (json.RawMessage, error) {
	patchRaw, ok := row["patch"]
	if !ok || patchRaw == nil {
		return nil, errors.New("ranked row missing patch")
	}
	encoded, err := json.Marshal(patchRaw)
	if err != nil {
		return nil, err
	}
	var patch struct {
		OpenStrategy json.RawMessage `json:"open_strategy"`
	}
	if err := json.Unmarshal(encoded, &patch); err != nil {
		return nil, fmt.Errorf("patch: %w", err)
	}
	if len(patch.OpenStrategy) == 0 || string(patch.OpenStrategy) == "null" {
		return nil, errors.New("patch.open_strategy missing")
	}
	var probe any
	if err := json.Unmarshal(patch.OpenStrategy, &probe); err != nil {
		return nil, fmt.Errorf("patch.open_strategy: %w", err)
	}
	canon, err := json.Marshal(probe)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(canon), nil
}

func canonicalJSONEqual(a, b json.RawMessage) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if string(a) == "null" && string(b) == "null" {
		return true
	}
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

func promotionBaselinesEqual(a, b tuningPromotionBaseline) bool {
	if a.OpenStrategyPresent != b.OpenStrategyPresent ||
		a.UserDefaultsPresent != b.UserDefaultsPresent ||
		a.UserCloseDefaultsPresent != b.UserCloseDefaultsPresent {
		return false
	}
	if a.OpenStrategyPresent && !canonicalJSONEqual(a.OpenStrategy, b.OpenStrategy) {
		return false
	}
	if a.UserDefaultsPresent && !canonicalJSONEqual(a.UserDefaults, b.UserDefaults) {
		return false
	}
	if a.UserCloseDefaultsPresent && !canonicalJSONEqual(a.UserCloseDefaults, b.UserCloseDefaults) {
		return false
	}
	return true
}

func patchSHA256(patch json.RawMessage) (string, error) {
	var v any
	if err := json.Unmarshal(patch, &v); err != nil {
		return "", err
	}
	canon, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

func rawFieldPresence(root map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	raw, ok := root[key]
	if !ok {
		return nil, false
	}
	return append(json.RawMessage(nil), raw...), true
}

func extractLivePromotionBaseline(root map[string]json.RawMessage, strategyID string) (tuningPromotionBaseline, error) {
	list, err := configStrategies(root)
	if err != nil {
		return tuningPromotionBaseline{}, err
	}
	matchIdx := -1
	for i, raw := range list {
		if strategyRawID(raw) != strategyID {
			continue
		}
		if matchIdx >= 0 {
			return tuningPromotionBaseline{}, fmt.Errorf("%w: %s", errTuningStrategyDup, strategyID)
		}
		matchIdx = i
	}
	if matchIdx < 0 {
		return tuningPromotionBaseline{}, fmt.Errorf("%w: %s", errTuningStrategyMissing, strategyID)
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal(list[matchIdx], &item); err != nil {
		return tuningPromotionBaseline{}, fmt.Errorf("parse strategy %s: %w", strategyID, err)
	}
	openRaw, openPresent := rawFieldPresence(item, "open_strategy")
	udRaw, udPresent := rawFieldPresence(root, "user_defaults")
	ucdRaw, ucdPresent := rawFieldPresence(root, "user_close_defaults")
	return tuningPromotionBaseline{
		OpenStrategy:             openRaw,
		OpenStrategyPresent:      openPresent,
		UserDefaults:             udRaw,
		UserDefaultsPresent:      udPresent,
		UserCloseDefaults:        ucdRaw,
		UserCloseDefaultsPresent: ucdPresent,
	}, nil
}

func extractLiveOpenStrategy(root map[string]json.RawMessage, strategyID string) (json.RawMessage, bool, error) {
	baseline, err := extractLivePromotionBaseline(root, strategyID)
	if err != nil {
		return nil, false, err
	}
	return baseline.OpenStrategy, baseline.OpenStrategyPresent, nil
}

// replaceStrategyOpenStrategy sets the target strategy's open_strategy to patch
// verbatim. It refuses zero or duplicate strategy id matches.
func replaceStrategyOpenStrategy(root map[string]json.RawMessage, strategyID string, patch json.RawMessage) error {
	if strategyID == "" {
		return errors.New("strategy id is required")
	}
	if len(patch) == 0 || string(patch) == "null" {
		return errors.New("open_strategy patch is required")
	}
	list, err := configStrategies(root)
	if err != nil {
		return err
	}
	matchIdx := -1
	for i, raw := range list {
		if strategyRawID(raw) != strategyID {
			continue
		}
		if matchIdx >= 0 {
			return fmt.Errorf("%w: %s", errTuningStrategyDup, strategyID)
		}
		matchIdx = i
	}
	if matchIdx < 0 {
		return fmt.Errorf("%w: %s", errTuningStrategyMissing, strategyID)
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal(list[matchIdx], &item); err != nil {
		return fmt.Errorf("parse strategy %s: %w", strategyID, err)
	}
	item["open_strategy"] = append(json.RawMessage(nil), patch...)
	nb, err := json.Marshal(item)
	if err != nil {
		return err
	}
	list[matchIdx] = nb
	return setConfigStrategies(root, list)
}

func readConfigRootMap(configPath string) (map[string]json.RawMessage, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return root, nil
}

func (m *tuningRunManager) resolveApplyTarget(runID, strategyID, suggestionKey string) (tuningApplyTarget, string, error) {
	if !validTuningRunID(runID) {
		return tuningApplyTarget{}, tuningReasonRunMissing, os.ErrNotExist
	}
	rec, ok := m.record(runID)
	if !ok {
		return tuningApplyTarget{}, tuningReasonRunMissing, os.ErrNotExist
	}
	runDir := filepath.Join(m.rootDir, runID)
	if _, err := os.Stat(runDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return tuningApplyTarget{}, tuningReasonManualReview, fmt.Errorf("run directory pruned")
		}
		return tuningApplyTarget{}, tuningReasonConflict, err
	}
	if rec.Status != tuningRunCompleted {
		return tuningApplyTarget{}, tuningReasonNotCompleted, fmt.Errorf("run status is %s", rec.Status)
	}
	results, err := readTuningResults(filepath.Join(runDir, tuningRunResultsFile))
	if err != nil {
		return tuningApplyTarget{}, tuningReasonLegacy, err
	}
	schema, err := tuningResultsSchemaVersion(results)
	if err != nil || schema < tuningResultsSchemaV2 {
		return tuningApplyTarget{}, tuningReasonLegacy, errors.New("artifact schema below v2")
	}
	strategy, err := findTuningStrategyEntry(results, strategyID)
	if err != nil {
		return tuningApplyTarget{}, tuningReasonBadRequest, err
	}
	baseline, err := decodePromotionBaseline(strategy["promotion_baseline"])
	if err != nil {
		return tuningApplyTarget{}, tuningReasonLegacy, err
	}
	row, err := findTuningRankedRow(strategy, suggestionKey)
	if err != nil {
		return tuningApplyTarget{}, tuningReasonBadRequest, err
	}
	verdict, _ := row["verdict"].(string)
	key, _ := row["key"].(string)
	if verdict != "survivor" || key == "baseline" {
		return tuningApplyTarget{}, tuningReasonNotSurvivor, errors.New("suggestion is not a promotable survivor")
	}
	patch, err := extractOpenStrategyPatch(row)
	if err != nil {
		return tuningApplyTarget{}, tuningReasonNotSurvivor, err
	}
	hash, err := patchSHA256(patch)
	if err != nil {
		return tuningApplyTarget{}, tuningReasonConflict, err
	}
	return tuningApplyTarget{
		RunID:         runID,
		StrategyID:    strategyID,
		SuggestionKey: suggestionKey,
		Patch:         patch,
		PatchHash:     hash,
		Baseline:      baseline,
	}, "", nil
}

func computeApplyEligibility(runStatus tuningRunStatus, results map[string]any, strategyID, suggestionKey string, liveRoot map[string]json.RawMessage, journalRec tuningPromotionRecord, hasJournal bool) (elig string, appliedAt *time.Time) {
	if runStatus != tuningRunCompleted {
		return tuningEligNotSurvivor, nil
	}
	schema, err := tuningResultsSchemaVersion(results)
	if err != nil || schema < tuningResultsSchemaV2 {
		return tuningEligLegacyArtifact, nil
	}
	strategy, err := findTuningStrategyEntry(results, strategyID)
	if err != nil {
		return tuningEligNotSurvivor, nil
	}
	if _, err := decodePromotionBaseline(strategy["promotion_baseline"]); err != nil {
		return tuningEligLegacyArtifact, nil
	}
	row, err := findTuningRankedRow(strategy, suggestionKey)
	if err != nil {
		return tuningEligNotSurvivor, nil
	}
	verdict, _ := row["verdict"].(string)
	key, _ := row["key"].(string)
	if verdict != "survivor" || key == "baseline" {
		return tuningEligNotSurvivor, nil
	}
	if _, err := extractOpenStrategyPatch(row); err != nil {
		return tuningEligNotSurvivor, nil
	}
	if liveRoot == nil {
		return tuningEligConfigUnavailable, nil
	}
	if hasJournal {
		switch journalRec.State {
		case tuningPromoApplied:
			return tuningEligAlreadyApplied, journalRec.AppliedAt
		case tuningPromoManualReview:
			return tuningEligBaselineDrifted, nil
		case tuningPromoPending:
			patch, err := extractOpenStrategyPatch(row)
			if err != nil {
				return tuningEligNotSurvivor, nil
			}
			liveOpen, present, err := extractLiveOpenStrategy(liveRoot, strategyID)
			if err != nil {
				return tuningEligConfigUnavailable, nil
			}
			if present && canonicalJSONEqual(liveOpen, patch) {
				return tuningEligAlreadyApplied, journalRec.AppliedAt
			}
		}
	}
	baseline, err := decodePromotionBaseline(strategy["promotion_baseline"])
	if err != nil {
		return tuningEligLegacyArtifact, nil
	}
	liveBaseline, err := extractLivePromotionBaseline(liveRoot, strategyID)
	if err != nil {
		return tuningEligConfigUnavailable, nil
	}
	if !promotionBaselinesEqual(baseline, liveBaseline) {
		return tuningEligBaselineDrifted, nil
	}
	return tuningEligEligible, nil
}

func overlayTuningApplyEligibility(detail *tuningRunDetail, configPath string, m *tuningRunManager) {
	if detail == nil || detail.Results == nil {
		return
	}
	strategies, ok := detail.Results["strategies"].([]any)
	if !ok {
		return
	}
	liveRoot, liveErr := readConfigRootMap(configPath)
	var liveRootOK map[string]json.RawMessage
	if liveErr == nil {
		liveRootOK = liveRoot
	}
	for _, raw := range strategies {
		strategy, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		strategyID, _ := strategy["strategy_id"].(string)
		ranked, ok := strategy["ranked"].([]any)
		if !ok {
			continue
		}
		for _, rowRaw := range ranked {
			row, ok := rowRaw.(map[string]any)
			if !ok {
				continue
			}
			key, _ := row["key"].(string)
			rec, hasJournal, err := m.getPromotion(detail.Run.ID, strategyID, key)
			if err != nil {
				row["apply_eligibility"] = tuningEligConfigUnavailable
				continue
			}
			elig, appliedAt := computeApplyEligibility(detail.Run.Status, detail.Results, strategyID, key, liveRootOK, rec, hasJournal)
			row["apply_eligibility"] = elig
			if appliedAt != nil {
				row["applied_at"] = appliedAt.UTC().Format(time.RFC3339Nano)
			} else if hasJournal && rec.State == tuningPromoApplied && rec.AppliedAt != nil {
				row["applied_at"] = rec.AppliedAt.UTC().Format(time.RFC3339Nano)
			}
		}
	}
}

func (ss *StatusServer) applyTuningPromotion(target tuningApplyTarget) (reason string, appliedAt time.Time, err error) {
	key := tuningPromotionKey(target.RunID, target.StrategyID, target.SuggestionKey)
	release, err := ss.tuning.beginApplyInflight(key)
	if err != nil {
		return tuningReasonConflict, time.Time{}, err
	}
	defer release()

	if rec, ok, err := ss.tuning.getPromotion(target.RunID, target.StrategyID, target.SuggestionKey); err != nil {
		return tuningReasonConflict, time.Time{}, err
	} else if ok {
		switch rec.State {
		case tuningPromoApplied:
			at := ss.tuning.now()
			if rec.AppliedAt != nil {
				at = *rec.AppliedAt
			}
			return tuningReasonAlreadyApplied, at, nil
		case tuningPromoManualReview:
			return tuningReasonManualReview, time.Time{}, errors.New("promotion requires manual review")
		case tuningPromoPending:
			return ss.recoverPendingTuningPromotion(target, rec)
		}
	}

	// Optimistic pre-check avoids journaling an obvious drift. The in-transaction
	// compare under configWriteMu remains the authoritative gate.
	if root, err := readConfigRootMap(ss.configPath); err != nil {
		return tuningReasonConflict, time.Time{}, err
	} else if liveBaseline, err := extractLivePromotionBaseline(root, target.StrategyID); err != nil {
		return tuningReasonConflict, time.Time{}, err
	} else if !promotionBaselinesEqual(target.Baseline, liveBaseline) {
		return tuningReasonBaselineDrift, time.Time{}, errTuningBaselineDrifted
	}

	now := ss.tuning.now()
	pending := tuningPromotionRecord{
		RunID:         target.RunID,
		StrategyID:    target.StrategyID,
		SuggestionKey: target.SuggestionKey,
		State:         tuningPromoPending,
		PatchHash:     target.PatchHash,
		CreatedAt:     now,
	}
	if err := ss.tuning.upsertPromotion(pending); err != nil {
		return tuningReasonConflict, time.Time{}, err
	}
	return ss.commitTuningPromotion(target, pending)
}

func (ss *StatusServer) recoverPendingTuningPromotion(target tuningApplyTarget, rec tuningPromotionRecord) (string, time.Time, error) {
	if _, err := os.Stat(filepath.Join(ss.tuning.rootDir, target.RunID)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			rec.State = tuningPromoManualReview
			rec.Reason = "run directory pruned while promotion was pending"
			_ = ss.tuning.upsertPromotion(rec)
			return tuningReasonManualReview, time.Time{}, errors.New(rec.Reason)
		}
		return tuningReasonConflict, time.Time{}, err
	}
	root, err := readConfigRootMap(ss.configPath)
	if err != nil {
		return tuningReasonConflict, time.Time{}, err
	}
	liveOpen, present, err := extractLiveOpenStrategy(root, target.StrategyID)
	if err != nil {
		return tuningReasonConflict, time.Time{}, err
	}
	if present && canonicalJSONEqual(liveOpen, target.Patch) {
		now := ss.tuning.now()
		rec.State = tuningPromoApplied
		rec.AppliedAt = &now
		rec.Reason = ""
		if err := ss.tuning.upsertPromotion(rec); err != nil {
			return tuningReasonConflict, time.Time{}, err
		}
		return tuningReasonAlreadyApplied, now, nil
	}
	liveBaseline, err := extractLivePromotionBaseline(root, target.StrategyID)
	if err != nil {
		return tuningReasonConflict, time.Time{}, err
	}
	if !promotionBaselinesEqual(target.Baseline, liveBaseline) {
		rec.State = tuningPromoManualReview
		rec.Reason = "baseline drifted while promotion was pending"
		_ = ss.tuning.upsertPromotion(rec)
		return tuningReasonManualReview, time.Time{}, errTuningBaselineDrifted
	}
	return ss.commitTuningPromotion(target, rec)
}

func (ss *StatusServer) commitTuningPromotion(target tuningApplyTarget, pending tuningPromotionRecord) (string, time.Time, error) {
	err := ss.mutateConfigRoot(func(root map[string]json.RawMessage) error {
		liveBaseline, err := extractLivePromotionBaseline(root, target.StrategyID)
		if err != nil {
			return err
		}
		if !promotionBaselinesEqual(target.Baseline, liveBaseline) {
			return errTuningBaselineDrifted
		}
		return replaceStrategyOpenStrategy(root, target.StrategyID, target.Patch)
	})
	if errors.Is(err, errTuningBaselineDrifted) {
		pending.State = tuningPromoManualReview
		pending.Reason = "baseline drifted during apply"
		_ = ss.tuning.upsertPromotion(pending)
		return tuningReasonBaselineDrift, time.Time{}, errTuningBaselineDrifted
	}
	if err != nil {
		return tuningReasonConflict, time.Time{}, err
	}
	now := ss.tuning.now()
	pending.State = tuningPromoApplied
	pending.AppliedAt = &now
	pending.Reason = ""
	if err := ss.tuning.upsertPromotion(pending); err != nil {
		return tuningReasonConflict, time.Time{}, err
	}
	return tuningReasonApplied, now, nil
}

func writeTuningApplyResponse(w http.ResponseWriter, status int, reason string, appliedAt time.Time, errMsg string) {
	payload := map[string]any{"reason": reason}
	if !appliedAt.IsZero() {
		payload["applied_at"] = appliedAt.UTC().Format(time.RFC3339Nano)
	}
	if errMsg != "" {
		payload["error"] = errMsg
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (ss *StatusServer) handleAPITuningApply(w http.ResponseWriter, r *http.Request) {
	if ss.rejectIfDraining(w) {
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !ss.requireMutatingAPIAuth(w, r) || !requireJSONContentType(w, r) || !requireSameOrigin(w, r) {
		return
	}
	if ss.tuning == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "tuning service unavailable")
		return
	}
	if strings.TrimSpace(ss.configPath) == "" {
		writeTuningApplyResponse(w, http.StatusServiceUnavailable, tuningEligConfigUnavailable, time.Time{}, "config path not configured")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, tuningApplyBodyLimit)
	var req tuningApplyRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeTuningApplyResponse(w, http.StatusBadRequest, tuningReasonBadRequest, time.Time{}, "invalid json body: "+err.Error())
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeTuningApplyResponse(w, http.StatusBadRequest, tuningReasonBadRequest, time.Time{}, "invalid json body: trailing data")
		return
	}
	req.RunID = strings.TrimSpace(req.RunID)
	req.StrategyID = strings.TrimSpace(req.StrategyID)
	req.SuggestionKey = strings.TrimSpace(req.SuggestionKey)
	if req.RunID == "" || req.StrategyID == "" || req.SuggestionKey == "" {
		writeTuningApplyResponse(w, http.StatusBadRequest, tuningReasonBadRequest, time.Time{}, "run_id, strategy_id, and suggestion_key are required")
		return
	}

	target, refuseReason, err := ss.tuning.resolveApplyTarget(req.RunID, req.StrategyID, req.SuggestionKey)
	if err != nil {
		if refuseReason == tuningReasonManualReview {
			if rec, ok, jerr := ss.tuning.getPromotion(req.RunID, req.StrategyID, req.SuggestionKey); jerr == nil && ok && rec.State == tuningPromoPending {
				rec.State = tuningPromoManualReview
				rec.Reason = err.Error()
				_ = ss.tuning.upsertPromotion(rec)
			}
		}
		status := http.StatusBadRequest
		switch refuseReason {
		case tuningReasonRunMissing:
			status = http.StatusNotFound
		case tuningReasonNotCompleted, tuningReasonNotSurvivor, tuningReasonLegacy, tuningReasonManualReview:
			status = http.StatusConflict
		case tuningReasonConflict:
			status = http.StatusInternalServerError
		}
		writeTuningApplyResponse(w, status, refuseReason, time.Time{}, err.Error())
		return
	}

	reason, appliedAt, err := ss.applyTuningPromotion(target)
	if err != nil {
		status := http.StatusConflict
		if reason == tuningReasonConflict {
			status = http.StatusInternalServerError
		}
		writeTuningApplyResponse(w, status, reason, time.Time{}, err.Error())
		return
	}
	writeTuningApplyResponse(w, http.StatusOK, reason, appliedAt, "")
}
