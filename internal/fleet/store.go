package fleet

import (
	"errors"
	"sort"
	"sync"
	"time"
)

var (
	ErrDeviceNotFound = errors.New("device not found")
	ErrConflict       = errors.New("conflicting event or batch content")
)

type Device struct {
	ID            string             `json:"id"`
	Site          string             `json:"site"`
	RegisteredAt  time.Time          `json:"registeredAt"`
	LastSeenAt    time.Time          `json:"lastSeenAt"`
	LastTelemetry map[string]float64 `json:"lastTelemetry,omitempty"`

	history []HistoryEntry
	events  map[string]int64
	batches map[string]BatchRecord
}

// HistoryEntry is one receive-ordered record in a device's telemetry history.
type HistoryEntry struct {
	Seq        int64              `json:"seq"`
	EventID    string             `json:"eventId,omitempty"`
	ObservedAt time.Time          `json:"observedAt"`
	ReceivedAt time.Time          `json:"receivedAt"`
	Values     map[string]float64 `json:"values"`
	BatchID    string             `json:"batchId,omitempty"`
}

// ReplaySample is a validated sample submitted through a replay batch.
type ReplaySample struct {
	EventID    string
	ObservedAt time.Time
	Values     map[string]float64
}

// SampleResult describes how a single sample was accepted.
type SampleResult struct {
	EventID string `json:"eventId"`
	Seq     int64  `json:"seq"`
	Status  string `json:"status"`
}

// ReplayReceipt is returned for both new (202) and idempotent (200) batches.
type ReplayReceipt struct {
	BatchID        string         `json:"batchId"`
	Results        []SampleResult `json:"results"`
	NewCount       int            `json:"newCount"`
	DuplicateCount int            `json:"duplicateCount"`
}

// BatchRecord stores the ordered content and first receipt of a batch.
type BatchRecord struct {
	Samples []ReplaySample
	Receipt ReplayReceipt
}

type Store struct {
	mu      sync.RWMutex
	devices map[string]Device
	now     func() time.Time
}

func NewStore() *Store {
	return &Store{devices: make(map[string]Device), now: time.Now}
}

func (s *Store) Register(id, site string) (Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.devices[id]; ok {
		return cloneDevice(current), false
	}
	now := s.now().UTC()
	device := Device{
		ID:           id,
		Site:         site,
		RegisteredAt: now,
		LastSeenAt:   now,
		events:       make(map[string]int64),
		batches:      make(map[string]BatchRecord),
	}
	s.devices[id] = device
	return cloneDevice(device), true
}

// RecordTelemetry appends a direct telemetry write to the device's history.
// The sampling time is the receive time.
func (s *Store) RecordTelemetry(id string, values map[string]float64) (HistoryEntry, Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	device, ok := s.devices[id]
	if !ok {
		return HistoryEntry{}, Device{}, ErrDeviceNotFound
	}
	now := s.now().UTC()
	seq := int64(len(device.history)) + 1
	entry := HistoryEntry{
		Seq:        seq,
		ObservedAt: now,
		ReceivedAt: now,
		Values:     cloneTelemetry(values),
	}
	device.history = append(device.history, entry)
	device.LastSeenAt = now
	device.LastTelemetry = cloneTelemetry(values)
	s.devices[id] = device
	return cloneHistoryEntry(entry), cloneDevice(device), nil
}

// ReplayBatch atomically applies a validated batch.
//
// New batches are assigned sequence numbers per device starting at 1 with no
// gaps. A sample whose eventId already exists with identical content (same
// observed instant, same values key-by-key) is a duplicate and reuses the
// original sequence number. Any content conflict, or a batchId reused with
// different ordered content, fails with ErrConflict and leaves no state
// change. A batchId resubmitted with identical content returns the stored
// receipt with idempotent=true.
func (s *Store) ReplayBatch(id, batchID string, samples []ReplaySample) (receipt ReplayReceipt, idempotent bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	device, ok := s.devices[id]
	if !ok {
		return ReplayReceipt{}, false, ErrDeviceNotFound
	}
	if existing, ok := device.batches[batchID]; ok {
		if sameBatchSamples(existing.Samples, samples) {
			return cloneReceipt(existing.Receipt), true, nil
		}
		return ReplayReceipt{}, false, ErrConflict
	}

	now := s.now().UTC()
	receipt = ReplayReceipt{BatchID: batchID, Results: make([]SampleResult, 0, len(samples))}
	var added []HistoryEntry
	var lastNewValues map[string]float64
	newCount, dupCount := 0, 0

	for _, sample := range samples {
		if seq, known := device.events[sample.EventID]; known {
			entry := device.history[seq-1]
			if entry.ObservedAt.Equal(sample.ObservedAt) && sameValues(entry.Values, sample.Values) {
				receipt.Results = append(receipt.Results, SampleResult{
					EventID: sample.EventID,
					Seq:     seq,
					Status:  "duplicate",
				})
				dupCount++
				continue
			}
			return ReplayReceipt{}, false, ErrConflict
		}
		seq := int64(len(device.history)) + int64(len(added)) + 1
		entry := HistoryEntry{
			Seq:        seq,
			EventID:    sample.EventID,
			ObservedAt: sample.ObservedAt,
			ReceivedAt: now,
			Values:     cloneTelemetry(sample.Values),
			BatchID:    batchID,
		}
		added = append(added, entry)
		receipt.Results = append(receipt.Results, SampleResult{
			EventID: sample.EventID,
			Seq:     seq,
			Status:  "new",
		})
		newCount++
		lastNewValues = cloneTelemetry(sample.Values)
	}

	device.history = append(device.history, added...)
	for _, entry := range added {
		device.events[entry.EventID] = entry.Seq
	}
	if newCount > 0 {
		device.LastSeenAt = now
		device.LastTelemetry = lastNewValues
	}
	receipt.NewCount = newCount
	receipt.DuplicateCount = dupCount
	device.batches[batchID] = BatchRecord{
		Samples: cloneBatchSamples(samples),
		Receipt: cloneReceipt(receipt),
	}
	s.devices[id] = device
	return receipt, false, nil
}

// MaxSeq returns the current largest receive sequence number for a device.
func (s *Store) MaxSeq(id string) (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	device, ok := s.devices[id]
	if !ok {
		return 0, false
	}
	return int64(len(device.history)), true
}

// QueryHistory returns entries with sequence numbers in (after, upper] whose
// sampling time falls in [from, to] (both bounds inclusive). At most limit
// entries are returned; callers request limit+1 to detect a next page.
func (s *Store) QueryHistory(id string, from, to *time.Time, upper, after int64, limit int) ([]HistoryEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	device, ok := s.devices[id]
	if !ok {
		return nil, ErrDeviceNotFound
	}
	result := make([]HistoryEntry, 0, min(limit, len(device.history)))
	for _, entry := range device.history {
		if entry.Seq <= after {
			continue
		}
		if entry.Seq > upper {
			break
		}
		if from != nil && entry.ObservedAt.Before(*from) {
			continue
		}
		if to != nil && entry.ObservedAt.After(*to) {
			continue
		}
		result = append(result, cloneHistoryEntry(entry))
		if len(result) >= limit {
			break
		}
	}
	return result, nil
}

func (s *Store) Snapshot() []Device {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Device, 0, len(s.devices))
	for _, device := range s.devices {
		result = append(result, cloneDevice(device))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

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

func sameBatchSamples(a, b []ReplaySample) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].EventID != b[i].EventID ||
			!a[i].ObservedAt.Equal(b[i].ObservedAt) ||
			!sameValues(a[i].Values, b[i].Values) {
			return false
		}
	}
	return true
}

func cloneDevice(device Device) Device {
	device.LastTelemetry = cloneTelemetry(device.LastTelemetry)
	return device
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

func cloneHistoryEntry(entry HistoryEntry) HistoryEntry {
	entry.Values = cloneTelemetry(entry.Values)
	return entry
}

func cloneBatchSamples(samples []ReplaySample) []ReplaySample {
	cloned := make([]ReplaySample, len(samples))
	for i, sample := range samples {
		cloned[i] = ReplaySample{
			EventID:    sample.EventID,
			ObservedAt: sample.ObservedAt,
			Values:     cloneTelemetry(sample.Values),
		}
	}
	return cloned
}

func cloneReceipt(receipt ReplayReceipt) ReplayReceipt {
	cloned := receipt
	cloned.Results = append([]SampleResult(nil), receipt.Results...)
	return cloned
}
