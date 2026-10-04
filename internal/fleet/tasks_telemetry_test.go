package fleet

import (
	"net/http"
	"testing"
	"time"
)

// fleetDevices returns the device list keyed by device id.
func fleetDevices(t *testing.T, h http.Handler) map[string]Device {
	t.Helper()
	r := doRequest(t, h, http.MethodGet, "/v1/fleet", "")
	if r.Code != http.StatusOK {
		t.Fatalf("fleet list status = %d: %s", r.Code, r.Body.String())
	}
	snapshot := decodeBody[struct {
		Devices []Device `json:"devices"`
	}](t, r)
	devices := make(map[string]Device, len(snapshot.Devices))
	for _, device := range snapshot.Devices {
		devices[device.ID] = device
	}
	return devices
}

// Diagnostic results are task data, not telemetry: a reported success or
// failure must never refresh a device's lastSeenAt, overwrite its
// lastTelemetry or append to its history, even when the diagnostic payload
// reuses the telemetry's metric names with different values. Rejected reports
// (wrong credential, non-object result) change nothing either, and a later
// live telemetry sample still lands normally with the next history sequence.
func TestTaskReportDoesNotTouchTelemetry(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "dev-1")
	mustRegister(t, h, "dev-2")

	// Seed each device with its own telemetry at its own receive time.
	*clock = clock.Add(time.Minute)
	dev1SeenAt := clock.UTC()
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/dev-1/telemetry", `{"temperature":21.5,"battery":87}`); r.Code != http.StatusAccepted {
		t.Fatalf("dev-1 telemetry status = %d: %s", r.Code, r.Body.String())
	}
	*clock = clock.Add(time.Minute)
	dev2SeenAt := clock.UTC()
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/dev-2/telemetry", `{"humidity":40}`); r.Code != http.StatusAccepted {
		t.Fatalf("dev-2 telemetry status = %d: %s", r.Code, r.Body.String())
	}

	// requireTelemetryState pins the pre-task state of both devices: the list
	// view keeps the original last-seen times and last telemetry, and dev-1's
	// history holds exactly its one original sample.
	requireTelemetryState := func() {
		t.Helper()
		devices := fleetDevices(t, h)
		dev1, ok := devices["dev-1"]
		if !ok {
			t.Fatalf("dev-1 missing from fleet list: %+v", devices)
		}
		if !dev1.LastSeenAt.Equal(dev1SeenAt) {
			t.Fatalf("dev-1 lastSeenAt = %s, want %s", dev1.LastSeenAt, dev1SeenAt)
		}
		if len(dev1.LastTelemetry) != 2 || dev1.LastTelemetry["temperature"] != 21.5 || dev1.LastTelemetry["battery"] != 87 {
			t.Fatalf("dev-1 lastTelemetry = %+v, want temperature 21.5 and battery 87", dev1.LastTelemetry)
		}
		dev2, ok := devices["dev-2"]
		if !ok {
			t.Fatalf("dev-2 missing from fleet list: %+v", devices)
		}
		if !dev2.LastSeenAt.Equal(dev2SeenAt) {
			t.Fatalf("dev-2 lastSeenAt = %s, want %s", dev2.LastSeenAt, dev2SeenAt)
		}
		if len(dev2.LastTelemetry) != 1 || dev2.LastTelemetry["humidity"] != 40 {
			t.Fatalf("dev-2 lastTelemetry = %+v, want humidity 40", dev2.LastTelemetry)
		}
		events := allEvents(t, h, "dev-1")
		if len(events) != 1 {
			t.Fatalf("dev-1 history length = %d, want 1: %+v", len(events), events)
		}
		if events[0].Sequence != 1 || !events[0].ObservedAt.Equal(dev1SeenAt) ||
			events[0].Values["temperature"] != 21.5 || events[0].Values["battery"] != 87 {
			t.Fatalf("dev-1 history sample rewritten: %+v", events[0])
		}
	}
	requireTelemetryState()

	// A success report whose result reuses the telemetry metric names with
	// different values is accepted within the deadline and completes the task
	// with the submitted result and the report time as its completion time.
	*clock = clock.Add(time.Minute)
	reportAt := clock.UTC()
	createTask(t, h, "dev-1", taskBody("req-success", 30), http.StatusCreated)
	claim := claimTask(t, h, "dev-1", http.StatusOK)
	report := reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-success", claim.Credential, true, "", `{"temperature":99,"battery":0}`), http.StatusCreated)
	if !report.Success || report.ReceiptID != "rcpt-success" || !report.ReceivedAt.Equal(reportAt) {
		t.Fatalf("success report = %+v, want receipt rcpt-success received at %s", report, reportAt)
	}
	task := getTask(t, h, "dev-1", claim.Task.ID, http.StatusOK)
	if task.Status != "succeeded" || task.CompletedAt == nil || !task.CompletedAt.Equal(reportAt) {
		t.Fatalf("task after success = %+v, want succeeded completed at %s", task, reportAt)
	}
	if string(task.Result) != `{"temperature":99,"battery":0}` {
		t.Fatalf("stored result = %s, want the submitted diagnostic object", task.Result)
	}
	// The diagnostic result must not leak into telemetry state.
	requireTelemetryState()

	// A first-attempt failure with a non-blank reason is accepted as well and
	// only moves the task to waiting with the reported reason readable.
	*clock = clock.Add(time.Minute)
	failAt := clock.UTC()
	createTask(t, h, "dev-1", taskBody("req-failure", 30), http.StatusCreated)
	claim = claimTask(t, h, "dev-1", http.StatusOK)
	if claim.Task.ID != 2 {
		t.Fatalf("second claim took task %d, want 2", claim.Task.ID)
	}
	report = reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-failure", claim.Credential, false, "sensor offline", ""), http.StatusCreated)
	if report.Success || report.Reason != "sensor offline" || !report.ReceivedAt.Equal(failAt) {
		t.Fatalf("failure report = %+v, want reason received at %s", report, failAt)
	}
	task = getTask(t, h, "dev-1", claim.Task.ID, http.StatusOK)
	if task.Status != "waiting" || task.FailureReason != "sensor offline" {
		t.Fatalf("task after failure = %+v, want waiting with reason", task)
	}
	requireTelemetryState()

	// Rejected reports change nothing: a fresh receiptId with a wrong
	// credential conflicts, and a success whose result is an array instead of
	// an object is a 400. Neither ends the task, stores a result or touches
	// telemetry. Task 2 is still inside its backoff, so the claim takes task 3.
	createTask(t, h, "dev-1", taskBody("req-reject", 30), http.StatusCreated)
	claim = claimTask(t, h, "dev-1", http.StatusOK)
	if claim.Task.ID != 3 {
		t.Fatalf("third claim took task %d, want 3", claim.Task.ID)
	}
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-wrong-cred", "not-the-credential", true, "", `{"temperature":1}`), http.StatusConflict)
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-array", claim.Credential, true, "", `[99,0]`), http.StatusBadRequest)
	task = getTask(t, h, "dev-1", claim.Task.ID, http.StatusOK)
	if task.Status != "in_progress" || task.Result != nil || task.CompletedAt != nil {
		t.Fatalf("rejected reports changed the task: %+v", task)
	}
	requireTelemetryState()

	// A later live telemetry sample still lands normally: 202, the list shows
	// the new values and receive time, and history gains the next sequence
	// while the other device keeps its original state.
	*clock = clock.Add(time.Minute)
	liveAt := clock.UTC()
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/dev-1/telemetry", `{"temperature":22.5,"battery":86}`); r.Code != http.StatusAccepted {
		t.Fatalf("live telemetry status = %d, want 202: %s", r.Code, r.Body.String())
	}
	devices := fleetDevices(t, h)
	dev1 := devices["dev-1"]
	if !dev1.LastSeenAt.Equal(liveAt) {
		t.Fatalf("dev-1 lastSeenAt = %s, want live receive time %s", dev1.LastSeenAt, liveAt)
	}
	if len(dev1.LastTelemetry) != 2 || dev1.LastTelemetry["temperature"] != 22.5 || dev1.LastTelemetry["battery"] != 86 {
		t.Fatalf("dev-1 lastTelemetry = %+v, want the live sample", dev1.LastTelemetry)
	}
	dev2 := devices["dev-2"]
	if !dev2.LastSeenAt.Equal(dev2SeenAt) || len(dev2.LastTelemetry) != 1 || dev2.LastTelemetry["humidity"] != 40 {
		t.Fatalf("dev-2 changed by dev-1 telemetry: %+v", dev2)
	}
	events := allEvents(t, h, "dev-1")
	if len(events) != 2 {
		t.Fatalf("dev-1 history length = %d, want 2: %+v", len(events), events)
	}
	if events[0].Sequence != 1 || !events[0].ObservedAt.Equal(dev1SeenAt) ||
		events[0].Values["temperature"] != 21.5 || events[0].Values["battery"] != 87 {
		t.Fatalf("first sample rewritten: %+v", events[0])
	}
	if events[1].Sequence != 2 || !events[1].ObservedAt.Equal(liveAt) ||
		events[1].Values["temperature"] != 22.5 || events[1].Values["battery"] != 86 {
		t.Fatalf("live sample = %+v, want sequence 2 observed at %s", events[1], liveAt)
	}
}
