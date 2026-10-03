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

// permanentFailingWAL always fails to append, leaving in-memory state untouched.
type permanentFailingWAL struct{}

func (w *permanentFailingWAL) appendRecord(recType byte, value any) error {
	return fmt.Errorf("simulated permanent disk failure")
}

// --- a failed cancel keeps the in-progress execution eligible ----------------

// A cancel whose record cannot be persisted returns 503 and commits nothing:
// the task keeps its claim terms and audit trail, the device stays occupied,
// and the pending task behind it is not advanced. Once storage is healthy the
// cancel can be retried within the original deadline; only then is the
// credential revoked and the device released.
func TestPersistentTaskCancelWriteFailureKeepsExecution(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createTask(t, h, "gw", taskBody("req-1", 30), http.StatusCreated)
	createTask(t, h, "gw", taskBody("req-2", 30), http.StatusCreated)
	claim := claimTask(t, h, "gw", http.StatusOK)
	if claim.Task.ID != 1 {
		t.Fatalf("claimed task %d, want 1", claim.Task.ID)
	}
	auditBefore := listAudit(t, h, "gw", 1)
	if got := joinEvents(eventsOf(auditBefore)); got != "created,claimed" {
		t.Fatalf("audit before cancel = %s", got)
	}

	real := store.wal.(*walFile)
	store.wal = &permanentFailingWAL{}

	// The cancel cannot be recorded: 503.
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/tasks/1/cancel", "")
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing cancel = %d, want 503: %s", r.Code, r.Body.String())
	}

	// The task detail keeps the pre-cancel execution state: same attempt,
	// claim time and deadline, and no completion time or result.
	task := getTask(t, h, "gw", 1, http.StatusOK)
	if task.Status != "in_progress" || task.Attempts != 1 {
		t.Fatalf("failed cancel changed task: %+v", task)
	}
	if task.ClaimedAt == nil || claim.Task.ClaimedAt == nil || !task.ClaimedAt.Equal(*claim.Task.ClaimedAt) {
		t.Fatalf("claimedAt after failed cancel = %v, want %v", task.ClaimedAt, claim.Task.ClaimedAt)
	}
	if task.Deadline == nil || !task.Deadline.Equal(claim.Deadline) {
		t.Fatalf("deadline after failed cancel = %v, want %s", task.Deadline, claim.Deadline)
	}
	if task.CompletedAt != nil || task.Result != nil {
		t.Fatalf("failed cancel left completion state: %+v", task)
	}

	// The audit trail is untouched: no canceled record was added.
	audit := listAudit(t, h, "gw", 1)
	if got := joinEvents(eventsOf(audit)); got != "created,claimed" {
		t.Fatalf("audit after failed cancel = %s", got)
	}

	// The task list reflects the same un-canceled state.
	list := decodeBody[struct {
		Tasks []taskResponse `json:"tasks"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/tasks", ""))
	if len(list.Tasks) != 2 || list.Tasks[0].Status != "in_progress" || list.Tasks[0].CompletedAt != nil {
		t.Fatalf("list after failed cancel: %+v", list.Tasks)
	}
	if list.Tasks[0].ClaimedAt == nil || list.Tasks[0].Deadline == nil {
		t.Fatalf("list lost the claim terms: %+v", list.Tasks[0])
	}
	if list.Tasks[1].Status != "pending" || list.Tasks[1].Attempts != 0 {
		t.Fatalf("failed cancel advanced the pending task: %+v", list.Tasks[1])
	}

	// The device is still occupied by the in-progress task: the pending task
	// is not handed out.
	claimTask(t, h, "gw", http.StatusNoContent)
	if task2 := getTask(t, h, "gw", 2, http.StatusOK); task2.Status != "pending" || task2.Attempts != 0 {
		t.Fatalf("pending task advanced while device occupied: %+v", task2)
	}

	// Storage is healthy again; the retry lands within the original deadline
	// and is the cancel that counts.
	store.wal = real
	cancelAt := claim.Deadline.Add(-20 * time.Second)
	store.now = func() time.Time { return cancelAt }
	canceled := cancelTask(t, h, "gw", 1, http.StatusOK)
	if canceled.Status != "canceled" || canceled.CompletedAt == nil || !canceled.CompletedAt.Equal(cancelAt) {
		t.Fatalf("retried cancel = %+v, want completedAt %s", canceled, cancelAt)
	}
	// The cancel did not claim again and cleared the claim terms.
	if canceled.Attempts != 1 || canceled.ClaimedAt != nil || canceled.Deadline != nil {
		t.Fatalf("cancel response kept claim state: %+v", canceled)
	}

	// Exactly one canceled record was appended after the untouched records.
	audit = listAudit(t, h, "gw", 1)
	if got := joinEvents(eventsOf(audit)); got != "created,claimed,canceled" {
		t.Fatalf("audit after retried cancel = %s", got)
	}
	if !audit[0].At.Equal(auditBefore[0].At) || !audit[1].At.Equal(auditBefore[1].At) ||
		audit[1].Attempt != auditBefore[1].Attempt || audit[1].FromStatus != auditBefore[1].FromStatus {
		t.Fatalf("retried cancel rewrote earlier audit: %+v vs %+v", audit, auditBefore)
	}
	cancelRec := audit[2]
	if cancelRec.Seq != 3 || cancelRec.FromStatus != "in_progress" || cancelRec.ToStatus != "canceled" ||
		!cancelRec.At.Equal(cancelAt) {
		t.Fatalf("cancel audit record = %+v", cancelRec)
	}

	// Queries agree: canceled, no claim terms, no result.
	task = getTask(t, h, "gw", 1, http.StatusOK)
	if task.Status != "canceled" || task.ClaimedAt != nil || task.Deadline != nil ||
		task.Result != nil || task.CompletedAt == nil || !task.CompletedAt.Equal(cancelAt) {
		t.Fatalf("task after retried cancel: %+v", task)
	}
	list = decodeBody[struct {
		Tasks []taskResponse `json:"tasks"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/devices/gw/tasks", ""))
	if list.Tasks[0].Status != "canceled" || list.Tasks[0].ClaimedAt != nil || list.Tasks[0].Deadline != nil {
		t.Fatalf("list after retried cancel: %+v", list.Tasks[0])
	}

	// The released device now takes the previously blocked task; the canceled
	// task is never handed out again.
	claim2 := claimTask(t, h, "gw", http.StatusOK)
	if claim2.Task.ID != 2 || claim2.Attempt != 1 {
		t.Fatalf("claim after cancel = task %d attempt %d, want task 2 attempt 1", claim2.Task.ID, claim2.Attempt)
	}

	// The canceled task's credential is revoked: the first report with it
	// conflicts and stores nothing.
	reportTask(t, h, "gw", 1,
		reportBody("rcpt-1", claim.Credential, true, "", `{"ok":true}`), http.StatusConflict)
	task = getTask(t, h, "gw", 1, http.StatusOK)
	if task.Status != "canceled" || task.Result != nil {
		t.Fatalf("report on canceled task changed it: %+v", task)
	}
	if got := joinEvents(eventsOf(listAudit(t, h, "gw", 1))); got != "created,claimed,canceled" {
		t.Fatalf("audit after rejected report = %s", got)
	}
}

// The failed cancel did not revoke the execution credential either: with
// storage healthy and the deadline still ahead, the device can still complete
// the very attempt the failed cancel tried to stop. This is the opposite
// outcome of a committed cancel, which invalidates the credential.
func TestPersistentTaskCancelWriteFailureKeepsCredential(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createTask(t, h, "gw", taskBody("req-1", 30), http.StatusCreated)
	claim := claimTask(t, h, "gw", http.StatusOK)

	real := store.wal.(*walFile)
	store.wal = &permanentFailingWAL{}
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/tasks/1/cancel", "")
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing cancel = %d, want 503: %s", r.Code, r.Body.String())
	}
	store.wal = real

	// The original claim still reports successfully: 201 and the result is
	// stored; the earlier 503 is not held against the execution.
	report := reportTask(t, h, "gw", 1,
		reportBody("rcpt-1", claim.Credential, true, "", `{"ok":true,"checks":3}`), http.StatusCreated)
	if !report.Success {
		t.Fatalf("report after failed cancel = %+v", report)
	}
	task := getTask(t, h, "gw", 1, http.StatusOK)
	if task.Status != "succeeded" || string(task.Result) != `{"ok":true,"checks":3}` || task.CompletedAt == nil {
		t.Fatalf("task after report = %+v", task)
	}
	if got := joinEvents(eventsOf(listAudit(t, h, "gw", 1))); got != "created,claimed,succeeded" {
		t.Fatalf("audit after report = %s", got)
	}
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
