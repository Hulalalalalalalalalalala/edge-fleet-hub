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

// A failed rule update must not disturb the active alert: later samples keep
// judging against the old rule, so no second alert opens and the original
// alert still recovers at the old recovery threshold.
func TestPersistentFailedRuleUpdateKeepsActiveAlert(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	created := createRule(t, h, "gw", "r1", "temperature", 30, 25)

	// Temperature 32 opens alert 1 under rule version 1.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":32}`)

	real := store.wal.(*walFile)
	store.wal = &failingWAL{inner: real}

	r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":35,"recover":28,"version":1}`)
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing update = %d, want 503: %s", r.Code, r.Body.String())
	}

	// The rule is exactly as before: thresholds, enabled flag, version and
	// update time are all untouched by the failed write.
	rule := decodeBody[ruleResponse](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/rules/r1", ""))
	if rule.Metric != "temperature" || rule.Trigger != 30 || rule.Recover != 25 ||
		!rule.Enabled || rule.Version != 1 || !rule.UpdatedAt.Equal(created.UpdatedAt) {
		t.Fatalf("rule changed after failed update: %+v", rule)
	}

	// The alert is still the same active record, with no ending fields.
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 {
		t.Fatalf("alerts after failed update = %+v", alerts)
	}
	alert := alerts[0]
	if alert.ID != 1 || alert.Status != alertStatusActive || alert.RuleVersion != 1 ||
		alert.TriggerSequence != 1 || alert.TriggerValue != 32 ||
		alert.EndedAt != nil || alert.EndReason != "" {
		t.Fatalf("alert disturbed by failed update: %+v", alert)
	}

	// Storage recovers. A sample between the old thresholds keeps the original
	// alert; it must not open a second one.
	store.wal = real
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":33}`)
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].ID != 1 || alerts[0].Status != alertStatusActive {
		t.Fatalf("sustained sample opened a second alert: %+v", alerts)
	}

	// 26 is above the old recovery threshold: still no recovery.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":26}`)
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].Status != alertStatusActive {
		t.Fatalf("sample 26 ended the alert: %+v", alerts)
	}

	// 25 reaches the old recovery threshold: the original alert ends as
	// recovered, recording this sample's sequence, value and observed time.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":25}`)
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 1 {
		t.Fatalf("alerts after recovery = %+v", alerts)
	}
	alert = alerts[0]
	if alert.ID != 1 || alert.Status != alertStatusEnded ||
		alert.EndReason != endReasonRecovered ||
		alert.RecoverSequence == nil || *alert.RecoverSequence != 4 ||
		alert.RecoverValue == nil || *alert.RecoverValue != 25 ||
		alert.RecoverObservedAt == nil {
		t.Fatalf("recovered alert = %+v", alert)
	}
	// The trigger evidence and rule version were not overwritten.
	if alert.RuleVersion != 1 || alert.TriggerSequence != 1 || alert.TriggerValue != 32 {
		t.Fatalf("trigger evidence overwritten: %+v", alert)
	}

	// The failed update never consumed a version: the same request with the
	// pre-failure version now commits and bumps the version exactly once.
	r = doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":35,"recover":28,"version":1}`)
	if r.Code != http.StatusOK {
		t.Fatalf("retried update = %d: %s", r.Code, r.Body.String())
	}
	rule = decodeBody[ruleResponse](t, r)
	if rule.Version != 2 || rule.Trigger != 35 || rule.Recover != 28 {
		t.Fatalf("retried update = %+v", rule)
	}
}

// A failed update retried while its alert is still active ends that alert
// with rule_changed, and rejected updates (409/400) never disturb it either.
func TestPersistentFailedRuleUpdateRetryEndsActiveAlert(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":32}`)

	real := store.wal.(*walFile)
	store.wal = &failingWAL{inner: real}
	r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":35,"recover":28,"version":1}`)
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing update = %d, want 503", r.Code)
	}
	store.wal = real

	// The retry with the pre-failure version succeeds and ends the active
	// alert as rule_changed.
	r = doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":35,"recover":28,"version":1}`)
	if r.Code != http.StatusOK {
		t.Fatalf("retried update = %d: %s", r.Code, r.Body.String())
	}
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].Status != alertStatusEnded ||
		alerts[0].EndReason != endReasonRuleChanged || alerts[0].EndedAt == nil {
		t.Fatalf("alert after retried update = %+v", alerts)
	}

	// New samples judge against the new thresholds.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":36}`)
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 2 || alerts[1].Status != alertStatusActive ||
		alerts[1].RuleVersion != 2 {
		t.Fatalf("alert under new rule = %+v", alerts)
	}

	// A stale version is rejected with 409 and an invalid threshold pair with
	// 400; neither touches the active alert or the rule.
	r = doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":40,"recover":30,"version":1}`)
	if r.Code != http.StatusConflict {
		t.Fatalf("stale update = %d, want 409", r.Code)
	}
	r = doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":20,"recover":30,"version":2}`)
	if r.Code != http.StatusBadRequest {
		t.Fatalf("invalid update = %d, want 400", r.Code)
	}
	rule := decodeBody[ruleResponse](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/rules/r1", ""))
	if rule.Version != 2 || rule.Trigger != 35 || rule.Recover != 28 {
		t.Fatalf("rule changed by rejected updates: %+v", rule)
	}
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 2 || alerts[1].Status != alertStatusActive ||
		alerts[1].EndedAt != nil || alerts[1].EndReason != "" {
		t.Fatalf("alert disturbed by rejected updates: %+v", alerts)
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
