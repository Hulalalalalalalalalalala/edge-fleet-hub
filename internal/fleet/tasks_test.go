package fleet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- response types and helpers ---------------------------------------------

type taskResponse struct {
	ID              int64           `json:"id"`
	RequestID       string          `json:"requestId"`
	DurationSeconds int             `json:"durationSeconds"`
	Status          string          `json:"status"`
	Attempts        int             `json:"attempts"`
	CreatedAt       time.Time       `json:"createdAt"`
	NextClaimableAt time.Time       `json:"nextClaimableAt"`
	ClaimedAt       *time.Time      `json:"claimedAt"`
	Deadline        *time.Time      `json:"deadline"`
	Result          json.RawMessage `json:"result"`
	FailureReason   string          `json:"failureReason"`
	CompletedAt     *time.Time      `json:"completedAt"`
}

type claimResponse struct {
	Task       taskResponse `json:"task"`
	Attempt    int          `json:"attempt"`
	Credential string       `json:"credential"`
	Deadline   time.Time    `json:"deadline"`
}

type taskReportResponse struct {
	ReceiptID  string          `json:"receiptId"`
	Success    bool            `json:"success"`
	Reason     string          `json:"reason"`
	Result     json.RawMessage `json:"result"`
	ReceivedAt time.Time       `json:"receivedAt"`
}

type taskAuditResponse struct {
	Seq        int64     `json:"seq"`
	Event      string    `json:"event"`
	FromStatus string    `json:"fromStatus"`
	ToStatus   string    `json:"toStatus"`
	Attempt    int       `json:"attempt"`
	At         time.Time `json:"at"`
	Reason     string    `json:"reason"`
}

func taskBody(requestID string, seconds int) string {
	return fmt.Sprintf(`{"requestId":%q,"durationSeconds":%d}`, requestID, seconds)
}

func reportBody(receiptID, credential string, success bool, reason, result string) string {
	if success {
		return fmt.Sprintf(`{"receiptId":%q,"credential":%q,"success":true,"result":%s}`, receiptID, credential, result)
	}
	return fmt.Sprintf(`{"receiptId":%q,"credential":%q,"success":false,"reason":%q}`, receiptID, credential, reason)
}

func createTask(t *testing.T, h http.Handler, device, body string, wantStatus int) taskResponse {
	t.Helper()
	r := doRequest(t, h, http.MethodPost, fmt.Sprintf("/v1/devices/%s/tasks", device), body)
	if r.Code != wantStatus {
		t.Fatalf("create task status = %d, want %d: %s", r.Code, wantStatus, r.Body.String())
	}
	if wantStatus == http.StatusCreated || wantStatus == http.StatusOK {
		return decodeBody[taskResponse](t, r)
	}
	return taskResponse{}
}

func claimTask(t *testing.T, h http.Handler, device string, wantStatus int) claimResponse {
	t.Helper()
	r := doRequest(t, h, http.MethodPost, fmt.Sprintf("/v1/devices/%s/tasks/claim", device), "")
	if r.Code != wantStatus {
		t.Fatalf("claim status = %d, want %d: %s", r.Code, wantStatus, r.Body.String())
	}
	if wantStatus == http.StatusOK {
		return decodeBody[claimResponse](t, r)
	}
	return claimResponse{}
}

func reportTask(t *testing.T, h http.Handler, device string, taskID int64, body string, wantStatus int) taskReportResponse {
	t.Helper()
	r := doRequest(t, h, http.MethodPost, fmt.Sprintf("/v1/devices/%s/tasks/%d/reports", device, taskID), body)
	if r.Code != wantStatus {
		t.Fatalf("report status = %d, want %d: %s", r.Code, wantStatus, r.Body.String())
	}
	if wantStatus == http.StatusCreated || wantStatus == http.StatusOK {
		return decodeBody[taskReportResponse](t, r)
	}
	return taskReportResponse{}
}

func getTask(t *testing.T, h http.Handler, device string, taskID int64, wantStatus int) taskResponse {
	t.Helper()
	r := doRequest(t, h, http.MethodGet, fmt.Sprintf("/v1/devices/%s/tasks/%d", device, taskID), "")
	if r.Code != wantStatus {
		t.Fatalf("get task status = %d, want %d: %s", r.Code, wantStatus, r.Body.String())
	}
	if wantStatus == http.StatusOK {
		return decodeBody[taskResponse](t, r)
	}
	return taskResponse{}
}

func cancelTask(t *testing.T, h http.Handler, device string, taskID int64, wantStatus int) taskResponse {
	t.Helper()
	r := doRequest(t, h, http.MethodPost, fmt.Sprintf("/v1/devices/%s/tasks/%d/cancel", device, taskID), "")
	if r.Code != wantStatus {
		t.Fatalf("cancel status = %d, want %d: %s", r.Code, wantStatus, r.Body.String())
	}
	if wantStatus == http.StatusOK {
		return decodeBody[taskResponse](t, r)
	}
	return taskResponse{}
}

func listAudit(t *testing.T, h http.Handler, device string, taskID int64) []taskAuditResponse {
	t.Helper()
	r := doRequest(t, h, http.MethodGet, fmt.Sprintf("/v1/devices/%s/tasks/%d/audit", device, taskID), "")
	if r.Code != http.StatusOK {
		t.Fatalf("audit status = %d: %s", r.Code, r.Body.String())
	}
	return decodeBody[struct {
		Audit []taskAuditResponse `json:"audit"`
	}](t, r).Audit
}

// --- tests -------------------------------------------------------------------

func TestTaskCreate(t *testing.T) {
	store, _ := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "dev-1")

	task := createTask(t, h, "dev-1", taskBody("req-1", 30), http.StatusCreated)
	if task.ID != 1 || task.Status != "pending" || task.DurationSeconds != 30 || task.Attempts != 0 {
		t.Fatalf("unexpected task: %+v", task)
	}
	if task.CreatedAt.IsZero() || task.NextClaimableAt.IsZero() {
		t.Fatalf("task times not set: %+v", task)
	}

	// Same requestId, same duration -> 200 with the first result.
	repeat := createTask(t, h, "dev-1", taskBody("req-1", 30), http.StatusOK)
	if repeat.ID != task.ID || !repeat.CreatedAt.Equal(task.CreatedAt) {
		t.Fatalf("repeat returned a different task: %+v vs %+v", repeat, task)
	}

	// Same requestId, different duration -> 409.
	createTask(t, h, "dev-1", taskBody("req-1", 45), http.StatusConflict)

	// Validation.
	createTask(t, h, "dev-1", `{"requestId":"  ","durationSeconds":10}`, http.StatusBadRequest)
	createTask(t, h, "dev-1", `{"requestId":"req-2"}`, http.StatusBadRequest)
	createTask(t, h, "dev-1", taskBody("req-3", 0), http.StatusBadRequest)
	createTask(t, h, "dev-1", taskBody("req-4", 61), http.StatusBadRequest)
	createTask(t, h, "dev-1", `{"requestId":"req-5","durationSeconds":"10"}`, http.StatusBadRequest)
	createTask(t, h, "dev-1", `{"requestId":"req-6","durationSeconds":1.5}`, http.StatusBadRequest)

	// Unknown device.
	createTask(t, h, "missing", taskBody("req-7", 10), http.StatusNotFound)
}

func TestTaskClaimAndReportSuccess(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "dev-1")
	createTask(t, h, "dev-1", taskBody("req-1", 30), http.StatusCreated)

	claim := claimTask(t, h, "dev-1", http.StatusOK)
	if claim.Attempt != 1 || claim.Credential == "" {
		t.Fatalf("unexpected claim: %+v", claim)
	}
	if !claim.Deadline.Equal(clock.UTC().Add(30 * time.Second)) {
		t.Fatalf("deadline = %s, want %s", claim.Deadline, clock.UTC().Add(30*time.Second))
	}
	if claim.Task.Status != "in_progress" || claim.Task.Attempts != 1 {
		t.Fatalf("claim task state: %+v", claim.Task)
	}

	// One task in progress at a time.
	claimTask(t, h, "dev-1", http.StatusNoContent)

	// Report success.
	report := reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, true, "", `{"ok":true,"value":42}`), http.StatusCreated)
	if !report.Success || report.ReceiptID != "rcpt-1" {
		t.Fatalf("unexpected report: %+v", report)
	}

	task := getTask(t, h, "dev-1", claim.Task.ID, http.StatusOK)
	if task.Status != "succeeded" || task.Result == nil || task.CompletedAt == nil {
		t.Fatalf("task after success: %+v", task)
	}

	// Same receiptId, same content -> 200 with the first result.
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, true, "", `{"value":42,"ok":true}`), http.StatusOK)
	// Same receiptId, different content -> 409.
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, true, "", `{"ok":false}`), http.StatusConflict)
	// Same receiptId, numeric value compared by value -> 200.
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, true, "", `{"ok":true,"value":42.0}`), http.StatusOK)
	// Same receiptId, array order is significant -> 409.
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, true, "", `{"ok":true,"value":[1,2]}`), http.StatusConflict)

	// A new receiptId with a stale credential -> 409.
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-2", claim.Credential, true, "", `{"ok":true}`), http.StatusConflict)
}

func TestTaskReportFailureAndBackoff(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "dev-1")
	createTask(t, h, "dev-1", taskBody("req-1", 30), http.StatusCreated)

	// First failure.
	claim := claimTask(t, h, "dev-1", http.StatusOK)
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, false, "disk full", ""), http.StatusCreated)
	task := getTask(t, h, "dev-1", claim.Task.ID, http.StatusOK)
	if task.Status != "waiting" || task.FailureReason != "disk full" || task.Attempts != 1 {
		t.Fatalf("after first failure: %+v", task)
	}
	if !task.NextClaimableAt.Equal(clock.UTC().Add(1 * time.Second)) {
		t.Fatalf("next claimable = %s, want +1s", task.NextClaimableAt)
	}
	claimTask(t, h, "dev-1", http.StatusNoContent)

	// After the backoff, claim again.
	*clock = clock.Add(2 * time.Second)
	claim = claimTask(t, h, "dev-1", http.StatusOK)
	if claim.Attempt != 2 {
		t.Fatalf("attempt = %d, want 2", claim.Attempt)
	}
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-2", claim.Credential, false, "still full", ""), http.StatusCreated)
	task = getTask(t, h, "dev-1", claim.Task.ID, http.StatusOK)
	if task.Status != "waiting" || task.Attempts != 2 {
		t.Fatalf("after second failure: %+v", task)
	}
	if !task.NextClaimableAt.Equal(clock.UTC().Add(2 * time.Second)) {
		t.Fatalf("next claimable = %s, want +2s", task.NextClaimableAt)
	}

	// Third failure ends the task.
	*clock = clock.Add(3 * time.Second)
	claim = claimTask(t, h, "dev-1", http.StatusOK)
	if claim.Attempt != 3 {
		t.Fatalf("attempt = %d, want 3", claim.Attempt)
	}
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-3", claim.Credential, false, "gave up", ""), http.StatusCreated)
	task = getTask(t, h, "dev-1", claim.Task.ID, http.StatusOK)
	if task.Status != "failed" || task.CompletedAt == nil {
		t.Fatalf("after third failure: %+v", task)
	}
	claimTask(t, h, "dev-1", http.StatusNoContent)
}

func TestTaskFailureReportRetry(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "dev-1")
	createTask(t, h, "dev-1", taskBody("req-1", 30), http.StatusCreated)

	claim := claimTask(t, h, "dev-1", http.StatusOK)
	first := reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, false, "disk full", ""), http.StatusCreated)
	if first.Success || first.Reason != "disk full" || first.ReceivedAt.IsZero() {
		t.Fatalf("first failure report: %+v", first)
	}
	waiting := getTask(t, h, "dev-1", claim.Task.ID, http.StatusOK)
	auditBefore := listAudit(t, h, "dev-1", claim.Task.ID)

	// An identical retry is the stored receipt, not a new failure: 200 with the
	// first receipt's content and receive time, and no state movement.
	*clock = clock.Add(30 * time.Minute)
	retry := reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, false, "disk full", ""), http.StatusOK)
	if retry.ReceiptID != first.ReceiptID || retry.Reason != first.Reason ||
		!retry.ReceivedAt.Equal(first.ReceivedAt) {
		t.Fatalf("retry = %+v, want the first receipt %+v", retry, first)
	}
	task := getTask(t, h, "dev-1", claim.Task.ID, http.StatusOK)
	if task.Status != "waiting" || !task.NextClaimableAt.Equal(waiting.NextClaimableAt) {
		t.Fatalf("retry moved the task: %+v, want waiting until %s", task, waiting.NextClaimableAt)
	}
	if audit := listAudit(t, h, "dev-1", claim.Task.ID); len(audit) != len(auditBefore) {
		t.Fatalf("retry appended audit records: %+v", audit)
	}

	// The same receiptId with a changed reason or flipped to success conflicts;
	// the stored receipt is untouched.
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, false, "out of memory", ""), http.StatusConflict)
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, true, "", `{"ok":true}`), http.StatusConflict)
	again := reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, false, "disk full", ""), http.StatusOK)
	if !again.ReceivedAt.Equal(first.ReceivedAt) {
		t.Fatalf("receipt after conflicts = %+v, want receivedAt %s", again, first.ReceivedAt)
	}

	// The task is claimed again (attempt 2, new credential). The old receipt
	// still answers retries without the original credential, and neither the
	// retry nor a conflict disturbs the running attempt.
	claim2 := claimTask(t, h, "dev-1", http.StatusOK)
	if claim2.Attempt != 2 || claim2.Credential == claim.Credential {
		t.Fatalf("second claim: %+v", claim2)
	}
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, false, "disk full", ""), http.StatusOK)
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, false, "changed reason", ""), http.StatusConflict)
	running := getTask(t, h, "dev-1", claim.Task.ID, http.StatusOK)
	if running.Status != "in_progress" || running.Deadline == nil ||
		!running.Deadline.Equal(claim2.Deadline) {
		t.Fatalf("old receipt disturbed the running attempt: %+v", running)
	}

	// A receiptId rejected earlier for a bad credential was never stored, so it
	// is still free for the current attempt.
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-2", "bogus-credential", false, "disk full", ""), http.StatusConflict)
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-2", claim2.Credential, false, "still full", ""), http.StatusCreated)

	// A genuinely new failure on this attempt follows the usual rules.
	task = getTask(t, h, "dev-1", claim.Task.ID, http.StatusOK)
	if task.Status != "waiting" || task.Attempts != 2 ||
		!task.NextClaimableAt.Equal(clock.UTC().Add(2*time.Second)) {
		t.Fatalf("after second failure: %+v", task)
	}

	// Once the task has ended, the original receipt is still replayed.
	*clock = clock.Add(3 * time.Second)
	claim3 := claimTask(t, h, "dev-1", http.StatusOK)
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-3", claim3.Credential, false, "gave up", ""), http.StatusCreated)
	if task := getTask(t, h, "dev-1", claim.Task.ID, http.StatusOK); task.Status != "failed" {
		t.Fatalf("after third failure: %+v", task)
	}
	final := reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, false, "disk full", ""), http.StatusOK)
	if !final.ReceivedAt.Equal(first.ReceivedAt) {
		t.Fatalf("receipt after task end = %+v, want receivedAt %s", final, first.ReceivedAt)
	}
}

func TestTaskTimeout(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "dev-1")
	createTask(t, h, "dev-1", taskBody("req-1", 5), http.StatusCreated)

	claim := claimTask(t, h, "dev-1", http.StatusOK)
	if !claim.Deadline.Equal(clock.UTC().Add(5 * time.Second)) {
		t.Fatalf("deadline = %s", claim.Deadline)
	}

	// Advance past the deadline; query must reflect the timeout.
	*clock = clock.Add(6 * time.Second)
	task := getTask(t, h, "dev-1", claim.Task.ID, http.StatusOK)
	if task.Status != "waiting" || task.FailureReason != "deadline exceeded" {
		t.Fatalf("after timeout: %+v", task)
	}
	if !task.NextClaimableAt.Equal(claim.Deadline.Add(1 * time.Second)) {
		t.Fatalf("next claimable = %s, want deadline+1s", task.NextClaimableAt)
	}

	// A report after the deadline conflicts.
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, true, "", `{"ok":true}`), http.StatusConflict)

	// The timeout released the device; after the backoff it can be claimed again.
	*clock = clock.Add(2 * time.Second)
	claim = claimTask(t, h, "dev-1", http.StatusOK)
	if claim.Attempt != 2 {
		t.Fatalf("attempt = %d, want 2", claim.Attempt)
	}
}

func TestTaskTimeoutThirdAttemptEnds(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "dev-1")
	createTask(t, h, "dev-1", taskBody("req-1", 1), http.StatusCreated)

	var claim claimResponse
	for i := 1; i <= 3; i++ {
		claim = claimTask(t, h, "dev-1", http.StatusOK)
		if claim.Attempt != i {
			t.Fatalf("attempt = %d, want %d", claim.Attempt, i)
		}
		// Advance past this attempt's deadline and its backoff (1s after the
		// first failure, 2s after the second); the third timeout ends the task.
		*clock = clock.Add(time.Duration(2+i) * time.Second)
	}
	task := getTask(t, h, "dev-1", claim.Task.ID, http.StatusOK)
	if task.Status != "failed" || task.FailureReason != "deadline exceeded" {
		t.Fatalf("task should be failed after 3 timeouts: %+v", task)
	}
}

func TestTaskCancel(t *testing.T) {
	store, _ := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "dev-1")

	// Cancel a pending task.
	createTask(t, h, "dev-1", taskBody("req-1", 30), http.StatusCreated)
	task := cancelTask(t, h, "dev-1", 1, http.StatusOK)
	if task.Status != "canceled" || task.CompletedAt == nil {
		t.Fatalf("cancel pending: %+v", task)
	}
	// Duplicate cancel returns the original result.
	cancelTask(t, h, "dev-1", 1, http.StatusOK)

	// Cancel an in-progress task.
	createTask(t, h, "dev-1", taskBody("req-2", 30), http.StatusCreated)
	claim := claimTask(t, h, "dev-1", http.StatusOK)
	task = cancelTask(t, h, "dev-1", claim.Task.ID, http.StatusOK)
	if task.Status != "canceled" {
		t.Fatalf("cancel in progress: %+v", task)
	}

	// Cancel after a success ends conflicts.
	createTask(t, h, "dev-1", taskBody("req-3", 30), http.StatusCreated)
	claim = claimTask(t, h, "dev-1", http.StatusOK)
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, true, "", `{"ok":true}`), http.StatusCreated)
	cancelTask(t, h, "dev-1", claim.Task.ID, http.StatusConflict)

	// Cancel after a failure end conflicts.
	createTask(t, h, "dev-1", taskBody("req-4", 30), http.StatusCreated)
	claim = claimTask(t, h, "dev-1", http.StatusOK)
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-2", claim.Credential, false, "nope", ""), http.StatusCreated)
	cancelTask(t, h, "dev-1", claim.Task.ID, http.StatusOK) // waiting, not ended
}

func TestTaskListAndAudit(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "dev-1")
	createTask(t, h, "dev-1", taskBody("req-1", 30), http.StatusCreated)
	createTask(t, h, "dev-1", taskBody("req-2", 30), http.StatusCreated)

	list := decodeBody[struct {
		Tasks []taskResponse `json:"tasks"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/devices/dev-1/tasks", ""))
	if len(list.Tasks) != 2 || list.Tasks[0].ID != 1 || list.Tasks[1].ID != 2 {
		t.Fatalf("unexpected task list: %+v", list.Tasks)
	}

	// Drive task 1 through claim -> success.
	claim := claimTask(t, h, "dev-1", http.StatusOK)
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, true, "", `{"done":true}`), http.StatusCreated)

	audit := listAudit(t, h, "dev-1", claim.Task.ID)
	events := []string{}
	for _, rec := range audit {
		events = append(events, rec.Event)
	}
	if got := strings.Join(events, ","); got != "created,claimed,succeeded" {
		t.Fatalf("audit events = %s", got)
	}
	if audit[0].ToStatus != "pending" || audit[1].ToStatus != "in_progress" || audit[2].ToStatus != "succeeded" {
		t.Fatalf("audit transitions: %+v", audit)
	}
	if audit[1].Attempt != 1 || audit[2].Attempt != 1 {
		t.Fatalf("audit attempts: %+v", audit)
	}

	// Task 2 is still pending; its audit has only the create record.
	audit2 := listAudit(t, h, "dev-1", 2)
	if len(audit2) != 1 || audit2[0].Event != "created" {
		t.Fatalf("task 2 audit: %+v", audit2)
	}

	// The list reflects the succeeded task and the still-pending task.
	list = decodeBody[struct {
		Tasks []taskResponse `json:"tasks"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/devices/dev-1/tasks", ""))
	if list.Tasks[0].Status != "succeeded" || list.Tasks[1].Status != "pending" {
		t.Fatalf("list after success: %+v", list.Tasks)
	}
	_ = clock
}

func TestTaskClaimOrdering(t *testing.T) {
	store, _ := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "dev-1")
	createTask(t, h, "dev-1", taskBody("req-1", 30), http.StatusCreated)
	createTask(t, h, "dev-1", taskBody("req-2", 30), http.StatusCreated)

	// The earliest-created task is claimed first.
	claim := claimTask(t, h, "dev-1", http.StatusOK)
	if claim.Task.ID != 1 {
		t.Fatalf("claimed task %d, want 1", claim.Task.ID)
	}
	// Task 2 is not claimable while task 1 is in progress.
	claimTask(t, h, "dev-1", http.StatusNoContent)
}

func TestTaskClaimConcurrent(t *testing.T) {
	store, _ := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "dev-1")
	createTask(t, h, "dev-1", taskBody("req-1", 30), http.StatusCreated)

	var wg sync.WaitGroup
	successes := make(chan int, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := doRequest(t, h, http.MethodPost, "/v1/devices/dev-1/tasks/claim", "")
			if r.Code == http.StatusOK {
				successes <- 1
			}
		}()
	}
	wg.Wait()
	close(successes)
	count := 0
	for range successes {
		count++
	}
	if count != 1 {
		t.Fatalf("concurrent claims succeeded %d times, want 1", count)
	}
}

func TestTaskReportCredentialChecks(t *testing.T) {
	store, _ := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "dev-1")
	createTask(t, h, "dev-1", taskBody("req-1", 30), http.StatusCreated)
	createTask(t, h, "dev-1", taskBody("req-2", 30), http.StatusCreated)

	claim := claimTask(t, h, "dev-1", http.StatusOK)

	// Unknown credential.
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-1", "bogus", true, "", `{"ok":true}`), http.StatusConflict)

	// Credential from another task (task 2 is pending, so it is not in progress).
	reportTask(t, h, "dev-1", 2,
		reportBody("rcpt-2", claim.Credential, true, "", `{"ok":true}`), http.StatusConflict)

	// Missing fields.
	reportTask(t, h, "dev-1", claim.Task.ID, `{"credential":"x","success":true,"result":{}}`, http.StatusBadRequest)
	reportTask(t, h, "dev-1", claim.Task.ID,
		`{"receiptId":"rcpt-3","credential":"x","success":true}`, http.StatusBadRequest)
	reportTask(t, h, "dev-1", claim.Task.ID,
		`{"receiptId":"rcpt-4","credential":"x","success":false}`, http.StatusBadRequest)
	reportTask(t, h, "dev-1", claim.Task.ID,
		`{"receiptId":"rcpt-5","credential":"x","success":"yes"}`, http.StatusBadRequest)
}

func TestTaskDoesNotTouchDeviceActivity(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "dev-1")

	// Record telemetry and capture the last-seen time.
	doRequest(t, h, http.MethodPost, "/v1/devices/dev-1/telemetry", `{"temperature":21.0}`)
	before := decodeBody[struct {
		LastSeenAt time.Time `json:"lastSeenAt"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/fleet", "")).LastSeenAt

	// Drive a task through its full lifecycle.
	createTask(t, h, "dev-1", taskBody("req-1", 30), http.StatusCreated)
	claim := claimTask(t, h, "dev-1", http.StatusOK)
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, true, "", `{"ok":true}`), http.StatusCreated)

	after := decodeBody[struct {
		LastSeenAt time.Time `json:"lastSeenAt"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/fleet", "")).LastSeenAt
	if !after.Equal(before) {
		t.Fatalf("device activity time changed: %s -> %s", before, after)
	}
	_ = clock
}

func TestTaskUnknownDeviceAndTask(t *testing.T) {
	store, _ := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "dev-1")

	claimTask(t, h, "missing", http.StatusNotFound)
	getTask(t, h, "dev-1", 99, http.StatusNotFound)
	cancelTask(t, h, "dev-1", 99, http.StatusNotFound)
	reportTask(t, h, "dev-1", 99, reportBody("rcpt-1", "x", true, "", `{"ok":true}`), http.StatusNotFound)
	r := doRequest(t, h, http.MethodGet, "/v1/devices/dev-1/tasks/99/audit", "")
	if r.Code != http.StatusNotFound {
		t.Fatalf("audit unknown task status = %d", r.Code)
	}
}

// Reported failures and deadline timeouts accumulate into one failure count
// with one set of backoff/end rules, while their timestamps, reasons and audit
// events stay distinct.
func TestTaskFailuresAccumulateAcrossSources(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "dev-1")
	createTask(t, h, "dev-1", taskBody("req-1", 1), http.StatusCreated)

	// Failure 1: device-reported; the wait runs from the receipt time.
	claim := claimTask(t, h, "dev-1", http.StatusOK)
	reportTask(t, h, "dev-1", claim.Task.ID,
		reportBody("rcpt-1", claim.Credential, false, "disk full", ""), http.StatusCreated)
	task := getTask(t, h, "dev-1", claim.Task.ID, http.StatusOK)
	if task.Status != "waiting" || task.FailureReason != "disk full" || task.Attempts != 1 {
		t.Fatalf("after first failure: %+v", task)
	}
	if !task.NextClaimableAt.Equal(clock.UTC().Add(1 * time.Second)) {
		t.Fatalf("next claimable = %s, want receipt time +1s", task.NextClaimableAt)
	}
	if got := store.devices["dev-1"].tasks[0].task.Failures; got != 1 {
		t.Fatalf("failures = %d, want 1", got)
	}

	// The waiting task does not block another already-claimable task.
	createTask(t, h, "dev-1", taskBody("req-other", 60), http.StatusCreated)
	other := claimTask(t, h, "dev-1", http.StatusOK)
	if other.Task.ID != 2 {
		t.Fatalf("claim during backoff took task %d, want 2", other.Task.ID)
	}
	reportTask(t, h, "dev-1", other.Task.ID,
		reportBody("rcpt-other", other.Credential, true, "", `{"ok":true}`), http.StatusCreated)

	// Failure 2: a timeout. It takes the 2-second backoff because the failure
	// count is shared, and the wait runs from the deadline rather than the
	// (much later) query time.
	*clock = clock.Add(2 * time.Second)
	claim = claimTask(t, h, "dev-1", http.StatusOK)
	if claim.Attempt != 2 || claim.Task.ID != 1 {
		t.Fatalf("re-claim = %+v, want task 1 attempt 2", claim)
	}
	deadline2 := claim.Deadline
	*clock = clock.Add(4 * time.Second)
	task = getTask(t, h, "dev-1", 1, http.StatusOK)
	if task.Status != "waiting" || task.FailureReason != "deadline exceeded" {
		t.Fatalf("after timeout: %+v", task)
	}
	if !task.NextClaimableAt.Equal(deadline2.Add(2 * time.Second)) {
		t.Fatalf("next claimable = %s, want deadline +2s (shared failure 2)", task.NextClaimableAt)
	}
	if got := store.devices["dev-1"].tasks[0].task.Failures; got != 2 {
		t.Fatalf("failures = %d, want 2", got)
	}
	// The timed-out credential no longer authorizes a receipt.
	reportTask(t, h, "dev-1", 1,
		reportBody("rcpt-late", claim.Credential, true, "", `{"ok":true}`), http.StatusConflict)
	// Repeated queries neither re-time the task nor accumulate another failure.
	getTask(t, h, "dev-1", 1, http.StatusOK)
	if got := store.devices["dev-1"].tasks[0].task.Failures; got != 2 {
		t.Fatalf("repeat query changed failures to %d", got)
	}
	if got := len(store.devices["dev-1"].tasks[0].audit); got != 5 {
		t.Fatalf("audit length after repeat query = %d, want 5", got)
	}

	// Failure 3: a reported failure ends the task, stamped at receipt time.
	claim = claimTask(t, h, "dev-1", http.StatusOK)
	if claim.Attempt != 3 {
		t.Fatalf("attempt = %d, want 3", claim.Attempt)
	}
	reportTask(t, h, "dev-1", 1,
		reportBody("rcpt-3", claim.Credential, false, "gave up", ""), http.StatusCreated)
	task = getTask(t, h, "dev-1", 1, http.StatusOK)
	if task.Status != "failed" || task.CompletedAt == nil || task.Attempts != 3 {
		t.Fatalf("after third failure: %+v", task)
	}
	if !task.CompletedAt.Equal(clock.UTC()) {
		t.Fatalf("completedAt = %s, want receipt time %s", task.CompletedAt, clock.UTC())
	}
	claimTask(t, h, "dev-1", http.StatusNoContent)

	// The audit keeps both sources distinct, dated and ordered.
	audit := listAudit(t, h, "dev-1", 1)
	if got := joinEvents(eventsOf(audit)); got != "created,claimed,failed,claimed,timed_out,claimed,failed" {
		t.Fatalf("audit events = %s", got)
	}
	timedOut := audit[4]
	if timedOut.Event != "timed_out" || timedOut.At != deadline2 || timedOut.Reason != "deadline exceeded" ||
		timedOut.Attempt != 2 || timedOut.FromStatus != "in_progress" || timedOut.ToStatus != "waiting" {
		t.Fatalf("timeout audit record = %+v", timedOut)
	}
	if audit[2].At.IsZero() || audit[6].ToStatus != "failed" {
		t.Fatalf("reported failure audit records = %+v / %+v", audit[2], audit[6])
	}
}

// An attempt is live exactly up to and including its deadline; only a strictly
// later instant is a timeout.
func TestTaskDeadlineBoundary(t *testing.T) {
	store, _ := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "dev-1")
	createTask(t, h, "dev-1", taskBody("req-1", 5), http.StatusCreated)
	claim := claimTask(t, h, "dev-1", http.StatusOK)
	deadline := claim.Deadline

	// Exactly at the deadline the attempt is still in progress...
	store.now = func() time.Time { return deadline }
	if task := getTask(t, h, "dev-1", 1, http.StatusOK); task.Status != "in_progress" {
		t.Fatalf("at the deadline status = %s, want in_progress", task.Status)
	}
	// ...and a report presented exactly then is accepted with the credential.
	reportTask(t, h, "dev-1", 1,
		reportBody("rcpt-1", claim.Credential, true, "", `{"ok":true}`), http.StatusCreated)

	// A strictly later instant expires a fresh attempt.
	createTask(t, h, "dev-1", taskBody("req-2", 5), http.StatusCreated)
	claim2 := claimTask(t, h, "dev-1", http.StatusOK)
	store.now = func() time.Time { return claim2.Deadline.Add(time.Second) }
	task := getTask(t, h, "dev-1", claim2.Task.ID, http.StatusOK)
	if task.Status != "waiting" || !task.NextClaimableAt.Equal(claim2.Deadline.Add(1*time.Second)) {
		t.Fatalf("past the deadline: %+v", task)
	}
}
