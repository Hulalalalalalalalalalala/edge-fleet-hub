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

A fleet operator opens a diagnostic task for a registered device; the simulated
device claims it, runs it for the task's bounded number of seconds, and reports
the outcome. Start the server (`go run ./cmd/edge-fleet`) and follow the calls
below in order; the timestamps in the responses are illustrative.

Register the device as usual, then create a task with a time limit between 1
and 60 seconds:

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
#  "status":"pending","attempts":0,"createdAt":"...","nextClaimableAt":"..."}
```

Creating a task only opens it: it is `pending` and carries no diagnostic
result. A result exists only after the device reports a successful outcome.

The device pulls the next task by claiming it:

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks/claim
# 200 ->
# {"task":{"id":1,"requestId":"diag-2026-10-04-1","durationSeconds":30,
#    "status":"in_progress","attempts":1,"createdAt":"...",
#    "claimedAt":"2026-10-04T10:00:00Z","deadline":"2026-10-04T10:00:30Z"},
#  "attempt":1,
#  "credential":"9e8525d072f9b9740f04e4994c88ec3b",
#  "deadline":"2026-10-04T10:00:30Z"}
```

After the claim the task is `in_progress`, and the response gives the two
values the report needs: the task number at `task.id` and the one-time
execution `credential`. The `deadline` is 30 seconds after the claim. Only
one task per device can be in progress, so a second claim while this one is
running returns `204 No Content`. The credential is returned **only** by the
claim call — `GET /v1/devices/{id}/tasks` and
`GET /v1/devices/{id}/tasks/{taskId}` never include it.

Report success to the task's `reports` endpoint before the deadline, using
`task.id` and `credential` from the claim response. A successful report
carries `success: true` and a `result` that is a JSON object; an empty object
(`{}`) is a valid result:

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks/1/reports \
  -H 'Content-Type: application/json' \
  -d '{"receiptId":"diag-rc-1",
       "credential":"9e8525d072f9b9740f04e4994c88ec3b",
       "success":true,
       "result":{"ok":true,"checks":42}}'
# 201 ->
# {"receiptId":"diag-rc-1","success":true,
#  "result":{"ok":true,"checks":42},
#  "receivedAt":"2026-10-04T10:00:03Z"}
```

A first-time accepted report returns `201`. `receivedAt` is the server time
at which that result was first received — it stays fixed on every later retry
of the same `receiptId`. Querying the task now shows `succeeded`, the saved
result and the completion time:

```bash
curl -sS http://127.0.0.1:8080/v1/devices/gateway-01/tasks/1
# 200 ->
# {"id":1,"requestId":"diag-2026-10-04-1","durationSeconds":30,
#  "status":"succeeded","attempts":1,"createdAt":"...",
#  "result":{"ok":true,"checks":42},
#  "completedAt":"2026-10-04T10:00:03Z"}
```

The task's audit trail records the success (alongside the earlier create and
claim transitions):

```bash
curl -sS http://127.0.0.1:8080/v1/devices/gateway-01/tasks/1/audit
# 200 ->
# {"audit":[
#   {"seq":1,"event":"created","toStatus":"pending", ...},
#   {"seq":2,"event":"claimed","fromStatus":"pending",
#    "toStatus":"in_progress","attempt":1,"at":"2026-10-04T10:00:00Z"},
#   {"seq":3,"event":"succeeded","toStatus":"succeeded","attempt":1,
#    "at":"2026-10-04T10:00:03Z"}]}
```

### Retrying after a lost response

If the `201` response never arrives, resend the **same** body with the same
`receiptId`. The credential from the original claim is still presented; an
already-accepted report keeps replaying with it even though the task is
finished:

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks/1/reports \
  -H 'Content-Type: application/json' \
  -d '{"receiptId":"diag-rc-1",
       "credential":"9e8525d072f9b9740f04e4994c88ec3b",
       "success":true,
       "result":{"checks":42.0,"ok":true}}'
# 200 ->
# {"receiptId":"diag-rc-1","success":true,
#  "result":{"ok":true,"checks":42},
#  "receivedAt":"2026-10-04T10:00:03Z"}
```

The duplicate returns `200` with the first receipt and its **original**
`receivedAt`: the task is not completed a second time, no second `succeeded`
audit record is added, and the saved result is unchanged. Result equality is
semantic:

- Object key order is ignored — `{"ok":true,"checks":42}` and
  `{"checks":42,"ok":true}` are the same result.
- Numbers compare by value — `42` and `42.0` are the same result.
- Array element order is significant — `{"v":[1,2]}` and `{"v":[2,1]}` are
  different results.

Two reuses of a completed task are rejected with `409
{"error":"diagnostic task conflict"}`, and the saved result stays exactly as
first accepted:

- the same `receiptId` with a changed result (or a changed `success`/
  `reason`), e.g. `{"ok":false,"checks":42}`;
- a brand-new `receiptId` submitted after the task already succeeded.

A diagnostic `receiptId` belongs to exactly one task **of the device**: the
task on which it was first accepted. Submitting an already-accepted number to
another task of the same device is `409` even when the content is identical,
the second task is still within its deadline and the request carries that
task's own valid credential. The second task keeps its previous status,
attempt count, deadline and credential, and neither its query result nor its
audit trail gains a completion or failure record; retrying with an unused
`receiptId` still succeeds while the execution is valid. The same number is
accepted independently on another device, and configuration application
receipts use a separate number space that does not take part in this check.
The binding is made only on acceptance, so a submission rejected for its
credential or deadline does not occupy the number. With local persistence
enabled, the ownership survives a restart of the data directory and the
original receipt still replays with its first `receivedAt`.

### When an execution exceeds its deadline

A claim does not guarantee a result. If the simulated device never posts an
outcome for a claimed attempt, the attempt **times out**: the task itself is
not finished, but its credential is invalidated and the device slot is
released. The task moves `in_progress` → `waiting`, and after a short backoff
it must be **claimed again** — the server never restarts an attempt on its
own. Timeouts are settled lazily: task detail, the task list, the audit
trail, claim and report all apply a due deadline expiry before they run. No
background timer is involved, so coming back an hour later produces the same
status and timestamps as polling immediately — the wait is never measured
from the query time.

The walkthrough below continues on the same registered device with a second
task and a five-second limit. Save the same four values as before —
`task.id`, `attempt`, `credential` and `deadline` — and deliberately do not
report a result:

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks \
  -H 'Content-Type: application/json' \
  -d '{"requestId":"diag-2026-10-04-2","durationSeconds":5}'
# 201 ->
# {"id":2,"requestId":"diag-2026-10-04-2","durationSeconds":5,
#  "status":"pending","attempts":0,"createdAt":"2026-10-04T10:09:55Z",
#  "nextClaimableAt":"2026-10-04T10:09:55Z"}

curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks/claim
# 200 ->
# {"task":{"id":2,...,"status":"in_progress","attempts":1,
#    "claimedAt":"2026-10-04T10:10:00Z","deadline":"2026-10-04T10:10:05Z"},
#  "attempt":1,
#  "credential":"a3f1c9d24b7e4f60a8c5d1e9b2f70463",
#  "deadline":"2026-10-04T10:10:05Z"}
# Keep: task id 2, credential a3f1...0463, deadline 10:10:05. Do NOT report.
```

The deadline is five seconds after the claim. Between the deadline and one
second after it, a claim already returns `204 No Content`: the attempt has
expired, but the task is serving a one-second backoff and is not claimable
yet (the claim call below also settles the expiry — see the audit timestamps
later):

```bash
# Between 10:10:05 and 10:10:06, with no result ever posted:
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks/claim \
  -w '\n%{http_code}\n'
# 204
```

Querying the task now (or at any later time — 10:15 would give the same
timestamps) shows `waiting`:

```bash
curl -sS http://127.0.0.1:8080/v1/devices/gateway-01/tasks/2
# 200 ->
# {"id":2,"requestId":"diag-2026-10-04-2","durationSeconds":5,
#  "status":"waiting","attempts":1,"createdAt":"2026-10-04T10:09:55Z",
#  "nextClaimableAt":"2026-10-04T10:10:06Z",
#  "failureReason":"deadline exceeded"}

curl -sS http://127.0.0.1:8080/v1/devices/gateway-01/tasks
# 200 -> {"tasks":[ ...the same object... ]} — the list settles timeouts too
```

What the fields tell the device:

- `status` is `waiting` — not `in_progress`, but also not finished; there is
  no `result` or `completedAt`, and the view no longer carries `claimedAt` or
  `deadline` for the dead attempt.
- `attempts` is still `1`. It counts **claims**, not failures: this task has
  been picked up once.
- `failureReason` is `"deadline exceeded"`, the fixed reason for a missed
  deadline.
- `nextClaimableAt` is the **original deadline plus one second**
  (`10:10:05Z` → `10:10:06Z`), never the query time plus a second.

The audit trail records the expiry exactly once, stamped at the deadline
instant rather than at query time:

```bash
curl -sS http://127.0.0.1:8080/v1/devices/gateway-01/tasks/2/audit
# 200 ->
# {"audit":[
#   {"seq":1,"event":"created","toStatus":"pending",
#    "at":"2026-10-04T10:09:55Z"},
#   {"seq":2,"event":"claimed","fromStatus":"pending",
#    "toStatus":"in_progress","attempt":1,"at":"2026-10-04T10:10:00Z"},
#   {"seq":3,"event":"timed_out","fromStatus":"in_progress",
#    "toStatus":"waiting","attempt":1,"at":"2026-10-04T10:10:05Z",
#    "reason":"deadline exceeded"}]}
```

Repeating the detail, list or audit query (or issuing more losing claims or
rejected reports) changes nothing: the same timeout is not counted a second
time, no second `timed_out` record appears, and the `seq` numbers stay as
shown. Because every one of those endpoints settles the expiry first, even a
first query minutes or hours late still dates the failure at `10:10:05Z` and
the backoff still ends at `10:10:06Z` — nothing restarts from the moment you
happen to reconnect.

The expired credential no longer authorizes anything. Submitting the result
that "would have been" the first attempt's outcome is rejected:

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks/2/reports \
  -H 'Content-Type: application/json' \
  -d '{"receiptId":"diag-rc-timeout-1",
       "credential":"a3f1c9d24b7e4f60a8c5d1e9b2f70463",
       "success":true,"result":{"ok":true}}'
# 409 -> {"error":"diagnostic task conflict"}
```

- The `409` saves **nothing**: no result, no completion, no audit record.
- The `receiptId` is **not occupied** by this rejection — the binding is made
  only on acceptance, so the same `diag-rc-timeout-1` can be used again once a
  valid execution exists (it is, below).
- The deadline boundary is inclusive. An attempt is live up to and including
  its `deadline`: a report presented at exactly `10:10:05Z` is accepted with
  `201`; only a strictly later instant counts as a timeout.

To get a runnable attempt again, the device must claim the task itself. While
the clock is before `nextClaimableAt`, claims keep returning `204`; at or
after `10:10:06Z` the same task is handed out again:

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks/claim
# 200 ->
# {"task":{"id":2,"requestId":"diag-2026-10-04-2","durationSeconds":5,
#    "status":"in_progress","attempts":2,"createdAt":"2026-10-04T10:09:55Z",
#    "claimedAt":"2026-10-04T10:10:10Z","deadline":"2026-10-04T10:10:15Z",
#    "failureReason":"deadline exceeded"},
#  "attempt":2,
#  "credential":"7d04b6e2c9184a5fb36d7e0c82a19f54",
#  "deadline":"2026-10-04T10:10:15Z"}
```

`attempts` is now `2`, and the response carries a **new** one-time credential
and a **new** deadline (another five seconds from this claim). The lingering
`failureReason` on the in-progress task view is historical — the previous
attempt's reason; it is overwritten by a newer failure and disappears on
success.

If the device's very first interaction after missing the deadline is already
past the backoff, the `204` step is skipped entirely — the claim itself
settles the expiry and immediately succeeds. This is the same mechanism; the
task simply waits in `waiting` until someone pulls it:

```bash
# Another task, claimed at 10:12:00 (deadline 10:12:05), then the device
# goes quiet until 10:12:40:
curl -sS http://127.0.0.1:8080/v1/devices/gateway-01/tasks/3
# 200 -> {"id":3,"status":"waiting","attempts":1,
#         "nextClaimableAt":"2026-10-04T10:12:06Z", ...}
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks/claim
# 200 -> {"task":{...,"id":3,"status":"in_progress","attempts":2,
#    "claimedAt":"2026-10-04T10:12:40Z","deadline":"2026-10-04T10:12:45Z"},
#  "attempt":2,"credential":"b91f...","deadline":"2026-10-04T10:12:45Z"}
```

Back on task 2, submit the new execution's result with the **new**
credential. Note that `diag-rc-timeout-1`, rejected with `409` on the dead
attempt, is accepted now because that rejection never bound it:

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks/2/reports \
  -H 'Content-Type: application/json' \
  -d '{"receiptId":"diag-rc-timeout-1",
       "credential":"7d04b6e2c9184a5fb36d7e0c82a19f54",
       "success":true,"result":{"ok":true,"checks":42}}'
# 201 ->
# {"receiptId":"diag-rc-timeout-1","success":true,
#  "result":{"ok":true,"checks":42},
#  "receivedAt":"2026-10-04T10:10:11Z"}

curl -sS http://127.0.0.1:8080/v1/devices/gateway-01/tasks/2
# 200 ->
# {"id":2,...,"status":"succeeded","attempts":2,
#  "result":{"ok":true,"checks":42},
#  "completedAt":"2026-10-04T10:10:11Z"}

curl -sS http://127.0.0.1:8080/v1/devices/gateway-01/tasks/2/audit
# 200 ->
# {"audit":[
#   {"seq":1,"event":"created", ...},
#   {"seq":2,"event":"claimed",...,"attempt":1,...,"at":"...10:10:00Z"},
#   {"seq":3,"event":"timed_out",...,"attempt":1,...,"at":"...10:10:05Z"},
#   {"seq":4,"event":"claimed","fromStatus":"waiting",
#    "toStatus":"in_progress","attempt":2,"at":"2026-10-04T10:10:10Z"},
#   {"seq":5,"event":"succeeded","toStatus":"succeeded","attempt":2,
#    "at":"2026-10-04T10:10:11Z"}]}
```

The timeout stays in the trail as a real first attempt; the success belongs
to the second. Any later report must follow the usual rules: the accepted
receipt replays idempotently (see [Retrying after a lost
response](#retrying-after-a-lost-response)), while a brand-new `receiptId`
presented with the old credential is still `409`.

### Repeated failures: backoff and the failed end

Deadline timeouts and device-reported failures (`"success": false` with a
`reason`) draw on **one shared budget of three failures**. The first two
failures both move the task to `waiting`, with a backoff of one second after
the first and two seconds after the second; the third failure ends the task
as `failed` and it can never be claimed again. The two sources differ only in
their timestamps, reason and audit event: a reported failure's wait runs from
the moment the receipt was received, while a timeout's wait and completion
run from that attempt's deadline.

On a new five-second task: the device reports failure immediately on the
first attempt.

```bash
# Claimed at 10:20:00, deadline 10:20:05; the failure report arrives at
# 10:20:01:
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks/4/reports \
  -H 'Content-Type: application/json' \
  -d '{"receiptId":"diag-f-1",
       "credential":"<credential from the attempt-1 claim>",
       "success":false,"reason":"sensor unreachable"}'
# 201 ->
# {"receiptId":"diag-f-1","success":false,"reason":"sensor unreachable",
#  "receivedAt":"2026-10-04T10:20:01Z"}

curl -sS http://127.0.0.1:8080/v1/devices/gateway-01/tasks/4
# 200 -> {"id":4,"status":"waiting","attempts":1,
#         "nextClaimableAt":"2026-10-04T10:20:02Z",
#         "failureReason":"sensor unreachable"}

curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks/claim \
  -w '\n%{http_code}\n'
# 204  — still inside the 1-second backoff
```

Failure 1 waits **one second from the report time** (`10:20:01Z` →
`10:20:02Z`). At or after `nextClaimableAt`, claim the task for the second
attempt; this time send nothing and let the deadline pass. Failure 2 is a
timeout, and because the count is shared it takes the **two-second** backoff,
measured from the attempt's deadline (`10:20:07Z` → `10:20:09Z`):

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks/claim
# 200 -> {"task":{...,"id":4,"status":"in_progress","attempts":2,
#    "claimedAt":"2026-10-04T10:20:02Z","deadline":"2026-10-04T10:20:07Z"},
#  "attempt":2,"credential":"<new credential>",
#  "deadline":"2026-10-04T10:20:07Z"}

# After 10:20:07 with no report:
curl -sS http://127.0.0.1:8080/v1/devices/gateway-01/tasks/4
# 200 -> {"id":4,"status":"waiting","attempts":2,
#         "nextClaimableAt":"2026-10-04T10:20:09Z",
#         "failureReason":"deadline exceeded"}
```

Claim the third attempt after `10:20:09Z` and miss that deadline as well.
The third failure ends the task; with a timeout the completion time is that
attempt's deadline:

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks/claim
# 200 -> {"task":{...,"id":4,"status":"in_progress","attempts":3,
#    "claimedAt":"2026-10-04T10:20:09Z","deadline":"2026-10-04T10:20:14Z"},
#  "attempt":3,"credential":"<new credential>",
#  "deadline":"2026-10-04T10:20:14Z"}

# After 10:20:14:
curl -sS http://127.0.0.1:8080/v1/devices/gateway-01/tasks/4
# 200 ->
# {"id":4,...,"status":"failed","attempts":3,
#  "failureReason":"deadline exceeded",
#  "completedAt":"2026-10-04T10:20:14Z"}

curl -sS -X POST http://127.0.0.1:8080/v1/devices/gateway-01/tasks/claim \
  -w '\n%{http_code}\n'
# 204  — a failed task is never claimable again

curl -sS http://127.0.0.1:8080/v1/devices/gateway-01/tasks/4/audit
# 200 ->
# {"audit":[
#   {"seq":1,"event":"created","toStatus":"pending",...},
#   {"seq":2,"event":"claimed",...,"attempt":1,"at":"...10:20:00Z"},
#   {"seq":3,"event":"failed","toStatus":"waiting","attempt":1,
#    "at":"2026-10-04T10:20:01Z","reason":"sensor unreachable"},
#   {"seq":4,"event":"claimed","fromStatus":"waiting",
#    "toStatus":"in_progress","attempt":2,"at":"...10:20:02Z"},
#   {"seq":5,"event":"timed_out","fromStatus":"in_progress",
#    "toStatus":"waiting","attempt":2,"at":"2026-10-04T10:20:07Z",
#    "reason":"deadline exceeded"},
#   {"seq":6,"event":"claimed","fromStatus":"waiting",
#    "toStatus":"in_progress","attempt":3,"at":"...10:20:09Z"},
#   {"seq":7,"event":"timed_out","fromStatus":"in_progress",
#    "toStatus":"failed","attempt":3,"at":"2026-10-04T10:20:14Z",
#    "reason":"deadline exceeded"}]}
```

A `failed` task carries no `nextClaimableAt`, never appears in a claim
response again (hence the persistent `204`), and a new report against it —
even one presenting a credential that was valid on the last attempt — is
`409`; a receipt already accepted on the task earlier still replays
idempotently with `200` as on any finished task. A third failure reported by
the device ends the task the same way, with `failed` as the event and the
receipt time as `completedAt`. During any `waiting` backoff the device slot
is free, so another already-claimable task on the same device can be claimed
and finished in the meantime.

### Failure conditions for a report

- `400` — a required field is missing or blank (`receiptId`, `credential` or
  `success`), or a successful report's `result` is missing or not a JSON
  object (an array, string or number is rejected).
- `409` — a **first** submission presents a credential that does not match
  the current claim, or arrives after the claim's `deadline`; or the
  `receiptId` was already accepted by another task of the same device. The
  deadline instant itself is still within the attempt: a report presented
  exactly at `deadline` is accepted; only a strictly later instant is past
  it.
- `404` — the device or task id does not exist.

Task creation has its own validation: a blank `requestId`, a missing
`durationSeconds`, or a duration outside 1–60 returns `400`; reusing a
`requestId` with the same duration is an idempotent retry (`200`, first task
returned) and with a different duration is `409`.

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
