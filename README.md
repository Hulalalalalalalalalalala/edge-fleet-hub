# Edge Fleet Hub

Edge Fleet Hub is a local-first simulator for managing edge devices without requiring physical hardware or an external broker. The service provides device registration, heartbeat updates, live telemetry ingestion, fleet snapshots, telemetry history with bounded pagination, offline batch replay with per-device deduplication, threshold rules with alerts, and per-device configuration publishing and application receipts. It is intended to grow into a complete device operations platform with messaging and auditable remote maintenance.

All state is held in local memory by default: no hardware, database, or external service is required.

## Run

```bash
go run ./cmd/edge-fleet
```

The service listens on `127.0.0.1:8080` by default. Override it with `EDGE_FLEET_ADDR`.

### Optional local persistence

Set `EDGE_FLEET_DATA_DIR` to a directory for durable storage; leave it unset to
keep the original in-memory behaviour. The listening configuration, HTTP
endpoints and response formats are unchanged.

```bash
EDGE_FLEET_DATA_DIR=./fleet-data go run ./cmd/edge-fleet
```

- The directory is created if missing; an empty directory simply starts fresh.
- If the directory cannot be created, read or written, startup fails with an
  explanatory error — the service never silently falls back to memory mode.
- All successful writes are flushed to a local write-ahead log before the
  response is returned. Each write request is one commit unit, in particular a
  replay batch (its samples, sequences, device state and receipt commit or
  roll back together). If a write cannot be persisted at runtime the request
  fails with `503`: no sequence is consumed, no receipt is kept and previously
  queryable state is untouched; the same request can be retried once storage is
  healthy again.
- After a normal exit, a crash or a forced kill, reopening the same directory
  restores devices, registration and last-active times, last telemetry, the
  complete histories with their receive sequences, per-device event
  deduplication records and batch receipts, rules and alerts, and the complete
  configuration delivery state (published versions, target/applied versions,
  application receipts and publish/receipt deduplication records). Recovery
  never refreshes device timestamps; new samples continue the previous
  sequence. Re-submitting a batch that had already succeeded returns the first
  receipt (`200`); changing its content still returns `409`. The same applies
  to configuration publish requests and application receipts.
- History continuation tokens issued before a restart remain valid afterwards
  and keep their pinned sequence high-water mark. Using a cursor against a
  different data directory (even one with the same device ids) returns `400`;
  unknown devices still return `404`.
- Only one server process may open a given data directory at a time; a second
  process fails before accepting requests and never modifies existing data.
  The lock is released automatically on exit or kill — no manual cleanup is
  needed before restarting.
- If committed data is corrupt, uses an unsupported format, or contains
  inconsistent records, startup fails with an explanatory error and leaves the
  files untouched rather than discarding records.
- Persistence depends only on local files; no database or external service is
  involved.

The data directory holds three files: `wal.log` (checksummed record log),
`commit.log` (fixed-width commit journal marking which WAL bytes are
acknowledged) and `lock` (the inter-process lock). They are managed entirely by
the server; copying a directory offline while no process holds it is a valid
backup, but the files should not be hand-edited.

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

## Configuration delivery

Each registered device has its own configuration version line. Versions are
per-device integers starting at 1; before the first publish both the target
and applied version read as `0`, independently for every device.

`POST /v1/devices/{id}/configs` publishes a complete configuration:

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/configs \
  -H 'Content-Type: application/json' \
  -d '{"requestId":"cfg-2024-01-02-1","baseVersion":0,
       "config":{"network":{"ssid":"warehouse-a"},"policies":[1,2,3]}}'
# 201 -> {"requestId":"...","version":1,"config":{...},"publishedAt":"..."}
```

- The request needs a non-blank `requestId`, a non-negative integer
  `baseVersion`, and `config` as a non-empty JSON object; nested objects,
  arrays and ordinary JSON values (strings, numbers, booleans, null) are
  allowed.
- A new version is created only when `baseVersion` equals the current target.
  It returns `201` with the new version, the complete content and the first
  publish time. A stale base returns `409` and consumes no version, so two
  concurrent publishers racing on the same base produce exactly one `201` and
  the rest `409`.
- History versions are immutable. To restore an older configuration, publish
  its content again as a new version.
- `requestId` is deduplicated **within the device**. Retrying the same
  `requestId` with the same base and content returns `200` with the first
  result, never a new version. Same id but a different base or content is
  `409`. Equality ignores object key order and whitespace, compares numbers by
  value, and keeps array order significant.

The simulated device reads only the newest target — old versions are never
delivered one by one:

```bash
curl -sS http://127.0.0.1:8080/v1/devices/gateway-01/configs/pending
# 200 -> {"version":1,"config":{...},"publishedAt":"..."}
# 204    -> nothing published yet, or the latest version is already applied
```

Repeated reads do not change state. The device reports the application result:

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/configs/receipts \
  -d '{"receiptId":"dev-rc-1","version":1,"success":true}'            # 201
# on failure:
curl -sS -X POST .../configs/receipts \
  -d '{"receiptId":"dev-rc-2","version":1,"success":false,"reason":"disk full"}'
```

- A receipt needs a non-blank `receiptId`, a positive integer `version`, and a
  `success` boolean; a failure requires a non-blank `reason`. An unknown
  version is `404`.
- A new receipt for a version below the applied one is `409`; reporting a
  failure for a version that already succeeded is also `409`.
- Success advances the applied version to that version. A lagging success
  (the device applied an older target while a newer one exists) is recorded,
  but the new target stays pending. A failure never advances, so the latest
  configuration stays readable for a retry and the failure reason is exposed
  in the status view.
- `receiptId` is deduplicated independently of `requestId`: a new receipt is
  `201`; the same id with the same version/result/reason retries as `200`
  returning the first result and receive time; the same id with different
  content is `409`.

Queries:

- `GET /v1/devices/{id}/configs/status` —
  `{"targetVersion","appliedVersion","failureReason"}`; `failureReason` is the
  reason of the most recent failure still concerning an unapplied version.
- `GET /v1/devices/{id}/configs` — every published version with its complete
  content, in ascending version order.
- `GET /v1/devices/{id}/configs/receipts` — receipts in first-receive order,
  each with its first receive time.

Missing fields, wrong types and bodies containing more than one JSON document
return `400`; an unknown device returns `404`. Configuration operations never
modify telemetry history, device activity times, rules or alerts.

## Persistence and restart

Without `EDGE_FLEET_DATA_DIR`, everything (devices, events, sequences, batch
receipts) lives in process memory and **restarting the process clears all
data**; sequence counters restart from 1 for each freshly registered device.

With `EDGE_FLEET_DATA_DIR` set, the same state survives process restarts via
the local write-ahead log described under [Optional local
persistence](#optional-local-persistence) above.

## Test

```bash
go test ./...
```
