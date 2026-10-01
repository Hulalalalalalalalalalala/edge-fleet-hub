package fleet

import (
	"net/http"
	"testing"
	"time"
)

// --- rules, alerts and acknowledgements survive a restart ---------------------

func TestPersistentAlertsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	mustCreateRule(t, h, "gw", ruleBody("temp", "temperature", 30, 25))
	mustCreateRule(t, h, "gw", ruleBody("hum", "humidity", 80, 60))
	// Bump temp to version 2 so version continuity is checked across restart.
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/temp", updateBody("temperature", 30, 25, 1)); r.Code != http.StatusOK {
		t.Fatalf("update = %d", r.Code)
	}

	// Alert 1: recovered naturally. Alert 2: open and acknowledged.
	sendTelemetry(t, h, "gw", `{"temperature":35}`)
	sendTelemetry(t, h, "gw", `{"temperature":10}`)
	sendTelemetry(t, h, "gw", `{"humidity":90}`)
	ackTime := store.now().Add(time.Minute)
	store.now = func() time.Time { return ackTime }
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/alerts/2/ack", ""); r.Code != http.StatusOK {
		t.Fatalf("ack = %d", r.Code)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	// Rules keep their versions and enabled state.
	rules := decodeBody[struct {
		Rules []Rule `json:"rules"`
	}](t, doRequest(t, h2, http.MethodGet, "/v1/devices/gw/rules", "")).Rules
	if len(rules) != 2 || rules[0].RuleID != "hum" || rules[1].RuleID != "temp" {
		t.Fatalf("rules = %+v", rules)
	}
	if rules[1].Version != 2 || !rules[1].Enabled || rules[0].Version != 1 {
		t.Fatalf("rule versions not preserved: %+v", rules)
	}
	// Updates continue from the recovered version.
	if r := doRequest(t, h2, http.MethodPut, "/v1/devices/gw/rules/temp", updateBody("temperature", 30, 25, 1)); r.Code != http.StatusConflict {
		t.Fatalf("stale version after restart = %d, want 409", r.Code)
	}
	if r := doRequest(t, h2, http.MethodPut, "/v1/devices/gw/rules/temp", updateBody("temperature", 33, 22, 2)); r.Code != http.StatusOK {
		t.Fatalf("update after restart = %d: %s", r.Code, r.Body.String())
	}

	// All alerts, their closures and the acknowledgement time are intact.
	alerts := listAlerts(t, h2, "gw", "")
	if len(alerts) != 2 {
		t.Fatalf("alerts = %+v, want 2", alerts)
	}
	if !alerts[0].Closed || alerts[0].CloseReason != CloseReasonRecovered ||
		alerts[0].RecoveredBy == nil || alerts[0].RecoveredBy.Sequence != 2 {
		t.Fatalf("recovered alert = %+v", alerts[0])
	}
	if alerts[1].Closed || alerts[1].AcknowledgedAt == nil || !alerts[1].AcknowledgedAt.Equal(ackTime.UTC()) {
		t.Fatalf("acked alert = %+v, want open with ack time %s", alerts[1], ackTime)
	}

	// Recovery must not re-trigger: history alone produces no new alerts, and
	// the still-open alert 2 recovers from the next low sample instead of a
	// duplicate alert appearing.
	if alerts := listAlerts(t, h2, "gw", "closed=false"); len(alerts) != 1 || alerts[0].AlertID != 2 {
		t.Fatalf("open alerts after restart = %+v", alerts)
	}
	sendTelemetry(t, h2, "gw", `{"humidity":10}`)
	alerts = listAlerts(t, h2, "gw", "")
	if len(alerts) != 2 || !alerts[1].Closed || alerts[1].CloseReason != CloseReasonRecovered {
		t.Fatalf("alerts after post-restart recovery = %+v", alerts)
	}

	// A new trigger gets the next id, not a reused one.
	sendTelemetry(t, h2, "gw", `{"temperature":99}`)
	alerts = listAlerts(t, h2, "gw", "")
	if len(alerts) != 3 || alerts[2].AlertID != 3 || alerts[2].RuleVersion != 3 {
		t.Fatalf("alerts = %+v, want new alert id 3 pinned to rule version 3", alerts)
	}
}

// --- durable failures on rule and ack writes ----------------------------------

func TestPersistentRuleAndAckWriteFailureReturns503(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	mustCreateRule(t, h, "gw", ruleBody("r", "v", 10, 5))
	sendTelemetry(t, h, "gw", `{"v":50}`) // opens alert 1

	real := store.wal.(*walFile)
	failing := &failingWAL{inner: real}
	store.wal = failing

	// Rule update fails at the commit point: 503, version and alert unchanged.
	r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r", updateBody("v", 20, 8, 1))
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing rule update = %d, want 503: %s", r.Code, r.Body.String())
	}
	rule := decodeBody[Rule](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/rules/r", ""))
	if rule.Version != 1 || rule.Trigger != 10 {
		t.Fatalf("rule changed despite 503: %+v", rule)
	}
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 1 || alerts[0].Closed {
		t.Fatalf("alert changed despite 503: %+v", alerts)
	}

	// Ack fails too: 503 and the alert stays unacknowledged.
	failing.didFail = false
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/alerts/1/ack", "")
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing ack = %d, want 503", r.Code)
	}
	if alerts := listAlerts(t, h, "gw", ""); alerts[0].AcknowledgedAt != nil {
		t.Fatalf("alert acked despite 503: %+v", alerts)
	}

	// Rule creation fails as well.
	failing.didFail = false
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/rules", ruleBody("r2", "v", 3, 1))
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing create = %d, want 503", r.Code)
	}
	if r := doRequest(t, h, http.MethodGet, "/v1/devices/gw/rules/r2", ""); r.Code != http.StatusNotFound {
		t.Fatalf("failed create left a rule: %d", r.Code)
	}

	// Once storage is healthy the same requests succeed.
	r = doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r", updateBody("v", 20, 8, 1))
	if r.Code != http.StatusOK {
		t.Fatalf("retried update = %d: %s", r.Code, r.Body.String())
	}
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/alerts/1/ack", "")
	if r.Code != http.StatusOK {
		t.Fatalf("retried ack = %d: %s", r.Code, r.Body.String())
	}
	// The retried update closed alert 1 (rule-changed); the ack then applies to
	// the already-closed alert, which is fine.
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || !alerts[0].Closed || alerts[0].CloseReason != CloseReasonRuleChanged ||
		alerts[0].AcknowledgedAt == nil {
		t.Fatalf("alerts after retries = %+v", alerts)
	}

	// Everything above is exactly what a restart recovers.
	store.wal = real
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	rule = decodeBody[Rule](t, doRequest(t, h2, http.MethodGet, "/v1/devices/gw/rules/r", ""))
	if rule.Version != 2 || rule.Trigger != 20 {
		t.Fatalf("recovered rule = %+v", rule)
	}
	alerts = listAlerts(t, h2, "gw", "")
	if len(alerts) != 1 || !alerts[0].Closed || alerts[0].AcknowledgedAt == nil {
		t.Fatalf("recovered alerts = %+v", alerts)
	}
}

// --- a telemetry write failure leaves no alert changes -------------------------

func TestPersistentTelemetryFailureLeavesNoAlertChanges(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	mustCreateRule(t, h, "gw", ruleBody("r", "v", 10, 5))

	real := store.wal.(*walFile)
	failing := &failingWAL{inner: real}
	store.wal = failing

	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"v":50}`)
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing telemetry = %d, want 503", r.Code)
	}
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 0 {
		t.Fatalf("failed telemetry left an alert: %+v", alerts)
	}

	// Retry commits both the sample and its alert; a restart sees exactly one.
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"v":50}`)
	if r.Code != http.StatusAccepted {
		t.Fatalf("retried telemetry = %d", r.Code)
	}
	store.wal = real
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	alerts := listAlerts(t, h2, "gw", "")
	if len(alerts) != 1 || alerts[0].AlertID != 1 || alerts[0].TriggeredBy.Sequence != 1 {
		t.Fatalf("recovered alerts = %+v", alerts)
	}
}

// --- replay batches commit alert changes atomically ----------------------------

func TestPersistentReplayAlertChangesAreOneCommitUnit(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	mustCreateRule(t, h, "gw", ruleBody("r", "v", 10, 5))

	real := store.wal.(*walFile)
	failing := &failingWAL{inner: real}
	store.wal = failing

	// A batch that would trigger fails at the commit point: no sample, no
	// receipt, no alert.
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b1", sample("e1", "2024-01-02T10:00:00Z", `{"v":50}`)))
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing replay = %d, want 503", r.Code)
	}
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 0 {
		t.Fatalf("failed replay left an alert: %+v", alerts)
	}
	if events := allEvents(t, h, "gw"); len(events) != 0 {
		t.Fatalf("failed replay left events: %+v", events)
	}

	// The retry commits sample, receipt and alert together.
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b1", sample("e1", "2024-01-02T10:00:00Z", `{"v":50}`)))
	if r.Code != http.StatusAccepted {
		t.Fatalf("retried replay = %d: %s", r.Code, r.Body.String())
	}
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].TriggeredBy.Sequence != 1 {
		t.Fatalf("alerts = %+v", alerts)
	}

	store.wal = real
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	if alerts := listAlerts(t, h2, "gw", ""); len(alerts) != 1 || alerts[0].TriggeredBy.Sequence != 1 {
		t.Fatalf("recovered alerts = %+v", alerts)
	}
	// The receipt survived identically and does not re-trigger.
	r = doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b1", sample("e1", "2024-01-02T10:00:00Z", `{"v":50}`)))
	if r.Code != http.StatusOK {
		t.Fatalf("repeat batch after restart = %d, want 200", r.Code)
	}
	if alerts := listAlerts(t, h2, "gw", ""); len(alerts) != 1 {
		t.Fatalf("repeat batch re-triggered after restart: %+v", alerts)
	}
}

// --- rule disable/enable and alert state across restart ------------------------

func TestPersistentRuleDisableEnableSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	mustCreateRule(t, h, "gw", ruleBody("r", "v", 10, 5))
	sendTelemetry(t, h, "gw", `{"v":50}`) // opens alert 1
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/rules/r/disable", `{"version":1}`); r.Code != http.StatusOK {
		t.Fatalf("disable = %d", r.Code)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	rule := decodeBody[Rule](t, doRequest(t, h2, http.MethodGet, "/v1/devices/gw/rules/r", ""))
	if rule.Enabled || rule.Version != 2 {
		t.Fatalf("recovered rule = %+v, want disabled at version 2", rule)
	}
	// The disable closed the active alert as a rule change.
	alerts := listAlerts(t, h2, "gw", "")
	if len(alerts) != 1 || !alerts[0].Closed || alerts[0].CloseReason != CloseReasonRuleChanged {
		t.Fatalf("recovered alerts = %+v", alerts)
	}
	// While disabled, samples are accepted but not judged.
	sendTelemetry(t, h2, "gw", `{"v":99}`)
	if alerts := listAlerts(t, h2, "gw", ""); len(alerts) != 1 {
		t.Fatalf("disabled rule evaluated after restart: %+v", alerts)
	}
}
