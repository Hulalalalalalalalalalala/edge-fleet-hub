package fleet

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"time"
)

// Config delivery lets a fleet operator publish complete JSON configurations
// to a registered device. A simulated device reads only the latest target
// version, applies it, and reports a success or failure receipt. Versions are
// per-device integers starting at 1; before the first publish both the target
// and the applied version read as 0.
//
// History is immutable: publishing never edits an old version, so rolling back
// means re-publishing the old content as a new version. Every device keeps its
// own versions, publish-request deduplication records and receipts.

// ConfigView is one published configuration version with its complete content.
type ConfigView struct {
	Version     int64           `json:"version"`
	Content     json.RawMessage `json:"config"`
	PublishedAt time.Time       `json:"publishedAt"`
}

// ConfigPublishResult is returned (and stored for idempotent retries) by a
// successful publish.
type ConfigPublishResult struct {
	RequestID   string          `json:"requestId"`
	Version     int64           `json:"version"`
	Content     json.RawMessage `json:"config"`
	PublishedAt time.Time       `json:"publishedAt"`
}

// ConfigReceipt is one device-reported application outcome, retained in
// receive order.
type ConfigReceipt struct {
	ReceiptID  string    `json:"receiptId"`
	Version    int64     `json:"version"`
	Success    bool      `json:"success"`
	Reason     string    `json:"reason,omitempty"`
	ReceivedAt time.Time `json:"receivedAt"`
}

// ConfigStatus summarises where a device stands.
type ConfigStatus struct {
	TargetVersion  int64  `json:"targetVersion"`
	AppliedVersion int64  `json:"appliedVersion"`
	FailureReason  string `json:"-"`
}

var (
	// ErrConfigConflict covers a stale publish base, a publish requestId reused
	// with a different base or content, a stale/contradictory receipt, and a
	// receiptId reused with different content.
	ErrConfigConflict = errors.New("configuration conflict")
	// ErrConfigVersionUnknown is reported for a receipt for a version that was
	// never published to the device.
	ErrConfigVersionUnknown = errors.New("configuration version unknown")
)

type publishedConfig struct {
	content     json.RawMessage
	publishedAt time.Time
}

// storedPublish remembers a committed publish so the same requestId can be
// recognised as an idempotent retry or a conflicting reuse.
type storedPublish struct {
	baseVersion int64
	content     json.RawMessage
	result      ConfigPublishResult
}

type configState struct {
	versions  []publishedConfig        // index + 1 is the version number
	applied   int64                    // highest successfully applied version
	publishes map[string]storedPublish // requestId -> first publish
	receipts  []ConfigReceipt          // receive order
	byReceipt map[string]ConfigReceipt // receiptId -> first receipt
}

func newConfigState() *configState {
	return &configState{
		publishes: make(map[string]storedPublish),
		byReceipt: make(map[string]ConfigReceipt),
	}
}

// ensureConfig lazily attaches config state to a device; callers must hold the
// store lock (write mode when allocate is wanted).
func (state *deviceState) ensureConfig() *configState {
	if state.config == nil {
		state.config = newConfigState()
	}
	return state.config
}

// PublishConfig applies one optimistic-concurrency publish.
//
// A requestId already committed on this device is an idempotent retry when the
// base and content match (the first result is returned with repeat=true) and a
// conflict otherwise. A fresh requestId only creates a version when baseVersion
// equals the current target; the loser of a concurrent race therefore gets
// ErrConfigConflict without consuming a version.
func (s *Store) PublishConfig(id, requestID string, baseVersion int64, content json.RawMessage) (ConfigPublishResult, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[id]
	if !ok {
		return ConfigPublishResult{}, false, ErrDeviceNotFound
	}
	cfg := state.ensureConfig()

	if previous, seen := cfg.publishes[requestID]; seen {
		if previous.baseVersion != baseVersion || !sameJSON(previous.content, content) {
			return ConfigPublishResult{}, false, ErrConfigConflict
		}
		return clonePublishResult(previous.result), true, nil
	}

	target := int64(len(cfg.versions))
	if baseVersion != target {
		return ConfigPublishResult{}, false, ErrConfigConflict
	}
	version := target + 1
	now := s.now().UTC()
	stored := storedPublish{
		baseVersion: baseVersion,
		content:     cloneJSON(content),
		result: ConfigPublishResult{
			RequestID:   requestID,
			Version:     version,
			Content:     cloneJSON(content),
			PublishedAt: now,
		},
	}
	if s.wal != nil {
		if err := s.wal.appendRecord(recConfigPublish, walConfigPublish{
			DeviceID:    id,
			RequestID:   requestID,
			BaseVersion: baseVersion,
			Version:     version,
			Content:     cloneJSON(content),
			PublishedAt: now,
		}); err != nil {
			return ConfigPublishResult{}, false, storageUnavailable(err)
		}
	}
	cfg.versions = append(cfg.versions, publishedConfig{
		content:     cloneJSON(content),
		publishedAt: now,
	})
	cfg.publishes[requestID] = stored
	return clonePublishResult(stored.result), false, nil
}

// PendingConfig returns the latest target version when the device has not yet
// applied it. It returns ok=false when nothing was ever published or the
// latest version is already applied; reads never mutate state and old versions
// skipped by a newer target are not delivered individually.
func (s *Store) PendingConfig(id string) (ConfigView, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.devices[id]
	if !ok {
		return ConfigView{}, false, ErrDeviceNotFound
	}
	if state.config == nil {
		return ConfigView{}, false, nil
	}
	cfg := state.config
	target := int64(len(cfg.versions))
	if target == 0 || cfg.applied >= target {
		return ConfigView{}, false, nil
	}
	latest := cfg.versions[target-1]
	return ConfigView{
		Version:     target,
		Content:     cloneJSON(latest.content),
		PublishedAt: latest.publishedAt,
	}, true, nil
}

// RecordConfigReceipt validates and stores one application receipt.
//
// A repeated receiptId with identical version/result/reason returns the first
// receipt (repeat=true); a reused receiptId with different content conflicts.
// A version that was never published is ErrConfigVersionUnknown; a new receipt
// below the applied version and a failure for an already applied version
// conflict. Success advances the applied version (a success lagging behind the
// target is recorded and advances only up to its own version); failure never
// advances, so the latest target stays readable for a retry.
func (s *Store) RecordConfigReceipt(id, receiptID string, version int64, success bool, reason string) (ConfigReceipt, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[id]
	if !ok {
		return ConfigReceipt{}, false, ErrDeviceNotFound
	}
	cfg := state.ensureConfig()

	// A reason belongs to failures only; a successful result never carries one.
	if success {
		reason = ""
	}

	if previous, seen := cfg.byReceipt[receiptID]; seen {
		if previous.Version != version || previous.Success != success || previous.Reason != reason {
			return ConfigReceipt{}, false, ErrConfigConflict
		}
		return previous, true, nil
	}

	if version < 1 || version > int64(len(cfg.versions)) {
		return ConfigReceipt{}, false, ErrConfigVersionUnknown
	}
	if version < cfg.applied {
		return ConfigReceipt{}, false, ErrConfigConflict
	}
	if !success && version <= cfg.applied {
		// The version already succeeded; it cannot fail afterwards.
		return ConfigReceipt{}, false, ErrConfigConflict
	}

	now := s.now().UTC()
	receipt := ConfigReceipt{
		ReceiptID:  receiptID,
		Version:    version,
		Success:    success,
		Reason:     reason,
		ReceivedAt: now,
	}
	if s.wal != nil {
		if err := s.wal.appendRecord(recConfigReceipt, walConfigReceipt{
			DeviceID:   id,
			ReceiptID:  receiptID,
			Version:    version,
			Success:    success,
			Reason:     reason,
			ReceivedAt: now,
		}); err != nil {
			return ConfigReceipt{}, false, storageUnavailable(err)
		}
	}
	cfg.receipts = append(cfg.receipts, receipt)
	cfg.byReceipt[receiptID] = receipt
	if success && version > cfg.applied {
		cfg.applied = version
	}
	return receipt, false, nil
}

// ListConfigs returns every published version in ascending version order, each
// with its complete content.
func (s *Store) ListConfigs(id string) ([]ConfigView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.devices[id]
	if !ok {
		return nil, ErrDeviceNotFound
	}
	if state.config == nil {
		return []ConfigView{}, nil
	}
	cfg := state.config
	views := make([]ConfigView, 0, len(cfg.versions))
	for i, published := range cfg.versions {
		views = append(views, ConfigView{
			Version:     int64(i + 1),
			Content:     cloneJSON(published.content),
			PublishedAt: published.publishedAt,
		})
	}
	return views, nil
}

// ListConfigReceipts returns the device's receipts in first-receive order.
func (s *Store) ListConfigReceipts(id string) ([]ConfigReceipt, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.devices[id]
	if !ok {
		return nil, ErrDeviceNotFound
	}
	if state.config == nil {
		return []ConfigReceipt{}, nil
	}
	return append([]ConfigReceipt(nil), state.config.receipts...), nil
}

// ConfigStatus returns target/applied versions plus the reason of the most
// recent failing receipt that still concerns an unapplied version.
func (s *Store) GetConfigStatus(id string) (ConfigStatus, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.devices[id]
	if !ok {
		return ConfigStatus{}, ErrDeviceNotFound
	}
	status := ConfigStatus{}
	if state.config == nil {
		return status, nil
	}
	cfg := state.config
	status.TargetVersion = int64(len(cfg.versions))
	status.AppliedVersion = cfg.applied
	for i := len(cfg.receipts) - 1; i >= 0; i-- {
		receipt := cfg.receipts[i]
		if !receipt.Success && receipt.Version > cfg.applied {
			status.FailureReason = receipt.Reason
			break
		}
	}
	return status, nil
}

func clonePublishResult(result ConfigPublishResult) ConfigPublishResult {
	result.Content = cloneJSON(result.Content)
	return result
}

func cloneJSON(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

// isNonEmptyJSONObject reports whether raw is exactly one JSON object with at
// least one member. It is shared by HTTP validation and WAL recovery.
func isNonEmptyJSONObject(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	object, ok := value.(map[string]any)
	return ok && len(object) > 0
}

// sameJSON compares two JSON documents semantically: object key order and
// insignificant whitespace are ignored, numbers compare by their exact
// written decimal value, and array element order is significant.
func sameJSON(a, b json.RawMessage) bool {
	av, aok := decodeJSONValue(a)
	bv, bok := decodeJSONValue(b)
	if !aok || !bok {
		return false
	}
	return jsonValueEqual(av, bv)
}

// decodeJSONValue decodes exactly one JSON document, keeping numbers as their
// original literal (json.Number) so no precision is lost before comparison.
func decodeJSONValue(raw json.RawMessage) (any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, false
	}
	return value, true
}

func jsonValueEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for key, value := range av {
			other, ok := bv[key]
			if !ok || !jsonValueEqual(value, other) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonValueEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case json.Number:
		// Numbers compare by exact decimal value: 1, 1.0 and 1e0 are equal,
		// but 9007199254740992 and 9007199254740993 (or 0 and 1e-400) are
		// not, even though each pair shares one float64.
		bv, ok := b.(json.Number)
		return ok && sameJSONNumber(av, bv)
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case nil:
		return b == nil
	default:
		return false
	}
}

// sameJSONNumber reports whether two JSON number literals denote the same
// exact decimal value. Parsing through big.Rat keeps every written digit and
// exponent significant, so neither large integers beyond 2^53 nor tiny
// numbers that would underflow to zero are ever conflated.
func sameJSONNumber(a, b json.Number) bool {
	ra, okA := new(big.Rat).SetString(a.String())
	rb, okB := new(big.Rat).SetString(b.String())
	if !okA || !okB {
		// A literal the exact parser rejects (never produced by the JSON
		// decoder) falls back to comparing the written form.
		return a.String() == b.String()
	}
	return ra.Cmp(rb) == 0
}
