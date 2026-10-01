package fleet

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

var (
	ErrRuleExists      = errors.New("rule already exists")
	ErrRuleNotFound    = errors.New("rule not found")
	ErrAlertNotFound   = errors.New("alert not found")
	ErrVersionConflict = errors.New("rule version conflict")
)

// Close reasons recorded on an alert when it ends.
const (
	// CloseReasonRecovered marks a natural recovery: a sample at or below the
	// recovery threshold ended the alert.
	CloseReasonRecovered = "recovered"
	// CloseReasonRuleChanged marks an administrative close: the rule was
	// updated, disabled or re-enabled, which ends the old version's alert.
	CloseReasonRuleChanged = "rule-changed"
)

// Rule is a per-device alerting rule. RuleID is unique within its device and
// immutable together with the device assignment. Version starts at 1 and
// increments on every successful modification; writers must present the
// current version, so concurrent updates on one version succeed exactly once.
type Rule struct {
	RuleID    string    `json:"ruleId"`
	Metric    string    `json:"metric"`
	Trigger   float64   `json:"trigger"`
	Recover   float64   `json:"recover"`
	Enabled   bool      `json:"enabled"`
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// AlertSample captures the sample that triggered or recovered an alert.
type AlertSample struct {
	Sequence   int64     `json:"sequence"`
	Value      float64   `json:"value"`
	ObservedAt time.Time `json:"observedAt"`
}

// Alert is one open or resolved alert. It pins the rule version and the
// metric/thresholds in effect when it triggered, so later rule updates never
// rewrite history.
type Alert struct {
	AlertID        int64        `json:"alertId"`
	RuleID         string       `json:"ruleId"`
	RuleVersion    int64        `json:"ruleVersion"`
	Metric         string       `json:"metric"`
	Trigger        float64      `json:"trigger"`
	Recover        float64      `json:"recover"`
	TriggeredBy    AlertSample  `json:"triggeredBy"`
	Closed         bool         `json:"closed"`
	CloseReason    string       `json:"closeReason,omitempty"`
	ClosedAt       *time.Time   `json:"closedAt,omitempty"`
	RecoveredBy    *AlertSample `json:"recoveredBy,omitempty"`
	AcknowledgedAt *time.Time   `json:"acknowledgedAt,omitempty"`
}

// AlertClose records how an open alert ended: naturally (Sample carries the
// recovery sample) or because the rule changed (ClosedAt is the server time).
type AlertClose struct {
	AlertID  int64        `json:"alertId"`
	Reason   string       `json:"reason"`
	ClosedAt time.Time    `json:"closedAt"`
	Sample   *AlertSample `json:"sample,omitempty"`
}

// AlertFilter narrows an alert listing. Nil pointers mean "no constraint".
type AlertFilter struct {
	RuleID       string
	Closed       *bool
	Acknowledged *bool
}

func cloneAlert(alert Alert) Alert {
	if alert.ClosedAt != nil {
		closedAt := *alert.ClosedAt
		alert.ClosedAt = &closedAt
	}
	if alert.RecoveredBy != nil {
		recoveredBy := *alert.RecoveredBy
		alert.RecoveredBy = &recoveredBy
	}
	if alert.AcknowledgedAt != nil {
		ackedAt := *alert.AcknowledgedAt
		alert.AcknowledgedAt = &ackedAt
	}
	return alert
}

// --- rule CRUD --------------------------------------------------------------

// CreateRule registers a new enabled rule at version 1.
func (s *Store) CreateRule(deviceID, ruleID, metric string, trigger, recoverThreshold float64) (Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[deviceID]
	if !ok {
		return Rule{}, ErrDeviceNotFound
	}
	if _, exists := state.rules[ruleID]; exists {
		return Rule{}, ErrRuleExists
	}
	now := s.now().UTC()
	rule := Rule{
		RuleID: ruleID, Metric: metric, Trigger: trigger, Recover: recoverThreshold,
		Enabled: true, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if s.wal != nil {
		if err := s.wal.appendRecord(recRule, walRule{DeviceID: deviceID, Rule: rule}); err != nil {
			return Rule{}, storageUnavailable(err)
		}
	}
	state.rules[ruleID] = &rule
	return rule, nil
}

// UpdateRule replaces the metric and thresholds of a rule. The presented
// version must match the current one; any active alert of the old version is
// closed with reason rule-changed and evaluation restarts with no active
// alert.
func (s *Store) UpdateRule(deviceID, ruleID, metric string, trigger, recoverThreshold float64, version int64) (Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[deviceID]
	if !ok {
		return Rule{}, ErrDeviceNotFound
	}
	rule, ok := state.rules[ruleID]
	if !ok {
		return Rule{}, ErrRuleNotFound
	}
	if rule.Version != version {
		return Rule{}, ErrVersionConflict
	}
	now := s.now().UTC()
	updated := Rule{
		RuleID: ruleID, Metric: metric, Trigger: trigger, Recover: recoverThreshold,
		Enabled: rule.Enabled, Version: rule.Version + 1,
		CreatedAt: rule.CreatedAt, UpdatedAt: now,
	}
	return s.commitRuleChange(state, deviceID, rule, updated, now)
}

// SetRuleEnabled disables or re-enables a rule. Like an update, it requires
// the current version, bumps it, and closes any active alert of the old
// version. While disabled the rule accepts telemetry but never evaluates it.
func (s *Store) SetRuleEnabled(deviceID, ruleID string, enabled bool, version int64) (Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[deviceID]
	if !ok {
		return Rule{}, ErrDeviceNotFound
	}
	rule, ok := state.rules[ruleID]
	if !ok {
		return Rule{}, ErrRuleNotFound
	}
	if rule.Version != version {
		return Rule{}, ErrVersionConflict
	}
	now := s.now().UTC()
	updated := *rule
	updated.Enabled = enabled
	updated.Version = rule.Version + 1
	updated.UpdatedAt = now
	return s.commitRuleChange(state, deviceID, rule, updated, now)
}

// commitRuleChange persists and applies a rule mutation together with the
// administrative close of the old version's active alert, as one commit unit.
func (s *Store) commitRuleChange(state *deviceState, deviceID string, rule *Rule, updated Rule, now time.Time) (Rule, error) {
	var closed *AlertClose
	if activeID, has := state.active[updated.RuleID]; has {
		closed = &AlertClose{AlertID: activeID, Reason: CloseReasonRuleChanged, ClosedAt: now}
	}
	if s.wal != nil {
		if err := s.wal.appendRecord(recRule, walRule{DeviceID: deviceID, Rule: updated, Closed: closed}); err != nil {
			return Rule{}, storageUnavailable(err)
		}
	}
	*rule = updated
	if closed != nil {
		state.applyAlertClose(*closed)
	}
	return updated, nil
}

func (s *Store) GetRule(deviceID, ruleID string) (Rule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.devices[deviceID]
	if !ok {
		return Rule{}, ErrDeviceNotFound
	}
	rule, ok := state.rules[ruleID]
	if !ok {
		return Rule{}, ErrRuleNotFound
	}
	return *rule, nil
}

func (s *Store) ListRules(deviceID string) ([]Rule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.devices[deviceID]
	if !ok {
		return nil, ErrDeviceNotFound
	}
	rules := make([]Rule, 0, len(state.rules))
	for _, rule := range state.rules {
		rules = append(rules, *rule)
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].RuleID < rules[j].RuleID })
	return rules, nil
}

// --- alerts -----------------------------------------------------------------

func (s *Store) GetAlert(deviceID string, alertID int64) (Alert, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.devices[deviceID]
	if !ok {
		return Alert{}, ErrDeviceNotFound
	}
	if alertID < 1 || alertID > int64(len(state.alerts)) {
		return Alert{}, ErrAlertNotFound
	}
	return cloneAlert(state.alerts[alertID-1]), nil
}

func (s *Store) ListAlerts(deviceID string, filter AlertFilter) ([]Alert, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.devices[deviceID]
	if !ok {
		return nil, ErrDeviceNotFound
	}
	alerts := make([]Alert, 0, len(state.alerts))
	for _, alert := range state.alerts {
		if filter.RuleID != "" && alert.RuleID != filter.RuleID {
			continue
		}
		if filter.Closed != nil && alert.Closed != *filter.Closed {
			continue
		}
		if filter.Acknowledged != nil && (alert.AcknowledgedAt != nil) != *filter.Acknowledged {
			continue
		}
		alerts = append(alerts, cloneAlert(alert))
	}
	return alerts, nil
}

// AckAlert acknowledges an alert. The first acknowledgement records the server
// time; repeating it returns the original time unchanged. Acknowledging never
// closes an alert and never blocks later recovery or re-triggering.
func (s *Store) AckAlert(deviceID string, alertID int64) (Alert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[deviceID]
	if !ok {
		return Alert{}, ErrDeviceNotFound
	}
	if alertID < 1 || alertID > int64(len(state.alerts)) {
		return Alert{}, ErrAlertNotFound
	}
	alert := &state.alerts[alertID-1]
	if alert.AcknowledgedAt != nil {
		return cloneAlert(*alert), nil
	}
	now := s.now().UTC()
	if s.wal != nil {
		if err := s.wal.appendRecord(recAck, walAck{DeviceID: deviceID, AlertID: alertID, AckedAt: now}); err != nil {
			return Alert{}, storageUnavailable(err)
		}
	}
	alert.AcknowledgedAt = &now
	return cloneAlert(*alert), nil
}

// --- evaluation -------------------------------------------------------------

// alertPlan accumulates the alert transitions caused by the accepted samples
// of one commit unit (a single telemetry write or one replay batch) without
// mutating the store. The plan is applied only after the durable commit
// point, so a failed write leaves no alert changes behind.
type alertPlan struct {
	active map[string]int64 // ruleID -> alertID, seeded from the device state
	nextID int64
	opened []Alert
	closed []AlertClose
}

func (state *deviceState) newAlertPlan() *alertPlan {
	active := make(map[string]int64, len(state.active))
	for ruleID, alertID := range state.active {
		active[ruleID] = alertID
	}
	return &alertPlan{active: active, nextID: state.nextAlert}
}

// observe evaluates one accepted sample against every enabled rule, in receive
// order. An active alert always belongs to the rule's current version (any
// rule change closes it first), so the rule's current thresholds apply.
func (p *alertPlan) observe(state *deviceState, sequence int64, values map[string]float64, observedAt, now time.Time) {
	if len(state.rules) == 0 {
		return
	}
	ruleIDs := make([]string, 0, len(state.rules))
	for ruleID := range state.rules {
		ruleIDs = append(ruleIDs, ruleID)
	}
	sort.Strings(ruleIDs) // deterministic transition order within one sample
	for _, ruleID := range ruleIDs {
		rule := state.rules[ruleID]
		if !rule.Enabled {
			continue
		}
		value, ok := values[rule.Metric]
		if !ok {
			continue // metric absent: keep the current state
		}
		if activeID, has := p.active[ruleID]; has {
			if value <= rule.Recover {
				sample := AlertSample{Sequence: sequence, Value: value, ObservedAt: observedAt}
				p.closed = append(p.closed, AlertClose{
					AlertID: activeID, Reason: CloseReasonRecovered, ClosedAt: now, Sample: &sample,
				})
				delete(p.active, ruleID)
			}
			continue // still above the recovery threshold: keep the same alert
		}
		if value >= rule.Trigger {
			alertID := p.nextID
			p.nextID++
			p.opened = append(p.opened, Alert{
				AlertID: alertID, RuleID: ruleID, RuleVersion: rule.Version,
				Metric: rule.Metric, Trigger: rule.Trigger, Recover: rule.Recover,
				TriggeredBy: AlertSample{Sequence: sequence, Value: value, ObservedAt: observedAt},
			})
			p.active[ruleID] = alertID
		}
	}
}

// applyAlertPlan commits planned transitions to the device state. It runs only
// after the durable commit point of the record that carries them.
func (state *deviceState) applyAlertPlan(p *alertPlan) {
	for i := range p.opened {
		state.alerts = append(state.alerts, p.opened[i])
		state.active[p.opened[i].RuleID] = p.opened[i].AlertID
	}
	for _, close := range p.closed {
		state.applyAlertClose(close)
	}
	state.nextAlert = p.nextID
}

func (state *deviceState) applyAlertClose(close AlertClose) {
	alert := &state.alerts[close.AlertID-1]
	alert.Closed = true
	alert.CloseReason = close.Reason
	closedAt := close.ClosedAt
	alert.ClosedAt = &closedAt
	alert.RecoveredBy = close.Sample
	delete(state.active, alert.RuleID)
}

func finiteValue(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// --- recovery helpers (validating) ------------------------------------------

// applyAlertTransitions replays the alert changes recorded alongside one
// committed sample or batch, rejecting anything inconsistent with the rules
// and alerts rebuilt so far. Recovery applies recorded transitions instead of
// re-evaluating samples, so a restart can never re-trigger an alert.
func applyAlertTransitions(state *deviceState, opened []Alert, closed []AlertClose) error {
	for _, alert := range opened {
		if err := applyOpenedAlert(state, alert); err != nil {
			return err
		}
	}
	for _, close := range closed {
		if err := applyClosedAlert(state, close); err != nil {
			return err
		}
	}
	return nil
}

func applyOpenedAlert(state *deviceState, alert Alert) error {
	rule, ok := state.rules[alert.RuleID]
	if !ok {
		return fmt.Errorf("alert %d references unknown rule %q", alert.AlertID, alert.RuleID)
	}
	if alert.AlertID != state.nextAlert {
		return fmt.Errorf("alert id gap: got %d, want %d", alert.AlertID, state.nextAlert)
	}
	if alert.RuleVersion != rule.Version {
		return fmt.Errorf("alert %d pins rule %q version %d, current is %d", alert.AlertID, alert.RuleID, alert.RuleVersion, rule.Version)
	}
	if _, has := state.active[alert.RuleID]; has {
		return fmt.Errorf("rule %q already has an active alert", alert.RuleID)
	}
	if alert.Metric == "" || alert.Metric != rule.Metric ||
		alert.Trigger != rule.Trigger || alert.Recover != rule.Recover {
		return fmt.Errorf("alert %d disagrees with rule %q", alert.AlertID, alert.RuleID)
	}
	if alert.TriggeredBy.Sequence < 1 || alert.TriggeredBy.ObservedAt.IsZero() ||
		!finiteValue(alert.TriggeredBy.Value) {
		return fmt.Errorf("alert %d has an invalid trigger sample", alert.AlertID)
	}
	if alert.Closed || alert.CloseReason != "" || alert.ClosedAt != nil ||
		alert.RecoveredBy != nil || alert.AcknowledgedAt != nil {
		return fmt.Errorf("alert %d opens in a closed or acknowledged state", alert.AlertID)
	}
	state.alerts = append(state.alerts, cloneAlert(alert))
	state.active[alert.RuleID] = alert.AlertID
	state.nextAlert++
	return nil
}

func applyClosedAlert(state *deviceState, close AlertClose) error {
	if close.AlertID < 1 || close.AlertID > int64(len(state.alerts)) {
		return fmt.Errorf("close references unknown alert %d", close.AlertID)
	}
	alert := &state.alerts[close.AlertID-1]
	if alert.Closed {
		return fmt.Errorf("alert %d is closed twice", close.AlertID)
	}
	if state.active[alert.RuleID] != close.AlertID {
		return fmt.Errorf("alert %d is not the active alert of rule %q", close.AlertID, alert.RuleID)
	}
	switch close.Reason {
	case CloseReasonRecovered:
		if close.Sample == nil || close.Sample.Sequence < 1 ||
			close.Sample.ObservedAt.IsZero() || !finiteValue(close.Sample.Value) {
			return fmt.Errorf("alert %d recovery is missing its recovery sample", close.AlertID)
		}
	case CloseReasonRuleChanged:
		if close.Sample != nil {
			return fmt.Errorf("alert %d rule-changed close must not carry a sample", close.AlertID)
		}
	default:
		return fmt.Errorf("alert %d has unknown close reason %q", close.AlertID, close.Reason)
	}
	if close.ClosedAt.IsZero() {
		return fmt.Errorf("alert %d close is missing its server time", close.AlertID)
	}
	state.applyAlertClose(close)
	return nil
}
