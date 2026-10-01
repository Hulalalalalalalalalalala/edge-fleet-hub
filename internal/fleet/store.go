package fleet

import (
	"errors"
	"sort"
	"sync"
	"time"
)

var (
	ErrDeviceNotFound  = errors.New("device not found")
	ErrBatchConflict   = errors.New("batch conflicts with stored events")
	ErrRuleNotFound    = errors.New("rule not found")
	ErrAlertNotFound   = errors.New("alert not found")
	ErrRuleConflict    = errors.New("rule already exists")
	ErrVersionConflict = errors.New("rule version conflict")
	ErrInvalidRule     = errors.New("invalid rule")
)

// Rule is a per-device threshold rule. It judges only samples accepted while
// enabled; it never backfills history. Device and ID are immutable after
// creation. Version starts at 1 and bumps on every successful update.
type Rule struct {
	ID        string    `json:"id"`
	Metric    string    `json:"metric"`
	Trigger   float64   `json:"trigger"`
	Recover   float64   `json:"recover"`
	Enabled   bool      `json:"enabled"`
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Alert is one threshold crossing. It retains the rule version and the
// metric/thresholds in force when it fired. A natural recovery records the
// recovery sample's sequence, value and observed time; an end caused by a rule
// update records the server time and reason instead.
type Alert struct {
	ID                int64      `json:"id"`
	RuleID            string     `json:"ruleId"`
	RuleVersion       int64      `json:"ruleVersion"`
	Metric            string     `json:"metric"`
	Trigger           float64    `json:"trigger"`
	Recover           float64    `json:"recover"`
	Status            string     `json:"status"` // "active" | "ended"
	TriggerSequence   int64      `json:"triggerSequence"`
	TriggerValue      float64    `json:"triggerValue"`
	TriggerObservedAt time.Time  `json:"triggerObservedAt"`
	RecoverSequence   *int64     `json:"recoverSequence,omitempty"`
	RecoverValue      *float64   `json:"recoverValue,omitempty"`
	RecoverObservedAt *time.Time `json:"recoverObservedAt,omitempty"`
	EndedAt           *time.Time `json:"endedAt,omitempty"`
	EndReason         string     `json:"endReason,omitempty"` // "recovered" | "rule_changed"
	AcknowledgedAt    *time.Time `json:"acknowledgedAt,omitempty"`
}

const (
	alertStatusActive    = "active"
	alertStatusEnded     = "ended"
	endReasonRecovered   = "recovered"
	endReasonRuleChanged = "rule_changed"
)

// ruleState pairs a rule with the id of its currently active alert (0 when
// none).
type ruleState struct {
	rule          Rule
	activeAlertID int64
}

type Device struct {
	ID            string             `json:"id"`
	Site          string             `json:"site"`
	RegisteredAt  time.Time          `json:"registeredAt"`
	LastSeenAt    time.Time          `json:"lastSeenAt"`
	LastTelemetry map[string]float64 `json:"lastTelemetry,omitempty"`
}

// Event is a single telemetry sample retained in a device's history.
type Event struct {
	Sequence   int64              `json:"sequence"`
	ObservedAt time.Time          `json:"observedAt"`
	Values     map[string]float64 `json:"values"`
}

// Sample is one replayed sample, decoded from the replay request body.
type Sample struct {
	EventID    string
	ObservedAt time.Time
	Values     map[string]float64
}

// ReplayReceipt summarises the outcome of a replay batch.
type ReplayReceipt struct {
	BatchID      string         `json:"batchId"`
	NewCount     int            `json:"newCount"`
	Duplicate    int            `json:"duplicateCount"`
	SampleStatus []SampleStatus `json:"samples"`
}

// SampleStatus reports the history sequence assigned to (or reused by) a sample.
type SampleStatus struct {
	EventID   string `json:"eventId"`
	Sequence  int64  `json:"sequence"`
	Duplicate bool   `json:"duplicate"`
}

// HistoryFilter narrows a history query by observed-time range. Zero times mean
// the bound is open.
type HistoryFilter struct {
	From time.Time
	To   time.Time
}

// StoredBatch records the ordered samples an accepted batchId committed along
// with the receipt returned on first acceptance, so a repeated batchId can be
// checked for identical content and replayed with the original receipt.
type storedBatch struct {
	samples []Sample
	receipt ReplayReceipt
}

type deviceState struct {
	device  Device
	events  []Event          // history ordered by sequence (sequence = index + 1)
	byEvent map[string]int64 // eventId -> sequence
	batches map[string]storedBatch
	rules   map[string]*ruleState
	alerts  []*Alert // per-device, ordered by alert id (id = index + 1)
	config  *configState
	tasks   []*taskState          // per-device, ordered by task id (id = index + 1)
	tasksByRequest map[string]*taskState // requestId -> first task
}

type Store struct {
	mu      sync.RWMutex
	devices map[string]*deviceState
	now     func() time.Time

	// wal is nil in memory mode. When set, every mutation is appended and
	// synced here before in-memory state changes or success is returned.
	wal walSink
	iid []byte // data-directory instance id bound into history cursors
}

func NewStore() *Store {
	return &Store{devices: make(map[string]*deviceState), now: time.Now}
}

// instanceID identifies the backing data directory; cursors are scoped to it.
// It is nil in memory mode.
func (s *Store) instanceID() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.iid
}

// Exists reports whether a device is registered.
func (s *Store) Exists(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.devices[id]
	return ok
}

func (s *Store) Register(id, site string) (Device, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state, ok := s.devices[id]; ok {
		return cloneDevice(state.device), false, nil
	}
	now := s.now().UTC()
	device := Device{ID: id, Site: site, RegisteredAt: now, LastSeenAt: now}
	if s.wal != nil {
		if err := s.wal.appendRecord(recRegister, walRegister{
			ID: id, Site: site, RegisteredAt: now, LastSeenAt: now,
		}); err != nil {
			return Device{}, false, storageUnavailable(err)
		}
	}
	s.devices[id] = &deviceState{
		device:  device,
		byEvent: make(map[string]int64),
		batches: make(map[string]storedBatch),
		rules:   make(map[string]*ruleState),
	}
	return cloneDevice(device), true, nil
}

// RecordTelemetry stores a live telemetry sample in the device history. The
// sample has no caller-supplied eventId, so it is never a replay duplicate; its
// observed time is the server receive time.
func (s *Store) RecordTelemetry(id string, values map[string]float64) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[id]
	if !ok {
		return Device{}, ErrDeviceNotFound
	}
	now := s.now().UTC()
	sequence := int64(len(state.events)) + 1
	eval := newAlertEvaluator(state)
	eval.evaluate(sequence, now, values)
	if s.wal != nil {
		if err := s.wal.appendRecord(recTelemetry, walTelemetry{
			DeviceID:   id,
			Sequence:   sequence,
			ObservedAt: now,
			Values:     values,
			Alerts:     eval.created,
			Ended:      eval.ended,
		}); err != nil {
			return Device{}, storageUnavailable(err)
		}
	}
	state.events = append(state.events, Event{
		Sequence:   sequence,
		ObservedAt: now,
		Values:     cloneTelemetry(values),
	})
	state.device.LastSeenAt = now
	state.device.LastTelemetry = cloneTelemetry(values)
	eval.commit()
	return cloneDevice(state.device), nil
}

// Replay validates and commits a batch atomically. repeat is true when the
// batchId was already committed with identical content and the first receipt
// is returned unchanged. It returns ErrDeviceNotFound for an unknown device and
// ErrBatchConflict when any sample clashes with an existing eventId or the
// batchId was previously committed with different ordered content; in either
// conflict case no state changes.
func (s *Store) Replay(id, batchID string, samples []Sample) (receipt ReplayReceipt, repeat bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[id]
	if !ok {
		return ReplayReceipt{}, false, ErrDeviceNotFound
	}

	if previous, exists := state.batches[batchID]; exists {
		if !sameOrderedSamples(previous.samples, samples) {
			return ReplayReceipt{}, false, ErrBatchConflict
		}
		return cloneReceipt(previous.receipt), true, nil
	}

	// Validate every sample against committed history before appending so a
	// conflict rolls back without leaving sequences, events or receipts.
	type plan struct {
		sample   Sample
		sequence int64
		dup      bool
	}
	planned := make([]plan, len(samples))
	nextSequence := int64(len(state.events)) + 1
	for i, sample := range samples {
		if sequence, seen := state.byEvent[sample.EventID]; seen {
			existing := state.events[sequence-1]
			if !sameInstant(existing.ObservedAt, sample.ObservedAt) ||
				!sameValues(existing.Values, sample.Values) {
				return ReplayReceipt{}, false, ErrBatchConflict
			}
			planned[i] = plan{sample, sequence, true}
			continue
		}
		planned[i] = plan{sample, nextSequence, false}
		nextSequence++
	}

	// Build the receipt from the plan: only samples that matched a committed
	// event before this batch are duplicates.
	receipt = ReplayReceipt{BatchID: batchID, SampleStatus: make([]SampleStatus, 0, len(samples))}
	for _, entry := range planned {
		receipt.SampleStatus = append(receipt.SampleStatus, SampleStatus{
			EventID:   entry.sample.EventID,
			Sequence:  entry.sequence,
			Duplicate: entry.dup,
		})
		if entry.dup {
			receipt.Duplicate++
		} else {
			receipt.NewCount++
		}
	}

	// Evaluate the batch's new samples against the device's enabled rules, in
	// array order (which is receive-sequence order). Duplicates never judge,
	// create, end or refresh alerts. A trigger and a recovery inside one batch
	// both produce alert changes, all committed together with the batch.
	eval := newAlertEvaluator(state)
	for _, entry := range planned {
		if entry.dup {
			continue
		}
		eval.evaluate(entry.sequence, entry.sample.ObservedAt, entry.sample.Values)
	}

	// Durable commit point: the whole batch is one record, synced before any
	// in-memory state changes and before success is reported. A failure here
	// leaves sequences, history, receipts, device state and alert changes
	// untouched.
	lastNew := -1
	for i, entry := range planned {
		if !entry.dup {
			lastNew = i
		}
	}
	var committedAt time.Time
	if s.wal != nil {
		record := walReplay{
			DeviceID: id,
			BatchID:  batchID,
			Samples:  make([]walSample, len(samples)),
			Receipt:  receipt,
			Alerts:   eval.created,
			Ended:    eval.ended,
		}
		for i, sample := range samples {
			record.Samples[i] = walSample{
				EventID:    sample.EventID,
				ObservedAt: sample.ObservedAt,
				Values:     sample.Values,
			}
		}
		if lastNew >= 0 {
			committedAt = s.now().UTC()
			record.LastSeenAt = &committedAt
			record.LastTelemetry = cloneTelemetry(samples[lastNew].Values)
		}
		if err := s.wal.appendRecord(recReplay, record); err != nil {
			return ReplayReceipt{}, false, storageUnavailable(err)
		}
	}

	// Commit: append new samples in array order.
	for _, entry := range planned {
		if entry.dup {
			continue
		}
		state.events = append(state.events, Event{
			Sequence:   entry.sequence,
			ObservedAt: entry.sample.ObservedAt,
			Values:     cloneTelemetry(entry.sample.Values),
		})
		state.byEvent[entry.sample.EventID] = entry.sequence
	}

	committed := make([]Sample, len(samples))
	copy(committed, samples)
	for i := range committed {
		committed[i].Values = cloneTelemetry(samples[i].Values)
	}
	state.batches[batchID] = storedBatch{samples: committed, receipt: cloneReceipt(receipt)}

	if lastNew >= 0 {
		if s.wal == nil {
			committedAt = s.now().UTC()
		}
		state.device.LastSeenAt = committedAt
		state.device.LastTelemetry = cloneTelemetry(samples[lastNew].Values)
	}
	eval.commit()
	return receipt, false, nil
}

func cloneReceipt(receipt ReplayReceipt) ReplayReceipt {
	receipt.SampleStatus = append([]SampleStatus(nil), receipt.SampleStatus...)
	return receipt
}

// --- rules and alerts -------------------------------------------------------

// ruleUpdate carries the mutable fields of a rule update. A nil pointer leaves
// the field unchanged.
type ruleUpdate struct {
	Metric  *string
	Trigger *float64
	Recover *float64
	Enabled *bool
}

// AlertFilter narrows an alert listing. An empty string or nil pointer leaves
// that dimension unfiltered.
type AlertFilter struct {
	RuleID       string
	Status       string
	Acknowledged *bool
}

// CreateRule registers a new enabled rule on a device. Version starts at 1.
// It returns ErrDeviceNotFound for an unknown device and ErrRuleConflict when
// the device already has a rule with that id.
func (s *Store) CreateRule(id, ruleID, metric string, trigger, recover float64) (Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[id]
	if !ok {
		return Rule{}, ErrDeviceNotFound
	}
	if _, exists := state.rules[ruleID]; exists {
		return Rule{}, ErrRuleConflict
	}
	now := s.now().UTC()
	rule := Rule{
		ID: ruleID, Metric: metric, Trigger: trigger, Recover: recover,
		Enabled: true, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if s.wal != nil {
		if err := s.wal.appendRecord(recRule, walRule{DeviceID: id, Rule: rule}); err != nil {
			return Rule{}, storageUnavailable(err)
		}
	}
	state.rules[ruleID] = &ruleState{rule: rule}
	return cloneRule(rule), nil
}

// UpdateRule applies a version-checked optimistic update. A version mismatch
// returns ErrVersionConflict with state unchanged. Every successful update
// bumps the version by one and immediately ends the rule's active alert (if
// any) with reason rule_changed; the rule starts again with no active alert.
func (s *Store) UpdateRule(id, ruleID string, update ruleUpdate, version int64) (Rule, []*Alert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[id]
	if !ok {
		return Rule{}, nil, ErrDeviceNotFound
	}
	rs, ok := state.rules[ruleID]
	if !ok {
		return Rule{}, nil, ErrRuleNotFound
	}
	if rs.rule.Version != version {
		return Rule{}, nil, ErrVersionConflict
	}

	updated := rs.rule
	if update.Metric != nil {
		updated.Metric = *update.Metric
	}
	if update.Trigger != nil {
		updated.Trigger = *update.Trigger
	}
	if update.Recover != nil {
		updated.Recover = *update.Recover
	}
	if update.Enabled != nil {
		updated.Enabled = *update.Enabled
	}
	if updated.Recover >= updated.Trigger {
		return Rule{}, nil, ErrInvalidRule
	}

	// End the active alert of the old version before publishing the new one.
	var ended []*Alert
	if rs.activeAlertID != 0 {
		alert := cloneAlert(*state.alerts[rs.activeAlertID-1])
		alert.Status = alertStatusEnded
		alert.EndReason = endReasonRuleChanged
		now := s.now().UTC()
		alert.EndedAt = &now
		ended = append(ended, &alert)
		rs.activeAlertID = 0
	}

	updated.Version++
	updated.UpdatedAt = s.now().UTC()

	if s.wal != nil {
		if err := s.wal.appendRecord(recRule, walRule{DeviceID: id, Rule: updated, Ended: ended}); err != nil {
			return Rule{}, nil, storageUnavailable(err)
		}
	}
	rs.rule = updated
	for _, alert := range ended {
		state.alerts[alert.ID-1] = alert
	}
	return cloneRule(updated), cloneAlerts(ended), nil
}

// GetRule returns one rule.
func (s *Store) GetRule(id, ruleID string) (Rule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.devices[id]
	if !ok {
		return Rule{}, ErrDeviceNotFound
	}
	rs, ok := state.rules[ruleID]
	if !ok {
		return Rule{}, ErrRuleNotFound
	}
	return cloneRule(rs.rule), nil
}

// ListRules returns a device's rules, ordered by rule id.
func (s *Store) ListRules(id string) ([]Rule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.devices[id]
	if !ok {
		return nil, ErrDeviceNotFound
	}
	rules := make([]Rule, 0, len(state.rules))
	for _, rs := range state.rules {
		rules = append(rules, cloneRule(rs.rule))
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })
	return rules, nil
}

// AckAlert records the server time of the first acknowledgment. Repeated
// acknowledgments keep the original time and write nothing. It returns
// ErrDeviceNotFound for an unknown device and ErrAlertNotFound for an unknown
// alert id.
func (s *Store) AckAlert(id string, alertID int64) (Alert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[id]
	if !ok {
		return Alert{}, ErrDeviceNotFound
	}
	if alertID < 1 || int(alertID) > len(state.alerts) {
		return Alert{}, ErrAlertNotFound
	}
	alert := state.alerts[alertID-1]
	if alert.AcknowledgedAt != nil {
		return cloneAlert(*alert), nil
	}
	now := s.now().UTC()
	if s.wal != nil {
		if err := s.wal.appendRecord(recAlertAck, walAlertAck{
			DeviceID: id, AlertID: alertID, AcknowledgedAt: now,
		}); err != nil {
			return Alert{}, storageUnavailable(err)
		}
	}
	alert.AcknowledgedAt = &now
	return cloneAlert(*alert), nil
}

// ListAlerts returns a device's alerts with the given filters, ordered by
// alert id.
func (s *Store) ListAlerts(id string, filter AlertFilter) ([]Alert, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.devices[id]
	if !ok {
		return nil, ErrDeviceNotFound
	}
	if filter.RuleID != "" {
		if _, ok := state.rules[filter.RuleID]; !ok {
			return nil, ErrRuleNotFound
		}
	}
	alerts := make([]Alert, 0, len(state.alerts))
	for _, alert := range state.alerts {
		if filter.RuleID != "" && alert.RuleID != filter.RuleID {
			continue
		}
		if filter.Status != "" && alert.Status != filter.Status {
			continue
		}
		if filter.Acknowledged != nil {
			acknowledged := alert.AcknowledgedAt != nil
			if *filter.Acknowledged != acknowledged {
				continue
			}
		}
		alerts = append(alerts, cloneAlert(*alert))
	}
	return alerts, nil
}

// alertEvaluator applies accepted samples to a device's enabled rules without
// mutating state until commit. It tracks which rule currently has an active
// alert so a trigger and a recovery inside the same batch are both handled in
// order.
type alertEvaluator struct {
	state        *deviceState
	activeByRule map[string]int64 // ruleID -> active alert id (0 when none)
	created      []*Alert
	ended        []*Alert
	nextID       int64
}

func newAlertEvaluator(state *deviceState) *alertEvaluator {
	eval := &alertEvaluator{
		state:        state,
		activeByRule: make(map[string]int64, len(state.rules)),
		nextID:       int64(len(state.alerts)) + 1,
	}
	for id, rs := range state.rules {
		eval.activeByRule[id] = rs.activeAlertID
	}
	return eval
}

// evaluate judges one accepted sample. A value at or above the trigger opens
// an alert (or keeps the existing one); a value at or below the recovery
// threshold ends the active alert; anything in between or a missing metric
// leaves state unchanged.
func (eval *alertEvaluator) evaluate(seq int64, observedAt time.Time, values map[string]float64) {
	for _, rs := range eval.state.rules {
		if !rs.rule.Enabled {
			continue
		}
		value, ok := values[rs.rule.Metric]
		if !ok {
			continue
		}
		activeID := eval.activeByRule[rs.rule.ID]
		if value >= rs.rule.Trigger && activeID == 0 {
			alert := &Alert{
				ID:                eval.nextID,
				RuleID:            rs.rule.ID,
				RuleVersion:       rs.rule.Version,
				Metric:            rs.rule.Metric,
				Trigger:           rs.rule.Trigger,
				Recover:           rs.rule.Recover,
				Status:            alertStatusActive,
				TriggerSequence:   seq,
				TriggerValue:      value,
				TriggerObservedAt: observedAt,
			}
			eval.nextID++
			eval.created = append(eval.created, alert)
			eval.activeByRule[rs.rule.ID] = alert.ID
		} else if value <= rs.rule.Recover && activeID != 0 {
			var source *Alert
			if activeID <= int64(len(eval.state.alerts)) {
				source = eval.state.alerts[activeID-1]
			} else {
				source = eval.created[activeID-int64(len(eval.state.alerts))-1]
			}
			alert := cloneAlert(*source)
			alert.Status = alertStatusEnded
			alert.EndReason = endReasonRecovered
			alert.RecoverSequence = &seq
			alert.RecoverValue = &value
			alert.RecoverObservedAt = &observedAt
			eval.ended = append(eval.ended, &alert)
			eval.activeByRule[rs.rule.ID] = 0
		}
	}
}

// commit publishes the planned alert changes to the device state.
func (eval *alertEvaluator) commit() {
	applyAlertChanges(eval.state, eval.created, eval.ended)
}

// applyAlertChanges appends created alerts and replaces ended ones, keeping
// each rule's active-alert pointer in sync. It is shared by live evaluation
// and WAL recovery.
func applyAlertChanges(state *deviceState, created, ended []*Alert) {
	for _, alert := range created {
		state.alerts = append(state.alerts, alert)
		if rs, ok := state.rules[alert.RuleID]; ok {
			rs.activeAlertID = alert.ID
		}
	}
	for _, alert := range ended {
		state.alerts[alert.ID-1] = alert
		if rs, ok := state.rules[alert.RuleID]; ok && rs.activeAlertID == alert.ID {
			rs.activeAlertID = 0
		}
	}
}

func cloneRule(rule Rule) Rule {
	return rule
}

func cloneAlert(alert Alert) Alert {
	if alert.RecoverSequence != nil {
		seq := *alert.RecoverSequence
		alert.RecoverSequence = &seq
	}
	if alert.RecoverValue != nil {
		value := *alert.RecoverValue
		alert.RecoverValue = &value
	}
	if alert.RecoverObservedAt != nil {
		t := *alert.RecoverObservedAt
		alert.RecoverObservedAt = &t
	}
	if alert.EndedAt != nil {
		t := *alert.EndedAt
		alert.EndedAt = &t
	}
	if alert.AcknowledgedAt != nil {
		t := *alert.AcknowledgedAt
		alert.AcknowledgedAt = &t
	}
	return alert
}

func cloneAlerts(alerts []*Alert) []*Alert {
	if alerts == nil {
		return nil
	}
	cloned := make([]*Alert, len(alerts))
	for i, alert := range alerts {
		c := cloneAlert(*alert)
		cloned[i] = &c
	}
	return cloned
}

// History returns one page of a device's event history. afterIndex is the first
// 0-based event slot to scan (0 for the first page); limit caps the number of
// matching events returned. bound pins the scan to sequences no larger than
// bound; a zero bound means "current maximum", which is then returned so
// callers can pin subsequent pages. nextIndex is the slot a following page
// should start at when hasMore is true. Concurrent writes therefore cannot
// skip or duplicate rows within a paged walk.
func (s *Store) History(id string, filter HistoryFilter, afterIndex, bound int64, limit int) (events []Event, appliedBound, nextIndex int64, hasMore bool, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.devices[id]
	if !ok {
		return nil, 0, 0, false, ErrDeviceNotFound
	}
	highWater := int64(len(state.events))
	if bound == 0 {
		bound = highWater
	}
	events = make([]Event, 0, limit)
	for index := afterIndex; index < bound; index++ {
		event := state.events[index]
		if !filter.From.IsZero() && event.ObservedAt.Before(filter.From) {
			continue
		}
		if !filter.To.IsZero() && event.ObservedAt.After(filter.To) {
			continue
		}
		if len(events) < limit {
			events = append(events, cloneEvent(event))
			continue
		}
		// This match belongs to the next page; resume scanning here so rows in
		// the gap are never examined twice.
		return events, bound, index, true, nil
	}
	return events, bound, bound, false, nil
}

func (s *Store) Snapshot() []Device {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Device, 0, len(s.devices))
	for _, state := range s.devices {
		result = append(result, cloneDevice(state.device))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func cloneDevice(device Device) Device {
	device.LastTelemetry = cloneTelemetry(device.LastTelemetry)
	return device
}

func cloneEvent(event Event) Event {
	event.Values = cloneTelemetry(event.Values)
	return event
}

func cloneTelemetry(values map[string]float64) map[string]float64 {
	if values == nil {
		return nil
	}
	cloned := make(map[string]float64, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

// sameInstant compares times as UTC instants.
func sameInstant(a, b time.Time) bool {
	return a.UTC().Equal(b.UTC())
}

// sameValues compares metric maps by key and value.
func sameValues(a, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		other, ok := b[key]
		if !ok || value != other {
			return false
		}
	}
	return true
}

// sameOrderedSamples reports whether two batches carry the same samples in the
// same order, compared as UTC instants and key/value maps.
func sameOrderedSamples(a, b []Sample) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].EventID != b[i].EventID ||
			!sameInstant(a[i].ObservedAt, b[i].ObservedAt) ||
			!sameValues(a[i].Values, b[i].Values) {
			return false
		}
	}
	return true
}
