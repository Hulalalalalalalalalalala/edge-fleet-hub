package fleet

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"time"
)

var (
	ErrConfigConflict        = errors.New("config conflict")
	ErrReceiptConflict       = errors.New("config receipt conflict")
	ErrConfigVersionNotFound = errors.New("config version not found")
)

const (
	configResultSuccess = "success"
	configResultFailed  = "failed"
)

// ConfigVersion is one published configuration. Versions start at 1 and
// increase contiguously per device; historical versions are immutable.
type ConfigVersion struct {
	Version     int64     `json:"version"`
	Config      any       `json:"config"`
	PublishedAt time.Time `json:"publishedAt"`
}

// ConfigReceipt is one device-reported application result, retained in receive
// order.
type ConfigReceipt struct {
	ReceiptID  string    `json:"receiptId"`
	Version    int64     `json:"version"`
	Result     string    `json:"result"`
	Reason     string    `json:"reason,omitempty"`
	ReceivedAt time.Time `json:"receivedAt"`
}

// ConfigStatus summarises a device's configuration delivery state.
type ConfigStatus struct {
	TargetVersion  int64  `json:"targetVersion"`
	AppliedVersion int64  `json:"appliedVersion"`
	FailureReason  string `json:"failureReason,omitempty"`
}

// configPublishRecord retains the first acceptance of a requestId so a retry
// with the same base and content can be answered with the original result.
type configPublishRecord struct {
	baseVersion int64
	config      any
	response    ConfigVersion
}

type configState struct {
	versions       []ConfigVersion
	targetVersion  int64
	appliedVersion int64
	publishes      map[string]configPublishRecord // requestId -> first record
	receiptsList   []ConfigReceipt                // receive order
	receiptsByID   map[string]ConfigReceipt       // receiptId -> first receipt
}

func newConfigState() *configState {
	return &configState{
		publishes:    make(map[string]configPublishRecord),
		receiptsByID: make(map[string]ConfigReceipt),
	}
}

// PublishConfig commits a new configuration version when baseVersion matches
// the current target. A repeated requestId with the same base and content
// returns the first result without consuming a version; a repeated requestId
// with a different base or content conflicts. A baseVersion that does not
// match the current target also conflicts, so concurrent publishes on the same
// base can only create one version.
func (s *Store) PublishConfig(id, requestID string, baseVersion int64, config any) (ConfigVersion, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[id]
	if !ok {
		return ConfigVersion{}, false, ErrDeviceNotFound
	}
	if state.config == nil {
		state.config = newConfigState()
	}
	cs := state.config

	if prev, exists := cs.publishes[requestID]; exists {
		if prev.baseVersion == baseVersion && configEqual(prev.config, config) {
			return prev.response, false, nil
		}
		return ConfigVersion{}, false, ErrConfigConflict
	}
	if baseVersion != cs.targetVersion {
		return ConfigVersion{}, false, ErrConfigConflict
	}

	version := cs.targetVersion + 1
	now := s.now().UTC()
	response := ConfigVersion{
		Version:     version,
		Config:      cloneConfigValue(config),
		PublishedAt: now,
	}

	if s.wal != nil {
		if err := s.wal.appendRecord(recConfigPublish, walConfigPublish{
			DeviceID:    id,
			RequestID:   requestID,
			BaseVersion: baseVersion,
			Version:     version,
			Config:      cloneConfigValue(config),
			PublishedAt: now,
		}); err != nil {
			return ConfigVersion{}, false, storageUnavailable(err)
		}
	}

	cs.versions = append(cs.versions, response)
	cs.targetVersion = version
	cs.publishes[requestID] = configPublishRecord{
		baseVersion: baseVersion,
		config:      cloneConfigValue(config),
		response:    response,
	}
	return response, true, nil
}

// PendingConfig returns the latest target for a device to apply. When no
// configuration has been published or the latest version is already applied,
// ok is false (the caller reports 204). Repeated reads do not change state.
func (s *Store) PendingConfig(id string) (ConfigVersion, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.devices[id]
	if !ok {
		return ConfigVersion{}, false, ErrDeviceNotFound
	}
	cs := state.config
	if cs == nil || cs.targetVersion == 0 || cs.appliedVersion >= cs.targetVersion {
		return ConfigVersion{}, false, nil
	}
	return cs.versions[cs.targetVersion-1], true, nil
}

// ConfigStatus returns the target and applied versions plus the most recent
// failure reason (empty when no failure has been reported).
func (s *Store) ConfigStatus(id string) (ConfigStatus, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.devices[id]
	if !ok {
		return ConfigStatus{}, ErrDeviceNotFound
	}
	status := ConfigStatus{}
	if cs := state.config; cs != nil {
		status.TargetVersion = cs.targetVersion
		status.AppliedVersion = cs.appliedVersion
		for i := len(cs.receiptsList) - 1; i >= 0; i-- {
			if cs.receiptsList[i].Result == configResultFailed {
				status.FailureReason = cs.receiptsList[i].Reason
				break
			}
		}
	}
	return status, nil
}

// ListConfigVersions returns all published versions in ascending order.
func (s *Store) ListConfigVersions(id string) ([]ConfigVersion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.devices[id]
	if !ok {
		return nil, ErrDeviceNotFound
	}
	versions := []ConfigVersion{}
	if cs := state.config; cs != nil {
		for _, v := range cs.versions {
			versions = append(versions, ConfigVersion{
				Version:     v.Version,
				Config:      cloneConfigValue(v.Config),
				PublishedAt: v.PublishedAt,
			})
		}
	}
	return versions, nil
}

// ListConfigReceipts returns all receipts in receive order.
func (s *Store) ListConfigReceipts(id string) ([]ConfigReceipt, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.devices[id]
	if !ok {
		return nil, ErrDeviceNotFound
	}
	receipts := []ConfigReceipt{}
	if cs := state.config; cs != nil {
		receipts = append(receipts, cs.receiptsList...)
	}
	return receipts, nil
}

// ReportConfigReceipt records a device-reported application result. A success
// advances the applied version; a failure does not. A receipt for an unknown
// version is not found; a new receipt below the applied version conflicts, as
// does reporting failure on an already-succeeded version. A repeated receiptId
// with identical content returns the first result without changing state.
func (s *Store) ReportConfigReceipt(id, receiptID string, version int64, result, reason string) (ConfigReceipt, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[id]
	if !ok {
		return ConfigReceipt{}, false, ErrDeviceNotFound
	}
	if state.config == nil {
		state.config = newConfigState()
	}
	cs := state.config

	if prev, exists := cs.receiptsByID[receiptID]; exists {
		if prev.Version == version && prev.Result == result && prev.Reason == reason {
			return prev, false, nil
		}
		return ConfigReceipt{}, false, ErrReceiptConflict
	}

	if version > cs.targetVersion {
		return ConfigReceipt{}, false, ErrConfigVersionNotFound
	}
	if version < cs.appliedVersion {
		return ConfigReceipt{}, false, ErrReceiptConflict
	}
	if version == cs.appliedVersion && result == configResultFailed {
		return ConfigReceipt{}, false, ErrReceiptConflict
	}

	now := s.now().UTC()
	receipt := ConfigReceipt{
		ReceiptID:  receiptID,
		Version:    version,
		Result:     result,
		Reason:     reason,
		ReceivedAt: now,
	}

	if s.wal != nil {
		if err := s.wal.appendRecord(recConfigReceipt, walConfigReceipt{
			DeviceID:   id,
			ReceiptID:  receiptID,
			Version:    version,
			Result:     result,
			Reason:     reason,
			ReceivedAt: now,
		}); err != nil {
			return ConfigReceipt{}, false, storageUnavailable(err)
		}
	}

	cs.receiptsList = append(cs.receiptsList, receipt)
	cs.receiptsByID[receiptID] = receipt
	if result == configResultSuccess && version > cs.appliedVersion {
		cs.appliedVersion = version
	}
	return receipt, true, nil
}

// configEqual compares configuration content semantically: object key order
// and whitespace are irrelevant, numbers compare by value, and array order
// matters.
func configEqual(a, b any) bool {
	return reflect.DeepEqual(a, b)
}

// cloneConfigValue deep-copies a parsed JSON value through a JSON round trip.
func cloneConfigValue(value any) any {
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var cloned any
	if err := json.Unmarshal(raw, &cloned); err != nil {
		return nil
	}
	return cloned
}

// isJSONInteger reports whether raw is a JSON integer literal (no decimal
// point or exponent), so it can be decoded into an int64 without ambiguity.
func isJSONInteger(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return false
	}
	if s[0] == '-' {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// --- HTTP handlers -----------------------------------------------------------

func (h *handler) publishConfig(w http.ResponseWriter, r *http.Request) {
	raw := map[string]json.RawMessage{}
	if err := decodeSingleJSON(r, &raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must be a single valid JSON object"})
		return
	}

	var requestID string
	if v, ok := raw["requestId"]; !ok || json.Unmarshal(v, &requestID) != nil || strings.TrimSpace(requestID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "requestId is required"})
		return
	}

	var baseVersion int64
	if v, ok := raw["baseVersion"]; !ok || !isJSONInteger(v) || json.Unmarshal(v, &baseVersion) != nil || baseVersion < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "baseVersion must be a non-negative integer"})
		return
	}

	var config any
	if v, ok := raw["config"]; !ok || json.Unmarshal(v, &config) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "config is required"})
		return
	}
	obj, ok := config.(map[string]any)
	if !ok || len(obj) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "config must be a non-empty JSON object"})
		return
	}

	published, created, err := h.store.PublishConfig(r.PathValue("id"), strings.TrimSpace(requestID), baseVersion, config)
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ErrConfigConflict) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ErrStorageUnavailable) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, published)
}

func (h *handler) pendingConfig(w http.ResponseWriter, r *http.Request) {
	pending, ok, err := h.store.PendingConfig(r.PathValue("id"))
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, pending)
}

func (h *handler) configStatus(w http.ResponseWriter, r *http.Request) {
	status, err := h.store.ConfigStatus(r.PathValue("id"))
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (h *handler) listConfigVersions(w http.ResponseWriter, r *http.Request) {
	versions, err := h.store.ListConfigVersions(r.PathValue("id"))
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": versions})
}

func (h *handler) listConfigReceipts(w http.ResponseWriter, r *http.Request) {
	receipts, err := h.store.ListConfigReceipts(r.PathValue("id"))
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"receipts": receipts})
}

func (h *handler) reportConfigReceipt(w http.ResponseWriter, r *http.Request) {
	raw := map[string]json.RawMessage{}
	if err := decodeSingleJSON(r, &raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must be a single valid JSON object"})
		return
	}

	var receiptID string
	if v, ok := raw["receiptId"]; !ok || json.Unmarshal(v, &receiptID) != nil || strings.TrimSpace(receiptID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "receiptId is required"})
		return
	}

	var version int64
	if v, ok := raw["version"]; !ok || !isJSONInteger(v) || json.Unmarshal(v, &version) != nil || version < 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "version must be a positive integer"})
		return
	}

	var result string
	if v, ok := raw["result"]; !ok || json.Unmarshal(v, &result) != nil || (result != configResultSuccess && result != configResultFailed) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "result must be success or failed"})
		return
	}

	var reason string
	if v, ok := raw["reason"]; ok {
		if err := json.Unmarshal(v, &reason); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reason must be a string"})
			return
		}
	}
	if result == configResultFailed && strings.TrimSpace(reason) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reason is required for a failed result"})
		return
	}

	receipt, created, err := h.store.ReportConfigReceipt(r.PathValue("id"), strings.TrimSpace(receiptID), version, result, reason)
	if errors.Is(err, ErrDeviceNotFound) || errors.Is(err, ErrConfigVersionNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ErrReceiptConflict) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ErrStorageUnavailable) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, receipt)
}
