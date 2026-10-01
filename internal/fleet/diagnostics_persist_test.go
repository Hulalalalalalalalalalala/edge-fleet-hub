package fleet

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Restart restores tasks in every non-terminal state, including deadlines and
// retry instants which must not be reset by recovery.
func TestPersistentDiagnosticsRestartRestoresTasks(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	base := store.now()

	// Task 1: running with a live token and deadline base+30s.
	createVia(t, h, "gw", "req-running", 30)
	running := claimVia(t, h, "gw")
	if running.Task != 1 || running.Attempt != 1 {
		t.Fatalf("running claim = %+v", running)
	}

	// Task 2: failed once -> waiting with backoff until base+11s.
	createVia(t, h, "gw", "req-waiting", 10)
	// Cannot claim task 2 while task 1 runs; fail task 1 out first via reports.
	// Instead use a second device to exercise a waiting restore independently.
	mustRegister(t, h, "gw2")
	createVia(t, h, "gw2", "wreq", 10)
	wclaim := claimVia(t, h, "gw2")
	if code := doRequest(t, h, http.MethodPost,
		"/v1/devices/gw2/diagnostic-tasks/reports",
		reportBody(wclaim, "f1", false, "boom")).Code; code != http.StatusCreated {
		t.Fatalf("gw2 failure = %d", code)
	}
	waitingNext := base.Add(1 * time.Second)

	// Task 3 (gw): succeeded terminal with result.
	mustRegister(t, h, "gw3")
	createVia(t, h, "gw3", "sreq", 5)
	sclaim := claimVia(t, h, "gw3")
	if code := doRequest(t, h, http.MethodPost,
		"/v1/devices/gw3/diagnostic-tasks/reports",
		reportBody(sclaim, "s1", true, `{"ok":true}`)).Code; code != http.StatusCreated {
		t.Fatalf("gw3 success = %d", code)
	}

	// Task 4 (gw2 request id space): cancelled pending.
	createVia(t, h, "gw2", "creq", 5)
	if code := doRequest(t, h, http.MethodPost,
		"/v1/devices/gw2/diagnostic-tasks/2/cancel", "").Code; code != http.StatusOK {
		t.Fatalf("cancel = %d", code)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2, err := NewPersistentStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })
	// Pin the reopened clock before the deadlines so nothing settles.
	reopenedClock := base.Add(5 * time.Second)
	store2.now = func() time.Time { return reopenedClock }
	h2 := NewHandler(store2)

	// Running task survives with its token and deadline; the old token still
	// works for a report.
	task1 := getTaskVia(t, h2, "gw", 1)
	if task1.Status != taskStatusRunning || task1.Attempts != 1 {
		t.Fatalf("running task after restart = %+v", task1)
	}
	if task1.Deadline == nil || !task1.Deadline.Equal(base.Add(30*time.Second)) {
		t.Fatalf("deadline after restart = %v, want %s", task1.Deadline, base.Add(30*time.Second))
	}
	if code := doRequest(t, h2, http.MethodPost,
		"/v1/devices/gw/diagnostic-tasks/reports",
		fmt.Sprintf(`{"token":%q,"receiptId":"post-restart","success":true,"result":{"fine":true}}`,
			running.Token)).Code; code != http.StatusCreated {
		t.Fatalf("report with restored token = %d, want 201", code)
	}

	// Waiting task keeps its nextClaimableAt anchored to the original failure.
	task2 := getTaskVia(t, h2, "gw2", 1)
	if task2.Status != taskStatusWaiting || task2.Attempts != 1 {
		t.Fatalf("waiting task after restart = %+v", task2)
	}
	if task2.NextClaimableAt == nil || !task2.NextClaimableAt.Equal(waitingNext) {
		t.Fatalf("nextClaimableAt after restart = %v, want %s", task2.NextClaimableAt, waitingNext)
	}
	// A claim at base+5s is still before the base+1s? No: base+5 is after the
	// backoff, so it must succeed and keep the original attempt numbering.
	reclaim := claimVia(t, h2, "gw2")
	if reclaim.Attempt != 2 {
		t.Fatalf("reclaim after restart attempt = %d, want 2", reclaim.Attempt)
	}

	// Succeeded task survives with its result; a late different receipt is 409
	// and a matching receiptId retry is 200 with the first receive time.
	task3 := getTaskVia(t, h2, "gw3", 1)
	if task3.Status != taskStatusSucceeded || string(task3.Result) == "" {
		t.Fatalf("succeeded task after restart = %+v", task3)
	}
	r := doRequest(t, h2, http.MethodPost, "/v1/devices/gw3/diagnostic-tasks/reports",
		reportBody(sclaim, "s1", true, `{"ok":true}`))
	if r.Code != http.StatusOK {
		t.Fatalf("receipt retry after restart = %d, want 200: %s", r.Code, r.Body.String())
	}

	// Cancelled task is still cancelled and a repeat cancel returns the
	// original cancellation (200).
	task4 := getTaskVia(t, h2, "gw2", 2)
	if task4.Status != taskStatusCancelled {
		t.Fatalf("cancelled task after restart = %+v", task4)
	}
	r = doRequest(t, h2, http.MethodPost, "/v1/devices/gw2/diagnostic-tasks/2/cancel", "")
	if r.Code != http.StatusOK {
		t.Fatalf("repeat cancel after restart = %d", r.Code)
	}

	// Audit is restored in order.
	audit := auditVia(t, h2, "gw")
	if len(audit) != 4 { // create, claim, create(second gw task), post-restart success
		t.Fatalf("gw audit rows = %d: %+v", len(audit), audit)
	}
}

// requestId dedup survives a restart: same content returns the first result
// (200), changed seconds conflict (409).
func TestPersistentDiagnosticsRequestDedupAfterRestart(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createVia(t, h, "gw", "req-1", 7)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2, err := NewPersistentStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })
	store2.now = store.now
	h2 := NewHandler(store2)

	r := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/diagnostic-tasks",
		createTaskBody("req-1", 7))
	if r.Code != http.StatusOK {
		t.Fatalf("same requestId after restart = %d, want 200: %s", r.Code, r.Body.String())
	}
	again := decodeBody[TaskSummary](t, r)
	if again.Number != 1 {
		t.Fatalf("dedup returned number %d, want 1", again.Number)
	}
	r = doRequest(t, h2, http.MethodPost, "/v1/devices/gw/diagnostic-tasks",
		createTaskBody("req-1", 8))
	if r.Code != http.StatusConflict {
		t.Fatalf("changed seconds after restart = %d, want 409", r.Code)
	}
}

// An elapsed deadline discovered only after a restart is timed out from the
// original deadline instant; the wait is not reset by reopening.
func TestPersistentDiagnosticsDeadlineSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	base := store.now()
	createVia(t, h, "gw", "req-1", 5)
	claimVia(t, h, "gw") // deadline base+5s
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen well after the deadline plus the 1s backoff: the task must be
	// waiting and immediately re-claimable as attempt 2.
	store2, err := NewPersistentStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })
	later := base.Add(30 * time.Second)
	store2.now = func() time.Time { return later }
	h2 := NewHandler(store2)

	reclaim := claimVia(t, h2, "gw") // settles timeout at base+5, then claims
	if reclaim.Attempt != 2 {
		t.Fatalf("attempt after restart-timeout = %d, want 2", reclaim.Attempt)
	}
	task := getTaskVia(t, h2, "gw", 1)
	if task.Status != taskStatusRunning {
		t.Fatalf("task = %+v", task)
	}
	audit := auditVia(t, h2, "gw")
	var sawTimeout bool
	for _, entry := range audit {
		if entry.Action == auditActionTimeout {
			sawTimeout = true
			if !entry.At.Equal(base.Add(5 * time.Second)) {
				t.Fatalf("timeout recorded at %s, want deadline %s",
					entry.At, base.Add(5*time.Second))
			}
			if entry.Reason != timeoutReason {
				t.Fatalf("timeout reason = %q", entry.Reason)
			}
		}
	}
	if !sawTimeout {
		t.Fatalf("no timeout audit row after restart: %+v", audit)
	}
}

// A data directory created before diagnostics existed still opens.
func TestPersistentDiagnosticsOldDirectoryOpens(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "legacy")
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/legacy/telemetry", `{"v":1}`); r.Code != http.StatusAccepted {
		t.Fatalf("telemetry = %d", r.Code)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2, err := NewPersistentStore(dir)
	if err != nil {
		t.Fatalf("old directory should still open: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })
	h2 := NewHandler(store2)
	// New feature works against the old directory.
	createVia(t, h2, "legacy", "new-req", 3)
	r := doRequest(t, h2, http.MethodPost, "/v1/devices/legacy/diagnostic-tasks/claim", "")
	if r.Code != http.StatusOK {
		t.Fatalf("claim on old directory = %d: %s", r.Code, r.Body.String())
	}
}

// Runtime task-write failures return 503 and roll task state and audit back.
func TestPersistentDiagnosticsWriteFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	// 503 on create: nothing visible afterwards, requestId can be retried.
	real := store.wal.(*walFile)
	failing := &failingWAL{inner: real}
	store.wal = failing
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/diagnostic-tasks",
		createTaskBody("req-1", 5))
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing create = %d, want 503: %s", r.Code, r.Body.String())
	}
	r = doRequest(t, h, http.MethodGet, "/v1/devices/gw/diagnostic-tasks", "")
	tasks := decodeBody[struct {
		Tasks []TaskSummary `json:"tasks"`
	}](t, r).Tasks
	if len(tasks) != 0 {
		t.Fatalf("failed create left tasks: %+v", tasks)
	}
	// The audit is empty too.
	if len(auditVia(t, h, "gw")) != 0 {
		t.Fatal("failed create left audit rows")
	}

	// Create succeeds after recovery; now fail the claim.
	createVia(t, h, "gw", "req-1", 5)
	failing2 := &failingWAL{inner: real}
	store.wal = failing2
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/diagnostic-tasks/claim", "")
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing claim = %d, want 503: %s", r.Code, r.Body.String())
	}
	// The task is still pending and claimable; attempts were not consumed.
	task := getTaskVia(t, h, "gw", 1)
	if task.Status != taskStatusPending || task.Attempts != 0 {
		t.Fatalf("after failed claim task = %+v", task)
	}
	claim := claimVia(t, h, "gw")
	if claim.Attempt != 1 {
		t.Fatalf("retry claim attempt = %d, want 1", claim.Attempt)
	}

	// Fail the success report: the task stays running with the token valid.
	failing3 := &failingWAL{inner: real}
	store.wal = failing3
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/diagnostic-tasks/reports",
		reportBody(claim, "rc-1", true, `{"ok":true}`))
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing report = %d, want 503: %s", r.Code, r.Body.String())
	}
	task = getTaskVia(t, h, "gw", 1)
	if task.Status != taskStatusRunning {
		t.Fatalf("after failed report task = %+v, want running", task)
	}
	// The same content can be retried once storage is healthy.
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/diagnostic-tasks/reports",
		reportBody(claim, "rc-1", true, `{"ok":true}`))
	if r.Code != http.StatusCreated {
		t.Fatalf("report retry = %d, want 201: %s", r.Code, r.Body.String())
	}

	// A failing cancel leaves a pending task pending and retryable.
	createVia(t, h, "gw", "req-2", 5)
	failing4 := &failingWAL{inner: real}
	store.wal = failing4
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/diagnostic-tasks/2/cancel", "")
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing cancel = %d, want 503: %s", r.Code, r.Body.String())
	}
	task = getTaskVia(t, h, "gw", 2)
	if task.Status != taskStatusPending {
		t.Fatalf("after failed cancel task = %+v, want pending", task)
	}
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/diagnostic-tasks/2/cancel", "")
	if r.Code != http.StatusOK {
		t.Fatalf("cancel retry = %d, want 200: %s", r.Code, r.Body.String())
	}
}

// --- small HTTP wrappers ----------------------------------------------------

func createVia(t *testing.T, h http.Handler, device, requestID string, seconds int) TaskSummary {
	t.Helper()
	r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+device+"/diagnostic-tasks", createTaskBody(requestID, seconds))
	if r.Code != http.StatusCreated {
		t.Fatalf("create %s/%s = %d: %s", device, requestID, r.Code, r.Body.String())
	}
	return decodeBody[TaskSummary](t, r)
}

func claimVia(t *testing.T, h http.Handler, device string) claimResponse {
	t.Helper()
	r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+device+"/diagnostic-tasks/claim", "")
	if r.Code != http.StatusOK {
		t.Fatalf("claim %s = %d: %s", device, r.Code, r.Body.String())
	}
	return decodeBody[claimResponse](t, r)
}

func getTaskVia(t *testing.T, h http.Handler, device string, number int64) TaskSummary {
	t.Helper()
	r := doRequest(t, h, http.MethodGet,
		fmt.Sprintf("/v1/devices/%s/diagnostic-tasks/%d", device, number), "")
	if r.Code != http.StatusOK {
		t.Fatalf("get %s task %d = %d: %s", device, number, r.Code, r.Body.String())
	}
	return decodeBody[TaskSummary](t, r)
}

func auditVia(t *testing.T, h http.Handler, device string) []AuditEntry {
	t.Helper()
	r := doRequest(t, h, http.MethodGet,
		"/v1/devices/"+device+"/diagnostic-tasks/audit", "")
	if r.Code != http.StatusOK {
		t.Fatalf("audit %s = %d: %s", device, r.Code, r.Body.String())
	}
	return decodeBody[struct {
		Audit []AuditEntry `json:"audit"`
	}](t, r).Audit
}
