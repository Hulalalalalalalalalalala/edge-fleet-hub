package fleet

import (
	"encoding/base64"
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
	_, device, err := h.store.RecordTelemetry(r.PathValue("id"), values)
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, device)
}

func (h *handler) replay(w http.ResponseWriter, r *http.Request) {
	var request struct {
		BatchID string `json:"batchId"`
		Samples []struct {
			EventID    string             `json:"eventId"`
			ObservedAt string             `json:"observedAt"`
			Values     map[string]float64 `json:"values"`
		} `json:"samples"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	batchID := strings.TrimSpace(request.BatchID)
	if batchID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "batchId is required"})
		return
	}
	if len(request.Samples) == 0 || len(request.Samples) > 100 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "samples must contain between 1 and 100 entries"})
		return
	}

	samples := make([]ReplaySample, 0, len(request.Samples))
	seenEventIDs := make(map[string]bool, len(request.Samples))
	for _, raw := range request.Samples {
		eventID := strings.TrimSpace(raw.EventID)
		if eventID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "eventId is required"})
			return
		}
		if seenEventIDs[eventID] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "eventId must be unique within a batch"})
			return
		}
		seenEventIDs[eventID] = true

		observedAt, err := time.Parse(time.RFC3339, raw.ObservedAt)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "observedAt must be an RFC3339 timestamp"})
			return
		}
		if len(raw.Values) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "values must be a non-empty object"})
			return
		}
		values := make(map[string]float64, len(raw.Values))
		for name, value := range raw.Values {
			trimmedName := strings.TrimSpace(name)
			if trimmedName == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "metric names must not be blank"})
				return
			}
			if math.IsNaN(value) || math.IsInf(value, 0) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "values must be finite numbers"})
				return
			}
			values[trimmedName] = value
		}
		samples = append(samples, ReplaySample{
			EventID:    eventID,
			ObservedAt: observedAt.UTC(),
			Values:     values,
		})
	}

	receipt, idempotent, err := h.store.ReplayBatch(r.PathValue("id"), batchID, samples)
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ErrConflict) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	status := http.StatusAccepted
	if idempotent {
		status = http.StatusOK
	}
	writeJSON(w, status, receipt)
}

type historyResponse struct {
	Samples    []historySample `json:"samples"`
	NextCursor *string         `json:"nextCursor"`
}

type historySample struct {
	Seq        int64              `json:"seq"`
	ObservedAt time.Time          `json:"observedAt"`
	Values     map[string]float64 `json:"values"`
}

type historyCursor struct {
	Device string     `json:"d"`
	From   *time.Time `json:"f,omitempty"`
	To     *time.Time `json:"t,omitempty"`
	Upper  int64      `json:"u"`
	Last   int64      `json:"s"`
}

func (h *handler) history(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	query := r.URL.Query()

	from, err := parseTimeParam(query.Get("from"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "from must be an RFC3339 timestamp"})
		return
	}
	to, err := parseTimeParam(query.Get("to"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "to must be an RFC3339 timestamp"})
		return
	}
	if from != nil && to != nil && from.After(*to) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "from must not be after to"})
		return
	}

	limit := 20
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be between 1 and 100"})
			return
		}
		limit = parsed
	}

	var upper, after int64
	if query.Has("cursor") {
		raw := query.Get("cursor")
		if raw == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cursor"})
			return
		}
		cursor, err := decodeHistoryCursor(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cursor"})
			return
		}
		if cursor.Device != id || !timePtrEqual(cursor.From, from) || !timePtrEqual(cursor.To, to) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cursor does not match this request"})
			return
		}
		maxSeq, found := h.store.MaxSeq(id)
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": ErrDeviceNotFound.Error()})
			return
		}
		if cursor.Upper < 1 || cursor.Upper > maxSeq || cursor.Last < 0 || cursor.Last >= cursor.Upper {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cursor"})
			return
		}
		upper, after = cursor.Upper, cursor.Last
	} else {
		maxSeq, found := h.store.MaxSeq(id)
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": ErrDeviceNotFound.Error()})
			return
		}
		upper, after = maxSeq, 0
	}

	entries, err := h.store.QueryHistory(id, from, to, upper, after, limit+1)
	if errors.Is(err, ErrDeviceNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	response := historyResponse{Samples: make([]historySample, 0, limit)}
	for _, entry := range entries {
		if len(response.Samples) >= limit {
			break
		}
		response.Samples = append(response.Samples, historySample{
			Seq:        entry.Seq,
			ObservedAt: entry.ObservedAt,
			Values:     entry.Values,
		})
	}
	if len(entries) > limit {
		cursor := encodeHistoryCursor(historyCursor{
			Device: id,
			From:   from,
			To:     to,
			Upper:  upper,
			Last:   entries[limit-1].Seq,
		})
		response.NextCursor = &cursor
	}
	writeJSON(w, http.StatusOK, response)
}

func parseTimeParam(raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, err
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

func timePtrEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func encodeHistoryCursor(cursor historyCursor) string {
	raw, err := json.Marshal(cursor)
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeHistoryCursor(raw string) (historyCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return historyCursor{}, err
	}
	var cursor historyCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return historyCursor{}, err
	}
	if cursor.Device == "" {
		return historyCursor{}, errors.New("invalid cursor")
	}
	return cursor, nil
}

func (h *handler) snapshot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"devices": h.store.Snapshot()})
}

// decodeJSON decodes a single JSON value. Trailing content (including a second
// JSON value) is rejected so the body is exactly one JSON document.
func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("body must contain a single JSON value")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
