package fleet

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

const batchBody = `{
	"batchId": "b-1",
	"samples": [
		{"eventId": "e-1", "observedAt": "2026-09-30T08:15:00Z", "values": {"temperature": 22.4, "humidity": 45}},
		{"eventId": "e-2", "observedAt": "2026-09-30T08:16:00Z", "values": {"temperature": 22.6}}
	]
}`

func TestReplayNewBatchAccepted(t *testing.T) {
	h, _ := newTestHandler(t)
	registerDevice(t, h, "dev-1")

	rec := doJSON(t, h, http.MethodPost, "/v1/devices/dev-1/replay", batchBody)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d, want %d: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var receipt ReplayReceipt
	decodeBody(t, rec, &receipt)
	if receipt.BatchID != "b-1" || receipt.NewCount != 2 || receipt.DuplicateCount != 0 {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}
	wantStatuses := []string{"new", "new"}
	for i, result := range receipt.Results {
		if result.Seq != int64(i+1) || result.Status != wantStatuses[i] {
			t.Fatalf("result[%d] = %+v, want seq=%d status=%s", i, result, i+1, wantStatuses[i])
		}
	}

	page := fetchHistory(t, h, "dev-1", "")
	if len(page.Samples) != 2 || page.NextCursor != nil {
		t.Fatalf("history = %+v, want 2 samples and null cursor", page)
	}
}

func TestReplayUnknownDeviceNotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doJSON(t, h, http.MethodPost, "/v1/devices/missing/replay", batchBody)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestReplayCrossBatchDuplicate(t *testing.T) {
	h, _ := newTestHandler(t)
	registerDevice(t, h, "dev-1")

	first := doJSON(t, h, http.MethodPost, "/v1/devices/dev-1/replay", batchBody)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first batch: %d %s", first.Code, first.Body.String())
	}

	// b-2 reuses e-1 with identical content and adds e-3.
	secondBody := `{
		"batchId": "b-2",
		"samples": [
			{"eventId": "e-1", "observedAt": "2026-09-30T08:15:00Z", "values": {"humidity": 45, "temperature": 22.4}},
			{"eventId": "e-3", "observedAt": "2026-09-30T08:17:00Z", "values": {"temperature": 23.1}}
		]
	}`
	rec := doJSON(t, h, http.MethodPost, "/v1/devices/dev-1/replay", secondBody)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("second batch: %d %s", rec.Code, rec.Body.String())
	}
	var receipt ReplayReceipt
	decodeBody(t, rec, &receipt)
	if receipt.NewCount != 1 || receipt.DuplicateCount != 1 {
		t.Fatalf("counts = new %d dup %d, want 1/1", receipt.NewCount, receipt.DuplicateCount)
	}
	if receipt.Results[0].Seq != 1 || receipt.Results[0].Status != "duplicate" {
		t.Fatalf("e-1 result = %+v, want seq=1 duplicate", receipt.Results[0])
	}
	if receipt.Results[1].Seq != 3 || receipt.Results[1].Status != "new" {
		t.Fatalf("e-3 result = %+v, want seq=3 new", receipt.Results[1])
	}

	page := fetchHistory(t, h, "dev-1", "")
	if len(page.Samples) != 3 {
		t.Fatalf("history count = %d, want 3", len(page.Samples))
	}
}

func TestReplayConflictRollsBack(t *testing.T) {
	h, _ := newTestHandler(t)
	registerDevice(t, h, "dev-1")

	first := doJSON(t, h, http.MethodPost, "/v1/devices/dev-1/replay", batchBody)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first batch: %d %s", first.Code, first.Body.String())
	}

	// b-2 changes the content of e-1: conflict, whole batch rejected.
	conflictBody := `{
		"batchId": "b-2",
		"samples": [
			{"eventId": "e-1", "observedAt": "2026-09-30T08:15:00Z", "values": {"temperature": 99.9}},
			{"eventId": "e-4", "observedAt": "2026-09-30T08:18:00Z", "values": {"temperature": 24.0}}
		]
	}`
	rec := doJSON(t, h, http.MethodPost, "/v1/devices/dev-1/replay", conflictBody)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d, want %d: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}

	// No sample, sequence number, or batch receipt survived the rollback.
	page := fetchHistory(t, h, "dev-1", "")
	if len(page.Samples) != 2 {
		t.Fatalf("history count = %d, want 2", len(page.Samples))
	}

	// The same batchId with identical content is accepted as all-duplicate.
	identicalBody := `{
		"batchId": "b-2",
		"samples": [
			{"eventId": "e-1", "observedAt": "2026-09-30T08:15:00Z", "values": {"temperature": 22.4, "humidity": 45}},
			{"eventId": "e-4", "observedAt": "2026-09-30T08:18:00Z", "values": {"temperature": 24.0}}
		]
	}`
	rec = doJSON(t, h, http.MethodPost, "/v1/devices/dev-1/replay", identicalBody)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("identical-content batch: %d %s", rec.Code, rec.Body.String())
	}
	var receipt ReplayReceipt
	decodeBody(t, rec, &receipt)
	if receipt.NewCount != 1 || receipt.DuplicateCount != 1 {
		t.Fatalf("counts = new %d dup %d, want 1/1", receipt.NewCount, receipt.DuplicateCount)
	}
	if receipt.Results[0].Status != "duplicate" || receipt.Results[1].Status != "new" {
		t.Fatalf("results = %+v, want duplicate then new", receipt.Results)
	}

	// Reusing b-1 with changed ordered content conflicts as well.
	changedFirst := `{
		"batchId": "b-1",
		"samples": [
			{"eventId": "e-1", "observedAt": "2026-09-30T08:15:00Z", "values": {"temperature": 22.4, "humidity": 45}},
			{"eventId": "e-2", "observedAt": "2026-09-30T08:16:00Z", "values": {"temperature": 22.7}}
		]
	}`
	rec = doJSON(t, h, http.MethodPost, "/v1/devices/dev-1/replay", changedFirst)
	if rec.Code != http.StatusConflict {
		t.Fatalf("changed b-1 status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestReplayIdempotentResubmission(t *testing.T) {
	h, _ := newTestHandler(t)
	registerDevice(t, h, "dev-1")

	first := doJSON(t, h, http.MethodPost, "/v1/devices/dev-1/replay", batchBody)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first batch: %d %s", first.Code, first.Body.String())
	}
	var firstReceipt ReplayReceipt
	decodeBody(t, first, &firstReceipt)

	rec := doJSON(t, h, http.MethodPost, "/v1/devices/dev-1/replay", batchBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("resubmit status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var secondReceipt ReplayReceipt
	decodeBody(t, rec, &secondReceipt)
	if secondReceipt.NewCount != firstReceipt.NewCount ||
		secondReceipt.DuplicateCount != firstReceipt.DuplicateCount ||
		len(secondReceipt.Results) != len(firstReceipt.Results) {
		t.Fatalf("receipt changed on resubmission: %+v vs %+v", secondReceipt, firstReceipt)
	}
	for i := range firstReceipt.Results {
		if secondReceipt.Results[i] != firstReceipt.Results[i] {
			t.Fatalf("receipt result %d changed: %+v vs %+v", i, secondReceipt.Results[i], firstReceipt.Results[i])
		}
	}

	page := fetchHistory(t, h, "dev-1", "")
	if len(page.Samples) != 2 {
		t.Fatalf("history count = %d, want 2 (no duplicates added)", len(page.Samples))
	}
}

func TestReplayValidation(t *testing.T) {
	h, _ := newTestHandler(t)
	registerDevice(t, h, "dev-1")

	validSample := `{"eventId":"e-1","observedAt":"2026-09-30T08:15:00Z","values":{"temperature":22.4}}`
	cases := map[string]string{
		"empty body":               ``,
		"not an object":            `[]`,
		"empty object":             `{}`,
		"missing batchId":          `{"samples":[` + validSample + `]}`,
		"blank batchId":            `{"batchId":"  ","samples":[` + validSample + `]}`,
		"no samples":               `{"batchId":"b-1","samples":[]}`,
		"too many samples":         `{"batchId":"b-1","samples":[` + repeatJSON(validSample, 101) + `]}`,
		"blank eventId":            `{"batchId":"b-1","samples":[{"eventId":"  ","observedAt":"2026-09-30T08:15:00Z","values":{"temperature":22.4}}]}`,
		"duplicate eventId":        `{"batchId":"b-1","samples":[` + validSample + `,` + validSample + `]}`,
		"bad observedAt":           `{"batchId":"b-1","samples":[{"eventId":"e-1","observedAt":"not-a-time","values":{"temperature":22.4}}]}`,
		"missing observedAt":       `{"batchId":"b-1","samples":[{"eventId":"e-1","values":{"temperature":22.4}}]}`,
		"empty values":             `{"batchId":"b-1","samples":[{"eventId":"e-1","observedAt":"2026-09-30T08:15:00Z","values":{}}]}`,
		"blank metric name":        `{"batchId":"b-1","samples":[{"eventId":"e-1","observedAt":"2026-09-30T08:15:00Z","values":{"  ":1}}]}`,
		"non-finite value":         `{"batchId":"b-1","samples":[{"eventId":"e-1","observedAt":"2026-09-30T08:15:00Z","values":{"temperature":"NaN"}}]}`,
		"trailing json value":      `{"batchId":"b-1","samples":[` + validSample + `]} {}`,
		"trailing garbage":         `{"batchId":"b-1","samples":[` + validSample + `]} garbage`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(t, h, http.MethodPost, "/v1/devices/dev-1/replay", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}
}

func TestReplayConcurrentBatches(t *testing.T) {
	h, _ := newTestHandler(t)
	registerDevice(t, h, "dev-1")

	const n = 20
	var wg sync.WaitGroup
	statuses := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{
				"batchId": "b-%d",
				"samples": [{"eventId": "e-%d", "observedAt": "2026-09-30T08:%02d:00Z", "values": {"v": %d}}]
			}`, i, i, i%60, i)
			rec := doJSON(t, h, http.MethodPost, "/v1/devices/dev-1/replay", body)
			statuses <- rec.Code
		}(i)
	}
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusAccepted {
			t.Fatalf("concurrent batch status = %d, want %d", status, http.StatusAccepted)
		}
	}

	page := fetchHistory(t, h, "dev-1", "")
	if len(page.Samples) != n {
		t.Fatalf("history count = %d, want %d", len(page.Samples), n)
	}
	seen := make(map[int64]bool)
	for _, sample := range page.Samples {
		if seen[sample.Seq] {
			t.Fatalf("duplicate seq %d", sample.Seq)
		}
		seen[sample.Seq] = true
	}
	if len(seen) != n {
		t.Fatalf("distinct seqs = %d, want %d", len(seen), n)
	}
}

func TestReplayConcurrentSameEventDedup(t *testing.T) {
	h, _ := newTestHandler(t)
	registerDevice(t, h, "dev-1")

	const n = 10
	var wg sync.WaitGroup
	statuses := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{
				"batchId": "b-%d",
				"samples": [{"eventId": "shared", "observedAt": "2026-09-30T08:15:00Z", "values": {"v": 1}}]
			}`, i)
			rec := doJSON(t, h, http.MethodPost, "/v1/devices/dev-1/replay", body)
			statuses <- rec.Code
		}(i)
	}
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusAccepted {
			t.Fatalf("status = %d, want %d", status, http.StatusAccepted)
		}
	}

	page := fetchHistory(t, h, "dev-1", "")
	if len(page.Samples) != 1 || page.Samples[0].Seq != 1 {
		t.Fatalf("history = %+v, want single sample at seq 1", page)
	}
}

func TestReplayConcurrentIdempotentResubmit(t *testing.T) {
	h, _ := newTestHandler(t)
	registerDevice(t, h, "dev-1")

	const n = 8
	var wg sync.WaitGroup
	statuses := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := doJSON(t, h, http.MethodPost, "/v1/devices/dev-1/replay", batchBody)
			statuses <- rec.Code
		}()
	}
	wg.Wait()
	close(statuses)
	var accepted, okStatus int
	for status := range statuses {
		switch status {
		case http.StatusAccepted:
			accepted++
		case http.StatusOK:
			okStatus++
		default:
			t.Fatalf("status = %d, want 202 or 200", status)
		}
	}
	if accepted != 1 || okStatus != n-1 {
		t.Fatalf("202 count = %d, 200 count = %d, want 1/%d", accepted, okStatus, n-1)
	}

	page := fetchHistory(t, h, "dev-1", "")
	if len(page.Samples) != 2 {
		t.Fatalf("history count = %d, want 2", len(page.Samples))
	}
}

func TestReplayDoesNotRefreshStateOnPureDuplicate(t *testing.T) {
	h, store := newTestHandler(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return base }
	registerDevice(t, h, "dev-1")

	store.now = func() time.Time { return base.Add(time.Minute) }
	rec := doJSON(t, h, http.MethodPost, "/v1/devices/dev-1/telemetry", `{"temperature":20}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("telemetry: %d %s", rec.Code, rec.Body.String())
	}

	store.now = func() time.Time { return base.Add(2 * time.Minute) }
	first := doJSON(t, h, http.MethodPost, "/v1/devices/dev-1/replay",
		`{"batchId":"b-1","samples":[{"eventId":"e-1","observedAt":"2026-01-01T00:00:00Z","values":{"temperature":22.4}}]}`)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first batch: %d %s", first.Code, first.Body.String())
	}

	var before struct {
		Devices []Device `json:"devices"`
	}
	decodeBody(t, doJSON(t, h, http.MethodGet, "/v1/fleet", ""), &before)

	store.now = func() time.Time { return base.Add(3 * time.Minute) }
	dup := doJSON(t, h, http.MethodPost, "/v1/devices/dev-1/replay",
		`{"batchId":"b-2","samples":[{"eventId":"e-1","observedAt":"2026-01-01T00:00:00Z","values":{"temperature":22.4}}]}`)
	if dup.Code != http.StatusAccepted {
		t.Fatalf("duplicate batch: %d %s", dup.Code, dup.Body.String())
	}

	var after struct {
		Devices []Device `json:"devices"`
	}
	decodeBody(t, doJSON(t, h, http.MethodGet, "/v1/fleet", ""), &after)
	if len(before.Devices) != 1 || len(after.Devices) != 1 {
		t.Fatalf("snapshot sizes = %d/%d, want 1/1", len(before.Devices), len(after.Devices))
	}
	if !after.Devices[0].LastSeenAt.Equal(before.Devices[0].LastSeenAt) {
		t.Fatalf("LastSeenAt changed on pure duplicate: %s -> %s", before.Devices[0].LastSeenAt, after.Devices[0].LastSeenAt)
	}
	if after.Devices[0].LastTelemetry["temperature"] != before.Devices[0].LastTelemetry["temperature"] {
		t.Fatalf("LastTelemetry changed on pure duplicate: %v -> %v", before.Devices[0].LastTelemetry, after.Devices[0].LastTelemetry)
	}
}

func TestReplayUpdatesSnapshotWithLastNewSample(t *testing.T) {
	h, _ := newTestHandler(t)
	registerDevice(t, h, "dev-1")

	body := `{
		"batchId": "b-1",
		"samples": [
			{"eventId": "e-1", "observedAt": "2026-09-30T08:15:00Z", "values": {"a": 1}},
			{"eventId": "e-2", "observedAt": "2026-09-30T08:16:00Z", "values": {"b": 2}},
			{"eventId": "e-3", "observedAt": "2026-09-30T08:17:00Z", "values": {"c": 3}}
		]
	}`
	rec := doJSON(t, h, http.MethodPost, "/v1/devices/dev-1/replay", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body.String())
	}
	var snapshot struct {
		Devices []Device `json:"devices"`
	}
	decodeBody(t, doJSON(t, h, http.MethodGet, "/v1/fleet", ""), &snapshot)
	if len(snapshot.Devices) != 1 {
		t.Fatalf("devices = %d, want 1", len(snapshot.Devices))
	}
	last := snapshot.Devices[0].LastTelemetry
	if len(last) != 1 || last["c"] != 3 {
		t.Fatalf("LastTelemetry = %v, want {c:3} (last new sample)", last)
	}
}

func repeatJSON(fragment string, times int) string {
	result := ""
	for i := 0; i < times; i++ {
		if i > 0 {
			result += ","
		}
		result += fragment
	}
	return result
}
