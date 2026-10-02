package fleet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// --- retention response shapes ---------------------------------------------

type retentionResponse struct {
	MaxEvents        int64  `json:"maxEvents"`
	RetainedCount    int64  `json:"retainedCount"`
	EarliestSequence *int64 `json:"earliestSequence"`
	MaxSequence      int64  `json:"maxSequence"`
}

type goneResponse struct {
	Error            string `json:"error"`
	EarliestSequence int64  `json:"earliestSequence"`
}

func getRetention(t *testing.T, h http.Handler, id string) retentionResponse {
	t.Helper()
	result := doRequest(t, h, http.MethodGet, fmt.Sprintf("/v1/devices/%s/history/retention", id), "")
	if result.Code != http.StatusOK {
		t.Fatalf("get retention = %d, want 200: %s", result.Code, result.Body.String())
	}
	return decodeBody[retentionResponse](t, result)
}

func putRetention(t *testing.T, h http.Handler, id string, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, h, http.MethodPut, fmt.Sprintf("/v1/devices/%s/history/retention", id), body)
}

// --- GET on empty device ----------------------------------------------------

func TestRetentionGetEmptyDevice(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")

	info := getRetention(t, h, "gw")
	if info.MaxEvents != 0 || info.RetainedCount != 0 || info.MaxSequence != 0 {
		t.Fatalf("empty retention = %+v, want all zeros", info)
	}
	if info.EarliestSequence != nil {
		t.Fatalf("empty earliest = %v, want nil", *info.EarliestSequence)
	}
}

func TestRetentionUnknownDevice(t *testing.T) {
	h := NewHandler(NewStore())
	if r := doRequest(t, h, http.MethodGet, "/v1/devices/missing/history/retention", ""); r.Code != http.StatusNotFound {
		t.Fatalf("get unknown = %d, want 404", r.Code)
	}
	if r := putRetention(t, h, "missing", `{"maxEvents":5}`); r.Code != http.StatusNotFound {
		t.Fatalf("put unknown = %d, want 404", r.Code)
	}
}

// --- PUT validation ----------------------------------------------------------

func TestRetentionSetAndGet(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")

	r := putRetention(t, h, "gw", `{"maxEvents":42}`)
	if r.Code != http.StatusOK {
		t.Fatalf("put = %d, want 200: %s", r.Code, r.Body.String())
	}
	info := decodeBody[retentionResponse](t, r)
	if info.MaxEvents != 42 {
		t.Fatalf("maxEvents = %d, want 42", info.MaxEvents)
	}
	if info.RetainedCount != 0 || info.MaxSequence != 0 || info.EarliestSequence != nil {
		t.Fatalf("retention = %+v, want empty state", info)
	}

	// GET reflects the stored cap.
	got := getRetention(t, h, "gw")
	if got.MaxEvents != 42 {
		t.Fatalf("get maxEvents = %d, want 42", got.MaxEvents)
	}

	// Restore unlimited.
	r = putRetention(t, h, "gw", `{"maxEvents":0}`)
	if r.Code != http.StatusOK {
		t.Fatalf("put 0 = %d, want 200", r.Code)
	}
	if info := decodeBody[retentionResponse](t, r); info.MaxEvents != 0 {
		t.Fatalf("maxEvents = %d, want 0", info.MaxEvents)
	}
}

func TestRetentionValidationRejectsBadBodies(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")

	cases := []struct {
		name string
		body string
	}{
		{"missing field", `{}`},
		{"null", `{"maxEvents":null}`},
		{"non-integer 5.0", `{"maxEvents":5.0}`},
		{"non-integer 5.5", `{"maxEvents":5.5}`},
		{"non-integer 1e3", `{"maxEvents":1e3}`},
		{"string", `{"maxEvents":"5"}`},
		{"boolean", `{"maxEvents":true}`},
		{"zero-as-float", `{"maxEvents":0.0}`},
		{"out of range 10001", `{"maxEvents":10001}`},
		{"out of range -1", `{"maxEvents":-1}`},
		{"out of range 100000", `{"maxEvents":100000}`},
		{"multiple docs", `{"maxEvents":5}{"maxEvents":6}`},
		{"not an object", `[5]`},
		{"empty body", ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := putRetention(t, h, "gw", tc.body)
			if r.Code != http.StatusBadRequest {
				t.Fatalf("put %q = %d, want 400: %s", tc.body, r.Code, r.Body.String())
			}
		})
	}
}

func TestRetentionBoundaryValues(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")

	for _, v := range []int64{0, 1, 10000} {
		r := putRetention(t, h, "gw", fmt.Sprintf(`{"maxEvents":%d}`, v))
		if r.Code != http.StatusOK {
			t.Fatalf("put %d = %d, want 200: %s", v, r.Code, r.Body.String())
		}
	}
}

// --- enforcement on telemetry ------------------------------------------------

func TestRetentionEnforcedOnTelemetry(t *testing.T) {
	store, _ := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	putRetention(t, h, "gw", `{"maxEvents":3}`)
	for i := 1; i <= 5; i++ {
		if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry",
			fmt.Sprintf(`{"temperature":%d}`, i)); r.Code != http.StatusAccepted {
			t.Fatalf("telemetry %d = %d", i, r.Code)
		}
	}

	info := getRetention(t, h, "gw")
	if info.RetainedCount != 3 {
		t.Fatalf("retained = %d, want 3", info.RetainedCount)
	}
	if info.MaxSequence != 5 {
		t.Fatalf("maxSequence = %d, want 5", info.MaxSequence)
	}
	if info.EarliestSequence == nil || *info.EarliestSequence != 3 {
		t.Fatalf("earliest = %v, want 3", info.EarliestSequence)
	}

	events := allEvents(t, h, "gw")
	if len(events) != 3 {
		t.Fatalf("history = %d events, want 3", len(events))
	}
	wantSeq := []int64{3, 4, 5}
	for i, e := range events {
		if e.Sequence != wantSeq[i] {
			t.Fatalf("events[%d].Sequence = %d, want %d", i, e.Sequence, wantSeq[i])
		}
		if e.Values["temperature"] != float64(i+3) {
			t.Fatalf("events[%d] value = %v, want %v", i, e.Values["temperature"], float64(i+3))
		}
	}
}

func TestRetentionLoweringCleansOldestImmediately(t *testing.T) {
	store, _ := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	for i := 1; i <= 6; i++ {
		doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", fmt.Sprintf(`{"temperature":%d}`, i))
	}
	// Unlimited so far: all 6 retained.
	if info := getRetention(t, h, "gw"); info.RetainedCount != 6 || info.MaxSequence != 6 {
		t.Fatalf("before cleanup = %+v", info)
	}

	// Lower to 2: oldest 4 are cleaned immediately.
	putRetention(t, h, "gw", `{"maxEvents":2}`)
	info := getRetention(t, h, "gw")
	if info.RetainedCount != 2 || info.MaxSequence != 6 {
		t.Fatalf("after lowering = %+v", info)
	}
	if info.EarliestSequence == nil || *info.EarliestSequence != 5 {
		t.Fatalf("earliest = %v, want 5", info.EarliestSequence)
	}
	events := allEvents(t, h, "gw")
	if len(events) != 2 || events[0].Sequence != 5 || events[1].Sequence != 6 {
		t.Fatalf("history = %+v, want seq 5,6", events)
	}

	// Raising the cap does not restore cleaned events.
	putRetention(t, h, "gw", `{"maxEvents":100}`)
	info = getRetention(t, h, "gw")
	if info.RetainedCount != 2 || info.MaxSequence != 6 {
		t.Fatalf("after raising = %+v", info)
	}
}

func TestRetentionRestoreUnlimitedDoesNotRestore(t *testing.T) {
	store, _ := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	putRetention(t, h, "gw", `{"maxEvents":2}`)
	for i := 1; i <= 4; i++ {
		doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", fmt.Sprintf(`{"temperature":%d}`, i))
	}
	putRetention(t, h, "gw", `{"maxEvents":0}`)
	info := getRetention(t, h, "gw")
	if info.RetainedCount != 2 || info.MaxSequence != 4 {
		t.Fatalf("after unlimited = %+v", info)
	}
}

// --- enforcement on replay ---------------------------------------------------

func TestRetentionEnforcedOnReplay(t *testing.T) {
	store, _ := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	putRetention(t, h, "gw", `{"maxEvents":2}`)
	body := replayBody("batch-1",
		sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`),
		sample("e2", "2024-01-02T11:00:00Z", `{"v":2}`),
		sample("e3", "2024-01-02T12:00:00Z", `{"v":3}`),
	)
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body)
	if r.Code != http.StatusAccepted {
		t.Fatalf("replay = %d, want 202: %s", r.Code, r.Body.String())
	}
	receipt := decodeBody[receiptResponse](t, r)
	if receipt.NewCount != 3 || receipt.DuplicateCount != 0 {
		t.Fatalf("counts = %+d/%d, want 3/0", receipt.NewCount, receipt.DuplicateCount)
	}
	// Receipt lists all samples with their assigned sequences.
	wantSeq := []int64{1, 2, 3}
	for i, s := range receipt.Samples {
		if s.Sequence != wantSeq[i] || s.Duplicate {
			t.Fatalf("sample %d = %+v, want seq %d", i, s, wantSeq[i])
		}
	}

	// Only the last 2 are retained.
	info := getRetention(t, h, "gw")
	if info.RetainedCount != 2 || info.MaxSequence != 3 {
		t.Fatalf("retention = %+v", info)
	}
	if info.EarliestSequence == nil || *info.EarliestSequence != 2 {
		t.Fatalf("earliest = %v, want 2", info.EarliestSequence)
	}
	events := allEvents(t, h, "gw")
	if len(events) != 2 || events[0].Sequence != 2 || events[1].Sequence != 3 {
		t.Fatalf("history = %+v, want seq 2,3", events)
	}
}

func TestRetentionBatchOverLimitStillAssignsAllSequences(t *testing.T) {
	store, _ := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	putRetention(t, h, "gw", `{"maxEvents":3}`)
	// Batch of 5 new samples: all get sequences 1..5, but only last 3 retained.
	body := replayBody("big-batch",
		sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`),
		sample("e2", "2024-01-02T11:00:00Z", `{"v":2}`),
		sample("e3", "2024-01-02T12:00:00Z", `{"v":3}`),
		sample("e4", "2024-01-02T13:00:00Z", `{"v":4}`),
		sample("e5", "2024-01-02T14:00:00Z", `{"v":5}`),
	)
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body)
	if r.Code != http.StatusAccepted {
		t.Fatalf("replay = %d, want 202", r.Code)
	}
	receipt := decodeBody[receiptResponse](t, r)
	if receipt.NewCount != 5 {
		t.Fatalf("newCount = %d, want 5", receipt.NewCount)
	}
	for i, s := range receipt.Samples {
		if s.Sequence != int64(i+1) {
			t.Fatalf("sample %d seq = %d, want %d", i, s.Sequence, i+1)
		}
	}
	info := getRetention(t, h, "gw")
	if info.RetainedCount != 3 || info.MaxSequence != 5 {
		t.Fatalf("retention = %+v", info)
	}
}

// --- cleaned event dedup ------------------------------------------------------

func TestRetentionCleanedEventDuplicateReturnsOriginalSequence(t *testing.T) {
	store, _ := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	putRetention(t, h, "gw", `{"maxEvents":2}`)
	// First batch: e1, e2, e3. Only e2, e3 retained; e1 is cleaned.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-1",
			sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`),
			sample("e2", "2024-01-02T11:00:00Z", `{"v":2}`),
			sample("e3", "2024-01-02T12:00:00Z", `{"v":3}`),
		))

	// Re-submit e1 with identical content: it is a duplicate reusing seq 1,
	// even though seq 1 was cleaned from history.
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-2",
			sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`),
		))
	if r.Code != http.StatusAccepted {
		t.Fatalf("replay = %d, want 202: %s", r.Code, r.Body.String())
	}
	receipt := decodeBody[receiptResponse](t, r)
	if receipt.NewCount != 0 || receipt.DuplicateCount != 1 {
		t.Fatalf("counts = %+d/%d, want 0/1", receipt.NewCount, receipt.DuplicateCount)
	}
	if len(receipt.Samples) != 1 || receipt.Samples[0].Sequence != 1 || !receipt.Samples[0].Duplicate {
		t.Fatalf("samples = %+v, want duplicate seq 1", receipt.Samples)
	}
	// The duplicate did not re-enter history or squeeze existing records.
	info := getRetention(t, h, "gw")
	if info.RetainedCount != 2 || info.MaxSequence != 3 {
		t.Fatalf("retention = %+v", info)
	}
}

func TestRetentionCleanedEventDifferentContentConflicts(t *testing.T) {
	store, _ := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	putRetention(t, h, "gw", `{"maxEvents":2}`)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-1",
			sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`),
			sample("e2", "2024-01-02T11:00:00Z", `{"v":2}`),
			sample("e3", "2024-01-02T12:00:00Z", `{"v":3}`),
		))

	// e1 with different value: conflict, whole batch 409.
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-2",
			sample("e1", "2024-01-02T10:00:00Z", `{"v":99}`),
		))
	if r.Code != http.StatusConflict {
		t.Fatalf("replay = %d, want 409: %s", r.Code, r.Body.String())
	}
	// No state change.
	info := getRetention(t, h, "gw")
	if info.RetainedCount != 2 || info.MaxSequence != 3 {
		t.Fatalf("retention = %+v after conflict", info)
	}
}

func TestRetentionOriginalBatchRetryReturnsFirstReceipt(t *testing.T) {
	store, _ := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	putRetention(t, h, "gw", `{"maxEvents":2}`)
	body := replayBody("batch-1",
		sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`),
		sample("e2", "2024-01-02T11:00:00Z", `{"v":2}`),
		sample("e3", "2024-01-02T12:00:00Z", `{"v":3}`),
	)
	first := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first = %d", first.Code)
	}
	// Retry the same batch: 200 with the first receipt.
	retry := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body)
	if retry.Code != http.StatusOK {
		t.Fatalf("retry = %d, want 200: %s", retry.Code, retry.Body.String())
	}
	var firstReceipt, retryReceipt receiptResponse
	json.Unmarshal(first.Body.Bytes(), &firstReceipt)
	json.Unmarshal(retry.Body.Bytes(), &retryReceipt)
	if retryReceipt.NewCount != firstReceipt.NewCount || retryReceipt.DuplicateCount != firstReceipt.DuplicateCount {
		t.Fatalf("retry counts = %+d/%d, want %+d/%d", retryReceipt.NewCount, retryReceipt.DuplicateCount, firstReceipt.NewCount, firstReceipt.DuplicateCount)
	}
	for i := range firstReceipt.Samples {
		if retryReceipt.Samples[i].Sequence != firstReceipt.Samples[i].Sequence {
			t.Fatalf("retry sample %d seq = %d, want %d", i, retryReceipt.Samples[i].Sequence, firstReceipt.Samples[i].Sequence)
		}
	}
}

// --- cursor behavior with cleanup --------------------------------------------

func TestRetentionCursorGoneReturns410(t *testing.T) {
	store, _ := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	putRetention(t, h, "gw", `{"maxEvents":100}`)
	for i := 1; i <= 10; i++ {
		doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", fmt.Sprintf(`{"temperature":%d}`, i))
	}

	// First page: limit 3, resume at seq 4.
	pages := walkHistory(t, h, "/v1/devices/gw/history?limit=3")
	if len(pages) < 1 || pages[0].NextCursor == nil {
		t.Fatalf("expected a next cursor, got %d pages", len(pages))
	}
	cursor := *pages[0].NextCursor

	// Lower retention to 2: cleanup keeps only seq 9,10; the resume point
	// (seq 4) is gone.
	putRetention(t, h, "gw", `{"maxEvents":2}`)

	r := doRequest(t, h, http.MethodGet, "/v1/devices/gw/history?limit=3&cursor="+cursor, "")
	if r.Code != http.StatusGone {
		t.Fatalf("cursor = %d, want 410: %s", r.Code, r.Body.String())
	}
	gone := decodeBody[goneResponse](t, r)
	if gone.EarliestSequence != 9 {
		t.Fatalf("earliest = %d, want 9", gone.EarliestSequence)
	}
}

func TestRetentionCursorValidWhenResumePointKept(t *testing.T) {
	store, _ := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	putRetention(t, h, "gw", `{"maxEvents":100}`)
	for i := 1; i <= 10; i++ {
		doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", fmt.Sprintf(`{"temperature":%d}`, i))
	}

	// First page: limit 3, resume at seq 4, highWater pinned to 10.
	pages := walkHistory(t, h, "/v1/devices/gw/history?limit=3")
	if len(pages) < 1 || pages[0].NextCursor == nil {
		t.Fatalf("expected a next cursor")
	}
	cursor := *pages[0].NextCursor

	// Lower retention to 7: cleanup keeps seq 4..10; the resume point (seq 4)
	// survives, so the cursor remains valid.
	putRetention(t, h, "gw", `{"maxEvents":7}`)

	r := doRequest(t, h, http.MethodGet, "/v1/devices/gw/history?limit=3&cursor="+cursor, "")
	if r.Code != http.StatusOK {
		t.Fatalf("cursor = %d, want 200: %s", r.Code, r.Body.String())
	}
	page := decodeBody[historyResponse](t, r)
	if len(page.Events) != 3 {
		t.Fatalf("page = %d events, want 3", len(page.Events))
	}
	wantSeq := []int64{4, 5, 6}
	for i, e := range page.Events {
		if e.Sequence != wantSeq[i] {
			t.Fatalf("events[%d].Sequence = %d, want %d", i, e.Sequence, wantSeq[i])
		}
	}
}

func TestRetentionCursorDoesNotLeakNewWrites(t *testing.T) {
	store, _ := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	for i := 1; i <= 6; i++ {
		doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", fmt.Sprintf(`{"temperature":%d}`, i))
	}
	// First page: limit 2, resume at seq 3, highWater pinned to 6.
	pages := walkHistory(t, h, "/v1/devices/gw/history?limit=2")
	if len(pages) < 1 || pages[0].NextCursor == nil {
		t.Fatalf("expected a next cursor")
	}
	cursor := *pages[0].NextCursor

	// New writes arrive after the first page.
	for i := 7; i <= 9; i++ {
		doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", fmt.Sprintf(`{"temperature":%d}`, i))
	}

	// Follow the cursor: it must not return seq 7..9 (beyond highWater 6).
	r := doRequest(t, h, http.MethodGet, "/v1/devices/gw/history?limit=100&cursor="+cursor, "")
	if r.Code != http.StatusOK {
		t.Fatalf("cursor = %d, want 200", r.Code)
	}
	page := decodeBody[historyResponse](t, r)
	for _, e := range page.Events {
		if e.Sequence > 6 {
			t.Fatalf("cursor leaked seq %d (beyond highWater 6)", e.Sequence)
		}
	}
}

func TestRetentionCursorTamperedReturns400(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":1}`)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":2}`)

	// Issue a valid cursor, then tamper with it.
	pages := walkHistory(t, h, "/v1/devices/gw/history?limit=1")
	if len(pages) < 1 || pages[0].NextCursor == nil {
		t.Fatalf("expected a cursor")
	}
	cursor := *pages[0].NextCursor
	tampered := cursor[:len(cursor)-2] + "AA"

	r := doRequest(t, h, http.MethodGet, "/v1/devices/gw/history?limit=1&cursor="+tampered, "")
	if r.Code != http.StatusBadRequest {
		t.Fatalf("tampered cursor = %d, want 400", r.Code)
	}
}

// --- persistence --------------------------------------------------------------

func TestRetentionPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	putRetention(t, h, "gw", `{"maxEvents":3}`)
	for i := 1; i <= 5; i++ {
		doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", fmt.Sprintf(`{"temperature":%d}`, i))
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	info := getRetention(t, h2, "gw")
	if info.MaxEvents != 3 {
		t.Fatalf("maxEvents = %d, want 3", info.MaxEvents)
	}
	if info.RetainedCount != 3 || info.MaxSequence != 5 {
		t.Fatalf("retention = %+v", info)
	}
	if info.EarliestSequence == nil || *info.EarliestSequence != 3 {
		t.Fatalf("earliest = %v, want 3", info.EarliestSequence)
	}

	// History survives with sequences intact.
	events := allEvents(t, h2, "gw")
	if len(events) != 3 {
		t.Fatalf("history = %d, want 3", len(events))
	}
	wantSeq := []int64{3, 4, 5}
	for i, e := range events {
		if e.Sequence != wantSeq[i] {
			t.Fatalf("events[%d].Sequence = %d, want %d", i, e.Sequence, wantSeq[i])
		}
	}

	// New samples continue the sequence and enforce the cap.
	doRequest(t, h2, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":6}`)
	info = getRetention(t, h2, "gw")
	if info.RetainedCount != 3 || info.MaxSequence != 6 {
		t.Fatalf("after new write = %+v", info)
	}
	events = allEvents(t, h2, "gw")
	if len(events) != 3 || events[0].Sequence != 4 || events[2].Sequence != 6 {
		t.Fatalf("history = %+v, want seq 4,5,6", events)
	}
}

func TestRetentionCursorSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	putRetention(t, h, "gw", `{"maxEvents":100}`)
	for i := 1; i <= 6; i++ {
		doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", fmt.Sprintf(`{"temperature":%d}`, i))
	}
	pages := walkHistory(t, h, "/v1/devices/gw/history?limit=2")
	if len(pages) < 1 || pages[0].NextCursor == nil {
		t.Fatalf("expected a cursor")
	}
	cursor := *pages[0].NextCursor
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	// The cursor pins highWater=6; after restart it still reads seq 3..6.
	r := doRequest(t, h2, http.MethodGet, "/v1/devices/gw/history?limit=100&cursor="+cursor, "")
	if r.Code != http.StatusOK {
		t.Fatalf("cursor after restart = %d, want 200: %s", r.Code, r.Body.String())
	}
	page := decodeBody[historyResponse](t, r)
	wantSeq := []int64{3, 4, 5, 6}
	if len(page.Events) != 4 {
		t.Fatalf("page = %d events, want 4", len(page.Events))
	}
	for i, e := range page.Events {
		if e.Sequence != wantSeq[i] {
			t.Fatalf("events[%d].Sequence = %d, want %d", i, e.Sequence, wantSeq[i])
		}
	}
}

func TestRetentionOldDataDefaultsToUnlimited(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	for i := 1; i <= 4; i++ {
		doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", fmt.Sprintf(`{"temperature":%d}`, i))
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	info := getRetention(t, h2, "gw")
	if info.MaxEvents != 0 {
		t.Fatalf("maxEvents = %d, want 0 (unlimited)", info.MaxEvents)
	}
	if info.RetainedCount != 4 || info.MaxSequence != 4 {
		t.Fatalf("retention = %+v", info)
	}
}

// --- write failure returns 503 -----------------------------------------------

func TestRetentionSetFailureReturns503AndChangesNothing(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	for i := 1; i <= 5; i++ {
		doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", fmt.Sprintf(`{"temperature":%d}`, i))
	}

	real := store.wal.(*walFile)
	failing := &failingWAL{inner: real}
	store.wal = failing

	r := putRetention(t, h, "gw", `{"maxEvents":2}`)
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("put = %d, want 503: %s", r.Code, r.Body.String())
	}

	// Setting and cleanup both failed: history and cap are untouched.
	info := getRetention(t, h, "gw")
	if info.MaxEvents != 0 {
		t.Fatalf("maxEvents = %d, want 0 (unchanged)", info.MaxEvents)
	}
	if info.RetainedCount != 5 || info.MaxSequence != 5 {
		t.Fatalf("retention = %+v, want 5/5 (unchanged)", info)
	}

	// Once healthy, the same request commits.
	store.wal = real
	r = putRetention(t, h, "gw", `{"maxEvents":2}`)
	if r.Code != http.StatusOK {
		t.Fatalf("retry put = %d, want 200: %s", r.Code, r.Body.String())
	}
	info = getRetention(t, h, "gw")
	if info.RetainedCount != 2 || info.MaxSequence != 5 {
		t.Fatalf("after retry = %+v", info)
	}
}

// --- device independence and state preservation -------------------------------

func TestRetentionDevicesAreIndependent(t *testing.T) {
	store, _ := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw-a")
	mustRegister(t, h, "gw-b")

	putRetention(t, h, "gw-a", `{"maxEvents":2}`)
	for i := 1; i <= 4; i++ {
		doRequest(t, h, http.MethodPost, "/v1/devices/gw-a/telemetry", fmt.Sprintf(`{"temperature":%d}`, i))
		doRequest(t, h, http.MethodPost, "/v1/devices/gw-b/telemetry", fmt.Sprintf(`{"temperature":%d}`, i))
	}

	infoA := getRetention(t, h, "gw-a")
	infoB := getRetention(t, h, "gw-b")
	if infoA.RetainedCount != 2 {
		t.Fatalf("gw-a retained = %d, want 2", infoA.RetainedCount)
	}
	if infoB.RetainedCount != 4 {
		t.Fatalf("gw-b retained = %d, want 4 (unaffected by gw-a cap)", infoB.RetainedCount)
	}
}

func TestRetentionDoesNotChangeDeviceState(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	first := clock.Add(5 * time.Second)
	*clock = first
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":20}`)
	*clock = clock.Add(5 * time.Second)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":22}`)

	before := decodeBody[struct {
		LastSeenAt    time.Time          `json:"lastSeenAt"`
		LastTelemetry map[string]float64 `json:"lastTelemetry"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/fleet", ""))

	putRetention(t, h, "gw", `{"maxEvents":1}`)

	after := decodeBody[struct {
		LastSeenAt    time.Time          `json:"lastSeenAt"`
		LastTelemetry map[string]float64 `json:"lastTelemetry"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/fleet", ""))

	if !after.LastSeenAt.Equal(before.LastSeenAt) {
		t.Fatalf("lastSeenAt changed: %s -> %s", before.LastSeenAt, after.LastSeenAt)
	}
	if after.LastTelemetry["temperature"] != before.LastTelemetry["temperature"] {
		t.Fatalf("lastTelemetry changed: %v -> %v", before.LastTelemetry, after.LastTelemetry)
	}
}

// --- concurrency ---------------------------------------------------------------

func TestRetentionConcurrentChangesAndWrites(t *testing.T) {
	store, _ := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	// Hammer the device with concurrent retention changes and telemetry writes.
	// Every successful operation is a complete commit; the history must never
	// exceed the final effective cap and sequences must stay contiguous.
	const goroutines = 8
	const iterations = 50
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if i%3 == 0 {
					putRetention(t, h, "gw", fmt.Sprintf(`{"maxEvents":%d}`, 1+((g+i)%10)))
				} else {
					doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry",
						fmt.Sprintf(`{"temperature":%d}`, g*100+i))
				}
			}
		}(g)
	}
	wg.Wait()

	info := getRetention(t, h, "gw")
	events := allEvents(t, h, "gw")

	// Retained count never exceeds the final cap.
	if int64(len(events)) > info.MaxEvents {
		t.Fatalf("retained %d exceeds cap %d", len(events), info.MaxEvents)
	}
	// Sequences are contiguous and match the retained window.
	for i, e := range events {
		want := info.MaxSequence - int64(len(events)) + 1 + int64(i)
		if e.Sequence != want {
			t.Fatalf("events[%d].Sequence = %d, want %d (window %d..%d)", i, e.Sequence, want, info.MaxSequence-int64(len(events))+1, info.MaxSequence)
		}
	}
	// Max sequence is consistent with the number of successful writes.
	if info.MaxSequence < int64(len(events)) {
		t.Fatalf("maxSequence %d < retained %d", info.MaxSequence, len(events))
	}
}
