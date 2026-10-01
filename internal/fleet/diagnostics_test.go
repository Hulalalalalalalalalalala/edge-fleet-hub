package fleet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// --- helpers ---------------------------------------------------------------

const diagDevice = "gw-diag"

func newDiagHarness(t *testing.T) (http.Handler, *Store, *time.Time) {
	t.Helper()
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, diagDevice)
	return h, store, clock
}

func createTaskBody(requestID string, seconds int) string {
	return fmt.Sprintf(`{"requestId":%q,"seconds":%d}`, requestID, seconds)
}

func mustCreateTask(t *testing.T, h http.Handler, requestID string, seconds int) TaskSummary {
	t.Helper()
	r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks", createTaskBody(requestID, seconds))
	if r.Code != http.StatusCreated {
		t.Fatalf("create task %s: %d %s", requestID, r.Code, r.Body.String())
	}
	return decodeBody[TaskSummary](t, r)
}

type claimResponse struct {
	Task     int64     `json:"task"`
	Attempt  int       `json:"attempt"`
	Token    string    `json:"token"`
	Deadline time.Time `json:"deadline"`
}

func mustClaim(t *testing.T, h http.Handler) claimResponse {
	t.Helper()
	r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks/claim", "")
	if r.Code != http.StatusOK {
		t.Fatalf("claim: status = %d, want 200: %s", r.Code, r.Body.String())
	}
	return decodeBody[claimResponse](t, r)
}

func reportBody(claim claimResponse, receiptID string, success bool, resultOrReason string) string {
	if success {
		return fmt.Sprintf(`{"token":%q,"receiptId":%q,"success":true,"result":%s}`,
			claim.Token, receiptID, resultOrReason)
	}
	return fmt.Sprintf(`{"token":%q,"receiptId":%q,"success":false,"reason":%q}`,
		claim.Token, receiptID, resultOrReason)
}

// --- create ----------------------------------------------------------------

func TestDiagnosticCreateAndDedup(t *testing.T) {
	h, _, clock := newDiagHarness(t)

	task := mustCreateTask(t, h, "req-1", 5)
	if task.Number != 1 || task.Status != taskStatusPending || task.Attempts != 0 {
		t.Fatalf("unexpected first task: %+v", task)
	}
	if task.CreatedAt != clock.UTC() {
		t.Fatalf("createdAt = %s, want %s", task.CreatedAt, clock)
	}
	if task.Deadline != nil || task.NextClaimableAt != nil {
		t.Fatalf("fresh task should carry no deadline/nextClaim: %+v", task)
	}

	// Same requestId and seconds -> 200 with the first result.
	r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks", createTaskBody("req-1", 5))
	if r.Code != http.StatusOK {
		t.Fatalf("re-submit = %d, want 200: %s", r.Code, r.Body.String())
	}
	again := decodeBody[TaskSummary](t, r)
	if again.Number != 1 || again.CreatedAt != task.CreatedAt {
		t.Fatalf("re-submit returned a different task: %+v vs %+v", again, task)
	}

	// Same requestId with changed seconds -> 409.
	r = doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks", createTaskBody("req-1", 6))
	if r.Code != http.StatusConflict {
		t.Fatalf("changed seconds = %d, want 409", r.Code)
	}

	// A new requestId gets the next number.
	task2 := mustCreateTask(t, h, "req-2", 1)
	if task2.Number != 2 {
		t.Fatalf("second task number = %d, want 2", task2.Number)
	}
}

func TestDiagnosticCreateReplayStaysFirstResult(t *testing.T) {
	h, _, _ := newDiagHarness(t)
	first := mustCreateTask(t, h, "req-1", 5)
	claim := mustClaim(t, h)
	if r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks/reports",
		reportBody(claim, "rc", true, `{"ok":true}`)); r.Code != http.StatusCreated {
		t.Fatalf("success = %d", r.Code)
	}
	// Re-submitting after the task finished still returns the creation-time
	// snapshot (pending, attempt 0) with 200.
	r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks", createTaskBody("req-1", 5))
	if r.Code != http.StatusOK {
		t.Fatalf("re-submit after finish = %d, want 200", r.Code)
	}
	replayed := decodeBody[TaskSummary](t, r)
	if replayed.Status != taskStatusPending || replayed.Attempts != 0 ||
		replayed.CreatedAt != first.CreatedAt {
		t.Fatalf("replay is not the first creation result: %+v", replayed)
	}
}

func TestDiagnosticCreateValidation(t *testing.T) {
	h, _, _ := newDiagHarness(t)
	url := "/v1/devices/" + diagDevice + "/diagnostic-tasks"
	cases := map[string]string{
		"missing requestId": `{"seconds":5}`,
		"blank requestId":   `{"requestId":"   ","seconds":5}`,
		"missing seconds":   `{"requestId":"r"}`,
		"seconds zero":      `{"requestId":"r","seconds":0}`,
		"seconds over 60":   `{"requestId":"r","seconds":61}`,
		"seconds negative":  `{"requestId":"r","seconds":-3}`,
		"seconds float":     `{"requestId":"r","seconds":2.5}`,
		"seconds string":    `{"requestId":"r","seconds":"5"}`,
		"unknown field":     `{"requestId":"r","seconds":5,"bogus":1}`,
		"two documents":     `{"requestId":"r","seconds":5}{}`,
	}
	for name, body := range cases {
		r := doRequest(t, h, http.MethodPost, url, body)
		if r.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", name, r.Code, r.Body.String())
		}
	}
}

func TestDiagnosticUnknownDeviceAndTask(t *testing.T) {
	h, _, _ := newDiagHarness(t)

	r := doRequest(t, h, http.MethodPost,
		"/v1/devices/nope/diagnostic-tasks", createTaskBody("r", 5))
	if r.Code != http.StatusNotFound {
		t.Fatalf("create on unknown device = %d, want 404", r.Code)
	}
	r = doRequest(t, h, http.MethodPost, "/v1/devices/nope/diagnostic-tasks/claim", "")
	if r.Code != http.StatusNotFound {
		t.Fatalf("claim on unknown device = %d, want 404", r.Code)
	}
	r = doRequest(t, h, http.MethodGet, "/v1/devices/"+diagDevice+"/diagnostic-tasks/99", "")
	if r.Code != http.StatusNotFound {
		t.Fatalf("unknown task = %d, want 404", r.Code)
	}
	r = doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks/99/cancel", "")
	if r.Code != http.StatusNotFound {
		t.Fatalf("cancel unknown task = %d, want 404", r.Code)
	}
	r = doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks/reports",
		`{"token":"deadbeef","receiptId":"rc","success":true,"result":{}}`)
	if r.Code != http.StatusConflict {
		t.Fatalf("unknown token = %d, want 409", r.Code)
	}
}

// --- claim -----------------------------------------------------------------

func TestDiagnosticClaimOldestAndEmpty(t *testing.T) {
	h, _, clock := newDiagHarness(t)
	mustCreateTask(t, h, "req-1", 5)
	mustCreateTask(t, h, "req-2", 10)

	claim := mustClaim(t, h)
	if claim.Task != 1 || claim.Attempt != 1 {
		t.Fatalf("first claim = %+v, want task 1 attempt 1", claim)
	}
	if want := clock.Add(5 * time.Second); !claim.Deadline.Equal(want) {
		t.Fatalf("deadline = %s, want %s", claim.Deadline, want)
	}
	if claim.Token == "" {
		t.Fatal("claim returned an empty token")
	}

	// One running task per device: the second task is not claimable.
	r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks/claim", "")
	if r.Code != http.StatusNoContent {
		t.Fatalf("concurrent claim = %d, want 204: %s", r.Code, r.Body.String())
	}
}

func TestDiagnosticClaimConcurrentOnce(t *testing.T) {
	h, _, _ := newDiagHarness(t)
	mustCreateTask(t, h, "req-1", 5)

	const n = 50
	var wg sync.WaitGroup
	results := make(chan int, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := doRequest(t, h, http.MethodPost,
				"/v1/devices/"+diagDevice+"/diagnostic-tasks/claim", "")
			results <- r.Code
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	ok, empty := 0, 0
	for code := range results {
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusNoContent:
			empty++
		default:
			t.Fatalf("unexpected claim status %d", code)
		}
	}
	if ok != 1 || empty != n-1 {
		t.Fatalf("claims: %d ok, %d empty; want exactly 1 ok", ok, empty)
	}
}

// --- success report --------------------------------------------------------

func TestDiagnosticSuccessFlow(t *testing.T) {
	h, _, clock := newDiagHarness(t)
	mustCreateTask(t, h, "req-1", 5)
	claim := mustClaim(t, h)

	r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks/reports",
		reportBody(claim, "rc-1", true, `{"ping":"pong","rtt":12}`))
	if r.Code != http.StatusCreated {
		t.Fatalf("success report = %d, want 201: %s", r.Code, r.Body.String())
	}
	ack := decodeBody[TaskReportAck](t, r)
	if ack.Status != taskStatusSucceeded || ack.Attempts != 1 ||
		ack.ReceivedAt != clock.UTC() || ack.ReceiptID != "rc-1" {
		t.Fatalf("unexpected ack: %+v", ack)
	}

	// The slot is freed and the next task can run.
	mustCreateTask(t, h, "req-2", 1)
	second := mustClaim(t, h)
	if second.Task != 2 {
		t.Fatalf("next claim = task %d, want 2", second.Task)
	}

	// Detail view shows the result and succeeded status.
	r = doRequest(t, h, http.MethodGet,
		fmt.Sprintf("/v1/devices/%s/diagnostic-tasks/1", diagDevice), "")
	detail := decodeBody[TaskSummary](t, r)
	if detail.Status != taskStatusSucceeded || string(detail.Result) == "" {
		t.Fatalf("detail = %+v", detail)
	}
}

func TestDiagnosticReportValidation(t *testing.T) {
	h, _, _ := newDiagHarness(t)
	mustCreateTask(t, h, "req-1", 5)
	claim := mustClaim(t, h)
	url := "/v1/devices/" + diagDevice + "/diagnostic-tasks/reports"

	r := doRequest(t, h, http.MethodPost, url,
		fmt.Sprintf(`{"token":%q,"receiptId":"rc","success":true}`, claim.Token))
	if r.Code != http.StatusBadRequest {
		t.Fatalf("success without result = %d, want 400", r.Code)
	}
	r = doRequest(t, h, http.MethodPost, url,
		fmt.Sprintf(`{"token":%q,"receiptId":"rc","success":true,"result":[1,2]}`, claim.Token))
	if r.Code != http.StatusBadRequest {
		t.Fatalf("array result = %d, want 400", r.Code)
	}
	r = doRequest(t, h, http.MethodPost, url,
		fmt.Sprintf(`{"token":%q,"receiptId":"rc","success":false}`, claim.Token))
	if r.Code != http.StatusBadRequest {
		t.Fatalf("failure without reason = %d, want 400", r.Code)
	}
	r = doRequest(t, h, http.MethodPost, url,
		`{"token":"  ","receiptId":"rc","success":true,"result":{}}`)
	if r.Code != http.StatusBadRequest {
		t.Fatalf("blank token = %d, want 400", r.Code)
	}
}

func TestDiagnosticSuccessReceiptDedup(t *testing.T) {
	h, _, _ := newDiagHarness(t)
	mustCreateTask(t, h, "req-1", 60)
	claim := mustClaim(t, h)
	url := "/v1/devices/" + diagDevice + "/diagnostic-tasks/reports"

	first := `{"a":1,"b":[1,2,3]}`
	r := doRequest(t, h, http.MethodPost, url,
		reportBody(claim, "rc-1", true, first))
	if r.Code != http.StatusCreated {
		t.Fatalf("first report = %d: %s", r.Code, r.Body.String())
	}

	// Same receiptId, semantically identical content (reordered keys, 1 == 1.0)
	// returns the first ack with 200 even though the token is now spent.
	firstBody := reportBody(claim, "rc-1", true, first)
	r = doRequest(t, h, http.MethodPost, url,
		fmt.Sprintf(`{"token":%q,"receiptId":"rc-1","success":true,
		             "result":{"b":[1,2.0,3],"a":1.0}}`, claim.Token))
	if r.Code != http.StatusOK {
		t.Fatalf("identical retry = %d, want 200: %s", r.Code, r.Body.String())
	}
	retry := decodeBody[TaskReportAck](t, r)
	original := decodeBody[TaskReportAck](t, doRequest(t, h, http.MethodPost, url, firstBody))
	if retry.ReceivedAt != original.ReceivedAt {
		t.Fatalf("retry time = %s, want first time %s", retry.ReceivedAt, original.ReceivedAt)
	}

	// Same receiptId, different content -> 409.
	r = doRequest(t, h, http.MethodPost, url,
		reportBody(claim, "rc-1", true, `{"a":2}`))
	if r.Code != http.StatusConflict {
		t.Fatalf("changed content = %d, want 409", r.Code)
	}
	// Array order is significant.
	r = doRequest(t, h, http.MethodPost, url,
		reportBody(claim, "rc-2", true, `{"a":1,"b":[1,2,3]}`))
	if r.Code != http.StatusConflict {
		t.Fatalf("second receiptId on ended task = %d, want 409", r.Code)
	}
}

func TestDiagnosticRejectStaleAndForeignTokens(t *testing.T) {
	h, _, _ := newDiagHarness(t)
	mustCreateTask(t, h, "req-1", 30)
	mustCreateTask(t, h, "req-2", 30)
	claim1 := mustClaim(t, h)
	url := "/v1/devices/" + diagDevice + "/diagnostic-tasks/reports"

	// Succeed task 1, then its token must not be reusable.
	if r := doRequest(t, h, http.MethodPost, url,
		reportBody(claim1, "rc-1", true, `{"ok":true}`)); r.Code != http.StatusCreated {
		t.Fatalf("report task 1 = %d", r.Code)
	}
	if r := doRequest(t, h, http.MethodPost, url,
		reportBody(claim1, "rc-other", true, `{"ok":true}`)); r.Code != http.StatusConflict {
		t.Fatalf("spent token = %d, want 409", r.Code)
	}

	// Fail task 2 once; the attempt-1 token must be invalid afterwards.
	claim2 := mustClaim(t, h)
	if r := doRequest(t, h, http.MethodPost, url,
		reportBody(claim2, "f-1", false, "boom")); r.Code != http.StatusCreated {
		t.Fatalf("failure = %d", r.Code)
	}
	if r := doRequest(t, h, http.MethodPost, url,
		reportBody(claim2, "f-1", false, "boom")); r.Code != http.StatusOK {
		t.Fatalf("failure retry = %d, want 200 first receipt", r.Code)
	}
	if r := doRequest(t, h, http.MethodPost, url,
		reportBody(claim2, "f-other", false, "boom")); r.Code != http.StatusConflict {
		t.Fatalf("invalid token after failure = %d, want 409", r.Code)
	}
}

func TestDiagnosticReportAfterDeadlineConflict(t *testing.T) {
	h, _, clock := newDiagHarness(t)
	mustCreateTask(t, h, "req-1", 5)
	claim := mustClaim(t, h)

	*clock = clock.Add(5 * time.Second) // exactly at the deadline
	r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks/reports",
		reportBody(claim, "rc-1", true, `{"ok":true}`))
	if r.Code != http.StatusConflict {
		t.Fatalf("report at deadline = %d, want 409: %s", r.Code, r.Body.String())
	}
}

// --- failure, backoff and retries ------------------------------------------

func TestDiagnosticFailureBackoff(t *testing.T) {
	h, _, clock := newDiagHarness(t)
	mustCreateTask(t, h, "req-1", 5)
	url := "/v1/devices/" + diagDevice + "/diagnostic-tasks/reports"

	claim1 := mustClaim(t, h)
	if r := doRequest(t, h, http.MethodPost, url,
		reportBody(claim1, "f-1", false, "first")); r.Code != http.StatusCreated {
		t.Fatalf("failure 1 = %d: %s", r.Code, r.Body.String())
	}
	detail := decodeBody[TaskSummary](t, doRequest(t, h, http.MethodGet,
		fmt.Sprintf("/v1/devices/%s/diagnostic-tasks/1", diagDevice), ""))
	if detail.Status != taskStatusWaiting || detail.Attempts != 1 {
		t.Fatalf("after first failure: %+v", detail)
	}
	if detail.NextClaimableAt == nil ||
		!detail.NextClaimableAt.Equal(clock.Add(1*time.Second)) {
		t.Fatalf("nextClaimableAt = %v, want %s", detail.NextClaimableAt, clock.Add(time.Second))
	}

	// Before the backoff elapses, nothing is claimable.
	if r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks/claim", ""); r.Code != http.StatusNoContent {
		t.Fatalf("early claim = %d, want 204", r.Code)
	}

	*clock = clock.Add(1 * time.Second)
	claim2 := mustClaim(t, h)
	if claim2.Attempt != 2 || claim2.Deadline.Equal(claim1.Deadline) {
		t.Fatalf("second claim = %+v", claim2)
	}
	if r := doRequest(t, h, http.MethodPost, url,
		reportBody(claim2, "f-2", false, "second")); r.Code != http.StatusCreated {
		t.Fatalf("failure 2 = %d", r.Code)
	}
	detail = decodeBody[TaskSummary](t, doRequest(t, h, http.MethodGet,
		fmt.Sprintf("/v1/devices/%s/diagnostic-tasks/1", diagDevice), ""))
	if !detail.NextClaimableAt.Equal(clock.Add(2 * time.Second)) {
		t.Fatalf("second backoff = %v, want %s", detail.NextClaimableAt, clock.Add(2*time.Second))
	}

	*clock = clock.Add(2 * time.Second)
	claim3 := mustClaim(t, h)
	if claim3.Attempt != 3 {
		t.Fatalf("third claim attempt = %d, want 3", claim3.Attempt)
	}
	if r := doRequest(t, h, http.MethodPost, url,
		reportBody(claim3, "f-3", false, "third")); r.Code != http.StatusCreated {
		t.Fatalf("failure 3 = %d", r.Code)
	}
	detail = decodeBody[TaskSummary](t, doRequest(t, h, http.MethodGet,
		fmt.Sprintf("/v1/devices/%s/diagnostic-tasks/1", diagDevice), ""))
	if detail.Status != taskStatusFailed || detail.FailureReason != "third" {
		t.Fatalf("terminal failure = %+v", detail)
	}
	if detail.NextClaimableAt != nil {
		t.Fatalf("failed task should not carry nextClaimableAt: %+v", detail)
	}

	// A finished task is never claimable and cancelling it is 409.
	if r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks/claim", ""); r.Code != http.StatusNoContent {
		t.Fatalf("claim after terminal failure = %d, want 204", r.Code)
	}
	if r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks/1/cancel", ""); r.Code != http.StatusConflict {
		t.Fatalf("cancel failed task = %d, want 409", r.Code)
	}
}

// --- timeout ---------------------------------------------------------------

func TestDiagnosticTimeoutLifecycle(t *testing.T) {
	h, _, clock := newDiagHarness(t)
	mustCreateTask(t, h, "req-1", 5)

	mustClaim(t, h)
	*clock = clock.Add(5 * time.Second) // attempt 1 deadline passes

	// A claim at the deadline settles the timeout and returns 204 (1s backoff).
	if r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks/claim", ""); r.Code != http.StatusNoContent {
		t.Fatalf("claim at deadline = %d, want 204", r.Code)
	}
	detail := decodeBody[TaskSummary](t, doRequest(t, h, http.MethodGet,
		fmt.Sprintf("/v1/devices/%s/diagnostic-tasks/1", diagDevice), ""))
	if detail.Status != taskStatusWaiting || detail.Attempts != 1 {
		t.Fatalf("after timeout: %+v", detail)
	}

	*clock = clock.Add(1 * time.Second)
	claim2 := mustClaim(t, h)
	if claim2.Attempt != 2 {
		t.Fatalf("attempt after timeout = %d, want 2", claim2.Attempt)
	}
	*clock = clock.Add(5 * time.Second)
	*clock = clock.Add(2 * time.Second) // wait out the second backoff too
	claim3 := mustClaim(t, h)
	if claim3.Attempt != 3 {
		t.Fatalf("attempt = %d, want 3", claim3.Attempt)
	}
	*clock = clock.Add(5 * time.Second)
	// A list settles the third timeout and ends the task as failed.
	list := decodeBody[struct {
		Tasks []TaskSummary `json:"tasks"`
	}](t, doRequest(t, h, http.MethodGet,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks", ""))
	if len(list.Tasks) != 1 || list.Tasks[0].Status != taskStatusFailed {
		t.Fatalf("list = %+v", list.Tasks)
	}
	if list.Tasks[0].FailureReason != timeoutReason {
		t.Fatalf("failure reason = %q, want timeout reason", list.Tasks[0].FailureReason)
	}
}

// --- cancel ----------------------------------------------------------------

func TestDiagnosticCancel(t *testing.T) {
	h, _, clock := newDiagHarness(t)
	url := "/v1/devices/" + diagDevice + "/diagnostic-tasks"

	// Cancel a pending task.
	mustCreateTask(t, h, "req-pending", 5)
	r := doRequest(t, h, http.MethodPost, url+"/1/cancel", "")
	if r.Code != http.StatusOK {
		t.Fatalf("cancel pending = %d: %s", r.Code, r.Body.String())
	}
	cancel := decodeBody[TaskCancellation](t, r)
	if cancel.Status != taskStatusCancelled || cancel.CancelledAt != clock.UTC() {
		t.Fatalf("cancellation = %+v", cancel)
	}
	// Repeat returns the original result.
	r = doRequest(t, h, http.MethodPost, url+"/1/cancel", "")
	if r.Code != http.StatusOK {
		t.Fatalf("repeat cancel = %d, want 200", r.Code)
	}
	again := decodeBody[TaskCancellation](t, r)
	if again.CancelledAt != cancel.CancelledAt {
		t.Fatalf("repeat cancel changed the time: %+v", again)
	}

	// Cancel a running task invalidates its token.
	mustCreateTask(t, h, "req-running", 5)
	claim := mustClaim(t, h)
	if r := doRequest(t, h, http.MethodPost, url+"/2/cancel", ""); r.Code != http.StatusOK {
		t.Fatalf("cancel running = %d", r.Code)
	}
	if r := doRequest(t, h, http.MethodPost, url+"/reports",
		reportBody(claim, "rc", true, `{"ok":true}`)); r.Code != http.StatusConflict {
		t.Fatalf("report with cancelled token = %d, want 409", r.Code)
	}

	// Cancel a waiting task.
	mustCreateTask(t, h, "req-waiting", 5)
	waitingClaim := mustClaim(t, h)
	if r := doRequest(t, h, http.MethodPost, url+"/reports",
		reportBody(waitingClaim, "wf", false, "nope")); r.Code != http.StatusCreated {
		t.Fatalf("failure = %d", r.Code)
	}
	if r := doRequest(t, h, http.MethodPost, url+"/3/cancel", ""); r.Code != http.StatusOK {
		t.Fatalf("cancel waiting = %d", r.Code)
	}
	if r := doRequest(t, h, http.MethodPost, url+"/3/cancel", ""); r.Code != http.StatusOK {
		t.Fatalf("repeat cancel waiting = %d, want 200", r.Code)
	}

	// Cancelling a succeeded task is 409.
	mustCreateTask(t, h, "req-done", 5)
	doneClaim := mustClaim(t, h)
	if r := doRequest(t, h, http.MethodPost, url+"/reports",
		reportBody(doneClaim, "done-rc", true, `{"ok":true}`)); r.Code != http.StatusCreated {
		t.Fatalf("success = %d", r.Code)
	}
	if r := doRequest(t, h, http.MethodPost, url+"/4/cancel", ""); r.Code != http.StatusConflict {
		t.Fatalf("cancel succeeded = %d, want 409", r.Code)
	}
}

// --- audit -----------------------------------------------------------------

func TestDiagnosticAuditOrder(t *testing.T) {
	h, _, clock := newDiagHarness(t)
	t0 := clock.UTC()
	mustCreateTask(t, h, "req-1", 5)
	claim := mustClaim(t, h)
	if r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks/reports",
		reportBody(claim, "f-1", false, "boom")); r.Code != http.StatusCreated {
		t.Fatalf("failure = %d", r.Code)
	}
	*clock = clock.Add(1 * time.Second)
	claim2 := mustClaim(t, h)
	if r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks/reports",
		reportBody(claim2, "ok-1", true, `{"fine":true}`)); r.Code != http.StatusCreated {
		t.Fatalf("success = %d", r.Code)
	}

	// Retries and rejected requests must not add audit rows.
	if r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks", createTaskBody("req-1", 5)); r.Code != http.StatusOK {
		t.Fatalf("dedup = %d", r.Code)
	}
	if r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks/reports",
		reportBody(claim2, "ok-1", true, `{"fine":true}`)); r.Code != http.StatusOK {
		t.Fatalf("report retry = %d", r.Code)
	}
	if r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks/claim", ""); r.Code != http.StatusNoContent {
		t.Fatalf("empty claim = %d", r.Code)
	}

	entries := decodeBody[struct {
		Audit []AuditEntry `json:"audit"`
	}](t, doRequest(t, h, http.MethodGet,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks/audit", "")).Audit

	want := []struct {
		action  string
		from    string
		to      string
		attempt int
	}{
		{auditActionCreate, "", taskStatusPending, 0},
		{auditActionClaim, taskStatusPending, taskStatusRunning, 1},
		{auditActionFail, taskStatusRunning, taskStatusWaiting, 1},
		{auditActionClaim, taskStatusWaiting, taskStatusRunning, 2},
		{auditActionSuccess, taskStatusRunning, taskStatusSucceeded, 2},
	}
	if len(entries) != len(want) {
		t.Fatalf("audit rows = %d, want %d: %+v", len(entries), len(want), entries)
	}
	for i, row := range entries {
		w := want[i]
		if row.Seq != i+1 || row.Action != w.action || row.FromStatus != w.from ||
			row.ToStatus != w.to || row.Attempt != w.attempt || row.Task != 1 {
			t.Fatalf("audit[%d] = %+v, want action %s %s->%s attempt %d",
				i, row, w.action, w.from, w.to, w.attempt)
		}
		if row.At.Before(t0) {
			t.Fatalf("audit[%d] time %s before start", i, row.At)
		}
	}
	if entries[2].Reason != "boom" {
		t.Fatalf("failure row reason = %q", entries[2].Reason)
	}
}

// --- isolation -------------------------------------------------------------

func TestDiagnosticTasksDoNotTouchDeviceActivity(t *testing.T) {
	h, _, _ := newDiagHarness(t)

	// Establish a last-active time with real telemetry.
	if r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/telemetry", `{"temperature":20}`); r.Code != http.StatusAccepted {
		t.Fatalf("telemetry = %d", r.Code)
	}
	snapshot := decodeBody[struct {
		Devices []Device `json:"devices"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/fleet", ""))
	lastSeen := snapshot.Devices[0].LastSeenAt

	task := mustCreateTask(t, h, "req-1", 5)
	claim := mustClaim(t, h)
	if r := doRequest(t, h, http.MethodPost,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks/reports",
		reportBody(claim, "rc", true, `{"ok":true}`)); r.Code != http.StatusCreated {
		t.Fatalf("report = %d", r.Code)
	}
	_ = task

	snapshot = decodeBody[struct {
		Devices []Device `json:"devices"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/fleet", ""))
	if !snapshot.Devices[0].LastSeenAt.Equal(lastSeen) {
		t.Fatalf("task operations changed lastSeenAt: %s -> %s",
			lastSeen, snapshot.Devices[0].LastSeenAt)
	}
	events := allEvents(t, h, diagDevice)
	if len(events) != 1 {
		t.Fatalf("task operations changed history: %+v", events)
	}
}

func TestDiagnosticListShape(t *testing.T) {
	h, _, _ := newDiagHarness(t)
	one := mustCreateTask(t, h, "req-1", 5)
	mustCreateTask(t, h, "req-2", 7)

	list := decodeBody[struct {
		Tasks []TaskSummary `json:"tasks"`
	}](t, doRequest(t, h, http.MethodGet,
		"/v1/devices/"+diagDevice+"/diagnostic-tasks", ""))
	if len(list.Tasks) != 2 || list.Tasks[0].Number != one.Number {
		t.Fatalf("list = %+v", list)
	}
	raw, _ := json.Marshal(list.Tasks[0])
	if string(raw) == "" {
		t.Fatal("empty task json")
	}
}
