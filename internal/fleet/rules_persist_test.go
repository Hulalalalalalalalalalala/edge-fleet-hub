package fleet

import (
	"net/http"
	"testing"
)

// --- restart preserves rules, alerts and ack times ---------------------------

func TestPersistentRestartPreservesRulesAndAlerts(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)

	// Trigger, sustain, recover, then ack.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":32}`)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":33}`)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":24}`)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/alerts/1/acknowledge", "")

	// Update the rule (version 1 -> 2) with no active alert.
	doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":35,"recover":28,"version":1}`)

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	// Rule version and config survive.
	rules := listRules(t, h2, "gw")
	if len(rules) != 1 || rules[0].Version != 2 || rules[0].Trigger != 35 ||
		rules[0].Recover != 28 || !rules[0].Enabled {
		t.Fatalf("recovered rule = %+v", rules)
	}

	// Alert, its recovery fields and ack time survive.
	alerts := listAlerts(t, h2, "gw", "")
	if len(alerts) != 1 {
		t.Fatalf("recovered alerts = %+v", alerts)
	}
	alert := alerts[0]
	if alert.ID != 1 || alert.Status != alertStatusEnded ||
		alert.EndReason != endReasonRecovered || alert.TriggerSequence != 1 ||
		alert.TriggerValue != 32 || alert.RecoverSequence == nil ||
		*alert.RecoverSequence != 3 || *alert.RecoverValue != 24 {
		t.Fatalf("recovered alert = %+v", alert)
	}
	if alert.AcknowledgedAt == nil {
		t.Fatalf("ack time lost across restart")
	}

	// No re-trigger on recovery: the active alert count is unchanged.
	active := listAlerts(t, h2, "gw", "status=active")
	if len(active) != 0 {
		t.Fatalf("recovery re-triggered: %+v", active)
	}

	// New samples continue to judge against the recovered rule version.
	doRequest(t, h2, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":40}`)
	alerts = listAlerts(t, h2, "gw", "")
	if len(alerts) != 2 || alerts[1].Status != alertStatusActive ||
		alerts[1].RuleVersion != 2 || alerts[1].TriggerSequence != 4 {
		t.Fatalf("post-restart alert = %+v", alerts[1])
	}
}

func TestPersistentRestartPreservesRuleChangedEnding(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)

	// Trigger an active alert, then update the rule: the alert ends rule_changed.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":32}`)
	doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":35,"recover":28,"version":1}`)

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	alerts := listAlerts(t, h2, "gw", "")
	if len(alerts) != 1 {
		t.Fatalf("recovered alerts = %+v", alerts)
	}
	alert := alerts[0]
	if alert.Status != alertStatusEnded || alert.EndReason != endReasonRuleChanged ||
		alert.EndedAt == nil {
		t.Fatalf("recovered rule_changed alert = %+v", alert)
	}
	if alert.RecoverSequence != nil || alert.RecoverValue != nil ||
		alert.RecoverObservedAt != nil {
		t.Fatalf("rule_changed alert should not have recovery fields: %+v", alert)
	}
}

func TestPersistentRestartPreservesReplayAlertChanges(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)

	// One batch triggers and recovers.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b1",
			sample("e1", "2024-01-02T10:00:00Z", `{"temperature":40}`),
			sample("e2", "2024-01-02T10:05:00Z", `{"temperature":20}`),
		))

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	alerts := listAlerts(t, h2, "gw", "")
	if len(alerts) != 1 || alerts[0].Status != alertStatusEnded ||
		alerts[0].EndReason != endReasonRecovered {
		t.Fatalf("recovered replay alert = %+v", alerts)
	}
	if alerts[0].TriggerSequence != 1 || alerts[0].RecoverSequence == nil ||
		*alerts[0].RecoverSequence != 2 {
		t.Fatalf("recovered replay alert fields = %+v", alerts[0])
	}
}

// --- write failure returns 503 and changes nothing ---------------------------

func TestPersistentRuleWriteFailureReturns503(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)

	// Fail the first rule update at the storage layer.
	real := store.wal.(*walFile)
	failing := &failingWAL{inner: real}
	store.wal = failing

	r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":35,"recover":28,"version":1}`)
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing update = %d, want 503: %s", r.Code, r.Body.String())
	}

	// Version and config are unchanged.
	after := decodeBody[ruleResponse](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/rules/r1", ""))
	if after.Version != 1 || after.Trigger != 30 {
		t.Fatalf("state changed after failed write: %+v", after)
	}

	// Once writes succeed, the same request commits.
	store.wal = real
	r = doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":35,"recover":28,"version":1}`)
	if r.Code != http.StatusOK {
		t.Fatalf("retried update = %d: %s", r.Code, r.Body.String())
	}
	after = decodeBody[ruleResponse](t, r)
	if after.Version != 2 || after.Trigger != 35 {
		t.Fatalf("retried update = %+v", after)
	}
}

func TestPersistentAckWriteFailureReturns503(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":32}`)

	real := store.wal.(*walFile)
	failing := &failingWAL{inner: real}
	store.wal = failing

	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/alerts/1/acknowledge", "")
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing ack = %d, want 503", r.Code)
	}
	alerts := listAlerts(t, h, "gw", "")
	if alerts[0].AcknowledgedAt != nil {
		t.Fatalf("ack persisted after failed write")
	}

	store.wal = real
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/alerts/1/acknowledge", "")
	if r.Code != http.StatusOK {
		t.Fatalf("retried ack = %d", r.Code)
	}
	alerts = listAlerts(t, h, "gw", "")
	if alerts[0].AcknowledgedAt == nil {
		t.Fatalf("ack not persisted after retry")
	}
}

func TestPersistentReplayAlertWriteFailureAtomic(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)

	// Fail the replay write: the batch, sequences and alert changes must all
	// be absent.
	real := store.wal.(*walFile)
	failing := &failingWAL{inner: real}
	store.wal = failing

	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b1", sample("e1", "2024-01-02T10:00:00Z", `{"temperature":40}`)))
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing replay = %d, want 503", r.Code)
	}
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 0 {
		t.Fatalf("failed replay left alerts: %+v", alerts)
	}
	if events := allEvents(t, h, "gw"); len(events) != 0 {
		t.Fatalf("failed replay left events: %+v", events)
	}

	// Retry commits everything together.
	store.wal = real
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b1", sample("e1", "2024-01-02T10:00:00Z", `{"temperature":40}`)))
	if r.Code != http.StatusAccepted {
		t.Fatalf("retried replay = %d", r.Code)
	}
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].Status != alertStatusActive {
		t.Fatalf("retried replay alerts = %+v", alerts)
	}
}

// --- existing data directory compatibility -----------------------------------

func TestPersistentOldRecordsRecoverWithoutAlerts(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	// Write telemetry and a replay batch (records without alert fields).
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":20}`)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b1", sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`)))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: old records recover with no rules or alerts.
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	if rules := listRules(t, h2, "gw"); len(rules) != 0 {
		t.Fatalf("rules = %+v", rules)
	}
	if alerts := listAlerts(t, h2, "gw", ""); len(alerts) != 0 {
		t.Fatalf("alerts = %+v", alerts)
	}
	// History and sequences are intact.
	events := allEvents(t, h2, "gw")
	if len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 2 {
		t.Fatalf("events = %+v", events)
	}
	// New rules work after recovery.
	createRule(t, h2, "gw", "r1", "temperature", 30, 25)
	doRequest(t, h2, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":40}`)
	if alerts := listAlerts(t, h2, "gw", ""); len(alerts) != 1 {
		t.Fatalf("new rule alerts = %+v", alerts)
	}
}
