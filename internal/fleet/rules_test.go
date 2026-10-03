package fleet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- response types ---------------------------------------------------------

type ruleResponse struct {
	ID        string    `json:"id"`
	Metric    string    `json:"metric"`
	Trigger   float64   `json:"trigger"`
	Recover   float64   `json:"recover"`
	Enabled   bool      `json:"enabled"`
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type alertResponse struct {
	ID                int64      `json:"id"`
	RuleID            string     `json:"ruleId"`
	RuleVersion       int64      `json:"ruleVersion"`
	Metric            string     `json:"metric"`
	Trigger           float64    `json:"trigger"`
	Recover           float64    `json:"recover"`
	Status            string     `json:"status"`
	TriggerSequence   int64      `json:"triggerSequence"`
	TriggerValue      float64    `json:"triggerValue"`
	TriggerObservedAt time.Time  `json:"triggerObservedAt"`
	RecoverSequence   *int64     `json:"recoverSequence"`
	RecoverValue      *float64   `json:"recoverValue"`
	RecoverObservedAt *time.Time `json:"recoverObservedAt"`
	EndedAt           *time.Time `json:"endedAt"`
	EndReason         string     `json:"endReason"`
	AcknowledgedAt    *time.Time `json:"acknowledgedAt"`
}

func createRule(t *testing.T, h http.Handler, device, id, metric string, trigger, recover float64) ruleResponse {
	t.Helper()
	body := fmt.Sprintf(`{"id":%q,"metric":%q,"trigger":%v,"recover":%v}`, id, metric, trigger, recover)
	r := doRequest(t, h, http.MethodPost, fmt.Sprintf("/v1/devices/%s/rules", device), body)
	if r.Code != http.StatusCreated {
		t.Fatalf("create rule %s = %d, want 201: %s", id, r.Code, r.Body.String())
	}
	return decodeBody[ruleResponse](t, r)
}

func listAlerts(t *testing.T, h http.Handler, device string, query string) []alertResponse {
	t.Helper()
	target := fmt.Sprintf("/v1/devices/%s/alerts", device)
	if query != "" {
		target += "?" + query
	}
	r := doRequest(t, h, http.MethodGet, target, "")
	if r.Code != http.StatusOK {
		t.Fatalf("list alerts = %d: %s", r.Code, r.Body.String())
	}
	return decodeBody[struct {
		Alerts []alertResponse `json:"alerts"`
	}](t, r).Alerts
}

func listRules(t *testing.T, h http.Handler, device string) []ruleResponse {
	t.Helper()
	r := doRequest(t, h, http.MethodGet, fmt.Sprintf("/v1/devices/%s/rules", device), "")
	if r.Code != http.StatusOK {
		t.Fatalf("list rules = %d: %s", r.Code, r.Body.String())
	}
	return decodeBody[struct {
		Rules []ruleResponse `json:"rules"`
	}](t, r).Rules
}

// --- rule CRUD --------------------------------------------------------------

func TestRuleCreateAndList(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")

	rule := createRule(t, h, "gw", "r1", "temperature", 30, 25)
	if rule.Version != 1 || !rule.Enabled || rule.Trigger != 30 || rule.Recover != 25 {
		t.Fatalf("created rule = %+v", rule)
	}

	rules := listRules(t, h, "gw")
	if len(rules) != 1 || rules[0].ID != "r1" || rules[0].Version != 1 {
		t.Fatalf("rules = %+v", rules)
	}

	// A second rule on the same device.
	createRule(t, h, "gw", "r2", "humidity", 80, 60)
	rules = listRules(t, h, "gw")
	if len(rules) != 2 {
		t.Fatalf("rules = %+v, want 2", rules)
	}

	// Get a single rule.
	r := doRequest(t, h, http.MethodGet, "/v1/devices/gw/rules/r1", "")
	if r.Code != http.StatusOK {
		t.Fatalf("get rule = %d", r.Code)
	}
	got := decodeBody[ruleResponse](t, r)
	if got.ID != "r1" || got.Metric != "temperature" {
		t.Fatalf("got rule = %+v", got)
	}
}

func TestRuleCreateErrors(t *testing.T) {
	h := NewHandler(NewStore())

	// Unknown device -> 404.
	r := doRequest(t, h, http.MethodPost, "/v1/devices/ghost/rules",
		`{"id":"r1","metric":"temperature","trigger":30,"recover":25}`)
	if r.Code != http.StatusNotFound {
		t.Fatalf("unknown device = %d, want 404", r.Code)
	}

	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)

	// Duplicate rule id -> 409.
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/rules",
		`{"id":"r1","metric":"humidity","trigger":80,"recover":60}`)
	if r.Code != http.StatusConflict {
		t.Fatalf("duplicate rule = %d, want 409", r.Code)
	}

	// Same rule id on a different device is independent.
	mustRegister(t, h, "other")
	createRule(t, h, "other", "r1", "temperature", 30, 25)

	cases := map[string]string{
		"blank id":        `{"id":"  ","metric":"temperature","trigger":30,"recover":25}`,
		"blank metric":    `{"id":"r2","metric":"  ","trigger":30,"recover":25}`,
		"non-finite trig": `{"id":"r2","metric":"temperature","trigger":1e999,"recover":25}`,
		"non-finite rec":  `{"id":"r2","metric":"temperature","trigger":30,"recover":-1e999}`,
		"rec >= trigger":  `{"id":"r2","metric":"temperature","trigger":25,"recover":30}`,
		"rec == trigger":  `{"id":"r2","metric":"temperature","trigger":30,"recover":30}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/rules", body); r.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", r.Code, r.Body.String())
			}
		})
	}

	// Failed creates leave no rules.
	rules := listRules(t, h, "gw")
	if len(rules) != 1 {
		t.Fatalf("rules = %+v, want only r1", rules)
	}
}

func TestRuleUpdateVersioning(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)

	// Missing version -> 400.
	r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":35,"recover":28}`)
	if r.Code != http.StatusBadRequest {
		t.Fatalf("missing version = %d, want 400", r.Code)
	}

	// Wrong version -> 409, state unchanged.
	r = doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":35,"recover":28,"version":99}`)
	if r.Code != http.StatusConflict {
		t.Fatalf("wrong version = %d, want 409", r.Code)
	}
	after := decodeBody[ruleResponse](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/rules/r1", ""))
	if after.Version != 1 || after.Trigger != 30 {
		t.Fatalf("state changed after 409: %+v", after)
	}

	// Correct version -> 200, version bumps.
	r = doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":35,"recover":28,"version":1}`)
	if r.Code != http.StatusOK {
		t.Fatalf("update = %d, want 200: %s", r.Code, r.Body.String())
	}
	updated := decodeBody[ruleResponse](t, r)
	if updated.Version != 2 || updated.Trigger != 35 || updated.Recover != 28 {
		t.Fatalf("updated rule = %+v", updated)
	}

	// Metric can be changed too; device and rule id stay fixed.
	r = doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"metric":"humidity","version":2}`)
	if r.Code != http.StatusOK {
		t.Fatalf("metric update = %d: %s", r.Code, r.Body.String())
	}
	updated = decodeBody[ruleResponse](t, r)
	if updated.Metric != "humidity" || updated.Version != 3 {
		t.Fatalf("updated rule = %+v", updated)
	}
}

func TestRuleUpdateErrors(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)

	// Unknown rule -> 404.
	r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/nope",
		`{"version":1}`)
	if r.Code != http.StatusNotFound {
		t.Fatalf("unknown rule = %d, want 404", r.Code)
	}

	// Unknown device -> 404.
	r = doRequest(t, h, http.MethodPut, "/v1/devices/ghost/rules/r1",
		`{"version":1}`)
	if r.Code != http.StatusNotFound {
		t.Fatalf("unknown device = %d, want 404", r.Code)
	}

	// Invalid thresholds -> 400, state unchanged.
	cases := map[string]string{
		"blank metric":    `{"metric":"  ","version":1}`,
		"non-finite trig": `{"trigger":1e999,"version":1}`,
		"non-finite rec":  `{"recover":1e999,"version":1}`,
		"rec >= trigger":  `{"recover":30,"version":1}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1", body); r.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", r.Code)
			}
		})
	}
	after := decodeBody[ruleResponse](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/rules/r1", ""))
	if after.Version != 1 || after.Metric != "temperature" {
		t.Fatalf("state changed after failed updates: %+v", after)
	}
}

func TestRuleDisableAndEnable(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)

	// Disable: version bumps, enabled=false.
	r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"enabled":false,"version":1}`)
	if r.Code != http.StatusOK {
		t.Fatalf("disable = %d: %s", r.Code, r.Body.String())
	}
	disabled := decodeBody[ruleResponse](t, r)
	if disabled.Enabled || disabled.Version != 2 {
		t.Fatalf("disabled rule = %+v", disabled)
	}

	// While disabled, telemetry is received but not judged.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":99}`)
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 0 {
		t.Fatalf("disabled rule created alerts: %+v", alerts)
	}

	// Re-enable: version bumps, enabled=true, starts with no active alert.
	r = doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"enabled":true,"version":2}`)
	if r.Code != http.StatusOK {
		t.Fatalf("enable = %d: %s", r.Code, r.Body.String())
	}
	enabled := decodeBody[ruleResponse](t, r)
	if !enabled.Enabled || enabled.Version != 3 {
		t.Fatalf("enabled rule = %+v", enabled)
	}
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":99}`)
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].Status != alertStatusActive {
		t.Fatalf("enabled rule alerts = %+v", alerts)
	}
}

func TestRuleConcurrentUpdatesOneWins(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)

	const callers = 12
	var start, wg sync.WaitGroup
	start.Add(1)
	codes := make([]int, callers)
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			start.Wait()
			r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
				`{"trigger":35,"recover":28,"version":1}`)
			codes[i] = r.Code
		}(i)
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
			t.Errorf("unexpected status %d", code)
		}
	}
	if ok != 1 || conflict != callers-1 {
		t.Fatalf("codes = %d ok / %d conflict, want 1/%d", ok, conflict, callers-1)
	}
	after := decodeBody[ruleResponse](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/rules/r1", ""))
	if after.Version != 2 {
		t.Fatalf("version = %d, want 2", after.Version)
	}
}

// --- alert lifecycle --------------------------------------------------------

func TestAlertTriggerSustainRecoverRetrigger(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)

	// Trigger at seq 1.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":32}`)
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].Status != alertStatusActive {
		t.Fatalf("alerts = %+v", alerts)
	}
	if alerts[0].TriggerSequence != 1 || alerts[0].TriggerValue != 32 ||
		alerts[0].RuleVersion != 1 || alerts[0].Metric != "temperature" ||
		alerts[0].Trigger != 30 || alerts[0].Recover != 25 {
		t.Fatalf("alert fields = %+v", alerts[0])
	}

	// Sustained high keeps the same alert.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":33}`)
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].Status != alertStatusActive ||
		alerts[0].TriggerSequence != 1 || alerts[0].TriggerValue != 32 {
		t.Fatalf("sustained high changed alert: %+v", alerts[0])
	}

	// Between thresholds: no change.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":27}`)
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].Status != alertStatusActive {
		t.Fatalf("mid-band changed alert: %+v", alerts[0])
	}

	// Missing metric: no change.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"humidity":50}`)
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].Status != alertStatusActive {
		t.Fatalf("missing metric changed alert: %+v", alerts[0])
	}

	// Explicit null is not a zero reading: the request is rejected and must not
	// recover the alert, consume a sequence or overwrite trigger evidence.
	nullResult := doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":null}`)
	if nullResult.Code != http.StatusBadRequest {
		t.Fatalf("null telemetry status = %d, want 400: %s", nullResult.Code, nullResult.Body.String())
	}
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].Status != alertStatusActive ||
		alerts[0].TriggerSequence != 1 || alerts[0].TriggerValue != 32 ||
		alerts[0].RecoverSequence != nil || alerts[0].RecoverValue != nil {
		t.Fatalf("null reading disturbed the active alert: %+v", alerts[0])
	}

	// A real zero (finite number at/below the 25 recovery threshold) recovers
	// under the ordinary rule, at the next contiguous sequence (5).
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":0}`); r.Code != http.StatusAccepted {
		t.Fatalf("real zero status = %d, want 202", r.Code)
	}
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].Status != alertStatusEnded ||
		alerts[0].EndReason != endReasonRecovered ||
		alerts[0].RecoverSequence == nil || *alerts[0].RecoverSequence != 5 ||
		alerts[0].RecoverValue == nil || *alerts[0].RecoverValue != 0 {
		t.Fatalf("real zero should recover with sequence 5 and value 0: %+v", alerts[0])
	}
	if alerts[0].RecoverObservedAt == nil {
		t.Fatalf("recovery evidence missing recoverObservedAt: %+v", alerts[0])
	}
	if alerts[0].EndedAt != nil {
		t.Fatalf("recovered alert should not have endedAt: %+v", alerts[0])
	}
	if events := allEvents(t, h, "gw"); len(events) != 5 {
		t.Fatalf("history length = %d, want 5 (null consumed no sequence)", len(events))
	}

	// Re-trigger produces a new alert id.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":40}`)
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 2 {
		t.Fatalf("alerts = %+v, want 2", alerts)
	}
	if alerts[1].ID != 2 || alerts[1].Status != alertStatusActive ||
		alerts[1].TriggerSequence != 6 || alerts[1].TriggerValue != 40 {
		t.Fatalf("re-trigger alert = %+v", alerts[1])
	}
}

func TestAlertAck(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":32}`)

	// Ack records server time.
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/alerts/1/acknowledge", "")
	if r.Code != http.StatusOK {
		t.Fatalf("ack = %d: %s", r.Code, r.Body.String())
	}
	first := decodeBody[alertResponse](t, r)
	if first.AcknowledgedAt == nil {
		t.Fatalf("acknowledgedAt = nil")
	}
	firstTime := *first.AcknowledgedAt

	// Repeat ack keeps the original time.
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/alerts/1/acknowledge", "")
	if r.Code != http.StatusOK {
		t.Fatalf("repeat ack = %d", r.Code)
	}
	second := decodeBody[alertResponse](t, r)
	if second.AcknowledgedAt == nil || !second.AcknowledgedAt.Equal(firstTime) {
		t.Fatalf("repeat ack changed time: %v vs %v", second.AcknowledgedAt, firstTime)
	}

	// Ack does not end the alert.
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].Status != alertStatusActive {
		t.Fatalf("ack ended alert: %+v", alerts[0])
	}

	// Ack unknown alert -> 404.
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/alerts/999/acknowledge", "")
	if r.Code != http.StatusNotFound {
		t.Fatalf("unknown alert = %d, want 404", r.Code)
	}
	// Ack unknown device -> 404.
	r = doRequest(t, h, http.MethodPost, "/v1/devices/ghost/alerts/1/acknowledge", "")
	if r.Code != http.StatusNotFound {
		t.Fatalf("unknown device = %d, want 404", r.Code)
	}
}

func TestAlertFilters(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)
	createRule(t, h, "gw", "r2", "humidity", 80, 60)

	// r1: trigger then recover.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":32}`)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":24}`)
	// r2: trigger, stays active.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"humidity":90}`)

	// Filter by rule.
	if alerts := listAlerts(t, h, "gw", "ruleId=r1"); len(alerts) != 1 || alerts[0].RuleID != "r1" {
		t.Fatalf("r1 alerts = %+v", alerts)
	}
	if alerts := listAlerts(t, h, "gw", "ruleId=r2"); len(alerts) != 1 || alerts[0].RuleID != "r2" {
		t.Fatalf("r2 alerts = %+v", alerts)
	}
	// Unknown rule filter -> 404.
	if r := doRequest(t, h, http.MethodGet, "/v1/devices/gw/alerts?ruleId=nope", ""); r.Code != http.StatusNotFound {
		t.Fatalf("unknown rule filter = %d, want 404", r.Code)
	}

	// Filter by status.
	if alerts := listAlerts(t, h, "gw", "status=active"); len(alerts) != 1 || alerts[0].RuleID != "r2" {
		t.Fatalf("active alerts = %+v", alerts)
	}
	if alerts := listAlerts(t, h, "gw", "status=ended"); len(alerts) != 1 || alerts[0].RuleID != "r1" {
		t.Fatalf("ended alerts = %+v", alerts)
	}
	// Bad status -> 400.
	if r := doRequest(t, h, http.MethodGet, "/v1/devices/gw/alerts?status=bogus", ""); r.Code != http.StatusBadRequest {
		t.Fatalf("bad status = %d, want 400", r.Code)
	}

	// Filter by acknowledged.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/alerts/1/acknowledge", "")
	if alerts := listAlerts(t, h, "gw", "acknowledged=true"); len(alerts) != 1 || alerts[0].ID != 1 {
		t.Fatalf("acknowledged alerts = %+v", alerts)
	}
	if alerts := listAlerts(t, h, "gw", "acknowledged=false"); len(alerts) != 1 || alerts[0].ID != 2 {
		t.Fatalf("unacknowledged alerts = %+v", alerts)
	}
	// Bad acknowledged -> 400.
	if r := doRequest(t, h, http.MethodGet, "/v1/devices/gw/alerts?acknowledged=maybe", ""); r.Code != http.StatusBadRequest {
		t.Fatalf("bad acknowledged = %d, want 400", r.Code)
	}

	// Combined filters.
	if alerts := listAlerts(t, h, "gw", "ruleId=r1&status=ended&acknowledged=true"); len(alerts) != 1 {
		t.Fatalf("combined filters = %+v", alerts)
	}
	if alerts := listAlerts(t, h, "gw", "ruleId=r1&status=active"); len(alerts) != 0 {
		t.Fatalf("combined filters = %+v", alerts)
	}
}

// --- rule update ends active alerts -----------------------------------------

func TestRuleUpdateEndsActiveAlert(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)

	// Trigger an active alert.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":32}`)
	// Update the rule: the active alert must end with rule_changed.
	doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":35,"recover":28,"version":1}`)

	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v, want 1", alerts)
	}
	if alerts[0].Status != alertStatusEnded || alerts[0].EndReason != endReasonRuleChanged {
		t.Fatalf("alert = %+v, want rule_changed end", alerts[0])
	}
	if alerts[0].EndedAt == nil {
		t.Fatalf("rule_changed alert missing endedAt")
	}
	if alerts[0].RecoverSequence != nil || alerts[0].RecoverValue != nil ||
		alerts[0].RecoverObservedAt != nil {
		t.Fatalf("rule_changed alert should not have recovery fields: %+v", alerts[0])
	}

	// After the update, no active alert: a value in the new mid-band does nothing.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":32}`)
	if alerts := listAlerts(t, h, "gw", "status=active"); len(alerts) != 0 {
		t.Fatalf("active alerts after update = %+v", alerts)
	}
	// A value at the new trigger opens a fresh alert.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":36}`)
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 2 || alerts[1].Status != alertStatusActive ||
		alerts[1].RuleVersion != 2 || alerts[1].TriggerSequence != 3 {
		t.Fatalf("post-update alert = %+v", alerts)
	}
}

func TestRuleUpdateWithNoActiveAlert(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)

	// Update with no active alert: version bumps, no alert changes.
	r := doRequest(t, h, http.MethodPut, "/v1/devices/gw/rules/r1",
		`{"trigger":35,"recover":28,"version":1}`)
	if r.Code != http.StatusOK {
		t.Fatalf("update = %d", r.Code)
	}
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 0 {
		t.Fatalf("alerts = %+v", alerts)
	}
}

// --- replay participation ---------------------------------------------------

func TestReplayTriggersAndRecoversInBatch(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)

	// One batch: trigger then recover. Both changes must be queryable.
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b1",
			sample("e1", "2024-01-02T10:00:00Z", `{"temperature":40}`),
			sample("e2", "2024-01-02T10:05:00Z", `{"temperature":20}`),
		))
	if r.Code != http.StatusAccepted {
		t.Fatalf("replay = %d: %s", r.Code, r.Body.String())
	}
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v, want 1", alerts)
	}
	if alerts[0].Status != alertStatusEnded || alerts[0].EndReason != endReasonRecovered {
		t.Fatalf("alert = %+v, want recovered", alerts[0])
	}
	if alerts[0].TriggerSequence != 1 || alerts[0].TriggerValue != 40 ||
		alerts[0].RecoverSequence == nil || *alerts[0].RecoverSequence != 2 ||
		*alerts[0].RecoverValue != 20 {
		t.Fatalf("alert fields = %+v", alerts[0])
	}
	// Observed times come from the samples, not server receive time.
	if !alerts[0].TriggerObservedAt.Equal(time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)) ||
		!alerts[0].RecoverObservedAt.Equal(time.Date(2024, 1, 2, 10, 5, 0, 0, time.UTC)) {
		t.Fatalf("observed times = %s / %s", alerts[0].TriggerObservedAt, alerts[0].RecoverObservedAt)
	}
}

func TestReplayJudgedByReceiveOrderNotObservedTime(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)

	// Observed times are out of order; receive order decides. The first sample
	// (later observed time) triggers; the second (earlier observed time) recovers.
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b1",
			sample("e1", "2024-01-03T10:00:00Z", `{"temperature":40}`),
			sample("e2", "2024-01-01T10:00:00Z", `{"temperature":20}`),
		))
	if r.Code != http.StatusAccepted {
		t.Fatalf("replay = %d", r.Code)
	}
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].Status != alertStatusEnded {
		t.Fatalf("alerts = %+v", alerts)
	}
	// The trigger is at receive sequence 1 (e1), recovery at sequence 2 (e2).
	if alerts[0].TriggerSequence != 1 || alerts[0].RecoverSequence == nil ||
		*alerts[0].RecoverSequence != 2 {
		t.Fatalf("alert = %+v", alerts[0])
	}
}

func TestReplayDuplicatesDoNotChangeAlerts(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)

	// First batch: trigger.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b1", sample("e1", "2024-01-02T10:00:00Z", `{"temperature":40}`)))
	// Replay the same batch: duplicate, must not refresh or re-trigger.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b1", sample("e1", "2024-01-02T10:00:00Z", `{"temperature":40}`)))
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].Status != alertStatusActive ||
		alerts[0].TriggerSequence != 1 {
		t.Fatalf("duplicate batch changed alerts: %+v", alerts)
	}

	// A duplicate event in a mixed batch must not judge either.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b2",
			sample("e1", "2024-01-02T10:00:00Z", `{"temperature":40}`),
			sample("e2", "2024-01-02T11:00:00Z", `{"temperature":20}`),
		))
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].Status != alertStatusEnded {
		t.Fatalf("mixed duplicate batch = %+v", alerts)
	}
	if alerts[0].RecoverSequence == nil || *alerts[0].RecoverSequence != 2 {
		t.Fatalf("recover sequence = %v, want 2", alerts[0].RecoverSequence)
	}
}

func TestReplayConflictLeavesNoAlertChanges(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)

	// First batch triggers an alert.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b1", sample("e1", "2024-01-02T10:00:00Z", `{"temperature":40}`)))
	// Conflicting batch: e1 already exists with a different value -> 409 and no
	// alert changes.
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b2", sample("e1", "2024-01-02T10:00:00Z", `{"temperature":99}`)))
	if r.Code != http.StatusConflict {
		t.Fatalf("conflict = %d, want 409", r.Code)
	}
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].Status != alertStatusActive {
		t.Fatalf("conflict changed alerts: %+v", alerts)
	}
}

// --- rules only judge new samples -------------------------------------------

func TestRuleDoesNotBackfillHistory(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")

	// History exists before the rule.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":40}`)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":40}`)

	// Creating the rule must not backfill the high history.
	createRule(t, h, "gw", "r1", "temperature", 30, 25)
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 0 {
		t.Fatalf("rule backfilled history: %+v", alerts)
	}

	// A new high sample triggers.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":40}`)
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 1 || alerts[0].TriggerSequence != 3 {
		t.Fatalf("alerts = %+v", alerts)
	}
}

// --- multiple rules per device ----------------------------------------------

func TestMultipleRulesPerDevice(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "temp", "temperature", 30, 25)
	createRule(t, h, "gw", "hum", "humidity", 80, 60)

	// One sample can trigger both rules.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry",
		`{"temperature":32,"humidity":90}`)
	alerts := listAlerts(t, h, "gw", "")
	if len(alerts) != 2 {
		t.Fatalf("alerts = %+v, want 2", alerts)
	}
	// Both alerts at the same sequence.
	for _, alert := range alerts {
		if alert.TriggerSequence != 1 || alert.Status != alertStatusActive {
			t.Fatalf("alert = %+v", alert)
		}
	}

	// Recovering one does not affect the other.
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry",
		`{"temperature":24,"humidity":90}`)
	alerts = listAlerts(t, h, "gw", "")
	if len(alerts) != 2 {
		t.Fatalf("alerts = %+v", alerts)
	}
	for _, alert := range alerts {
		if alert.RuleID == "temp" && alert.Status != alertStatusEnded {
			t.Fatalf("temp alert = %+v", alert)
		}
		if alert.RuleID == "hum" && alert.Status != alertStatusActive {
			t.Fatalf("hum alert = %+v", alert)
		}
	}
}

// --- JSON round-trip sanity -------------------------------------------------

func TestAlertJSONFields(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":32}`)

	raw := doRequest(t, h, http.MethodGet, "/v1/devices/gw/alerts", "").Body.String()
	// Verify the JSON has the expected field names.
	for _, field := range []string{
		`"id":1`, `"ruleId":"r1"`, `"ruleVersion":1`, `"metric":"temperature"`,
		`"trigger":30`, `"recover":25`, `"status":"active"`,
		`"triggerSequence":1`, `"triggerValue":32`, `"triggerObservedAt":`,
	} {
		if !strings.Contains(raw, field) {
			t.Fatalf("JSON missing %s: %s", field, raw)
		}
	}
	// Ended/recovery fields are omitted when nil.
	if strings.Contains(raw, "recoverSequence") || strings.Contains(raw, "endReason") {
		t.Fatalf("active alert JSON should omit ended fields: %s", raw)
	}

	// Ensure the response is valid JSON.
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}
