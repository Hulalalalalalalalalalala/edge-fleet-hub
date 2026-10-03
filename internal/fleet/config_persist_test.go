package fleet

import (
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

// --- recovered configs and dedup records keep exact numbers ------------------

func TestPersistentRestartKeepsConfigNumbersExact(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	publishConfig(t, h, "gw", publishBody("r1", 0,
		`{"big":9007199254740993,"tiny":1e-400,"frac":0.10000000000000001}`), http.StatusCreated)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	// Recovered content is byte-identical: no number was rounded or rewritten.
	configs := listConfigs(t, h2, "gw")
	if len(configs) != 1 ||
		string(configs[0].Config) != `{"big":9007199254740993,"tiny":1e-400,"frac":0.10000000000000001}` {
		t.Fatalf("recovered content = %+v, numbers must not be rounded", configs)
	}

	// The recovered dedup record still judges by exact decimal value: a
	// float64-equivalent but different number conflicts, an exact retry is 200.
	if r := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/configs",
		publishBody("r1", 0, `{"big":9007199254740992,"tiny":1e-400,"frac":0.10000000000000001}`)); r.Code != http.StatusConflict {
		t.Fatalf("rounded big after restart = %d, want 409", r.Code)
	}
	if r := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/configs",
		publishBody("r1", 0, `{"big":9007199254740993,"tiny":0,"frac":0.10000000000000001}`)); r.Code != http.StatusConflict {
		t.Fatalf("underflowed tiny after restart = %d, want 409", r.Code)
	}
	retry := publishConfig(t, h2, "gw", publishBody("r1", 0,
		`{"big":9007199254740993.0,"tiny":10e-401,"frac":0.100000000000000010}`), http.StatusOK)
	if retry.Version != 1 || !retry.PublishedAt.Equal(configs[0].PublishedAt) {
		t.Fatalf("retry after restart = %+v, want first result", retry)
	}
	if string(retry.Config) != `{"big":9007199254740993,"tiny":1e-400,"frac":0.10000000000000001}` {
		t.Fatalf("retry rewrote stored content: %s", retry.Config)
	}
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}
}



// --- recovered configs beyond float64 range stay valid and exact --------------

func TestPersistentRestartKeepsOutOfRangeConfigNumbers(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	large := `{"range":1e309,"nested":{"steps":[-1e309,1e-400]}}`
	publishConfig(t, h, "gw", publishBody("r1", 0, large), http.StatusCreated)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Recovery must not reject a once-committed config just because its numbers
	// overflow float64, and the content comes back byte-identical.
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	configs := listConfigs(t, h2, "gw")
	if len(configs) != 1 || string(configs[0].Config) != large {
		t.Fatalf("recovered out-of-range content = %+v, want %s", configs, large)
	}
	status := getConfigStatus(t, h2, "gw")
	if status.TargetVersion != 1 || status.AppliedVersion != 0 {
		t.Fatalf("status after restart = %+v", status)
	}
	r := doRequest(t, h2, http.MethodGet, "/v1/devices/gw/configs/pending", "")
	if r.Code != http.StatusOK {
		t.Fatalf("pending after restart = %d, want 200", r.Code)
	}
	if pending := decodeBody[configViewResponse](t, r); string(pending.Config) != large {
		t.Fatalf("pending out-of-range content = %s", pending.Config)
	}

	// The recovered dedup record still compares out-of-range numbers exactly:
	// equal value retries as the first result, a changed value conflicts.
	retry := publishConfig(t, h2, "gw", publishBody("r1", 0,
		`{"range":10e308,"nested":{"steps":[-10e308,10e-401]}}`), http.StatusOK)
	if retry.Version != 1 || !retry.PublishedAt.Equal(configs[0].PublishedAt) {
		t.Fatalf("retry after restart = %+v, want first result", retry)
	}
	if string(retry.Config) != large {
		t.Fatalf("retry rewrote stored content: %s", retry.Config)
	}
	if r := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/configs",
		publishBody("r1", 0, `{"range":2e309,"nested":{"steps":[-1e309,1e-400]}}`)); r.Code != http.StatusConflict {
		t.Fatalf("different out-of-range value after restart = %d, want 409", r.Code)
	}
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}
}

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

// --- the failure-reason rule survives restarts and receipt dedup --------------

func TestPersistentFailureReasonTracksStillUnappliedVersions(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	publishConfig(t, h, "gw", publishBody("p1", 0, `{"v":1}`), http.StatusCreated)
	publishConfig(t, h, "gw", publishBody("p2", 1, `{"v":2}`), http.StatusCreated)
	publishConfig(t, h, "gw", publishBody("p3", 2, `{"v":3}`), http.StatusCreated)

	// Failures arrive out of version order (v3 then v2): the current reason is
	// the most recently received failure for a still-unapplied version.
	postReceipt(t, h, "gw", receiptBody("rc-f3", 3, false, "存储空间不足"), http.StatusCreated)
	postReceipt(t, h, "gw", receiptBody("rc-f2", 2, false, "配置校验失败"), http.StatusCreated)
	if status := getConfigStatus(t, h, "gw"); status.TargetVersion != 3 ||
		status.AppliedVersion != 0 || status.FailureReason != "配置校验失败" {
		t.Fatalf("status after out-of-order failures = %+v", status)
	}

	// Duplicate v3 failure: 200 with its first receive time, no reordering.
	f3Retry := postReceipt(t, h, "gw", receiptBody("rc-f3", 3, false, "存储空间不足"), http.StatusOK)

	// v2 catches up: its failure is obsolete but the v3 failure resurfaces
	// rather than being wiped by the success.
	postReceipt(t, h, "gw", receiptBody("rc-ok-2", 2, true, ""), http.StatusCreated)
	if status := getConfigStatus(t, h, "gw"); status.TargetVersion != 3 ||
		status.AppliedVersion != 2 || status.FailureReason != "存储空间不足" {
		t.Fatalf("status after lagging success = %+v", status)
	}
	// Repeating the obsolete v2 failure is still 200 and must not revive it.
	postReceipt(t, h, "gw", receiptBody("rc-f2", 2, false, "配置校验失败"), http.StatusOK)
	if status := getConfigStatus(t, h, "gw"); status.FailureReason != "存储空间不足" {
		t.Fatalf("duplicate obsolete failure revived reason: %+v", status)
	}
	// A new receiptId failing the applied v2 is 409 and stores nothing.
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs/receipts",
		receiptBody("rc-f2-late", 2, false, "配置校验失败")); r.Code != http.StatusConflict {
		t.Fatalf("new failure for applied v2 = %d, want 409: %s", r.Code, r.Body.String())
	}
	before := listConfigReceipts(t, h, "gw")
	if len(before) != 3 ||
		before[0].ReceiptID != "rc-f3" || before[0].Reason != "存储空间不足" ||
		before[1].ReceiptID != "rc-f2" || before[1].Reason != "配置校验失败" ||
		before[2].ReceiptID != "rc-ok-2" {
		t.Fatalf("receipts before restart = %+v", before)
	}
	if !f3Retry.ReceivedAt.Equal(before[0].ReceivedAt) {
		t.Fatalf("duplicate v3 time = %s, want first %s", f3Retry.ReceivedAt, before[0].ReceivedAt)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// After a restart the derived reason is recomputed from the recovered
	// receipt history: v3 still unapplied, so its failure still shows.
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	if status := getConfigStatus(t, h2, "gw"); status.TargetVersion != 3 ||
		status.AppliedVersion != 2 || status.FailureReason != "存储空间不足" {
		t.Fatalf("status after restart = %+v", status)
	}
	r := doRequest(t, h2, http.MethodGet, "/v1/devices/gw/configs/pending", "")
	if r.Code != http.StatusOK || decodeBody[configViewResponse](t, r).Version != 3 {
		t.Fatalf("pending after restart = %d %s, want 200 v3", r.Code, r.Body.String())
	}
	after := listConfigReceipts(t, h2, "gw")
	if len(after) != 3 ||
		after[0].ReceiptID != "rc-f3" || after[1].ReceiptID != "rc-f2" || after[2].ReceiptID != "rc-ok-2" {
		t.Fatalf("receipts after restart = %+v", after)
	}

	// Receipt dedup survives the restart: despite the clock moving on during
	// reopen, the retry is 200 with the ORIGINAL receive time and changes
	// nothing.
	rcF3Again := postReceipt(t, h2, "gw", receiptBody("rc-f3", 3, false, "存储空间不足"), http.StatusOK)
	if !rcF3Again.ReceivedAt.Equal(after[0].ReceivedAt) {
		t.Fatalf("post-restart retry time = %s, want first %s", rcF3Again.ReceivedAt, after[0].ReceivedAt)
	}
	postReceipt(t, h2, "gw", receiptBody("rc-f2", 2, false, "配置校验失败"), http.StatusOK)
	if status := getConfigStatus(t, h2, "gw"); status.FailureReason != "存储空间不足" {
		t.Fatalf("obsolete duplicate after restart revived reason: %+v", status)
	}
	if r := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/configs/receipts",
		receiptBody("rc-f2-other", 2, false, "x")); r.Code != http.StatusConflict {
		t.Fatalf("new applied-v2 failure after restart = %d, want 409", r.Code)
	}

	// v3 applies: current reason clears and pending goes 204, but the failure
	// receipts remain in history.
	postReceipt(t, h2, "gw", receiptBody("rc-ok-3", 3, true, ""), http.StatusCreated)
	if status := getConfigStatus(t, h2, "gw"); status.TargetVersion != 3 ||
		status.AppliedVersion != 3 || status.FailureReason != "" {
		t.Fatalf("status after final success = %+v", status)
	}
	if r := doRequest(t, h2, http.MethodGet, "/v1/devices/gw/configs/pending", ""); r.Code != http.StatusNoContent {
		t.Fatalf("pending after final success = %d, want 204", r.Code)
	}
	if receipts := listConfigReceipts(t, h2, "gw"); len(receipts) != 4 {
		t.Fatalf("clearing reason deleted history: %+v", receipts)
	}
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}

	// A second restart keeps the clean derived status and the full history.
	store3 := reopenPersistent(t, dir)
	h3 := NewHandler(store3)
	if status := getConfigStatus(t, h3, "gw"); status.TargetVersion != 3 ||
		status.AppliedVersion != 3 || status.FailureReason != "" {
		t.Fatalf("status after second restart = %+v", status)
	}
	if receipts := listConfigReceipts(t, h3, "gw"); len(receipts) != 4 ||
		receipts[0].ReceiptID != "rc-f3" || receipts[1].ReceiptID != "rc-f2" ||
		receipts[2].ReceiptID != "rc-ok-2" || receipts[3].ReceiptID != "rc-ok-3" {
		t.Fatalf("receipts after second restart = %+v", receipts)
	}
}

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
