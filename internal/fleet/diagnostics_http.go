package fleet

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// --- remote diagnostic tasks ------------------------------------------------

// createTaskRequest is the operator's task creation envelope. Seconds is a
// pointer so a missing or non-integer value is distinguishable from a valid 0
// (which is itself out of range).
type createTaskRequest struct {
	RequestID string `json:"requestId"`
	Seconds   *int   `json:"seconds"`
}

// reportTaskRequest is the simulated device's outcome envelope. Result is
// decoded as raw JSON and validated separately to be a JSON object.
type reportTaskRequest struct {
	Token     string          `json:"token"`
	ReceiptID string          `json:"receiptId"`
	Success   *bool           `json:"success"`
	Reason    string          `json:"reason"`
	Result    json.RawMessage `json:"result"`
}

func (h *handler) createDiagnosticTask(w http.ResponseWriter, r *http.Request) {
	var request createTaskRequest
	if err := decodeSingleJSON(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must be a single valid JSON object"})
		return
	}
	requestID := strings.TrimSpace(request.RequestID)
	if requestID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "requestId is required"})
		return
	}
	if request.Seconds == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "seconds is required"})
		return
	}
	if *request.Seconds < 1 || *request.Seconds > 60 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "seconds must be an integer between 1 and 60"})
		return
	}

	task, repeat, err := h.store.CreateDiagnosticTask(r.PathValue("id"), requestID, *request.Seconds)
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ErrTaskConflict) {
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
	writeJSON(w, status, task)
}

func (h *handler) claimDiagnosticTask(w http.ResponseWriter, r *http.Request) {
	claim, ok, err := h.store.ClaimDiagnosticTask(r.PathValue("id"))
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
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
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, claim)
}

func (h *handler) reportDiagnosticTask(w http.ResponseWriter, r *http.Request) {
	var request reportTaskRequest
	if err := decodeSingleJSON(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must be a single valid JSON object"})
		return
	}
	token := strings.TrimSpace(request.Token)
	if token == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "token is required"})
		return
	}
	receiptID := strings.TrimSpace(request.ReceiptID)
	if receiptID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "receiptId is required"})
		return
	}
	if request.Success == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "success is required"})
		return
	}
	reason := strings.TrimSpace(request.Reason)
	var result json.RawMessage
	if *request.Success {
		raw, valid := jsonObject(w, request.Result)
		if !valid {
			return
		}
		result = raw
	} else {
		if reason == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a failed report requires a non-blank reason"})
			return
		}
	}

	ack, repeat, err := h.store.ReportDiagnosticTask(r.PathValue("id"), token, receiptID, *request.Success, reason, result)
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ErrTaskConflict) {
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
	writeJSON(w, status, ack)
}

func (h *handler) cancelDiagnosticTask(w http.ResponseWriter, r *http.Request) {
	number, err := strconv.ParseInt(r.PathValue("number"), 10, 64)
	if err != nil || number < 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task number must be a positive integer"})
		return
	}
	cancellation, _, err := h.store.CancelDiagnosticTask(r.PathValue("id"), number)
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ErrTaskNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ErrTaskConflict) {
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
	writeJSON(w, http.StatusOK, cancellation)
}

func (h *handler) listDiagnosticTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := h.store.ListDiagnosticTasks(r.PathValue("id"))
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
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
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

func (h *handler) getDiagnosticTask(w http.ResponseWriter, r *http.Request) {
	number, err := strconv.ParseInt(r.PathValue("number"), 10, 64)
	if err != nil || number < 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task number must be a positive integer"})
		return
	}
	task, err := h.store.GetDiagnosticTask(r.PathValue("id"), number)
	if errors.Is(err, ErrDeviceNotFound) || errors.Is(err, ErrTaskNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
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
	writeJSON(w, http.StatusOK, task)
}

func (h *handler) listTaskAudit(w http.ResponseWriter, r *http.Request) {
	entries, err := h.store.ListTaskAudit(r.PathValue("id"))
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
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
	writeJSON(w, http.StatusOK, map[string]any{"audit": entries})
}

// jsonObject validates that raw is exactly one JSON object (an empty object is
// accepted) and returns its original bytes. It writes a 400 on any other
// shape.
func jsonObject(w http.ResponseWriter, raw json.RawMessage) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a successful report requires a JSON object result"})
		return nil, false
	}
	var value any
	if json.Unmarshal(trimmed, &value) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "result must be valid JSON"})
		return nil, false
	}
	if _, ok := value.(map[string]any); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "result must be a JSON object"})
		return nil, false
	}
	return trimmed, true
}
