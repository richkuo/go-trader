package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
)

func runPairReplayWith(raw []byte, opts basketReplayOptions, out io.Writer) int {
	emit := func(v pairReplayOutput, code int) int {
		enc := json.NewEncoder(out)
		if err := enc.Encode(v); err != nil {
			fmt.Fprintf(os.Stderr, "[basket-replay] could not write the result: %v\n", err)
			return basketReplayExitError
		}
		return code
	}
	fail := func(err error) int {
		o, code := pairReplayErrorOutput(err)
		return emit(o, code)
	}
	if len(raw) > basketReplayMaxSpecBytes {
		return fail(refuseReplay("spec_too_large", fmt.Sprintf("the specification exceeds %d bytes", basketReplayMaxSpecBytes)))
	}
	spec, err := decodePairReplaySpec(raw)
	if err != nil {
		return fail(err)
	}
	lt, err := validatePairReplaySpec(spec)
	if err != nil {
		return fail(err)
	}
	if err := pairReplayRuntimeChecks(spec); err != nil {
		return fail(err)
	}
	rt, err := openPairReplay(spec, raw, lt, opts)
	if err != nil {
		return fail(err)
	}
	defer rt.Close()
	if err := rt.Run(); err != nil {
		return fail(err)
	}
	res, err := rt.output()
	if err != nil {
		return fail(err)
	}
	if res.MechanicallyComplete {
		return emit(res, basketReplayExitOK)
	}
	return emit(res, basketReplayExitError)
}

func pairReplayErrorOutput(err error) (pairReplayOutput, int) {
	out := pairReplayOutput{Protocol: basketReplayProtocolName, Version: pairReplayProtocolVersion}
	var refusal *basketReplayRefusal
	if errors.As(err, &refusal) {
		out.Status, out.Refusal = basketReplayStatusRefused, refusal
		return out, basketReplayExitRefused
	}
	out.Status, out.Error = basketReplayStatusError, err.Error()
	return out, basketReplayExitError
}

func pairReplayRuntimeChecks(spec *pairReplaySpec) error {
	var shape struct {
		Strategies []struct {
			Script   string `json:"script"`
			Platform string `json:"platform"`
		} `json:"strategies"`
	}
	if err := json.Unmarshal(spec.ResearchConfig, &shape); err != nil {
		return refuseReplay("config_refused", err.Error())
	}
	required := []string{".venv/bin/python3", pairPolicyScript}
	probes := [][]string{}
	for _, sc := range shape.Strategies {
		switch sc.Platform {
		case "hyperliquid":
			required = append(required, sc.Script)
			probes = append(probes, append([]string{sc.Script}, hyperliquidMarketCheckProbeArgv...))
		case "okx":
			required = append(required, sc.Script)
			probes = append(probes, append([]string{sc.Script}, okxPairManagementProbeArgv...))
		}
	}
	probes = append(probes, append([]string{pairPolicyScript}, pairPolicyProbeArgv...))
	for _, p := range required {
		if _, err := os.Stat(p); err != nil {
			return refuseReplay("runtime_missing", fmt.Sprintf("the pair replay runs the real member checks and the canonical policy runner from the repository root; %s is missing: %v", p, err))
		}
	}
	for _, p := range probes {
		if err := probeOneCheckScriptFn(p[0], p[1:]); err != nil {
			return refuseReplay("probe_failed", fmt.Sprintf("%s %s: %v", p[0], strings.Join(p[1:], " "), err))
		}
	}
	return nil
}

type pairReplayResponse struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func (w *pairReplayResponse) Header() http.Header         { return w.header }
func (w *pairReplayResponse) Write(b []byte) (int, error) { return w.body.Write(b) }
func (w *pairReplayResponse) WriteHeader(code int)        { w.code = code }

func (r *pairReplayRuntime) serverStatus() (map[string]json.RawMessage, error) {
	ss := NewStatusServer(r.state, r.mu, "", r.cfg.Strategies, r.store)
	ss.priceSymbols, ss.futuresSymbols, ss.hlPerpsCoins, ss.okxPerpsCoins = nil, nil, nil, nil
	req, err := http.NewRequest(http.MethodGet, "/status", nil)
	if err != nil {
		return nil, err
	}
	w := &pairReplayResponse{header: http.Header{}, code: http.StatusOK}
	ss.handleStatus(w, req)
	if w.code != http.StatusOK {
		return nil, fmt.Errorf("the status handler answered %d", w.code)
	}
	var full map[string]json.RawMessage
	if err := json.Unmarshal(w.body.Bytes(), &full); err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	for _, k := range []string{"pair_valuation", "pair_valuation_by_scope", "pair_accounts", "total_value", "total_value_by_scope", "total_notional", "total_notional_by_scope"} {
		if v, ok := full[k]; ok {
			out[k] = v
		}
	}
	var strategies map[string]map[string]json.RawMessage
	if err := json.Unmarshal(full["strategies"], &strategies); err != nil {
		return nil, err
	}
	rows := map[string]map[string]json.RawMessage{}
	for _, sc := range r.members {
		row := map[string]json.RawMessage{}
		for _, k := range []string{"portfolio_value", "pair_valuation", "cash", "partition"} {
			if v, ok := strategies[sc.ID][k]; ok {
				row[k] = v
			}
		}
		rows[sc.ID] = row
	}
	b, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	out["strategies"] = b
	return out, nil
}

var pairReplayExportTables = []struct {
	table   string
	where   string
	exclude []string
}{
	{"strategies", "", nil},
	{"positions", "", nil},
	{"closed_positions", "", nil},
	{"trades", "", []string{"id"}},
	{"portfolio_risk", "", nil},
	{"kill_switch_events", "", []string{"id"}},
	{"basket_contracts", "", []string{"progress_seq"}},
	{"basket_member_state", "", nil},
	{"basket_rebalances", "", nil},
	{"basket_member_actions", "", nil},
	{"entry_order_intents", "", []string{"updated_at"}},
	{"pair_quote_event_uses", "", nil},
	{"pair_funding_events", "", nil},
	{"pair_funding_inventory", "", []string{"id", "seq"}},
	{"pair_virtual_allocations", "", nil},
	{"pair_virtual_events", "kind != 'query'", []string{"seq", "previous_event_hash", "event_hash"}},
}

var pairReplayTradeRowRef = regexp.MustCompile(`"trade_row_id":\s*([0-9]+)`)

func (r *pairReplayRuntime) dumpTable(table, where string) ([]map[string]interface{}, []string, error) {
	ok, err := tableExists(r.file, table)
	if err != nil || !ok {
		return nil, nil, err
	}
	q := "SELECT * FROM " + table
	if where != "" {
		q += " WHERE " + where
	}
	rows, err := r.file.db.Query(q)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	var out []map[string]interface{}
	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		row := map[string]interface{}{}
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				row[c] = string(b)
			} else {
				row[c] = vals[i]
			}
		}
		out = append(out, row)
	}
	return out, cols, rows.Err()
}

type pairReplayAdminIDs struct {
	ids      map[string]string
	ordinals map[string]int
	trades   map[int64]string
}

func pairReplayAdminKind(col string) string {
	switch {
	case strings.Contains(col, "position_id"):
		return "position"
	case strings.Contains(col, "intent_id"):
		return "intent"
	}
	return ""
}

func (a *pairReplayAdminIDs) blank(s string) string {
	keys := make([]string, 0, len(a.ids))
	for id := range a.ids {
		keys = append(keys, id)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	for _, id := range keys {
		if strings.Contains(s, id) {
			s = strings.ReplaceAll(s, id, "<"+a.ids[id]+">")
		}
	}
	return pairReplayTradeRowRef.ReplaceAllString(s, `"trade_row_id":"<trade>"`)
}

func (a *pairReplayAdminIDs) assign(s string) {
	type hit struct {
		at int
		id string
	}
	var hits []hit
	for id := range a.ids {
		if i := strings.Index(s, id); i >= 0 {
			hits = append(hits, hit{i, id})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].at != hits[j].at {
			return hits[i].at < hits[j].at
		}
		return len(hits[i].id) > len(hits[j].id)
	})
	for _, h := range hits {
		if _, ok := a.ordinals[h.id]; ok {
			continue
		}
		kind := a.ids[h.id]
		n := 1
		for id, o := range a.ordinals {
			if a.ids[id] == kind && o >= n {
				n = o + 1
			}
		}
		a.ordinals[h.id] = n
	}
}

func (a *pairReplayAdminIDs) project(s string) string {
	keys := make([]string, 0, len(a.ids))
	for id := range a.ids {
		keys = append(keys, id)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	for _, id := range keys {
		if strings.Contains(s, id) {
			s = strings.ReplaceAll(s, id, fmt.Sprintf("%s#%d", a.ids[id], a.ordinals[id]))
		}
	}
	return pairReplayTradeRowRef.ReplaceAllStringFunc(s, func(m string) string {
		sub := pairReplayTradeRowRef.FindStringSubmatch(m)
		var raw int64
		fmt.Sscan(sub[1], &raw)
		if t, ok := a.trades[raw]; ok {
			return `"trade_row_id":"` + t + `"`
		}
		return `"trade_row_id":"trade#unknown"`
	})
}

func pairReplayRowString(row map[string]interface{}) string {
	b, _ := json.Marshal(row)
	return string(b)
}

func (r *pairReplayRuntime) projection(steps []pairReplayStoredStep) (map[string]interface{}, error) {
	tables := map[string][]map[string]interface{}{}
	admin := &pairReplayAdminIDs{ids: map[string]string{}, ordinals: map[string]int{}, trades: map[int64]string{}}
	tradeIDs := map[string]int64{}
	for _, t := range pairReplayExportTables {
		rows, cols, err := r.dumpTable(t.table, t.where)
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", t.table, err)
		}
		for _, row := range rows {
			for _, c := range cols {
				if kind := pairReplayAdminKind(c); kind != "" {
					if s, ok := row[c].(string); ok && s != "" {
						admin.ids[s] = kind
					}
				}
			}
			if t.table == "basket_contracts" {
				if s, ok := row["progress_json"].(string); ok && s != "" {
					var p map[string]interface{}
					if json.Unmarshal([]byte(s), &p) == nil {
						delete(p, "seq")
						b, _ := json.Marshal(p)
						row["progress_json"] = string(b)
					}
				}
			}
			if t.table == "trades" {
				if id, ok := row["id"].(int64); ok {
					tradeIDs[pairReplayRowString(row)] = id
				}
			}
		}
		tables[t.table] = rows
	}
	for _, t := range pairReplayExportTables {
		for _, row := range tables[t.table] {
			if t.table == "trades" {
				id := tradeIDs[pairReplayRowString(row)]
				for _, c := range t.exclude {
					delete(row, c)
				}
				row["__raw_trade_id"] = id
				continue
			}
			for _, c := range t.exclude {
				delete(row, c)
			}
		}
	}
	order := make([]string, 0, len(pairReplayExportTables))
	for _, t := range pairReplayExportTables {
		order = append(order, t.table)
	}
	for _, name := range order {
		rows := tables[name]
		sort.SliceStable(rows, func(i, j int) bool {
			ri, rj := copyWithout(rows[i], "__raw_trade_id"), copyWithout(rows[j], "__raw_trade_id")
			return admin.blank(pairReplayRowString(ri)) < admin.blank(pairReplayRowString(rj))
		})
		for i, row := range rows {
			if name == "trades" {
				if raw, ok := row["__raw_trade_id"].(int64); ok {
					admin.trades[raw] = fmt.Sprintf("trade#%d", i+1)
				}
				delete(row, "__raw_trade_id")
			}
			admin.assign(pairReplayRowString(row))
		}
	}
	var projected []json.RawMessage
	for _, s := range steps {
		if s.Status != pairReplayStatusComplete {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal(s.Record, &m); err != nil {
			return nil, err
		}
		delete(m, "trace")
		b, _ := json.Marshal(m)
		admin.assign(string(b))
		projected = append(projected, b)
	}
	out := map[string]interface{}{}
	for _, name := range order {
		var list []json.RawMessage
		for _, row := range tables[name] {
			list = append(list, json.RawMessage(admin.project(pairReplayRowString(row))))
		}
		if list == nil {
			list = []json.RawMessage{}
		}
		out[name] = list
	}
	stepsOut := []json.RawMessage{}
	for _, b := range projected {
		stepsOut = append(stepsOut, json.RawMessage(admin.project(string(b))))
	}
	out["steps"] = stepsOut
	return out, nil
}

func copyWithout(m map[string]interface{}, key string) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		if k != key {
			out[k] = v
		}
	}
	return out
}

func (r *pairReplayRuntime) rawChecks(steps []pairReplayStoredStep) ([]string, []string) {
	var ok, bad []string
	for _, s := range steps {
		if s.Status == pairReplayStatusComplete && sha256Hex(s.Record) != s.ResultHash {
			bad = append(bad, fmt.Sprintf("step %d record does not match its result hash", s.Ordinal))
		}
	}
	ok = append(ok, fmt.Sprintf("%d step record hashes", len(steps)))
	rows, _, err := r.dumpTable("pair_virtual_events", "")
	if err != nil {
		bad = append(bad, "virtual venue ledger unreadable: "+err.Error())
	}
	chains := map[[2]string]bool{}
	for _, row := range rows {
		tag, _ := row["run_tag"].(string)
		key, _ := row["member_key_json"].(string)
		chains[[2]string{tag, key}] = true
	}
	keys := make([][2]string, 0, len(chains))
	for k := range chains {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i][0]+keys[i][1] < keys[j][0]+keys[j][1] })
	for _, k := range keys {
		events, err := pairVirtualReadEvents(r.file.db, k[0], k[1])
		if err == nil {
			err = pairVirtualCheckChain(k[0], k[1], events)
		}
		if err != nil {
			bad = append(bad, fmt.Sprintf("virtual venue ledger %s: %v", k[1], err))
		}
	}
	ok = append(ok, fmt.Sprintf("%d virtual venue ledger chains", len(keys)))
	frows, _, err := r.dumpTable("pair_funding_events", "status = 'applied'")
	if err != nil {
		bad = append(bad, "funding events unreadable: "+err.Error())
	}
	for _, row := range frows {
		blob, _ := row["event_json"].(string)
		ev, err := pairDecodeFundingEvent([]byte(blob))
		if err != nil {
			bad = append(bad, fmt.Sprintf("funding event %v: %v", row["event_id"], err))
			continue
		}
		if h, _ := row["evidence_hash"].(string); h != ev.EvidenceHash {
			bad = append(bad, fmt.Sprintf("funding event %v row hash differs from its evidence", row["event_id"]))
		}
	}
	ok = append(ok, fmt.Sprintf("%d applied funding events", len(frows)))
	r.mu.RLock()
	slots := 0
	for _, d := range r.rt.sortedDeadlines() {
		rb := r.rt.Rebalances[d]
		if rb.EvidenceJSON == "" {
			continue
		}
		slots++
		if _, why := verifyPairEnvelope(r.rt, rb); why != "" {
			bad = append(bad, fmt.Sprintf("slot %d envelope: %s", d, why))
		}
	}
	r.mu.RUnlock()
	ok = append(ok, fmt.Sprintf("%d frozen slot envelopes", slots))
	if why := r.verifyTradeStamps(); why != "" {
		bad = append(bad, why)
	}
	ok = append(ok, "member trade stamps")
	return ok, bad
}

func (r *pairReplayRuntime) incompleteReasons(steps []pairReplayStoredStep, rawBad []string) []string {
	var out []string
	add := func(format string, a ...interface{}) { out = append(out, fmt.Sprintf(format, a...)) }
	if !r.finished {
		add("the bounded run did not finish: %s", r.stopReason)
	} else if r.stopReason != "" {
		add("%s", r.stopReason)
	}
	durable, err := r.file.readPairReplayCursor(r.spec.Run.RunID)
	switch {
	case err != nil:
		add("the run cursor is unreadable: %v", err)
	case durable.Applied != r.cursor.Applied || durable.PendingID != "":
		add("the durable cursor (%d applied, pending %q) is not the run's %d completed steps", durable.Applied, durable.PendingID, r.cursor.Applied)
	}
	for _, s := range steps {
		if s.Status != pairReplayStatusComplete {
			add("step %d (%s) is still %s in phase %s", s.Ordinal, s.StepID, s.Status, s.Phase)
		}
	}
	for _, f := range r.failures {
		if f.Stage == pairReplayStagePolicy || f.Stage == pairReplayStageManagement {
			add("%s %s at step %d: %s", f.Stage, f.Kind, f.Ordinal, f.Cause)
		}
	}
	out = append(out, rawBad...)
	r.mu.RLock()
	rt := r.rt
	if o, ok := pairIncompleteEvidence(rt); ok {
		add("pair incomplete evidence: %s %s %s", o.Kind, o.MemberID, o.Detail)
	}
	for _, o := range rt.Pair.Progress.Obligations {
		add("open pair obligation %s (member %q, action %q): %s", o.Kind, o.MemberID, o.ActionID, o.Detail)
	}
	for _, a := range rt.pairUnresolvedActions() {
		add("action %s is %s at the end of the run", a.ActionID, a.Status)
	}
	for _, id := range rt.sortedActionIDs() {
		a := rt.Actions[id]
		if a.Status == basketActionHeld {
			continue
		}
		if a.ConfirmedQty-a.AppliedQty > 1e-12 {
			add("action %s has confirmed %.10g but applied %.10g", a.ActionID, a.ConfirmedQty, a.AppliedQty)
		}
	}
	for _, d := range rt.sortedDeadlines() {
		rb := rt.Rebalances[d]
		if rb.Status == basketRebalanceReserved || rb.Status == basketRebalanceRanking || (rb.Status == basketRebalanceFrozen && rb.TargetsJSON == "") {
			add("slot %d is %s and undecided at the end of the run", d, rb.Status)
		}
	}
	if rt.needsDurable {
		add("pair settlement is not durable at the end of the run")
	}
	if l := rt.Pair.Progress.Lifecycle; l == pairLifecycleRecovery || l == pairLifecycleOperatorHeld {
		add("the pair ends in lifecycle %s", l)
	}
	if why := r.bc.pairVirtualConservation(rt); why != "" {
		add("virtual inventory conservation: %s", why)
	}
	if pend := pairEmergencyPendingFor(r.state, r.partition); len(pend) > 0 {
		add("emergency closes are pending for %s", strings.Join(pend, ", "))
	}
	pm := r.bc.pairMarksLocked()
	for _, sc := range r.members {
		if _, st := portfolioValueMarked(r.state.Strategies[sc.ID], map[string]float64{}, pm); !st.complete() {
			add("member %s valuation is incomplete at the end of the run: %s", sc.ID, st.label())
		}
	}
	r.mu.RUnlock()
	if rows, _, err := r.dumpTable("pair_funding_events", "status = 'incomplete'"); err != nil {
		add("funding events unreadable: %v", err)
	} else {
		for _, row := range rows {
			add("funding event %v of %v is incomplete: %v", row["event_id"], row["strategy_id"], row["reason"])
		}
	}
	sort.Strings(out)
	return out
}

func (r *pairReplayRuntime) output() (pairReplayOutput, error) {
	out := pairReplayOutput{Protocol: basketReplayProtocolName, Version: pairReplayProtocolVersion, ClockModel: pairReplayClockNote, EventOrder: pairReplayEventOrder,
		AdministrativeFields: pairReplayAdministrativeFields, MemberCosts: r.costs}
	out.Run = &pairReplayRunInfo{RunID: r.spec.Run.RunID, Mode: r.spec.Mode, StateDir: r.spec.Run.StateDir, Fingerprints: r.fp, Lifetime: r.lt, StepsApplied: r.cursor.Applied,
		ResumedAt: r.resumedAt, Resumed: r.resumed, CursorMs: r.cursor.CursorMs, CursorStepID: r.cursor.CursorStepID, NextWakeMs: r.nextAt, Finished: r.finished, StopReason: r.stopReason}
	steps, err := r.file.readPairReplaySteps(r.spec.Run.RunID)
	if err != nil {
		return out, err
	}
	for _, s := range steps {
		if s.Status == pairReplayStatusComplete {
			out.Steps = append(out.Steps, s.Record)
		}
	}
	failures, err := r.file.readPairReplayFailures(r.spec.Run.RunID)
	if err != nil {
		return out, err
	}
	r.failMu.Lock()
	r.failures = failures
	r.failMu.Unlock()
	out.Failures = failures
	status, err := r.serverStatus()
	if err != nil {
		return out, fmt.Errorf("the server status owner could not report the run: %w", err)
	}
	out.ServerStatus = status
	okChecks, bad := r.rawChecks(steps)
	out.IncompleteReasons = r.incompleteReasons(steps, bad)
	out.MechanicallyComplete = len(out.IncompleteReasons) == 0
	proj, err := r.projection(steps)
	if err != nil {
		return out, err
	}
	out.Export = proj
	canon, err := json.Marshal(proj)
	if err != nil {
		return out, err
	}
	cmp := &pairReplayComparison{Hash: sha256Hex(canon), RawCheck: okChecks, Reasons: append([]string(nil), r.equality...)}
	if !out.MechanicallyComplete {
		cmp.Reasons = append(cmp.Reasons, "the run is not mechanically complete, so it makes no equality claim")
	}
	cmp.Eligible = len(cmp.Reasons) == 0
	out.Comparison = cmp
	switch {
	case !r.finished:
		out.Status = basketReplayStatusStopped
	case out.MechanicallyComplete:
		out.Status = basketReplayStatusComplete
	default:
		out.Status = basketReplayStatusIncomplete
	}
	return out, nil
}
