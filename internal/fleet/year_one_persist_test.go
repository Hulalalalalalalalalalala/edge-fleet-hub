package fleet

import (
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// yearOneInstant is 0001-01-01T00:00:00Z, which is also Go's zero time.Time.
// It is a valid RFC3339 instant and must be stored and restored like any other.
var yearOneInstant = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)

// A replayed sample captured at 0001-01-01T00:00:00Z is accepted (202) and the
// sample, its values, its sequence and the captured time all survive a restart
// of the same data directory. Equivalent timezone spellings are the same
// instant, sequences continue afterwards, and batch/event dedup is unchanged.
func TestReplayYearOneObservedAtRestoresAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b-year-one",
			sample("e1", "0001-01-01T00:00:00Z", `{"v":1}`),
			sample("e2", "2024-01-02T10:00:00Z", `{"v":2}`),
		))
	if r.Code != http.StatusAccepted {
		t.Fatalf("year-one batch = %d, want 202: %s", r.Code, r.Body.String())
	}
	receipt := decodeBody[receiptResponse](t, r)
	if receipt.NewCount != 2 || receipt.DuplicateCount != 0 ||
		receipt.Samples[0].Sequence != 1 || receipt.Samples[1].Sequence != 2 ||
		receipt.Samples[0].Duplicate || receipt.Samples[1].Duplicate {
		t.Fatalf("year-one receipt = %+v", receipt.Samples)
	}

	events := allEvents(t, h, "gw")
	if len(events) != 2 || events[0].Sequence != 1 || !events[0].ObservedAt.Equal(yearOneInstant) ||
		events[0].Values["v"] != 1 {
		t.Fatalf("year-one sample not stored as accepted: %+v", events)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening the previously saved directory must succeed and keep the sample.
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	events = allEvents(t, h2, "gw")
	if len(events) != 2 || events[0].Sequence != 1 || !events[0].ObservedAt.Equal(yearOneInstant) ||
		events[0].Values["v"] != 1 || events[1].Sequence != 2 {
		t.Fatalf("year-one sample lost or altered across restart: %+v", events)
	}

	// A different timezone spelling of the same instant is the same sample.
	r = doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b-dup", sample("e1", "0001-01-01T08:00:00+08:00", `{"v":1}`)))
	if r.Code != http.StatusAccepted {
		t.Fatalf("offset-equivalent duplicate = %d: %s", r.Code, r.Body.String())
	}
	dup := decodeBody[receiptResponse](t, r)
	if dup.NewCount != 0 || dup.DuplicateCount != 1 ||
		dup.Samples[0].Sequence != 1 || !dup.Samples[0].Duplicate {
		t.Fatalf("equivalent-instant replay not a duplicate: %+v", dup.Samples)
	}

	// Same eventId, same instant, changed value still conflicts the batch.
	r = doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b-conflict-value", sample("e1", "0001-01-01T00:00:00Z", `{"v":9}`)))
	if r.Code != http.StatusConflict {
		t.Fatalf("changed value at year-one = %d, want 409", r.Code)
	}
	// Same eventId, changed time still conflicts.
	r = doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b-conflict-time", sample("e1", "0001-01-02T00:00:00Z", `{"v":1}`)))
	if r.Code != http.StatusConflict {
		t.Fatalf("changed time for event = %d, want 409", r.Code)
	}

	// Re-submitting the original batch returns the first receipt (200).
	r = doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b-year-one",
			sample("e1", "0001-01-01T00:00:00Z", `{"v":1}`),
			sample("e2", "2024-01-02T10:00:00Z", `{"v":2}`),
		))
	if r.Code != http.StatusOK {
		t.Fatalf("repeat original batch = %d, want 200: %s", r.Code, r.Body.String())
	}
	repeated := decodeBody[receiptResponse](t, r)
	if repeated.NewCount != 2 || repeated.Samples[0].Sequence != 1 || repeated.Samples[1].Sequence != 2 {
		t.Fatalf("first receipt not preserved: %+v", repeated.Samples)
	}

	// New samples continue the original sequence after the duplicate replays.
	r = doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b-next", sample("e3", "2024-01-03T10:00:00Z", `{"v":3}`)))
	if r.Code != http.StatusAccepted {
		t.Fatalf("next batch = %d: %s", r.Code, r.Body.String())
	}
	next := decodeBody[receiptResponse](t, r)
	if next.Samples[0].Sequence != 3 {
		t.Fatalf("sequence did not continue at 3: %+v", next.Samples)
	}
}

// The year-one instant works as an inclusive history bound and the resulting
// continuation cursor stays valid across a restart.
func TestReplayYearOneHistoryBoundAndCursor(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b",
			sample("e1", "0001-01-01T00:00:00Z", `{"v":1}`),
			sample("e2", "2024-01-02T10:00:00Z", `{"v":2}`),
		))

	// to = year-one must be a real bound that keeps the year-one event and
	// excludes the later one, not an ignored ("open") bound.
	page := decodeBody[historyResponse](t, doRequest(t, h, http.MethodGet,
		"/v1/devices/gw/history?to=0001-01-01T00:00:00Z&limit=10", ""))
	if len(page.Events) != 1 || page.Events[0].Sequence != 1 || !page.Events[0].ObservedAt.Equal(yearOneInstant) {
		t.Fatalf("to=year-one bound = %+v, want only the year-one event", page.Events)
	}
	// from = year-one keeps both events.
	page = decodeBody[historyResponse](t, doRequest(t, h, http.MethodGet,
		"/v1/devices/gw/history?from=0001-01-01T00:00:00Z&limit=1", ""))
	if len(page.Events) != 1 || page.NextCursor == nil {
		t.Fatalf("first page with from=year-one = %+v", page)
	}
	cursor := *page.NextCursor

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	r := doRequest(t, h2, http.MethodGet,
		"/v1/devices/gw/history?from=0001-01-01T00:00:00Z&limit=1&cursor="+cursor, "")
	if r.Code != http.StatusOK {
		t.Fatalf("year-one-bound cursor after restart = %d: %s", r.Code, r.Body.String())
	}
}

// A year-one trigger (and a one-second-later recovery) produces a normal
// alert, and its id, rule version, trigger sequence/value/time and recovery
// evidence all survive reopening the data directory.
func TestReplayYearOneAlertEvidenceRestores(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)

	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b",
			sample("e1", "0001-01-01T00:00:00Z", `{"temperature":40}`),
			sample("e2", "0001-01-01T00:00:01Z", `{"temperature":20}`),
		))
	if r.Code != http.StatusAccepted {
		t.Fatalf("batch = %d: %s", r.Code, r.Body.String())
	}

	check := func(t *testing.T, alerts []alertResponse) {
		t.Helper()
		if len(alerts) != 1 {
			t.Fatalf("alerts = %+v, want 1", alerts)
		}
		a := alerts[0]
		recoverAt := time.Date(1, time.January, 1, 0, 0, 1, 0, time.UTC)
		if a.ID != 1 || a.RuleID != "r1" || a.RuleVersion != 1 ||
			a.Status != alertStatusEnded || a.EndReason != endReasonRecovered ||
			a.TriggerSequence != 1 || a.TriggerValue != 40 ||
			!a.TriggerObservedAt.Equal(yearOneInstant) ||
			a.RecoverSequence == nil || *a.RecoverSequence != 2 ||
			a.RecoverValue == nil || *a.RecoverValue != 20 ||
			a.RecoverObservedAt == nil || !a.RecoverObservedAt.Equal(recoverAt) {
			t.Fatalf("alert evidence wrong: %+v", a)
		}
	}
	check(t, listAlerts(t, h, "gw", ""))

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	check(t, listAlerts(t, h2, "gw", ""))

	// Rules still judge later samples in receive order after recovery: a fresh
	// crossing opens a new alert with the continuing sequence (3).
	r = doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b2", sample("e3", "2024-01-02T10:00:00Z", `{"temperature":36}`)))
	if r.Code != http.StatusAccepted {
		t.Fatalf("later trigger batch = %d: %s", r.Code, r.Body.String())
	}
	active := listAlerts(t, h2, "gw", "status=active")
	if len(active) != 1 || active[0].ID != 2 || active[0].TriggerSequence != 3 ||
		!active[0].TriggerObservedAt.Equal(time.Date(2024, time.January, 2, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("later alert = %+v", active)
	}
}

// Missing, null, blank or malformed observedAt is a 400 validation error that
// leaves no samples and no receipt.
func TestReplayRejectsMissingOrInvalidObservedAt(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	for _, bad := range []string{
		`{"eventId":"e1","values":{"v":1}}`,
		`{"eventId":"e1","observedAt":null,"values":{"v":1}}`,
		`{"eventId":"e1","observedAt":"","values":{"v":1}}`,
		`{"eventId":"e1","observedAt":"not-a-time","values":{"v":1}}`,
	} {
		r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
			fmt.Sprintf(`{"batchId":"b","samples":[%s]}`, bad))
		if r.Code != http.StatusBadRequest {
			t.Fatalf("bad sample %s -> %d, want 400: %s", bad, r.Code, r.Body.String())
		}
	}
	if events := allEvents(t, h, "gw"); len(events) != 0 {
		t.Fatalf("rejected batches left samples: %+v", events)
	}
	// The rejected batchId left no receipt: a valid submission is a fresh 202.
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b", sample("e1", "0001-01-01T00:00:00Z", `{"v":1}`)))
	if r.Code != http.StatusAccepted {
		t.Fatalf("valid batch after rejections = %d: %s", r.Code, r.Body.String())
	}
}

// appendCommittedFrame writes one frame to the WAL and marks it committed in
// the journal, simulating data that an older server build had acknowledged.
func appendCommittedFrame(t *testing.T, dir string, recType byte, value any) {
	t.Helper()
	frame, err := marshalFrame(recType, value)
	if err != nil {
		t.Fatal(err)
	}
	walPath := filepath.Join(dir, walName)
	walBytes, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatal(err)
	}
	newLen := int64(len(walBytes) + len(frame))
	if err := os.WriteFile(walPath, append(walBytes, frame...), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := make([]byte, commitEntry)
	binary.LittleEndian.PutUint64(entry[0:8], uint64(newLen))
	binary.LittleEndian.PutUint32(entry[8:12], commitCRC(entry[0:8]))
	commitPath := filepath.Join(dir, commitName)
	journal, err := os.ReadFile(commitPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(commitPath, append(journal, entry...), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Data committed by an earlier build with the year-one instant serialised
// explicitly reopens directly: the on-disk string "0001-01-01T00:00:00Z" is a
// present time, not a missing one.
func TestPersistentYearOneCommittedByOlderBuildReopens(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	appendCommittedFrame(t, dir, recReplay, walReplay{
		DeviceID: "gw",
		BatchID:  "old-batch",
		Samples: []walSample{
			{EventID: "e1", ObservedAt: utcTimePtr(yearOneInstant), Values: map[string]float64{"v": 1}},
		},
		Receipt: ReplayReceipt{
			BatchID:      "old-batch",
			NewCount:     1,
			SampleStatus: []SampleStatus{{EventID: "e1", Sequence: 1}},
		},
		LastSeenAt:    utcTimePtr(time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)),
		LastTelemetry: map[string]float64{"v": 1},
	})

	reopened := reopenPersistent(t, dir)
	events, _, _, _, err := reopened.History("gw", HistoryFilter{}, 0, 0, 100)
	if err != nil {
		t.Fatalf("reopen old year-one data failed: %v", err)
	}
	if len(events) != 1 || events[0].Sequence != 1 || !events[0].ObservedAt.Equal(yearOneInstant) {
		t.Fatalf("recovered old year-one event = %+v", events)
	}
}

// A genuinely missing/null required time in committed data is still corrupt and
// fails startup, leaving the files in place.
func TestPersistentMissingTimeFailsStartup(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	appendCommittedFrame(t, dir, recReplay, walReplay{
		DeviceID: "gw",
		BatchID:  "bad-batch",
		Samples: []walSample{
			{EventID: "e1", ObservedAt: nil, Values: map[string]float64{"v": 1}},
		},
		Receipt: ReplayReceipt{
			BatchID:      "bad-batch",
			NewCount:     1,
			SampleStatus: []SampleStatus{{EventID: "e1", Sequence: 1}},
		},
		LastSeenAt:    utcTimePtr(time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)),
		LastTelemetry: map[string]float64{"v": 1},
	})

	if _, err := NewPersistentStore(dir); err == nil {
		t.Fatalf("startup succeeded with a null observedAt")
	}

	// An alert whose trigger evidence lost its captured time is also rejected.
	dir2 := t.TempDir()
	store2 := openPersistent(t, dir2)
	h2 := NewHandler(store2)
	mustRegister(t, h2, "gw")
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}
	appendCommittedFrame(t, dir2, recTelemetry, walTelemetry{
		DeviceID:   "gw",
		Sequence:   1,
		ObservedAt: time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC),
		Values:     map[string]float64{"v": 1},
		Alerts: []*walAlert{{
			ID: 1, RuleID: "r1", Metric: "v", Status: alertStatusActive,
			TriggerSequence: 1, TriggerValue: 1, // TriggerObservedAt intentionally nil
		}},
	})
	if _, err := NewPersistentStore(dir2); err == nil {
		t.Fatalf("startup succeeded with an alert missing triggerObservedAt")
	}
}
