package fleet

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type handler struct{ store *Store }

func NewHandler(store *Store) http.Handler {
	h := &handler{store: store}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/devices", h.register)
	mux.HandleFunc("POST /v1/devices/{id}/telemetry", h.telemetry)
	mux.HandleFunc("POST /v1/devices/{id}/replay", h.replay)
	mux.HandleFunc("GET /v1/devices/{id}/history", h.history)
	mux.HandleFunc("GET /v1/fleet", h.snapshot)
	mux.HandleFunc("POST /v1/devices/{id}/rules", h.createRule)
	mux.HandleFunc("GET /v1/devices/{id}/rules", h.listRules)
	mux.HandleFunc("GET /v1/devices/{id}/rules/{ruleId}", h.getRule)
	mux.HandleFunc("PUT /v1/devices/{id}/rules/{ruleId}", h.updateRule)
	mux.HandleFunc("POST /v1/devices/{id}/alerts/{alertId}/acknowledge", h.ackAlert)
	mux.HandleFunc("GET /v1/devices/{id}/alerts", h.listAlerts)
	mux.HandleFunc("POST /v1/devices/{id}/configs", h.publishConfig)
	mux.HandleFunc("GET /v1/devices/{id}/configs", h.listConfigs)
	mux.HandleFunc("GET /v1/devices/{id}/configs/pending", h.pendingConfig)
	mux.HandleFunc("GET /v1/devices/{id}/configs/status", h.configStatus)
	mux.HandleFunc("POST /v1/devices/{id}/configs/receipts", h.receiveConfigReceipt)
	mux.HandleFunc("GET /v1/devices/{id}/configs/receipts", h.listConfigReceipts)

	// Remote diagnostic tasks.
	mux.HandleFunc("POST /v1/devices/{id}/diagnostic-tasks", h.createDiagnosticTask)
	mux.HandleFunc("GET /v1/devices/{id}/diagnostic-tasks", h.listDiagnosticTasks)
	mux.HandleFunc("POST /v1/devices/{id}/diagnostic-tasks/claim", h.claimDiagnosticTask)
	mux.HandleFunc("POST /v1/devices/{id}/diagnostic-tasks/reports", h.reportDiagnosticTask)
	mux.HandleFunc("GET /v1/devices/{id}/diagnostic-tasks/audit", h.listTaskAudit)
	mux.HandleFunc("GET /v1/devices/{id}/diagnostic-tasks/{number}", h.getDiagnosticTask)
	mux.HandleFunc("POST /v1/devices/{id}/diagnostic-tasks/{number}/cancel", h.cancelDiagnosticTask)
	return mux
}

func (h *handler) register(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID   string `json:"id"`
		Site string `json:"site"`
	}
	if err := decodeJSON(r, &request); err != nil || strings.TrimSpace(request.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required"})
		return
	}
	device, created, err := h.store.Register(strings.TrimSpace(request.ID), strings.TrimSpace(request.Site))
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
	writeJSON(w, status, device)
}

func (h *handler) telemetry(w http.ResponseWriter, r *http.Request) {
	values := map[string]float64{}
	if err := decodeJSON(r, &values); err != nil || len(values) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "telemetry values are required"})
		return
	}
	if !finiteValues(values) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "telemetry values must be finite"})
		return
	}
	device, err := h.store.RecordTelemetry(r.PathValue("id"), values)
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
	writeJSON(w, http.StatusAccepted, device)
}

type replayRequest struct {
	BatchID string `json:"batchId"`
	Samples []struct {
		EventID    string             `json:"eventId"`
		ObservedAt string             `json:"observedAt"`
		Values     map[string]float64 `json:"values"`
	} `json:"samples"`
}

func (h *handler) replay(w http.ResponseWriter, r *http.Request) {
	var request replayRequest
	if err := decodeSingleJSON(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must be a single valid JSON object"})
		return
	}
	if strings.TrimSpace(request.BatchID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "batchId is required"})
		return
	}
	if len(request.Samples) < 1 || len(request.Samples) > 100 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "samples must contain between 1 and 100 entries"})
		return
	}
	samples := make([]Sample, len(request.Samples))
	seenEventIDs := make(map[string]struct{}, len(request.Samples))
	for i, entry := range request.Samples {
		eventID := strings.TrimSpace(entry.EventID)
		if eventID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "each sample requires a non-blank eventId"})
			return
		}
		if _, dup := seenEventIDs[eventID]; dup {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "eventId values must be unique within a batch"})
			return
		}
		seenEventIDs[eventID] = struct{}{}
		observedAt, err := time.Parse(time.RFC3339, entry.ObservedAt)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "observedAt must be RFC3339 formatted"})
			return
		}
		if len(entry.Values) == 0 || hasBlankKey(entry.Values) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "values must be a non-empty map with non-blank metric names"})
			return
		}
		if !finiteValues(entry.Values) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "values must be finite numbers"})
			return
		}
		samples[i] = Sample{
			EventID:    eventID,
			ObservedAt: observedAt.UTC(),
			Values:     cloneTelemetry(entry.Values),
		}
	}

	receipt, repeat, err := h.store.Replay(r.PathValue("id"), strings.TrimSpace(request.BatchID), samples)
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ErrBatchConflict) {
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
	status := http.StatusAccepted
	if repeat {
		status = http.StatusOK
	}
	writeJSON(w, status, receipt)
}

func (h *handler) history(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	query := r.URL.Query()

	filter := HistoryFilter{}
	if raw := strings.TrimSpace(query.Get("from")); raw != "" {
		from, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "from must be RFC3339 formatted"})
			return
		}
		filter.From = from.UTC()
	}
	if raw := strings.TrimSpace(query.Get("to")); raw != "" {
		to, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "to must be RFC3339 formatted"})
			return
		}
		filter.To = to.UTC()
	}
	if !filter.From.IsZero() && !filter.To.IsZero() && filter.From.After(filter.To) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "from must not be after to"})
		return
	}

	limit := 20
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be between 1 and 100"})
			return
		}
		limit = value
	}

	var afterIndex, highWater int64
	rawCursor := strings.TrimSpace(query.Get("cursor"))
	if rawCursor != "" {
		// An unknown device is always 404, even with a malformed or foreign
		// cursor; cursor problems on a registered device are 400.
		if !h.store.Exists(id) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": ErrDeviceNotFound.Error()})
			return
		}
		cursor, err := decodeCursor(rawCursor)
		if err != nil || cursor.DeviceID != id ||
			!bytes.Equal(cursor.IID, h.store.instanceID()) ||
			!cursor.From.Equal(filter.From) || !cursor.To.Equal(filter.To) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cursor"})
			return
		}
		afterIndex, highWater = cursor.ScanPos, cursor.HighWater
	}

	events, appliedBound, nextIndex, hasMore, err := h.store.History(id, filter, afterIndex, highWater, limit)
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	highWater = appliedBound

	var nextCursor any
	if hasMore {
		nextCursor = encodeCursor(pageCursor{
			DeviceID:  id,
			From:      filter.From,
			To:        filter.To,
			HighWater: highWater,
			ScanPos:   nextIndex,
			IID:       h.store.instanceID(),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"deviceId":   id,
		"events":     events,
		"nextCursor": nextCursor,
	})
}

func (h *handler) snapshot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"devices": h.store.Snapshot()})
}

// --- rules ------------------------------------------------------------------

func (h *handler) createRule(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID      string  `json:"id"`
		Metric  string  `json:"metric"`
		Trigger float64 `json:"trigger"`
		Recover float64 `json:"recover"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must be a single valid JSON object"})
		return
	}
	ruleID := strings.TrimSpace(request.ID)
	metric := strings.TrimSpace(request.Metric)
	if ruleID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required"})
		return
	}
	if metric == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "metric is required"})
		return
	}
	if !finiteFloat(request.Trigger) || !finiteFloat(request.Recover) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "thresholds must be finite numbers"})
		return
	}
	if request.Recover >= request.Trigger {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "recover threshold must be less than trigger threshold"})
		return
	}
	rule, err := h.store.CreateRule(r.PathValue("id"), ruleID, metric, request.Trigger, request.Recover)
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ErrRuleConflict) {
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
	writeJSON(w, http.StatusCreated, rule)
}

func (h *handler) listRules(w http.ResponseWriter, r *http.Request) {
	rules, err := h.store.ListRules(r.PathValue("id"))
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

func (h *handler) getRule(w http.ResponseWriter, r *http.Request) {
	rule, err := h.store.GetRule(r.PathValue("id"), r.PathValue("ruleId"))
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ErrRuleNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (h *handler) updateRule(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Metric  *string  `json:"metric"`
		Trigger *float64 `json:"trigger"`
		Recover *float64 `json:"recover"`
		Enabled *bool    `json:"enabled"`
		Version int64    `json:"version"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must be a single valid JSON object"})
		return
	}
	if request.Version < 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "version is required"})
		return
	}
	if request.Metric != nil && strings.TrimSpace(*request.Metric) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "metric must not be blank"})
		return
	}
	if request.Trigger != nil && !finiteFloat(*request.Trigger) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "trigger must be a finite number"})
		return
	}
	if request.Recover != nil && !finiteFloat(*request.Recover) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "recover must be a finite number"})
		return
	}
	update := ruleUpdate{}
	if request.Metric != nil {
		metric := strings.TrimSpace(*request.Metric)
		update.Metric = &metric
	}
	if request.Trigger != nil {
		update.Trigger = request.Trigger
	}
	if request.Recover != nil {
		update.Recover = request.Recover
	}
	if request.Enabled != nil {
		update.Enabled = request.Enabled
	}
	rule, _, err := h.store.UpdateRule(r.PathValue("id"), r.PathValue("ruleId"), update, request.Version)
	if errors.Is(err, ErrDeviceNotFound) || errors.Is(err, ErrRuleNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ErrVersionConflict) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ErrInvalidRule) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "recover threshold must be less than trigger threshold"})
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
	writeJSON(w, http.StatusOK, rule)
}

// --- alerts -----------------------------------------------------------------

func (h *handler) listAlerts(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := AlertFilter{RuleID: strings.TrimSpace(query.Get("ruleId"))}
	if raw := strings.TrimSpace(query.Get("status")); raw != "" {
		if raw != alertStatusActive && raw != alertStatusEnded {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be active or ended"})
			return
		}
		filter.Status = raw
	}
	if raw := strings.TrimSpace(query.Get("acknowledged")); raw != "" {
		switch raw {
		case "true", "1":
			v := true
			filter.Acknowledged = &v
		case "false", "0":
			v := false
			filter.Acknowledged = &v
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "acknowledged must be true or false"})
			return
		}
	}
	alerts, err := h.store.ListAlerts(r.PathValue("id"), filter)
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ErrRuleNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": alerts})
}

func (h *handler) ackAlert(w http.ResponseWriter, r *http.Request) {
	alertID, err := strconv.ParseInt(r.PathValue("alertId"), 10, 64)
	if err != nil || alertID < 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "alertId must be a positive integer"})
		return
	}
	alert, err := h.store.AckAlert(r.PathValue("id"), alertID)
	if errors.Is(err, ErrDeviceNotFound) || errors.Is(err, ErrAlertNotFound) {
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
	writeJSON(w, http.StatusOK, alert)
}

func finiteFloat(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func hasBlankKey(values map[string]float64) bool {
	for key := range values {
		if strings.TrimSpace(key) == "" {
			return true
		}
	}
	return false
}

func finiteValues(values map[string]float64) bool {
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	return true
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

// decodeSingleJSON requires the body to be exactly one JSON value, rejecting
// trailing data such as a second JSON object or concatenated documents.
func decodeSingleJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("body must contain a single JSON value")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
