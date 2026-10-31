//go:build pyintegration

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func pairPaperBookServer(t *testing.T, f *pairPaperFixture) (string, *atomic.Int64) {
	t.Helper()
	return pairPaperBookServerWith(t, f, pairBookTestFrame)
}

func pairBookTestTightFrame(at int64, bid string) []byte {
	var raw map[string]any
	_ = json.Unmarshal(pairBookTestFrame(at, bid), &raw)
	levels := raw["data"].(map[string]any)["levels"].([]any)
	asks := levels[1].([]any)
	v, _ := strconv.ParseFloat(bid, 64)
	for i := range asks {
		asks[i].(map[string]any)["px"] = strconv.FormatFloat(v+20+float64(i), 'f', -1, 64)
	}
	return pairPaperJSON(raw)
}

func pairPaperBookServerWith(t *testing.T, f *pairPaperFixture, frame func(int64, string) []byte) (string, *atomic.Int64) {
	t.Helper()
	connections := &atomic.Int64{}
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		connections.Add(1)
		var sub map[string]any
		if conn.ReadJSON(&sub) != nil {
			return
		}
		if conn.WriteJSON(map[string]any{"channel": "subscriptionResponse", "data": sub}) != nil {
			return
		}
		var last int64
		for {
			blob, err := os.ReadFile(f.clock)
			if err != nil {
				return
			}
			at, err := strconv.ParseInt(strings.TrimSpace(string(blob)), 10, 64)
			if err != nil {
				return
			}
			if at-last >= 250 {
				bid := "60000"
				if at >= f.fromMs+8000 {
					bid = "59998"
				}
				if err = conn.WriteMessage(websocket.TextMessage, frame(at, bid)); err != nil {
					return
				}
				last = at
			}
			time.Sleep(time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), connections
}

func TestBasketReplayPairPaperBooksProcessSealsAndReproduces(t *testing.T) {
	f := newPairPaperFixture(t, pairPaperOptions2{runID: "paper-books", collectS: 16, drainS: 8, convergeS: 10, interval: 4, mutate: func(root map[string]interface{}) {
		pairTestMemberEntry(root, 0)["source"] = pairSourceSealedFeed
		pairTestPair(root)["book_need"].(map[string]interface{})["window_ms"] = 1000
	}})
	f.scenario.PaperFundingPollMs = 1000
	endpoint, connections := pairPaperBookServer(t, f)
	dir := filepath.Join(f.parent, "paper-books")
	env := append(f.env(false), "GO_TRADER_PAPER_TEST_BOOK_ENDPOINT="+endpoint)
	p := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), env, "--seal")
	if p.Out.Recording == nil || p.Out.Recording.Seal == nil || !p.Out.Recording.Seal.Sealed {
		t.Fatalf("book stream must seal: exit %d reasons %v seal %+v stderr %s", p.Code, p.Out.IncompleteReasons, p.Out.Recording, tail(p.Stderr, 5000))
	}
	rows := pairPaperLogRows(t, dir, f.runID)
	available, frames := 0, 0
	for _, row := range rows {
		if in := row.Event.BookInput; in != nil && in.Outcome == feedBookOutcomeAccepted {
			frames++
		}
		if end := row.Event.End; end != nil {
			var v pairPaperBookBoundary
			if json.Unmarshal(end.Result, &v) == nil && v.Disposition == "available" {
				available++
			}
		}
		if st := row.Event.Start; st != nil && st.Kind == pairRecKindMatchPoll && st.MemberID == pairTestHL {
			t.Fatal("sealed member must make no public match poll")
		}
	}
	if available == 0 || frames == 0 {
		t.Fatalf("book stream needs acknowledged reads and accepted frames: %d %d", available, frames)
	}
	book, err := os.ReadFile(filepath.Join(dir, "export", "books-"+pairTestHL+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc pairReplayBookDoc
	if err = json.Unmarshal(book, &doc); err != nil || doc.structural(pairTestHL) != "" {
		t.Fatalf("export must satisfy standalone book schema: %v %+v", err, doc)
	}
	before := connections.Load()
	play := pairPaperReproduce(t, f, dir, "book-playback")
	if play.Out.Comparison == nil || p.Out.Comparison == nil || play.Out.Comparison.Hash != p.Out.Comparison.Hash {
		t.Fatalf("blocked-network playback must reproduce comparison: paper %+v replay %+v stderr %s", p.Out.Comparison, play.Out.Comparison, tail(play.Stderr, 8000))
	}
	if connections.Load() != before {
		t.Fatal("playback must instantiate no receiver")
	}
	var standalone map[string]interface{}
	reproBlob, err := os.ReadFile(filepath.Join(dir, "export", pairRecordingSpecName))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(reproBlob, &standalone); err != nil {
		t.Fatal(err)
	}
	delete(standalone, "recording")
	standalone["run"].(map[string]interface{})["state_dir"] = filepath.Join(f.parent, "standalone-books")
	plain := runPairReplayAdapter(t, f.repo, standalone, f.env(true), "--max-steps", "1")
	if plain.Code == basketReplayExitRefused || plain.Code == basketReplayExitUsage || plain.Out.Refusal != nil {
		t.Fatalf("standalone version-2 replay must admit exported books: exit %d %+v", plain.Code, plain.Out.Refusal)
	}
	if p.Code != basketReplayExitOK {
		t.Fatalf("positive sealed run must complete: %s", p.Raw)
	}
	purposes := pairPaperPurposes(t, p.Out)
	if purposes[pairPurposeEntry] < 2 {
		t.Fatalf("sealed run must fill both entry legs: %v", purposes)
	}
	for _, row := range rows {
		if row.Event.Start != nil && row.Event.Start.Kind == pairRecKindBookMark {
			return
		}
	}
	t.Fatal("management must record its own book mark boundary")
}

func TestBasketReplayPairPaperBooksProcessProxyRefusalOpensNoSocket(t *testing.T) {
	f := newPairPaperFixture(t, pairPaperOptions2{runID: "paper-book-proxy", mutate: func(root map[string]interface{}) { pairTestMemberEntry(root, 0)["source"] = pairSourceSealedFeed }})
	endpoint, connections := pairPaperBookServer(t, f)
	if err := os.WriteFile(f.config, []byte(`{"egress":{"proxy_required":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(f.env(false), "GO_TRADER_PAPER_TEST_BOOK_ENDPOINT="+endpoint)
	p := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, filepath.Join(f.parent, "refused")), env)
	if p.Code != basketReplayExitRefused || connections.Load() != 0 {
		t.Fatalf("proxy refusal must open no direct book socket: exit %d connections %d stderr %s", p.Code, connections.Load(), tail(p.Stderr, 1000))
	}
}

func TestBasketReplayPairPaperBooksProcessResumesCommittedInput(t *testing.T) {
	for _, mode := range []string{"chunk", "collection fault", "receiver commit fault"} {
		t.Run(mode, func(t *testing.T) {
			f := newPairPaperFixture(t, pairPaperOptions2{runID: "paper-book-resume", collectS: 16, drainS: 8, convergeS: 10, interval: 4, mutate: func(root map[string]interface{}) {
				pairTestMemberEntry(root, 0)["source"] = pairSourceSealedFeed
				pairTestPair(root)["book_need"].(map[string]interface{})["window_ms"] = 1000
			}})
			endpoint, connections := pairPaperBookServer(t, f)
			env := append(f.env(false), "GO_TRADER_PAPER_TEST_BOOK_ENDPOINT="+endpoint)
			dir := filepath.Join(f.parent, "book-resume")
			args := []string{"--max-steps", "5"}
			if mode == "collection fault" {
				args = []string{"--fault", "exit-after-collection:1"}
			} else if mode == "receiver commit fault" {
				args = []string{"--fault", "exit-after-book-input-commit:1"}
			}
			first := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), env, args...)
			if mode != "chunk" && first.Code != basketReplayExitFault {
				t.Fatalf("expected recorded crash: %d %s", first.Code, tail(first.Stderr, 2000))
			}
			resumed := runPairPaperAdapter(t, f, f.spec(basketReplayModeResume, dir), env, "--seal")
			if resumed.Out.Recording == nil || resumed.Out.Recording.Seal == nil || !resumed.Out.Recording.Seal.Sealed {
				t.Fatalf("resume must seal: exit %d refusal %+v reasons %v stderr %s", resumed.Code, resumed.Out.Refusal, resumed.Out.IncompleteReasons, tail(resumed.Stderr, 3000))
			}
			epochs := map[int]bool{}
			rows := pairPaperLogRows(t, dir, f.runID)
			for _, row := range rows {
				if in := row.Event.BookInput; in != nil && in.Kind == "epoch" {
					epochs[in.Epoch] = true
				}
			}
			if len(epochs) != 2 {
				t.Fatalf("resume must record one new socket epoch: %v", epochs)
			}
			before := connections.Load()
			play := pairPaperReproduce(t, f, dir, "resume-playback")
			if play.Out.Comparison == nil || resumed.Out.Comparison == nil || play.Out.Comparison.Hash != resumed.Out.Comparison.Hash {
				t.Fatalf("resumed recording must reproduce with no network: %+v %+v exit %d output %s stderr %s", resumed.Out.Comparison, play.Out.Comparison, play.Code, tail(string(play.Raw), 4000), tail(play.Stderr, 3000))
			}
			if connections.Load() != before {
				t.Fatal("resume playback opened a book socket")
			}
		})
	}
}

func TestBasketReplayPairPaperBooksProcessFundingPolicyAdmission(t *testing.T) {
	f, _ := newPairPaperResearchFixture(t, pairPaperResearchOptions{runID: "paper-book-funding", mutate: func(root map[string]interface{}) {
		pairTestMemberEntry(root, 0)["source"] = pairSourceSealedFeed
		pairTestPair(root)["book_need"].(map[string]interface{})["window_ms"] = 1000
	}})
	endpoint, _ := pairPaperBookServer(t, f)
	dir := filepath.Join(f.parent, "book-funding")
	p := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), append(f.env(false), "GO_TRADER_PAPER_TEST_BOOK_ENDPOINT="+endpoint), "--max-steps", "1")
	if p.Code == basketReplayExitRefused || p.Code == basketReplayExitUsage || p.Out.Refusal != nil {
		t.Fatalf("family 4 must admit the optional funding policy: %d %+v %s", p.Code, p.Out.Refusal, tail(p.Stderr, 2000))
	}
	man, err := readPairPaperManifest(dir)
	if err != nil || man.Version != pairPaperManifestBooksVer || man.Scenario.FundingPolicy == nil {
		t.Fatalf("family-4 funding binding must persist: %v %+v", err, man)
	}
	rows := pairPaperLogRows(t, dir, f.runID)
	if len(rows) == 0 || rows[0].Schema != pairRecSchemaBooks {
		t.Fatal("family-4 funding run must use schema 3")
	}
}

func TestBasketReplayPairPaperBooksProcessFailedAuxiliaryReadReproduces(t *testing.T) {
	f := newPairPaperFixture(t, pairPaperOptions2{runID: "paper-book-empty-aux", collectS: 8, drainS: 4, interval: 4, fixtureMore: map[string]interface{}{"fail_okx": "/api/v5/market/candles"}, mutate: func(root map[string]interface{}) {
		pairTestMemberEntry(root, 0)["source"] = pairSourceSealedFeed
		pairTestPair(root)["book_need"].(map[string]interface{})["window_ms"] = 1000
	}})
	endpoint, connections := pairPaperBookServer(t, f)
	dir := filepath.Join(f.parent, "empty-aux")
	p := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), append(f.env(false), "GO_TRADER_PAPER_TEST_BOOK_ENDPOINT="+endpoint), "--seal")
	if p.Out.Recording == nil || p.Out.Recording.Seal == nil || !p.Out.Recording.Seal.Sealed || p.Out.MechanicallyComplete {
		t.Fatalf("failed auxiliary reads must seal a reproducible incomplete recording: exit %d %+v reasons %v", p.Code, p.Out.Recording, p.Out.IncompleteReasons)
	}
	before := connections.Load()
	play := pairPaperReproduce(t, f, dir, "failed-aux-playback")
	if play.Out.Comparison == nil || p.Out.Comparison == nil || play.Out.Comparison.Hash != p.Out.Comparison.Hash || play.Out.MechanicallyComplete || connections.Load() != before {
		t.Fatalf("failed read playback must preserve the comparison and incomplete result: exit %d %s", play.Code, tail(string(play.Raw), 3000))
	}
	blob, err := os.ReadFile(filepath.Join(dir, "export", pairRecordingSpecName))
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]interface{}
	if err = json.Unmarshal(blob, &spec); err != nil {
		t.Fatal(err)
	}
	delete(spec, "recording")
	spec["run"].(map[string]interface{})["state_dir"] = filepath.Join(f.parent, "empty-aux-standalone")
	plain := runPairReplayAdapter(t, f.repo, spec, f.env(true), "--max-steps", "1")
	if plain.Code != basketReplayExitRefused || plain.Out.Refusal == nil {
		t.Fatalf("standalone replay must retain empty-candle refusal: %d", plain.Code)
	}
}

func pairPaperOpenSealedChecks(t *testing.T, rows []pairRecRow) []pairRecRow {
	t.Helper()
	var out []pairRecRow
	for _, row := range rows {
		st := row.Event.Start
		if st == nil || st.Kind != pairRecKindNativeCheck || st.MemberID != pairTestHL {
			continue
		}
		var req struct {
			Args []string `json:"args"`
		}
		if err := json.Unmarshal(st.Request, &req); err != nil {
			t.Fatal(err)
		}
		for _, a := range req.Args {
			if strings.HasPrefix(a, "--position-qty=") {
				out = append(out, row)
				break
			}
		}
	}
	return out
}

func TestBasketReplayPairPaperBooksProcessManagesOpenSealedPositionAfterAppendFault(t *testing.T) {
	fixture := func(runID string) (*pairPaperFixture, []string) {
		f := newPairPaperFixture(t, pairPaperOptions2{runID: runID, collectS: 40, drainS: 8, convergeS: 30, interval: 4, mutate: func(root map[string]interface{}) {
			pairTestMemberEntry(root, 0)["source"] = pairSourceSealedFeed
			pairTestPair(root)["book_need"].(map[string]interface{})["window_ms"] = 1000
		}})
		f.scenario.PaperFundingPollMs = 1000
		endpoint, _ := pairPaperBookServerWith(t, f, pairBookTestTightFrame)
		return f, []string{"GO_TRADER_PAPER_TEST_BOOK_ENDPOINT=" + endpoint}
	}
	bf, benv := fixture("paper-books-open")
	base := runPairPaperAdapter(t, bf, bf.spec(basketReplayModeNew, filepath.Join(bf.parent, "baseline")), append(bf.env(false), benv...), "--seal")
	open := pairPaperOpenSealedChecks(t, pairPaperLogRows(t, filepath.Join(bf.parent, "baseline"), bf.runID))
	if base.Code == basketReplayExitRefused || base.Code == basketReplayExitUsage || len(open) < 3 {
		t.Fatalf("the baseline must manage an open sealed position on at least 3 cycles: exit %d checks %d refusal %+v stderr %s", base.Code, len(open), base.Out.Refusal, tail(base.Stderr, 3000))
	}
	f, fenv := fixture("paper-books-fault")
	from := f.fromMs + open[0].Event.Start.RawStartMs - bf.fromMs + 1
	to := from + 40
	dir := filepath.Join(f.parent, "fault")
	env := append(pairPaperAppendFaultEnv(f, from, to), fenv...)
	p := runPairPaperAdapter(t, f, f.spec(basketReplayModeNew, dir), env)
	rows := pairPaperLogRows(t, dir, f.runID)
	var clearSeq, lostSeq, restartEpoch int64
	for _, row := range rows {
		if row.Kind == pairRecEventClear && clearSeq == 0 {
			clearSeq = row.Seq
		}
		if in := row.Event.BookInput; in != nil {
			if in.LostTicketFrom > 0 && lostSeq == 0 {
				lostSeq = row.Seq
			}
			if in.Kind == "epoch" && lostSeq > 0 && row.Seq > lostSeq && restartEpoch == 0 {
				restartEpoch = int64(in.Epoch)
			}
		}
	}
	if clearSeq == 0 {
		t.Fatalf("the append fault inside [%d, %d) must set and clear the recording barrier: exit %d stderr %s", from, to, p.Code, tail(p.Stderr, 4000))
	}
	var after []pairRecRow
	for _, row := range pairPaperOpenSealedChecks(t, rows) {
		if row.Seq > clearSeq {
			after = append(after, row)
		}
	}
	if len(after) == 0 {
		t.Fatalf("native management of the open sealed position must run after the barrier clears at seq %d: stderr %s", clearSeq, tail(p.Stderr, 4000))
	}
	t.Logf("OPEN_SEALED_FAULT window=[%d,%d) clear_seq=%d lost_stop_seq=%d restart_epoch=%d first_open_check_after_clear_seq=%d open_checks_after_clear=%d exit=%d", from, to, clearSeq, lostSeq, restartEpoch, after[0].Seq, len(after), p.Code)
}
