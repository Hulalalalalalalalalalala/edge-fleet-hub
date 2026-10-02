package fleet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

type retentionResponse struct {
	MaxEvents        int64  `json:"maxEvents"`
	RetainedEvents   int64  `json:"retainedEvents"`
	EarliestSequence *int64 `json:"earliestSequence"`
	MaxSequence      int64  `json:"maxSequence"`
	Error            string `json:"error"`
}

func getRetention(t *testing.T, h http.Handler, id string) retentionResponse {
	t.Helper()
	r := doRequest(t, h, http.MethodGet, fmt.Sprintf("/v1/devices/%s/history/retention", id), "")
	if r.Code != http.StatusOK {
		t.Fatalf("GET retention status = %d body = %s", r.Code, r.Body.String())
	}
	return decodeBody[retentionResponse](t, r)
}

// sendTelemetry posts n live samples {"v":i}.
func sendTelemetry(t *testing.T, h http.Handler, id string, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		r := doRequest(t, h, http.MethodPost, fmt.Sprintf("/v1/devices/%s/telemetry", id),
			fmt.Sprintf(`{"v":%d}`, i))
		if r.Code != http.StatusAccepted {
			t.Fatalf("telemetry %d status = %d body = %s", i, r.Code, r.Body.String())
		}
	}
}

func sequencesOf(events []Event) []int64 {
	seqs := make([]int64, len(events))
	for i, event := range events {
		seqs[i] = event.Sequence
	}
	return seqs
}

// --- defaults and empty-device counters -------------------------------------

func TestRetentionDefaults(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")

	status := getRetention(t, h, "gw")
	if status.MaxEvents != 0 || status.RetainedEvents != 0 || status.EarliestSequence != nil || status.MaxSequence != 0 {
		t.Fatalf("fresh device retention = %+v, want unlimited empty counters", status)
	}

	sendTelemetry(t, h, "gw", 3)
	status = getRetention(t, h, "gw")
	if status.MaxEvents != 0 || status.RetainedEvents != 3 ||
		status.EarliestSequence == nil || *status.EarliestSequence != 1 || status.MaxSequence != 3 {
		t.Fatalf("after 3 samples retention = %+v", status)
	}
}

func TestRetentionUnknownDevice(t *testing.T) {
	h := NewHandler(NewStore())
	if r := doRequest(t, h, http.MethodGet, "/v1/devices/ghost/history/retention", ""); r.Code != http.StatusNotFound {
		t.Fatalf("GET unknown device status = %d, want 404", r.Code)
	}
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/ghost/history/retention", `{"maxEvents":5}`); r.Code != http.StatusNotFound {
		t.Fatalf("PUT unknown device status = %d, want 404", r.Code)
	}
}

// --- PUT validation ---------------------------------------------------------

func TestSetRetentionValidation(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")

	cases := map[string]string{
		"empty body":         ``,
		"missing field":      `{}`,
		"null":               `{"maxEvents":null}`,
		"negative":           `{"maxEvents":-1}`,
		"above range":        `{"maxEvents":10001}`,
		"fractional":         `{"maxEvents":2.5}`,
		"float-looking 1.0":  `{"maxEvents":1.0}`,
		"string":             `{"maxEvents":"5"}`,
		"boolean":            `{"maxEvents":true}`,
		"array":              `{"maxEvents":[5]}`,
		"two JSON documents": `{"maxEvents":5}{"maxEvents":6}`,
		"trailing garbage":   `{"maxEvents":5}garbage`,
		"unknown field":      `{"maxEvents":5,"extra":1}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/history/retention", body)
			if r.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", r.Code, r.Body.String())
			}
		})
	}

	// The rejected requests changed nothing.
	status := getRetention(t, h, "gw")
	if status.MaxEvents != 0 {
		t.Fatalf("retention changed after invalid PUTs: %+v", status)
	}
}

// --- trimming behaviour -----------------------------------------------------

func TestSetRetentionTrimsImmediatelyBySequence(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	sendTelemetry(t, h, "gw", 10)

	r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/history/retention", `{"maxEvents":3}`)
	if r.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body = %s", r.Code, r.Body.String())
	}
	status := decodeBody[retentionResponse](t, r)
	if status.MaxEvents != 3 || status.RetainedEvents != 3 ||
		status.EarliestSequence == nil || *status.EarliestSequence != 8 || status.MaxSequence != 10 {
		t.Fatalf("retention after trim = %+v", status)
	}
	events := allEvents(t, h, "gw")
	if got := sequencesOf(events); len(got) != 3 || got[0] != 8 || got[2] != 10 {
		t.Fatalf("history after trim = %v, want [8 9 10]", got)
	}

	// 0 means unlimited again: trimmed events do not come back.
	r = doRequest(t, h, http.MethodPut, "/v1/devices/gw/history/retention", `{"maxEvents":0}`)
	if r.Code != http.StatusOK {
		t.Fatalf("PUT unlimited status = %d: %s", r.Code, r.Body.String())
	}
	status = decodeBody[retentionResponse](t, r)
	if status.RetainedEvents != 3 || status.EarliestSequence == nil ||
		*status.EarliestSequence != 8 || status.MaxSequence != 10 {
		t.Fatalf("retention after unlimited = %+v, want no resurrection", status)
	}

	// New samples continue after the historical maximum.
	sendTelemetry(t, h, "gw", 1)
	events = allEvents(t, h, "gw")
	if got := sequencesOf(events); len(got) != 4 || got[3] != 11 {
		t.Fatalf("history after new sample = %v, want 8..11", got)
	}
	status = getRetention(t, h, "gw")
	if status.MaxSequence != 11 {
		t.Fatalf("maxSequence = %d, want 11", status.MaxSequence)
	}
}

func TestRaisingLimitOnlyAffectsFuture(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	sendTelemetry(t, h, "gw", 10)

	doRequest(t, h, http.MethodPut, "/v1/devices/gw/history/retention", `{"maxEvents":3}`)
	r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/history/retention", `{"maxEvents":8}`)
	if r.Code != http.StatusOK {
		t.Fatalf("raise status = %d", r.Code)
	}
	status := decodeBody[retentionResponse](t, r)
	if status.RetainedEvents != 3 || *status.EarliestSequence != 8 {
		t.Fatalf("raising the limit restored events: %+v", status)
	}

	sendTelemetry(t, h, "gw", 5)
	status = getRetention(t, h, "gw")
	if status.RetainedEvents != 8 || *status.EarliestSequence != 8 || status.MaxSequence != 15 {
		t.Fatalf("after growth retention = %+v, want 8 retained 8..15", status)
	}
}

func TestRetentionEnforcedOnLiveWrites(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/history/retention", `{"maxEvents":2}`); r.Code != http.StatusOK {
		t.Fatalf("PUT status = %d", r.Code)
	}
	sendTelemetry(t, h, "gw", 5)
	events := allEvents(t, h, "gw")
	if got := sequencesOf(events); len(got) != 2 || got[0] != 4 || got[1] != 5 {
		t.Fatalf("history = %v, want [4 5]", got)
	}
	status := getRetention(t, h, "gw")
	if *status.EarliestSequence != 4 || status.MaxSequence != 5 {
		t.Fatalf("counters = %+v", status)
	}

	// Device last telemetry and active time reflect the newest sample.
	fleet := decodeBody[struct {
		Devices []Device `json:"devices"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/fleet", ""))
	if got := fleet.Devices[0].LastTelemetry["v"]; got != 5 {
		t.Fatalf("last telemetry = %v, want 5", fleet.Devices[0].LastTelemetry)
	}
}

func TestRetentionIsPerDevice(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "a")
	mustRegister(t, h, "b")
	sendTelemetry(t, h, "a", 5)
	sendTelemetry(t, h, "b", 5)
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/a/history/retention", `{"maxEvents":2}`); r.Code != http.StatusOK {
		t.Fatalf("PUT status = %d", r.Code)
	}
	if events := allEvents(t, h, "a"); len(events) != 2 {
		t.Fatalf("device a retained %d, want 2", len(events))
	}
	if events := allEvents(t, h, "b"); len(events) != 5 {
		t.Fatalf("device b retained %d, want 5 (unaffected)", len(events))
	}
	if status := getRetention(t, h, "b"); status.MaxEvents != 0 {
		t.Fatalf("device b maxEvents = %d, want 0", status.MaxEvents)
	}
}

// Observed-time order must not change retention order: older observedAt
// samples arriving late keep their receive-sequence position.
func TestRetentionUsesReceiveSequenceNotObservedTime(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	body := replayBody("b1",
		sample("e1", "2024-03-01T00:00:00Z", `{"v":1}`),
		sample("e2", "2024-01-01T00:00:00Z", `{"v":2}`),
		sample("e3", "2024-02-01T00:00:00Z", `{"v":3}`),
	)
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body); r.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d: %s", r.Code, r.Body.String())
	}
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/history/retention", `{"maxEvents":1}`); r.Code != http.StatusOK {
		t.Fatalf("PUT status = %d", r.Code)
	}
	events := allEvents(t, h, "gw")
	if len(events) != 1 || events[0].Sequence != 3 {
		t.Fatalf("history = %+v, want only receive sequence 3 even though it is not the newest observedAt", events)
	}
}

// --- replay batches larger than the cap -------------------------------------

func TestReplayBatchOverCapKeepsLastAndListsAll(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/history/retention", `{"maxEvents":2}`); r.Code != http.StatusOK {
		t.Fatalf("PUT status = %d", r.Code)
	}
	samples := []string{
		sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`),
		sample("e2", "2024-01-02T10:01:00Z", `{"v":2}`),
		sample("e3", "2024-01-02T10:02:00Z", `{"v":3}`),
		sample("e4", "2024-01-02T10:03:00Z", `{"v":4}`),
		sample("e5", "2024-01-02T10:04:00Z", `{"v":5}`),
	}
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", replayBody("big", samples...))
	if r.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d: %s", r.Code, r.Body.String())
	}
	receipt := decodeBody[receiptResponse](t, r)
	if receipt.NewCount != 5 || receipt.DuplicateCount != 0 || len(receipt.Samples) != 5 {
		t.Fatalf("receipt = %+v, want all 5 samples listed as new", receipt)
	}
	for i, sample := range receipt.Samples {
		if sample.Sequence != int64(i+1) || sample.Duplicate {
			t.Fatalf("receipt sample %d = %+v, want sequence %d non-duplicate", i, sample, i+1)
		}
	}
	events := allEvents(t, h, "gw")
	if got := sequencesOf(events); len(got) != 2 || got[0] != 4 || got[1] != 5 {
		t.Fatalf("history = %v, want only [4 5]", got)
	}
	status := getRetention(t, h, "gw")
	if status.MaxSequence != 5 || status.RetainedEvents != 2 || *status.EarliestSequence != 4 {
		t.Fatalf("counters = %+v", status)
	}
}

// Every new sample in an over-cap batch still participates in rule evaluation;
// alerts and their trigger evidence survive the trimming.
func TestReplayBatchOverCapStillJudgesRules(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	createRule := doRequest(t, h, http.MethodPost, "/v1/devices/gw/rules",
		`{"id":"r1","metric":"v","trigger":10,"recover":0}`)
	if createRule.Code != http.StatusCreated {
		t.Fatalf("create rule status = %d", createRule.Code)
	}
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/history/retention", `{"maxEvents":2}`); r.Code != http.StatusOK {
		t.Fatalf("PUT status = %d", r.Code)
	}
	body := replayBody("b1",
		sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`),
		sample("e2", "2024-01-02T10:01:00Z", `{"v":11}`), // triggers, later trimmed
		sample("e3", "2024-01-02T10:02:00Z", `{"v":2}`),
		sample("e4", "2024-01-02T10:03:00Z", `{"v":12}`), // keeps the alert active, trimmed
		sample("e5", "2024-01-02T10:04:00Z", `{"v":3}`),
	)
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body); r.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d: %s", r.Code, r.Body.String())
	}
	alerts := decodeBody[struct {
		Alerts []Alert `json:"alerts"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/alerts", ""))
	if len(alerts.Alerts) != 1 {
		t.Fatalf("alerts = %+v, want exactly 1", alerts.Alerts)
	}
	if alerts.Alerts[0].TriggerSequence != 2 || alerts.Alerts[0].Status != alertStatusActive {
		t.Fatalf("alert = %+v, want trigger sequence 2 still active", alerts.Alerts[0])
	}
}

func TestTrimmedDuplicateStaysDuplicateAndDoesNotReenter(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/history/retention", `{"maxEvents":2}`); r.Code != http.StatusOK {
		t.Fatalf("PUT status = %d", r.Code)
	}
	first := replayBody("b1",
		sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`),
		sample("e2", "2024-01-02T10:01:00Z", `{"v":2}`),
		sample("e3", "2024-01-02T10:02:00Z", `{"v":3}`),
	)
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", first); r.Code != http.StatusAccepted {
		t.Fatalf("first replay status = %d: %s", r.Code, r.Body.String())
	}
	// e1 (sequence 1) has been trimmed; seqs 2,3 remain.
	fleetBefore := decodeBody[struct {
		Devices []Device `json:"devices"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/fleet", ""))

	// Same content: still a duplicate with the original sequence, even though
	// its event left history. It must not re-enter, evict anything or refresh
	// the device.
	dup := replayBody("b2", sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`))
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", dup)
	if r.Code != http.StatusAccepted {
		t.Fatalf("trimmed duplicate status = %d: %s", r.Code, r.Body.String())
	}
	receipt := decodeBody[receiptResponse](t, r)
	if receipt.NewCount != 0 || receipt.DuplicateCount != 1 ||
		len(receipt.Samples) != 1 || receipt.Samples[0].Sequence != 1 || !receipt.Samples[0].Duplicate {
		t.Fatalf("trimmed duplicate receipt = %+v, want dup of sequence 1", receipt)
	}
	after := allEvents(t, h, "gw")
	if got := sequencesOf(after); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("history after trimmed duplicate = %v, want unchanged [2 3]", got)
	}
	status := getRetention(t, h, "gw")
	if status.MaxSequence != 3 {
		t.Fatalf("duplicate consumed sequence: maxSequence = %d, want 3", status.MaxSequence)
	}
	fleetAfter := decodeBody[struct {
		Devices []Device `json:"devices"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/fleet", ""))
	if !fleetBefore.Devices[0].LastSeenAt.Equal(fleetAfter.Devices[0].LastSeenAt) {
		t.Fatalf("duplicate refreshed LastSeenAt: %v -> %v", fleetBefore.Devices[0].LastSeenAt, fleetAfter.Devices[0].LastSeenAt)
	}

	// Different content for the same eventId still conflicts the whole batch.
	conflict := replayBody("b3", sample("e1", "2024-01-02T10:00:00Z", `{"v":99}`))
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", conflict); r.Code != http.StatusConflict {
		t.Fatalf("trimmed conflict status = %d, want 409: %s", r.Code, r.Body.String())
	}
	if got := sequencesOf(allEvents(t, h, "gw")); len(got) != 2 || got[0] != 2 {
		t.Fatalf("history changed after conflict = %v", got)
	}

	// Retrying the original batch returns the first receipt (200).
	retry := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", first)
	if retry.Code != http.StatusOK {
		t.Fatalf("batch retry status = %d, want 200: %s", retry.Code, retry.Body.String())
	}
	retryReceipt := decodeBody[receiptResponse](t, retry)
	if retryReceipt.NewCount != 3 || len(retryReceipt.Samples) != 3 {
		t.Fatalf("retry receipt = %+v, want original receipt", retryReceipt)
	}
}

// --- pagination vs concurrent retention changes ----------------------------

func firstPage(t *testing.T, h http.Handler, id string, limit int) (historyResponse, string) {
	t.Helper()
	r := doRequest(t, h, http.MethodGet, fmt.Sprintf("/v1/devices/%s/history?limit=%d", id, limit), "")
	if r.Code != http.StatusOK {
		t.Fatalf("first page status = %d: %s", r.Code, r.Body.String())
	}
	page := decodeBody[historyResponse](t, r)
	if page.NextCursor == nil {
		t.Fatalf("expected a cursor")
	}
	return page, *page.NextCursor
}

func TestPaginationSurvivesTrimBeforeCursorStart(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	sendTelemetry(t, h, "gw", 10)

	page, cursor := firstPage(t, h, "gw", 5)
	if got := sequencesOf(page.Events); len(got) != 5 || got[0] != 1 || got[4] != 5 {
		t.Fatalf("first page = %v, want 1..5", got)
	}

	// Trim only sequences strictly before the cursor start (1..2): the
	// continuation stays valid and still honours the pinned high-water mark.
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/history/retention", `{"maxEvents":8}`); r.Code != http.StatusOK {
		t.Fatalf("PUT status = %d", r.Code)
	}
	// New samples arriving meanwhile must not leak into the pinned walk and
	// must not trim the cursor start away (limit 8 keeps 5..12).
	sendTelemetry(t, h, "gw", 2)
	r := doRequest(t, h, http.MethodGet, "/v1/devices/gw/history?limit=5&cursor="+cursor, "")
	if r.Code != http.StatusOK {
		t.Fatalf("continuation status = %d, want 200: %s", r.Code, r.Body.String())
	}
	page2 := decodeBody[historyResponse](t, r)
	if got := sequencesOf(page2.Events); len(got) != 5 || got[0] != 6 || got[4] != 10 {
		t.Fatalf("second page = %v, want 6..10 within the pinned bound", got)
	}
	if page2.NextCursor != nil {
		t.Fatalf("second page cursor = %v, want null at pinned end", *page2.NextCursor)
	}
	// The fresh samples are visible only through a new first page.
	fresh := decodeBody[historyResponse](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/history?limit=20", ""))
	if got := sequencesOf(fresh.Events); len(got) != 8 || got[0] != 5 || got[7] != 12 {
		t.Fatalf("fresh page = %v, want retained 5..12", got)
	}
}

func TestPaginationReturns410WhenCursorStartTrimmed(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	sendTelemetry(t, h, "gw", 10)

	_, cursor := firstPage(t, h, "gw", 2) // next page starts at sequence 3

	// Trim through sequence 4: the cursor start (3) is gone.
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/history/retention", `{"maxEvents":6}`); r.Code != http.StatusOK {
		t.Fatalf("PUT status = %d", r.Code)
	}
	r := doRequest(t, h, http.MethodGet, "/v1/devices/gw/history?limit=2&cursor="+cursor, "")
	if r.Code != http.StatusGone {
		t.Fatalf("continuation status = %d, want 410: %s", r.Code, r.Body.String())
	}
	var body struct {
		Error            string `json:"error"`
		EarliestSequence int64  `json:"earliestSequence"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 410 body: %v", err)
	}
	if body.EarliestSequence != 5 {
		t.Fatalf("410 earliestSequence = %d, want 5", body.EarliestSequence)
	}

	// A fresh first page works and starts at the current earliest sequence.
	fresh := doRequest(t, h, http.MethodGet, "/v1/devices/gw/history?limit=2", "")
	if fresh.Code != http.StatusOK {
		t.Fatalf("fresh query status = %d", fresh.Code)
	}
	page := decodeBody[historyResponse](t, fresh)
	if got := sequencesOf(page.Events); len(got) != 2 || got[0] != 5 || got[1] != 6 {
		t.Fatalf("fresh page = %v, want 5..6", got)
	}
}

func TestPaginationCursorStillValidatedAfterTrimming(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "a")
	mustRegister(t, h, "b")
	sendTelemetry(t, h, "a", 4)
	_, cursor := firstPage(t, h, "a", 1)

	// Tampering remains 400 even once the genuine cursor start is trimmed.
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/a/history/retention", `{"maxEvents":1}`); r.Code != http.StatusOK {
		t.Fatalf("PUT status = %d", r.Code)
	}
	tampered := cursor[:len(cursor)-2]
	if tampered[len(tampered)-1] == 'A' {
		tampered = tampered[:len(tampered)-1] + "B"
	} else {
		tampered = tampered[:len(tampered)-1] + "A"
	}
	if r := doRequest(t, h, http.MethodGet, "/v1/devices/a/history?limit=1&cursor="+tampered, ""); r.Code != http.StatusBadRequest {
		t.Fatalf("tampered cursor status = %d, want 400", r.Code)
	}
	// Foreign device is still 400, unknown device 404.
	if r := doRequest(t, h, http.MethodGet, "/v1/devices/b/history?limit=1&cursor="+cursor, ""); r.Code != http.StatusBadRequest {
		t.Fatalf("foreign cursor status = %d, want 400", r.Code)
	}
	if r := doRequest(t, h, http.MethodGet, "/v1/devices/ghost/history?limit=1&cursor="+cursor, ""); r.Code != http.StatusNotFound {
		t.Fatalf("unknown device cursor status = %d, want 404", r.Code)
	}
}

// Filtered pagination: the missing range must never be silently skipped, even
// when every trimmed event would have failed the filter.
func TestPagination410RegardlessOfFilter(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	// All samples share an observed time; the filter excludes none of them.
	body := replayBody("b1",
		sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`),
		sample("e2", "2024-01-02T10:01:00Z", `{"v":2}`),
		sample("e3", "2024-01-02T10:02:00Z", `{"v":3}`),
		sample("e4", "2024-01-02T10:03:00Z", `{"v":4}`),
	)
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body); r.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d", r.Code)
	}
	r := doRequest(t, h, http.MethodGet, "/v1/devices/gw/history?from=2024-01-02T10:00:00Z&limit=1", "")
	page := decodeBody[historyResponse](t, r)
	cursor := *page.NextCursor
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/history/retention", `{"maxEvents":1}`); r.Code != http.StatusOK {
		t.Fatalf("PUT status = %d", r.Code)
	}
	cont := doRequest(t, h, http.MethodGet,
		"/v1/devices/gw/history?from=2024-01-02T10:00:00Z&limit=1&cursor="+cursor, "")
	if cont.Code != http.StatusGone {
		t.Fatalf("filtered continuation status = %d, want 410: %s", cont.Code, cont.Body.String())
	}
}

// Retention changes never refresh last-active time or last telemetry.
func TestSetRetentionDoesNotRefreshDeviceState(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	sendTelemetry(t, h, "gw", 2)
	fleet1 := decodeBody[struct {
		Devices []Device `json:"devices"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/fleet", ""))

	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/history/retention", `{"maxEvents":1}`); r.Code != http.StatusOK {
		t.Fatalf("PUT status = %d", r.Code)
	}
	fleet2 := decodeBody[struct {
		Devices []Device `json:"devices"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/fleet", ""))
	if !fleet1.Devices[0].LastSeenAt.Equal(fleet2.Devices[0].LastSeenAt) {
		t.Fatalf("PUT refreshed LastSeenAt: %v -> %v", fleet1.Devices[0].LastSeenAt, fleet2.Devices[0].LastSeenAt)
	}
	if fleet2.Devices[0].LastTelemetry["v"] != 2 {
		t.Fatalf("last telemetry changed: %v", fleet2.Devices[0].LastTelemetry)
	}
}

// A duplicate inside a brand-new batch (same eventId, history trimmed) keeps
// batch semantics: sequences are not consumed and alerts are not re-fired.
func TestDuplicateInNewBatchDoesNotConsumeSequenceOrFireAlert(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	cr := doRequest(t, h, http.MethodPost, "/v1/devices/gw/rules",
		`{"id":"r1","metric":"v","trigger":10,"recover":0}`)
	if cr.Code != http.StatusCreated {
		t.Fatalf("create rule status = %d", cr.Code)
	}
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/history/retention", `{"maxEvents":1}`); r.Code != http.StatusOK {
		t.Fatalf("PUT status = %d", r.Code)
	}
	first := replayBody("b1",
		sample("e1", "2024-01-02T10:00:00Z", `{"v":11}`),
		sample("e2", "2024-01-02T10:01:00Z", `{"v":12}`),
	)
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", first); r.Code != http.StatusAccepted {
		t.Fatalf("first batch status = %d: %s", r.Code, r.Body.String())
	}
	// e1 fired the alert; both trimmed except e2 (seq 2).
	alerts := decodeBody[struct {
		Alerts []Alert `json:"alerts"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/alerts", ""))
	if len(alerts.Alerts) != 1 {
		t.Fatalf("alerts before dup = %d, want 1", len(alerts.Alerts))
	}

	// A new batch that re-mentions trimmed e1 (identical) plus a genuinely new
	// e3: the duplicate judges nothing and consumes no sequence; e3 gets 3.
	body := replayBody("b2",
		sample("e1", "2024-01-02T10:00:00Z", `{"v":11}`),
		sample("e3", "2024-01-02T10:02:00Z", `{"v":13}`),
	)
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body)
	if r.Code != http.StatusAccepted {
		t.Fatalf("second batch status = %d: %s", r.Code, r.Body.String())
	}
	receipt := decodeBody[receiptResponse](t, r)
	if receipt.NewCount != 1 || receipt.DuplicateCount != 1 || len(receipt.Samples) != 2 {
		t.Fatalf("receipt = %+v, want 1 new (seq 3) + 1 dup (seq 1)", receipt)
	}
	if receipt.Samples[0].Sequence != 1 || !receipt.Samples[0].Duplicate {
		t.Fatalf("dup sample = %+v, want sequence 1 duplicate", receipt.Samples[0])
	}
	if receipt.Samples[1].Sequence != 3 || receipt.Samples[1].Duplicate {
		t.Fatalf("new sample = %+v, want sequence 3", receipt.Samples[1])
	}
	alerts = decodeBody[struct {
		Alerts []Alert `json:"alerts"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/alerts", ""))
	if len(alerts.Alerts) != 1 {
		t.Fatalf("duplicate re-fired an alert: %d alerts now", len(alerts.Alerts))
	}
	status := getRetention(t, h, "gw")
	if status.MaxSequence != 3 || status.RetainedEvents != 1 {
		t.Fatalf("counters = %+v, want maxSequence 3 retained 1", status)
	}
}

func TestLegacyCursorTranslatesToSequenceCoordinates(t *testing.T) {
	raw := encodeLegacyCursorForTest(t, "gw", 10, 5)
	cursor, err := decodeCursor(raw)
	if err != nil {
		t.Fatalf("decode legacy cursor: %v", err)
	}
	if cursor.StartSeq != 6 || cursor.HighWater != 10 || cursor.DeviceID != "gw" {
		t.Fatalf("legacy cursor = %+v, want start sequence 6", cursor)
	}
}
