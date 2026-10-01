package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Diagnostic task WAL recovery. Records are replayed in committed order and
// rebuild task state, the token index, receiptId dedup records and the audit
// trail. Deadlines and retry instants are persisted explicitly, so reopening a
// data directory never resets them; recovery itself never consults the clock.

func applyTaskCreateRecord(s *Store, payload []byte) error {
	var rec walTaskCreate
	if err := json.Unmarshal(payload, &rec); err != nil {
		return fmt.Errorf("invalid task create record: %w", err)
	}
	state, ok := s.devices[rec.DeviceID]
	if !ok {
		return fmt.Errorf("task create for unknown device %q", rec.DeviceID)
	}
	if strings.TrimSpace(rec.RequestID) == "" || rec.CreatedAt.IsZero() {
		return fmt.Errorf("device %q task create record is incomplete", rec.DeviceID)
	}
	if rec.Seconds < 1 || rec.Seconds > 60 {
		return fmt.Errorf("device %q task %d seconds %d out of range", rec.DeviceID, rec.Number, rec.Seconds)
	}
	d := state.ensureDiagnostics()
	if rec.Number != int64(len(d.tasks))+1 {
		return fmt.Errorf("device %q task number gap: got %d, want %d",
			rec.DeviceID, rec.Number, len(d.tasks)+1)
	}
	if _, dup := d.byRequest[rec.RequestID]; dup {
		return fmt.Errorf("device %q duplicate task request %q", rec.DeviceID, rec.RequestID)
	}
	task := &diagTask{
		number:    rec.Number,
		requestID: rec.RequestID,
		seconds:   rec.Seconds,
		status:    taskStatusPending,
		createdAt: rec.CreatedAt.UTC(),
		reports:   make(map[string]storedReport),
	}
	task.createAck = taskView(task)
	d.tasks = append(d.tasks, task)
	d.byRequest[rec.RequestID] = task
	d.recoverAudit(task, auditActionCreate, "", taskStatusPending, 0, rec.CreatedAt, "")
	return nil
}

func applyTaskClaimRecord(s *Store, payload []byte) error {
	var rec walTaskClaim
	if err := json.Unmarshal(payload, &rec); err != nil {
		return fmt.Errorf("invalid task claim record: %w", err)
	}
	state, ok := s.devices[rec.DeviceID]
	if !ok {
		return fmt.Errorf("task claim for unknown device %q", rec.DeviceID)
	}
	d := state.ensureDiagnostics()

	// Embedded timeout transitions commit first, exactly as they did live.
	for i := range rec.Settles {
		if err := applyTimeoutTransition(d, &rec.Settles[i]); err != nil {
			return fmt.Errorf("device %q claim record: %w", rec.DeviceID, err)
		}
	}

	task, err := d.taskByNumber(rec.Number)
	if err != nil {
		return fmt.Errorf("device %q claim for %w", rec.DeviceID, err)
	}
	if rec.Attempt != len(task.history)+1 || rec.Attempt < 1 || rec.Attempt > maxTaskAttempts {
		return fmt.Errorf("device %q task %d attempt gap: got %d, want %d",
			rec.DeviceID, rec.Number, rec.Attempt, len(task.history)+1)
	}
	if strings.TrimSpace(rec.Token) == "" {
		return fmt.Errorf("device %q task %d claim has no token", rec.DeviceID, rec.Number)
	}
	if _, dup := d.byToken[rec.Token]; dup {
		return fmt.Errorf("device %q task %d reuses token", rec.DeviceID, rec.Number)
	}
	if rec.ClaimedAt.IsZero() {
		return fmt.Errorf("device %q task %d claim has no time", rec.DeviceID, rec.Number)
	}
	wantDeadline := rec.ClaimedAt.Add(time.Duration(task.seconds) * time.Second)
	if !rec.Deadline.Equal(wantDeadline) {
		return fmt.Errorf("device %q task %d deadline %s disagrees with claim time %s + %ds",
			rec.DeviceID, rec.Number, rec.Deadline, rec.ClaimedAt, task.seconds)
	}
	wantFrom := taskStatusPending
	if rec.Attempt > 1 {
		wantFrom = taskStatusWaiting
	}
	if task.status != wantFrom {
		return fmt.Errorf("device %q task %d claimed from status %q, want %q",
			rec.DeviceID, rec.Number, task.status, wantFrom)
	}
	if wantFrom == taskStatusWaiting && rec.ClaimedAt.Before(task.nextClaim) {
		return fmt.Errorf("device %q task %d claimed at %s before retry time %s",
			rec.DeviceID, rec.Number, rec.ClaimedAt, task.nextClaim)
	}
	if d.active != 0 {
		return fmt.Errorf("device %q task %d claimed while task %d is running",
			rec.DeviceID, rec.Number, d.active)
	}

	claimedAt := rec.ClaimedAt.UTC()
	deadline := rec.Deadline.UTC()
	task.history = append(task.history, taskAttempt{
		num: rec.Attempt, token: rec.Token, claimedAt: claimedAt, deadline: deadline,
	})
	task.status = taskStatusRunning
	task.attempts = rec.Attempt
	task.claimedAt = &claimedAt
	task.deadline = &deadline
	task.nextClaim = time.Time{}
	d.active = task.number
	d.byToken[rec.Token] = tokenRef{number: task.number, attempt: rec.Attempt}
	d.recoverAudit(task, auditActionClaim, wantFrom, taskStatusRunning, rec.Attempt, claimedAt, "")
	return nil
}

func applyTaskReportRecord(s *Store, payload []byte) error {
	var rec walTaskReport
	if err := json.Unmarshal(payload, &rec); err != nil {
		return fmt.Errorf("invalid task report record: %w", err)
	}
	state, ok := s.devices[rec.DeviceID]
	if !ok {
		return fmt.Errorf("task report for unknown device %q", rec.DeviceID)
	}
	d := state.ensureDiagnostics()
	task, err := d.taskByNumber(rec.Number)
	if err != nil {
		return fmt.Errorf("device %q report for %w", rec.DeviceID, err)
	}
	if strings.TrimSpace(rec.ReceiptID) == "" || rec.At.IsZero() {
		return fmt.Errorf("device %q task %d report is incomplete", rec.DeviceID, rec.Number)
	}
	if rec.Attempt < 1 || rec.Attempt > len(task.history) {
		return fmt.Errorf("device %q task %d report for unknown attempt %d",
			rec.DeviceID, rec.Number, rec.Attempt)
	}
	attempt := task.history[rec.Attempt-1]
	if rec.Token != attempt.token {
		return fmt.Errorf("device %q task %d report token disagrees with claim %d",
			rec.DeviceID, rec.Number, rec.Attempt)
	}
	if _, dup := task.reports[rec.ReceiptID]; dup {
		return fmt.Errorf("device %q task %d duplicate receipt %q", rec.DeviceID, rec.Number, rec.ReceiptID)
	}
	if task.status != taskStatusRunning || d.active != task.number {
		return fmt.Errorf("device %q task %d reported while not running", rec.DeviceID, rec.Number)
	}
	if !rec.At.Before(attempt.deadline) {
		return fmt.Errorf("device %q task %d report at %s is past deadline %s",
			rec.DeviceID, rec.Number, rec.At, attempt.deadline)
	}

	at := rec.At.UTC()
	ack := TaskReportAck{
		Number: task.number, Attempt: rec.Attempt, ReceiptID: rec.ReceiptID,
		Attempts: task.attempts, ReceivedAt: at,
	}
	from := task.status
	if rec.Success {
		if !isNonEmptyJSONObject(rec.Result) {
			return fmt.Errorf("device %q task %d success report lacks a JSON object result", rec.DeviceID, rec.Number)
		}
		task.status = taskStatusSucceeded
		task.result = cloneJSON(rec.Result)
		task.reason = ""
		finished := at
		task.finishedAt = &finished
		ack.Status = taskStatusSucceeded
		ack.Result = cloneJSON(rec.Result)
		d.recoverAudit(task, auditActionSuccess, from, taskStatusSucceeded, rec.Attempt, at, "")
	} else {
		if strings.TrimSpace(rec.Reason) == "" {
			return fmt.Errorf("device %q task %d failure report lacks a reason", rec.DeviceID, rec.Number)
		}
		if rec.Attempt >= maxTaskAttempts {
			task.status = taskStatusFailed
			task.reason = rec.Reason
			finished := at
			task.finishedAt = &finished
			ack.Status = taskStatusFailed
			ack.Reason = rec.Reason
		} else {
			task.status = taskStatusWaiting
			task.nextClaim = at.Add(retryDelays[rec.Attempt])
			task.deadline = nil
			next := task.nextClaim.UTC()
			ack.Status = taskStatusWaiting
			ack.Reason = rec.Reason
			ack.NextClaimableAt = &next
		}
		d.recoverAudit(task, auditActionFail, from, task.status, rec.Attempt, at, rec.Reason)
	}
	d.active = 0
	task.reports[rec.ReceiptID] = storedReport{
		receiptID: rec.ReceiptID,
		success:   rec.Success,
		reason:    rec.Reason,
		result:    cloneJSON(rec.Result),
		ack:       ack,
	}
	return nil
}

func applyTaskCancelRecord(s *Store, payload []byte) error {
	var rec walTaskCancel
	if err := json.Unmarshal(payload, &rec); err != nil {
		return fmt.Errorf("invalid task cancel record: %w", err)
	}
	state, ok := s.devices[rec.DeviceID]
	if !ok {
		return fmt.Errorf("task cancel for unknown device %q", rec.DeviceID)
	}
	d := state.ensureDiagnostics()
	for i := range rec.Settles {
		if err := applyTimeoutTransition(d, &rec.Settles[i]); err != nil {
			return fmt.Errorf("device %q cancel record: %w", rec.DeviceID, err)
		}
	}
	task, err := d.taskByNumber(rec.Number)
	if err != nil {
		return fmt.Errorf("device %q cancel for %w", rec.DeviceID, err)
	}
	if rec.CancelledAt.IsZero() {
		return fmt.Errorf("device %q task %d cancel has no time", rec.DeviceID, rec.Number)
	}
	switch rec.FromStatus {
	case taskStatusPending, taskStatusWaiting, taskStatusRunning:
	default:
		return fmt.Errorf("device %q task %d cancel from invalid status %q",
			rec.DeviceID, rec.Number, rec.FromStatus)
	}
	if task.status != rec.FromStatus {
		return fmt.Errorf("device %q task %d cancel from %q disagrees with status %q",
			rec.DeviceID, rec.Number, rec.FromStatus, task.status)
	}
	if d.active == task.number && rec.FromStatus != taskStatusRunning {
		return fmt.Errorf("device %q task %d active but recorded as %q at cancel",
			rec.DeviceID, rec.Number, rec.FromStatus)
	}

	at := rec.CancelledAt.UTC()
	task.status = taskStatusCancelled
	task.nextClaim = time.Time{}
	task.deadline = nil
	task.cancelledAt = &at
	task.finishedAt = &at
	if d.active == task.number {
		d.active = 0
	}
	d.recoverAudit(task, auditActionCancel, rec.FromStatus, taskStatusCancelled, rec.Attempt, at, "")
	return nil
}

func applyTaskTransitionsRecord(s *Store, payload []byte) error {
	var rec walTaskTransitions
	if err := json.Unmarshal(payload, &rec); err != nil {
		return fmt.Errorf("invalid task transitions record: %w", err)
	}
	state, ok := s.devices[rec.DeviceID]
	if !ok {
		return fmt.Errorf("task transitions for unknown device %q", rec.DeviceID)
	}
	if len(rec.Transitions) == 0 {
		return fmt.Errorf("device %q empty task transitions record", rec.DeviceID)
	}
	d := state.ensureDiagnostics()
	for i := range rec.Transitions {
		if err := applyTimeoutTransition(d, &rec.Transitions[i]); err != nil {
			return fmt.Errorf("device %q transitions record: %w", rec.DeviceID, err)
		}
	}
	return nil
}

// applyTimeoutTransition rebuilds one deadline-timeout transition produced
// lazily by a read, an empty claim or embedded in a claim/cancel record.
func applyTimeoutTransition(d *diagState, tr *walTaskTransition) error {
	if tr.Action != auditActionTimeout {
		return fmt.Errorf("transition for task %d has unexpected action %q", tr.Number, tr.Action)
	}
	if strings.TrimSpace(tr.Reason) == "" || tr.At.IsZero() {
		return errors.New("timeout transition is incomplete")
	}
	task, err := d.taskByNumber(tr.Number)
	if err != nil {
		return err
	}
	if tr.Attempt < 1 || tr.Attempt > len(task.history) || tr.Attempt > maxTaskAttempts {
		return fmt.Errorf("task %d timeout for unknown attempt %d", tr.Number, tr.Attempt)
	}
	attempt := task.history[tr.Attempt-1]
	if tr.From != taskStatusRunning || task.status != taskStatusRunning {
		return fmt.Errorf("task %d timeout from %q while status is %q",
			tr.Number, tr.From, task.status)
	}
	if d.active != task.number {
		return fmt.Errorf("task %d timed out without holding the device slot", tr.Number)
	}
	if tr.At.Before(attempt.deadline) {
		return fmt.Errorf("task %d timeout at %s precedes deadline %s", tr.Number, tr.At, attempt.deadline)
	}

	at := tr.At.UTC()
	switch tr.To {
	case taskStatusWaiting:
		if tr.Attempt >= maxTaskAttempts {
			return fmt.Errorf("task %d timeout waits after the final attempt", tr.Number)
		}
		task.status = taskStatusWaiting
		task.nextClaim = at.Add(retryDelays[tr.Attempt])
		task.deadline = nil
	case taskStatusFailed:
		if tr.Attempt != maxTaskAttempts {
			return fmt.Errorf("task %d timeout fails on attempt %d, want %d",
				tr.Number, tr.Attempt, maxTaskAttempts)
		}
		task.status = taskStatusFailed
		task.reason = tr.Reason
		finished := at
		task.finishedAt = &finished
		task.nextClaim = time.Time{}
		task.deadline = nil
	default:
		return fmt.Errorf("task %d timeout ends in invalid status %q", tr.Number, tr.To)
	}
	d.active = 0
	d.recoverAudit(task, auditActionTimeout, tr.From, tr.To, tr.Attempt, at, tr.Reason)
	return nil
}

// recoverAudit appends an audit row during WAL replay. Rows are rebuilt in
// committed order, so seq values match the original run exactly.
func (d *diagState) recoverAudit(task *diagTask, action, from, to string, attempt int, at time.Time, reason string) {
	d.audit = append(d.audit, AuditEntry{
		Seq:        len(d.audit) + 1,
		Task:       task.number,
		Action:     action,
		FromStatus: from,
		ToStatus:   to,
		Attempt:    attempt,
		At:         at.UTC(),
		Reason:     reason,
	})
}
