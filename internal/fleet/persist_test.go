package fleet

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- persistence test helpers ------------------------------------------------

func newPersistentHandler(t *testing.T) (http.Handler, *Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "fleet-data")
	store, err := NewPersistentStore(dir)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return NewHandler(store), store, dir
}

func statePath(dir string) string { return filepath.Join(dir, stateFileName) }

func writeState(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(statePath(dir), []byte(content), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

func validState(t *testing.T, instance string, mutate func(map[string]any)) string {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	state := map[string]any{
		"format":   formatVersion,
		"instance": instance,
		"devices": map[string]any{
			"gw": map[string]any{
				"device": map[string]any{
					"id":           "gw",
					"site":         "lab",
					"registeredAt": now,
					"lastSeenAt":   now,
				},
				"events":  []any{},
				"byEvent": map[string]any{},
				"batches": map[string]any{},
			},
		},
	}
	if mutate != nil {
		mutate(state)
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	return string(data)
}

// --- restart recovery ---------------------------------------------------------

func TestPersistenceRestoresStateAcrossRestart(t *testing.T) {
	h, store, dir := newPersistentHandler(t)
	mustRegister(t, h, "gw")

	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":20}`)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":22}`)
	firstReplay := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-1",
			sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`),
			sample("e2", "2024-01-02T11:00:00Z", `{"v":2}`),
		))
	if firstReplay.Code != http.StatusAccepted {
		t.Fatalf("first replay status = %d", firstReplay.Code)
	}
	firstReceipt := firstReplay.Body.String()

	before := decodeBody[struct {
		Devices []Device `json:"devices"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/fleet", ""))
	if len(before.Devices) != 1 {
		t.Fatalf("snapshot before restart = %+v", before)
	}

	// Reopen the same directory: everything must come back.
	if err := store.Close(); err != nil {
		t.Fatalf("close before restart: %v", err)
	}
	store, err := NewPersistentStore(dir)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	h2 := NewHandler(store)

	after := decodeBody[struct {
		Devices []Device `json:"devices"`
	}](t, doRequest(t, h2, http.MethodGet, "/v1/fleet", ""))
	if len(after.Devices) != 1 {
		t.Fatalf("snapshot after restart = %+v", after)
	}
	if !after.Devices[0].RegisteredAt.Equal(before.Devices[0].RegisteredAt) ||
		!after.Devices[0].LastSeenAt.Equal(before.Devices[0].LastSeenAt) {
		t.Fatalf("restart changed device times: before=%+v after=%+v", before.Devices[0], after.Devices[0])
	}
	if !sameValues(after.Devices[0].LastTelemetry, before.Devices[0].LastTelemetry) {
		t.Fatalf("last telemetry after restart = %+v, want %+v", after.Devices[0].LastTelemetry, before.Devices[0].LastTelemetry)
	}

	events := allEvents(t, h2, "gw")
	if len(events) != 4 {
		t.Fatalf("history after restart = %d events, want 4", len(events))
	}
	for i, event := range events {
		if event.Sequence != int64(i+1) {
			t.Fatalf("event %d sequence = %d", i, event.Sequence)
		}
	}

	// New samples continue the original sequence.
	next := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":24}`)
	if next.Code != http.StatusAccepted {
		t.Fatalf("telemetry after restart status = %d", next.Code)
	}
	if events := allEvents(t, h2, "gw"); len(events) != 5 || events[4].Sequence != 5 {
		t.Fatalf("history after new write = %+v", events)
	}

	// Re-submitting a committed batch returns the first receipt (200).
	repeat := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-1",
			sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`),
			sample("e2", "2024-01-02T11:00:00Z", `{"v":2}`),
		))
	if repeat.Code != http.StatusOK || repeat.Body.String() != firstReceipt {
		t.Fatalf("repeat after restart = %d %s, want 200 %s", repeat.Code, repeat.Body.String(), firstReceipt)
	}

	// Changed content under the same batchId still conflicts.
	changed := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-1", sample("e1", "2024-01-02T10:00:00Z", `{"v":999}`)))
	if changed.Code != http.StatusConflict {
		t.Fatalf("changed batch after restart status = %d, want 409", changed.Code)
	}

	// A fresh batch commits with continued sequences.
	fresh := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-2", sample("e3", "2024-01-03T10:00:00Z", `{"v":3}`)))
	if fresh.Code != http.StatusAccepted {
		t.Fatalf("new batch after restart status = %d", fresh.Code)
	}
	receipt := decodeBody[receiptResponse](t, fresh)
	if receipt.NewCount != 1 || receipt.Samples[0].Sequence != 6 {
		t.Fatalf("new batch receipt = %+v, want sequence 6", receipt)
	}
}

func TestPersistenceRestoresDedupAcrossRestart(t *testing.T) {
	h, store, dir := newPersistentHandler(t)
	mustRegister(t, h, "gw")
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-a", sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`)))

	if err := store.Close(); err != nil {
		t.Fatalf("close before restart: %v", err)
	}
	store, err := NewPersistentStore(dir)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	h2 := NewHandler(store)

	// Cross-batch duplicate after restart reuses the sequence and does not
	// refresh device state.
	result := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-b",
			sample("e1", "2024-01-02T18:00:00+08:00", `{"v":1}`),
			sample("e2", "2024-01-02T12:00:00Z", `{"v":2}`),
		))
	if result.Code != http.StatusAccepted {
		t.Fatalf("status = %d", result.Code)
	}
	receipt := decodeBody[receiptResponse](t, result)
	if receipt.NewCount != 1 || receipt.DuplicateCount != 1 ||
		!receipt.Samples[0].Duplicate || receipt.Samples[0].Sequence != 1 {
		t.Fatalf("dedup after restart = %+v", receipt)
	}
	if events := allEvents(t, h2, "gw"); len(events) != 2 {
		t.Fatalf("history = %d events, want 2", len(events))
	}
}

// --- cursors across restart and directories ----------------------------------

func TestPersistenceCursorSurvivesRestartAndBindsToDirectory(t *testing.T) {
	h, store, dir := newPersistentHandler(t)
	mustRegister(t, h, "gw")
	for i := 0; i < 3; i++ {
		doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
			replayBody("batch-"+string(rune('a'+i)), sample("e"+string(rune('a'+i)), "2024-01-02T10:00:00Z", `{"v":1}`)))
	}

	first := doRequest(t, h, http.MethodGet, "/v1/devices/gw/history?limit=1", "")
	page := decodeBody[historyResponse](t, first)
	if page.NextCursor == nil {
		t.Fatalf("no cursor on first page")
	}
	cursor := *page.NextCursor
	foreignCursor := cursor

	// Restart: the cursor must still work and stay pinned to the old bound.
	if err := store.Close(); err != nil {
		t.Fatalf("close before restart: %v", err)
	}
	store, err := NewPersistentStore(dir)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	h2 := NewHandler(store)
	walked := 1
	for cursor != "" {
		r := doRequest(t, h2, http.MethodGet, "/v1/devices/gw/history?limit=1&cursor="+cursor, "")
		if r.Code != http.StatusOK {
			t.Fatalf("cursor after restart status = %d: %s", r.Code, r.Body.String())
		}
		p := decodeBody[historyResponse](t, r)
		walked++
		if walked > 3 {
			t.Fatalf("cursor walked past the pinned high-water mark")
		}
		if p.NextCursor == nil {
			cursor = ""
		} else {
			cursor = *p.NextCursor
		}
	}
	if walked != 3 {
		t.Fatalf("walked %d pages, want 3", walked)
	}

	// A cursor minted in another data directory (same device id) is 400.
	otherDir := filepath.Join(t.TempDir(), "other-data")
	other, err := NewPersistentStore(otherDir)
	if err != nil {
		t.Fatalf("other dir: %v", err)
	}
	h3 := NewHandler(other)
	mustRegister(t, h3, "gw")
	r := doRequest(t, h3, http.MethodGet, "/v1/devices/gw/history?limit=1&cursor="+foreignCursor, "")
	if r.Code != http.StatusBadRequest {
		t.Fatalf("foreign-directory cursor status = %d, want 400", r.Code)
	}
}

func TestMemoryModeCursorsRejectPersistentDirectory(t *testing.T) {
	mem := NewHandler(NewStore())
	mustRegister(t, mem, "gw")
	doRequest(t, mem, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-a", sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`)))
	doRequest(t, mem, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-b", sample("e2", "2024-01-02T11:00:00Z", `{"v":2}`)))
	r := doRequest(t, mem, http.MethodGet, "/v1/devices/gw/history?limit=1", "")
	cursor := *decodeBody[historyResponse](t, r).NextCursor

	_, store, _ := newPersistentHandler(t)
	persistent := NewHandler(store)
	mustRegister(t, persistent, "gw")
	if r := doRequest(t, persistent, http.MethodGet, "/v1/devices/gw/history?limit=1&cursor="+cursor, ""); r.Code != http.StatusBadRequest {
		t.Fatalf("memory-mode cursor in persistent dir status = %d, want 400", r.Code)
	}
}

// --- directory locking --------------------------------------------------------

func TestDataDirectoryLockedToSingleProcess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fleet-data")
	first, err := NewPersistentStore(dir)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := NewPersistentStore(dir); err == nil {
		t.Fatalf("second open of same directory succeeded, want failure")
	}
	// No manual cleanup needed: releasing the lock lets a successor in.
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	second, err := NewPersistentStore(dir)
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	_ = second.Close()
}

// --- corruption and format validation -----------------------------------------

func TestRejectsCorruptDataAndPreservesFiles(t *testing.T) {
	h, store, dir := newPersistentHandler(t)
	mustRegister(t, h, "gw")
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":20}`)
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	cases := map[string]string{
		"garbage":        `not json at all`,
		"unsupported":    `{"format":"edge-fleet-hub-v999","instance":"x","devices":{}}`,
		"missing format": `{"instance":"x","devices":{}}`,
		"bad sequence": validState(t, "x", func(state map[string]any) {
			devices := state["devices"].(map[string]any)
			device := devices["gw"].(map[string]any)
			device["events"] = []any{
				map[string]any{"sequence": 2, "observedAt": time.Now().UTC().Format(time.RFC3339Nano), "values": map[string]any{"v": 1}},
			}
		}),
		"dedup mismatch": validState(t, "x", func(state map[string]any) {
			devices := state["devices"].(map[string]any)
			device := devices["gw"].(map[string]any)
			device["byEvent"] = map[string]any{"ghost": 1}
		}),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			writeState(t, dir, content)
			if _, err := NewPersistentStore(dir); err == nil {
				t.Fatalf("startup succeeded on %s data, want failure", name)
			}
			// The bad file must be left untouched.
			data, err := os.ReadFile(statePath(dir))
			if err != nil {
				t.Fatalf("read state: %v", err)
			}
			if string(data) != content {
				t.Fatalf("startup modified the %s state file", name)
			}
		})
	}
}

func TestStaleTempFileRemovedOnStartup(t *testing.T) {
	h, store, dir := newPersistentHandler(t)
	mustRegister(t, h, "gw")
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":20}`)
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A leftover temp file is an uncommitted write: startup removes it and
	// serves the last committed state.
	if err := os.WriteFile(filepath.Join(dir, stateFileName+".tmp"), []byte("partial"), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	store2, err := NewPersistentStore(dir)
	if err != nil {
		t.Fatalf("startup with stale temp: %v", err)
	}
	defer store2.Close()
	if _, err := os.Stat(filepath.Join(dir, stateFileName+".tmp")); !os.IsNotExist(err) {
		t.Fatalf("stale temp file was not removed")
	}
	h2 := NewHandler(store2)
	if events := allEvents(t, h2, "gw"); len(events) != 1 {
		t.Fatalf("history = %d events, want 1 committed event", len(events))
	}
}

// --- runtime write failures ---------------------------------------------------

func TestWriteFailureReturns503AndChangesNothing(t *testing.T) {
	h, _, dir := newPersistentHandler(t)
	mustRegister(t, h, "gw")
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":20}`)

	// Make the directory unwritable: subsequent saves fail, but reads keep
	// working.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(dir, 0o700)

	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":22}`); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("telemetry during outage status = %d, want 503", r.Code)
	}
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-1", sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`))); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("replay during outage status = %d, want 503", r.Code)
	}
	if r := doRequest(t, h, http.MethodPost, "/v1/devices", `{"id":"new"}`); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("register during outage status = %d, want 503", r.Code)
	}

	// Queries still work against the last committed state.
	if r := doRequest(t, h, http.MethodGet, "/v1/devices/gw/history", ""); r.Code != http.StatusOK {
		t.Fatalf("history during outage status = %d", r.Code)
	}
	if events := allEvents(t, h, "gw"); len(events) != 1 {
		t.Fatalf("history during outage = %d events, want 1", len(events))
	}

	// Writes recover: no sequence was consumed and no receipt was left.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("restore chmod: %v", err)
	}
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":22}`); r.Code != http.StatusAccepted {
		t.Fatalf("telemetry after recovery status = %d, want 202", r.Code)
	}
	retry := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-1", sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`)))
	if retry.Code != http.StatusAccepted {
		t.Fatalf("retry of failed batch status = %d, want 202 (no receipt was left)", retry.Code)
	}
	receipt := decodeBody[receiptResponse](t, retry)
	if receipt.Samples[0].Sequence != 3 {
		t.Fatalf("retry sequence = %d, want 3 (no sequence consumed by the 503)", receipt.Samples[0].Sequence)
	}
	if events := allEvents(t, h, "gw"); len(events) != 3 {
		t.Fatalf("history = %d events, want 3", len(events))
	}
}

// --- fresh directory -----------------------------------------------------------

func TestNewPersistentStoreCreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "c")
	store, err := NewPersistentStore(dir)
	if err != nil {
		t.Fatalf("NewPersistentStore on missing dir: %v", err)
	}
	defer store.Close()
	if _, err := os.Stat(statePath(dir)); err != nil {
		t.Fatalf("state file not created: %v", err)
	}
}

func TestNewPersistentStoreRejectsUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.MkdirAll(dir, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer os.Chmod(dir, 0o700)
	if _, err := NewPersistentStore(dir); err == nil {
		t.Fatalf("startup on unwritable directory succeeded, want failure")
	}
}

func TestPersistentModeReplayConflictRollback(t *testing.T) {
	h, _, _ := newPersistentHandler(t)
	mustRegister(t, h, "gw")
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-a", sample("e1", "2024-01-02T10:00:00Z", `{"v":1}`)))

	conflict := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-b",
			sample("e2", "2024-01-02T10:00:00Z", `{"v":2}`),
			sample("e1", "2024-01-02T10:00:00Z", `{"v":999}`),
		))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", conflict.Code)
	}
	if events := allEvents(t, h, "gw"); len(events) != 1 {
		t.Fatalf("conflict left %d events, want 1", len(events))
	}
	// batch-b has no receipt: retry with fresh content is accepted.
	retry := doRequest(t, h, http.MethodPost, "/v1/devices/gw/replay",
		replayBody("batch-b", sample("e3", "2024-01-03T10:00:00Z", `{"v":3}`)))
	if retry.Code != http.StatusAccepted {
		t.Fatalf("retry status = %d, want 202", retry.Code)
	}
}

// Ensure the in-memory mode keeps working without a data directory.
func TestMemoryModeStillWorks(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":21.5}`)
	if r.Code != http.StatusAccepted {
		t.Fatalf("status = %d", r.Code)
	}
	if !strings.Contains(r.Body.String(), `"temperature":21.5`) {
		t.Fatalf("body = %s", r.Body.String())
	}
}
