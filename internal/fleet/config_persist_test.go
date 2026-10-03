package fleet

import (
	"encoding/json"
	"net/http"
	"testing"
)

// --- restart restores the whole configuration state --------------------------

func TestPersistentRestartRestoresConfigState(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	publishConfig(t, h, "gw", publishBody("r1", 0, `{"v":1}`), http.StatusCreated)
	publishConfig(t, h, "gw", publishBody("r2", 1, `{"nested":{"a":[1,2,3]}}`), http.StatusCreated)
	// v2 fails to apply while v1 succeeds later: applied=1, target=2, and the
	// v2 failure reason must remain visible.
	postReceipt(t, h, "gw", receiptBody("rc-fail", 2, false, "disk full"), http.StatusCreated)
	postReceipt(t, h, "gw", receiptBody("rc-ok-1", 1, true, ""), http.StatusCreated)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	status := getConfigStatus(t, h2, "gw")
	if status.TargetVersion != 2 || status.AppliedVersion != 1 || status.FailureReason != "disk full" {
		t.Fatalf("status after restart = %+v", status)
	}
	// The newer target is still pending and delivered as the latest only.
	r := doRequest(t, h2, http.MethodGet, "/v1/devices/gw/configs/pending", "")
	if r.Code != http.StatusOK {
		t.Fatalf("pending after restart = %d, want 200", r.Code)
	}
	pending := decodeBody[configViewResponse](t, r)
	if pending.Version != 2 || string(pending.Config) != `{"nested":{"a":[1,2,3]}}` {
		t.Fatalf("pending after restart = %+v", pending)
	}

	// Complete history with contents comes back in ascending version order.
	configs := listConfigs(t, h2, "gw")
	if len(configs) != 2 ||
		configs[0].Version != 1 || string(configs[0].Config) != `{"v":1}` ||
		configs[1].Version != 2 {
		t.Fatalf("config history after restart = %+v", configs)
	}

	// Receipts come back in receive order with their first receive times.
	before := listConfigReceipts(t, h2, "gw")
	if len(before) != 2 || before[0].ReceiptID != "rc-fail" || before[1].ReceiptID != "rc-ok-1" {
		t.Fatalf("receipts after restart = %+v", before)
	}

	// Publish dedup survives: the r2 retry is 200 with its first publish time,
	// not a new version (even though the clock moved on during reopen).
	r2retry := publishConfig(t, h2, "gw", publishBody("r2", 1, `{"nested": {"a": [1, 2.0, 3.0]}}`), http.StatusOK)
	if !r2retry.PublishedAt.Equal(configs[1].PublishedAt) {
		t.Fatalf("publish retry time = %s, want first time %s", r2retry.PublishedAt, configs[1].PublishedAt)
	}
	if r := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/configs",
		publishBody("r2", 1, `{"nested":{"a":[1,2,4]}}`)); r.Code != http.StatusConflict {
		t.Fatalf("changed content for old requestId = %d, want 409", r.Code)
	}

	// Receipt dedup survives too.
	rcRetry := postReceipt(t, h2, "gw", receiptBody("rc-fail", 2, false, "disk full"), http.StatusOK)
	if !rcRetry.ReceivedAt.Equal(before[0].ReceivedAt) {
		t.Fatalf("receipt retry time = %s, want first %s", rcRetry.ReceivedAt, before[0].ReceivedAt)
	}

	// Finish applying v2; the failure reason must clear and stay cleared.
	postReceipt(t, h2, "gw", receiptBody("rc-ok-2", 2, true, ""), http.StatusCreated)
	status = getConfigStatus(t, h2, "gw")
	if status.AppliedVersion != 2 || status.FailureReason != "" {
		t.Fatalf("status after catch-up = %+v", status)
	}
	if r := doRequest(t, h2, http.MethodGet, "/v1/devices/gw/configs/pending", ""); r.Code != http.StatusNoContent {
		t.Fatalf("pending after catch-up = %d, want 204", r.Code)
	}
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}

	store3 := reopenPersistent(t, dir)
	h3 := NewHandler(store3)
	status = getConfigStatus(t, h3, "gw")
	if status.TargetVersion != 2 || status.AppliedVersion != 2 || status.FailureReason != "" {
		t.Fatalf("status after second restart = %+v", status)
	}
}

// --- restart keeps exact numeric content and dedup semantics ------------------

func TestPersistentRestartPreservesExactNumbersAndDedup(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	// Numbers chosen so float64 decoding would merge them or round them away:
	// two distinct integers above 2^53, and a nonzero value below float64's
	// smallest positive number.
	first := publishConfig(t, h, "gw", publishBody("r1", 0,
		`{"big":9007199254740993,"tiny":1e-400,"nested":{"a":[9007199254740992,0.1]}}`), http.StatusCreated)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	// Stored digits must come back byte-for-byte: nothing may be reparsed
	// through float64 and rounded on the way out.
	configs := listConfigs(t, h2, "gw")
	if len(configs) != 1 {
		t.Fatalf("configs after restart = %+v", configs)
	}
	wantContent := `{"big":9007199254740993,"tiny":1e-400,"nested":{"a":[9007199254740992,0.1]}}`
	if !sameJSON(configs[0].Config, json.RawMessage(wantContent)) {
		t.Fatalf("content after restart = %s, want %s", configs[0].Config, wantContent)
	}
	if string(configs[0].Config) != wantContent {
		t.Fatalf("digits were rewritten on save: %s", configs[0].Config)
	}

	// An equivalent spelling retried after restart still returns the first
	// result (200, original version/time) ...
	retry := publishConfig(t, h2, "gw", publishBody("r1", 0,
		`{"big":9.007199254740993e15,"tiny":0.01e-398,"nested":{"a":[9007199254740992.0,0.10000000000000000]}}`), http.StatusOK)
	if retry.Version != first.Version || !retry.PublishedAt.Equal(configs[0].PublishedAt) {
		t.Fatalf("retry = %+v, want first v%d at %s", retry, first.Version, configs[0].PublishedAt)
	}
	// ... while any actually different number conflicts even after recovery.
	for _, changed := range []string{
		`{"big":9007199254740992,"tiny":1e-400,"nested":{"a":[9007199254740992,0.1]}}`,
		`{"big":9007199254740993,"tiny":0,"nested":{"a":[9007199254740992,0.1]}}`,
		`{"big":9007199254740993,"tiny":1e-400,"nested":{"a":[9007199254740992,0.10000000000000001]}}`,
	} {
		if r := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/configs",
			publishBody("r1", 0, changed)); r.Code != http.StatusConflict {
			t.Fatalf("changed number after restart => %d, want 409: %s", r.Code, changed)
		}
	}
	// Same requestId with another baseVersion still conflicts.
	if r := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/configs",
		publishBody("r1", 1, wantContent)); r.Code != http.StatusConflict {
		t.Fatalf("changed base after restart => %d, want 409", r.Code)
	}
	// The rejected retries must not have created a version or moved the target.
	if status := getConfigStatus(t, h2, "gw"); status.TargetVersion != 1 || status.AppliedVersion != 0 {
		t.Fatalf("state changed by rejected retries: %+v", status)
	}
	if configs := listConfigs(t, h2, "gw"); len(configs) != 1 {
		t.Fatalf("a rejected retry created a version: %+v", configs)
	}
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}
}

// --- runtime write failures return 503 and roll everything back --------------

func TestPersistentConfigWriteFailureReturns503(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	// Failing publish: no version, no target, no dedup record.
	real := store.wal.(*walFile)
	failing := &failingWAL{inner: real}
	store.wal = failing

	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs",
		publishBody("r1", 0, `{"v":1}`))
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing publish = %d, want 503: %s", r.Code, r.Body.String())
	}
	status := getConfigStatus(t, h, "gw")
	if status.TargetVersion != 0 || status.AppliedVersion != 0 {
		t.Fatalf("failed publish changed state: %+v", status)
	}

	// Same request is fully retryable and creates version 1 (not 200).
	store.wal = real
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs",
		publishBody("r1", 0, `{"v":1}`))
	if r.Code != http.StatusCreated {
		t.Fatalf("retried publish = %d, want 201: %s", r.Code, r.Body.String())
	}

	// Failing receipt: no receipt stored, applied version unchanged.
	store.wal = failing
	failing.didFail = false
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs/receipts",
		receiptBody("rc-ok", 1, true, ""))
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing receipt = %d, want 503", r.Code)
	}
	if receipts := listConfigReceipts(t, h, "gw"); len(receipts) != 0 {
		t.Fatalf("failed receipt was stored: %+v", receipts)
	}
	status = getConfigStatus(t, h, "gw")
	if status.AppliedVersion != 0 {
		t.Fatalf("failed receipt advanced applied to %d", status.AppliedVersion)
	}

	// Same receipt commits on retry.
	store.wal = real
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs/receipts",
		receiptBody("rc-ok", 1, true, ""))
	if r.Code != http.StatusCreated {
		t.Fatalf("retried receipt = %d, want 201: %s", r.Code, r.Body.String())
	}
	status = getConfigStatus(t, h, "gw")
	if status.AppliedVersion != 1 {
		t.Fatalf("applied after retry = %d, want 1", status.AppliedVersion)
	}
}

// --- existing directories keep working --------------------------------------

func TestPersistentExistingDeviceStartsWithNoConfig(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":1}`); r.Code != http.StatusAccepted {
		t.Fatalf("telemetry = %d", r.Code)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	if configs := listConfigs(t, h2, "gw"); len(configs) != 0 {
		t.Fatalf("old device has configs: %+v", configs)
	}
	status := getConfigStatus(t, h2, "gw")
	if status.TargetVersion != 0 || status.AppliedVersion != 0 || status.FailureReason != "" {
		t.Fatalf("old device config status = %+v, want all zero", status)
	}
	if r := doRequest(t, h2, http.MethodGet, "/v1/devices/gw/configs/pending", ""); r.Code != http.StatusNoContent {
		t.Fatalf("pending on old device = %d, want 204", r.Code)
	}
	// New config business works on the recovered device.
	publishConfig(t, h2, "gw", publishBody("r1", 0, `{"v":1}`), http.StatusCreated)
	postReceipt(t, h2, "gw", receiptBody("rc-1", 1, true, ""), http.StatusCreated)

	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}
	store3 := reopenPersistent(t, dir)
	h3 := NewHandler(store3)
	status = getConfigStatus(t, h3, "gw")
	if status.TargetVersion != 1 || status.AppliedVersion != 1 {
		t.Fatalf("config after third open = %+v", status)
	}
}
