package fleet

// Remote diagnostics HTTP handlers. A fleet operator creates and cancels
// tasks and reads lists, details and audit trails; the simulated device claims
// the earliest claimable task and reports its outcome. Task operations never
// touch telemetry, rules, configuration or device activity times.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// taskCreateRequest is the raw task creation envelope.
type taskCreateRequest struct {
	RequestID       string `json:"requestId"`
	DurationSeconds *int   `json:"durationSeconds"`
}

// taskReportRequest is the device outcome envelope.
type taskReportRequest struct {
	ReceiptID  string          `json:"receiptId"`
	Credential string          `json:"credential"`
	Success    *bool           `json:"success"`
	Reason     string          `json:"reason"`
	Result     json.RawMessage `json:"result"`
}

func (h *handler) createTask(w http.ResponseWriter, r *http.Request) {
	var request taskCreateRequest
	if err := decodeSingleJSON(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must be a single valid JSON object"})
		return
	}
	requestID := strings.TrimSpace(request.RequestID)
	if requestID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "requestId is required"})
		return
	}
	if request.DurationSeconds == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "durationSeconds is required"})
		return
	}
	if *request.DurationSeconds < taskMinDuration || *request.DurationSeconds > taskMaxDuration {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "durationSeconds must be between 1 and 60"})
		return
	}

	task, repeat, err := h.store.CreateTask(r.PathValue("id"), requestID, *request.DurationSeconds)
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

func (h *handler) listTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := h.store.ListTasks(r.PathValue("id"))
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

func (h *handler) getTask(w http.ResponseWriter, r *http.Request) {
	taskID, err := parseTaskID(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "taskId must be a positive integer"})
		return
	}
	task, err := h.store.GetTask(r.PathValue("id"), taskID)
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

// claimTask serves the simulated device's pull. Only the earliest-created task
// that has reached its claimable time is returned; 204 means nothing is
// claimable (including when another task is still executing).
func (h *handler) claimTask(w http.ResponseWriter, r *http.Request) {
	view, claimed, err := h.store.ClaimTask(r.PathValue("id"))
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
	if !claimed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *handler) reportTask(w http.ResponseWriter, r *http.Request) {
	taskID, err := parseTaskID(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "taskId must be a positive integer"})
		return
	}
	var request taskReportRequest
	if err := decodeSingleJSON(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must be a single valid JSON object"})
		return
	}
	receiptID := strings.TrimSpace(request.ReceiptID)
	if receiptID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "receiptId is required"})
		return
	}
	credential := strings.TrimSpace(request.Credential)
	if credential == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "credential is required"})
		return
	}
	if request.Success == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "success is required"})
		return
	}
	reason := strings.TrimSpace(request.Reason)
	var result json.RawMessage
	if *request.Success {
		if len(request.Result) == 0 || !isJSONObject(request.Result) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a successful report requires a JSON object result"})
			return
		}
		result = request.Result
	} else if reason == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a failed report requires a non-blank reason"})
		return
	}

	report, repeat, err := h.store.ReportTask(r.PathValue("id"), taskID, receiptID, credential, *request.Success, reason, result)
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
	status := http.StatusCreated
	if repeat {
		status = http.StatusOK
	}
	writeJSON(w, status, report)
}

func (h *handler) cancelTask(w http.ResponseWriter, r *http.Request) {
	taskID, err := parseTaskID(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "taskId must be a positive integer"})
		return
	}
	task, err := h.store.CancelTask(r.PathValue("id"), taskID)
	if errors.Is(err, ErrDeviceNotFound) || errors.Is(err, ErrTaskNotFound) {
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
	writeJSON(w, http.StatusOK, task)
}

func (h *handler) listTaskAudit(w http.ResponseWriter, r *http.Request) {
	taskID, err := parseTaskID(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "taskId must be a positive integer"})
		return
	}
	audit, err := h.store.ListTaskAudit(r.PathValue("id"), taskID)
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
	writeJSON(w, http.StatusOK, map[string]any{"audit": audit})
}

func parseTaskID(r *http.Request) (int64, error) {
	taskID, err := strconv.ParseInt(r.PathValue("taskId"), 10, 64)
	if err != nil || taskID < 1 {
		return 0, err
	}
	return taskID, nil
}
