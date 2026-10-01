package fleet

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// Remote diagnostics lets a fleet operator create tasks for a registered
// device; a simulated device claims the oldest claimable task, runs it for
// the requested number of seconds, and reports a JSON result or a failure.
//
// Lifecycle:
//
//	pending -> running -> succeeded
//	pending -> running -> waiting -> running -> ... -> failed (third failure)
//	pending | waiting | running -> cancelled
//
// An in-flight attempt that passes its deadline counts as a timeout failure.
// Failures invalidate the claim token, free the device's single execution
// slot, and arm a retry backoff (1s after the first failure, 2s after the
// second); the third failure ends the task. Cancellation ends immediately and
// invalidates outstanding tokens. Every state transition appends one audit
// entry; idempotent retries and rejected requests never do.

const (
	taskStatusPending   = "pending"
	taskStatusRunning   = "running"
	taskStatusWaiting   = "waiting"
	taskStatusSucceeded = "succeeded"
	taskStatusFailed    = "failed"
	taskStatusCancelled = "cancelled"

	auditActionCreate  = "create"
	auditActionClaim   = "claim"
	auditActionFail    = "fail"
	auditActionTimeout = "timeout"
	auditActionSuccess = "success"
	auditActionCancel  = "cancel"

	maxTaskAttempts = 3
)

var (
	// ErrTaskConflict covers a requestId reused with changed seconds, a report
	// whose receiptId was already committed with different content, a report
	// using an invalid/unknown/foreign/expired token, and cancelling a task
	// already ended by success or failure.
	ErrTaskConflict = errors.New("diagnostic task conflict")
	// ErrTaskNotFound is returned for an unknown task number on a known device.
	ErrTaskNotFound = errors.New("diagnostic task not found")
)

// retryDelays[k] is the wait after the k-th failure before the task may be
// claimed again: 1s after the first, 2s after the second.
var retryDelays = map[int]time.Duration{
	1: time.Second,
	2: 2 * time.Second,
}

const timeoutReason = "execution deadline exceeded"

// TaskSummary is the external view of one diagnostic task.
type TaskSummary struct {
	Number          int64           `json:"number"`
	RequestID       string          `json:"requestId"`
	Seconds         int             `json:"seconds"`
	Status          string          `json:"status"`
	Attempts        int             `json:"attempts"`
	CreatedAt       time.Time       `json:"createdAt"`
	ClaimedAt       *time.Time      `json:"claimedAt,omitempty"`
	Deadline        *time.Time      `json:"deadline,omitempty"`
	NextClaimableAt *time.Time      `json:"nextClaimableAt,omitempty"`
	FinishedAt      *time.Time      `json:"finishedAt,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	FailureReason   string          `json:"failureReason,omitempty"`
}

// TaskClaimView is returned on a successful claim.
type TaskClaimView struct {
	Number   int64     `json:"task"`
	Attempt  int       `json:"attempt"`
	Token    string    `json:"token"`
	Deadline time.Time `json:"deadline"`
}

// TaskReportAck is the response body for an accepted report or a matching
// idempotent retry.
type TaskReportAck struct {
	Number          int64           `json:"task"`
	Attempt         int             `json:"attempt"`
	ReceiptID       string          `json:"receiptId"`
	Status          string          `json:"status"`
	Attempts        int             `json:"attempts"`
	ReceivedAt      time.Time       `json:"receivedAt"`
	NextClaimableAt *time.Time      `json:"nextClaimableAt,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	Reason          string          `json:"reason,omitempty"`
}

// TaskCancellation is returned on cancellation and repeated cancellation.
type TaskCancellation struct {
	Number      int64     `json:"task"`
	Status      string    `json:"status"`
	CancelledAt time.Time `json:"cancelledAt"`
}

// AuditEntry records one state transition. Retries and rejections never
// create entries.
type AuditEntry struct {
	Seq        int       `json:"seq"`
	Task       int64     `json:"task"`
	Action     string    `json:"action"`
	FromStatus string    `json:"fromStatus"`
	ToStatus   string    `json:"toStatus"`
	Attempt    int       `json:"attempt"`
	At         time.Time `json:"at"`
	Reason     string    `json:"reason,omitempty"`
}

// taskAttempt is one claim: the token handed out, its 1-based attempt number,
// its claim time and deadline. Attempts are never deleted: a token from an
// earlier attempt must keep resolving to its task so it can be rejected as
// stale rather than unknown.
type taskAttempt struct {
	num       int
	token     string
	claimedAt time.Time
	deadline  time.Time
}

// storedReport remembers one accepted report so re-POSTing the same receiptId
// returns the first ack (200), including after the task has ended, and a
// reused receiptId with different content conflicts (409).
type storedReport struct {
	receiptID string
	success   bool
	reason    string
	result    json.RawMessage
	ack       TaskReportAck
}

// diagTask is the full state of one diagnostic task.
type diagTask struct {
	number    int64
	requestID string
	seconds   int
	status    string
	attempts  int // number of claims so far
	createdAt time.Time

	history []taskAttempt // one entry per claim

	claimedAt *time.Time // most recent claim
	deadline  *time.Time // current attempt deadline (running)
	nextClaim time.Time  // earliest re-claim (waiting)

	result     json.RawMessage
	reason     string
	finishedAt *time.Time

	createAck   TaskSummary             // first creation response, returned on retry
	reports     map[string]storedReport // receiptId -> first report
	cancelledAt *time.Time              // set once cancelled
}

// diagState holds all diagnostic tasks for one device.
type diagState struct {
	tasks     []*diagTask          // number = index + 1
	byRequest map[string]*diagTask // requestId -> task
	byToken   map[string]tokenRef  // token -> task/attempt
	active    int64                // number of the running task, 0 when free
	audit     []AuditEntry         // occurrence order across the device
}

type tokenRef struct {
	number  int64
	attempt int
}

func newDiagState() *diagState {
	return &diagState{
		byRequest: make(map[string]*diagTask),
		byToken:   make(map[string]tokenRef),
	}
}

// ensureDiagnostics lazily attaches diagnostic state to a device.
func (state *deviceState) ensureDiagnostics() *diagState {
	if state.diag == nil {
		state.diag = newDiagState()
	}
	return state.diag
}

// plannedTransition is a state transition awaiting its durable commit point.
// Audit rows are appended only after the WAL write succeeds, so a failed write
// rolls state back without leaving audit residue.
type plannedTransition struct {
	task    *diagTask
	action  string
	from    string
	to      string
	attempt int
	at      time.Time
	reason  string
}

// settleSnapshot lets a lazy timeout settle be undone when its durable write
// fails. A settle only changes the task's status/reason/finished/nextClaim and
// the device's active pointer; history and deadlines are untouched.
type settleSnapshot struct {
	task       *diagTask
	status     string
	reason     string
	finishedAt *time.Time
	deadline   *time.Time
	nextClaim  time.Time
	active     int64
}

func (d *diagState) snapshotSettle() *settleSnapshot {
	if d.active == 0 {
		return nil
	}
	task := d.tasks[d.active-1]
	return &settleSnapshot{
		task:       task,
		status:     task.status,
		reason:     task.reason,
		finishedAt: task.finishedAt,
		deadline:   cloneTimePtr(task.deadline),
		nextClaim:  task.nextClaim,
		active:     d.active,
	}
}

func (d *diagState) restore(snap *settleSnapshot) {
	if snap == nil {
		return
	}
	task := snap.task
	task.status = snap.status
	task.reason = snap.reason
	task.finishedAt = snap.finishedAt
	task.deadline = cloneTimePtr(snap.deadline)
	task.nextClaim = snap.nextClaim
	d.active = snap.active
}

// settleExpired turns an elapsed running attempt into a timeout failure. It
// returns the planned transition (audit appended by the caller after a
// successful commit) and a snapshot for rollback.
func (d *diagState) settleExpired(now time.Time) (*plannedTransition, *settleSnapshot) {
	if d.active == 0 {
		return nil, nil
	}
	task := d.tasks[d.active-1]
	if task.status != taskStatusRunning {
		return nil, nil
	}
	attempt := task.history[len(task.history)-1]
	if now.Before(attempt.deadline) {
		return nil, nil
	}
	snap := d.snapshotSettle()
	from := task.status
	// A timeout counts as a failure at the instant of the deadline, and the
	// retry wait runs from that instant even when the lapse is discovered
	// later by a claim or query.
	failureAt := attempt.deadline.UTC()
	task.attemptFailure(attempt.num, timeoutReason, failureAt)
	d.active = 0
	transition := &plannedTransition{
		task: task, action: auditActionTimeout, from: from, to: task.status,
		attempt: attempt.num, at: failureAt, reason: timeoutReason,
	}
	return transition, snap
}

// commitSettles persists a standalone batch of lazy timeout transitions (used
// by queries and by claims/cancels that end up without their own write). On
// failure the in-memory settle is rolled back. On success the audit rows are
// appended.
func (s *Store) commitSettles(deviceID string, d *diagState, transitions []*plannedTransition, snapshots []*settleSnapshot) error {
	if len(transitions) == 0 {
		return nil
	}
	if s.wal != nil {
		if err := s.wal.appendRecord(recTaskTransitions, walTaskTransitions{
			DeviceID:    deviceID,
			Transitions: walTransitions(transitions),
		}); err != nil {
			for i := len(snapshots) - 1; i >= 0; i-- {
				d.restore(snapshots[i])
			}
			return storageUnavailable(err)
		}
	}
	for _, tr := range transitions {
		d.appendAudit(tr)
	}
	return nil
}

// attemptFailure applies a failure (explicit or timeout) for the latest
// attempt: the third failure ends the task, earlier ones arm the backoff.
func (t *diagTask) attemptFailure(attempt int, reason string, now time.Time) {
	if attempt >= maxTaskAttempts {
		t.status = taskStatusFailed
		t.reason = reason
		finished := now
		t.finishedAt = &finished
		t.nextClaim = time.Time{}
		t.deadline = nil
		return
	}
	t.status = taskStatusWaiting
	t.reason = ""
	t.nextClaim = now.Add(retryDelays[attempt])
	t.deadline = nil
}

// claimable reports whether the task may be claimed at now.
func (t *diagTask) claimable(now time.Time) bool {
	switch t.status {
	case taskStatusPending:
		return true
	case taskStatusWaiting:
		return !now.Before(t.nextClaim)
	}
	return false
}

func (t *diagTask) terminal() bool {
	switch t.status {
	case taskStatusSucceeded, taskStatusFailed, taskStatusCancelled:
		return true
	}
	return false
}

// --- create ----------------------------------------------------------------

// CreateDiagnosticTask creates a pending task. requestId dedups within the
// device: an identical re-submission returns the first result (repeat=true,
// HTTP 200); a changed duration is ErrTaskConflict (409).
func (s *Store) CreateDiagnosticTask(deviceID, requestID string, seconds int) (TaskSummary, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[deviceID]
	if !ok {
		return TaskSummary{}, false, ErrDeviceNotFound
	}
	d := state.ensureDiagnostics()
	now := s.now().UTC()

	// Reflect a deadline that has elapsed before appending anything, so the
	// audit trail stays in occurrence order.
	if err := s.settleForRead(deviceID, d); err != nil {
		return TaskSummary{}, false, err
	}

	if previous, seen := d.byRequest[requestID]; seen {
		if previous.seconds != seconds {
			return TaskSummary{}, false, ErrTaskConflict
		}
		// Re-submission always replays the exact first creation response, even
		// after the task has since been claimed or finished.
		return previous.createAck, true, nil
	}

	task := &diagTask{
		number:    int64(len(d.tasks)) + 1,
		requestID: requestID,
		seconds:   seconds,
		status:    taskStatusPending,
		createdAt: now,
		reports:   make(map[string]storedReport),
	}
	ack := taskView(task)
	task.createAck = ack
	if s.wal != nil {
		if err := s.wal.appendRecord(recTaskCreate, walTaskCreate{
			DeviceID: deviceID, Number: task.number, RequestID: requestID,
			Seconds: seconds, CreatedAt: now,
		}); err != nil {
			return TaskSummary{}, false, storageUnavailable(err)
		}
	}
	d.tasks = append(d.tasks, task)
	d.byRequest[requestID] = task
	d.appendAudit(&plannedTransition{
		task: task, action: auditActionCreate, from: "", to: taskStatusPending, at: now,
	})
	return ack, false, nil
}

// --- claim -----------------------------------------------------------------

// ClaimDiagnosticTask hands out the oldest claimable task. At most one task
// per device is running at a time and concurrent callers can only succeed
// once; ok=false means HTTP 204.
func (s *Store) ClaimDiagnosticTask(deviceID string) (TaskClaimView, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[deviceID]
	if !ok {
		return TaskClaimView{}, false, ErrDeviceNotFound
	}
	d := state.ensureDiagnostics()
	now := s.now().UTC()

	// An elapsed attempt becomes a timeout failure before anything else can be
	// claimed; it is committed together with the claim below as one unit.
	settleTr, settleSnap := d.settleExpired(now)

	if d.active != 0 {
		// Slot still occupied by a live task. A settle may nevertheless have
		// happened only when it freed the slot, so reaching here means no settle.
		return TaskClaimView{}, false, nil
	}

	var target *diagTask
	for _, task := range d.tasks {
		if task.claimable(now) {
			target = task
			break
		}
	}
	if target == nil {
		var transitions []*plannedTransition
		var snapshots []*settleSnapshot
		if settleTr != nil {
			transitions = []*plannedTransition{settleTr}
			snapshots = []*settleSnapshot{settleSnap}
		}
		if err := s.commitSettles(deviceID, d, transitions, snapshots); err != nil {
			return TaskClaimView{}, false, err
		}
		return TaskClaimView{}, false, nil
	}

	from := target.status
	attemptNum := target.attempts + 1
	token := newTaskToken()
	deadline := now.Add(time.Duration(target.seconds) * time.Second)
	target.history = append(target.history, taskAttempt{
		num: attemptNum, token: token, claimedAt: now, deadline: deadline,
	})
	target.status = taskStatusRunning
	target.attempts = attemptNum
	claimedAt := now
	target.claimedAt = &claimedAt
	target.deadline = &deadline
	d.active = target.number
	d.byToken[token] = tokenRef{number: target.number, attempt: attemptNum}

	if s.wal != nil {
		record := walTaskClaim{
			DeviceID: deviceID, Number: target.number, Attempt: attemptNum,
			Token: token, ClaimedAt: now, Deadline: deadline,
		}
		if settleTr != nil {
			record.Settles = []walTaskTransition{walTransition(settleTr)}
		}
		if err := s.wal.appendRecord(recTaskClaim, record); err != nil {
			d.rollbackClaim(target, attemptNum, from)
			d.restore(settleSnap)
			return TaskClaimView{}, false, storageUnavailable(err)
		}
	}
	if settleTr != nil {
		d.appendAudit(settleTr)
	}
	d.appendAudit(&plannedTransition{
		task: target, action: auditActionClaim, from: from, to: taskStatusRunning,
		attempt: attemptNum, at: now,
	})
	return TaskClaimView{
		Number: target.number, Attempt: attemptNum, Token: token, Deadline: deadline,
	}, true, nil
}

// rollbackClaim undoes a claim whose durable write failed.
func (d *diagState) rollbackClaim(t *diagTask, attemptNum int, from string) {
	delete(d.byToken, t.history[len(t.history)-1].token)
	t.history = t.history[:len(t.history)-1]
	t.status = from
	t.attempts = attemptNum - 1
	t.deadline = nil
	if len(t.history) > 0 {
		previous := t.history[len(t.history)-1].claimedAt
		t.claimedAt = &previous
	} else {
		t.claimedAt = nil
	}
	d.active = 0
}

// --- report ----------------------------------------------------------------

// ReportDiagnosticTask accepts the simulated device's outcome for an attempt.
//
// A receiptId already used on the task with identical content returns the
// first ack (repeat=true, 200) even after the task ended; different content is
// 409. A new report needs a live token for the current attempt submitted
// before its deadline; an unknown/foreign/stale/expired token is 409.
func (s *Store) ReportDiagnosticTask(deviceID, token, receiptID string, success bool, reason string, result json.RawMessage) (TaskReportAck, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[deviceID]
	if !ok {
		return TaskReportAck{}, false, ErrDeviceNotFound
	}
	d := state.ensureDiagnostics()
	now := s.now().UTC()

	// A reason belongs to failures only; a successful report never carries one.
	if success {
		reason = ""
	}

	ref, known := d.byToken[token]
	if !known {
		return TaskReportAck{}, false, ErrTaskConflict
	}
	if ref.number < 1 || int(ref.number) > len(d.tasks) {
		return TaskReportAck{}, false, ErrTaskConflict
	}
	task := d.tasks[ref.number-1]

	// receiptId dedup within the task, checked before token liveness so an
	// accepted report's retry keeps working once the token is invalidated.
	if prior, seen := task.reports[receiptID]; seen {
		if !reportsEqual(prior, success, reason, result) {
			return TaskReportAck{}, false, ErrTaskConflict
		}
		return prior.ack, true, nil
	}

	// An elapsed deadline is a timeout, settled before the report is judged;
	// its transition is committed on its own.
	settleTr, settleSnap := d.settleExpired(now)
	if settleTr != nil {
		if err := s.commitSettles(deviceID, d,
			[]*plannedTransition{settleTr}, []*settleSnapshot{settleSnap}); err != nil {
			return TaskReportAck{}, false, err
		}
	}

	if task.status != taskStatusRunning || d.active != task.number ||
		ref.attempt != len(task.history) || !now.Before(task.history[ref.attempt-1].deadline) {
		return TaskReportAck{}, false, ErrTaskConflict
	}

	attempt := task.history[len(task.history)-1]
	from := task.status
	var transition *plannedTransition
	ack := TaskReportAck{
		Number: task.number, Attempt: attempt.num, ReceiptID: receiptID,
		Attempts: task.attempts, ReceivedAt: now,
	}
	if success {
		task.status = taskStatusSucceeded
		task.result = cloneJSON(result)
		task.reason = ""
		finished := now
		task.finishedAt = &finished
		ack.Status = taskStatusSucceeded
		ack.Result = cloneJSON(result)
		transition = &plannedTransition{
			task: task, action: auditActionSuccess, from: from, to: taskStatusSucceeded,
			attempt: attempt.num, at: now,
		}
	} else {
		task.attemptFailure(attempt.num, reason, now)
		ack.Status = task.status
		ack.Reason = reason
		if task.status == taskStatusWaiting {
			next := task.nextClaim.UTC()
			ack.NextClaimableAt = &next
		}
		transition = &plannedTransition{
			task: task, action: auditActionFail, from: from, to: task.status,
			attempt: attempt.num, at: now, reason: reason,
		}
	}

	if s.wal != nil {
		if err := s.wal.appendRecord(recTaskReport, walTaskReport{
			DeviceID: deviceID, Number: task.number, Attempt: attempt.num,
			Token: token, ReceiptID: receiptID, Success: success, Reason: reason,
			Result: cloneJSON(result), At: now,
		}); err != nil {
			d.rollbackReport(task, attempt.num, from)
			return TaskReportAck{}, false, storageUnavailable(err)
		}
	}

	// Success and failure alike release the single execution slot; the token
	// stays in the map resolving to a stale attempt so a late report is 409.
	d.active = 0
	if task.reports == nil {
		task.reports = make(map[string]storedReport)
	}
	task.reports[receiptID] = storedReport{
		receiptID: receiptID, success: success, reason: reason,
		result: cloneJSON(result), ack: cloneReportAck(ack),
	}
	d.appendAudit(transition)
	return ack, false, nil
}

// rollbackReport undoes a report whose durable write failed.
func (d *diagState) rollbackReport(t *diagTask, attempt int, from string) {
	last := t.history[attempt-1]
	t.status = from
	t.result = nil
	t.reason = ""
	t.finishedAt = nil
	t.deadline = &last.deadline
	t.nextClaim = time.Time{}
	d.active = t.number
}

// reportsEqual compares a new report with a stored one: success flag and
// reason must match, and successful results compare semantically (object key
// order ignored, numbers by value, array order significant).
func reportsEqual(prior storedReport, success bool, reason string, result json.RawMessage) bool {
	if prior.success != success || prior.reason != reason {
		return false
	}
	if success {
		return sameJSON(prior.result, result)
	}
	return true
}

// --- cancel ----------------------------------------------------------------

// CancelDiagnosticTask cancels a pending, waiting or running task. Repeated
// cancellation returns the first result (repeat=true). Cancelling a task that
// already succeeded or failed is ErrTaskConflict (409).
func (s *Store) CancelDiagnosticTask(deviceID string, number int64) (TaskCancellation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[deviceID]
	if !ok {
		return TaskCancellation{}, false, ErrDeviceNotFound
	}
	d := state.ensureDiagnostics()
	now := s.now().UTC()

	settleTr, settleSnap := d.settleExpired(now)

	task, err := d.taskByNumber(number)
	if err != nil {
		if settleTr != nil {
			if commitErr := s.commitSettles(deviceID, d,
				[]*plannedTransition{settleTr}, []*settleSnapshot{settleSnap}); commitErr != nil {
				return TaskCancellation{}, false, commitErr
			}
		}
		return TaskCancellation{}, false, err
	}
	if task.cancelledAt != nil {
		if settleTr != nil {
			if commitErr := s.commitSettles(deviceID, d,
				[]*plannedTransition{settleTr}, []*settleSnapshot{settleSnap}); commitErr != nil {
				return TaskCancellation{}, false, commitErr
			}
		}
		return TaskCancellation{
			Number: task.number, Status: taskStatusCancelled, CancelledAt: *task.cancelledAt,
		}, true, nil
	}
	if task.terminal() {
		if settleTr != nil {
			if commitErr := s.commitSettles(deviceID, d,
				[]*plannedTransition{settleTr}, []*settleSnapshot{settleSnap}); commitErr != nil {
				return TaskCancellation{}, false, commitErr
			}
		}
		return TaskCancellation{}, false, ErrTaskConflict
	}

	from := task.status
	if from == taskStatusRunning && settleTr != nil && settleTr.task == task {
		// The settle above turned this very task from running to waiting; the
		// cancel audit's from-status reflects the post-settle state.
		from = taskStatusWaiting
	}
	prevDeadline := cloneTimePtr(task.deadline)
	prevNextClaim := task.nextClaim
	prevActive := d.active
	task.status = taskStatusCancelled
	task.nextClaim = time.Time{}
	task.deadline = nil
	if d.active == task.number {
		d.active = 0
	}
	cancelledAt := now
	task.cancelledAt = &cancelledAt
	task.finishedAt = &cancelledAt

	if s.wal != nil {
		record := walTaskCancel{
			DeviceID: deviceID, Number: task.number, Attempt: task.attempts,
			FromStatus: from, CancelledAt: now,
		}
		if settleTr != nil {
			record.Settles = []walTaskTransition{walTransition(settleTr)}
		}
		if err := s.wal.appendRecord(recTaskCancel, record); err != nil {
			task.status = from
			task.cancelledAt = nil
			task.finishedAt = nil
			task.deadline = prevDeadline
			task.nextClaim = prevNextClaim
			d.active = prevActive
			d.restore(settleSnap)
			return TaskCancellation{}, false, storageUnavailable(err)
		}
	}
	if settleTr != nil {
		d.appendAudit(settleTr)
	}
	d.appendAudit(&plannedTransition{
		task: task, action: auditActionCancel, from: from, to: taskStatusCancelled,
		attempt: task.attempts, at: now,
	})
	return TaskCancellation{
		Number: task.number, Status: taskStatusCancelled, CancelledAt: now,
	}, false, nil
}

// --- queries ---------------------------------------------------------------

// ListDiagnosticTasks returns a device's tasks in number order, settling an
// elapsed attempt first so queries reflect expired state.
func (s *Store) ListDiagnosticTasks(deviceID string) ([]TaskSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[deviceID]
	if !ok {
		return nil, ErrDeviceNotFound
	}
	d := state.ensureDiagnostics()
	if err := s.settleForRead(deviceID, d); err != nil {
		return nil, err
	}
	result := make([]TaskSummary, 0, len(d.tasks))
	for _, task := range d.tasks {
		result = append(result, taskView(task))
	}
	return result, nil
}

// GetDiagnosticTask returns one task.
func (s *Store) GetDiagnosticTask(deviceID string, number int64) (TaskSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[deviceID]
	if !ok {
		return TaskSummary{}, ErrDeviceNotFound
	}
	d := state.ensureDiagnostics()
	if err := s.settleForRead(deviceID, d); err != nil {
		return TaskSummary{}, err
	}
	task, err := d.taskByNumber(number)
	if err != nil {
		return TaskSummary{}, err
	}
	return taskView(task), nil
}

// ListTaskAudit returns audit entries in occurrence order.
func (s *Store) ListTaskAudit(deviceID string) ([]AuditEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.devices[deviceID]
	if !ok {
		return nil, ErrDeviceNotFound
	}
	d := state.ensureDiagnostics()
	if err := s.settleForRead(deviceID, d); err != nil {
		return nil, err
	}
	return append([]AuditEntry(nil), d.audit...), nil
}

// settleForRead applies (and in persistent mode commits) any elapsed-timeout
// transition before a read-only view is returned.
func (s *Store) settleForRead(deviceID string, d *diagState) error {
	now := s.now().UTC()
	transition, snap := d.settleExpired(now)
	if transition == nil {
		return nil
	}
	return s.commitSettles(deviceID, d,
		[]*plannedTransition{transition}, []*settleSnapshot{snap})
}

// --- helpers ---------------------------------------------------------------

func (d *diagState) taskByNumber(number int64) (*diagTask, error) {
	if number < 1 || int(number) > len(d.tasks) {
		return nil, ErrTaskNotFound
	}
	return d.tasks[number-1], nil
}

func (d *diagState) appendAudit(tr *plannedTransition) {
	d.audit = append(d.audit, AuditEntry{
		Seq:        len(d.audit) + 1,
		Task:       tr.task.number,
		Action:     tr.action,
		FromStatus: tr.from,
		ToStatus:   tr.to,
		Attempt:    tr.attempt,
		At:         tr.at.UTC(),
		Reason:     tr.reason,
	})
}

func taskView(t *diagTask) TaskSummary {
	view := TaskSummary{
		Number:        t.number,
		RequestID:     t.requestID,
		Seconds:       t.seconds,
		Status:        t.status,
		Attempts:      t.attempts,
		CreatedAt:     t.createdAt,
		ClaimedAt:     cloneTimePtr(t.claimedAt),
		Deadline:      cloneTimePtr(t.deadline),
		FinishedAt:    cloneTimePtr(t.finishedAt),
		Result:        cloneJSON(t.result),
		FailureReason: t.reason,
	}
	if t.status == taskStatusWaiting {
		next := t.nextClaim.UTC()
		view.NextClaimableAt = &next
	}
	return view
}

func cloneTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := t.UTC()
	return &c
}

func cloneReportAck(a TaskReportAck) TaskReportAck {
	a.Result = cloneJSON(a.Result)
	if a.NextClaimableAt != nil {
		next := a.NextClaimableAt.UTC()
		a.NextClaimableAt = &next
	}
	return a
}

func newTaskToken() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		panic("cannot generate task token: " + err.Error())
	}
	return hex.EncodeToString(raw)
}
