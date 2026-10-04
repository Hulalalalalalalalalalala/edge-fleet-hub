package fleet

import (
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- helpers ---------------------------------------------------------------

func openPersistent(t *testing.T, dir string) *Store {
	t.Helper()
	store, err := NewPersistentStore(dir)
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	clock := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return clock }
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func reopenPersistent(t *testing.T, dir string) *Store {
	t.Helper()
	store, err := NewPersistentStore(dir)
	if err != nil {
		t.Fatalf("reopen %s: %v", dir, err)
	}
	clock := time.Date(2025, time.June, 1, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return clock }
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func walRaw(t *testing.T, dir string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, walName))
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	return raw
}

func commitRaw(t *testing.T, dir string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, commitName))
	if err != nil {
		t.Fatalf("read commit journal: %v", err)
	}
	return raw
}

// frameEnd walks one frame [length][type][payload][crc32] at pos.
func frameEnd(t *testing.T, data []byte, pos int) int {
	t.Helper()
	length := int(binary.LittleEndian.Uint32(data[pos : pos+4]))
	return pos + 5 + length + 4
}

// --- restart restores everything ------------------------------------------

func TestPersistentRestartRestoresState(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	firstClock := store.now().Add(10 * time.Second)
	store.now = func() time.Time { return firstClock }
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":20}`); r.Code != http.StatusAccepted {
		t.Fatalf("telemetry = %d", r.Code)
	}

	// A mixed batch: one new sample plus one duplicate of an earlier event.
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-a", sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`))); r.Code != http.StatusAccepted {
		t.Fatalf("batch-a = %d", r.Code)
	}
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-b",
			sample("e1", "2024-01-02T18:00:00+08:00", `{"v":1}`),
			sample("e2", "2024-01-02T11:00:00Z", `{"v":2}`),
		)); r.Code != http.StatusAccepted {
		t.Fatalf("batch-b = %d", r.Code)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Recovery uses a later clock; device timestamps must not be refreshed.
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	snapshot := decodeBody[struct {
		Devices []Device `json:"devices"`
	}](t, doRequest(t, h2, http.MethodGet, "/v1/fleet", ""))
	if len(snapshot.Devices) != 1 {
		t.Fatalf("devices = %+v", snapshot.Devices)
	}
	got := snapshot.Devices[0]
	if got.ID != "gw" || got.RegisteredAt.UTC() != time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("registration not restored: %+v", got)
	}
	if got.LastSeenAt.Equal(store2.now()) {
		t.Fatalf("recovery refreshed lastSeenAt to %s", got.LastSeenAt)
	}
	if got.LastTelemetry["v"] != 2 {
		t.Fatalf("lastTelemetry = %+v, want v:2", got.LastTelemetry)
	}

	events := allEvents(t, h2, "gw")
	if len(events) != 3 {
		t.Fatalf("history = %d events, want 3: %+v", len(events), events)
	}
	for i, event := range events {
		if event.Sequence != int64(i+1) {
			t.Fatalf("sequence %d = %d", i, event.Sequence)
		}
	}

	// New samples continue the sequence after restart.
	if r := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":25}`); r.Code != http.StatusAccepted {
		t.Fatalf("post-restart telemetry = %d", r.Code)
	}
	events = allEvents(t, h2, "gw")
	if len(events) != 4 || events[3].Sequence != 4 {
		t.Fatalf("sequence did not continue after restart: %+v", events)
	}

	// Replaying a previously accepted batch returns the first receipt (200);
	// changed content still conflicts.
	firstReceipt := `{"batchId":"batch-a","newCount":1,"duplicateCount":0,"samples":[{"eventId":"e1","sequence":2,"duplicate":false}]}`
	r := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-a", sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`)))
	if r.Code != http.StatusOK {
		t.Fatalf("repeat batch = %d, want 200: %s", r.Code, r.Body.String())
	}
	if compact(r.Body.String()) != firstReceipt {
		t.Fatalf("receipt changed: %s want %s", compact(r.Body.String()), firstReceipt)
	}
	r = doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-a", sample("e1", "2024-01-02T10:00:00Z", `{"v":9}`)))
	if r.Code != http.StatusConflict {
		t.Fatalf("changed batch = %d, want 409", r.Code)
	}

	// Per-device dedupe still holds after restart.
	r = doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-c",
			sample("e2", "2024-01-02T11:00:00Z", `{"v":2}`),
			sample("e3", "2024-01-02T12:00:00Z", `{"v":3}`),
		))
	if r.Code != http.StatusAccepted {
		t.Fatalf("batch-c = %d: %s", r.Code, r.Body.String())
	}
	receipt := decodeBody[receiptResponse](t, r)
	if receipt.NewCount != 1 || receipt.DuplicateCount != 1 ||
		receipt.Samples[0].Sequence != 3 || !receipt.Samples[0].Duplicate ||
		receipt.Samples[1].Sequence != 5 || receipt.Samples[1].Duplicate {
		t.Fatalf("post-restart dedupe receipt = %+v", receipt.Samples)
	}
}

func compact(jsonBody string) string {
	return strings.NewReplacer(" ", "", "\n", "").Replace(strings.TrimSpace(jsonBody))
}

func TestPersistentEmptyDirectoryStartsFresh(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "fleet-data")
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "fresh")
	if r := doRequest(t, h, http.MethodGet, "/v1/devices/fresh/history", ""); r.Code != http.StatusOK {
		t.Fatalf("status = %d", r.Code)
	}
}

// --- cursors survive restart but not a different data directory ------------

func TestPersistentCursorSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	for i := 0; i < 3; i++ {
		if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
			replayBody(fmt.Sprintf("b%d", i), sample(fmt.Sprintf("e%d", i), "2024-01-02T10:00:00Z", fmt.Sprintf(`{"v":%d}`, i)))); r.Code != http.StatusAccepted {
			t.Fatalf("seed %d = %d", i, r.Code)
		}
	}
	first := doRequest(t, h, http.MethodGet, "/v1/devices/gw/history?limit=1", "")
	page := decodeBody[historyResponse](t, first)
	if page.NextCursor == nil {
		t.Fatalf("expected cursor")
	}
	originalCursor := *page.NextCursor
	cursor := originalCursor

	// Add samples, restart: the pinned walk must still end at sequence 3.
	for i := 3; i < 6; i++ {
		if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
			replayBody(fmt.Sprintf("b%d", i), sample(fmt.Sprintf("e%d", i), "2024-01-02T10:00:00Z", fmt.Sprintf(`{"v":%d}`, i)))); r.Code != http.StatusAccepted {
			t.Fatalf("extra %d = %d", i, r.Code)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	seen := page.Events
	for cursor != "" {
		r := doRequest(t, h2, http.MethodGet, "/v1/devices/gw/history?limit=1&cursor="+cursor, "")
		if r.Code != http.StatusOK {
			t.Fatalf("paged status = %d: %s", r.Code, r.Body.String())
		}
		next := decodeBody[historyResponse](t, r)
		seen = append(seen, next.Events...)
		if next.NextCursor == nil {
			cursor = ""
		} else {
			cursor = *next.NextCursor
		}
	}
	if len(seen) != 3 {
		t.Fatalf("pinned walk after restart saw %d events, want 3: %+v", len(seen), seen)
	}

	// The same cursor presented to a different rebuilt data directory (even
	// with the same device id) is 400.
	other := openPersistent(t, t.TempDir())
	otherH := NewHandler(other)
	mustRegister(t, otherH, "gw")
	if r := doRequest(t, otherH, http.MethodGet, "/v1/devices/gw/history?limit=1&cursor="+originalCursor, ""); r.Code != http.StatusBadRequest {
		t.Fatalf("foreign-dir cursor = %d, want 400", r.Code)
	}
}

// --- inter-process directory lock ------------------------------------------

func TestPersistentDirectoryLockExclusive(t *testing.T) {
	dir := t.TempDir()
	first := openPersistent(t, dir)

	if _, err := NewPersistentStore(dir); err == nil {
		t.Fatalf("second open of %s succeeded while first was live", dir)
	} else if !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("second open error = %v, want lock message", err)
	}

	// Data files must be untouched: first process can still write.
	h := NewHandler(first)
	mustRegister(t, h, "gw")

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	// After exit (including a killed process releasing the kernel lock) the
	// directory reopens without manual cleanup.
	again := reopenPersistent(t, dir)
	if !again.Exists("gw") {
		t.Fatalf("device missing after lock release and reopen")
	}
}

// --- damaged committed data fails startup and is left untouched ------------

func TestPersistentCorruptCommittedRecordFailsStartup(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "a")
	mustRegister(t, h, "b")
	mustRegister(t, h, "c")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	raw := walRaw(t, dir)
	// Locate the second committed frame (a register record) and flip a payload
	// byte; its CRC then fails while later committed frames remain intact.
	firstEnd := frameEnd(t, raw, headerLen)
	secondPos := firstEnd
	frameEnd(t, raw, secondPos) // sanity: the frame is fully present
	target := secondPos + 5 + 2 // inside payload
	original := raw[target]
	raw[target] = original ^ 0xFF
	if err := os.WriteFile(filepath.Join(dir, walName), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := NewPersistentStore(dir); err == nil {
		t.Fatalf("startup succeeded with corrupt committed record")
	} else if !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("error = %v, want corruption message", err)
	}

	// The damaged file is left exactly as found.
	after := walRaw(t, dir)
	if string(after) != string(raw) {
		t.Fatalf("corrupt wal was modified by failed startup")
	}
}

// Damage to the length field of the final frame cannot masquerade as a torn
// tail: the commit journal pins the committed length, and the frame CRC fails.
func TestPersistentCorruptFinalFrameLengthFailsStartup(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "only")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	raw := walRaw(t, dir)
	before := walRaw(t, dir)
	_ = before
	// Flip a byte of the length field of the (last) register frame.
	raw[headerLen] ^= 0x01
	if err := os.WriteFile(filepath.Join(dir, walName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPersistentStore(dir); err == nil {
		t.Fatalf("startup succeeded with a damaged final-frame length")
	} else if !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("error = %v, want corruption message", err)
	}
}

// A bad CRC in a complete commit-journal block is committed-data damage; a
// short final block is an unacknowledged append and is trimmed.
func TestPersistentCommitJournalIntegrity(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Full final block with a flipped payload byte and invalid CRC -> fail.
	goodJournal := commitRaw(t, dir)
	journal := append([]byte(nil), goodJournal...)
	journal[0] ^= 0xFF
	journal[8] ^= 0xFF // keep the CRC invalid as well
	if err := os.WriteFile(filepath.Join(dir, commitName), journal, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPersistentStore(dir); err == nil || !strings.Contains(err.Error(), "commit journal") {
		t.Fatalf("got %v, want commit journal corruption error", err)
	}

	// Restore the good journal, then add a torn 5-byte tail: it must be
	// trimmed without losing committed state.
	if err := os.WriteFile(filepath.Join(dir, commitName), append(goodJournal, 1, 2, 3, 4, 5), 0o600); err != nil {
		t.Fatal(err)
	}
	store3 := reopenPersistent(t, dir)
	if !store3.Exists("gw") {
		t.Fatalf("device lost while trimming torn journal tail")
	}
}

func TestPersistentUnsupportedVersionFailsStartup(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	mustRegister(t, NewHandler(store), "gw")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	raw := walRaw(t, dir)
	raw[8] = 99
	if err := os.WriteFile(filepath.Join(dir, walName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPersistentStore(dir); err == nil || !strings.Contains(err.Error(), "unsupported format version") {
		t.Fatalf("got %v, want unsupported version error", err)
	}
}

// --- torn/unacknowledged tail is discarded, committed prefix survives -------

func TestPersistentTornTailIsDiscarded(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b1", sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`))); r.Code != http.StatusAccepted {
		t.Fatalf("seed = %d", r.Code)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate a process killed mid-write: a partial frame with no marker.
	f, err := os.OpenFile(filepath.Join(dir, walName), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0x05, 0x00, 0x00, 0x00, recReplay, 'x'}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	events := allEvents(t, h2, "gw")
	if len(events) != 1 || events[0].Sequence != 1 {
		t.Fatalf("committed event lost across torn tail: %+v", events)
	}
	// Sequence space is not consumed by the torn write.
	if r := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("b2", sample("e2", "2024-01-02T11:00:00Z", `{"v":2}`))); r.Code != http.StatusAccepted {
		t.Fatalf("write after torn tail = %d: %s", r.Code, r.Body.String())
	}
	events = allEvents(t, h2, "gw")
	if len(events) != 2 || events[1].Sequence != 2 {
		t.Fatalf("sequence after torn tail = %+v", events)
	}
}

// --- runtime write failure returns 503 and changes nothing ------------------

type failingWAL struct {
	inner    *walFile
	failOnce bool
	mu       sync.Mutex
	didFail  bool
}

func (w *failingWAL) appendRecord(recType byte, value any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.didFail {
		w.didFail = true
		// Leave residue from an interrupted write at the committed offset,
		// without a commit-journal entry and without advancing durable.
		if err := pwriteAll(w.inner.walF, []byte{0xff, 0xff, 0xff, 0xff, 0x12}, w.inner.walDurable); err != nil {
			return err
		}
		return fmt.Errorf("simulated disk failure")
	}
	return w.inner.appendRecord(recType, value)
}

func TestPersistentWriteFailureReturns503AndRetries(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	// Fail the first telemetry write at the storage layer.
	real := store.wal.(*walFile)
	failing := &failingWAL{inner: real}
	store.wal = failing

	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":1}`)
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing write status = %d, want 503: %s", r.Code, r.Body.String())
	}

	// Queryable state is unchanged and no sequence was consumed.
	events := allEvents(t, h, "gw")
	if len(events) != 0 {
		t.Fatalf("failed write left events: %+v", events)
	}

	// Once writes succeed again, the same request commits at sequence 1 and
	// overwrites the residue the failed attempt left behind.
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":1}`)
	if r.Code != http.StatusAccepted {
		t.Fatalf("retried write = %d: %s", r.Code, r.Body.String())
	}
	events = allEvents(t, h, "gw")
	if len(events) != 1 || events[0].Sequence != 1 {
		t.Fatalf("retry did not commit at sequence 1: %+v", events)
	}

	// A failing replay must not reserve sequences or leave a receipt.
	failing.didFail = false
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-x", sample("x1", "2024-01-03T10:00:00Z", `{"v":9}`)))
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing replay = %d, want 503", r.Code)
	}
	events = allEvents(t, h, "gw")
	if len(events) != 1 {
		t.Fatalf("failed replay consumed a sequence: %+v", events)
	}
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-x", sample("x1", "2024-01-03T10:00:00Z", `{"v":9}`)))
	if r.Code != http.StatusAccepted {
		t.Fatalf("retry of failed replay = %d, want 202 (no prior receipt): %s", r.Code, r.Body.String())
	}
	events = allEvents(t, h, "gw")
	if len(events) != 2 || events[1].Sequence != 2 {
		t.Fatalf("replay retry sequence = %+v", events)
	}

	// Reopening the real log proves the residue never became visible.
	store.wal = real
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	events = allEvents(t, h2, "gw")
	if len(events) != 2 {
		t.Fatalf("recovered history = %+v", events)
	}
}

// --- replay batch atomicity across a crash ----------------------------------
func TestPersistentReplayIsOneCommitUnit(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	// Commit a three-sample batch, then append a complete frame for another
	// batch WITHOUT a commit-journal entry (phase 1 survived, phase 2 did not):
	// recovery must expose none of it.
	good := replayBody("good",
		sample("g1", "2024-01-02T10:00:00Z", `{"v":1}`),
		sample("g2", "2024-01-02T11:00:00Z", `{"v":2}`),
		sample("g3", "2024-01-02T12:00:00Z", `{"v":3}`),
	)
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", good); r.Code != http.StatusAccepted {
		t.Fatalf("good batch = %d", r.Code)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Hand-craft a journal-less frame for another batch and append it.
	body, err := marshalFrame(recReplay, walReplay{
		DeviceID: "gw",
		BatchID:  "lost",
		Samples: []walSample{
			{EventID: "l1", ObservedAt: utcTimePtr(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)), Values: map[string]float64{"v": 4}},
		},
		Receipt: ReplayReceipt{
			BatchID:      "lost",
			NewCount:     1,
			SampleStatus: []SampleStatus{{EventID: "l1", Sequence: 4, Duplicate: false}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := walRaw(t, dir)
	raw = append(raw, body...) // frame present, but absent from commit.log
	if err := os.WriteFile(filepath.Join(dir, walName), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	events := allEvents(t, h2, "gw")
	if len(events) != 3 {
		t.Fatalf("uncommitted batch partially visible: %+v", events)
	}
	// The batch never committed, so resubmitting it is a fresh 202 at seq 4.
	r := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("lost", sample("l1", "2024-01-04T00:00:00Z", `{"v":4}`)))
	if r.Code != http.StatusAccepted {
		t.Fatalf("uncommitted batch retry = %d, want 202", r.Code)
	}
	receipt := decodeBody[receiptResponse](t, r)
	if receipt.Samples[0].Sequence != 4 {
		t.Fatalf("sequence = %d, want 4", receipt.Samples[0].Sequence)
	}
}

// --- durable mode keeps atomicity under concurrency -------------------------

func TestPersistentConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	var start, wg sync.WaitGroup
	start.Add(1)
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			start.Wait()
			samples := make([]string, 4)
			for i := range samples {
				samples[i] = sample(fmt.Sprintf("g%d-e%d", g, i), "2024-01-02T10:00:00Z", fmt.Sprintf(`{"g":%d}`, g))
			}
			r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay", replayBody(fmt.Sprintf("batch-g%d", g), samples...))
			if r.Code != http.StatusAccepted {
				t.Errorf("group %d = %d: %s", g, r.Code, r.Body.String())
			}
		}(g)
	}
	codes := map[int]int{}
	var codesMu sync.Mutex
	for c := 0; c < 10; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start.Wait()
			r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
				replayBody("dup", sample("dup-event", "2024-01-02T10:00:00Z", `{"v":1}`)))
			codesMu.Lock()
			codes[r.Code]++
			codesMu.Unlock()
		}()
	}
	start.Done()
	wg.Wait()
	if codes[http.StatusAccepted] != 1 || codes[http.StatusOK] != 9 {
		t.Fatalf("dup codes = %v", codes)
	}
	events := allEvents(t, h, "gw")
	if len(events) != 16*4+1 {
		t.Fatalf("events = %d, want %d", len(events), 16*4+1)
	}
	for i, event := range events {
		if event.Sequence != int64(i+1) {
			t.Fatalf("non-contiguous sequence at %d: %d", i, event.Sequence)
		}
	}

	// Restart: the same concurrent result must recover exactly.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	recovered := allEvents(t, h2, "gw")
	if len(recovered) != len(events) {
		t.Fatalf("recovered events = %d, want %d", len(recovered), len(events))
	}
	// Re-hammering the dup batch after restart is still a single first receipt.
	r := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("dup", sample("dup-event", "2024-01-02T10:00:00Z", `{"v":1}`)))
	if r.Code != http.StatusOK {
		t.Fatalf("post-restart dup = %d, want 200", r.Code)
	}
}
