package fleet

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// --- restart restores the whole task state -----------------------------------

func TestPersistentTaskCreateRestores(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	createTask(t, h, "gw", taskBody("req-1", 30), http.StatusCreated)
	createTask(t, h, "gw", taskBody("req-2", 45), http.StatusCreated)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	list := decodeBody[struct {
		Tasks []taskResponse `json:"tasks"`
	}](t, doRequest(t, h2, http.MethodGet, "/v1/devices/gw/tasks", ""))
	if len(list.Tasks) != 2 || list.Tasks[0].ID != 1 || list.Tasks[1].ID != 2 {
		t.Fatalf("tasks after restart = %+v", list.Tasks)
	}
	if list.Tasks[0].Status != "pending" || list.Tasks[0].DurationSeconds != 30 {
		t.Fatalf("task 1 after restart = %+v", list.Tasks[0])
	}

	// requestId dedup survives: same content -> 200, different duration -> 409.
	createTask(t, h2, "gw", taskBody("req-1", 30), http.StatusOK)
	createTask(t, h2, "gw", taskBody("req-1", 45), http.StatusConflict)

	// Audit survives with the create record.
	audit := listAudit(t, h2, "gw", 1)
	if len(audit) != 1 || audit[0].Event != "created" || audit[0].ToStatus != "pending" {
		t.Fatalf("audit after restart = %+v", audit)
	}
}

func TestPersistentTaskClaimRestoresAndTimesOut(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createTask(t, h, "gw", taskBody("req-1", 5), http.StatusCreated)
	claim := claimTask(t, h, "gw", http.StatusOK)
	claimedAt := claim.Task.ClaimedAt
	deadline := claim.Deadline
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen with a much later clock: the in-progress task is past its deadline.
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	task := getTask(t, h2, "gw", 1, http.StatusOK)
	if task.Status != "waiting" || task.FailureReason != "deadline exceeded" {
		t.Fatalf("task after restart should be timed out: %+v", task)
	}
	if task.Attempts != 1 {
		t.Fatalf("attempts after restart = %d, want 1", task.Attempts)
	}
	// The deadline and the wait are not reset.
	if task.NextClaimableAt.IsZero() || !task.NextClaimableAt.Equal(deadline.Add(1*time.Second)) {
		t.Fatalf("next claimable after restart = %s, want %s", task.NextClaimableAt, deadline.Add(1*time.Second))
	}
	_ = claimedAt

	// The timeout is recorded exactly once: re-reading does not duplicate it.
	audit := listAudit(t, h2, "gw", 1)
	events := []string{}
	for _, rec := range audit {
		events = append(events, rec.Event)
	}
	if got := joinEvents(events); got != "created,claimed,timed_out" {
		t.Fatalf("audit after restart = %s", got)
	}
	if audit[2].Attempt != 1 || audit[2].FromStatus != "in_progress" || audit[2].ToStatus != "waiting" {
		t.Fatalf("timeout audit record = %+v", audit[2])
	}

	// The released device can claim again.
	claim2 := claimTask(t, h2, "gw", http.StatusOK)
	if claim2.Attempt != 2 {
		t.Fatalf("re-claim attempt = %d, want 2", claim2.Attempt)
	}
}

func TestPersistentTaskReportRestores(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createTask(t, h, "gw", taskBody("req-1", 30), http.StatusCreated)
	claim := claimTask(t, h, "gw", http.StatusOK)
	reportTask(t, h, "gw", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, true, "", `{"ok":true,"n":7}`), http.StatusCreated)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	task := getTask(t, h2, "gw", 1, http.StatusOK)
	if task.Status != "succeeded" || task.Result == nil || task.CompletedAt == nil {
		t.Fatalf("task after restart = %+v", task)
	}
	if string(task.Result) != `{"ok":true,"n":7}` {
		t.Fatalf("result after restart = %s", task.Result)
	}

	// Receipt dedup survives: same content -> 200, different -> 409.
	reportTask(t, h2, "gw", 1,
		reportBody("rcpt-1", claim.Credential, true, "", `{"n":7,"ok":true}`), http.StatusOK)
	reportTask(t, h2, "gw", 1,
		reportBody("rcpt-1", claim.Credential, true, "", `{"ok":false}`), http.StatusConflict)

	// Audit survives.
	audit := listAudit(t, h2, "gw", 1)
	if got := joinEvents(eventsOf(audit)); got != "created,claimed,succeeded" {
		t.Fatalf("audit after restart = %s", got)
	}
}

// A success result carrying legal numbers beyond the float64 range (1e309,
// -1e309, 1e-400, also nested) is persisted byte-for-byte and recovered
// exactly. After reopening the directory the receipt retry/conflict rules
// still compare by exact decimal value and the task detail keeps the result.
func TestPersistentTaskReportOutOfRangeNumbersRestores(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createTask(t, h, "gw", taskBody("req-1", 30), http.StatusCreated)
	claim := claimTask(t, h, "gw", http.StatusOK)
	const result = `{"scale":1e309,"detail":{"offset":1e-400},"neg":-1e309,"samples":[1e309,1e-400]}`
	first := reportTask(t, h, "gw", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, true, "", result), http.StatusCreated)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	// The task and its result recover exactly, with no float conversion.
	task := getTask(t, h2, "gw", 1, http.StatusOK)
	if task.Status != "succeeded" || task.CompletedAt == nil {
		t.Fatalf("task after restart = %+v", task)
	}
	if string(task.Result) != result {
		t.Fatalf("result after restart = %s, want exact %s", task.Result, result)
	}

	// Equal-valued spelling and key order replay the first receipt with its
	// original receivedAt; a changed out-of-range magnitude or a subnormal
	// value equated with zero still conflict and do not overwrite the result.
	retry := reportTask(t, h2, "gw", 1,
		reportBody("rcpt-1", "stale-credential", true, "",
			`{"samples":[10e308,10e-401],"neg":-1e309,"detail":{"offset":1e-400},"scale":1e309}`),
		http.StatusOK)
	if string(retry.Result) != result || !retry.ReceivedAt.Equal(first.ReceivedAt) {
		t.Fatalf("retry after restart = %+v, want first receipt %+v", retry, first)
	}
	reportTask(t, h2, "gw", 1,
		reportBody("rcpt-1", "stale-credential", true, "", `{"scale":2e309}`), http.StatusConflict)
	reportTask(t, h2, "gw", 1,
		reportBody("rcpt-1", "stale-credential", true, "",
			`{"scale":1e309,"detail":{"offset":0}}`), http.StatusConflict)
	if got := getTask(t, h2, "gw", 1, http.StatusOK); string(got.Result) != result {
		t.Fatalf("conflict after restart overwrote result: %s", got.Result)
	}

	// Closing and reopening once more still yields the exact saved result.
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}
	store3 := reopenPersistent(t, dir)
	h3 := NewHandler(store3)
	if got := getTask(t, h3, "gw", 1, http.StatusOK); string(got.Result) != result {
		t.Fatalf("result after second restart = %s, want exact %s", got.Result, result)
	}
	reportTask(t, h3, "gw", 1,
		reportBody("rcpt-1", "stale-credential", true, "", result), http.StatusOK)
}

func TestPersistentTaskFailureRestores(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createTask(t, h, "gw", taskBody("req-1", 30), http.StatusCreated)
	claim := claimTask(t, h, "gw", http.StatusOK)
	reportTask(t, h, "gw", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, false, "disk full", ""), http.StatusCreated)
	nextClaimable := getTask(t, h, "gw", 1, http.StatusOK).NextClaimableAt
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	task := getTask(t, h2, "gw", 1, http.StatusOK)
	if task.Status != "waiting" || task.FailureReason != "disk full" {
		t.Fatalf("task after restart = %+v", task)
	}
	if !task.NextClaimableAt.Equal(nextClaimable) {
		t.Fatalf("next claimable after restart = %s, want %s", task.NextClaimableAt, nextClaimable)
	}
}

func TestPersistentTaskFailureReceiptRetryRestores(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createTask(t, h, "gw", taskBody("req-1", 30), http.StatusCreated)
	claim := claimTask(t, h, "gw", http.StatusOK)
	first := reportTask(t, h, "gw", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, false, "disk full", ""), http.StatusCreated)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	// The received failure receipt replays after the restart with its first
	// receive time; a changed reason still conflicts.
	retry := reportTask(t, h2, "gw", 1,
		reportBody("rcpt-1", claim.Credential, false, "disk full", ""), http.StatusOK)
	if retry.ReceiptID != "rcpt-1" || retry.Reason != "disk full" || !retry.ReceivedAt.Equal(first.ReceivedAt) {
		t.Fatalf("retry after restart = %+v, want receivedAt %s", retry, first.ReceivedAt)
	}
	reportTask(t, h2, "gw", 1,
		reportBody("rcpt-1", claim.Credential, false, "other reason", ""), http.StatusConflict)

	// The replay did not add a failure or an audit record.
	audit := listAudit(t, h2, "gw", 1)
	if got := joinEvents(eventsOf(audit)); got != "created,claimed,failed" {
		t.Fatalf("audit after restart = %s", got)
	}
}

// A receipt number's task ownership survives a restart: a number accepted by
// task 1 still conflicts on another task after reopening the directory, while
// its first receipt keeps replaying on task 1 with the original receive time.
func TestPersistentTaskReceiptOwnershipRestores(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createTask(t, h, "gw", taskBody("req-1", 30), http.StatusCreated)
	claim := claimTask(t, h, "gw", http.StatusOK)
	first := reportTask(t, h, "gw", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, true, "", `{"ok":true}`), http.StatusCreated)
	createTask(t, h, "gw", taskBody("req-2", 30), http.StatusCreated)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	// Task 2 is claimed in the new process with its own fresh credential.
	claim2 := claimTask(t, h2, "gw", http.StatusOK)
	if claim2.Task.ID != 2 {
		t.Fatalf("claim after restart took task %d, want 2", claim2.Task.ID)
	}
	// The number stays bound to task 1: task 2 cannot take it.
	reportTask(t, h2, "gw", 2,
		reportBody("rcpt-1", claim2.Credential, true, "", `{"ok":true}`), http.StatusConflict)
	if task := getTask(t, h2, "gw", 2, http.StatusOK); task.Status != "in_progress" {
		t.Fatalf("conflict after restart disturbed task 2: %+v", task)
	}

	// An unused number completes task 2.
	reportTask(t, h2, "gw", 2,
		reportBody("rcpt-2", claim2.Credential, true, "", `{"ok":true}`), http.StatusCreated)

	// The original receipt replays on task 1 with the first receive time.
	retry := reportTask(t, h2, "gw", 1,
		reportBody("rcpt-1", "any-credential-now", true, "", `{"ok":true}`), http.StatusOK)
	if !retry.ReceivedAt.Equal(first.ReceivedAt) {
		t.Fatalf("retry after restart receivedAt = %s, want %s", retry.ReceivedAt, first.ReceivedAt)
	}
}

func TestPersistentTaskCancelRestores(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createTask(t, h, "gw", taskBody("req-1", 30), http.StatusCreated)
	cancelTask(t, h, "gw", 1, http.StatusOK)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	task := getTask(t, h2, "gw", 1, http.StatusOK)
	if task.Status != "canceled" || task.CompletedAt == nil {
		t.Fatalf("task after restart = %+v", task)
	}
	// Duplicate cancel still returns the original; a report is rejected.
	cancelTask(t, h2, "gw", 1, http.StatusOK)
	audit := listAudit(t, h2, "gw", 1)
	if got := joinEvents(eventsOf(audit)); got != "created,canceled" {
		t.Fatalf("audit after restart = %s", got)
	}
}

// --- runtime write failures return 503 and roll everything back --------------

func TestPersistentTaskWriteFailureReturns503(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	real := store.wal.(*walFile)
	failing := &failingWAL{inner: real}
	store.wal = failing

	// Failing create: no task, no dedup record.
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/tasks", taskBody("req-1", 30))
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing create = %d, want 503: %s", r.Code, r.Body.String())
	}
	if list := decodeBody[struct {
		Tasks []taskResponse `json:"tasks"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/tasks", "")); len(list.Tasks) != 0 {
		t.Fatalf("failed create left tasks: %+v", list.Tasks)
	}

	// Retry succeeds.
	store.wal = real
	createTask(t, h, "gw", taskBody("req-1", 30), http.StatusCreated)

	// Failing claim: task stays pending.
	store.wal = failing
	failing.didFail = false
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/tasks/claim", "")
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing claim = %d, want 503: %s", r.Code, r.Body.String())
	}
	if task := getTask(t, h, "gw", 1, http.StatusOK); task.Status != "pending" {
		t.Fatalf("failed claim changed task: %+v", task)
	}

	// Retry succeeds.
	store.wal = real
	claim := claimTask(t, h, "gw", http.StatusOK)

	// Failing report: task stays in progress, no receipt.
	store.wal = failing
	failing.didFail = false
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/tasks/1/reports",
		reportBody("rcpt-1", claim.Credential, true, "", `{"ok":true}`))
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing report = %d, want 503: %s", r.Code, r.Body.String())
	}
	if task := getTask(t, h, "gw", 1, http.StatusOK); task.Status != "in_progress" {
		t.Fatalf("failed report changed task: %+v", task)
	}

	// Retry succeeds and the receipt is stored once.
	store.wal = real
	reportTask(t, h, "gw", 1,
		reportBody("rcpt-1", claim.Credential, true, "", `{"ok":true}`), http.StatusCreated)
	reportTask(t, h, "gw", 1,
		reportBody("rcpt-1", claim.Credential, true, "", `{"ok":true}`), http.StatusOK)
}

func eventsOf(audit []taskAuditResponse) []string {
	events := make([]string, len(audit))
	for i, rec := range audit {
		events[i] = rec.Event
	}
	return events
}

func joinEvents(events []string) string {
	result := ""
	for i, event := range events {
		if i > 0 {
			result += ","
		}
		result += event
	}
	return result
}

// --- timeout write failure rolls back and is retried -------------------------

func TestPersistentTaskTimeoutWriteFailureReturns503(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createTask(t, h, "gw", taskBody("req-1", 1), http.StatusCreated)
	claim := claimTask(t, h, "gw", http.StatusOK)

	// Advance past the deadline so the next read would time the task out.
	store.now = func() time.Time { return claim.Deadline.Add(2 * time.Second) }

	real := store.wal.(*walFile)
	store.wal = &permanentFailingWAL{}

	// The read evaluates the timeout, fails to persist it, and returns 503.
	r := doRequest(t, h, http.MethodGet, "/v1/devices/gw/tasks/1", "")
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("timeout read = %d, want 503: %s", r.Code, r.Body.String())
	}
	// The in-memory state is unchanged: the task is still in progress.
	if ts := store.devices["gw"].tasks[0]; ts.task.Status != "in_progress" {
		t.Fatalf("failed timeout changed task: %+v", ts.task)
	}

	// Retry with healthy storage: the timeout commits exactly once.
	store.wal = real
	task := getTask(t, h, "gw", 1, http.StatusOK)
	if task.Status != "waiting" || task.FailureReason != "deadline exceeded" {
		t.Fatalf("retried timeout: %+v", task)
	}
	audit := listAudit(t, h, "gw", 1)
	if got := joinEvents(eventsOf(audit)); got != "created,claimed,timed_out" {
		t.Fatalf("audit after retried timeout = %s", got)
	}
}

// --- cancel write failure keeps the in-progress execution alive --------------

// A cancel of an in-progress task whose cancel record cannot be persisted
// returns 503 and changes nothing: the task keeps executing under the same
// claim (attempt count, claim time, deadline and credential), gains no
// completion time, result or canceled audit record, and the device stays
// occupied so the next claim keeps returning 204 without advancing the
// pending task. The detail and list views report the same un-canceled state.
func TestPersistentTaskCancelWriteFailureKeepsExecution(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	// Task 1 is claimed and stays within its deadline; task 2 waits pending.
	createTask(t, h, "gw", taskBody("req-1", 30), http.StatusCreated)
	claim := claimTask(t, h, "gw", http.StatusOK)
	if claim.Task.ID != 1 {
		t.Fatalf("first claim took task %d, want 1", claim.Task.ID)
	}
	createTask(t, h, "gw", taskBody("req-2", 30), http.StatusCreated)
	claimedAt := claim.Task.ClaimedAt
	deadline := claim.Deadline

	// Fail the cancel write (one unacknowledged append, as in a disk outage).
	real := store.wal.(*walFile)
	store.wal = &failingWAL{inner: real}
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/tasks/1/cancel", "")
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing cancel = %d, want 503: %s", r.Code, r.Body.String())
	}

	// The task is still the same in-progress execution.
	task := getTask(t, h, "gw", 1, http.StatusOK)
	if task.Status != "in_progress" {
		t.Fatalf("failed cancel changed status to %q, want in_progress", task.Status)
	}
	if task.Attempts != 1 {
		t.Fatalf("failed cancel changed attempts to %d, want 1", task.Attempts)
	}
	if task.ClaimedAt == nil || !task.ClaimedAt.Equal(*claimedAt) {
		t.Fatalf("failed cancel changed claimedAt: %+v, want %s", task.ClaimedAt, claimedAt)
	}
	if task.Deadline == nil || !task.Deadline.Equal(deadline) {
		t.Fatalf("failed cancel changed deadline: %+v, want %s", task.Deadline, deadline)
	}
	if task.CompletedAt != nil {
		t.Fatalf("failed cancel set completedAt: %s", task.CompletedAt)
	}
	if len(task.Result) != 0 || task.FailureReason != "" {
		t.Fatalf("failed cancel produced an outcome: result=%s reason=%q", task.Result, task.FailureReason)
	}
	// The execution credential is still bound to the attempt.
	if ts := store.devices["gw"].tasks[0]; ts.task.Credential != claim.Credential {
		t.Fatalf("failed cancel revoked credential %q, want %q", ts.task.Credential, claim.Credential)
	}

	// The audit trail keeps exactly the pre-cancel records, with no canceled event.
	if got := joinEvents(eventsOf(listAudit(t, h, "gw", 1))); got != "created,claimed" {
		t.Fatalf("task 1 audit after failed cancel = %s, want created,claimed", got)
	}

	// The device is still occupied by task 1: another claim is 204 and the
	// pending task is not pulled forward.
	claimTask(t, h, "gw", http.StatusNoContent)
	task2 := getTask(t, h, "gw", 2, http.StatusOK)
	if task2.Status != "pending" || task2.Attempts != 0 || task2.ClaimedAt != nil {
		t.Fatalf("pending task advanced despite occupied device: %+v", task2)
	}
	if got := joinEvents(eventsOf(listAudit(t, h, "gw", 2))); got != "created" {
		t.Fatalf("task 2 audit = %s, want created", got)
	}

	// The list view reflects the same state, not just the cancel error code.
	list := decodeBody[struct {
		Tasks []taskResponse `json:"tasks"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/tasks", ""))
	if len(list.Tasks) != 2 {
		t.Fatalf("task list has %d entries, want 2", len(list.Tasks))
	}
	first := list.Tasks[0]
	if first.Status != "in_progress" || first.Attempts != 1 || first.CompletedAt != nil || len(first.Result) != 0 {
		t.Fatalf("list task 1 = %+v", first)
	}
	if first.ClaimedAt == nil || !first.ClaimedAt.Equal(*claimedAt) ||
		first.Deadline == nil || !first.Deadline.Equal(deadline) {
		t.Fatalf("list task 1 lost claim times: %+v", first)
	}
	if list.Tasks[1].Status != "pending" {
		t.Fatalf("list task 2 = %+v, want pending", list.Tasks[1])
	}
}

// Once storage recovers within the original deadline, re-issuing the cancel
// returns 200 and fully cancels: the completion time is the successful cancel's
// server time, attempts are unchanged, the claim times disappear and exactly
// one canceled audit record is appended while the earlier records keep their
// order and content. The released device then claims the previously blocked
// task; the canceled task can no longer be claimed and a report on its now
// invalid credential is 409, saving no result, receipt or audit record.
func TestPersistentTaskCancelWriteFailureRecoversAndCancels(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	clock := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return clock }
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	createTask(t, h, "gw", taskBody("req-1", 30), http.StatusCreated)
	claim := claimTask(t, h, "gw", http.StatusOK)
	createTask(t, h, "gw", taskBody("req-2", 30), http.StatusCreated)
	beforeAudit := listAudit(t, h, "gw", 1)

	// Failed cancel: 503, execution preserved.
	real := store.wal.(*walFile)
	store.wal = &failingWAL{inner: real}
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/tasks/1/cancel", ""); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing cancel = %d, want 503: %s", r.Code, r.Body.String())
	}

	// Storage recovers while the original deadline is still open.
	clock = clock.Add(5 * time.Second)
	store.wal = real
	canceled := cancelTask(t, h, "gw", 1, http.StatusOK)
	if canceled.Status != "canceled" {
		t.Fatalf("retried cancel status = %q, want canceled", canceled.Status)
	}
	if canceled.Attempts != 1 {
		t.Fatalf("cancel changed attempts to %d, want 1", canceled.Attempts)
	}
	if canceled.CompletedAt == nil || !canceled.CompletedAt.Equal(clock) {
		t.Fatalf("cancel completedAt = %+v, want success time %s", canceled.CompletedAt, clock)
	}
	if canceled.ClaimedAt != nil || canceled.Deadline != nil {
		t.Fatalf("canceled view still carries claim times: %+v", canceled)
	}

	// The query view agrees; the old claim time and deadline are gone.
	task := getTask(t, h, "gw", 1, http.StatusOK)
	if task.Status != "canceled" || task.Attempts != 1 || task.ClaimedAt != nil || task.Deadline != nil {
		t.Fatalf("task after recovered cancel: %+v", task)
	}
	if task.CompletedAt == nil || !task.CompletedAt.Equal(clock) || len(task.Result) != 0 {
		t.Fatalf("task outcome after recovered cancel: %+v", task)
	}

	// Exactly one canceled audit record follows the unchanged earlier records.
	audit := listAudit(t, h, "gw", 1)
	if len(audit) != 3 {
		t.Fatalf("audit len = %d, want 3: %+v", len(audit), audit)
	}
	if audit[0] != beforeAudit[0] || audit[1] != beforeAudit[1] {
		t.Fatalf("cancel reordered/changed earlier audit: %+v vs %+v", audit[:2], beforeAudit)
	}
	cancelRec := audit[2]
	if cancelRec.Event != "canceled" || cancelRec.FromStatus != "in_progress" ||
		cancelRec.ToStatus != "canceled" || !cancelRec.At.Equal(clock) {
		t.Fatalf("cancel audit record = %+v", cancelRec)
	}

	// The released device claims the task the failed cancel had blocked; the
	// canceled task is not offered.
	claim2 := claimTask(t, h, "gw", http.StatusOK)
	if claim2.Task.ID != 2 {
		t.Fatalf("claim after cancel took task %d, want 2", claim2.Task.ID)
	}
	claimTask(t, h, "gw", http.StatusNoContent)

	// A report on the canceled task with its old credential is 409 and saves
	// nothing: no result, no receipt binding and no new audit record.
	reportTask(t, h, "gw", 1,
		reportBody("rcpt-blocked", claim.Credential, true, "", `{"ok":true}`), http.StatusConflict)
	task = getTask(t, h, "gw", 1, http.StatusOK)
	if task.Status != "canceled" || len(task.Result) != 0 || !task.CompletedAt.Equal(clock) {
		t.Fatalf("conflicting report disturbed the canceled task: %+v", task)
	}
	ts1 := store.devices["gw"].tasks[0]
	if len(ts1.reports) != 0 {
		t.Fatalf("conflicting report saved a receipt: %+v", ts1.reports)
	}
	if _, bound := store.devices["gw"].taskReceipts["rcpt-blocked"]; bound {
		t.Fatalf("conflicting report bound the receipt number to task 1")
	}
	if len(listAudit(t, h, "gw", 1)) != 3 {
		t.Fatalf("conflicting report added an audit record")
	}

	// The rejected number was not occupied: task 2 accepts it on its own claim.
	reportTask(t, h, "gw", 2,
		reportBody("rcpt-blocked", claim2.Credential, true, "", `{"ok":true}`), http.StatusCreated)
	// Nothing claimable remains: the canceled task must never be claimed.
	claimTask(t, h, "gw", http.StatusNoContent)
}

// A cancel that could not be persisted must not have revoked the execution
// credential. With storage healthy again and the deadline still open, the
// device reports success against the original claim: 201, the result is saved
// and the task ends as succeeded with no canceled audit record. This is the
// deliberate counterpart of the post-cancel 409 above: only a successfully
// persisted cancel invalidates the credential; the earlier 503 does not.
func TestPersistentTaskFailedCancelKeepsCredentialForSuccessReport(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	clock := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return clock }
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	createTask(t, h, "gw", taskBody("req-1", 30), http.StatusCreated)
	createTask(t, h, "gw", taskBody("req-2", 30), http.StatusCreated)
	claim := claimTask(t, h, "gw", http.StatusOK) // task 1

	// The cancel write fails: 503, the execution is untouched.
	real := store.wal.(*walFile)
	store.wal = &failingWAL{inner: real}
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/tasks/1/cancel", ""); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing cancel = %d, want 503: %s", r.Code, r.Body.String())
	}

	// Storage recovers while the original deadline is still open.
	clock = clock.Add(5 * time.Second)
	store.wal = real

	// The original claim still authorizes the success report (not a 409).
	report := reportTask(t, h, "gw", 1,
		reportBody("rcpt-1", claim.Credential, true, "", `{"ok":true}`), http.StatusCreated)
	if !report.Success || !report.ReceivedAt.Equal(clock) {
		t.Fatalf("report after failed cancel = %+v", report)
	}
	task := getTask(t, h, "gw", 1, http.StatusOK)
	if task.Status != "succeeded" || task.CompletedAt == nil || !task.CompletedAt.Equal(clock) {
		t.Fatalf("task after late success report: %+v", task)
	}
	if string(task.Result) != `{"ok":true}` {
		t.Fatalf("saved result = %s, want {\"ok\":true}", task.Result)
	}
	if task.ClaimedAt != nil || task.Deadline != nil || task.Attempts != 1 {
		t.Fatalf("success report changed claim fields: %+v", task)
	}
	// No canceled record was ever committed by the failed attempt.
	if got := joinEvents(eventsOf(listAudit(t, h, "gw", 1))); got != "created,claimed,succeeded" {
		t.Fatalf("audit = %s, want created,claimed,succeeded", got)
	}

	// The success released the device, so the previously blocked task advances.
	claim2 := claimTask(t, h, "gw", http.StatusOK)
	if claim2.Task.ID != 2 {
		t.Fatalf("claim after success took task %d, want 2", claim2.Task.ID)
	}

	// Restart proves the failed cancel left no durable trace: the task is
	// recovered as succeeded with the same result and audit trail.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	task = getTask(t, h2, "gw", 1, http.StatusOK)
	if task.Status != "succeeded" || string(task.Result) != `{"ok":true}` || task.CompletedAt == nil {
		t.Fatalf("task after restart = %+v", task)
	}
	if got := joinEvents(eventsOf(listAudit(t, h2, "gw", 1))); got != "created,claimed,succeeded" {
		t.Fatalf("audit after restart = %s, want created,claimed,succeeded", got)
	}
}

// permanentFailingWAL always fails to append, leaving in-memory state untouched.
type permanentFailingWAL struct{}

func (w *permanentFailingWAL) appendRecord(recType byte, value any) error {
	return fmt.Errorf("simulated permanent disk failure")
}

// --- an old data directory (pre-task records) still opens --------------------

func TestPersistentOldDataDirStillOpens(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	// Write only pre-task records.
	publishConfig(t, h, "gw", publishBody("r1", 0, `{"v":1}`), http.StatusCreated)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen with the new code: the old records recover and tasks work fresh.
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	status := getConfigStatus(t, h2, "gw")
	if status.TargetVersion != 1 {
		t.Fatalf("old config after restart = %+v", status)
	}
	createTask(t, h2, "gw", taskBody("req-1", 30), http.StatusCreated)
	list := decodeBody[struct {
		Tasks []taskResponse `json:"tasks"`
	}](t, doRequest(t, h2, http.MethodGet, "/v1/devices/gw/tasks", ""))
	if len(list.Tasks) != 1 || list.Tasks[0].Status != "pending" {
		t.Fatalf("tasks on old data dir = %+v", list.Tasks)
	}
}
