package fleet

// Remote diagnostics let a fleet operator open a task for a registered device
// and let a simulated device claim it, run it for a bounded number of seconds,
// and report a success or failure outcome. Tasks are per-device: requestId and
// receiptId are deduplicated within the device, and only one task may be
// executing at a time.
//
// A task moves through pending -> in_progress -> waiting -> ... states. A
// failure or a deadline expiry invalidates the current credential and releases
// the device; after a short backoff the task becomes claimable again. The third
// failure ends the task. Every transition that changes the lifecycle is written
// to the per-task audit trail in occurrence order; retries and rejections are
// not.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

const (
	taskStatusPending    = "pending"
	taskStatusInProgress = "in_progress"
	taskStatusWaiting    = "waiting"
	taskStatusSucceeded  = "succeeded"
	taskStatusFailed     = "failed"
	taskStatusCanceled   = "canceled"
)

const (
	auditEventCreated   = "created"
	auditEventClaimed   = "claimed"
	auditEventFailed    = "failed"
	auditEventTimedOut  = "timed_out"
	auditEventSucceeded = "succeeded"
	auditEventCanceled  = "canceled"
)

const (
	taskMinDuration = 1
	taskMaxDuration = 60
	taskMaxAttempts = 3
	// taskTimeoutReason is the failure reason recorded when an attempt runs
	// past its deadline. Device-reported failures keep the submitted reason.
	taskTimeoutReason = "deadline exceeded"
)

var (
	// ErrTaskNotFound is reported for a task id the device has never seen.
	ErrTaskNotFound = errors.New("diagnostic task not found")
	// ErrTaskConflict covers a requestId reused with a different duration, a
	// receiptId reused with different content, a report presented with a stale
	// or foreign credential (or after its deadline), and a cancel attempted
	// after a success or failure end.
	ErrTaskConflict = errors.New("diagnostic task conflict")
)

// DiagnosticTask is the stored lifecycle of one diagnostic task.
type DiagnosticTask struct {
	ID              int64           `json:"id"`
	RequestID       string          `json:"requestId"`
	DurationSeconds int             `json:"durationSeconds"`
	Status          string          `json:"status"`
	Attempts        int             `json:"attempts"`
	Failures        int             `json:"failures"`
	CreatedAt       time.Time       `json:"createdAt"`
	NextClaimableAt time.Time       `json:"nextClaimableAt"`
	ClaimedAt       *time.Time      `json:"claimedAt,omitempty"`
	Deadline        *time.Time      `json:"deadline,omitempty"`
	Credential      string          `json:"-"`
	Result          json.RawMessage `json:"result,omitempty"`
	FailureReason   string          `json:"failureReason,omitempty"`
	CompletedAt     *time.Time      `json:"completedAt,omitempty"`
}

// TaskView is the task shape exposed over HTTP. The credential is never
// included; it is returned only by the claim call.
type TaskView struct {
	ID              int64           `json:"id"`
	RequestID       string          `json:"requestId"`
	DurationSeconds int             `json:"durationSeconds"`
	Status          string          `json:"status"`
	Attempts        int             `json:"attempts"`
	CreatedAt       time.Time       `json:"createdAt"`
	NextClaimableAt *time.Time      `json:"nextClaimableAt,omitempty"`
	ClaimedAt       *time.Time      `json:"claimedAt,omitempty"`
	Deadline        *time.Time      `json:"deadline,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	FailureReason   string          `json:"failureReason,omitempty"`
	CompletedAt     *time.Time      `json:"completedAt,omitempty"`
}

// ClaimView is returned to the simulated device on a successful claim.
type ClaimView struct {
	Task       TaskView  `json:"task"`
	Attempt    int       `json:"attempt"`
	Credential string    `json:"credential"`
	Deadline   time.Time `json:"deadline"`
}

// TaskReport is one device-reported outcome, retained for idempotent retries.
type TaskReport struct {
	ReceiptID  string          `json:"receiptId"`
	Success    bool            `json:"success"`
	Reason     string          `json:"reason,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	ReceivedAt time.Time       `json:"receivedAt"`
}

// TaskAuditRecord is one lifecycle transition in occurrence order.
type TaskAuditRecord struct {
	Seq        int64     `json:"seq"`
	Event      string    `json:"event"`
	FromStatus string    `json:"fromStatus,omitempty"`
	ToStatus   string    `json:"toStatus"`
	Attempt    int       `json:"attempt,omitempty"`
	At         time.Time `json:"at"`
	Reason     string    `json:"reason,omitempty"`
}

// storedReport remembers a committed report so the same receiptId can be
// recognised as an idempotent retry or a conflicting reuse.
type storedReport struct {
	receiptID  string
	success    bool
	reason     string
	result     json.RawMessage
	receivedAt time.Time
}

type taskState struct {
	task    DiagnosticTask
	reports map[string]storedReport // receiptId -> first report
	audit   []TaskAuditRecord
}

// ensureTasks lazily attaches task state to a device; callers must hold the
// store lock (write mode when allocation is wanted).
func (state *deviceState) ensureTasks() {
	if state.tasks == nil {
		state.tasks = make([]*taskState, 0)
	}
	if state.tasksByRequest == nil {
		state.tasksByRequest = make(map[string]*taskState)
	}
}

func findTask(state *deviceState, taskID int64) *taskState {
	for _, ts := range state.tasks {
		if ts.task.ID == taskID {
			return ts
		}
	}
	return nil
}

// CreateTask opens a new task in pending state. A requestId already committed
// on this device is an idempotent retry when the duration matches (the first
// result is returned with repeat=true) and a conflict otherwise.
func (s *Store) CreateTask(id, requestID string, durationSeconds int) (TaskView, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[id]
	if !ok {
		return TaskView{}, false, ErrDeviceNotFound
	}
	state.ensureTasks()
	if previous, seen := state.tasksByRequest[requestID]; seen {
		if previous.task.DurationSeconds != durationSeconds {
			return TaskView{}, false, ErrTaskConflict
		}
		return taskView(previous.task), true, nil
	}
	now := s.now().UTC()
	taskID := int64(len(state.tasks)) + 1
	task := DiagnosticTask{
		ID:              taskID,
		RequestID:       requestID,
		DurationSeconds: durationSeconds,
		Status:          taskStatusPending,
		CreatedAt:       now,
		NextClaimableAt: now,
	}
	auditRec := TaskAuditRecord{
		Seq:      1,
		Event:    auditEventCreated,
		ToStatus: taskStatusPending,
		At:       now,
	}
	if s.wal != nil {
		if err := s.wal.appendRecord(recTaskCreate, walTaskCreate{
			DeviceID:        id,
			TaskID:          taskID,
			RequestID:       requestID,
			DurationSeconds: durationSeconds,
			CreatedAt:       now,
			NextClaimableAt: now,
			Audit:           auditRec,
		}); err != nil {
			return TaskView{}, false, storageUnavailable(err)
		}
	}
	ts := &taskState{task: task, reports: make(map[string]storedReport)}
	ts.audit = append(ts.audit, auditRec)
	state.tasks = append(state.tasks, ts)
	state.tasksByRequest[requestID] = ts
	return taskView(task), false, nil
}

// ClaimTask lets the simulated device take the earliest-created task that has
// reached its claimable time. At most one task may be in progress on the
// device; concurrent claims therefore succeed for exactly one caller. It
// returns claimed=false (and no error) when nothing is claimable.
func (s *Store) ClaimTask(id string) (ClaimView, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[id]
	if !ok {
		return ClaimView{}, false, ErrDeviceNotFound
	}
	now := s.now().UTC()
	if err := s.evaluateTaskTimeouts(state, now); err != nil {
		return ClaimView{}, false, err
	}
	state.ensureTasks()
	for _, ts := range state.tasks {
		if ts.task.Status == taskStatusInProgress {
			return ClaimView{}, false, nil
		}
	}
	var candidate *taskState
	for _, ts := range state.tasks {
		if ts.task.Status != taskStatusPending && ts.task.Status != taskStatusWaiting {
			continue
		}
		if now.Before(ts.task.NextClaimableAt) {
			continue
		}
		if candidate == nil || ts.task.ID < candidate.task.ID {
			candidate = ts
		}
	}
	if candidate == nil {
		return ClaimView{}, false, nil
	}
	fromStatus := candidate.task.Status
	attempt := candidate.task.Attempts + 1
	credential := newTaskCredential()
	deadline := now.Add(time.Duration(candidate.task.DurationSeconds) * time.Second)
	auditRec := TaskAuditRecord{
		Seq:        int64(len(candidate.audit)) + 1,
		Event:      auditEventClaimed,
		FromStatus: fromStatus,
		ToStatus:   taskStatusInProgress,
		Attempt:    attempt,
		At:         now,
	}
	if s.wal != nil {
		if err := s.wal.appendRecord(recTaskClaim, walTaskClaim{
			DeviceID:   id,
			TaskID:     candidate.task.ID,
			Attempt:    attempt,
			Credential: credential,
			ClaimedAt:  now,
			Deadline:   deadline,
			Audit:      auditRec,
		}); err != nil {
			return ClaimView{}, false, storageUnavailable(err)
		}
	}
	candidate.task.Status = taskStatusInProgress
	candidate.task.Attempts = attempt
	candidate.task.Credential = credential
	candidate.task.ClaimedAt = &now
	candidate.task.Deadline = &deadline
	candidate.task.NextClaimableAt = time.Time{}
	candidate.audit = append(candidate.audit, auditRec)
	return ClaimView{
		Task:       taskView(candidate.task),
		Attempt:    attempt,
		Credential: credential,
		Deadline:   deadline,
	}, true, nil
}

// ReportTask records one device-reported outcome.
//
// A repeated receiptId with identical success/result/reason returns the first
// report (repeat=true); a reused receiptId with different content conflicts. A
// new receiptId is accepted only when the task is in progress, the credential
// matches the current attempt, and the deadline has not passed; otherwise it
// conflicts. Success ends the task with a JSON object result; a failure
// invalidates the credential, releases the device, and either schedules a
// backoff retry or ends the task on the third failure.
func (s *Store) ReportTask(id string, taskID int64, receiptID, credential string, success bool, reason string, result json.RawMessage) (TaskReport, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[id]
	if !ok {
		return TaskReport{}, false, ErrDeviceNotFound
	}
	state.ensureTasks()
	ts := findTask(state, taskID)
	if ts == nil {
		return TaskReport{}, false, ErrTaskNotFound
	}
	now := s.now().UTC()
	if err := s.evaluateTaskTimeouts(state, now); err != nil {
		return TaskReport{}, false, err
	}
	if previous, seen := ts.reports[receiptID]; seen {
		if previous.success != success || previous.reason != reason || !sameJSON(previous.result, result) {
			return TaskReport{}, false, ErrTaskConflict
		}
		return taskReportView(previous), true, nil
	}
	if ts.task.Status != taskStatusInProgress || ts.task.Credential == "" || ts.task.Credential != credential {
		return TaskReport{}, false, ErrTaskConflict
	}

	// Compute the resulting state without mutating yet. Success ends the task
	// with a JSON object result; a failure goes through the same backoff/end
	// rules as a deadline expiry (see planTaskFailure).
	newStatus := ts.task.Status
	newFailures := ts.task.Failures
	var newNextClaimableAt time.Time
	var newFailureReason string
	var newCompletedAt *time.Time
	var auditRec TaskAuditRecord
	if success {
		newStatus = taskStatusSucceeded
		completedAt := now
		newCompletedAt = &completedAt
		auditRec = TaskAuditRecord{
			Seq:      int64(len(ts.audit)) + 1,
			Event:    auditEventSucceeded,
			ToStatus: taskStatusSucceeded,
			Attempt:  ts.task.Attempts,
			At:       now,
		}
	} else {
		// The wait runs from the moment the server received the report and the
		// device's own reason is retained.
		outcome := planTaskFailure(ts.task, auditEventFailed, "", now, reason)
		outcome.audit.Seq = int64(len(ts.audit)) + 1
		auditRec = outcome.audit
		newStatus = outcome.status
		newFailures = outcome.failures
		newNextClaimableAt = outcome.nextClaimableAt
		newFailureReason = reason
		newCompletedAt = outcome.completedAt
	}
	stored := storedReport{
		receiptID:  receiptID,
		success:    success,
		reason:     reason,
		result:     cloneJSON(result),
		receivedAt: now,
	}
	if s.wal != nil {
		if err := s.wal.appendRecord(recTaskReport, walTaskReport{
			DeviceID:        id,
			TaskID:          taskID,
			ReceiptID:       receiptID,
			Success:         success,
			Reason:          reason,
			Result:          cloneJSON(result),
			ReceivedAt:      now,
			Status:          newStatus,
			Failures:        newFailures,
			NextClaimableAt: newNextClaimableAt,
			FailureReason:   newFailureReason,
			CompletedAt:     newCompletedAt,
			Audit:           auditRec,
		}); err != nil {
			return TaskReport{}, false, storageUnavailable(err)
		}
	}
	// Commit the in-memory state only after the durable commit point.
	ts.task.Status = newStatus
	ts.task.Failures = newFailures
	ts.task.NextClaimableAt = newNextClaimableAt
	ts.task.FailureReason = newFailureReason
	ts.task.CompletedAt = newCompletedAt
	if success {
		ts.task.Result = cloneJSON(result)
	}
	releaseTaskAttempt(&ts.task)
	ts.reports[receiptID] = stored
	ts.audit = append(ts.audit, auditRec)
	return taskReportView(stored), false, nil
}

// CancelTask terminates a pending, waiting or in-progress task immediately. A
// duplicate cancel returns the original result; a cancel after a success or
// failure end conflicts.
func (s *Store) CancelTask(id string, taskID int64) (TaskView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[id]
	if !ok {
		return TaskView{}, ErrDeviceNotFound
	}
	state.ensureTasks()
	ts := findTask(state, taskID)
	if ts == nil {
		return TaskView{}, ErrTaskNotFound
	}
	now := s.now().UTC()
	if err := s.evaluateTaskTimeouts(state, now); err != nil {
		return TaskView{}, err
	}
	fromStatus := ts.task.Status
	switch fromStatus {
	case taskStatusCanceled:
		return taskView(ts.task), nil
	case taskStatusSucceeded, taskStatusFailed:
		return TaskView{}, ErrTaskConflict
	}
	auditRec := TaskAuditRecord{
		Seq:        int64(len(ts.audit)) + 1,
		Event:      auditEventCanceled,
		FromStatus: fromStatus,
		ToStatus:   taskStatusCanceled,
		At:         now,
	}
	if s.wal != nil {
		if err := s.wal.appendRecord(recTaskCancel, walTaskCancel{
			DeviceID:   id,
			TaskID:     taskID,
			CanceledAt: now,
			Audit:      auditRec,
		}); err != nil {
			return TaskView{}, storageUnavailable(err)
		}
	}
	ts.task.Status = taskStatusCanceled
	completedAt := now
	ts.task.CompletedAt = &completedAt
	releaseTaskAttempt(&ts.task)
	ts.audit = append(ts.audit, auditRec)
	return taskView(ts.task), nil
}

// ListTasks returns every task on the device in ascending id order. Deadline
// expiries are applied before reading so the returned status is current.
func (s *Store) ListTasks(id string) ([]TaskView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[id]
	if !ok {
		return nil, ErrDeviceNotFound
	}
	now := s.now().UTC()
	if err := s.evaluateTaskTimeouts(state, now); err != nil {
		return nil, err
	}
	state.ensureTasks()
	views := make([]TaskView, 0, len(state.tasks))
	for _, ts := range state.tasks {
		views = append(views, taskView(ts.task))
	}
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	return views, nil
}

// GetTask returns one task. Deadline expiries are applied first.
func (s *Store) GetTask(id string, taskID int64) (TaskView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[id]
	if !ok {
		return TaskView{}, ErrDeviceNotFound
	}
	now := s.now().UTC()
	if err := s.evaluateTaskTimeouts(state, now); err != nil {
		return TaskView{}, err
	}
	if ts := findTask(state, taskID); ts != nil {
		return taskView(ts.task), nil
	}
	return TaskView{}, ErrTaskNotFound
}

// ListTaskAudit returns a task's audit records in occurrence order. Deadline
// expiries are applied first.
func (s *Store) ListTaskAudit(id string, taskID int64) ([]TaskAuditRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[id]
	if !ok {
		return nil, ErrDeviceNotFound
	}
	now := s.now().UTC()
	if err := s.evaluateTaskTimeouts(state, now); err != nil {
		return nil, err
	}
	if ts := findTask(state, taskID); ts != nil {
		return append([]TaskAuditRecord(nil), ts.audit...), nil
	}
	return nil, ErrTaskNotFound
}

// evaluateTaskTimeouts applies deadline expiry to every in-progress task whose
// deadline has passed, persisting each transition. The failure is recorded at
// the deadline (when it occurred), and the backoff wait runs from that same
// instant. Caller must hold s.mu.
func (s *Store) evaluateTaskTimeouts(state *deviceState, now time.Time) error {
	for _, ts := range state.tasks {
		if ts.task.Status != taskStatusInProgress || ts.task.Deadline == nil {
			continue
		}
		deadline := *ts.task.Deadline
		// Exactly at the deadline is still within it; only a later instant
		// expires it, so a long-delayed query never re-times a finished task.
		if !now.After(deadline) {
			continue
		}
		// The failure is attributed to the deadline, not to the query time:
		// the wait, completion and audit timestamp all run from the deadline.
		outcome := planTaskFailure(ts.task, auditEventTimedOut, taskStatusInProgress, deadline, taskTimeoutReason)
		outcome.audit.Seq = int64(len(ts.audit)) + 1
		if s.wal != nil {
			if err := s.wal.appendRecord(recTaskTimeout, walTaskTimeout{
				DeviceID:        state.device.ID,
				TaskID:          ts.task.ID,
				Attempt:         ts.task.Attempts,
				Deadline:        deadline,
				Status:          outcome.status,
				Failures:        outcome.failures,
				NextClaimableAt: outcome.nextClaimableAt,
				FailureReason:   taskTimeoutReason,
				CompletedAt:     outcome.completedAt,
				Audit:           outcome.audit,
			}); err != nil {
				return storageUnavailable(err)
			}
		}
		ts.task.Status = outcome.status
		ts.task.Failures = outcome.failures
		ts.task.NextClaimableAt = outcome.nextClaimableAt
		ts.task.FailureReason = taskTimeoutReason
		ts.task.CompletedAt = outcome.completedAt
		releaseTaskAttempt(&ts.task)
		ts.audit = append(ts.audit, outcome.audit)
	}
	return nil
}

// taskFailureOutcome is the planned, not-yet-committed result of one failure.
type taskFailureOutcome struct {
	status          string
	failures        int
	nextClaimableAt time.Time
	completedAt     *time.Time
	audit           TaskAuditRecord
}

// planTaskFailure applies the single failure rule shared by device-reported
// failures and deadline expiries. It accumulates onto the task's current
// failure count: the first two failures move the task to waiting with a 1s and
// 2s backoff measured from at; the third ends it as failed and stamps at as the
// completion time. event distinguishes the source (failed vs timed_out) and an
// empty fromStatus matches a reported failure, which records none. It never
// mutates the task and does not assign an audit sequence number.
func planTaskFailure(task DiagnosticTask, event, fromStatus string, at time.Time, reason string) taskFailureOutcome {
	failures := task.Failures + 1
	outcome := taskFailureOutcome{
		failures: failures,
		audit: TaskAuditRecord{
			Event:      event,
			FromStatus: fromStatus,
			Attempt:    task.Attempts,
			At:         at,
			Reason:     reason,
		},
	}
	if failures >= taskMaxAttempts {
		outcome.status = taskStatusFailed
		completedAt := at
		outcome.completedAt = &completedAt
		outcome.audit.ToStatus = taskStatusFailed
		return outcome
	}
	outcome.status = taskStatusWaiting
	outcome.nextClaimableAt = at.Add(backoffFor(failures))
	outcome.audit.ToStatus = taskStatusWaiting
	return outcome
}

// releaseTaskAttempt invalidates the current attempt's credential and frees the
// device so another claimable task can be taken. It is shared by every end of
// an in-progress attempt: success, reported failure, timeout and cancel.
func releaseTaskAttempt(task *DiagnosticTask) {
	task.Credential = ""
	task.ClaimedAt = nil
	task.Deadline = nil
}

// backoffFor returns the wait after the given failure count. The third failure
// ends the task, so no backoff is defined for it.
func backoffFor(failures int) time.Duration {
	switch failures {
	case 1:
		return 1 * time.Second
	case 2:
		return 2 * time.Second
	default:
		return 0
	}
}

func newTaskCredential() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("cannot generate task credential: %v", err))
	}
	return hex.EncodeToString(b)
}

func taskView(task DiagnosticTask) TaskView {
	view := TaskView{
		ID:              task.ID,
		RequestID:       task.RequestID,
		DurationSeconds: task.DurationSeconds,
		Status:          task.Status,
		Attempts:        task.Attempts,
		CreatedAt:       task.CreatedAt,
		ClaimedAt:       task.ClaimedAt,
		Deadline:        task.Deadline,
		Result:          cloneJSON(task.Result),
		FailureReason:   task.FailureReason,
		CompletedAt:     task.CompletedAt,
	}
	if !task.NextClaimableAt.IsZero() {
		t := task.NextClaimableAt
		view.NextClaimableAt = &t
	}
	return view
}

func taskReportView(report storedReport) TaskReport {
	return TaskReport{
		ReceiptID:  report.receiptID,
		Success:    report.success,
		Reason:     report.reason,
		Result:     cloneJSON(report.result),
		ReceivedAt: report.receivedAt,
	}
}

// isJSONObject reports whether raw is exactly one JSON object. It is shared by
// HTTP validation and WAL recovery.
func isJSONObject(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	_, ok := value.(map[string]any)
	return ok
}
