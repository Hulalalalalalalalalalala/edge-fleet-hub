package fleet

import (
	"errors"
	"sort"
	"sync"
	"time"
)

var ErrDeviceNotFound = errors.New("device not found")

type Device struct {
	ID            string             `json:"id"`
	Site          string             `json:"site"`
	RegisteredAt  time.Time          `json:"registeredAt"`
	LastSeenAt    time.Time          `json:"lastSeenAt"`
	LastTelemetry map[string]float64 `json:"lastTelemetry,omitempty"`
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
	device := Device{ID: id, Site: site, RegisteredAt: now, LastSeenAt: now}
	s.devices[id] = device
	return cloneDevice(device), true
}

func (s *Store) RecordTelemetry(id string, values map[string]float64) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	device, ok := s.devices[id]
	if !ok {
		return Device{}, ErrDeviceNotFound
	}
	device.LastSeenAt = s.now().UTC()
	device.LastTelemetry = cloneTelemetry(values)
	s.devices[id] = device
	return cloneDevice(device), nil
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
