package fleet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRegisterTelemetryAndSnapshot(t *testing.T) {
	h := NewHandler(NewStore())

	register := httptest.NewRequest(http.MethodPost, "/v1/devices", strings.NewReader(`{"id":"gateway-01","site":"lab"}`))
	register.Header.Set("Content-Type", "application/json")
	registerResult := httptest.NewRecorder()
	h.ServeHTTP(registerResult, register)
	if registerResult.Code != http.StatusCreated {
		t.Fatalf("register status = %d, want %d: %s", registerResult.Code, http.StatusCreated, registerResult.Body.String())
	}

	telemetry := httptest.NewRequest(http.MethodPost, "/v1/devices/gateway-01/telemetry", strings.NewReader(`{"temperature":21.75}`))
	telemetry.Header.Set("Content-Type", "application/json")
	telemetryResult := httptest.NewRecorder()
	h.ServeHTTP(telemetryResult, telemetry)
	if telemetryResult.Code != http.StatusAccepted {
		t.Fatalf("telemetry status = %d, want %d: %s", telemetryResult.Code, http.StatusAccepted, telemetryResult.Body.String())
	}

	snapshot := httptest.NewRequest(http.MethodGet, "/v1/fleet", nil)
	snapshotResult := httptest.NewRecorder()
	h.ServeHTTP(snapshotResult, snapshot)
	if snapshotResult.Code != http.StatusOK || !strings.Contains(snapshotResult.Body.String(), `"temperature":21.75`) {
		t.Fatalf("unexpected snapshot: status=%d body=%s", snapshotResult.Code, snapshotResult.Body.String())
	}
}

func TestTelemetryRejectsUnknownDevice(t *testing.T) {
	h := NewHandler(NewStore())
	request := httptest.NewRequest(http.MethodPost, "/v1/devices/missing/telemetry", strings.NewReader(`{"temperature":20}`))
	result := httptest.NewRecorder()
	h.ServeHTTP(result, request)
	if result.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", result.Code, http.StatusNotFound)
	}
}

// --- test helpers -----------------------------------------------------------

type historyResponse struct {
	DeviceID   string  `json:"deviceId"`
	Events     []Event `json:"events"`
	NextCursor *string `json:"nextCursor"`
}

type receiptResponse struct {
	BatchID        string `json:"batchId"`
	NewCount       int    `json:"newCount"`
	DuplicateCount int    `json:"duplicateCount"`
	Samples        []struct {
		EventID   string `json:"eventId"`
		Sequence  int64  `json:"sequence"`
		Duplicate bool   `json:"duplicate"`
	} `json:"samples"`
}

func newClockStore() (*Store, *time.Time) {
	store := NewStore()
	clock := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return clock }
	return store, &clock
}

func doRequest(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	result := httptest.NewRecorder()
	h.ServeHTTP(result, request)
	return result
}

func mustRegister(t *testing.T, h http.Handler, id string) {
	t.Helper()
	result := doRequest(t, h, http.MethodPost, "/v1/devices", fmt.Sprintf(`{"id":%q}`, id))
	if result.Code != http.StatusCreated {
		t.Fatalf("register %s: status = %d body = %s", id, result.Code, result.Body.String())
	}
}

func decodeBody[T any](t *testing.T, result *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(result.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode %s: %v", result.Body.String(), err)
	}
	return value
}

func replayBody(batchID string, samples ...string) string {
	return fmt.Sprintf(`{"batchId":%q,"samples":[%s]}`, batchID, strings.Join(samples, ","))
}

func sample(eventID, observedAt string, values string) string {
	return fmt.Sprintf(`{"eventId":%q,"observedAt":%q,"values":%s}`, eventID, observedAt, values)
}

// walkHistory pages through a device's full history, returning all events seen.
func walkHistory(t *testing.T, h http.Handler, target string) []historyResponse {
	t.Helper()
	var pages []historyResponse
	cursor := ""
	for {
		url := target
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		result := doRequest(t, h, http.MethodGet, url, "")
		if result.Code != http.StatusOK {
			t.Fatalf("history status = %d body = %s", result.Code, result.Body.String())
		}
		page := decodeBody[historyResponse](t, result)
		pages = append(pages, page)
		if page.NextCursor == nil {
			return pages
		}
		cursor = *page.NextCursor
	}
}

func allEvents(t *testing.T, h http.Handler, id string) []Event {
	t.Helper()
	pages := walkHistory(t, h, fmt.Sprintf("/v1/devices/%s/history?limit=100", id))
	var events []Event
	for _, page := range pages {
		events = append(events, page.Events...)
	}
	return events
}

// --- legacy telemetry enters history ----------------------------------------

func TestLegacyTelemetryEntersHistory(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	first := clock.Add(5 * time.Second)
	*clock = first
	if result := doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":20}`); result.Code != http.StatusAccepted {
		t.Fatalf("telemetry 1 status = %d", result.Code)
	}
	*clock = clock.Add(5 * time.Second)
	if result := doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":22}`); result.Code != http.StatusAccepted {
		t.Fatalf("telemetry 2 status = %d", result.Code)
	}

	events := allEvents(t, h, "gw")
	if len(events) != 2 {
		t.Fatalf("history length = %d, want 2", len(events))
	}
	if events[0].Sequence != 1 || events[1].Sequence != 2 {
		t.Fatalf("sequences = %d,%d, want 1,2", events[0].Sequence, events[1].Sequence)
	}
	if !events[0].ObservedAt.Equal(first) {
		t.Fatalf("observedAt = %s, want receive time %s", events[0].ObservedAt, first)
	}
	if events[0].Values["temperature"] != 20 || events[1].Values["temperature"] != 22 {
		t.Fatalf("unexpected values: %+v %+v", events[0].Values, events[1].Values)
	}
}

func TestTelemetryRejectsNonFiniteValues(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	// 1e999 overflows float64; either decode or finiteness validation rejects
	// it, but the contract is a 400 either way.
	result := doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":1e999}`)
	if result.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", result.Code)
	}
}

// --- replay: acceptance, dedup, receipts ------------------------------------

func TestReplayNewBatchReturns202AndReceipt(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	body := replayBody("batch-1",
		sample("e1", "2024-01-02T10:00:00Z", `{"a":1}`),
		sample("e2", "2024-01-02T11:00:00Z", `{"b":2}`),
	)
	result := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body)
	if result.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", result.Code, result.Body.String())
	}
	receipt := decodeBody[receiptResponse](t, result)
	if receipt.NewCount != 2 || receipt.DuplicateCount != 0 {
		t.Fatalf("counts = %+d/%d, want 2/0", receipt.NewCount, receipt.DuplicateCount)
	}
	if len(receipt.Samples) != 2 || receipt.Samples[0].Sequence != 1 || receipt.Samples[1].Sequence != 2 {
		t.Fatalf("receipt samples = %+v", receipt.Samples)
	}
	if receipt.Samples[0].Duplicate || receipt.Samples[1].Duplicate {
		t.Fatalf("new batch must not mark duplicates: %+v", receipt.Samples)
	}

	// Events land in array order; the last new sample drives the snapshot.
	events := allEvents(t, h, "gw")
	if len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 1+1 {
		t.Fatalf("history = %+v", events)
	}
	snapshot := decodeBody[struct {
		Devices []Device `json:"devices"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/fleet", ""))
	if len(snapshot.Devices) != 1 || snapshot.Devices[0].LastTelemetry["b"] != 2 {
		t.Fatalf("snapshot lastTelemetry = %+v, want b:2 from last new sample", snapshot.Devices)
	}
	if !snapshot.Devices[0].LastSeenAt.Equal(*clock) {
		t.Fatalf("lastSeenAt = %s, want receive time %s", snapshot.Devices[0].LastSeenAt, *clock)
	}
}

func TestReplayCrossBatchDuplicatesReuseSequence(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")

	first := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-a", sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`)))
	if first.Code != http.StatusAccepted {
		t.Fatalf("batch-a status = %d", first.Code)
	}

	// Same eventId with equivalent timestamp expressed with a different offset
	// and extra map key ordering is irrelevant; a new event rides along.
	second := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-b",
			sample("e1", "2024-01-02T18:00:00+08:00", `{"v":1}`),
			sample("e2", "2024-01-02T12:00:00Z", `{"v":2}`),
		))
	if second.Code != http.StatusAccepted {
		t.Fatalf("batch-b status = %d: %s", second.Code, second.Body.String())
	}
	receipt := decodeBody[receiptResponse](t, second)
	if receipt.NewCount != 1 || receipt.DuplicateCount != 1 {
		t.Fatalf("counts = %d/%d, want 1/1", receipt.NewCount, receipt.DuplicateCount)
	}
	if !receipt.Samples[0].Duplicate || receipt.Samples[0].Sequence != 1 {
		t.Fatalf("e1 should reuse sequence 1: %+v", receipt.Samples)
	}
	if receipt.Samples[1].Duplicate || receipt.Samples[1].Sequence != 2 {
		t.Fatalf("e2 should be new sequence 2: %+v", receipt.Samples)
	}

	events := allEvents(t, h, "gw")
	if len(events) != 2 {
		t.Fatalf("history length = %d, want 2 (duplicate must not append)", len(events))
	}
}

func TestReplayIdempotentBatchReturns200WithFirstReceipt(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	body := replayBody("batch-1", sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`))
	first := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d", first.Code)
	}
	firstReceipt := first.Body.String()

	*clock = clock.Add(time.Hour)
	second := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body)
	if second.Code != http.StatusOK {
		t.Fatalf("repeat status = %d, want 200", second.Code)
	}
	if second.Body.String() != firstReceipt {
		t.Fatalf("repeat receipt = %s, want original %s", second.Body.String(), firstReceipt)
	}

	// A pure replay must not refresh device state.
	snapshot := decodeBody[struct {
		Devices []Device `json:"devices"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/fleet", ""))
	if snapshot.Devices[0].LastSeenAt.Equal(*clock) {
		t.Fatalf("lastSeenAt refreshed to %s by pure duplicate batch", *clock)
	}
}

func TestReplayConflictRollsBackWholeBatch(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")

	doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-a", sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`)))

	// e2 is new and e1 conflicts; the whole batch must fail.
	conflict := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-b",
			sample("e2", "2024-01-02T10:00:00Z", `{"v":2}`),
			sample("e1", "2024-01-02T10:00:00Z", `{"v":999}`),
		))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", conflict.Code, conflict.Body.String())
	}

	events := allEvents(t, h, "gw")
	if len(events) != 1 || events[0].Sequence != 1 {
		t.Fatalf("conflict left state behind: %+v", events)
	}

	// batch-b must not have a stored receipt: fresh content under the same
	// batchId is accepted with 202 rather than rejected as "changed content".
	retry := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-b", sample("e3", "2024-01-03T10:00:00Z", `{"v":3}`)))
	if retry.Code != http.StatusAccepted {
		t.Fatalf("retry of previously failed batchId status = %d, want 202: %s", retry.Code, retry.Body.String())
	}
}

func TestReplaySameBatchIdChangedContentIs409(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")

	body := replayBody("batch-1", sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`))
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body); r.Code != http.StatusAccepted {
		t.Fatalf("first status = %d", r.Code)
	}
	changed := replayBody("batch-1",
		sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`),
		sample("e2", "2024-01-02T11:00:00Z", `{"v":2}`),
	)
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", changed)
	if r.Code != http.StatusConflict {
		t.Fatalf("changed batch status = %d, want 409: %s", r.Code, r.Body.String())
	}
	events := allEvents(t, h, "gw")
	if len(events) != 1 {
		t.Fatalf("409 batch left %d events, want 1", len(events))
	}

	// Reordered samples are also changed content.
	reordered := replayBody("batch-1",
		sample("e2", "2024-01-02T11:00:00Z", `{"v":2}`),
		sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`),
	)
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", reordered); r.Code != http.StatusConflict {
		t.Fatalf("reordered batch status = %d, want 409", r.Code)
	}
}

func TestReplayScopesIdentifiersPerDevice(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "a")
	mustRegister(t, h, "b")

	body := replayBody("shared-batch", sample("shared-event", "2024-01-02T10:00:00Z", `{"v":1}`))
	for _, id := range []string{"a", "b"} {
		if r := doRequest(t, h, http.MethodPost, "/v1/devices/"+id+"/replay", body); r.Code != http.StatusAccepted {
			t.Fatalf("device %s status = %d", id, r.Code)
		}
	}
	// Same batch + content again on each device is an independent idempotent hit.
	for _, id := range []string{"a", "b"} {
		if r := doRequest(t, h, http.MethodPost, "/v1/devices/"+id+"/replay", body); r.Code != http.StatusOK {
			t.Fatalf("device %s repeat status = %d, want 200", id, r.Code)
		}
		if events := allEvents(t, h, id); len(events) != 1 || events[0].Sequence != 1 {
			t.Fatalf("device %s history = %+v", id, events)
		}
	}
}

func TestReplayUnknownDeviceIs404AndValidationIs400(t *testing.T) {
	h := NewHandler(NewStore())

	// Valid request against an unknown device: 404.
	r := doRequest(t, h, http.MethodPost, "/v1/devices/ghost/replay",
		replayBody("b", sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`)))
	if r.Code != http.StatusNotFound {
		t.Fatalf("unknown device status = %d, want 404", r.Code)
	}

	// Invalid body against an unknown device is still 400.
	r = doRequest(t, h, http.MethodPost, "/v1/devices/ghost/replay", `{"batchId":"","samples":[]}`)
	if r.Code != http.StatusBadRequest {
		t.Fatalf("invalid body status = %d, want 400", r.Code)
	}
}

func TestReplayValidationFailures(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	good := sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`)

	cases := map[string]string{
		"not JSON":          `not json at all`,
		"two JSON values":   `{"batchId":"b","samples":[` + good + `]}{"batchId":"c","samples":[]}`,
		"unknown field":     `{"batchId":"b","samples":[` + good + `],"nope":1}`,
		"blank batchId":     `{"batchId":"   ","samples":[` + good + `]}`,
		"no samples":        `{"batchId":"b","samples":[]}`,
		"blank eventId":     `{"batchId":"b","samples":[` + sample("   ", "2024-01-02T10:00:00Z", `{"v":1}`) + `]}`,
		"empty values":      `{"batchId":"b","samples":[` + sample("e1", "2024-01-02T10:00:00Z", `{}`) + `]}`,
		"blank metric name": `{"batchId":"b","samples":[` + sample("e1", "2024-01-02T10:00:00Z", `{"  ":1}`) + `]}`,
		"bad observedAt":    `{"batchId":"b","samples":[` + sample("e1", "not-a-time", `{"v":1}`) + `]}`,
		"non-finite values": `{"batchId":"b","samples":[` + sample("e1", "2024-01-02T10:00:00Z", `{"v":1e999}`) + `]}`,
		"duplicate eventId": `{"batchId":"b","samples":[` + good + `,` + good + `]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body)
			if r.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", r.Code, r.Body.String())
			}
		})
	}

	t.Run("101 samples", func(t *testing.T) {
		samples := make([]string, 101)
		for i := range samples {
			samples[i] = sample(fmt.Sprintf("e%d", i), "2024-01-02T10:00:00Z", `{"v":1}`)
		}
		r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", replayBody("big", samples...))
		if r.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", r.Code)
		}
	})

	// Failed validation must not change history.
	if events := allEvents(t, h, "gw"); len(events) != 0 {
		t.Fatalf("failed requests left events: %+v", events)
	}
}

// --- history filtering, paging and cursors ----------------------------------

func TestHistoryReturnsReceiveSequenceOrder(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	// Observed timestamps deliberately out of order; receive sequence wins.
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b",
			sample("late", "2024-01-03T00:00:00Z", `{"v":3}`),
			sample("early", "2024-01-01T00:00:00Z", `{"v":1}`),
			sample("middle", "2024-01-02T00:00:00Z", `{"v":2}`),
		))
	if r.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d", r.Code)
	}
	events := allEvents(t, h, "gw")
	wantValues := []float64{3, 1, 2}
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	for i, event := range events {
		if event.Sequence != int64(i+1) {
			t.Fatalf("event %d sequence = %d, want %d", i, event.Sequence, i+1)
		}
		if event.Values["v"] != wantValues[i] {
			t.Fatalf("event %d value = %v, want %v (array insertion order)", i, event.Values["v"], wantValues[i])
		}
	}
}

func TestHistoryTimeFiltersInclusive(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b",
			sample("e1", "2024-01-01T00:00:00Z", `{"v":1}`),
			sample("e2", "2024-01-02T12:00:00Z", `{"v":2}`),
			sample("e3", "2024-01-03T00:00:00Z", `{"v":3}`),
		))
	if r.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d", r.Code)
	}

	// Both boundaries inclusive.
	page := doRequest(t, h, http.MethodGet,
		"/v1/devices/gw/history?from=2024-01-02T12:00:00Z&to=2024-01-03T00:00:00Z", "")
	if page.Code != http.StatusOK {
		t.Fatalf("filtered status = %d", page.Code)
	}
	resp := decodeBody[historyResponse](t, page)
	if len(resp.Events) != 2 || resp.Events[0].Sequence != 2 || resp.Events[1].Sequence != 3 {
		t.Fatalf("filtered events = %+v, want sequences 2,3", resp.Events)
	}

	for name, target := range map[string]string{
		"inverted range": "/v1/devices/gw/history?from=2024-01-03T00:00:00Z&to=2024-01-01T00:00:00Z",
		"bad from":       "/v1/devices/gw/history?from=yesterday",
		"bad to":         "/v1/devices/gw/history?to=nope",
		"limit zero":     "/v1/devices/gw/history?limit=0",
		"limit 101":      "/v1/devices/gw/history?limit=101",
		"limit text":     "/v1/devices/gw/history?limit=ten",
	} {
		t.Run(name, func(t *testing.T) {
			if r := doRequest(t, h, http.MethodGet, target, ""); r.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", r.Code, r.Body.String())
			}
		})
	}
}

func TestHistoryPagingAndDefaultLimit(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	for i := 0; i < 25; i++ {
		body := replayBody(fmt.Sprintf("batch-%d", i),
			sample(fmt.Sprintf("e%d", i), "2024-01-02T10:00:00Z", fmt.Sprintf(`{"v":%d}`, i)))
		if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body); r.Code != http.StatusAccepted {
			t.Fatalf("replay %d status = %d", i, r.Code)
		}
	}

	pages := walkHistory(t, h, "/v1/devices/gw/history?limit=10")
	wantSizes := []int{10, 10, 5}
	if len(pages) != 3 {
		t.Fatalf("pages = %d, want 3", len(pages))
	}
	var seen int64
	for i, page := range pages {
		if len(page.Events) != wantSizes[i] {
			t.Fatalf("page %d size = %d, want %d", i, len(page.Events), wantSizes[i])
		}
		for _, event := range page.Events {
			seen++
			if event.Sequence != seen {
				t.Fatalf("page %d sequence gap: got %d want %d", i, event.Sequence, seen)
			}
		}
	}
	if pages[len(pages)-1].NextCursor != nil {
		t.Fatalf("last page cursor = %v, want null", *pages[len(pages)-1].NextCursor)
	}

	// Default limit is 20.
	first := decodeBody[historyResponse](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/history", ""))
	if len(first.Events) != 20 || first.NextCursor == nil {
		t.Fatalf("default page size = %d cursor=%v, want 20 with cursor", len(first.Events), first.NextCursor)
	}
}

func TestHistoryPagingPinsFirstPageHighWater(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	for i := 0; i < 10; i++ {
		body := replayBody(fmt.Sprintf("b%d", i), sample(fmt.Sprintf("e%d", i), "2024-01-02T10:00:00Z", `{"v":1}`))
		if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body); r.Code != http.StatusAccepted {
			t.Fatalf("seed status = %d", r.Code)
		}
	}

	first := doRequest(t, h, http.MethodGet, "/v1/devices/gw/history?limit=5", "")
	page := decodeBody[historyResponse](t, first)
	if len(page.Events) != 5 || page.NextCursor == nil {
		t.Fatalf("first page = %+v", page)
	}
	cursor := *page.NextCursor

	// Writes landing between pages must not change the pinned walk.
	for i := 10; i < 20; i++ {
		body := replayBody(fmt.Sprintf("b%d", i), sample(fmt.Sprintf("e%d", i), "2024-01-02T10:00:00Z", `{"v":1}`))
		if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body); r.Code != http.StatusAccepted {
			t.Fatalf("mid-walk insert status = %d", r.Code)
		}
	}

	second := doRequest(t, h, http.MethodGet, "/v1/devices/gw/history?limit=5&cursor="+cursor, "")
	page2 := decodeBody[historyResponse](t, second)
	if len(page2.Events) != 5 {
		t.Fatalf("second page size = %d, want 5", len(page2.Events))
	}
	for _, event := range page2.Events {
		if event.Sequence > 10 {
			t.Fatalf("paged walk saw sequence %d beyond pinned high-water 10", event.Sequence)
		}
	}
	if page2.NextCursor != nil {
		t.Fatalf("cursor = %v, want null at pinned end", *page2.NextCursor)
	}

	// A fresh first page now sees all 20.
	fresh := decodeBody[historyResponse](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/history?limit=100", ""))
	if len(fresh.Events) != 20 {
		t.Fatalf("fresh walk events = %d, want 20", len(fresh.Events))
	}
}

func TestHistoryCursorValidation(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "a")
	mustRegister(t, h, "b")
	body := replayBody("b", sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`))
	doRequest(t, h, http.MethodPost, "/v1/devices/a/replay", body)
	doRequest(t, h, http.MethodPost, "/v1/devices/b/replay", body)
	doRequest(t, h, http.MethodPost, "/v1/devices/a/replay",
		replayBody("b2", sample("e2", "2024-01-02T11:00:00Z", `{"v":2}`)))
	doRequest(t, h, http.MethodPost, "/v1/devices/a/replay",
		replayBody("b3", sample("e3", "2024-01-02T12:00:00Z", `{"v":3}`)))

	cursor := func(device, query string) string {
		r := doRequest(t, h, http.MethodGet, "/v1/devices/"+device+"/history?"+query, "")
		resp := decodeBody[historyResponse](t, r)
		if resp.NextCursor == nil {
			t.Fatalf("no cursor for %s?%s", device, query)
		}
		return *resp.NextCursor
	}

	aCursor := cursor("a", "limit=1")

	// Foreign device.
	if r := doRequest(t, h, http.MethodGet, "/v1/devices/b/history?limit=1&cursor="+aCursor, ""); r.Code != http.StatusBadRequest {
		t.Fatalf("foreign device cursor status = %d, want 400", r.Code)
	}
	// Tampered token.
	tampered := aCursor
	if tampered[0] == 'A' {
		tampered = "B" + tampered[1:]
	} else {
		tampered = "A" + tampered[1:]
	}
	if r := doRequest(t, h, http.MethodGet, "/v1/devices/a/history?limit=1&cursor="+tampered, ""); r.Code != http.StatusBadRequest {
		t.Fatalf("tampered cursor status = %d, want 400", r.Code)
	}
	// Garbage token on a registered device.
	if r := doRequest(t, h, http.MethodGet, "/v1/devices/a/history?cursor=not-a-cursor", ""); r.Code != http.StatusBadRequest {
		t.Fatalf("garbage cursor status = %d, want 400", r.Code)
	}
	// Cursor reused with a different filter.
	filteredCursor := cursor("a", "limit=1&from=2024-01-02T10:00:00Z")
	if r := doRequest(t, h, http.MethodGet, "/v1/devices/a/history?limit=1&from=2024-01-03T00:00:00Z&cursor="+filteredCursor, ""); r.Code != http.StatusBadRequest {
		t.Fatalf("changed filter cursor status = %d, want 400", r.Code)
	}
	// Unknown device is 404, even with a malformed or foreign cursor.
	if r := doRequest(t, h, http.MethodGet, "/v1/devices/ghost/history?cursor=garbage", ""); r.Code != http.StatusNotFound {
		t.Fatalf("unknown device cursor status = %d, want 404", r.Code)
	}
	if r := doRequest(t, h, http.MethodGet, "/v1/devices/ghost/history?cursor="+aCursor, ""); r.Code != http.StatusNotFound {
		t.Fatalf("unknown device foreign cursor status = %d, want 404", r.Code)
	}
}

func TestHistoryUnknownDevice(t *testing.T) {
	h := NewHandler(NewStore())
	if r := doRequest(t, h, http.MethodGet, "/v1/devices/ghost/history", ""); r.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", r.Code)
	}
}

// --- concurrency ------------------------------------------------------------

func TestConcurrentReplays(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")

	const groups = 20
	const perGroup = 5
	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup

	// Distinct batches, globally unique eventIds.
	for g := 0; g < groups; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			start.Wait()
			samples := make([]string, perGroup)
			for i := 0; i < perGroup; i++ {
				samples[i] = sample(fmt.Sprintf("g%d-e%d", g, i), "2024-01-02T10:00:00Z", fmt.Sprintf(`{"g":%d,"i":%d}`, g, i))
			}
			r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", replayBody(fmt.Sprintf("batch-g%d", g), samples...))
			if r.Code != http.StatusAccepted {
				t.Errorf("group %d status = %d: %s", g, r.Code, r.Body.String())
			}
		}(g)
	}

	// Same batchId + identical content hammered concurrently: exactly one 202,
	// the rest 200, never 409.
	const dupCallers = 8
	var codesMu sync.Mutex
	codes := map[int]int{}
	for c := 0; c < dupCallers; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start.Wait()
			r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
				replayBody("dup-batch", sample("dup-event", "2024-01-02T10:00:00Z", `{"v":1}`)))
			codesMu.Lock()
			codes[r.Code]++
			codesMu.Unlock()
			if r.Code != http.StatusAccepted && r.Code != http.StatusOK {
				t.Errorf("duplicate batch status = %d: %s", r.Code, r.Body.String())
			}
		}()
	}

	start.Done()
	wg.Wait()

	if codes[http.StatusAccepted] != 1 || codes[http.StatusOK] != dupCallers-1 {
		t.Fatalf("dup-batch codes = %v, want one 202 and %d 200", codes, dupCallers-1)
	}

	events := allEvents(t, h, "gw")
	want := groups*perGroup + 1
	if len(events) != want {
		t.Fatalf("committed events = %d, want %d", len(events), want)
	}
	seen := make(map[int64]bool, len(events))
	for _, event := range events {
		if seen[event.Sequence] {
			t.Fatalf("duplicate sequence %d", event.Sequence)
		}
		seen[event.Sequence] = true
	}
	for seq := int64(1); seq <= int64(want); seq++ {
		if !seen[seq] {
			t.Fatalf("sequence %d missing; allocation was not contiguous", seq)
		}
	}
}
