# Edge Fleet Hub

Edge Fleet Hub is a local-first simulator for managing edge devices without requiring physical hardware or an external broker. It provides device registration, telemetry ingestion, offline batch replay with deduplication, telemetry history with cursor pagination, and fleet snapshots. All state is held in local memory: restarting the process clears every device, sequence number, and batch receipt.

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

Every telemetry write is appended to the device's history with a receive-time sampling instant and a sequence number assigned per device, starting at 1 with no gaps.

## Replay offline batches

Submit a batch of samples collected while the device was offline. Each batch has a `batchId` and 1–100 samples; each sample has an `eventId`, an RFC3339 `observedAt` instant, and a non-empty `values` object of finite numbers.

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/replay \
  -H 'Content-Type: application/json' \
  -d '{
    "batchId": "batch-2026-0001",
    "samples": [
      {"eventId": "evt-001", "observedAt": "2026-09-30T08:15:00Z", "values": {"temperature": 22.4, "humidity": 45.2}},
      {"eventId": "evt-002", "observedAt": "2026-09-30T08:16:00Z", "values": {"temperature": 22.6}}
    ]
  }'
```

A new batch returns `202` with a receipt listing every sample's sequence number and status (`new` or `duplicate`), plus `newCount` and `duplicateCount`. New samples are stored in array order; the last new sample updates the snapshot telemetry, and the receive time updates the device's last-active time.

Deduplication rules (all scoped to the device):

- `eventId` is unique per device. A sample whose `eventId` already exists with identical content — the same UTC instant and the same values key-by-key — is a duplicate and reuses the original sequence number.
- A batch resubmitted with the same `batchId` and identical ordered content returns `200` with the first receipt and adds nothing.
- Any content conflict — an existing `eventId` submitted with a different instant or values, or a `batchId` reused with different ordered samples — returns `409`. The whole batch is rolled back: no samples, sequence numbers, or receipts are created.
- Pure duplicate batches and failed requests do not refresh the device's last-active time or snapshot.

Validation errors (blank identifiers or metric names, non-finite values, duplicate `eventId` within a batch, batch outside 1–100 samples, malformed or non-single JSON body) return `400` without changing state. Replaying against an unknown device returns `404`.

## Query history

```bash
curl -sS 'http://127.0.0.1:8080/v1/devices/gateway-01/history?from=2026-09-30T00:00:00Z&to=2026-10-01T00:00:00Z&limit=20'
```

Returns entries in ascending receive-sequence order:

```json
{
  "samples": [
    {"seq": 1, "observedAt": "2026-09-30T08:15:00Z", "values": {"temperature": 22.4, "humidity": 45.2}},
    {"seq": 2, "observedAt": "2026-09-30T08:16:00Z", "values": {"temperature": 22.6}}
  ],
  "nextCursor": null
}
```

- `from` and `to` are inclusive RFC3339 bounds; an inverted range or invalid parameter returns `400`.
- `limit` defaults to 20 and must be between 1 and 100.
- `nextCursor` is an opaque pagination cursor; the last page returns `null`. The first page fixes the then-maximum sequence number, so writes arriving between pages never cause skips or duplicates.
- A cursor is bound to the device and the exact `from`/`to` filters of its first page: using it against another device, with changed filters, or after tampering returns `400`.
- Querying history of an unknown device returns `404`.

## Test

```bash
go test ./...
```

The tests cover: direct telemetry entering history, cross-batch event deduplication, conflict rollback with no state leakage, idempotent resubmission, concurrent batches and same-event dedup, stable pagination across writes arriving between pages, filter and cursor validation, and snapshot updates from the last new sample.
