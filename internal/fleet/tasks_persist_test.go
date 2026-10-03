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

func TestPersistentTaskFailureRetryRestores(t *testing.T) {
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

	// The received failure receipt survives the restart: an identical retry
	// returns it unchanged, a changed reason still conflicts.
	retry := reportTask(t, h2, "gw", 1,
		reportBody("rcpt-1", claim.Credential, false, "disk full", ""), http.StatusOK)
	if retry.ReceiptID != "rcpt-1" || retry.Reason != "disk full" ||
		!retry.ReceivedAt.Equal(first.ReceivedAt) {
		t.Fatalf("retry after restart = %+v, want the first receipt %+v", retry, first)
	}
	reportTask(t, h2, "gw", 1,
		reportBody("rcpt-1", claim.Credential, false, "out of memory", ""), http.StatusConflict)

	// The retry did not add a failure or an audit record.
	task := getTask(t, h2, "gw", 1, http.StatusOK)
	if task.Status != "waiting" || task.Attempts != 1 {
		t.Fatalf("task after retried failure = %+v", task)
	}
	audit := listAudit(t, h2, "gw", 1)
	if got := joinEvents(eventsOf(audit)); got != "created,claimed,failed" {
		t.Fatalf("audit after restart = %s", got)
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
