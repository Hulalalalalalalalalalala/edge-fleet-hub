package fleet

import (
	"errors"
	"sort"
	"sync"
	"time"
)

var (
	ErrDeviceNotFound = errors.New("device not found")
	ErrBatchConflict  = errors.New("batch conflicts with stored events")
)

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

	rules     map[string]*Rule
	alerts    []Alert          // ordered by alertId (alertId = index + 1)
	active    map[string]int64 // ruleID -> alertID of the open alert, if any
	nextAlert int64            // next alert id to assign, starts at 1
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
		device:    device,
		byEvent:   make(map[string]int64),
		batches:   make(map[string]storedBatch),
		rules:     make(map[string]*Rule),
		active:    make(map[string]int64),
		nextAlert: 1,
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
	// Plan alert transitions for this sample before the commit point; the plan
	// rides along in the same WAL record and is applied only after it.
	plan := state.newAlertPlan()
	plan.observe(state, sequence, values, now, now)
	if s.wal != nil {
		if err := s.wal.appendRecord(recTelemetry, walTelemetry{
			DeviceID:     id,
			Sequence:     sequence,
			ObservedAt:   now,
			Values:       values,
			AlertsOpened: plan.opened,
			AlertsClosed: plan.closed,
		}); err != nil {
			return Device{}, storageUnavailable(err)
		}
	}
	state.events = append(state.events, Event{
		Sequence:   sequence,
		ObservedAt: now,
		Values:     cloneTelemetry(values),
	})
	state.applyAlertPlan(plan)
	state.device.LastSeenAt = now
	state.device.LastTelemetry = cloneTelemetry(values)
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

	// Durable commit point: the whole batch is one record, synced before any
	// in-memory state changes and before success is reported. A failure here
	// leaves sequences, history, receipts, alerts and device state untouched.
	lastNew := -1
	for i, entry := range planned {
		if !entry.dup {
			lastNew = i
		}
	}
	var committedAt time.Time
	alerts := state.newAlertPlan()
	if lastNew >= 0 {
		committedAt = s.now().UTC()
		// Only new samples are evaluated, in receive order; duplicates carry no
		// alert effect. A batch that triggers and then recovers records both
		// transitions in the same commit unit.
		for _, entry := range planned {
			if entry.dup {
				continue
			}
			alerts.observe(state, entry.sequence, entry.sample.Values, entry.sample.ObservedAt, committedAt)
		}
	}
	if s.wal != nil {
		record := walReplay{
			DeviceID: id,
			BatchID:  batchID,
			Samples:  make([]walSample, len(samples)),
			Receipt:  receipt,
		}
		for i, sample := range samples {
			record.Samples[i] = walSample{
				EventID:    sample.EventID,
				ObservedAt: sample.ObservedAt,
				Values:     sample.Values,
			}
		}
		if lastNew >= 0 {
			record.LastSeenAt = &committedAt
			record.LastTelemetry = cloneTelemetry(samples[lastNew].Values)
			record.AlertsOpened = alerts.opened
			record.AlertsClosed = alerts.closed
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
	state.applyAlertPlan(alerts)

	committed := make([]Sample, len(samples))
	copy(committed, samples)
	for i := range committed {
		committed[i].Values = cloneTelemetry(samples[i].Values)
	}
	state.batches[batchID] = storedBatch{samples: committed, receipt: cloneReceipt(receipt)}

	if lastNew >= 0 {
		state.device.LastSeenAt = committedAt
		state.device.LastTelemetry = cloneTelemetry(samples[lastNew].Values)
	}
	return receipt, false, nil
}

func cloneReceipt(receipt ReplayReceipt) ReplayReceipt {
	receipt.SampleStatus = append([]SampleStatus(nil), receipt.SampleStatus...)
	return receipt
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
