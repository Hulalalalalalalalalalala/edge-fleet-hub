package fleet

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Persistent mode stores every acknowledged mutation as one checksummed record
// in an append-only write-ahead log. A record is the commit unit: either the
// whole record is durable (and visible after recovery) or none of it is, so a
// replay batch can never leave half of its samples behind.
//
// Commit protocol per write (WAL frame and commit journal are separate files so
// the commit decision never depends on parsing a possibly torn frame length):
//
//  1. write the frame at the committed offset and fsync the WAL;
//  2. append a fixed-size entry carrying the new committed WAL length to the
//     commit journal and fsync it;
//  3. only then report success.
//
// Recovery reads the committed length T from the journal, verifies that WAL
// bytes [header, T) form an exact chain of valid frames, applies them, and
// treats bytes (T, EOF] as the residue of an unacknowledged write. Damage to
// committed bytes always fails startup instead of being trimmed away.

const (
	walName     = "wal.log"
	commitName  = "commit.log"
	lockName    = "lock"
	walMagic    = "EDGEFLET" // 8 bytes
	headerLen   = 16         // magic(8) + version(4) + flags(4)
	walVersion  = 1
	commitEntry = 12 // [committed WAL length uint64 LE][crc32 LE]

	// maxRecordPayload bounds a single frame; HTTP bodies are capped at 1MiB so
	// this only rejects absurd length fields from damaged files.
	maxRecordPayload = 8 << 20
)

const (
	recInstance byte = 1 + iota
	recRegister
	recTelemetry
	recReplay
	recRule
	recAlertAck
	recConfigPublish
	recConfigReceipt
	recTaskCreate
	recTaskClaim
	recTaskReport
	recTaskTimeout
	recTaskCancel
	recRetention
)

// ErrStorageUnavailable wraps a durable-write failure at runtime. A request
// that fails this way never mutated queryable state.
var ErrStorageUnavailable = errors.New("storage unavailable")

// walTime is a timestamp field on the wire that must distinguish a present,
// valid time from an absent, null or malformed one. The distinction matters
// because the zero time.Time is itself the valid RFC3339 instant
// 0001-01-01T00:00:00Z: an observedAt legitimately carrying that instant must
// survive recovery, while IsZero() alone cannot tell it apart from a field
// that was dropped. Presence is tracked explicitly instead.
type walTime struct {
	time.Time
	present bool
}

func newWalTime(t time.Time) walTime { return walTime{Time: t.UTC(), present: true} }

// present reports whether the field was present and parsed as a time.
func (t walTime) ok() bool { return t.present }

func (t walTime) MarshalJSON() ([]byte, error) {
	if !t.present {
		return []byte("null"), nil
	}
	return json.Marshal(t.Time)
}

func (t *walTime) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		t.Time, t.present = time.Time{}, false
		return nil
	}
	var parsed time.Time
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	t.Time = parsed.UTC()
	t.present = true
	return nil
}

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// walSink is the commit log behind a persistent store.
type walSink interface {
	appendRecord(recType byte, value any) error
}

type walInstance struct {
	InstanceID []byte `json:"instanceId"`
}

type walRegister struct {
	ID           string    `json:"id"`
	Site         string    `json:"site"`
	RegisteredAt time.Time `json:"registeredAt"`
	LastSeenAt   time.Time `json:"lastSeenAt"`
}

type walTelemetry struct {
	DeviceID   string             `json:"deviceId"`
	Sequence   int64              `json:"sequence"`
	ObservedAt walTime            `json:"observedAt"`
	Values     map[string]float64 `json:"values"`
	// TrimThrough is the cumulative count of oldest events removed by the
	// retention limit once this sample is committed. Absent/zero in records
	// written before retention existed, which never trimmed anything.
	TrimThrough int64 `json:"trimThrough,omitempty"`
	// Alerts and Ended carry the alert changes judged from this sample, in the
	// same commit unit as the sample itself. Absent in records written before
	// alerting existed.
	Alerts []*walAlert `json:"alerts,omitempty"`
	Ended  []*walAlert `json:"ended,omitempty"`
}

type walSample struct {
	EventID    string             `json:"eventId"`
	ObservedAt walTime            `json:"observedAt"`
	Values     map[string]float64 `json:"values"`
}

// walAlert is the on-disk form of an Alert change embedded in telemetry and
// replay records. Its trigger and recovery observed times are presence-aware so
// the valid instant 0001-01-01T00:00:00Z round-trips instead of looking like a
// dropped field; a genuinely missing or null time still fails recovery.
type walAlert struct {
	ID                int64      `json:"id"`
	RuleID            string     `json:"ruleId"`
	RuleVersion       int64      `json:"ruleVersion"`
	Metric            string     `json:"metric"`
	Trigger           float64    `json:"trigger"`
	Recover           float64    `json:"recover"`
	Status            string     `json:"status"`
	TriggerSequence   int64      `json:"triggerSequence"`
	TriggerValue      float64    `json:"triggerValue"`
	TriggerObservedAt walTime    `json:"triggerObservedAt"`
	RecoverSequence   *int64     `json:"recoverSequence,omitempty"`
	RecoverValue      *float64   `json:"recoverValue,omitempty"`
	RecoverObservedAt *walTime   `json:"recoverObservedAt,omitempty"`
	EndedAt           *time.Time `json:"endedAt,omitempty"`
	EndReason         string     `json:"endReason,omitempty"`
	AcknowledgedAt    *time.Time `json:"acknowledgedAt,omitempty"`
}

// toWALAlert projects an in-memory alert into its on-disk form.
func toWALAlert(a *Alert) *walAlert {
	w := &walAlert{
		ID:                a.ID,
		RuleID:            a.RuleID,
		RuleVersion:       a.RuleVersion,
		Metric:            a.Metric,
		Trigger:           a.Trigger,
		Recover:           a.Recover,
		Status:            a.Status,
		TriggerSequence:   a.TriggerSequence,
		TriggerValue:      a.TriggerValue,
		TriggerObservedAt: newWalTime(a.TriggerObservedAt),
		RecoverSequence:   a.RecoverSequence,
		RecoverValue:      a.RecoverValue,
		EndedAt:           a.EndedAt,
		EndReason:         a.EndReason,
		AcknowledgedAt:    a.AcknowledgedAt,
	}
	if a.RecoverObservedAt != nil {
		t := newWalTime(*a.RecoverObservedAt)
		w.RecoverObservedAt = &t
	}
	return w
}

// toWALAlerts projects a slice of in-memory alerts, preserving nil so absent
// alert sections stay absent on the wire.
func toWALAlerts(alerts []*Alert) []*walAlert {
	if alerts == nil {
		return nil
	}
	out := make([]*walAlert, len(alerts))
	for i, a := range alerts {
		out[i] = toWALAlert(a)
	}
	return out
}

// toAlert projects a recovered on-disk alert back into memory. It does not
// validate; callers run applyAlertRecord first.
func (w *walAlert) toAlert() *Alert {
	a := &Alert{
		ID:                w.ID,
		RuleID:            w.RuleID,
		RuleVersion:       w.RuleVersion,
		Metric:            w.Metric,
		Trigger:           w.Trigger,
		Recover:           w.Recover,
		Status:            w.Status,
		TriggerSequence:   w.TriggerSequence,
		TriggerValue:      w.TriggerValue,
		TriggerObservedAt: w.TriggerObservedAt.Time,
		RecoverSequence:   w.RecoverSequence,
		RecoverValue:      w.RecoverValue,
		EndedAt:           w.EndedAt,
		EndReason:         w.EndReason,
		AcknowledgedAt:    w.AcknowledgedAt,
	}
	if w.RecoverObservedAt != nil {
		t := w.RecoverObservedAt.Time
		a.RecoverObservedAt = &t
	}
	return a
}

// walReplay is one committed batch. Samples holds the full ordered batch
// (duplicates included) and Receipt the first-acceptance receipt; new events
// and their sequences are derived by zipping the two during recovery. Alerts
// and Ended carry the alert changes judged from the batch's new samples, in
// the same commit unit as the batch itself.
type walReplay struct {
	DeviceID      string             `json:"deviceId"`
	BatchID       string             `json:"batchId"`
	Samples       []walSample        `json:"samples"`
	Receipt       ReplayReceipt      `json:"receipt"`
	TrimThrough   int64              `json:"trimThrough,omitempty"`
	LastSeenAt    *time.Time         `json:"lastSeenAt,omitempty"`
	LastTelemetry map[string]float64 `json:"lastTelemetry,omitempty"`
	Alerts        []*walAlert        `json:"alerts,omitempty"`
	Ended         []*walAlert        `json:"ended,omitempty"`
}

// walRetention is one committed retention setting. TrimThrough is the
// cumulative number of oldest events that had to be removed to satisfy the
// new limit at commit time; it can only grow.
type walRetention struct {
	DeviceID    string `json:"deviceId"`
	MaxEvents   int64  `json:"maxEvents"`
	TrimThrough int64  `json:"trimThrough,omitempty"`
}

// walRule is one committed rule create or update. Ended holds the alerts the
// update terminated with reason rule_changed, in the same commit unit.
type walRule struct {
	DeviceID string   `json:"deviceId"`
	Rule     Rule     `json:"rule"`
	Ended    []*Alert `json:"ended,omitempty"`
}

// walAlertAck is one committed alert acknowledgment.
type walAlertAck struct {
	DeviceID       string    `json:"deviceId"`
	AlertID        int64     `json:"alertId"`
	AcknowledgedAt time.Time `json:"acknowledgedAt"`
}

// walConfigPublish is one committed configuration version together with the
// requestId dedup record of its first publish.
type walConfigPublish struct {
	DeviceID    string          `json:"deviceId"`
	RequestID   string          `json:"requestId"`
	BaseVersion int64           `json:"baseVersion"`
	Version     int64           `json:"version"`
	Content     json.RawMessage `json:"content"`
	PublishedAt time.Time       `json:"publishedAt"`
}

// walConfigReceipt is one committed device application receipt.
type walConfigReceipt struct {
	DeviceID   string    `json:"deviceId"`
	ReceiptID  string    `json:"receiptId"`
	Version    int64     `json:"version"`
	Success    bool      `json:"success"`
	Reason     string    `json:"reason,omitempty"`
	ReceivedAt time.Time `json:"receivedAt"`
}

// walTaskCreate is one committed task creation together with its audit record.
type walTaskCreate struct {
	DeviceID        string          `json:"deviceId"`
	TaskID          int64           `json:"taskId"`
	RequestID       string          `json:"requestId"`
	DurationSeconds int             `json:"durationSeconds"`
	CreatedAt       time.Time       `json:"createdAt"`
	NextClaimableAt time.Time       `json:"nextClaimableAt"`
	Audit           TaskAuditRecord `json:"audit"`
}

// walTaskClaim is one committed task claim together with its audit record.
type walTaskClaim struct {
	DeviceID   string          `json:"deviceId"`
	TaskID     int64           `json:"taskId"`
	Attempt    int             `json:"attempt"`
	Credential string          `json:"credential"`
	ClaimedAt  time.Time       `json:"claimedAt"`
	Deadline   time.Time       `json:"deadline"`
	Audit      TaskAuditRecord `json:"audit"`
}

// walTaskReport is one committed device outcome together with the resulting
// task state and audit record.
type walTaskReport struct {
	DeviceID        string          `json:"deviceId"`
	TaskID          int64           `json:"taskId"`
	ReceiptID       string          `json:"receiptId"`
	Success         bool            `json:"success"`
	Reason          string          `json:"reason,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	ReceivedAt      time.Time       `json:"receivedAt"`
	Status          string          `json:"status"`
	Failures        int             `json:"failures"`
	NextClaimableAt time.Time       `json:"nextClaimableAt,omitempty"`
	FailureReason   string          `json:"failureReason,omitempty"`
	CompletedAt     *time.Time      `json:"completedAt,omitempty"`
	Audit           TaskAuditRecord `json:"audit"`
}

// walTaskTimeout is one committed deadline expiry together with the resulting
// task state and audit record.
type walTaskTimeout struct {
	DeviceID        string          `json:"deviceId"`
	TaskID          int64           `json:"taskId"`
	Attempt         int             `json:"attempt"`
	Deadline        time.Time       `json:"deadline"`
	Status          string          `json:"status"`
	Failures        int             `json:"failures"`
	NextClaimableAt time.Time       `json:"nextClaimableAt,omitempty"`
	FailureReason   string          `json:"failureReason,omitempty"`
	CompletedAt     *time.Time      `json:"completedAt,omitempty"`
	Audit           TaskAuditRecord `json:"audit"`
}

// walTaskCancel is one committed task cancellation together with its audit
// record.
type walTaskCancel struct {
	DeviceID   string          `json:"deviceId"`
	TaskID     int64           `json:"taskId"`
	CanceledAt time.Time       `json:"canceledAt"`
	Audit      TaskAuditRecord `json:"audit"`
}

// walFile is the durable backend. walDurable is the committed WAL length (also
// the next frame offset); commitDurable is the journal length. Both files are
// truncated back to those offsets before every write, so residue left by a
// failed write can never be mistaken for later committed data.
type walFile struct {
	walF          *os.File
	commitF       *os.File
	lock          *os.File
	mu            sync.Mutex
	walDurable    int64
	commitDurable int64
}

func (w *walFile) appendRecord(recType byte, value any) error {
	frame, err := marshalFrame(recType, value)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	// Remove residue any previous failed attempt may have left, then phase 1.
	if err := w.walF.Truncate(w.walDurable); err != nil {
		return err
	}
	if err := pwriteAll(w.walF, frame, w.walDurable); err != nil {
		return err
	}
	if err := w.walF.Sync(); err != nil {
		return err
	}

	// Phase 2: publish the new committed length in the fixed-width journal.
	newLength := w.walDurable + int64(len(frame))
	entry := make([]byte, commitEntry)
	binary.LittleEndian.PutUint64(entry[0:8], uint64(newLength))
	binary.LittleEndian.PutUint32(entry[8:12], commitCRC(entry[0:8]))
	if err := w.commitF.Truncate(w.commitDurable); err != nil {
		return err
	}
	if err := pwriteAll(w.commitF, entry, w.commitDurable); err != nil {
		return err
	}
	if err := w.commitF.Sync(); err != nil {
		return err
	}

	w.walDurable = newLength
	w.commitDurable += int64(commitEntry)
	return nil
}

func commitCRC(sizeBytes []byte) uint32 {
	return crc32.Checksum(sizeBytes, crcTable)
}

func pwriteAll(f *os.File, buf []byte, offset int64) error {
	for len(buf) > 0 {
		n, err := f.WriteAt(buf, offset)
		if err != nil {
			return err
		}
		buf = buf[n:]
		offset += int64(n)
	}
	return nil
}

func (w *walFile) Close() error {
	walErr := w.walF.Close()
	commitErr := w.commitF.Close()
	// Closing the lock fd releases the flock; a killed process releases it via
	// the kernel, so no manual lock cleanup is ever required.
	lockErr := w.lock.Close()
	if walErr != nil {
		return walErr
	}
	if commitErr != nil {
		return commitErr
	}
	return lockErr
}

// NewPersistentStore opens (or creates) a data directory and finishes full
// recovery before returning. It fails rather than falling back to memory when
// the directory cannot be created, locked, read or written, when another
// process already holds the directory, or when committed data is corrupt or
// in an unsupported format.
func NewPersistentStore(dir string) (_ *Store, err error) {
	if fi, statErr := os.Stat(dir); statErr == nil {
		if !fi.IsDir() {
			return nil, fmt.Errorf("data path %s is not a directory", dir)
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("cannot access data directory %s: %w", dir, statErr)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create data directory %s: %w", dir, err)
	}

	// Take the inter-process lock before reading or writing any business data.
	lockPath := filepath.Join(dir, lockName)
	lockFile, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cannot open lock file %s: %w", lockPath, err)
	}
	if lockErr := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lockErr != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("data directory %s is already in use by another edge-fleet process: %w", dir, lockErr)
	}
	handedOff := false
	defer func() {
		if !handedOff {
			_ = lockFile.Close()
		}
	}()

	walPath := filepath.Join(dir, walName)
	commitPath := filepath.Join(dir, commitName)
	walF, err := os.OpenFile(walPath, os.O_RDWR|os.O_CREATE|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cannot open write-ahead log %s: %w", walPath, err)
	}
	commitF, err := os.OpenFile(commitPath, os.O_RDWR|os.O_CREATE|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		_ = walF.Close()
		return nil, fmt.Errorf("cannot open commit journal %s: %w", commitPath, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = walF.Close()
			_ = commitF.Close()
		}
	}()

	walInfo, err := walF.Stat()
	if err != nil {
		return nil, fmt.Errorf("cannot stat write-ahead log %s: %w", walPath, err)
	}
	if walInfo.Size() == 0 {
		if _, err := walF.WriteAt(walHeader(), 0); err != nil {
			return nil, fmt.Errorf("cannot initialise write-ahead log %s: %w", walPath, err)
		}
		if err := walF.Sync(); err != nil {
			return nil, fmt.Errorf("cannot flush write-ahead log %s: %w", walPath, err)
		}
	}

	walData, err := io.ReadAll(walF)
	if err != nil {
		return nil, fmt.Errorf("cannot read write-ahead log %s: %w", walPath, err)
	}
	commitData, err := io.ReadAll(commitF)
	if err != nil {
		return nil, fmt.Errorf("cannot read commit journal %s: %w", commitPath, err)
	}
	if len(walData) < headerLen || string(walData[0:8]) != walMagic {
		return nil, fmt.Errorf("write-ahead log %s is corrupt: bad magic header", walPath)
	}
	version := binary.LittleEndian.Uint32(walData[8:12])
	if version != walVersion {
		return nil, fmt.Errorf("write-ahead log %s uses unsupported format version %d (this server supports %d)", walPath, version, walVersion)
	}
	if binary.LittleEndian.Uint32(walData[12:16]) != 0 {
		return nil, fmt.Errorf("write-ahead log %s is corrupt: bad header flags", walPath)
	}

	// Recover the committed WAL length from the fixed-width journal. A short
	// final block is a torn append from a write that never reported success;
	// a full block with a bad CRC is committed-data damage. Entries must chain
	// strictly forward and never point past the WAL. For a fresh journal the
	// committed prefix is the header itself.
	walDurable, commitDurable := int64(headerLen), int64(0)
	for pos := 0; pos < len(commitData); pos += commitEntry {
		if len(commitData)-pos < commitEntry {
			break // torn tail: discarded by the truncate below
		}
		block := commitData[pos : pos+commitEntry]
		declared := int64(binary.LittleEndian.Uint64(block[0:8]))
		if binary.LittleEndian.Uint32(block[8:12]) != commitCRC(block[0:8]) {
			return nil, fmt.Errorf("commit journal %s is corrupt at byte %d: bad checksum (files left untouched)", commitPath, pos)
		}
		if declared <= walDurable || declared > int64(len(walData)) || declared < headerLen {
			return nil, fmt.Errorf("commit journal %s is corrupt at byte %d: committed length %d is inconsistent", commitPath, pos, declared)
		}
		walDurable = declared
		commitDurable = int64(pos + commitEntry)
	}

	// Every byte the journal claims committed must form an exact chain of
	// valid, in-range frames. Failure here is damage to acknowledged data.
	type parsedFrame struct {
		recType byte
		payload []byte
	}
	frames := make([]parsedFrame, 0)
	pos := headerLen
	for int64(pos) < walDurable {
		if pos+5 > len(walData) {
			return nil, fmt.Errorf("write-ahead log %s is corrupt at byte %d: truncated frame inside committed prefix", walPath, pos)
		}
		length := int(binary.LittleEndian.Uint32(walData[pos : pos+4]))
		frameLen := 5 + length + 4
		if length > maxRecordPayload || int64(pos+frameLen) > walDurable {
			return nil, fmt.Errorf("write-ahead log %s is corrupt at byte %d: frame length %d is invalid within the committed prefix", walPath, pos, length)
		}
		recType := walData[pos+4]
		payload := walData[pos+5 : pos+5+length]
		wantCRC := binary.LittleEndian.Uint32(walData[pos+5+length : pos+frameLen])
		if crc32.Checksum(walData[pos:pos+5+length], crcTable) != wantCRC {
			return nil, fmt.Errorf("write-ahead log %s is corrupt at byte %d: frame failed its integrity check (file left untouched)", walPath, pos)
		}
		if recType < recInstance || recType > recRetention {
			return nil, fmt.Errorf("write-ahead log %s uses unsupported record type %d at byte %d", walPath, recType, pos)
		}
		frames = append(frames, parsedFrame{recType: recType, payload: payload})
		pos += frameLen
	}
	if int64(pos) != walDurable {
		return nil, fmt.Errorf("write-ahead log %s is corrupt: committed length %d is not a frame boundary", walPath, walDurable)
	}

	// Rebuild relational state from the committed frames, in order.
	store := NewStore()
	var instanceID []byte
	sawInstance := false
	for i, frame := range frames {
		switch frame.recType {
		case recInstance:
			if sawInstance || i != 0 {
				return nil, fmt.Errorf("write-ahead log %s is corrupt: instance marker out of place", walPath)
			}
			var rec walInstance
			if err := json.Unmarshal(frame.payload, &rec); err != nil || len(rec.InstanceID) == 0 || len(rec.InstanceID) > 64 {
				return nil, fmt.Errorf("write-ahead log %s is corrupt: invalid instance marker: %w", walPath, err)
			}
			instanceID = rec.InstanceID
			sawInstance = true
		case recRegister, recTelemetry, recReplay, recRule, recAlertAck, recConfigPublish, recConfigReceipt,
			recTaskCreate, recTaskClaim, recTaskReport, recTaskTimeout, recTaskCancel, recRetention:
			if !sawInstance {
				return nil, fmt.Errorf("write-ahead log %s is corrupt: data record precedes instance marker", walPath)
			}
			if err := applyRecord(store, frame.recType, frame.payload); err != nil {
				return nil, fmt.Errorf("write-ahead log %s is corrupt: %w", walPath, err)
			}
		}
	}

	// Discard unacknowledged residue: bytes from a request that never reached
	// its journal entry (crash mid-write or a request that returned 503).
	if int64(len(walData)) != walDurable {
		if err := walF.Truncate(walDurable); err != nil {
			return nil, fmt.Errorf("cannot discard unacknowledged tail of %s: %w", walPath, err)
		}
		if err := walF.Sync(); err != nil {
			return nil, fmt.Errorf("cannot flush truncated %s: %w", walPath, err)
		}
	}
	if int64(len(commitData)) != commitDurable {
		if err := commitF.Truncate(commitDurable); err != nil {
			return nil, fmt.Errorf("cannot discard torn entry of %s: %w", commitPath, err)
		}
		if err := commitF.Sync(); err != nil {
			return nil, fmt.Errorf("cannot flush truncated %s: %w", commitPath, err)
		}
	}

	wal := &walFile{
		walF:          walF,
		commitF:       commitF,
		lock:          lockFile,
		walDurable:    walDurable,
		commitDurable: commitDurable,
	}
	if !sawInstance {
		instanceID = newInstanceID()
		if err := wal.appendRecord(recInstance, walInstance{InstanceID: instanceID}); err != nil {
			return nil, fmt.Errorf("cannot write instance marker to %s: %w", walPath, err)
		}
	}
	// Make freshly created directory entries durable as well.
	if dirFile, err := os.Open(dir); err != nil {
		return nil, fmt.Errorf("cannot open data directory %s for sync: %w", dir, err)
	} else {
		syncErr := syscall.Fsync(int(dirFile.Fd()))
		_ = dirFile.Close()
		if syncErr != nil {
			return nil, fmt.Errorf("cannot flush data directory %s: %w", dir, syncErr)
		}
	}

	store.wal = wal
	store.iid = instanceID
	committed = true
	handedOff = true
	return store, nil
}

// Close releases the WAL files and the directory lock.
func (s *Store) Close() error {
	if closer, ok := s.wal.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

func storageUnavailable(err error) error {
	return fmt.Errorf("%w: %v", ErrStorageUnavailable, err)
}

func walHeader() []byte {
	header := make([]byte, headerLen)
	copy(header[0:8], walMagic)
	binary.LittleEndian.PutUint32(header[8:12], walVersion)
	return header
}

// marshalFrame frames one record as
// [length uint32 LE][type byte][payload][crc32 LE].
// The CRC covers length, type and payload, so even a damaged length field of
// the final frame fails verification instead of masquerading as a torn tail.
func marshalFrame(recType byte, value any) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(payload) > maxRecordPayload {
		return nil, fmt.Errorf("wal record too large: %d bytes", len(payload))
	}
	frame := make([]byte, 5+len(payload)+4)
	binary.LittleEndian.PutUint32(frame[0:4], uint32(len(payload)))
	frame[4] = recType
	copy(frame[5:], payload)
	binary.LittleEndian.PutUint32(frame[5+len(payload):], crc32.Checksum(frame[:5+len(payload)], crcTable))
	return frame, nil
}

// validateTrim checks a record's cumulative trim pointer against the state
// during recovery. It must never move backwards, never pass the record's own
// high-water sequence, and it must equal exactly what the live limit would
// have produced, so a damaged or hand-edited WAL fails startup.
func validateTrim(state *deviceState, trimThrough, recordMaxSeq, limit int64) error {
	if trimThrough < state.eventBase {
		return fmt.Errorf("device %q retention trim pointer went backwards: %d < %d",
			state.device.ID, trimThrough, state.eventBase)
	}
	if trimThrough > recordMaxSeq {
		return fmt.Errorf("device %q retention trim pointer %d past sequence %d",
			state.device.ID, trimThrough, recordMaxSeq)
	}
	if limit < 0 || limit > maxRetentionEvents {
		return fmt.Errorf("device %q retention limit %d out of range", state.device.ID, limit)
	}
	if want := planTrimThrough(state.eventBase, recordMaxSeq, limit); trimThrough != want {
		return fmt.Errorf("device %q retention trim pointer %d is inconsistent (want %d for limit %d)",
			state.device.ID, trimThrough, want, limit)
	}
	return nil
}

// applyAlertRecord validates and applies the alert changes carried by a
// telemetry or replay record. It rejects relational inconsistency so a damaged
// WAL fails startup rather than producing broken alert state. Timestamp
// presence is checked, not the zero instant: a trigger or recovery observed at
// 0001-01-01T00:00:00Z is complete, while a missing or null time field is not.
func applyAlertRecord(state *deviceState, created, ended []*walAlert) error {
	wantID := int64(len(state.alerts)) + 1
	for _, alert := range created {
		if alert == nil {
			return errors.New("alert record contains a null create")
		}
		if alert.ID != wantID {
			return fmt.Errorf("alert create id mismatch: got %d, want %d", alert.ID, wantID)
		}
		if alert.Status != alertStatusActive || alert.RuleID == "" || alert.Metric == "" ||
			alert.TriggerSequence <= 0 || !alert.TriggerObservedAt.ok() {
			return fmt.Errorf("alert %d create is incomplete", alert.ID)
		}
		wantID++
	}
	maxID := int64(len(state.alerts) + len(created))
	for _, alert := range ended {
		if alert == nil {
			return errors.New("alert record contains a null end")
		}
		if alert.ID < 1 || alert.ID > maxID {
			return fmt.Errorf("alert end id %d out of range (1..%d)", alert.ID, maxID)
		}
		if alert.Status != alertStatusEnded || alert.EndReason != endReasonRecovered {
			return fmt.Errorf("alert %d end is malformed", alert.ID)
		}
		if alert.RecoverSequence == nil || alert.RecoverValue == nil ||
			alert.RecoverObservedAt == nil || !alert.RecoverObservedAt.ok() {
			return fmt.Errorf("alert %d recovery evidence is incomplete", alert.ID)
		}
	}
	createdAlerts := make([]*Alert, len(created))
	for i, a := range created {
		createdAlerts[i] = a.toAlert()
	}
	endedAlerts := make([]*Alert, len(ended))
	for i, a := range ended {
		endedAlerts[i] = a.toAlert()
	}
	applyAlertChanges(state, createdAlerts, endedAlerts)
	return nil
}

// applyRecord rebuilds in-memory state from one committed record, rejecting
// any structural or relational inconsistency. It never calls the clock, so
// recovery cannot refresh device timestamps.
func applyRecord(s *Store, recType byte, payload []byte) error {
	switch recType {
	case recRegister:
		var rec walRegister
		if err := json.Unmarshal(payload, &rec); err != nil {
			return fmt.Errorf("invalid register record: %w", err)
		}
		if rec.ID == "" || rec.RegisteredAt.IsZero() || rec.LastSeenAt.IsZero() {
			return errors.New("register record missing fields")
		}
		if _, exists := s.devices[rec.ID]; exists {
			return fmt.Errorf("duplicate registration for device %q", rec.ID)
		}
		s.devices[rec.ID] = &deviceState{
			device: Device{
				ID:           rec.ID,
				Site:         rec.Site,
				RegisteredAt: rec.RegisteredAt.UTC(),
				LastSeenAt:   rec.LastSeenAt.UTC(),
			},
			known:   make(map[string]knownSample),
			batches: make(map[string]storedBatch),
			rules:   make(map[string]*ruleState),
		}
		return nil

	case recTelemetry:
		var rec walTelemetry
		if err := json.Unmarshal(payload, &rec); err != nil {
			return fmt.Errorf("invalid telemetry record: %w", err)
		}
		state, ok := s.devices[rec.DeviceID]
		if !ok {
			return fmt.Errorf("telemetry for unknown device %q", rec.DeviceID)
		}
		if rec.Sequence != state.maxSequence()+1 {
			return fmt.Errorf("device %q telemetry sequence gap: got %d, want %d", rec.DeviceID, rec.Sequence, state.maxSequence()+1)
		}
		if !rec.ObservedAt.ok() || len(rec.Values) == 0 {
			return fmt.Errorf("device %q telemetry record missing fields", rec.DeviceID)
		}
		if err := validateTrim(state, rec.TrimThrough, rec.Sequence, state.maxEvents); err != nil {
			return err
		}
		state.events = append(state.events, Event{
			Sequence:   rec.Sequence,
			ObservedAt: rec.ObservedAt.Time,
			Values:     cloneTelemetry(rec.Values),
		})
		state.device.LastSeenAt = rec.ObservedAt.Time
		state.device.LastTelemetry = cloneTelemetry(rec.Values)
		if err := applyAlertRecord(state, rec.Alerts, rec.Ended); err != nil {
			return err
		}
		state.applyTrim(rec.TrimThrough)
		return nil

	case recReplay:
		var rec walReplay
		if err := json.Unmarshal(payload, &rec); err != nil {
			return fmt.Errorf("invalid replay record: %w", err)
		}
		state, ok := s.devices[rec.DeviceID]
		if !ok {
			return fmt.Errorf("replay for unknown device %q", rec.DeviceID)
		}
		if rec.BatchID == "" || len(rec.Samples) == 0 || len(rec.Samples) != len(rec.Receipt.SampleStatus) {
			return fmt.Errorf("device %q replay record is incomplete", rec.DeviceID)
		}
		if rec.Receipt.BatchID != rec.BatchID {
			return fmt.Errorf("device %q receipt batchId %q does not match %q", rec.DeviceID, rec.Receipt.BatchID, rec.BatchID)
		}
		if _, exists := state.batches[rec.BatchID]; exists {
			return fmt.Errorf("device %q duplicate stored receipt for batch %q", rec.DeviceID, rec.BatchID)
		}

		samples := make([]Sample, len(rec.Samples))
		newCount, dupCount := 0, 0
		lastNew := -1
		for i, entry := range rec.Samples {
			status := rec.Receipt.SampleStatus[i]
			if entry.EventID == "" || !entry.ObservedAt.ok() || len(entry.Values) == 0 {
				return fmt.Errorf("device %q batch %q sample %d is incomplete", rec.DeviceID, rec.BatchID, i)
			}
			if status.EventID != entry.EventID {
				return fmt.Errorf("device %q batch %q receipt/sample eventId mismatch at %d", rec.DeviceID, rec.BatchID, i)
			}
			samples[i] = Sample{
				EventID:    entry.EventID,
				ObservedAt: entry.ObservedAt.Time,
				Values:     cloneTelemetry(entry.Values),
			}
			if known, seen := state.known[entry.EventID]; seen {
				if !sameInstant(known.observedAt, entry.ObservedAt.Time) || !sameValues(known.values, entry.Values) {
					return fmt.Errorf("device %q dedupe record for %q disagrees with stored sample", rec.DeviceID, entry.EventID)
				}
				if !status.Duplicate || status.Sequence != known.sequence {
					return fmt.Errorf("device %q receipt for %q should reuse sequence %d", rec.DeviceID, entry.EventID, known.sequence)
				}
				dupCount++
				continue
			}
			if status.Duplicate {
				return fmt.Errorf("device %q receipt marks unseen event %q as duplicate", rec.DeviceID, entry.EventID)
			}
			want := state.maxSequence() + 1
			if status.Sequence != want {
				return fmt.Errorf("device %q batch %q sequence mismatch for %q: got %d, want %d", rec.DeviceID, rec.BatchID, entry.EventID, status.Sequence, want)
			}
			state.events = append(state.events, Event{
				Sequence:   status.Sequence,
				ObservedAt: entry.ObservedAt.Time,
				Values:     cloneTelemetry(entry.Values),
			})
			state.known[entry.EventID] = knownSample{
				sequence:   status.Sequence,
				observedAt: entry.ObservedAt.Time,
				values:     cloneTelemetry(entry.Values),
			}
			newCount++
			lastNew = i
		}
		if rec.Receipt.NewCount != newCount || rec.Receipt.Duplicate != dupCount {
			return fmt.Errorf("device %q batch %q receipt counts disagree with samples (%d/%d vs %d/%d)",
				rec.DeviceID, rec.BatchID, rec.Receipt.NewCount, rec.Receipt.Duplicate, newCount, dupCount)
		}
		batchMaxSeq := state.maxSequence()
		if err := validateTrim(state, rec.TrimThrough, batchMaxSeq, state.maxEvents); err != nil {
			return err
		}

		if newCount > 0 {
			if rec.LastSeenAt == nil || rec.LastSeenAt.IsZero() {
				return fmt.Errorf("device %q batch %q missing recorded receive time", rec.DeviceID, rec.BatchID)
			}
			if !sameValues(rec.LastTelemetry, samples[lastNew].Values) {
				return fmt.Errorf("device %q batch %q last telemetry disagrees with last new sample", rec.DeviceID, rec.BatchID)
			}
			state.device.LastSeenAt = rec.LastSeenAt.UTC()
			state.device.LastTelemetry = cloneTelemetry(rec.LastTelemetry)
		} else if rec.LastSeenAt != nil || len(rec.LastTelemetry) != 0 {
			return fmt.Errorf("device %q all-duplicate batch %q unexpectedly refreshed device state", rec.DeviceID, rec.BatchID)
		}

		state.batches[rec.BatchID] = storedBatch{samples: samples, receipt: cloneReceipt(rec.Receipt)}
		if err := applyAlertRecord(state, rec.Alerts, rec.Ended); err != nil {
			return err
		}
		state.applyTrim(rec.TrimThrough)
		return nil

	case recRetention:
		var rec walRetention
		if err := json.Unmarshal(payload, &rec); err != nil {
			return fmt.Errorf("invalid retention record: %w", err)
		}
		state, ok := s.devices[rec.DeviceID]
		if !ok {
			return fmt.Errorf("retention for unknown device %q", rec.DeviceID)
		}
		if rec.MaxEvents < 0 || rec.MaxEvents > maxRetentionEvents {
			return fmt.Errorf("device %q retention limit %d out of range", rec.DeviceID, rec.MaxEvents)
		}
		if err := validateTrim(state, rec.TrimThrough, state.maxSequence(), rec.MaxEvents); err != nil {
			return err
		}
		state.maxEvents = rec.MaxEvents
		state.applyTrim(rec.TrimThrough)
		return nil

	case recRule:
		var rec walRule
		if err := json.Unmarshal(payload, &rec); err != nil {
			return fmt.Errorf("invalid rule record: %w", err)
		}
		state, ok := s.devices[rec.DeviceID]
		if !ok {
			return fmt.Errorf("rule for unknown device %q", rec.DeviceID)
		}
		if rec.Rule.ID == "" || rec.Rule.Metric == "" || rec.Rule.Version < 1 ||
			rec.Rule.CreatedAt.IsZero() || rec.Rule.UpdatedAt.IsZero() {
			return fmt.Errorf("device %q rule record is incomplete", rec.DeviceID)
		}
		if existing, exists := state.rules[rec.Rule.ID]; exists {
			if existing.rule.Version != rec.Rule.Version-1 {
				return fmt.Errorf("device %q rule %q version gap: got %d, want %d",
					rec.DeviceID, rec.Rule.ID, rec.Rule.Version, existing.rule.Version+1)
			}
			existing.rule = rec.Rule
		} else {
			if rec.Rule.Version != 1 {
				return fmt.Errorf("device %q rule %q first record has version %d, want 1",
					rec.DeviceID, rec.Rule.ID, rec.Rule.Version)
			}
			state.rules[rec.Rule.ID] = &ruleState{rule: rec.Rule}
		}
		for _, alert := range rec.Ended {
			if alert == nil || alert.ID < 1 || int(alert.ID) > len(state.alerts) {
				return fmt.Errorf("device %q rule %q ends unknown alert", rec.DeviceID, rec.Rule.ID)
			}
			if alert.Status != alertStatusEnded || alert.EndReason != endReasonRuleChanged || alert.EndedAt == nil {
				return fmt.Errorf("device %q rule %q end of alert %d is malformed", rec.DeviceID, rec.Rule.ID, alert.ID)
			}
			state.alerts[alert.ID-1] = alert
			if rs, ok := state.rules[alert.RuleID]; ok && rs.activeAlertID == alert.ID {
				rs.activeAlertID = 0
			}
		}
		return nil

	case recAlertAck:
		var rec walAlertAck
		if err := json.Unmarshal(payload, &rec); err != nil {
			return fmt.Errorf("invalid alert ack record: %w", err)
		}
		state, ok := s.devices[rec.DeviceID]
		if !ok {
			return fmt.Errorf("alert ack for unknown device %q", rec.DeviceID)
		}
		if rec.AlertID < 1 || int(rec.AlertID) > len(state.alerts) {
			return fmt.Errorf("device %q ack for unknown alert %d", rec.DeviceID, rec.AlertID)
		}
		if rec.AcknowledgedAt.IsZero() {
			return fmt.Errorf("device %q ack for alert %d missing time", rec.DeviceID, rec.AlertID)
		}
		alert := state.alerts[rec.AlertID-1]
		if alert.AcknowledgedAt == nil {
			t := rec.AcknowledgedAt.UTC()
			alert.AcknowledgedAt = &t
		}
		return nil

	case recConfigPublish:
		var rec walConfigPublish
		if err := json.Unmarshal(payload, &rec); err != nil {
			return fmt.Errorf("invalid config publish record: %w", err)
		}
		state, ok := s.devices[rec.DeviceID]
		if !ok {
			return fmt.Errorf("config publish for unknown device %q", rec.DeviceID)
		}
		if rec.RequestID == "" || rec.PublishedAt.IsZero() {
			return fmt.Errorf("device %q config publish record is incomplete", rec.DeviceID)
		}
		if !isNonEmptyJSONObject(rec.Content) {
			return fmt.Errorf("device %q config v%d content is not a non-empty JSON object", rec.DeviceID, rec.Version)
		}
		if state.config == nil {
			state.config = newConfigState()
		}
		cfg := state.config
		if rec.Version != int64(len(cfg.versions))+1 {
			return fmt.Errorf("device %q config version gap: got %d, want %d",
				rec.DeviceID, rec.Version, len(cfg.versions)+1)
		}
		if rec.BaseVersion != rec.Version-1 {
			return fmt.Errorf("device %q config v%d base %d does not precede version",
				rec.DeviceID, rec.Version, rec.BaseVersion)
		}
		if _, dup := cfg.publishes[rec.RequestID]; dup {
			return fmt.Errorf("device %q duplicate config publish request %q", rec.DeviceID, rec.RequestID)
		}
		if rec.Version <= cfg.applied {
			return fmt.Errorf("device %q config v%d published after applied %d", rec.DeviceID, rec.Version, cfg.applied)
		}
		content := cloneJSON(rec.Content)
		cfg.versions = append(cfg.versions, publishedConfig{
			content:     content,
			publishedAt: rec.PublishedAt.UTC(),
		})
		cfg.publishes[rec.RequestID] = storedPublish{
			baseVersion: rec.BaseVersion,
			content:     cloneJSON(content),
			result: ConfigPublishResult{
				RequestID:   rec.RequestID,
				Version:     rec.Version,
				Content:     cloneJSON(content),
				PublishedAt: rec.PublishedAt.UTC(),
			},
		}
		return nil

	case recConfigReceipt:
		var rec walConfigReceipt
		if err := json.Unmarshal(payload, &rec); err != nil {
			return fmt.Errorf("invalid config receipt record: %w", err)
		}
		state, ok := s.devices[rec.DeviceID]
		if !ok {
			return fmt.Errorf("config receipt for unknown device %q", rec.DeviceID)
		}
		if rec.ReceiptID == "" || rec.ReceivedAt.IsZero() {
			return fmt.Errorf("device %q config receipt record is incomplete", rec.DeviceID)
		}
		if !rec.Success && strings.TrimSpace(rec.Reason) == "" {
			return fmt.Errorf("device %q config receipt %q fails without a reason", rec.DeviceID, rec.ReceiptID)
		}
		if state.config == nil {
			state.config = newConfigState()
		}
		cfg := state.config
		if rec.Version < 1 || rec.Version > int64(len(cfg.versions)) {
			return fmt.Errorf("device %q config receipt %q for unknown version %d", rec.DeviceID, rec.ReceiptID, rec.Version)
		}
		if _, dup := cfg.byReceipt[rec.ReceiptID]; dup {
			return fmt.Errorf("device %q duplicate config receipt %q", rec.DeviceID, rec.ReceiptID)
		}
		if rec.Version < cfg.applied {
			return fmt.Errorf("device %q config receipt %q version %d is below applied %d",
				rec.DeviceID, rec.ReceiptID, rec.Version, cfg.applied)
		}
		if !rec.Success && rec.Version <= cfg.applied {
			return fmt.Errorf("device %q config receipt %q fails an already applied version %d",
				rec.DeviceID, rec.ReceiptID, rec.Version)
		}
		receipt := ConfigReceipt{
			ReceiptID:  rec.ReceiptID,
			Version:    rec.Version,
			Success:    rec.Success,
			Reason:     rec.Reason,
			ReceivedAt: rec.ReceivedAt.UTC(),
		}
		cfg.receipts = append(cfg.receipts, receipt)
		cfg.byReceipt[rec.ReceiptID] = receipt
		if rec.Success && rec.Version > cfg.applied {
			cfg.applied = rec.Version
		}
		return nil

	case recTaskCreate:
		var rec walTaskCreate
		if err := json.Unmarshal(payload, &rec); err != nil {
			return fmt.Errorf("invalid task create record: %w", err)
		}
		state, ok := s.devices[rec.DeviceID]
		if !ok {
			return fmt.Errorf("task create for unknown device %q", rec.DeviceID)
		}
		if rec.RequestID == "" || rec.DurationSeconds < 1 || rec.DurationSeconds > taskMaxDuration || rec.CreatedAt.IsZero() {
			return fmt.Errorf("device %q task create record is incomplete", rec.DeviceID)
		}
		state.ensureTasks()
		if _, dup := state.tasksByRequest[rec.RequestID]; dup {
			return fmt.Errorf("device %q duplicate task request %q", rec.DeviceID, rec.RequestID)
		}
		if rec.TaskID != int64(len(state.tasks))+1 {
			return fmt.Errorf("device %q task id gap: got %d, want %d", rec.DeviceID, rec.TaskID, len(state.tasks)+1)
		}
		task := DiagnosticTask{
			ID:              rec.TaskID,
			RequestID:       rec.RequestID,
			DurationSeconds: rec.DurationSeconds,
			Status:          taskStatusPending,
			CreatedAt:       rec.CreatedAt.UTC(),
			NextClaimableAt: rec.NextClaimableAt.UTC(),
		}
		ts := &taskState{task: task, reports: make(map[string]storedReport)}
		rec.Audit.Seq = 1
		ts.audit = append(ts.audit, rec.Audit)
		state.tasks = append(state.tasks, ts)
		state.tasksByRequest[rec.RequestID] = ts
		return nil

	case recTaskClaim:
		var rec walTaskClaim
		if err := json.Unmarshal(payload, &rec); err != nil {
			return fmt.Errorf("invalid task claim record: %w", err)
		}
		state, ok := s.devices[rec.DeviceID]
		if !ok {
			return fmt.Errorf("task claim for unknown device %q", rec.DeviceID)
		}
		ts := findTask(state, rec.TaskID)
		if ts == nil {
			return fmt.Errorf("device %q claim for unknown task %d", rec.DeviceID, rec.TaskID)
		}
		if ts.task.Status != taskStatusPending && ts.task.Status != taskStatusWaiting {
			return fmt.Errorf("device %q task %d claim in bad state %q", rec.DeviceID, rec.TaskID, ts.task.Status)
		}
		if ts.task.Attempts != rec.Attempt-1 {
			return fmt.Errorf("device %q task %d attempt gap: got %d, want %d", rec.DeviceID, rec.TaskID, rec.Attempt, ts.task.Attempts+1)
		}
		if rec.Credential == "" || rec.ClaimedAt.IsZero() || rec.Deadline.IsZero() {
			return fmt.Errorf("device %q task %d claim record is incomplete", rec.DeviceID, rec.TaskID)
		}
		ts.task.Status = taskStatusInProgress
		ts.task.Attempts = rec.Attempt
		ts.task.Credential = rec.Credential
		claimedAt := rec.ClaimedAt.UTC()
		ts.task.ClaimedAt = &claimedAt
		deadline := rec.Deadline.UTC()
		ts.task.Deadline = &deadline
		ts.task.NextClaimableAt = time.Time{}
		rec.Audit.Seq = int64(len(ts.audit)) + 1
		ts.audit = append(ts.audit, rec.Audit)
		return nil

	case recTaskReport:
		var rec walTaskReport
		if err := json.Unmarshal(payload, &rec); err != nil {
			return fmt.Errorf("invalid task report record: %w", err)
		}
		state, ok := s.devices[rec.DeviceID]
		if !ok {
			return fmt.Errorf("task report for unknown device %q", rec.DeviceID)
		}
		ts := findTask(state, rec.TaskID)
		if ts == nil {
			return fmt.Errorf("device %q report for unknown task %d", rec.DeviceID, rec.TaskID)
		}
		if ts.task.Status != taskStatusInProgress {
			return fmt.Errorf("device %q task %d report in bad state %q", rec.DeviceID, rec.TaskID, ts.task.Status)
		}
		if rec.ReceiptID == "" || rec.ReceivedAt.IsZero() {
			return fmt.Errorf("device %q task report record is incomplete", rec.DeviceID)
		}
		if _, dup := ts.reports[rec.ReceiptID]; dup {
			return fmt.Errorf("device %q duplicate task report receipt %q", rec.DeviceID, rec.ReceiptID)
		}
		if owner, bound := state.taskReceipts[rec.ReceiptID]; bound {
			return fmt.Errorf("device %q task report receipt %q already belongs to task %d", rec.DeviceID, rec.ReceiptID, owner)
		}
		if !rec.Success && strings.TrimSpace(rec.Reason) == "" {
			return fmt.Errorf("device %q task report %q fails without a reason", rec.DeviceID, rec.ReceiptID)
		}
		if rec.Success && !isJSONObject(rec.Result) {
			return fmt.Errorf("device %q task report %q success lacks an object result", rec.DeviceID, rec.ReceiptID)
		}
		if rec.Status != taskStatusSucceeded && rec.Status != taskStatusWaiting && rec.Status != taskStatusFailed {
			return fmt.Errorf("device %q task %d report has bad resulting status %q", rec.DeviceID, rec.TaskID, rec.Status)
		}
		ts.task.Status = rec.Status
		ts.task.Failures = rec.Failures
		ts.task.NextClaimableAt = rec.NextClaimableAt.UTC()
		ts.task.FailureReason = rec.FailureReason
		if rec.CompletedAt != nil {
			t := rec.CompletedAt.UTC()
			ts.task.CompletedAt = &t
		}
		if rec.Success {
			ts.task.Result = cloneJSON(rec.Result)
		}
		ts.task.Credential = ""
		ts.task.ClaimedAt = nil
		ts.task.Deadline = nil
		report := storedReport{
			receiptID:  rec.ReceiptID,
			success:    rec.Success,
			reason:     rec.Reason,
			result:     cloneJSON(rec.Result),
			receivedAt: rec.ReceivedAt.UTC(),
		}
		ts.reports[rec.ReceiptID] = report
		state.taskReceipts[rec.ReceiptID] = rec.TaskID
		rec.Audit.Seq = int64(len(ts.audit)) + 1
		ts.audit = append(ts.audit, rec.Audit)
		return nil

	case recTaskTimeout:
		var rec walTaskTimeout
		if err := json.Unmarshal(payload, &rec); err != nil {
			return fmt.Errorf("invalid task timeout record: %w", err)
		}
		state, ok := s.devices[rec.DeviceID]
		if !ok {
			return fmt.Errorf("task timeout for unknown device %q", rec.DeviceID)
		}
		ts := findTask(state, rec.TaskID)
		if ts == nil {
			return fmt.Errorf("device %q timeout for unknown task %d", rec.DeviceID, rec.TaskID)
		}
		if ts.task.Status != taskStatusInProgress {
			return fmt.Errorf("device %q task %d timeout in bad state %q", rec.DeviceID, rec.TaskID, ts.task.Status)
		}
		if ts.task.Attempts != rec.Attempt {
			return fmt.Errorf("device %q task %d timeout attempt mismatch: got %d, want %d", rec.DeviceID, rec.TaskID, rec.Attempt, ts.task.Attempts)
		}
		if rec.Deadline.IsZero() || rec.FailureReason == "" {
			return fmt.Errorf("device %q task %d timeout record is incomplete", rec.DeviceID, rec.TaskID)
		}
		if rec.Status != taskStatusWaiting && rec.Status != taskStatusFailed {
			return fmt.Errorf("device %q task %d timeout has bad resulting status %q", rec.DeviceID, rec.TaskID, rec.Status)
		}
		ts.task.Status = rec.Status
		ts.task.Failures = rec.Failures
		ts.task.NextClaimableAt = rec.NextClaimableAt.UTC()
		ts.task.FailureReason = rec.FailureReason
		if rec.CompletedAt != nil {
			t := rec.CompletedAt.UTC()
			ts.task.CompletedAt = &t
		}
		ts.task.Credential = ""
		ts.task.ClaimedAt = nil
		ts.task.Deadline = nil
		rec.Audit.Seq = int64(len(ts.audit)) + 1
		ts.audit = append(ts.audit, rec.Audit)
		return nil

	case recTaskCancel:
		var rec walTaskCancel
		if err := json.Unmarshal(payload, &rec); err != nil {
			return fmt.Errorf("invalid task cancel record: %w", err)
		}
		state, ok := s.devices[rec.DeviceID]
		if !ok {
			return fmt.Errorf("task cancel for unknown device %q", rec.DeviceID)
		}
		ts := findTask(state, rec.TaskID)
		if ts == nil {
			return fmt.Errorf("device %q cancel for unknown task %d", rec.DeviceID, rec.TaskID)
		}
		if ts.task.Status == taskStatusSucceeded || ts.task.Status == taskStatusFailed {
			return fmt.Errorf("device %q task %d cancel after end", rec.DeviceID, rec.TaskID)
		}
		if rec.CanceledAt.IsZero() {
			return fmt.Errorf("device %q task %d cancel record is incomplete", rec.DeviceID, rec.TaskID)
		}
		ts.task.Status = taskStatusCanceled
		completedAt := rec.CanceledAt.UTC()
		ts.task.CompletedAt = &completedAt
		ts.task.Credential = ""
		ts.task.ClaimedAt = nil
		ts.task.Deadline = nil
		rec.Audit.Seq = int64(len(ts.audit)) + 1
		ts.audit = append(ts.audit, rec.Audit)
		return nil

	default:
		return fmt.Errorf("unknown record type %d", recType)
	}
}

func newInstanceID() []byte {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		panic(fmt.Sprintf("cannot generate instance id: %v", err))
	}
	return id
}
