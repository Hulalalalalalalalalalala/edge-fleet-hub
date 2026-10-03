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

// TestPersistentRuleUpdateFailureKeepsOldRuleAndActiveAlert walks the full
// scenario: an update whose local save fails with 503 must not disturb the old
// rule or its open alert; later samples keep judging by the old thresholds, the
// failed attempt consumes no version, and the same request commits afterwards.
func TestPersistentRuleUpdateFailureKeepsOldRuleAndActiveAlert(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)

	// Temperature 32 opens alert 1 under version 1; ack it so the ack time is
	// also expected to survive the failed update.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":32}`)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/alerts/1/acknowledge", "")
	before := decodeBody[ruleResponse](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/rules/r1", ""))

	// The 35/28 update cannot be saved locally.
	real := store.wal.(*walFile)
	failing := &failingWAL{inner: real}
	store.wal = failing
	r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":35,"recover":28,"version":1}`)
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing update = %d, want 503: %s", r.Code, r.Body.String())
	}

	// The rule query still shows the pre-update rule in every respect.
	after := decodeBody[ruleResponse](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/rules/r1", ""))
	if after.Metric != "temperature" || after.Trigger != 30 || after.Recover != 25 ||
		!after.Enabled || after.Version != 1 ||
		!after.UpdatedAt.Equal(before.UpdatedAt) || !after.CreatedAt.Equal(before.CreatedAt) {
		t.Fatalf("rule changed after failed save: %+v", after)
	}

	// The same single alert is still open with its original evidence and ack.
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 {
		t.Fatalf("alerts after failed save = %+v, want 1", alerts)
	}
	alert := alerts[0]
	if alert.ID != 1 || alert.Status != alertStatusActive || alert.RuleVersion != 1 ||
		alert.TriggerSequence != 1 || alert.TriggerValue != 32 ||
		alert.Trigger != 30 || alert.Recover != 25 || alert.AcknowledgedAt == nil ||
		alert.EndReason != "" || alert.EndedAt != nil ||
		alert.RecoverSequence != nil || alert.RecoverValue != nil || alert.RecoverObservedAt != nil {
		t.Fatalf("alert changed after failed save: %+v", alert)
	}

	// Storage recovers; samples continue under the old thresholds.
	store.wal = real

	// 33 is high by the old trigger (30): the existing alert is kept, never a
	// second one.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":33}`)
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].ID != 1 || alerts[0].Status != alertStatusActive ||
		alerts[0].TriggerSequence != 1 || alerts[0].TriggerValue != 32 {
		t.Fatalf("sample after failed save altered the alert: %+v", alerts)
	}

	// 26 is strictly above the old recovery threshold (25): still open.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":26}`)
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].Status != alertStatusActive ||
		alerts[0].RecoverSequence != nil {
		t.Fatalf("26 ended the alert although recovery is 25: %+v", alerts)
	}

	// 25 reaches the recovery threshold: the original alert ends recovered,
	// carrying this sample's receive sequence, value and time.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":25}`)
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 1 {
		t.Fatalf("recovery created an extra alert: %+v", alerts)
	}
	alert = alerts[0]
	if alert.ID != 1 || alert.Status != alertStatusEnded || alert.EndReason != endReasonRecovered {
		t.Fatalf("alert not recovered by old threshold: %+v", alert)
	}
	if alert.RecoverSequence == nil || *alert.RecoverSequence != 4 ||
		alert.RecoverValue == nil || *alert.RecoverValue != 25 ||
		alert.RecoverObservedAt == nil || alert.RecoverObservedAt.IsZero() {
		t.Fatalf("recovery evidence wrong: %+v", alert)
	}
	if alert.EndedAt != nil {
		t.Fatalf("natural recovery should not set endedAt: %+v", alert)
	}
	// Trigger evidence, rule version and ack survive the later samples.
	if alert.TriggerSequence != 1 || alert.TriggerValue != 32 ||
		alert.RuleVersion != 1 || alert.AcknowledgedAt == nil {
		t.Fatalf("original alert evidence overwritten: %+v", alert)
	}

	// The failed attempt consumed no version: the identical request, still
	// carrying version 1, commits as version 2.
	r = doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":35,"recover":28,"version":1}`)
	if r.Code != http.StatusOK {
		t.Fatalf("retried update = %d, want 200: %s", r.Code, r.Body.String())
	}
	updated := decodeBody[ruleResponse](t, r)
	if updated.Version != 2 || updated.Trigger != 35 || updated.Recover != 28 {
		t.Fatalf("retried update = %+v", updated)
	}

	// Later samples judge by the new thresholds: 33 is mid-band now, 36 fires.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":33}`)
	if active := listAlerts(t, h, "gw", "status=active"); len(active) != 0 {
		t.Fatalf("33 triggered under the new trigger 35: %+v", active)
	}
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":36}`)
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 2 || alerts[1].Status != alertStatusActive ||
		alerts[1].RuleVersion != 2 || alerts[1].TriggerSequence != 6 {
		t.Fatalf("post-update alert = %+v", alerts)
	}
}

// TestPersistentRuleUpdateFailureThenRetryEndsAlertRuleChanged covers the case
// where the alert is still open when the previously failed update is retried:
// the successful update ends it with rule_changed exactly as a first-attempt
// success would.
func TestPersistentRuleUpdateFailureThenRetryEndsAlertRuleChanged(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":32}`)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/alerts/1/acknowledge", "")

	real := store.wal.(*walFile)
	failing := &failingWAL{inner: real}
	store.wal = failing
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":35,"recover":28,"version":1}`); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing update = %d, want 503", r.Code)
	}

	// While still open after the 503, a high sample neither duplicates nor ends
	// anything.
	store.wal = real
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":33}`)
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 1 ||
		alerts[0].Status != alertStatusActive {
		t.Fatalf("alerts before retry = %+v", alerts)
	}

	// Retry with the pre-failure version: 200, exactly one version bump, and the
	// open alert ends rule_changed with the end time recorded.
	r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":35,"recover":28,"version":1}`)
	if r.Code != http.StatusOK {
		t.Fatalf("retried update = %d, want 200: %s", r.Code, r.Body.String())
	}
	updated := decodeBody[ruleResponse](t, r)
	if updated.Version != 2 {
		t.Fatalf("version after retry = %d, want 2", updated.Version)
	}
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v, want 1", alerts)
	}
	alert := alerts[0]
	if alert.Status != alertStatusEnded || alert.EndReason != endReasonRuleChanged ||
		alert.EndedAt == nil || alert.AcknowledgedAt == nil {
		t.Fatalf("alert should end rule_changed with preserved ack: %+v", alert)
	}
	if alert.RecoverSequence != nil || alert.RecoverValue != nil || alert.RecoverObservedAt != nil {
		t.Fatalf("rule_changed alert carries recovery fields: %+v", alert)
	}
	if alert.TriggerSequence != 1 || alert.TriggerValue != 32 || alert.RuleVersion != 1 {
		t.Fatalf("original alert evidence overwritten: %+v", alert)
	}

	// Samples now follow the new thresholds: 33 is mid-band, 36 opens a v2 alert.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":33}`)
	if active := listAlerts(t, h, "gw", "status=active"); len(active) != 0 {
		t.Fatalf("33 triggered under the new trigger 35: %+v", active)
	}
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":36}`)
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 2 || alerts[1].Status != alertStatusActive ||
		alerts[1].RuleVersion != 2 || alerts[1].TriggerSequence != 4 {
		t.Fatalf("post-update alert = %+v", alerts)
	}
}

// TestPersistentRuleUpdateRejectionsKeepActiveAlert verifies that 409 version
// conflicts and 400 validation rejections leave an in-flight alert exactly as
// it was and the old thresholds stay in force afterwards.
func TestPersistentRuleUpdateRejectionsKeepActiveAlert(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":32}`)

	// Stale version -> 409.
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":35,"recover":28,"version":99}`); r.Code != http.StatusConflict {
		t.Fatalf("stale version = %d, want 409", r.Code)
	}
	// Illegal thresholds (recover >= trigger) -> 400.
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":30,"recover":30,"version":1}`); r.Code != http.StatusBadRequest {
		t.Fatalf("invalid thresholds = %d, want 400", r.Code)
	}
	// Blank metric -> 400.
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"metric":"  ","version":1}`); r.Code != http.StatusBadRequest {
		t.Fatalf("blank metric = %d, want 400", r.Code)
	}

	// Rule and the single open alert are untouched.
	rule := decodeBody[ruleResponse](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/rules/r1", ""))
	if rule.Version != 1 || rule.Trigger != 30 || rule.Recover != 25 {
		t.Fatalf("rule changed after rejections: %+v", rule)
	}
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].ID != 1 || alerts[0].Status != alertStatusActive ||
		alerts[0].TriggerSequence != 1 || alerts[0].TriggerValue != 32 ||
		alerts[0].EndReason != "" || alerts[0].EndedAt != nil {
		t.Fatalf("alert changed after rejections: %+v", alerts)
	}

	// Later samples still judge by the old rule: 33 sustains, 25 recovers.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":33}`)
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 1 ||
		alerts[0].Status != alertStatusActive || alerts[0].TriggerSequence != 1 {
		t.Fatalf("sustain after rejections went wrong: %+v", alerts)
	}
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":25}`)
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].Status != alertStatusEnded ||
		alerts[0].EndReason != endReasonRecovered ||
		alerts[0].RecoverSequence == nil || *alerts[0].RecoverSequence != 3 {
		t.Fatalf("recovery after rejections went wrong: %+v", alerts)
	}
}

// TestPersistentRuleUpdateFailureSurvivesRestart verifies the failed save
// leaves nothing partial on disk either: after reopening, the old rule still
// owns its open alert and subsequent samples behave as if no update happened.
func TestPersistentRuleUpdateFailureSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":32}`)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/alerts/1/acknowledge", "")

	real := store.wal.(*walFile)
	failing := &failingWAL{inner: real}
	store.wal = failing
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":35,"recover":28,"version":1}`); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing update = %d, want 503", r.Code)
	}
	// Restore the real sink so Close releases the directory lock.
	store.wal = real
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	rule := decodeBody[ruleResponse](t, doRequest(t, h2, http.MethodGet, "/v1/devices/gw/rules/r1", ""))
	if rule.Version != 1 || rule.Trigger != 30 || rule.Recover != 25 || !rule.Enabled {
		t.Fatalf("recovered rule after failed save = %+v", rule)
	}
	alerts := listAlerts(t, h2, "gw", "")
	if len(alerts) != 1 || alerts[0].ID != 1 || alerts[0].Status != alertStatusActive ||
		alerts[0].RuleVersion != 1 || alerts[0].TriggerSequence != 1 ||
		alerts[0].TriggerValue != 32 || alerts[0].AcknowledgedAt == nil ||
		alerts[0].EndReason != "" || alerts[0].EndedAt != nil {
		t.Fatalf("recovered alert after failed save = %+v", alerts)
	}

	// Old thresholds remain in force after the restart: 33 sustains, 25 recovers.
	doRequest(t, h2, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":33}`)
	if alerts := listAlerts(t, h2, "gw", ""); len(alerts) != 1 ||
		alerts[0].Status != alertStatusActive {
		t.Fatalf("post-restart sustain failed: %+v", alerts)
	}
	doRequest(t, h2, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":25}`)
	alerts = listAlerts(t, h2, "gw", "")
	if len(alerts) != 1 || alerts[0].Status != alertStatusEnded ||
		alerts[0].EndReason != endReasonRecovered ||
		alerts[0].RecoverSequence == nil || *alerts[0].RecoverSequence != 3 {
		t.Fatalf("post-restart recovery failed: %+v", alerts)
	}

	// The uncommitted change is still retryable against the old version.
	r := doRequest(t, h2, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":35,"recover":28,"version":1}`)
	if r.Code != http.StatusOK {
		t.Fatalf("post-restart retry = %d, want 200: %s", r.Code, r.Body.String())
	}
	if updated := decodeBody[ruleResponse](t, r); updated.Version != 2 {
		t.Fatalf("post-restart retry version = %d, want 2", updated.Version)
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
