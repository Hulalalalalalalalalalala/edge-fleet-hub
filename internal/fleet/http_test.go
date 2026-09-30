package fleet

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegisterTelemetryAndSnapshot(t *testing.T) {
	h := NewHandler(NewStore())

	register := httptest.NewRequest(http.MethodPost, "/v1/devices", strings.NewReader(`{"id":"gateway-01","site":"lab"}`))
	register.Header.Set("Content-Type", "application/json")
	registerResult := httptest.NewRecorder()
	h.ServeHTTP(registerResult, register)
	if registerResult.Code != http.StatusCreated {
		t.Fatalf("register status = %d, want %d: %s", registerResult.Code, http.StatusCreated, registerResult.Body.String())
	}

	telemetry := httptest.NewRequest(http.MethodPost, "/v1/devices/gateway-01/telemetry", strings.NewReader(`{"temperature":21.75}`))
	telemetry.Header.Set("Content-Type", "application/json")
	telemetryResult := httptest.NewRecorder()
	h.ServeHTTP(telemetryResult, telemetry)
	if telemetryResult.Code != http.StatusAccepted {
		t.Fatalf("telemetry status = %d, want %d: %s", telemetryResult.Code, http.StatusAccepted, telemetryResult.Body.String())
	}

	snapshot := httptest.NewRequest(http.MethodGet, "/v1/fleet", nil)
	snapshotResult := httptest.NewRecorder()
	h.ServeHTTP(snapshotResult, snapshot)
	if snapshotResult.Code != http.StatusOK || !strings.Contains(snapshotResult.Body.String(), `"temperature":21.75`) {
		t.Fatalf("unexpected snapshot: status=%d body=%s", snapshotResult.Code, snapshotResult.Body.String())
	}
}

func TestTelemetryRejectsUnknownDevice(t *testing.T) {
	h := NewHandler(NewStore())
	request := httptest.NewRequest(http.MethodPost, "/v1/devices/missing/telemetry", strings.NewReader(`{"temperature":20}`))
	result := httptest.NewRecorder()
	h.ServeHTTP(result, request)
	if result.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", result.Code, http.StatusNotFound)
	}
}
