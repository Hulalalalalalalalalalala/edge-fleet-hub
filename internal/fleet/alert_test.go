package fleet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// --- helpers -----------------------------------------------------------------

func mustCreateRule(t *testing.T, h http.Handler, device, body string) Rule {
	t.Helper()
	result := doRequest(t, h, http.MethodPost, "/v1/devices/"+device+"/rules", body)
	if result.Code != http.StatusCreated {
		t.Fatalf("create rule: status = %d body = %s", result.Code, result.Body.String())
	}
	return decodeBody[Rule](t, result)
}

func ruleBody(ruleID, metric string, trigger, recover float64) string {
	return fmt.Sprintf(`{"ruleId":%q,"metric":%q,"trigger":%v,"recover":%v}`, ruleID, metric, trigger, recover)
}

func updateBody(metric string, trigger, recover float64, version int64) string {
	return fmt.Sprintf(`{"metric":%q,"trigger":%v,"recover":%v,"version":%d}`, metric, trigger, recover, version)
}

type alertsResponse struct {
	DeviceID string  `json:"deviceId"`
	Alerts   []Alert `json:"alerts"`
}

func listAlerts(t *testing.T, h http.Handler, device, query string) []Alert {
	t.Helper()
	target := "/v1/devices/" + device + "/alerts"
	if query != "" {
		target += "?" + query
	}
	result := doRequest(t, h, http.MethodGet, target, "")
	if result.Code != http.StatusOK {
		t.Fatalf("list alerts %s: status = %d body = %s", target, result.Code, result.Body.String())
	}
	return decodeBody[alertsResponse](t, result).Alerts
}

func sendTelemetry(t *testing.T, h http.Handler, device, body string) {
	t.Helper()
	result := doRequest(t, h, http.MethodPost, "/v1/devices/"+device+"/telemetry", body)
	if result.Code != http.StatusAccepted {
		t.Fatalf("telemetry %s: status = %d body = %s", body, result.Code, result.Body.String())
	}
}

// --- rule CRUD and validation -------------------------------------------------

func TestRuleCreateLifecycleAndValidation(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	rule := mustCreateRule(t, h, "gw", ruleBody("high-temp", "temperature", 30, 25))
	if rule.RuleID != "high-temp" || rule.Metric != "temperature" ||
		rule.Trigger != 30 || rule.Recover != 25 {
		t.Fatalf("rule = %+v", rule)
	}
	if !rule.Enabled || rule.Version != 1 {
		t.Fatalf("new rule enabled=%v version=%d, want enabled at version 1", rule.Enabled, rule.Version)
	}
	if !rule.CreatedAt.Equal(*clock) || !rule.UpdatedAt.Equal(*clock) {
		t.Fatalf("rule timestamps = %s/%s, want server time %s", rule.CreatedAt, rule.UpdatedAt, *clock)
	}

	// Duplicate identifier within the device conflicts.
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/rules", ruleBody("high-temp", "temperature", 10, 5)); r.Code != http.StatusConflict {
		t.Fatalf("duplicate rule status = %d, want 409", r.Code)
	}
	// The same identifier on another device is independent.
	mustRegister(t, h, "other")
	mustCreateRule(t, h, "other", ruleBody("high-temp", "temperature", 30, 25))

	// Unknown device is 404.
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/ghost/rules", ruleBody("r", "m", 2, 1)); r.Code != http.StatusNotFound {
		t.Fatalf("unknown device status = %d, want 404", r.Code)
	}

	cases := map[string]string{
		"blank ruleId":       `{"ruleId":"  ","metric":"m","trigger":2,"recover":1}`,
		"missing ruleId":     `{"metric":"m","trigger":2,"recover":1}`,
		"blank metric":       `{"ruleId":"r","metric":" ","trigger":2,"recover":1}`,
		"missing metric":     `{"ruleId":"r","trigger":2,"recover":1}`,
		"recover above":      `{"ruleId":"r","metric":"m","trigger":2,"recover":3}`,
		"recover equal":      `{"ruleId":"r","metric":"m","trigger":2,"recover":2}`,
		"non-finite trigger": `{"ruleId":"r","metric":"m","trigger":1e999,"recover":1}`,
		"non-finite recover": `{"ruleId":"r","metric":"m","trigger":2,"recover":-1e999}`,
		"unknown field":      `{"ruleId":"r","metric":"m","trigger":2,"recover":1,"enabled":false}`,
		"two JSON values":    `{"ruleId":"r","metric":"m","trigger":2,"recover":1}{}`,
		"not JSON":           `nope`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/rules", body); r.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", r.Code, r.Body.String())
			}
		})
	}
}

func TestRuleGetAndList(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	mustCreateRule(t, h, "gw", ruleBody("b-rule", "temperature", 30, 25))
	mustCreateRule(t, h, "gw", ruleBody("a-rule", "battery", 10, 5))

	got := doRequest(t, h, http.MethodGet, "/v1/devices/gw/rules/a-rule", "")
	if got.Code != http.StatusOK {
		t.Fatalf("get rule status = %d", got.Code)
	}
	if rule := decodeBody[Rule](t, got); rule.RuleID != "a-rule" || rule.Metric != "battery" {
		t.Fatalf("get rule = %+v", rule)
	}

	listed := decodeBody[struct {
		Rules []Rule `json:"rules"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/rules", ""))
	if len(listed.Rules) != 2 || listed.Rules[0].RuleID != "a-rule" || listed.Rules[1].RuleID != "b-rule" {
		t.Fatalf("rules = %+v, want sorted by ruleId", listed.Rules)
	}

	for name, target := range map[string]string{
		"missing rule":        "/v1/devices/gw/rules/ghost",
		"unknown device rule": "/v1/devices/ghost/rules/a-rule",
		"unknown device list": "/v1/devices/ghost/rules",
	} {
		t.Run(name, func(t *testing.T) {
			if r := doRequest(t, h, http.MethodGet, target, ""); r.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", r.Code)
			}
		})
	}
}

func TestRuleUpdateRequiresCurrentVersion(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	mustCreateRule(t, h, "gw", ruleBody("r1", "temperature", 30, 25))

	// Stale version: 409 and the rule is unchanged.
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1", updateBody("temperature", 50, 40, 7)); r.Code != http.StatusConflict {
		t.Fatalf("stale version status = %d, want 409", r.Code)
	}
	rule := decodeBody[Rule](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/rules/r1", ""))
	if rule.Version != 1 || rule.Trigger != 30 {
		t.Fatalf("rule changed despite 409: %+v", rule)
	}

	// Missing version is a 400.
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1", `{"metric":"temperature","trigger":50,"recover":40}`); r.Code != http.StatusBadRequest {
		t.Fatalf("missing version status = %d, want 400", r.Code)
	}
	// The identifier cannot be changed by an update.
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1", `{"ruleId":"other","metric":"temperature","trigger":50,"recover":40,"version":1}`); r.Code != http.StatusBadRequest {
		t.Fatalf("ruleId in update status = %d, want 400", r.Code)
	}
	// Invalid spec is a 400 even with the right version.
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1", updateBody("temperature", 10, 20, 1)); r.Code != http.StatusBadRequest {
		t.Fatalf("recover >= trigger status = %d, want 400", r.Code)
	}

	// Correct version updates and bumps.
	updated := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1", updateBody("humidity", 50, 40, 1))
	if updated.Code != http.StatusOK {
		t.Fatalf("update status = %d: %s", updated.Code, updated.Body.String())
	}
	rule = decodeBody[Rule](t, updated)
	if rule.Version != 2 || rule.Metric != "humidity" || rule.Trigger != 50 || rule.Recover != 40 {
		t.Fatalf("updated rule = %+v", rule)
	}

	// Unknown rule / device are 404.
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/ghost", updateBody("m", 2, 1, 1)); r.Code != http.StatusNotFound {
		t.Fatalf("missing rule status = %d, want 404", r.Code)
	}
	if r := doRequest(t, h, http.MethodPut, "/v1/devices/ghost/rules/r1", updateBody("m", 2, 1, 1)); r.Code != http.StatusNotFound {
		t.Fatalf("unknown device status = %d, want 404", r.Code)
	}
}

func TestRuleConcurrentUpdateSameVersion(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	mustCreateRule(t, h, "gw", ruleBody("r1", "temperature", 30, 25))

	const callers = 8
	var start, wg sync.WaitGroup
	start.Add(1)
	codes := make([]int, callers)
	for c := 0; c < callers; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			start.Wait()
			r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
				updateBody("temperature", float64(40+c), 10, 1))
			codes[c] = r.Code
		}(c)
	}
	start.Done()
	wg.Wait()

	ok, conflict := 0, 0
	for _, code := range codes {
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if ok != 1 || conflict != callers-1 {
		t.Fatalf("codes ok=%d conflict=%d, want exactly one 200 and %d 409", ok, conflict, callers-1)
	}
	rule := decodeBody[Rule](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/rules/r1", ""))
	if rule.Version != 2 {
		t.Fatalf("version = %d, want 2 after a single successful update", rule.Version)
	}
}

func TestRuleDisableEnable(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	mustCreateRule(t, h, "gw", ruleBody("r1", "temperature", 30, 25))

	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/rules/r1/disable", `{"version":9}`); r.Code != http.StatusConflict {
		t.Fatalf("disable stale version = %d, want 409", r.Code)
	}
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/rules/r1/disable", `{"version":1}`)
	if r.Code != http.StatusOK {
		t.Fatalf("disable = %d: %s", r.Code, r.Body.String())
	}
	if rule := decodeBody[Rule](t, r); rule.Enabled || rule.Version != 2 {
		t.Fatalf("disabled rule = %+v, want disabled at version 2", rule)
	}

	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/rules/r1/enable", `{"version":2}`)
	if r.Code != http.StatusOK {
		t.Fatalf("enable = %d: %s", r.Code, r.Body.String())
	}
	if rule := decodeBody[Rule](t, r); !rule.Enabled || rule.Version != 3 {
		t.Fatalf("enabled rule = %+v, want enabled at version 3", rule)
	}

	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/rules/ghost/disable", `{"version":1}`); r.Code != http.StatusNotFound {
		t.Fatalf("disable missing rule = %d, want 404", r.Code)
	}
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/rules/r1/disable", `{}`); r.Code != http.StatusBadRequest {
		t.Fatalf("disable without version = %d, want 400", r.Code)
	}
}

// --- alert evaluation ----------------------------------------------------------

func TestAlertLifecycleViaTelemetry(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	mustCreateRule(t, h, "gw", ruleBody("high-temp", "temperature", 30, 25))

	// Below the trigger: nothing.
	sendTelemetry(t, h, "gw", `{"temperature":29.9}`)
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 0 {
		t.Fatalf("alerts = %+v, want none below trigger", alerts)
	}

	// Reaching the trigger opens an alert pinned to the rule version.
	triggeredAt := clock.Add(time.Minute)
	*clock = triggeredAt
	sendTelemetry(t, h, "gw", `{"temperature":31.5}`)
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v, want one open alert", alerts)
	}
	alert := alerts[0]
	if alert.AlertID != 1 || alert.RuleID != "high-temp" || alert.RuleVersion != 1 ||
		alert.Metric != "temperature" || alert.Trigger != 30 || alert.Recover != 25 {
		t.Fatalf("alert = %+v", alert)
	}
	if alert.Closed || alert.CloseReason != "" || alert.ClosedAt != nil || alert.RecoveredBy != nil {
		t.Fatalf("alert should be open: %+v", alert)
	}
	if alert.TriggeredBy.Sequence != 2 || alert.TriggeredBy.Value != 31.5 || !alert.TriggeredBy.ObservedAt.Equal(triggeredAt) {
		t.Fatalf("triggeredBy = %+v, want sequence 2 value 31.5 at %s", alert.TriggeredBy, triggeredAt)
	}

	// Sustained high values keep the same alert.
	sendTelemetry(t, h, "gw", `{"temperature":32}`)
	sendTelemetry(t, h, "gw", `{"temperature":30}`)
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 1 || alerts[0].AlertID != 1 || alerts[0].Closed {
		t.Fatalf("sustained high changed the alert: %+v", alerts)
	}

	// Dropping to the recovery threshold ends it with the recovery sample.
	recoveredAt := clock.Add(2 * time.Minute)
	*clock = recoveredAt
	sendTelemetry(t, h, "gw", `{"temperature":25}`)
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || !alerts[0].Closed || alerts[0].CloseReason != CloseReasonRecovered {
		t.Fatalf("alert after recovery = %+v", alerts)
	}
	if alerts[0].ClosedAt == nil || !alerts[0].ClosedAt.Equal(recoveredAt) {
		t.Fatalf("closedAt = %v, want server time %s", alerts[0].ClosedAt, recoveredAt)
	}
	recovered := alerts[0].RecoveredBy
	if recovered == nil || recovered.Sequence != 5 || recovered.Value != 25 || !recovered.ObservedAt.Equal(recoveredAt) {
		t.Fatalf("recoveredBy = %+v, want sequence 5 value 25 at %s", recovered, recoveredAt)
	}

	// Reaching the trigger again opens a new alert with a new identifier.
	sendTelemetry(t, h, "gw", `{"temperature":40}`)
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 2 || alerts[1].AlertID != 2 || alerts[1].Closed {
		t.Fatalf("re-trigger alerts = %+v, want a new open alert id 2", alerts)
	}
}

func TestAlertBetweenThresholdsAndMissingMetricKeepState(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	mustCreateRule(t, h, "gw", ruleBody("r", "temperature", 30, 25))

	// Between the thresholds no alert opens.
	sendTelemetry(t, h, "gw", `{"temperature":27}`)
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 0 {
		t.Fatalf("alerts = %+v, want none between thresholds", alerts)
	}

	sendTelemetry(t, h, "gw", `{"temperature":35}`)
	// Between thresholds and samples without the metric keep the alert open.
	sendTelemetry(t, h, "gw", `{"temperature":27}`)
	sendTelemetry(t, h, "gw", `{"humidity":80}`)
	sendTelemetry(t, h, "gw", `{"temperature":26}`)
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].Closed {
		t.Fatalf("alert should stay open between thresholds: %+v", alerts)
	}
}

func TestAlertMultipleRulesPerDevice(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	mustCreateRule(t, h, "gw", ruleBody("hot", "temperature", 30, 25))
	mustCreateRule(t, h, "gw", ruleBody("humid", "humidity", 80, 60))

	sendTelemetry(t, h, "gw", `{"temperature":35,"humidity":90}`)
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 2 {
		t.Fatalf("alerts = %+v, want one per rule", alerts)
	}
	byRule := map[string]Alert{}
	for _, alert := range alerts {
		byRule[alert.RuleID] = alert
	}
	if byRule["hot"].TriggeredBy.Value != 35 || byRule["humid"].TriggeredBy.Value != 90 {
		t.Fatalf("trigger samples = %+v", alerts)
	}
	// Distinct identifiers.
	if byRule["hot"].AlertID == byRule["humid"].AlertID {
		t.Fatalf("alert ids must differ: %+v", alerts)
	}
}

func TestAlertRulesDoNotBackfillHistory(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	// High samples committed before the rule exists are never re-judged.
	sendTelemetry(t, h, "gw", `{"temperature":99}`)
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b1", sample("e1", "2024-01-02T10:00:00Z", `{"temperature":88}`))); r.Code != http.StatusAccepted {
		t.Fatalf("replay = %d", r.Code)
	}
	mustCreateRule(t, h, "gw", ruleBody("r", "temperature", 30, 25))
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 0 {
		t.Fatalf("rule backfilled historical samples: %+v", alerts)
	}
	// Only samples accepted after creation count.
	sendTelemetry(t, h, "gw", `{"temperature":31}`)
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].TriggeredBy.Sequence != 3 {
		t.Fatalf("alerts = %+v, want one alert from the new sample (sequence 3)", alerts)
	}
}

func TestAlertReplayParticipatesAndDuplicatesDoNot(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	mustCreateRule(t, h, "gw", ruleBody("r", "v", 10, 5))

	// A replayed batch triggers by receive order, not observation order.
	body := replayBody("b1",
		sample("e1", "2024-01-02T10:00:00Z", `{"v":3}`),
		sample("e2", "2024-01-01T10:00:00Z", `{"v":20}`), // observed earlier, received later
	)
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body); r.Code != http.StatusAccepted {
		t.Fatalf("replay = %d", r.Code)
	}
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].TriggeredBy.Sequence != 2 || alerts[0].TriggeredBy.Value != 20 {
		t.Fatalf("alerts = %+v, want trigger at receive sequence 2", alerts)
	}

	// Re-submitting the identical batch is an idempotent 200 and changes no alerts.
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body); r.Code != http.StatusOK {
		t.Fatalf("repeat batch = %d, want 200", r.Code)
	}
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 1 || alerts[0].Closed {
		t.Fatalf("duplicate batch changed alerts: %+v", alerts)
	}

	// A duplicate sample inside a new batch is not evaluated: e2 reads 20 again
	// (a duplicate) and e3 recovers; only e3 may act.
	body2 := replayBody("b2",
		sample("e2", "2024-01-01T10:00:00Z", `{"v":20}`),
		sample("e3", "2024-01-02T11:00:00Z", `{"v":1}`),
	)
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body2); r.Code != http.StatusAccepted {
		t.Fatalf("replay b2 = %d", r.Code)
	}
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || !alerts[0].Closed || alerts[0].RecoveredBy == nil || alerts[0].RecoveredBy.Sequence != 3 {
		t.Fatalf("alerts after mixed batch = %+v, want recovery by sequence 3 only", alerts)
	}
}

func TestAlertBatchTriggerAndRecoverBothVisible(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	mustCreateRule(t, h, "gw", ruleBody("r", "v", 10, 5))

	body := replayBody("b1",
		sample("e1", "2024-01-02T10:00:00Z", `{"v":15}`),
		sample("e2", "2024-01-02T10:05:00Z", `{"v":2}`),
		sample("e3", "2024-01-02T10:10:00Z", `{"v":16}`),
	)
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body); r.Code != http.StatusAccepted {
		t.Fatalf("replay = %d", r.Code)
	}
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 2 {
		t.Fatalf("alerts = %+v, want trigger/recover cycle plus a new trigger", alerts)
	}
	if !alerts[0].Closed || alerts[0].CloseReason != CloseReasonRecovered ||
		alerts[0].RecoveredBy == nil || alerts[0].RecoveredBy.Sequence != 2 {
		t.Fatalf("first alert = %+v, want recovered by sequence 2", alerts[0])
	}
	if alerts[1].Closed || alerts[1].TriggeredBy.Sequence != 3 {
		t.Fatalf("second alert = %+v, want open from sequence 3", alerts[1])
	}
}

func TestAlertConflictBatchLeavesNoAlertChanges(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	mustCreateRule(t, h, "gw", ruleBody("r", "v", 10, 5))
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b1", sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`))); r.Code != http.StatusAccepted {
		t.Fatalf("seed = %d", r.Code)
	}
	// e2 would trigger, but e1 conflicts: the whole batch rolls back.
	conflict := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b2",
			sample("e2", "2024-01-02T11:00:00Z", `{"v":50}`),
			sample("e1", "2024-01-02T10:00:00Z", `{"v":999}`),
		))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict = %d, want 409", conflict.Code)
	}
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 0 {
		t.Fatalf("conflicted batch left alerts: %+v", alerts)
	}
}

func TestRuleUpdateClosesActiveAlert(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	mustCreateRule(t, h, "gw", ruleBody("r", "temperature", 30, 25))
	sendTelemetry(t, h, "gw", `{"temperature":35}`)

	updatedAt := clock.Add(time.Minute)
	*clock = updatedAt
	r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r", updateBody("temperature", 50, 40, 1))
	if r.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", r.Code, r.Body.String())
	}
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || !alerts[0].Closed || alerts[0].CloseReason != CloseReasonRuleChanged {
		t.Fatalf("alert after rule update = %+v, want closed with reason rule-changed", alerts)
	}
	if alerts[0].ClosedAt == nil || !alerts[0].ClosedAt.Equal(updatedAt) {
		t.Fatalf("closedAt = %v, want server time %s", alerts[0].ClosedAt, updatedAt)
	}
	if alerts[0].RecoveredBy != nil {
		t.Fatalf("rule-changed close must not carry a recovery sample: %+v", alerts[0])
	}
	// The alert keeps the thresholds of the version that triggered it.
	if alerts[0].RuleVersion != 1 || alerts[0].Trigger != 30 || alerts[0].Recover != 25 {
		t.Fatalf("alert lost its pinned rule version: %+v", alerts[0])
	}

	// Evaluation restarts with no active alert under the new thresholds: a
	// value between the old and new triggers does nothing.
	sendTelemetry(t, h, "gw", `{"temperature":35}`)
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 1 {
		t.Fatalf("alerts = %+v, want no new alert below the new trigger", alerts)
	}
	sendTelemetry(t, h, "gw", `{"temperature":55}`)
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 2 || alerts[1].RuleVersion != 2 || alerts[1].Trigger != 50 || alerts[1].Closed {
		t.Fatalf("alerts = %+v, want a new alert pinned to rule version 2", alerts)
	}
}

func TestDisabledRuleSkipsEvaluation(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	mustCreateRule(t, h, "gw", ruleBody("r", "v", 10, 5))
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/rules/r/disable", `{"version":1}`); r.Code != http.StatusOK {
		t.Fatalf("disable = %d", r.Code)
	}

	// Telemetry is still accepted while disabled, but not judged.
	sendTelemetry(t, h, "gw", `{"v":50}`)
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b1", sample("e1", "2024-01-02T10:00:00Z", `{"v":60}`))); r.Code != http.StatusAccepted {
		t.Fatalf("replay = %d", r.Code)
	}
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 0 {
		t.Fatalf("disabled rule evaluated samples: %+v", alerts)
	}
	if events := allEvents(t, h, "gw"); len(events) != 2 {
		t.Fatalf("events = %+v, want telemetry still recorded while disabled", events)
	}

	// Re-enabling starts fresh: the samples accepted while disabled are not
	// re-judged, only new ones are.
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/rules/r/enable", `{"version":2}`); r.Code != http.StatusOK {
		t.Fatalf("enable = %d", r.Code)
	}
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 0 {
		t.Fatalf("enable re-judged skipped samples: %+v", alerts)
	}
	sendTelemetry(t, h, "gw", `{"v":11}`)
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 1 || alerts[0].RuleVersion != 3 {
		t.Fatalf("alerts = %+v, want one alert from the enabled rule version 3", alerts)
	}
}

// --- acknowledgement -----------------------------------------------------------

func TestAckAlert(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	mustCreateRule(t, h, "gw", ruleBody("r", "v", 10, 5))
	sendTelemetry(t, h, "gw", `{"v":20}`)

	ackedAt := clock.Add(time.Minute)
	*clock = ackedAt
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/alerts/1/ack", "")
	if r.Code != http.StatusOK {
		t.Fatalf("ack = %d: %s", r.Code, r.Body.String())
	}
	alert := decodeBody[Alert](t, r)
	if alert.AcknowledgedAt == nil || !alert.AcknowledgedAt.Equal(ackedAt) {
		t.Fatalf("acknowledgedAt = %v, want server time %s", alert.AcknowledgedAt, ackedAt)
	}
	if alert.Closed {
		t.Fatalf("ack must not close the alert: %+v", alert)
	}

	// A repeated acknowledgement keeps the first server time.
	*clock = clock.Add(time.Hour)
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/alerts/1/ack", "")
	if r.Code != http.StatusOK {
		t.Fatalf("repeat ack = %d", r.Code)
	}
	if alert := decodeBody[Alert](t, r); alert.AcknowledgedAt == nil || !alert.AcknowledgedAt.Equal(ackedAt) {
		t.Fatalf("repeat ack moved the time: %+v", alert)
	}

	// Acknowledgement does not block recovery or re-triggering.
	sendTelemetry(t, h, "gw", `{"v":1}`)
	sendTelemetry(t, h, "gw", `{"v":20}`)
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 2 || !alerts[0].Closed || alerts[0].CloseReason != CloseReasonRecovered {
		t.Fatalf("alerts = %+v, want the acked alert recovered", alerts)
	}
	if alerts[1].AcknowledgedAt != nil || alerts[1].Closed {
		t.Fatalf("new alert = %+v, want open and unacknowledged", alerts[1])
	}

	// Unknown alerts and devices are 404.
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/alerts/99/ack", ""); r.Code != http.StatusNotFound {
		t.Fatalf("ack missing alert = %d, want 404", r.Code)
	}
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/alerts/notanumber/ack", ""); r.Code != http.StatusNotFound {
		t.Fatalf("ack malformed alert id = %d, want 404", r.Code)
	}
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/ghost/alerts/1/ack", ""); r.Code != http.StatusNotFound {
		t.Fatalf("ack on unknown device = %d, want 404", r.Code)
	}
	if r := doRequest(t, h, http.MethodGet, "/v1/devices/gw/alerts/99", ""); r.Code != http.StatusNotFound {
		t.Fatalf("get missing alert = %d, want 404", r.Code)
	}
}

// --- alert queries ---------------------------------------------------------------

func TestAlertFilters(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	mustCreateRule(t, h, "gw", ruleBody("temp", "temperature", 30, 25))
	mustCreateRule(t, h, "gw", ruleBody("hum", "humidity", 80, 60))

	sendTelemetry(t, h, "gw", `{"temperature":35}`) // alert 1 (temp, open)
	sendTelemetry(t, h, "gw", `{"humidity":90}`)    // alert 2 (hum, open)
	sendTelemetry(t, h, "gw", `{"temperature":10}`) // closes alert 1
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/alerts/2/ack", ""); r.Code != http.StatusOK {
		t.Fatalf("ack = %d", r.Code)
	}

	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 3-1 {
		t.Fatalf("alerts = %+v, want 2", alerts)
	}
	if alerts := listAlerts(t, h, "gw", "ruleId=temp"); len(alerts) != 1 || alerts[0].AlertID != 1 {
		t.Fatalf("ruleId filter = %+v", alerts)
	}
	if alerts := listAlerts(t, h, "gw", "closed=true"); len(alerts) != 1 || alerts[0].AlertID != 1 {
		t.Fatalf("closed=true filter = %+v", alerts)
	}
	if alerts := listAlerts(t, h, "gw", "closed=false"); len(alerts) != 1 || alerts[0].AlertID != 2 {
		t.Fatalf("closed=false filter = %+v", alerts)
	}
	if alerts := listAlerts(t, h, "gw", "acknowledged=true"); len(alerts) != 1 || alerts[0].AlertID != 2 {
		t.Fatalf("acknowledged=true filter = %+v", alerts)
	}
	if alerts := listAlerts(t, h, "gw", "acknowledged=false&closed=true"); len(alerts) != 1 || alerts[0].AlertID != 1 {
		t.Fatalf("combined filter = %+v", alerts)
	}
	if alerts := listAlerts(t, h, "gw", "ruleId=temp&closed=false"); len(alerts) != 0 {
		t.Fatalf("empty filter result = %+v", alerts)
	}

	for name, target := range map[string]string{
		"bad closed":       "/v1/devices/gw/alerts?closed=maybe",
		"bad acknowledged": "/v1/devices/gw/alerts?acknowledged=1x",
		"unknown device":   "/v1/devices/ghost/alerts",
	} {
		t.Run(name, func(t *testing.T) {
			want := http.StatusBadRequest
			if name == "unknown device" {
				want = http.StatusNotFound
			}
			if r := doRequest(t, h, http.MethodGet, target, ""); r.Code != want {
				t.Fatalf("status = %d, want %d", r.Code, want)
			}
		})
	}
}

func TestGetAlertByID(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	mustCreateRule(t, h, "gw", ruleBody("r", "v", 10, 5))
	sendTelemetry(t, h, "gw", `{"v":20}`)

	r := doRequest(t, h, http.MethodGet, "/v1/devices/gw/alerts/1", "")
	if r.Code != http.StatusOK {
		t.Fatalf("get alert = %d", r.Code)
	}
	var alert Alert
	if err := json.Unmarshal(r.Body.Bytes(), &alert); err != nil {
		t.Fatal(err)
	}
	if alert.AlertID != 1 || alert.RuleID != "r" || alert.Closed {
		t.Fatalf("alert = %+v", alert)
	}
}

// --- alerts and history stay independent ---------------------------------------

func TestAlertsDoNotAffectHistoryOrReceipts(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	mustCreateRule(t, h, "gw", ruleBody("r", "v", 10, 5))

	body := replayBody("b1", sample("e1", "2024-01-02T10:00:00Z", `{"v":50}`))
	first := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body)
	if first.Code != http.StatusAccepted {
		t.Fatalf("replay = %d", first.Code)
	}
	firstReceipt := first.Body.String()

	// Alert state changes nothing about receipts or history.
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", body); r.Code != http.StatusOK || r.Body.String() != firstReceipt {
		t.Fatalf("repeat receipt changed: %d %s", r.Code, r.Body.String())
	}
	events := allEvents(t, h, "gw")
	if len(events) != 1 || events[0].Sequence != 1 || events[0].Values["v"] != 50 {
		t.Fatalf("events = %+v", events)
	}
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 1 || alerts[0].TriggeredBy.Sequence != 1 {
		t.Fatalf("alerts = %+v", alerts)
	}
}
