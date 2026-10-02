package fleet

import (
	"net/http"
	"sync"
	"testing"
)

// --- retention survives restart ---------------------------------------------

func TestPersistentRetentionSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	sendTelemetry(t, h, "gw", 6)

	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/history/retention", `{"maxEvents":2}`); r.Code != http.StatusOK {
		t.Fatalf("PUT status = %d: %s", r.Code, r.Body.String())
	}
	events := allEvents(t, h, "gw")
	if got := sequencesOf(events); len(got) != 2 || got[0] != 5 || got[1] != 6 {
		t.Fatalf("before restart history = %v, want [5 6]", got)
	}

	// A cursor issued before restart, whose start survives, stays valid and
	// keeps its pinned high-water mark.
	pageR := doRequest(t, h, http.MethodGet, "/v1/devices/gw/history?limit=1", "")
	page := decodeBody[historyResponse](t, pageR)
	cursor := *page.NextCursor // next start is sequence 6, still retained

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	status := getRetention(t, h2, "gw")
	if status.MaxEvents != 2 || status.RetainedEvents != 2 ||
		status.EarliestSequence == nil || *status.EarliestSequence != 5 || status.MaxSequence != 6 {
		t.Fatalf("retention after restart = %+v", status)
	}
	events = allEvents(t, h2, "gw")
	if got := sequencesOf(events); len(got) != 2 || got[0] != 5 || got[1] != 6 {
		t.Fatalf("history after restart = %v, want [5 6]", got)
	}

	// New samples continue the cumulative sequence.
	sendTelemetry(t, h2, "gw", 1)
	events = allEvents(t, h2, "gw")
	if got := sequencesOf(events); len(got) != 2 || got[0] != 6 || got[1] != 7 {
		t.Fatalf("history after new sample = %v, want [6 7]", got)
	}

	// The pre-restart continuation still works within its pinned bound.
	r := doRequest(t, h2, http.MethodGet, "/v1/devices/gw/history?limit=1&cursor="+cursor, "")
	if r.Code != http.StatusOK {
		t.Fatalf("pre-restart cursor status = %d, want 200: %s", r.Code, r.Body.String())
	}
	page2 := decodeBody[historyResponse](t, r)
	if got := sequencesOf(page2.Events); len(got) != 1 || got[0] != 6 {
		t.Fatalf("pre-restart continuation = %v, want [6]", got)
	}
}

func TestPersistentTrimmedDedupeSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/history/retention", `{"maxEvents":1}`); r.Code != http.StatusOK {
		t.Fatalf("PUT status = %d", r.Code)
	}
	first := replayBody("b1",
		sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`),
		sample("e2", "2024-01-02T10:01:00Z", `{"v":2}`),
		sample("e3", "2024-01-02T10:02:00Z", `{"v":3}`),
	)
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", first); r.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d: %s", r.Code, r.Body.String())
	}
	// Only sequence 3 remains; e1/e2 are trimmed but their dedup records persist.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	h2 := NewHandler(reopenPersistent(t, dir))

	// Same content after restart: still the original sequence 1 duplicate.
	r := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b2", sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`)))
	if r.Code != http.StatusAccepted {
		t.Fatalf("trimmed duplicate after restart status = %d: %s", r.Code, r.Body.String())
	}
	receipt := decodeBody[receiptResponse](t, r)
	if receipt.NewCount != 0 || receipt.DuplicateCount != 1 || receipt.Samples[0].Sequence != 1 {
		t.Fatalf("receipt = %+v, want duplicate of sequence 1", receipt)
	}

	// Different content still conflicts.
	r = doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b3", sample("e2", "2024-01-02T10:01:00Z", `{"v":42}`)))
	if r.Code != http.StatusConflict {
		t.Fatalf("conflict after restart status = %d, want 409: %s", r.Code, r.Body.String())
	}

	// Original batch retry returns the first receipt with 200.
	r = doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay", first)
	if r.Code != http.StatusOK {
		t.Fatalf("batch retry after restart status = %d, want 200: %s", r.Code, r.Body.String())
	}
	retry := decodeBody[receiptResponse](t, r)
	if retry.NewCount != 3 || len(retry.Samples) != 3 {
		t.Fatalf("retry receipt = %+v, want original 3-sample receipt", retry)
	}
}

func TestPersistentPreRestartCursorTrimmedAcrossRestartGives410(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	sendTelemetry(t, h, "gw", 6)

	pageR := doRequest(t, h, http.MethodGet, "/v1/devices/gw/history?limit=2", "")
	page := decodeBody[historyResponse](t, pageR)
	cursor := *page.NextCursor // next start is sequence 3

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	// Trim across restart so sequence 3 is gone.
	if r := doRequest(t, h2, http.MethodPut, "/v1/devices/gw/history/retention", `{"maxEvents":2}`); r.Code != http.StatusOK {
		t.Fatalf("PUT status = %d", r.Code)
	}
	r := doRequest(t, h2, http.MethodGet, "/v1/devices/gw/history?limit=2&cursor="+cursor, "")
	if r.Code != http.StatusGone {
		t.Fatalf("trimmed pre-restart cursor status = %d, want 410: %s", r.Code, r.Body.String())
	}
}

// --- 503 atomicity -----------------------------------------------------------

func TestPersistentSetRetentionFailureReturns503(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	sendTelemetry(t, h, "gw", 5)

	real := store.wal.(*walFile)
	failing := &failingWAL{inner: real}
	store.wal = failing

	r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/history/retention", `{"maxEvents":2}`)
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing PUT status = %d, want 503: %s", r.Code, r.Body.String())
	}

	// Nothing changed: limit still unlimited and all events are queryable.
	status := getRetention(t, h, "gw")
	if status.MaxEvents != 0 || status.RetainedEvents != 5 || status.MaxSequence != 5 {
		t.Fatalf("state changed after failed PUT: %+v", status)
	}
	if events := allEvents(t, h, "gw"); len(events) != 5 {
		t.Fatalf("failed PUT trimmed events: %d remain", len(events))
	}

	// The retry overwrites the residue and commits the new setting.
	r = doRequest(t, h, http.MethodPut, "/v1/devices/gw/history/retention", `{"maxEvents":2}`)
	if r.Code != http.StatusOK {
		t.Fatalf("retried PUT status = %d: %s", r.Code, r.Body.String())
	}
	store.wal = real
	if events := allEvents(t, h, "gw"); len(events) != 2 {
		t.Fatalf("retried PUT retained %d events, want 2", len(events))
	}
}

func TestPersistentWriteUnderFailingRetentionAtomic(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/history/retention", `{"maxEvents":2}`); r.Code != http.StatusOK {
		t.Fatalf("PUT status = %d", r.Code)
	}
	sendTelemetry(t, h, "gw", 3) // seqs 1..3, only 2,3 retained

	real := store.wal.(*walFile)
	failing := &failingWAL{inner: real}
	store.wal = failing

	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("bX", sample("x1", "2024-02-01T00:00:00Z", `{"v":9}`)))
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing replay status = %d, want 503: %s", r.Code, r.Body.String())
	}

	// No sequence consumed, no trim, no receipt.
	status := getRetention(t, h, "gw")
	if status.MaxSequence != 3 || status.RetainedEvents != 2 || *status.EarliestSequence != 2 {
		t.Fatalf("failed replay moved counters: %+v", status)
	}
	store.wal = real

	// Retry commits fresh at sequence 4, then enforces the cap.
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("bX", sample("x1", "2024-02-01T00:00:00Z", `{"v":9}`)))
	if r.Code != http.StatusAccepted {
		t.Fatalf("retry status = %d, want 202: %s", r.Code, r.Body.String())
	}
	events := allEvents(t, h, "gw")
	if got := sequencesOf(events); len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Fatalf("history after retry = %v, want [3 4]", got)
	}
}

// --- concurrency: no history may exceed the limit in force per operation ----

func TestConcurrentWritesAndRetentionNeverExceedCap(t *testing.T) {
	store := NewStore()
	if _, _, err := store.Register("gw", ""); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if _, err := store.RecordTelemetry("gw", map[string]float64{"v": 1}); err != nil {
					t.Errorf("telemetry: %v", err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		limits := []int64{0, 1, 3, 10, 2, 100}
		for i := 0; i < 40; i++ {
			if _, err := store.SetRetention("gw", limits[i%len(limits)]); err != nil {
				t.Errorf("set retention: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	// Final state: 200 samples were received; whatever the final limit is, the
	// retained window must be a contiguous suffix ending at sequence 200.
	status, err := store.GetRetention("gw")
	if err != nil {
		t.Fatal(err)
	}
	if status.MaxSequence != 200 {
		t.Fatalf("maxSequence = %d, want 200", status.MaxSequence)
	}
	events, _, _, _, err := store.History("gw", HistoryFilter{}, 0, 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if status.MaxEvents != 0 && int64(len(events)) > status.MaxEvents {
		t.Fatalf("retained %d > final cap %d", len(events), status.MaxEvents)
	}
	if len(events) > 0 {
		if events[0].Sequence != status.MaxSequence-int64(len(events))+1 {
			t.Fatalf("retained window has a gap: first = %d", events[0].Sequence)
		}
		if events[len(events)-1].Sequence != status.MaxSequence {
			t.Fatalf("retained window ends at %d, want %d", events[len(events)-1].Sequence, status.MaxSequence)
		}
	}
}
