# Edge Fleet Hub

Edge Fleet Hub is a local-first simulator for managing edge devices without requiring physical hardware or an external broker. The initial baseline provides a small HTTP service for device registration, heartbeat updates, telemetry ingestion, and fleet snapshots. It is intended to grow into a complete device operations platform with offline synchronization, rules and alerts, configuration delivery, messaging, and auditable remote maintenance.

## Run

```bash
go run ./cmd/edge-fleet
```

The service listens on `127.0.0.1:8080` by default. Override it with `EDGE_FLEET_ADDR`.

Register a device and send telemetry:

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/devices \
  -H 'Content-Type: application/json' \
  -d '{"id":"gateway-01","site":"warehouse-a"}'

curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/telemetry \
  -H 'Content-Type: application/json' \
  -d '{"temperature":23.5,"battery":91}'

curl -sS http://127.0.0.1:8080/v1/fleet
```

## Test

```bash
go test ./...
```
