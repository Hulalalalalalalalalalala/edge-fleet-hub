package fleet

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// --- configuration delivery -------------------------------------------------

// configPublishRequest is the raw publish envelope. Config is decoded a second
// time as a generic JSON value so a non-object body is rejected even though the
// bytes themselves are valid JSON.
type configPublishRequest struct {
	RequestID   string          `json:"requestId"`
	BaseVersion *int64          `json:"baseVersion"`
	Config      json.RawMessage `json:"config"`
}

// configReceiptRequest is the device application receipt envelope.
type configReceiptRequest struct {
	ReceiptID string `json:"receiptId"`
	Version   int64  `json:"version"`
	Success   *bool  `json:"success"`
	Reason    string `json:"reason"`
}

func (h *handler) publishConfig(w http.ResponseWriter, r *http.Request) {
	var request configPublishRequest
	if err := decodeSingleJSON(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must be a single valid JSON object"})
		return
	}
	requestID := strings.TrimSpace(request.RequestID)
	if requestID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "requestId is required"})
		return
	}
	if request.BaseVersion == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "baseVersion is required"})
		return
	}
	if *request.BaseVersion < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "baseVersion must be a non-negative integer"})
		return
	}
	content, ok := nonEmptyJSONObject(w, request.Config)
	if !ok {
		return
	}

	result, repeat, err := h.store.PublishConfig(r.PathValue("id"), requestID, *request.BaseVersion, content)
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
	status := http.StatusCreated
	if repeat {
		status = http.StatusOK
	}
	writeJSON(w, status, result)
}

// pendingConfig serves the simulated device's pull. Only the newest target is
// ever returned; 204 (with empty body) means up to date or never published.
func (h *handler) pendingConfig(w http.ResponseWriter, r *http.Request) {
	view, ok, err := h.store.PendingConfig(r.PathValue("id"))
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
	writeJSON(w, http.StatusOK, view)
}

func (h *handler) receiveConfigReceipt(w http.ResponseWriter, r *http.Request) {
	var request configReceiptRequest
	if err := decodeSingleJSON(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must be a single valid JSON object"})
		return
	}
	receiptID := strings.TrimSpace(request.ReceiptID)
	if receiptID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "receiptId is required"})
		return
	}
	if request.Version < 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "version must be a positive integer"})
		return
	}
	if request.Success == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "success is required"})
		return
	}
	reason := strings.TrimSpace(request.Reason)
	if !*request.Success && reason == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a failed receipt requires a non-blank reason"})
		return
	}

	receipt, repeat, err := h.store.RecordConfigReceipt(r.PathValue("id"), receiptID, request.Version, *request.Success, reason)
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ErrConfigVersionUnknown) {
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
	status := http.StatusCreated
	if repeat {
		status = http.StatusOK
	}
	writeJSON(w, status, receipt)
}

func (h *handler) listConfigs(w http.ResponseWriter, r *http.Request) {
	views, err := h.store.ListConfigs(r.PathValue("id"))
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"configs": views})
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

func (h *handler) configStatus(w http.ResponseWriter, r *http.Request) {
	status, err := h.store.GetConfigStatus(r.PathValue("id"))
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"targetVersion":  status.TargetVersion,
		"appliedVersion": status.AppliedVersion,
		"failureReason":  status.FailureReason,
	})
}

// nonEmptyJSONObject validates that raw is exactly one non-empty JSON object
// and returns its original bytes (equality is semantic, so whitespace and key
// order do not matter). It writes a 400 itself for every malformed or
// wrong-shaped value.
func nonEmptyJSONObject(w http.ResponseWriter, raw json.RawMessage) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || !isNonEmptyJSONObject(trimmed) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "config must be a non-empty JSON object"})
		return nil, false
	}
	return trimmed, true
}
