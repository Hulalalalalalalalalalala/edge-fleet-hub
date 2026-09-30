package fleet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestHandler(t *testing.T) (http.Handler, *Store) {
	t.Helper()
	store := NewStore()
	return NewHandler(store), store
}

func registerDevice(t *testing.T, h http.Handler, id string) {
	t.Helper()
	rec := doJSON(t, h, http.MethodPost, "/v1/devices", fmt.Sprintf(`{"id":%q,"site":"lab"}`, id))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register %s: status=%d body=%s", id, rec.Code, rec.Body.String())
	}
}

func doJSON(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), target); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
}
