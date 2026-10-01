# Edge Fleet Hub

Edge Fleet Hub is a local-first simulator for managing edge devices without requiring physical hardware or an external broker. The service provides device registration, heartbeat updates, live telemetry ingestion, fleet snapshots, telemetry history with bounded pagination, and offline batch replay with per-device deduplication. It is intended to grow into a complete device operations platform with rules and alerts, configuration delivery, messaging, and auditable remote maintenance.

All state is held in local memory: no hardware, database, or external service is required.

## Run

```bash
go run ./cmd/edge-fleet
```

The service listens on `127.0.0.1:8080` by default. Override it with `EDGE_FLEET_ADDR`.

Register a device and send live telemetry:

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/devices \
  -H 'Content-Type: application/json' \
  -d '{"id":"gateway-01","site":"warehouse-a"}'

curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/telemetry \
  -H 'Content-Type: application/json' \
  -d '{"temperature":23.5,"battery":91}'

curl -sS http://127.0.0.1:8080/v1/fleet
```

## Offline batch replay

`POST /v1/devices/{id}/replay` submits a batch of samples captured offline.
Each sample needs a unique `eventId`, an RFC3339 `observedAt`, and a non-empty
`values` map of finite numbers; a batch holds 1–100 samples. Both `eventId`
and `batchId` are deduplicated **within the device**:

- A new batch returns `202 Accepted`. The receipt lists each sample's assigned
  receive sequence plus `newCount`/`duplicateCount`.
- An `eventId` already known to the device with the same UTC instant and the
  same key/value pairs is a duplicate: it reuses the original sequence and is
  not stored again. Different time or values for the same `eventId` conflicts
  the whole batch (`409`).
- Re-submitting the same `batchId` with identical ordered content returns `200
  OK` and the first receipt. Changed content returns `409`.
- Conflicts and validation errors roll the whole batch back: no samples,
  sequences, or receipts remain, and device state (last active time / last
  telemetry) is not refreshed by duplicates or failures.

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/replay \
  -H 'Content-Type: application/json' \
  -d '{
        "batchId": "offline-dump-2024-01-02",
        "samples": [
          {"eventId":"evt-1","observedAt":"2024-01-02T10:00:00Z","values":{"temperature":21.0}},
          {"eventId":"evt-2","observedAt":"2024-01-02T10:05:00Z","values":{"temperature":21.4,"battery":90}}
        ]
      }'
# 202 -> {"batchId":"...","newCount":2,"duplicateCount":0,
#         "samples":[{"eventId":"evt-1","sequence":2,"duplicate":false}, ...]}
```

Duplicate detection compares times as UTC instants (`2024-01-02T18:00:00+08:00`
equals `2024-01-02T10:00:00Z`) and values by key/value.

## Telemetry history

Live telemetry and accepted replay samples all enter the device history.
Per-device receive sequences start at 1 and increase contiguously; live
telemetry is timestamped at server receive time.

`GET /v1/devices/{id}/history` returns events in ascending receive sequence and
supports:

- `from` / `to`: RFC3339 observed-time bounds, both inclusive.
- `limit`: 1–100 matching events per page (default 20).
- `cursor`: opaque continuation token from the previous page; `nextCursor` is
  `null` on the last page.

```bash
curl -sS 'http://127.0.0.1:8080/v1/devices/gateway-01/history?from=2024-01-02T00:00:00Z&to=2024-01-03T00:00:00Z&limit=50'

# follow nextCursor
curl -sS 'http://127.0.0.1:8080/v1/devices/gateway-01/history?limit=50&cursor=<nextCursor>'
```

The first page pins the maximum sequence visible at that moment; later pages
only read within that bound, so writes arriving while paging can neither skip
nor duplicate rows (start a fresh first page to see newer data). A cursor is
only valid for its device and filter — reusing it on another device, with a
different filter, or after tampering returns `400`; an unknown device returns
`404`.

## Persistence and restart

By default everything (devices, events, sequences, batch receipts) lives in
process memory: **restarting the process clears all data** and sequence
counters restart from 1.

Set `EDGE_FLEET_DATA_DIR` to a local directory to enable persistent mode. The
directory is created if missing; an empty directory starts fresh. The service
holds an exclusive lock on the directory, so a second process using the same
directory fails before accepting requests. The lock is released automatically
on normal exit or forced termination — no manual cleanup is needed.

```bash
EDGE_FLEET_DATA_DIR=/var/lib/edge-fleet-hub go run ./cmd/edge-fleet
```

In persistent mode every write (registration, telemetry, replay) is made
durable before success is returned: a write is either fully committed or not
present at all, so a crash mid-write never leaves partial data. Device
information, registration and last-active times, last telemetry, full history,
receive sequences, event dedup records and batch receipts all survive restart;
recovery itself does not refresh device times, and new samples continue the
original sequence. Re-submitting a committed batch returns the first receipt;
changed content still returns `409`. History continuation cursors remain valid
across restart and stay pinned to the first-query sequence bound; a cursor
minted in another data directory is rejected with `400`.

If the directory cannot be created, read, or written, if it is already in use,
or if committed data is corrupt or uses an unsupported format, the service
fails at startup with a reason and keeps the original files. A write that
cannot be saved during runtime returns `503` without changing queryable state,
consuming a sequence, or leaving a receipt; queries keep working and the write
can be retried once storage recovers. No database or external messaging
service is used.

## Test

```bash
go test ./...
```
