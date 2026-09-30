package fleet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

type historyPage struct {
	Samples []struct {
		Seq        int64              `json:"seq"`
		ObservedAt string             `json:"observedAt"`
		Values     map[string]float64 `json:"values"`
	} `json:"samples"`
	NextCursor *string `json:"nextCursor"`
}

func fetchHistory(t *testing.T, h http.Handler, id, query string) historyPage {
	t.Helper()
	rec := doJSON(t, h, http.MethodGet, "/v1/devices/"+id+"/history?"+query, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("history status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var page historyPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	return page
}

func replaySample(t *testing.T, h http.Handler, id, batchID, eventID, observedAt string, values map[string]float64) {
	t.Helper()
	valuesJSON, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{
		"batchId": %q,
		"samples": [{"eventId": %q, "observedAt": %q, "values": %s}]
	}`, batchID, eventID, observedAt, string(valuesJSON))
	rec := doJSON(t, h, http.MethodPost, "/v1/devices/"+id+"/replay", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("replay %s: %d %s", eventID, rec.Code, rec.Body.String())
	}
}

func TestTelemetryWritesHistory(t *testing.T) {
	h, _ := newTestHandler(t)
	registerDevice(t, h, "dev-1")

	rec := doJSON(t, h, http.MethodPost, "/v1/devices/dev-1/telemetry", `{"temperature":21.75}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("telemetry: %d %s", rec.Code, rec.Body.String())
	}

	page := fetchHistory(t, h, "dev-1", "")
	if len(page.Samples) != 1 {
		t.Fatalf("history count = %d, want 1", len(page.Samples))
	}
	sample := page.Samples[0]
	if sample.Seq != 1 || sample.Values["temperature"] != 21.75 {
		t.Fatalf("sample = %+v, want seq 1 temperature 21.75", sample)
	}
	if sample.ObservedAt == "" {
		t.Fatal("observedAt is empty")
	}
	if page.NextCursor != nil {
		t.Fatalf("nextCursor = %q, want nil", *page.NextCursor)
	}
}

func TestHistoryFiltersByTimeRange(t *testing.T) {
	h, _ := newTestHandler(t)
	registerDevice(t, h, "dev-1")
	for i, ts := range []string{
		"2026-01-01T00:00:00Z",
		"2026-01-02T00:00:00Z",
		"2026-01-03T00:00:00Z",
	} {
		replaySample(t, h, "dev-1", fmt.Sprintf("b-%d", i), fmt.Sprintf("e-%d", i), ts, map[string]float64{"v": float64(i + 1)})
	}

	// Both boundaries are inclusive.
	page := fetchHistory(t, h, "dev-1", "from=2026-01-01T00:00:00Z&to=2026-01-03T00:00:00Z")
	if len(page.Samples) != 3 {
		t.Fatalf("inclusive range count = %d, want 3", len(page.Samples))
	}
	page = fetchHistory(t, h, "dev-1", "from=2026-01-02T00:00:00Z&to=2026-01-02T00:00:00Z")
	if len(page.Samples) != 1 || page.Samples[0].Values["v"] != 2 {
		t.Fatalf("single-day range = %+v, want v=2", page.Samples)
	}
	page = fetchHistory(t, h, "dev-1", "from=2025-12-31T00:00:00Z&to=2025-12-31T23:59:59Z")
	if len(page.Samples) != 0 {
		t.Fatalf("outside range count = %d, want 0", len(page.Samples))
	}
}

func TestHistoryPagination(t *testing.T) {
	h, _ := newTestHandler(t)
	registerDevice(t, h, "dev-1")
	for i := 0; i < 5; i++ {
		replaySample(t, h, "dev-1", fmt.Sprintf("b-%d", i), fmt.Sprintf("e-%d", i),
			fmt.Sprintf("2026-01-0%dT00:00:00Z", i+1), map[string]float64{"v": float64(i + 1)})
	}

	page := fetchHistory(t, h, "dev-1", "limit=2")
	if len(page.Samples) != 2 || page.Samples[0].Seq != 1 || page.Samples[1].Seq != 2 {
		t.Fatalf("first page = %+v, want seq 1,2", page.Samples)
	}
	if page.NextCursor == nil {
		t.Fatal("first page cursor is nil")
	}

	page = fetchHistory(t, h, "dev-1", "limit=2&cursor="+*page.NextCursor)
	if len(page.Samples) != 2 || page.Samples[0].Seq != 3 || page.Samples[1].Seq != 4 {
		t.Fatalf("second page = %+v, want seq 3,4", page.Samples)
	}
	if page.NextCursor == nil {
		t.Fatal("second page cursor is nil")
	}

	page = fetchHistory(t, h, "dev-1", "limit=2&cursor="+*page.NextCursor)
	if len(page.Samples) != 1 || page.Samples[0].Seq != 5 {
		t.Fatalf("third page = %+v, want seq 5", page.Samples)
	}
	if page.NextCursor != nil {
		t.Fatalf("third page cursor = %q, want nil", *page.NextCursor)
	}
}

func TestHistoryPaginationStableUnderWrites(t *testing.T) {
	h, _ := newTestHandler(t)
	registerDevice(t, h, "dev-1")
	for i := 0; i < 5; i++ {
		replaySample(t, h, "dev-1", fmt.Sprintf("b-%d", i), fmt.Sprintf("e-%d", i),
			fmt.Sprintf("2026-01-0%dT00:00:00Z", i+1), map[string]float64{"v": float64(i + 1)})
	}

	// First page fixes the upper bound at the then-max seq (5).
	page := fetchHistory(t, h, "dev-1", "limit=2")
	if len(page.Samples) != 2 || page.Samples[1].Seq != 2 {
		t.Fatalf("first page = %+v, want seq 1,2", page.Samples)
	}
	cursor := *page.NextCursor

	// Concurrent writes happen between pages.
	rec := doJSON(t, h, http.MethodPost, "/v1/devices/dev-1/telemetry", `{"temperature":99}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("telemetry: %d %s", rec.Code, rec.Body.String())
	}
	replaySample(t, h, "dev-1", "b-5", "e-5", "2026-01-06T00:00:00Z", map[string]float64{"v": 6})

	// Remaining pages stay within the fixed upper bound: no skips, no dupes.
	page = fetchHistory(t, h, "dev-1", "limit=2&cursor="+cursor)
	if len(page.Samples) != 2 || page.Samples[0].Seq != 3 || page.Samples[1].Seq != 4 {
		t.Fatalf("page after writes = %+v, want seq 3,4", page.Samples)
	}
	page = fetchHistory(t, h, "dev-1", "limit=2&cursor="+*page.NextCursor)
	if len(page.Samples) != 1 || page.Samples[0].Seq != 5 {
		t.Fatalf("last page = %+v, want seq 5", page.Samples)
	}
	if page.NextCursor != nil {
		t.Fatalf("cursor = %q, want nil", *page.NextCursor)
	}

	// A fresh first page sees the new writes.
	page = fetchHistory(t, h, "dev-1", "")
	if len(page.Samples) != 7 {
		t.Fatalf("fresh history count = %d, want 7", len(page.Samples))
	}
}

func TestHistoryValidation(t *testing.T) {
	h, _ := newTestHandler(t)
	registerDevice(t, h, "dev-1")
	replaySample(t, h, "dev-1", "b-1", "e-1", "2026-01-01T00:00:00Z", map[string]float64{"v": 1})

	cases := map[string]string{
		"bad from":          "from=not-a-time",
		"bad to":            "to=not-a-time",
		"inverted range":    "from=2026-01-02T00:00:00Z&to=2026-01-01T00:00:00Z",
		"limit zero":        "limit=0",
		"limit too large":   "limit=101",
		"limit non-numeric": "limit=abc",
		"empty cursor":      "cursor=",
		"tampered cursor":   "cursor=not-base64-json",
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(t, h, http.MethodGet, "/v1/devices/dev-1/history?"+query, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}
}

func TestHistoryCursorBoundToDeviceAndFilters(t *testing.T) {
	h, _ := newTestHandler(t)
	registerDevice(t, h, "dev-1")
	registerDevice(t, h, "dev-2")
	replaySample(t, h, "dev-1", "b-1", "e-1", "2026-01-01T00:00:00Z", map[string]float64{"v": 1})
	replaySample(t, h, "dev-1", "b-2", "e-2", "2026-01-02T00:00:00Z", map[string]float64{"v": 2})
	replaySample(t, h, "dev-2", "b-1", "e-1", "2026-01-01T00:00:00Z", map[string]float64{"v": 1})

	page := fetchHistory(t, h, "dev-1", "limit=1")
	if page.NextCursor == nil {
		t.Fatal("expected a cursor")
	}
	cursor := *page.NextCursor

	// Cursor cannot be used against another device.
	rec := doJSON(t, h, http.MethodGet, "/v1/devices/dev-2/history?limit=1&cursor="+cursor, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-device cursor status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	// Cursor cannot be reused with changed filters.
	rec = doJSON(t, h, http.MethodGet, "/v1/devices/dev-1/history?limit=1&from=2026-01-01T00:00:00Z&cursor="+cursor, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("changed-filter cursor status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHistoryUnknownDevice(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doJSON(t, h, http.MethodGet, "/v1/devices/missing/history", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHistoryDefaultLimit(t *testing.T) {
	h, _ := newTestHandler(t)
	registerDevice(t, h, "dev-1")
	for i := 0; i < 25; i++ {
		replaySample(t, h, "dev-1", fmt.Sprintf("b-%d", i), fmt.Sprintf("e-%d", i),
			"2026-01-01T00:00:00Z", map[string]float64{"v": float64(i)})
	}
	page := fetchHistory(t, h, "dev-1", "")
	if len(page.Samples) != 20 {
		t.Fatalf("default page size = %d, want 20", len(page.Samples))
	}
	if page.NextCursor == nil {
		t.Fatal("expected a cursor after 20 samples")
	}
}
