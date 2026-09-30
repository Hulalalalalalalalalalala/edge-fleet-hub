package fleet

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
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
	mux.HandleFunc("GET /v1/fleet", h.snapshot)
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
	device, created := h.store.Register(strings.TrimSpace(request.ID), strings.TrimSpace(request.Site))
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
	device, err := h.store.RecordTelemetry(r.PathValue("id"), values)
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, device)
}

func (h *handler) snapshot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"devices": h.store.Snapshot()})
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
