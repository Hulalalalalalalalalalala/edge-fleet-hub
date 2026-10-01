package fleet

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// --- alerting rules -----------------------------------------------------------

func (h *handler) createRule(w http.ResponseWriter, r *http.Request) {
	var request struct {
		RuleID  string  `json:"ruleId"`
		Metric  string  `json:"metric"`
		Trigger float64 `json:"trigger"`
		Recover float64 `json:"recover"`
	}
	if err := decodeSingleJSON(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must be a single valid JSON object"})
		return
	}
	ruleID := strings.TrimSpace(request.RuleID)
	metric := strings.TrimSpace(request.Metric)
	if ruleID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ruleId is required"})
		return
	}
	if err := validateRuleSpec(metric, request.Trigger, request.Recover); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	rule, err := h.store.CreateRule(r.PathValue("id"), ruleID, metric, request.Trigger, request.Recover)
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ErrRuleExists) {
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

func validateRuleSpec(metric string, trigger, recoverThreshold float64) error {
	if metric == "" {
		return errors.New("metric is required")
	}
	if !finiteValue(trigger) || !finiteValue(recoverThreshold) {
		return errors.New("trigger and recover thresholds must be finite numbers")
	}
	if recoverThreshold >= trigger {
		return errors.New("recover threshold must be lower than trigger threshold")
	}
	return nil
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
	writeJSON(w, http.StatusOK, map[string]any{"deviceId": r.PathValue("id"), "rules": rules})
}

func (h *handler) getRule(w http.ResponseWriter, r *http.Request) {
	rule, err := h.store.GetRule(r.PathValue("id"), r.PathValue("ruleId"))
	if errors.Is(err, ErrDeviceNotFound) || errors.Is(err, ErrRuleNotFound) {
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
		Metric  string  `json:"metric"`
		Trigger float64 `json:"trigger"`
		Recover float64 `json:"recover"`
		Version int64   `json:"version"`
	}
	if err := decodeSingleJSON(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must be a single valid JSON object"})
		return
	}
	metric := strings.TrimSpace(request.Metric)
	if err := validateRuleSpec(metric, request.Trigger, request.Recover); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if request.Version < 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "version is required"})
		return
	}
	rule, err := h.store.UpdateRule(r.PathValue("id"), r.PathValue("ruleId"), metric, request.Trigger, request.Recover, request.Version)
	h.writeRuleMutationResult(w, rule, err)
}

func (h *handler) setRuleEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Version int64 `json:"version"`
		}
		if err := decodeSingleJSON(r, &request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must be a single valid JSON object"})
			return
		}
		if request.Version < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "version is required"})
			return
		}
		rule, err := h.store.SetRuleEnabled(r.PathValue("id"), r.PathValue("ruleId"), enabled, request.Version)
		h.writeRuleMutationResult(w, rule, err)
	}
}

func (h *handler) writeRuleMutationResult(w http.ResponseWriter, rule Rule, err error) {
	switch {
	case errors.Is(err, ErrDeviceNotFound) || errors.Is(err, ErrRuleNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrVersionConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrStorageUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusOK, rule)
	}
}

// --- alerts -------------------------------------------------------------------

func (h *handler) listAlerts(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := AlertFilter{RuleID: strings.TrimSpace(query.Get("ruleId"))}
	if raw := strings.TrimSpace(query.Get("closed")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "closed must be true or false"})
			return
		}
		filter.Closed = &value
	}
	if raw := strings.TrimSpace(query.Get("acknowledged")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "acknowledged must be true or false"})
			return
		}
		filter.Acknowledged = &value
	}
	alerts, err := h.store.ListAlerts(r.PathValue("id"), filter)
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deviceId": r.PathValue("id"), "alerts": alerts})
}

func (h *handler) getAlert(w http.ResponseWriter, r *http.Request) {
	alert, err := h.store.GetAlert(r.PathValue("id"), parseAlertID(r))
	if errors.Is(err, ErrDeviceNotFound) || errors.Is(err, ErrAlertNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, alert)
}

func (h *handler) ackAlert(w http.ResponseWriter, r *http.Request) {
	alert, err := h.store.AckAlert(r.PathValue("id"), parseAlertID(r))
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

// parseAlertID converts the path parameter to an alert id; an unparseable id
// simply matches no alert, which the store reports as ErrAlertNotFound.
func parseAlertID(r *http.Request) int64 {
	alertID, err := strconv.ParseInt(r.PathValue("alertId"), 10, 64)
	if err != nil {
		return -1
	}
	return alertID
}
