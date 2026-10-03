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
  complete retained histories with their receive-sequence counters and
  retention limits, per-device event deduplication records (including events
  trimmed out of history) and batch receipts, rules and alerts, and the
  complete configuration delivery state (published versions, target/applied
  versions, application receipts and publish/receipt deduplication records).
  Data written before a retention limit existed defaults to unlimited.
  Recovery never refreshes device timestamps; new samples continue the
  previous sequence. Re-submitting a batch that had already succeeded returns
  the first receipt (`200`); changing its content still returns `409`. The
  same applies to configuration publish requests and application receipts.
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

While paging, retention trimming only removes records strictly before the
cursor's continuation start, so a walk in progress stays valid and keeps its
first-page upper bound — newer samples never leak in and missing ranges are
never silently skipped. If the exact record the continuation starts at has
been trimmed, the cursor returns `410 Gone` with the current
`earliestSequence`; issue a fresh first page (starting at that sequence) to
continue.

## History retention

Each device keeps its own bounded-history setting, independent of every other
device. `GET` and `PUT /v1/devices/{id}/history/retention` query and change
`maxEvents`:

- `0` — unlimited (the default for new devices and for data written before
  this feature existed);
- `1`–`10000` — keep at most that many of the newest samples.

Both operations return the same body:

```json
{
  "maxEvents": 1000,
  "retainedEvents": 1000,
  "earliestSequence": 42,
  "maxSequence": 1041
}
```

`earliestSequence` is `null` for an empty device and `maxSequence` is the
highest receive sequence ever accepted (starts at `0` and never retreats).

```bash
curl -sS -X PUT http://127.0.0.1:8080/v1/devices/gateway-01/history/retention \
  -H 'Content-Type: application/json' -d '{"maxEvents":1000}'
```

Lowering the limit immediately removes the oldest events by **receive**
sequence (observed time is irrelevant to retention); live telemetry and
successful replay batches enforce the same limit afterwards. Raising the
limit or switching back to unlimited only affects future writes — already
trimmed events never reappear. Trimming never renumbers or reuses sequences:
new samples continue after the all-time maximum. It also does not change last
telemetry, last-active time, alerts (including their trigger/recovery
evidence), configurations or diagnostic tasks.

A replay batch larger than the limit is still accepted in full: every sample
keeps its ordered sequence, judges the rules and appears on the receipt, and
only afterwards are the oldest excess events dropped. A duplicate whose event
was already trimmed is still recognised — identical content returns its
original sequence without re-entering history, evicting anything, refreshing
device state or firing alerts again; different content still returns `409`,
and retrying the original batch still returns its first receipt.

A missing field, a non-integer or out-of-range `maxEvents`, or a body with
more than one JSON document returns `400`; an unknown device returns `404`; a
successful update returns `200`. When local persistence is enabled, the
setting, trimming, cumulative sequences and replay deduplication records
survive restarts, and a setting or write that cannot be persisted returns
`503` with nothing applied and no sequence consumed.

## Threshold rules and alerts

A rule watches one telemetry metric on one device. `POST
/v1/devices/{id}/rules` creates it enabled at `version` 1; a sample at or
above `trigger` opens an alert, and a later sample at or below `recover` ends
it (`recover` must be strictly below `trigger`). `GET
/v1/devices/{id}/rules` and `GET /v1/devices/{id}/rules/{ruleId}` read rules
back; `GET /v1/devices/{id}/alerts` lists alerts, filterable by `ruleId`,
`status` (`active`/`ended`) and `acknowledged`.

### Updating a rule

`PUT /v1/devices/{id}/rules/{ruleId}` modifies a rule in place. The body
carries the `version` the caller last read (optimistic concurrency) plus any
of `metric`, `trigger`, `recover` and `enabled`; fields left out keep their
current values. A successful update returns `200` with the rule at
`version+1` — **every** successful update increments the version, even one
that only toggles `enabled` or submits values identical to the current ones.

A successful update also **ends the rule's currently active alert** (if any)
with reason `rule_changed`. The walkthrough below runs the full cycle locally
against one device and one temperature rule (timestamps are illustrative):

```bash
# register the device (skip if it already exists)
curl -sS -X POST http://127.0.0.1:8080/v1/devices \
  -H 'Content-Type: application/json' \
  -d '{"id":"gateway-01","site":"warehouse-a"}'

# create the rule: trigger at 30, recover at 25
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/rules \
  -H 'Content-Type: application/json' \
  -d '{"id":"temp-high","metric":"temperature","trigger":30,"recover":25}'
# 201 -> {"id":"temp-high","metric":"temperature","trigger":30,"recover":25,
#         "enabled":true,"version":1,"createdAt":"...","updatedAt":"..."}

# a sample at or above the trigger opens an alert
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/telemetry \
  -H 'Content-Type: application/json' \
  -d '{"temperature":31.5}'

curl -sS 'http://127.0.0.1:8080/v1/devices/gateway-01/alerts?status=active'
# 200 -> {"alerts":[{"id":1,"ruleId":"temp-high","ruleVersion":1,
#         "metric":"temperature","trigger":30,"recover":25,"status":"active",
#         "triggerSequence":1,"triggerValue":31.5,
#         "triggerObservedAt":"2024-01-02T10:00:00Z"}]}

# read the rule to learn its current version
curl -sS http://127.0.0.1:8080/v1/devices/gateway-01/rules/temp-high
# 200 -> {"id":"temp-high",...,"trigger":30,"recover":25,"version":1,...}

# raise the thresholds, passing the version just read
curl -sS -X PUT http://127.0.0.1:8080/v1/devices/gateway-01/rules/temp-high \
  -H 'Content-Type: application/json' \
  -d '{"trigger":32,"recover":26,"version":1}'
# 200 -> {"id":"temp-high","metric":"temperature","trigger":32,"recover":26,
#         "enabled":true,"version":2,"createdAt":"...","updatedAt":"..."}
```

Querying the rule and the alerts afterwards shows both halves of the effect:

```bash
curl -sS http://127.0.0.1:8080/v1/devices/gateway-01/rules/temp-high
# 200 -> {"id":"temp-high",...,"trigger":32,"recover":26,"version":2,...}

curl -sS http://127.0.0.1:8080/v1/devices/gateway-01/alerts
# 200 -> {"alerts":[{"id":1,"ruleId":"temp-high","ruleVersion":1,
#         "metric":"temperature","trigger":30,"recover":25,"status":"ended",
#         "triggerSequence":1,"triggerValue":31.5,
#         "triggerObservedAt":"2024-01-02T10:00:00Z",
#         "endedAt":"2024-01-02T10:05:00Z","endReason":"rule_changed"}]}
```

The ended record keeps the old `ruleVersion` (1), the thresholds in force
when it fired (30/25) and its trigger evidence, and gains `endedAt` — the
server time of the update. It has **no** `recoverSequence`, `recoverValue` or
`recoverObservedAt`: those belong only to a natural recovery, where a later
sample at or below `recover` ends the alert with `endReason: "recovered"`.
Ending via `rule_changed` is an administrative close, not a recovered
reading. If the rule had no active alert, the update leaves the alert list
untouched — a modification never creates an alert on its own.

The update does not re-judge historical samples either; only telemetry
received afterwards is evaluated against the new version:

```bash
# 31.0 was above the old trigger (30) but is below the new one (32): no alert
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/telemetry \
  -H 'Content-Type: application/json' -d '{"temperature":31.0}'
curl -sS 'http://127.0.0.1:8080/v1/devices/gateway-01/alerts?status=active'
# 200 -> {"alerts":[]}

# reaching the new trigger opens a NEW alert recorded against version 2
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/telemetry \
  -H 'Content-Type: application/json' -d '{"temperature":32.5}'
curl -sS 'http://127.0.0.1:8080/v1/devices/gateway-01/alerts?status=active'
# 200 -> {"alerts":[{"id":2,"ruleId":"temp-high","ruleVersion":2,
#         "metric":"temperature","trigger":32,"recover":26,"status":"active",
#         "triggerSequence":3,"triggerValue":32.5,
#         "triggerObservedAt":"2024-01-02T10:06:00Z"}]}
```

The retrigger creates a fresh alert record (`id` 2) — it never reopens or
reuses the record just ended by the update.

Two failure branches leave everything exactly as it was:

- **Stale version** — submitting again with the old `version` returns `409`;
  the rule keeps its current content and version, and its alerts (active or
  ended) are untouched. Re-read the rule to get the current version, then
  decide whether the change is still wanted before resubmitting:

  ```bash
  curl -sS -X PUT http://127.0.0.1:8080/v1/devices/gateway-01/rules/temp-high \
    -H 'Content-Type: application/json' \
    -d '{"trigger":35,"version":1}'
  # 409 -> {"error":"..."}
  ```

- **Invalid thresholds** — an update whose resulting `recover` is not
  strictly below `trigger` returns `400`. No version is consumed and no
  active alert is ended:

  ```bash
  curl -sS -X PUT http://127.0.0.1:8080/v1/devices/gateway-01/rules/temp-high \
    -H 'Content-Type: application/json' \
    -d '{"recover":32,"version":2}'   # recover 32 >= trigger 32
  # 400 -> {"error":"recover threshold must be less than trigger threshold"}
  ```

An unknown device or rule id returns `404`; a malformed body, a blank
`metric` or a non-finite threshold returns `400`.

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
