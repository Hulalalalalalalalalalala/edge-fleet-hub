package fleet

import (
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// epochZero is the valid RFC3339 instant 0001-01-01T00:00:00Z, which is also
// Go's zero time.Time and therefore used to be mistaken for a missing field.
var epochZero = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)

// A batch carrying the year-1 instant is accepted, survives a reopen with the
// same sample, value, observed time and receive sequence, and later samples
// continue the sequence. Resending the batch still replays the first receipt
// and changing its time or values still conflicts.
func TestPersistentReplayEpochZeroSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b-zero", sample("e1", "0001-01-01T00:00:00Z", `{"v":1}`)))
	if r.Code != http.StatusAccepted {
		t.Fatalf("year-1 batch = %d, want 202: %s", r.Code, r.Body.String())
	}
	receipt := decodeBody[receiptResponse](t, r)
	if receipt.NewCount != 1 || receipt.DuplicateCount != 0 ||
		len(receipt.Samples) != 1 || receipt.Samples[0].Sequence != 1 || receipt.Samples[0].Duplicate {
		t.Fatalf("year-1 receipt = %+v", receipt)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Data that committed under the previous code (which wrote exactly this
	// JSON timestamp but refused to read it back) must reopen directly.
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	events := allEvents(t, h2, "gw")
	if len(events) != 1 {
		t.Fatalf("recovered history = %+v", events)
	}
	if events[0].Sequence != 1 || events[0].Values["v"] != 1 || !events[0].ObservedAt.Equal(epochZero) {
		t.Fatalf("recovered sample = %+v", events[0])
	}

	// Resending the same batch returns the first receipt verbatim.
	r = doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b-zero", sample("e1", "0001-01-01T00:00:00Z", `{"v":1}`)))
	if r.Code != http.StatusOK {
		t.Fatalf("resend batch = %d, want 200: %s", r.Code, r.Body.String())
	}
	again := decodeBody[receiptResponse](t, r)
	if compact(r.Body.String()) != `{"batchId":"b-zero","newCount":1,"duplicateCount":0,"samples":[{"eventId":"e1","sequence":1,"duplicate":false}]}` {
		t.Fatalf("resend receipt changed: %+v", again)
	}
	if events := allEvents(t, h2, "gw"); len(events) != 1 {
		t.Fatalf("resend stored another sample: %+v", events)
	}

	// A different value for the same event still conflicts the whole batch.
	r = doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b-zero", sample("e1", "0001-01-01T00:00:00Z", `{"v":2}`)))
	if r.Code != http.StatusConflict {
		t.Fatalf("changed value = %d, want 409", r.Code)
	}

	// A genuinely new sample keeps using the original sequence line.
	r = doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b-next", sample("e2", "2024-01-02T10:00:00Z", `{"v":2}`)))
	if r.Code != http.StatusAccepted {
		t.Fatalf("next batch = %d: %s", r.Code, r.Body.String())
	}
	events = allEvents(t, h2, "gw")
	if len(events) != 2 || events[1].Sequence != 2 {
		t.Fatalf("sequence did not continue: %+v", events)
	}
}

// A timezone spelling of the same instant is the same sample: it is recognised
// as a duplicate and reuses the original sequence instead of being stored or
// rejected.
func TestPersistentReplayEpochZeroTimezoneEquivalence(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b-zero", sample("e1", "0001-01-01T00:00:00Z", `{"v":1}`))); r.Code != http.StatusAccepted {
		t.Fatalf("first batch = %d", r.Code)
	}

	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b-tz", sample("e1", "0001-01-01T08:00:00+08:00", `{"v":1}`)))
	if r.Code != http.StatusAccepted {
		t.Fatalf("equivalent-instant batch = %d: %s", r.Code, r.Body.String())
	}
	receipt := decodeBody[receiptResponse](t, r)
	if receipt.NewCount != 0 || receipt.DuplicateCount != 1 ||
		len(receipt.Samples) != 1 || !receipt.Samples[0].Duplicate || receipt.Samples[0].Sequence != 1 {
		t.Fatalf("equivalent-instant receipt = %+v", receipt)
	}
	if events := allEvents(t, h, "gw"); len(events) != 1 {
		t.Fatalf("equivalent instant stored a second event: %+v", events)
	}

	// The same instant in another spelling with a changed value still clashes.
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b-tz2", sample("e1", "0001-01-01T08:00:00+08:00", `{"v":9}`)))
	if r.Code != http.StatusConflict {
		t.Fatalf("changed value at equivalent instant = %d, want 409", r.Code)
	}
}

// A year-1 sample that crosses a threshold fires an alert judged in receive
// order, and the alert id, rule version, trigger sequence/value/time and the
// later recovery evidence all survive a reopen.
func TestPersistentReplayEpochZeroAlertSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "temp-high", "temperature", 30, 25)

	// The early observed time must not cause the sample to be skipped: it
	// opens the alert in receive order.
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b-trigger", sample("e1", "0001-01-01T00:00:00Z", `{"temperature":32}`))); r.Code != http.StatusAccepted {
		t.Fatalf("trigger batch = %d: %s", r.Code, r.Body.String())
	}
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].Status != alertStatusActive {
		t.Fatalf("year-1 sample did not fire an alert: %+v", alerts)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	alerts = listAlerts(t, h2, "gw", "")
	if len(alerts) != 1 {
		t.Fatalf("recovered alerts = %+v", alerts)
	}
	active := alerts[0]
	if active.ID != 1 || active.RuleVersion != 1 || active.TriggerSequence != 1 ||
		active.TriggerValue != 32 || !active.TriggerObservedAt.Equal(epochZero) ||
		active.Status != alertStatusActive {
		t.Fatalf("recovered trigger evidence = %+v", active)
	}

	// A later sample (in receive order) recovers the alert after the restart.
	if r := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b-recover", sample("e2", "2024-01-02T10:00:00Z", `{"temperature":20}`))); r.Code != http.StatusAccepted {
		t.Fatalf("recover batch = %d: %s", r.Code, r.Body.String())
	}
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}
	store3 := reopenPersistent(t, dir)
	h3 := NewHandler(store3)

	alerts = listAlerts(t, h3, "gw", "")
	if len(alerts) != 1 {
		t.Fatalf("alerts after recovery = %+v", alerts)
	}
	ended := alerts[0]
	if ended.Status != alertStatusEnded || ended.EndReason != endReasonRecovered {
		t.Fatalf("alert not recovered = %+v", ended)
	}
	if ended.RecoverSequence == nil || *ended.RecoverSequence != 2 ||
		ended.RecoverValue == nil || *ended.RecoverValue != 20 ||
		ended.RecoverObservedAt == nil ||
		!ended.RecoverObservedAt.Equal(time.Date(2024, time.January, 2, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("recovery evidence not preserved = %+v", ended)
	}
	if !ended.TriggerObservedAt.Equal(epochZero) {
		t.Fatalf("trigger observed time changed = %s", ended.TriggerObservedAt)
	}

	// No re-judgement or extra alert from recovery, and sequences continue.
	if r := doRequest(t, h3, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b-after", sample("e3", "2024-01-02T11:00:00Z", `{"temperature":40}`))); r.Code != http.StatusAccepted {
		t.Fatalf("post-recovery trigger batch = %d: %s", r.Code, r.Body.String())
	}
	all := listAlerts(t, h3, "gw", "")
	if len(all) != 2 || all[1].TriggerSequence != 3 {
		t.Fatalf("new alert after recovery = %+v", all)
	}
}

// Missing or invalid observedAt in a request is still rejected with 400 and
// leaves no sample or receipt behind.
func TestPersistentReplayEpochZeroStillRejectsBadTimestamps(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	for _, body := range []string{
		`{"batchId":"b","samples":[{"eventId":"e1","values":{"v":1}}]}`,
		`{"batchId":"b","samples":[{"eventId":"e1","observedAt":null,"values":{"v":1}}]}`,
		`{"batchId":"b","samples":[{"eventId":"e1","observedAt":"not-a-time","values":{"v":1}}]}`,
		`{"batchId":"b","samples":[{"eventId":"e1","observedAt":"","values":{"v":1}}]}`,
	} {
		r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body)
		if r.Code != http.StatusBadRequest {
			t.Fatalf("body %s -> %d, want 400: %s", body, r.Code, r.Body.String())
		}
	}
	if events := allEvents(t, h, "gw"); len(events) != 0 {
		t.Fatalf("rejected batches left events: %+v", events)
	}

	// Nothing committed: the directory reopens cleanly and still has no samples.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2 := reopenPersistent(t, dir)
	if events := allEvents(t, NewHandler(store2), "gw"); len(events) != 0 {
		t.Fatalf("rejected batches persisted: %+v", events)
	}
}

// buildRawFrame frames a hand-built JSON payload the same way marshalFrame does,
// for records that cannot be produced through the validated API.
func buildRawFrame(recType byte, payload []byte) []byte {
	frame := make([]byte, 5+len(payload)+4)
	binary.LittleEndian.PutUint32(frame[0:4], uint32(len(payload)))
	frame[4] = recType
	copy(frame[5:], payload)
	binary.LittleEndian.PutUint32(frame[5+len(payload):], crc32.Checksum(frame[:5+len(payload)], crcTable))
	return frame
}

// appendCommittedFrame appends one frame and its matching commit-journal entry,
// so recovery treats it as acknowledged data that must be validated.
func appendCommittedFrame(t *testing.T, dir string, recType byte, payload []byte) {
	t.Helper()
	frame := buildRawFrame(recType, payload)
	walPath := filepath.Join(dir, walName)
	wal, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(walPath, append(wal, frame...), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := make([]byte, commitEntry)
	binary.LittleEndian.PutUint64(entry[0:8], uint64(len(wal)+len(frame)))
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

// Locally saved data whose required observed time is genuinely null or
// malformed, or whose alert evidence lacks a trigger time, still fails startup
// with an explanatory error, and the files are left untouched.
func TestPersistentReplayCorruptTimestampsStillFailStartup(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{
			name: "null observedAt",
			payload: `{"deviceId":"gw","batchId":"bad","samples":[` +
				`{"eventId":"z1","observedAt":null,"values":{"v":1}}],` +
				`"receipt":{"batchId":"bad","newCount":1,"duplicateCount":0,` +
				`"samples":[{"eventId":"z1","sequence":1,"duplicate":false}]}}`,
		},
		{
			name: "missing observedAt",
			payload: `{"deviceId":"gw","batchId":"bad","samples":[` +
				`{"eventId":"z1","values":{"v":1}}],` +
				`"receipt":{"batchId":"bad","newCount":1,"duplicateCount":0,` +
				`"samples":[{"eventId":"z1","sequence":1,"duplicate":false}]}}`,
		},
		{
			name: "malformed observedAt",
			payload: `{"deviceId":"gw","batchId":"bad","samples":[` +
				`{"eventId":"z1","observedAt":"not-a-time","values":{"v":1}}]}`,
		},
		{
			name: "alert missing trigger time",
			payload: `{"deviceId":"gw","sequence":1,"observedAt":"2024-02-01T00:00:00Z",` +
				`"values":{"temperature":32},` +
				`"alerts":[{"id":1,"ruleId":"r1","ruleVersion":1,"metric":"temperature",` +
				`"trigger":30,"recover":25,"status":"active",` +
				`"triggerSequence":1,"triggerValue":32}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := openPersistent(t, dir)
			mustRegister(t, NewHandler(store), "gw")
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			recType := recReplay
			if tc.name == "alert missing trigger time" {
				recType = recTelemetry
			}
			beforeWal := walRaw(t, dir)
			beforeCommit := commitRaw(t, dir)
			appendCommittedFrame(t, dir, recType, []byte(tc.payload))

			_, err := NewPersistentStore(dir)
			if err == nil {
				t.Fatalf("startup succeeded with corrupt timestamp (%s)", tc.name)
			}

			// The committed files are exactly as the corruption left them.
			if got := walRaw(t, dir); string(got) != string(beforeWal)+string(buildRawFrame(recType, []byte(tc.payload))) {
				t.Fatalf("wal modified by failed startup (%s)", tc.name)
			}
			if got := commitRaw(t, dir); len(got) != len(beforeCommit)+commitEntry {
				t.Fatalf("commit journal modified unexpectedly (%s)", tc.name)
			}
		})
	}
}

// Sanity: walTime presence distinguishes the year-1 instant from a missing
// field through JSON round trips.
func TestWalTimePresence(t *testing.T) {
	raw, err := json.Marshal(newWalTime(epochZero))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `"0001-01-01T00:00:00Z"` {
		t.Fatalf("year-1 marshals as %s", raw)
	}
	var present walTime
	if err := json.Unmarshal(raw, &present); err != nil || !present.ok() || !present.Time.Equal(epochZero) {
		t.Fatalf("year-1 round trip = %+v ok=%v err=%v", present.Time, present.ok(), err)
	}
	for _, input := range []string{`null`, `""`} {
		var missing walTime
		err := json.Unmarshal([]byte(input), &missing)
		if input == "null" {
			if err != nil || missing.ok() {
				t.Fatalf("input %s: err=%v ok=%v, want absent", input, err, missing.ok())
			}
		} else if err == nil {
			t.Fatalf("input %s: expected parse error", input)
		}
	}
}
