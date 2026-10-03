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

## Threshold rules

Each device owns an independent set of threshold rules.
`POST /v1/devices/{id}/rules` with `{"id","metric","trigger","recover"}`
creates an enabled rule whose `version` starts at 1. While a rule is enabled,
a newly accepted sample whose metric is at or above `trigger` opens an alert,
and a later sample at or below `recover` closes it naturally; samples between
the two thresholds, or samples without the metric, change nothing. Rules only
judge samples accepted after the rule exists — neither creating nor changing
a rule ever re-judges history.

`PUT /v1/devices/{id}/rules/{ruleId}` modifies a rule with optimistic
versioning: the body must carry the current `version` and may carry any of
`metric`, `trigger`, `recover` and `enabled`. Start the server
(`go run ./cmd/edge-fleet`) and follow the calls below in order; the
timestamps in the responses are illustrative.

Register the device, create one temperature rule, and send a sample that
crosses the trigger:

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/devices \
  -H 'Content-Type: application/json' \
  -d '{"id":"gateway-01","site":"warehouse-a"}'
# 201 -> {"id":"gateway-01","site":"warehouse-a", ...}

curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/rules \
  -H 'Content-Type: application/json' \
  -d '{"id":"temp-high","metric":"temperature","trigger":30,"recover":25}'
# 201 ->
# {"id":"temp-high","metric":"temperature","trigger":30,"recover":25,
#  "enabled":true,"version":1,
#  "createdAt":"2026-10-03T10:00:00Z","updatedAt":"2026-10-03T10:00:00Z"}

# 32 >= 30 opens alert id 1 at receive sequence 1.
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/telemetry \
  -H 'Content-Type: application/json' \
  -d '{"temperature":32}'
# 202 Accepted
```

Read the current version before editing:

```bash
curl -sS http://127.0.0.1:8080/v1/devices/gateway-01/rules/temp-high
# 200 ->
# {"id":"temp-high","metric":"temperature","trigger":30,"recover":25,
#  "enabled":true,"version":1, ...}
```

Raise both thresholds, submitting the version just read:

```bash
curl -sS -X PUT http://127.0.0.1:8080/v1/devices/gateway-01/rules/temp-high \
  -H 'Content-Type: application/json' \
  -d '{"trigger":35,"recover":28,"version":1}'
# 200 ->
# {"id":"temp-high","metric":"temperature","trigger":35,"recover":28,
#  "enabled":true,"version":2, ...}
```

Then query the rule and its alerts again:

```bash
curl -sS http://127.0.0.1:8080/v1/devices/gateway-01/rules/temp-high
# 200 ->
# {"id":"temp-high","metric":"temperature","trigger":35,"recover":28,
#  "enabled":true,"version":2, ...}

curl -sS 'http://127.0.0.1:8080/v1/devices/gateway-01/alerts?ruleId=temp-high'
# 200 ->
# {"alerts":[
#   {"id":1,"ruleId":"temp-high","ruleVersion":1,"metric":"temperature",
#    "trigger":30,"recover":25,"status":"ended",
#    "triggerSequence":1,"triggerValue":32,
#    "triggerObservedAt":"2026-10-03T10:01:00Z",
#    "endedAt":"2026-10-03T10:02:00Z","endReason":"rule_changed"}]}
```

What the result shows:

- A successful update returns `200` and increments `version` (1 → 2).
  Mutable fields omitted from the request keep their current values — above,
  `metric` stayed `temperature` and `enabled` stayed `true`. The resulting
  `recover` threshold must be strictly lower than `trigger`.
- Every successful update increments the version, even one that only toggles
  `enabled` (e.g. `{"enabled":false,"version":2}`) or resubmits the current
  values unchanged (e.g. `{"trigger":35,"recover":28,"version":2}`); the same
  active-alert ending rule applies in every case.
- Alert 1 was active, so the edit ended it immediately: `status` is `ended`,
  `endReason` is `rule_changed`, and `endedAt` is the server time of the
  update. It carries no `recoverSequence`, `recoverValue` or
  `recoverObservedAt`, because no recovery telemetry was received. The old
  record keeps `ruleVersion` 1 and the thresholds and trigger evidence from
  when it fired.
- If the rule has no active alert when it is edited, the version still bumps
  but no alert appears — a modification never invents an alert.
- This differs from a natural recovery: without the edit, a later sample at
  or below `recover` (such as `{"temperature":24}` while `recover` was 25)
  would end the alert with `"endReason":"recovered"` and populate
  `recoverSequence`, `recoverValue` and `recoverObservedAt` from that sample,
  with no `endedAt`. A `rule_changed` end is caused by the edit itself, not
  by telemetry.

The update does not re-judge past samples: alert 1 stays an ended record, and
only telemetry received afterwards is judged against version 2. The value 32
crossed the old trigger but now lands between the new thresholds; 36 crosses
the new trigger and opens a separate, new alert:

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/telemetry \
  -H 'Content-Type: application/json' -d '{"temperature":32}'
# 202 Accepted; 28 < 32 < 35, so nothing changes (receive sequence 2)

curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/telemetry \
  -H 'Content-Type: application/json' -d '{"temperature":36}'
# 202 Accepted; 36 >= 35 opens a brand-new alert at receive sequence 3

curl -sS 'http://127.0.0.1:8080/v1/devices/gateway-01/alerts?ruleId=temp-high'
# 200 ->
# {"alerts":[
#   {"id":1,"ruleId":"temp-high","ruleVersion":1,"trigger":30,"recover":25,
#    "status":"ended","endReason":"rule_changed", ...},
#   {"id":2,"ruleId":"temp-high","ruleVersion":2,"trigger":35,"recover":28,
#    "status":"active","triggerSequence":3,"triggerValue":36, ...}]}
```

The new crossing is a new record (id 2) bound to rule version 2 — it never
reuses or reopens the just-ended alert 1.

Two failure branches follow directly from the successful update. Repeating
the earlier edit with the now-stale version is rejected without touching the
rule or its alerts:

```bash
curl -sS -X PUT http://127.0.0.1:8080/v1/devices/gateway-01/rules/temp-high \
  -H 'Content-Type: application/json' \
  -d '{"trigger":35,"recover":28,"version":1}'
# 409 -> {"error":"rule version conflict"}
```

The rule stays at version 2 with thresholds 35/28, alert 1 stays `ended` and
alert 2 stays `active`. Re-query the rule to obtain the current version, then
decide what to submit against it.

Submitting thresholds where the recovery threshold is not below the trigger
threshold is rejected as well:

```bash
curl -sS -X PUT http://127.0.0.1:8080/v1/devices/gateway-01/rules/temp-high \
  -H 'Content-Type: application/json' \
  -d '{"trigger":35,"recover":35,"version":2}'
# 400 -> {"error":"recover threshold must be less than trigger threshold"}
```

Nothing is applied: the version does not increment and no active alert is
ended or otherwise changed. An unknown device or rule returns `404`; a
missing or non-positive `version`, a blank `metric`, or a non-finite
threshold returns `400`.

`GET /v1/devices/{id}/rules` lists a device's rules, and
`GET /v1/devices/{id}/alerts` supports `ruleId`, `status` and `acknowledged`
filters.

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

## Remote diagnostics

A fleet operator opens a diagnostic task for a registered device, and the
simulated device claims it, runs it and reports the outcome. Start the server
(`go run ./cmd/edge-fleet`) and follow the calls below in order; the
timestamps and credential are illustrative — copy the task number, credential
and deadline out of your own responses.

Register the device, create one task with a time limit of 1–60 seconds, and
claim it:

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/devices \
  -H 'Content-Type: application/json' \
  -d '{"id":"gateway-01","site":"warehouse-a"}'
# 201 -> {"id":"gateway-01","site":"warehouse-a", ...}

curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks \
  -H 'Content-Type: application/json' \
  -d '{"requestId":"diag-2026-10-04-1","durationSeconds":30}'
# 201 ->
# {"id":1,"requestId":"diag-2026-10-04-1","durationSeconds":30,
#  "status":"pending","attempts":0,
#  "createdAt":"2026-10-04T10:00:00Z",
#  "nextClaimableAt":"2026-10-04T10:00:00Z"}

curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks/claim
# 200 ->
# {"task":{"id":1,"requestId":"diag-2026-10-04-1","durationSeconds":30,
#   "status":"in_progress","attempts":1,
#   "createdAt":"2026-10-04T10:00:00Z",
#   "claimedAt":"2026-10-04T10:00:01Z",
#   "deadline":"2026-10-04T10:00:31Z"},
#  "attempt":1,
#  "credential":"9f1c2a7e4b6d4801a3f5c8e2d7b09164",
#  "deadline":"2026-10-04T10:00:31Z"}
```

- The claim hands the device the earliest-created claimable task. Afterwards
  the task is `in_progress` and its deadline is the claim time plus
  `durationSeconds` (a `durationSeconds` outside 1–60 is rejected at creation
  with `400`).
- Both values the report needs come **only from this claim response**: the
  task number in `task.id` and the execution `credential`. Ordinary task
  queries (`GET /v1/devices/{id}/tasks` and
  `GET /v1/devices/{id}/tasks/{taskId}`) never return a credential.
- Creating the task itself produces no diagnostic result: the task view has
  no `result` field until a successful report is accepted.

The device reports success to that task's `reports` entry, using the task id
and credential from the claim:

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks/1/reports \
  -H 'Content-Type: application/json' \
  -d '{"receiptId":"diag-rcpt-1",
       "credential":"9f1c2a7e4b6d4801a3f5c8e2d7b09164",
       "success":true,
       "result":{"checks":42,"passed":true}}'
# 201 ->
# {"receiptId":"diag-rcpt-1","success":true,
#  "result":{"checks":42,"passed":true},
#  "receivedAt":"2026-10-04T10:00:02Z"}
```

- A successful report requires `success: true` and `result` as a JSON object
  (nested objects, arrays and ordinary JSON values are allowed). An empty
  object is a legal result: `"result":{}` records a successful run with no
  data.
- `receiptId` is the device-chosen id for this outcome and is deduplicated
  within the device, so it is also the retry key if the response is lost.
- `receivedAt` in the response is the server time at which it **first**
  received this result.

Query the same task and its audit trail:

```bash
curl -sS http://127.0.0.1:8080/v1/devices/gateway-01/tasks/1
# 200 ->
# {"id":1,"requestId":"diag-2026-10-04-1","durationSeconds":30,
#  "status":"succeeded","attempts":1,
#  "createdAt":"2026-10-04T10:00:00Z",
#  "result":{"checks":42,"passed":true},
#  "completedAt":"2026-10-04T10:00:02Z"}

curl -sS http://127.0.0.1:8080/v1/devices/gateway-01/tasks/1/audit
# 200 ->
# {"audit":[
#   {"seq":1,"event":"created","toStatus":"pending",
#    "at":"2026-10-04T10:00:00Z"},
#   {"seq":2,"event":"claimed","fromStatus":"pending",
#    "toStatus":"in_progress","attempt":1,
#    "at":"2026-10-04T10:00:01Z"},
#   {"seq":3,"event":"succeeded","toStatus":"succeeded","attempt":1,
#    "at":"2026-10-04T10:00:02Z"}]}
```

The task now reads `succeeded`, carries the saved result, and has
`completedAt` stamped at the first report's receive time; the audit shows the
successful completion as its last record.

### Retrying after a lost response

If the `201` response never reaches the device, resend exactly the same
outcome under the same `receiptId` — here with the object keys reordered and
`42` written as `42.0`:

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks/1/reports \
  -H 'Content-Type: application/json' \
  -d '{"receiptId":"diag-rcpt-1",
       "credential":"9f1c2a7e4b6d4801a3f5c8e2d7b09164",
       "success":true,
       "result":{"passed":true,"checks":42.0}}'
# 200 ->
# {"receiptId":"diag-rcpt-1","success":true,
#  "result":{"checks":42,"passed":true},
#  "receivedAt":"2026-10-04T10:00:02Z"}
```

- A recognised duplicate returns `200 OK` with the first stored receipt,
  including the **first** `receivedAt` — the retry does not complete the task
  a second time and no second `succeeded` record appears in the audit.
- Equality ignores object key order and compares numbers by value, so `42`
  and `42.0` are the same result. Array order is significant: a result whose
  array elements are reordered (e.g. `"checks":[2,1]` after `[1,2]`) is a
  different result and returns `409`.
- Even though success invalidated the claim's credential, the already
  accepted report can still be replayed with that original credential; the
  duplicate is recognised by `receiptId` first.

Two reuses of a finished task conflict instead, both with
`409 {"error":"diagnostic task conflict"}`, and the saved result stays
exactly as first accepted:

```bash
# same receiptId, changed result
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks/1/reports \
  -H 'Content-Type: application/json' \
  -d '{"receiptId":"diag-rcpt-1",
       "credential":"9f1c2a7e4b6d4801a3f5c8e2d7b09164",
       "success":true,"result":{"checks":43,"passed":true}}'
# 409 -> {"error":"diagnostic task conflict"}

# a brand-new receiptId after the task already succeeded
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks/1/reports \
  -H 'Content-Type: application/json' \
  -d '{"receiptId":"diag-rcpt-2",
       "credential":"9f1c2a7e4b6d4801a3f5c8e2d7b09164",
       "success":true,"result":{"checks":42,"passed":true}}'
# 409 -> {"error":"diagnostic task conflict"}
```

### Report failure conditions

- A report missing `receiptId`, `credential` or `success`, or a success whose
  `result` is not a JSON object (an array, string or number), returns `400`,
  e.g. `{"error":"a successful report requires a JSON object result"}`.
- A first-time submission with a wrong or foreign credential, or one made
  after the deadline from the claim response, returns
  `409 {"error":"diagnostic task conflict"}`. The boundary is inclusive: a
  report presented at exactly the deadline instant is still accepted; only a
  strictly later time conflicts.
- An unknown device or task id returns `404`.

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
