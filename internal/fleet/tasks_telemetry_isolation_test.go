package fleet

// Regression coverage for remote diagnostic task result reporting: a
// diagnostic outcome belongs to its task only and must never be mistaken for
// device telemetry. Reporting a success (whose result reuses telemetry metric
// names) or a failure keeps every device's lastSeenAt, lastTelemetry and
// telemetry history exactly as they were, on both the reporting device and the
// other registered device. A wrong credential is 409, a successful report whose
// result is an array is 400, neither ends the task or saves anything, and a
// later live telemetry sample is accepted normally with the next contiguous
// receive sequence.

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// snapshotDevice is one device entry from GET /v1/fleet.
type snapshotDevice struct {
	ID            string             `json:"id"`
	LastSeenAt    time.Time          `json:"lastSeenAt"`
	LastTelemetry map[string]float64 `json:"lastTelemetry"`
}

func fleetSnapshot(t *testing.T, h http.Handler) map[string]snapshotDevice {
	t.Helper()
	snapshot := decodeBody[struct {
		Devices []snapshotDevice `json:"devices"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/fleet", ""))
	byID := make(map[string]snapshotDevice, len(snapshot.Devices))
	for _, device := range snapshot.Devices {
		byID[device.ID] = device
	}
	return byID
}

// historySample is one retained telemetry sample as returned by the history API.
type historySample struct {
	Sequence   int64              `json:"sequence"`
	ObservedAt time.Time          `json:"observedAt"`
	Values     map[string]float64 `json:"values"`
}

func deviceHistory(t *testing.T, h http.Handler, id string) []historySample {
	t.Helper()
	page := decodeBody[struct {
		Events []historySample `json:"events"`
	}](t, doRequest(t, h, http.MethodGet, fmt.Sprintf("/v1/devices/%s/history?limit=100", id), ""))
	return page.Events
}

func assertTelemetryUnchanged(t *testing.T, h http.Handler, want map[string]snapshotDevice, wantHistory map[string][]historySample) {
	t.Helper()
	got := fleetSnapshot(t, h)
	for id, before := range want {
		after, ok := got[id]
		if !ok {
			t.Fatalf("device %s disappeared from the fleet snapshot", id)
		}
		if !after.LastSeenAt.Equal(before.LastSeenAt) {
			t.Fatalf("device %s lastSeenAt changed: %s -> %s", id, before.LastSeenAt, after.LastSeenAt)
		}
		if len(after.LastTelemetry) != len(before.LastTelemetry) {
			t.Fatalf("device %s lastTelemetry length changed: %v -> %v", id, before.LastTelemetry, after.LastTelemetry)
		}
		for key, value := range before.LastTelemetry {
			if other, ok := after.LastTelemetry[key]; !ok || other != value {
				t.Fatalf("device %s lastTelemetry[%q] changed: %v -> %v", id, key, value, after.LastTelemetry[key])
			}
		}
		events := deviceHistory(t, h, id)
		if len(events) != len(wantHistory[id]) {
			t.Fatalf("device %s history length changed: %d -> %d (%+v)", id, len(wantHistory[id]), len(events), events)
		}
		for i, wantEvent := range wantHistory[id] {
			gotEvent := events[i]
			if gotEvent.Sequence != wantEvent.Sequence || !gotEvent.ObservedAt.Equal(wantEvent.ObservedAt) ||
				!sameValues(gotEvent.Values, wantEvent.Values) {
				t.Fatalf("device %s history sample %d changed: %+v -> %+v", id, i, wantEvent, gotEvent)
			}
		}
	}
}

// TestTaskReportsNeverTouchDeviceTelemetry walks the full scenario: two
// registered devices with distinct telemetry, a successful diagnostic report
// and a failed one on the target device, the two direct rejection cases, and
// finally a fresh live telemetry sample. Throughout, task results stay on the
// task and both devices' telemetry state stays intact.
func TestTaskReportsNeverTouchDeviceTelemetry(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "target")
	mustRegister(t, h, "other")

	// Two distinct telemetry samples, one per device. The target's carries the
	// temperature and battery metrics that the later diagnostic result will
	// deliberately reuse with different values.
	*clock = clock.Add(1 * time.Second)
	targetSeen := clock.UTC()
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/target/telemetry", `{"temperature":23.5,"battery":91}`); r.Code != http.StatusAccepted {
		t.Fatalf("target telemetry = %d: %s", r.Code, r.Body.String())
	}
	*clock = clock.Add(1 * time.Second)
	otherSeen := clock.UTC()
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/other/telemetry", `{"temperature":18.0,"battery":77,"humidity":40}`); r.Code != http.StatusAccepted {
		t.Fatalf("other telemetry = %d: %s", r.Code, r.Body.String())
	}

	wantSnapshot := map[string]snapshotDevice{
		"target": {
			ID:            "target",
			LastSeenAt:    targetSeen,
			LastTelemetry: map[string]float64{"temperature": 23.5, "battery": 91},
		},
		"other": {
			ID:            "other",
			LastSeenAt:    otherSeen,
			LastTelemetry: map[string]float64{"temperature": 18.0, "battery": 77, "humidity": 40},
		},
	}
	wantHistory := map[string][]historySample{
		"target": {{
			Sequence:   1,
			ObservedAt: targetSeen,
			Values:     map[string]float64{"temperature": 23.5, "battery": 91},
		}},
		"other": {{
			Sequence:   1,
			ObservedAt: otherSeen,
			Values:     map[string]float64{"temperature": 18.0, "battery": 77, "humidity": 40},
		}},
	}

	// Sanity-check the seeded fleet state before any task activity.
	assertTelemetryUnchanged(t, h, wantSnapshot, wantHistory)

	// --- task 1: claimed, then reported successful with telemetry-named, -----
	// differently-valued result fields (temperature 99, battery 0). ----------
	createTask(t, h, "target", taskBody("diag-success", 30), http.StatusCreated)
	claim := claimTask(t, h, "target", http.StatusOK)
	if claim.Task.ID != 1 || claim.Attempt != 1 || claim.Credential == "" {
		t.Fatalf("unexpected claim: %+v", claim)
	}

	*clock = clock.Add(5 * time.Second)
	completedAt := clock.UTC()
	successResult := `{"temperature":99,"battery":0}`
	report := reportTask(t, h, "target", claim.Task.ID,
		reportBody("diag-rc-success", claim.Credential, true, "", successResult), http.StatusCreated)
	if !report.Success || report.ReceiptID != "diag-rc-success" || !report.ReceivedAt.Equal(completedAt) {
		t.Fatalf("success report = %+v, want receivedAt %s", report, completedAt)
	}
	if string(report.Result) != successResult {
		t.Fatalf("success report result = %s, want %s", report.Result, successResult)
	}

	succeeded := getTask(t, h, "target", claim.Task.ID, http.StatusOK)
	if succeeded.Status != taskStatusSucceeded {
		t.Fatalf("task status = %q, want succeeded", succeeded.Status)
	}
	if succeeded.CompletedAt == nil || !succeeded.CompletedAt.Equal(completedAt) {
		t.Fatalf("task completedAt = %+v, want %s", succeeded.CompletedAt, completedAt)
	}
	if string(succeeded.Result) != successResult {
		t.Fatalf("saved task result = %s, want %s", succeeded.Result, successResult)
	}
	if got := joinEvents(eventsOf(listAudit(t, h, "target", claim.Task.ID))); got != "created,claimed,succeeded" {
		t.Fatalf("success task audit = %s, want created,claimed,succeeded", got)
	}

	// The success result is diagnostic data, not telemetry: the target's
	// lastSeenAt/lastTelemetry and its single history sample are unchanged
	// (temperature stays 23.5, battery 91), and the other device is untouched.
	assertTelemetryUnchanged(t, h, wantSnapshot, wantHistory)

	// --- task 2: first execution, reported failed with a non-empty reason ----
	createTask(t, h, "target", taskBody("diag-failure", 30), http.StatusCreated)
	claim2 := claimTask(t, h, "target", http.StatusOK)
	if claim2.Task.ID != 2 || claim2.Attempt != 1 {
		t.Fatalf("second claim = %+v, want task 2 attempt 1", claim2)
	}

	*clock = clock.Add(5 * time.Second)
	failureAt := clock.UTC()
	failed := reportTask(t, h, "target", claim2.Task.ID,
		reportBody("diag-rc-failure", claim2.Credential, false, "sensor unreachable", ""), http.StatusCreated)
	if failed.Success || failed.Reason != "sensor unreachable" || !failed.ReceivedAt.Equal(failureAt) {
		t.Fatalf("failure report = %+v, want reason %q receivedAt %s", failed, "sensor unreachable", failureAt)
	}

	waiting := getTask(t, h, "target", claim2.Task.ID, http.StatusOK)
	if waiting.Status != taskStatusWaiting {
		t.Fatalf("task status = %q, want waiting", waiting.Status)
	}
	if waiting.FailureReason != "sensor unreachable" {
		t.Fatalf("failure reason = %q, want %q", waiting.FailureReason, "sensor unreachable")
	}
	if waiting.CompletedAt != nil {
		t.Fatalf("waiting task carries completedAt: %s", waiting.CompletedAt)
	}
	if got := joinEvents(eventsOf(listAudit(t, h, "target", claim2.Task.ID))); got != "created,claimed,failed" {
		t.Fatalf("failure task audit = %s, want created,claimed,failed", got)
	}

	// The reported failure neither refreshes lastSeenAt, overwrites
	// lastTelemetry nor appends a telemetry sample, on either device.
	assertTelemetryUnchanged(t, h, wantSnapshot, wantHistory)

	// --- task 3: the two direct rejections, both within the deadline with ----
	// receipt numbers that have never been accepted. -------------------------
	createTask(t, h, "target", taskBody("diag-reject", 30), http.StatusCreated)
	claim3 := claimTask(t, h, "target", http.StatusOK)
	if claim3.Task.ID != 3 || claim3.Attempt != 1 {
		t.Fatalf("third claim = %+v, want task 3 attempt 1", claim3)
	}

	// Wrong credential with a fresh receipt number -> 409.
	reportTask(t, h, "target", claim3.Task.ID,
		reportBody("diag-rc-wrong-cred", "not-the-current-credential", true, "", `{"ok":true}`), http.StatusConflict)
	// Current credential but an array result on a success -> 400.
	reportTask(t, h, "target", claim3.Task.ID,
		reportBody("diag-rc-array-result", claim3.Credential, true, "", `[{"temperature":99}]`), http.StatusBadRequest)

	// Neither rejection ends the task, saves a result, or advances the audit
	// trail beyond the claim.
	rejected := getTask(t, h, "target", claim3.Task.ID, http.StatusOK)
	if rejected.Status != taskStatusInProgress {
		t.Fatalf("rejected task status = %q, want in_progress", rejected.Status)
	}
	if rejected.Attempts != 1 || rejected.CompletedAt != nil || len(rejected.Result) != 0 {
		t.Fatalf("rejections changed the task: %+v", rejected)
	}
	if rejected.Deadline == nil || !rejected.Deadline.Equal(claim3.Deadline) {
		t.Fatalf("rejected task deadline = %+v, want %s", rejected.Deadline, claim3.Deadline)
	}
	if got := joinEvents(eventsOf(listAudit(t, h, "target", claim3.Task.ID))); got != "created,claimed" {
		t.Fatalf("rejected task audit = %s, want created,claimed", got)
	}
	// No receipt was saved and neither number was bound by the rejections.
	ts3 := store.devices["target"].tasks[claim3.Task.ID-1]
	if len(ts3.reports) != 0 {
		t.Fatalf("rejections saved reports: %+v", ts3.reports)
	}
	for _, receiptID := range []string{"diag-rc-wrong-cred", "diag-rc-array-result"} {
		if _, bound := store.devices["target"].taskReceipts[receiptID]; bound {
			t.Fatalf("rejection bound receipt number %q", receiptID)
		}
	}

	// Existing telemetry state is still exactly as seeded.
	assertTelemetryUnchanged(t, h, wantSnapshot, wantHistory)

	// --- a later live telemetry sample lands normally on the target ---------
	*clock = clock.Add(10 * time.Second)
	newSeen := clock.UTC()
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/target/telemetry", `{"temperature":24.0,"battery":88}`); r.Code != http.StatusAccepted {
		t.Fatalf("later telemetry = %d, want 202: %s", r.Code, r.Body.String())
	}

	snapshot := fleetSnapshot(t, h)
	targetAfter := snapshot["target"]
	if !targetAfter.LastSeenAt.Equal(newSeen) {
		t.Fatalf("target lastSeenAt after live telemetry = %s, want %s", targetAfter.LastSeenAt, newSeen)
	}
	if targetAfter.LastTelemetry["temperature"] != 24.0 || targetAfter.LastTelemetry["battery"] != 88 {
		t.Fatalf("target lastTelemetry after live telemetry = %v, want temperature 24, battery 88", targetAfter.LastTelemetry)
	}

	// History gains exactly the next contiguous sample (sequence 2); the
	// diagnostic results never entered it.
	events := deviceHistory(t, h, "target")
	if len(events) != 2 {
		t.Fatalf("target history after live telemetry has %d samples, want 2: %+v", len(events), events)
	}
	if events[0].Sequence != 1 || events[1].Sequence != 2 {
		t.Fatalf("target history sequences = %d,%d, want 1,2", events[0].Sequence, events[1].Sequence)
	}
	if !events[1].ObservedAt.Equal(newSeen) || !sameValues(events[1].Values, map[string]float64{"temperature": 24.0, "battery": 88}) {
		t.Fatalf("new target history sample = %+v", events[1])
	}
	// The original sample is intact.
	if !events[0].ObservedAt.Equal(targetSeen) || !sameValues(events[0].Values, wantHistory["target"][0].Values) {
		t.Fatalf("original target history sample changed: %+v", events[0])
	}

	// The other device keeps its own lastSeenAt, lastTelemetry and single
	// history sample throughout; nothing on the target crossed over to it.
	otherAfter := snapshot["other"]
	if !otherAfter.LastSeenAt.Equal(otherSeen) {
		t.Fatalf("other device lastSeenAt changed: %s -> %s", otherSeen, otherAfter.LastSeenAt)
	}
	if !sameValues(otherAfter.LastTelemetry, wantSnapshot["other"].LastTelemetry) {
		t.Fatalf("other device lastTelemetry changed: %v -> %v", wantSnapshot["other"].LastTelemetry, otherAfter.LastTelemetry)
	}
	otherEvents := deviceHistory(t, h, "other")
	if len(otherEvents) != 1 || otherEvents[0].Sequence != 1 ||
		!otherEvents[0].ObservedAt.Equal(otherSeen) || !sameValues(otherEvents[0].Values, wantHistory["other"][0].Values) {
		t.Fatalf("other device history changed: %+v", otherEvents)
	}
}
