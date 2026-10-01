package fleet

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Persistent on-disk layout for one data directory:
//
//	LOCK        exclusive flock held for the lifetime of the process
//	state.json  full committed state, replaced atomically (temp file + fsync
//	             + rename + directory fsync). A write is therefore either
//	             fully committed or not present at all; a crash never leaves
//	             a half-written state.json.
//
// state.json.tmp is the in-progress write target and is always uncommitted;
// it is removed when a process starts up under the held lock.

const (
	formatVersion = "edge-fleet-hub-v1"
	stateFileName = "state.json"
	lockFileName  = "LOCK"
)

// ErrPersist is returned to callers when a write could not be made durable.
// The in-memory state is rolled back before this is returned, so a failed
// write never changes queryable state, consumes a sequence, or leaves a
// receipt.
var ErrPersist = errors.New("persistence failure")

// ErrCorrupt is returned when on-disk data cannot be parsed or fails
// relationship validation. Startup fails and the original files are kept.
var ErrCorrupt = errors.New("persisted data is corrupt")

type persister interface {
	save(*persistedState) error
}

// persistedState is the serialisable form of a Store.
type persistedState struct {
	Format   string                      `json:"format"`
	Instance string                      `json:"instance"`
	Devices  map[string]*persistedDevice `json:"devices"`
}

type persistedDevice struct {
	Device  Device                    `json:"device"`
	Events  []Event                   `json:"events"`
	ByEvent map[string]int64          `json:"byEvent"`
	Batches map[string]persistedBatch `json:"batches"`
}

type persistedBatch struct {
	Samples []Sample      `json:"samples"`
	Receipt ReplayReceipt `json:"receipt"`
}

type filePersister struct {
	dir       string
	statePath string
}

func (p *filePersister) save(ps *persistedState) error {
	data, err := json.Marshal(ps)
	if err != nil {
		return err
	}
	tmp := p.statePath + ".tmp"
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tmp, p.statePath); err != nil {
		return err
	}
	return syncDir(p.dir)
}

func (p *filePersister) load() (*persistedState, error) {
	data, err := os.ReadFile(p.statePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read state file: %w", err)
	}
	var ps persistedState
	if err := json.Unmarshal(data, &ps); err != nil {
		return nil, fmt.Errorf("%w: cannot parse state file: %v", ErrCorrupt, err)
	}
	return &ps, nil
}

func syncDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

// NewPersistentStore opens (or creates) the data directory at dir and loads
// its committed state. It fails with a reason when the directory cannot be
// created, read, or written, when another process already holds the
// directory lock, or when committed data is corrupt or unsupported. It never
// falls back to in-memory mode.
func NewPersistentStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cannot create data directory %q: %w", dir, err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	dir = abs

	// Probe writability before taking the lock so an unwritable directory
	// fails with a clear reason.
	probe := filepath.Join(dir, ".write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return nil, fmt.Errorf("data directory %q is not writable: %w", dir, err)
	}
	if err := os.Remove(probe); err != nil {
		return nil, fmt.Errorf("cannot clean up data directory %q: %w", dir, err)
	}

	lockFile, err := os.OpenFile(filepath.Join(dir, lockFileName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cannot open lock file: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lockFile.Close()
		return nil, fmt.Errorf("data directory %q is already in use by another process", dir)
	}

	p := &filePersister{dir: dir, statePath: filepath.Join(dir, stateFileName)}

	// A leftover temp file is never committed (rename is the commit point),
	// so removing it under the held lock is safe.
	if _, err := os.Stat(p.statePath + ".tmp"); err == nil {
		if err := os.Remove(p.statePath + ".tmp"); err != nil {
			lockFile.Close()
			return nil, fmt.Errorf("cannot remove stale temporary state file: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		lockFile.Close()
		return nil, fmt.Errorf("cannot inspect data directory %q: %w", dir, err)
	}

	ps, err := p.load()
	if err != nil {
		lockFile.Close()
		return nil, err
	}
	if ps == nil {
		// First use of an empty directory: establish the format and a fresh
		// instance identifier.
		ps = &persistedState{Format: formatVersion, Instance: newInstanceID(), Devices: map[string]*persistedDevice{}}
		if err := p.save(ps); err != nil {
			lockFile.Close()
			return nil, fmt.Errorf("cannot initialize data directory %q: %w", dir, err)
		}
	} else if err := validate(ps); err != nil {
		lockFile.Close()
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}

	store := NewStore()
	store.persister = p
	store.instance = ps.Instance
	store.loadState(ps)
	store.lockFile = lockFile
	return store, nil
}

// Close releases the directory lock. It is called on normal shutdown; a
// forced process death releases the lock through the kernel, so no manual
// cleanup is ever needed.
func (s *Store) Close() error {
	if s.lockFile == nil {
		return nil
	}
	err := s.lockFile.Close()
	s.lockFile = nil
	return err
}

func newInstanceID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// rand.Read does not fail on supported platforms; keep a usable
		// fallback rather than panicking.
		return hex.EncodeToString([]byte("edge-fleet-hub-instance"))
	}
	return hex.EncodeToString(raw[:])
}

// validate checks every relationship in committed state so a corrupt or
// partially-written directory cannot be served.
func validate(ps *persistedState) error {
	if ps.Format != formatVersion {
		return fmt.Errorf("unsupported data format %q", ps.Format)
	}
	if ps.Instance == "" {
		return errors.New("missing data directory instance identifier")
	}
	for id, pd := range ps.Devices {
		if pd == nil {
			return fmt.Errorf("device %q: empty record", id)
		}
		if id == "" || pd.Device.ID != id {
			return fmt.Errorf("device %q: identifier mismatch", id)
		}
		device := pd.Device
		if device.RegisteredAt.IsZero() || device.LastSeenAt.IsZero() {
			return fmt.Errorf("device %q: missing timestamps", id)
		}
		count := int64(len(pd.Events))
		for i, event := range pd.Events {
			if event.Sequence != int64(i+1) {
				return fmt.Errorf("device %q: event sequence %d at position %d", id, event.Sequence, i+1)
			}
			if len(event.Values) == 0 {
				return fmt.Errorf("device %q: event %d has no values", id, event.Sequence)
			}
		}

		recomputed := make(map[string]int64)
		for batchID, batch := range pd.Batches {
			if batchID == "" {
				return fmt.Errorf("device %q: batch with empty identifier", id)
			}
			if batch.Receipt.BatchID != batchID {
				return fmt.Errorf("device %q: batch %q receipt identifier mismatch", id, batchID)
			}
			if len(batch.Samples) == 0 {
				return fmt.Errorf("device %q: batch %q has no samples", id, batchID)
			}
			if len(batch.Receipt.SampleStatus) != len(batch.Samples) {
				return fmt.Errorf("device %q: batch %q receipt length mismatch", id, batchID)
			}
			newCount, dupCount := 0, 0
			seenInBatch := make(map[string]bool, len(batch.Samples))
			for i, status := range batch.Receipt.SampleStatus {
				if status.EventID != batch.Samples[i].EventID {
					return fmt.Errorf("device %q: batch %q sample/event identifier mismatch", id, batchID)
				}
				if status.Sequence < 1 || status.Sequence > count {
					return fmt.Errorf("device %q: batch %q references sequence %d outside history", id, batchID, status.Sequence)
				}
				event := pd.Events[status.Sequence-1]
				sample := batch.Samples[i]
				if !sameInstant(event.ObservedAt, sample.ObservedAt) || !sameValues(event.Values, sample.Values) {
					return fmt.Errorf("device %q: batch %q sample does not match stored event at sequence %d", id, batchID, status.Sequence)
				}
				if status.Duplicate {
					dupCount++
					if pd.ByEvent[status.EventID] != status.Sequence {
						return fmt.Errorf("device %q: batch %q duplicate event %q is not in the dedup index at sequence %d", id, batchID, status.EventID, status.Sequence)
					}
				} else {
					newCount++
					recomputed[status.EventID] = status.Sequence
				}
				if seenInBatch[status.EventID] {
					return fmt.Errorf("device %q: batch %q contains duplicate event %q", id, batchID, status.EventID)
				}
				seenInBatch[status.EventID] = true
			}
			if newCount != batch.Receipt.NewCount || dupCount != batch.Receipt.Duplicate {
				return fmt.Errorf("device %q: batch %q receipt counts mismatch", id, batchID)
			}
		}

		if len(recomputed) != len(pd.ByEvent) {
			return fmt.Errorf("device %q: event dedup index has %d entries, %d committed", id, len(pd.ByEvent), len(recomputed))
		}
		for eventID, sequence := range pd.ByEvent {
			if eventID == "" {
				return fmt.Errorf("device %q: dedup index has blank event identifier", id)
			}
			if recomputed[eventID] != sequence {
				return fmt.Errorf("device %q: dedup index for event %q points to %d, committed %d", id, eventID, sequence, recomputed[eventID])
			}
		}
	}
	return nil
}

// snapshotLocked serialises the store's full state. Callers must hold s.mu.
func (s *Store) snapshotLocked() *persistedState {
	ps := &persistedState{
		Format:   formatVersion,
		Instance: s.instance,
		Devices:  make(map[string]*persistedDevice, len(s.devices)),
	}
	for id, state := range s.devices {
		pd := &persistedDevice{
			Device:  state.device,
			Events:  append([]Event(nil), state.events...),
			ByEvent: make(map[string]int64, len(state.byEvent)),
			Batches: make(map[string]persistedBatch, len(state.batches)),
		}
		pd.Device.LastTelemetry = cloneTelemetry(state.device.LastTelemetry)
		for i := range pd.Events {
			pd.Events[i].Values = cloneTelemetry(pd.Events[i].Values)
		}
		for eventID, sequence := range state.byEvent {
			pd.ByEvent[eventID] = sequence
		}
		for batchID, batch := range state.batches {
			samples := append([]Sample(nil), batch.samples...)
			for i := range samples {
				samples[i].Values = cloneTelemetry(samples[i].Values)
			}
			pd.Batches[batchID] = persistedBatch{Samples: samples, Receipt: cloneReceipt(batch.receipt)}
		}
		ps.Devices[id] = pd
	}
	return ps
}

// loadState replaces the in-memory state with recovered state.
func (s *Store) loadState(ps *persistedState) {
	for id, pd := range ps.Devices {
		state := &deviceState{
			device:  pd.Device,
			events:  pd.Events,
			byEvent: pd.ByEvent,
			batches: make(map[string]storedBatch, len(pd.Batches)),
		}
		for batchID, batch := range pd.Batches {
			state.batches[batchID] = storedBatch{samples: batch.Samples, receipt: batch.Receipt}
		}
		s.devices[id] = state
	}
}

// cloneDeviceState deep-copies one device's state for rollback when a
// persistence attempt fails.
func cloneDeviceState(state *deviceState) *deviceState {
	clone := &deviceState{
		device:  state.device,
		events:  append([]Event(nil), state.events...),
		byEvent: make(map[string]int64, len(state.byEvent)),
		batches: make(map[string]storedBatch, len(state.batches)),
	}
	clone.device.LastTelemetry = cloneTelemetry(state.device.LastTelemetry)
	for i := range clone.events {
		clone.events[i].Values = cloneTelemetry(clone.events[i].Values)
	}
	for eventID, sequence := range state.byEvent {
		clone.byEvent[eventID] = sequence
	}
	for batchID, batch := range state.batches {
		samples := append([]Sample(nil), batch.samples...)
		for i := range samples {
			samples[i].Values = cloneTelemetry(samples[i].Values)
		}
		clone.batches[batchID] = storedBatch{samples: samples, receipt: cloneReceipt(batch.receipt)}
	}
	return clone
}
