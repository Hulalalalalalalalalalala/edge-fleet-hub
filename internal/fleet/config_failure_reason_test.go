package fleet

import (
	"net/http"
	"testing"
	"time"
)

// Regression coverage for the status rule:
//
//	failureReason is the reason of the most recently RECEIVED failure receipt
//	that still concerns a version the device has not applied yet.
//
// It must not be chosen by highest version number, a single success must not
// clear failures that still concern other unapplied versions, resubmitting a
// known failure receipt is an idempotent 200 that neither moves its position
// nor revives an obsolete reason, and clearing the current reason never deletes
// the receipt history.

// wantConfigStatus asserts the derived status triple and returns it.
func wantConfigStatus(t *testing.T, h http.Handler, device string, target, applied int64, reason string) configStatusResponse {
	t.Helper()
	status := getConfigStatus(t, h, device)
	if status.TargetVersion != target || status.AppliedVersion != applied || status.FailureReason != reason {
		t.Fatalf("status = target %d / applied %d / reason %q, want target %d / applied %d / reason %q",
			status.TargetVersion, status.AppliedVersion, status.FailureReason, target, applied, reason)
	}
	return status
}

// wantPendingVersion asserts the device pull serves exactly wantVersion; 0
// means 204 with no body.
func wantPendingVersion(t *testing.T, h http.Handler, device string, wantVersion int64) {
	t.Helper()
	r := getPending(t, h, device)
	if wantVersion == 0 {
		if r.Code != http.StatusNoContent || r.Body.Len() != 0 {
			t.Fatalf("pending = %d body=%q, want 204 empty", r.Code, r.Body.String())
		}
		return
	}
	if r.Code != http.StatusOK {
		t.Fatalf("pending = %d, want 200: %s", r.Code, r.Body.String())
	}
	view := decodeBody[configViewResponse](t, r)
	if view.Version != wantVersion {
		t.Fatalf("pending offers v%d, want v%d", view.Version, wantVersion)
	}
}

// TestConfigFailureReasonScenarioOutOfOrderReceipts follows the full scenario:
// versions 1/2/3 published, failure receipts arrive 3-then-2, version 2 catches
// up before version 3, and every duplicate/conflict branch leaves the derived
// reason consistent.
func TestConfigFailureReasonScenarioOutOfOrderReceipts(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", publishBody("p1", 0, `{"v":1}`), http.StatusCreated)
	publishConfig(t, h, "gw", publishBody("p2", 1, `{"v":2}`), http.StatusCreated)
	publishConfig(t, h, "gw", publishBody("p3", 2, `{"v":3}`), http.StatusCreated)

	// v3 fails first while nothing is applied: its reason is the current one.
	*clock = clock.Add(time.Hour)
	fail3 := postReceipt(t, h, "gw", receiptBody("rc-fail-3", 3, false, "存储空间不足"), http.StatusCreated)
	wantConfigStatus(t, h, "gw", 3, 0, "存储空间不足")
	wantPendingVersion(t, h, "gw", 3)

	// v2 fails afterwards: it is the most recently received failure and still
	// concerns an unapplied version, so it wins even though v3 is higher.
	*clock = clock.Add(time.Hour)
	fail2 := postReceipt(t, h, "gw", receiptBody("rc-fail-2", 2, false, "配置校验失败"), http.StatusCreated)
	wantConfigStatus(t, h, "gw", 3, 0, "配置校验失败")

	// Repeating the earlier v3 failure is an idempotent retry: 200 with the
	// first receive time, and it must not change its receive-order position or
	// the current reason.
	*clock = clock.Add(time.Hour)
	retryFail3 := postReceipt(t, h, "gw", receiptBody("rc-fail-3", 3, false, "存储空间不足"), http.StatusOK)
	if !retryFail3.ReceivedAt.Equal(fail3.ReceivedAt) {
		t.Fatalf("v3 failure retry receivedAt = %s, want first %s", retryFail3.ReceivedAt, fail3.ReceivedAt)
	}
	wantConfigStatus(t, h, "gw", 3, 0, "配置校验失败")
	receipts := listConfigReceipts(t, h, "gw")
	if len(receipts) != 2 || receipts[0].ReceiptID != "rc-fail-3" || receipts[1].ReceiptID != "rc-fail-2" {
		t.Fatalf("duplicate failure changed ordering/count: %+v", receipts)
	}

	// v2 succeeds (a lagging success): the v2 failure becomes obsolete, but the
	// still-unapplied v3 failure must resurface rather than being wiped.
	*clock = clock.Add(time.Hour)
	postReceipt(t, h, "gw", receiptBody("rc-ok-2", 2, true, ""), http.StatusCreated)
	wantConfigStatus(t, h, "gw", 3, 2, "存储空间不足")
	wantPendingVersion(t, h, "gw", 3)

	// Repeating the now-obsolete v2 failure is still an idempotent 200 and must
	// not make its reason show again.
	*clock = clock.Add(time.Hour)
	retryFail2 := postReceipt(t, h, "gw", receiptBody("rc-fail-2", 2, false, "配置校验失败"), http.StatusOK)
	if !retryFail2.ReceivedAt.Equal(fail2.ReceivedAt) {
		t.Fatalf("v2 failure retry receivedAt = %s, want first %s", retryFail2.ReceivedAt, fail2.ReceivedAt)
	}
	wantConfigStatus(t, h, "gw", 3, 2, "存储空间不足")

	// A NEW receiptId reporting a failure for the already-applied v2 is 409 and
	// changes neither applied version, current reason nor received receipts.
	*clock = clock.Add(time.Hour)
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs/receipts",
		receiptBody("rc-fail-2-late", 2, false, "配置校验失败")); r.Code != http.StatusConflict {
		t.Fatalf("new failure for applied version = %d, want 409: %s", r.Code, r.Body.String())
	}
	wantConfigStatus(t, h, "gw", 3, 2, "存储空间不足")
	receipts = listConfigReceipts(t, h, "gw")
	if len(receipts) != 3 {
		t.Fatalf("409 receipt was stored: %+v", receipts)
	}
	wantIDs := []string{"rc-fail-3", "rc-fail-2", "rc-ok-2"}
	for i, id := range wantIDs {
		if receipts[i].ReceiptID != id {
			t.Fatalf("receipts[%d] = %s, want %s; full: %+v", i, receipts[i].ReceiptID, id, receipts)
		}
	}

	// v3 succeeds: applied catches the target and the current reason clears.
	*clock = clock.Add(time.Hour)
	postReceipt(t, h, "gw", receiptBody("rc-ok-3", 3, true, ""), http.StatusCreated)
	wantConfigStatus(t, h, "gw", 3, 3, "")
	wantPendingVersion(t, h, "gw", 0)

	// Clearing the current reason does not delete history: every failure
	// receipt is still queryable in first-receive order with its own reason.
	receipts = listConfigReceipts(t, h, "gw")
	wantIDs = []string{"rc-fail-3", "rc-fail-2", "rc-ok-2", "rc-ok-3"}
	if len(receipts) != len(wantIDs) {
		t.Fatalf("receipts after completion = %d, want %d: %+v", len(receipts), len(wantIDs), receipts)
	}
	for i, id := range wantIDs {
		if receipts[i].ReceiptID != id {
			t.Fatalf("receipts[%d] = %s, want %s; full: %+v", i, receipts[i].ReceiptID, id, receipts)
		}
	}
	if receipts[0].Version != 3 || receipts[0].Success || receipts[0].Reason != "存储空间不足" {
		t.Fatalf("historical v3 failure altered: %+v", receipts[0])
	}
	if receipts[1].Version != 2 || receipts[1].Success || receipts[1].Reason != "配置校验失败" {
		t.Fatalf("historical v2 failure altered: %+v", receipts[1])
	}

	// Even after the version applied, resending the original failure id is a
	// 200 idempotent retry and the status stays clean.
	*clock = clock.Add(time.Hour)
	postReceipt(t, h, "gw", receiptBody("rc-fail-3", 3, false, "存储空间不足"), http.StatusOK)
	wantConfigStatus(t, h, "gw", 3, 3, "")
}

// TestConfigFailureReasonFollowsReceiveOrderNotVersionOrder is a compact
// crossing: failures for v2 then v1 arrive while nothing is applied, v1 later
// catches up (its failure must be discarded but v2's kept), and two different
// failure receipts for the SAME unapplied version surface the latest one.
func TestConfigFailureReasonFollowsReceiveOrderNotVersionOrder(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", publishBody("p1", 0, `{"v":1}`), http.StatusCreated)
	publishConfig(t, h, "gw", publishBody("p2", 1, `{"v":2}`), http.StatusCreated)
	publishConfig(t, h, "gw", publishBody("p3", 2, `{"v":3}`), http.StatusCreated)

	// Receive order is v2, v1: the latest receipt is the v1 failure, so it is
	// shown even though v2 is the higher failed version.
	postReceipt(t, h, "gw", receiptBody("f2", 2, false, "err-two"), http.StatusCreated)
	postReceipt(t, h, "gw", receiptBody("f1", 1, false, "err-one"), http.StatusCreated)
	wantConfigStatus(t, h, "gw", 3, 0, "err-one")

	// Two distinct failure receipts for the same unapplied v3: the latest
	// received one wins (the first is not frozen as "the" v3 reason).
	postReceipt(t, h, "gw", receiptBody("f3a", 3, false, "err-three-a"), http.StatusCreated)
	postReceipt(t, h, "gw", receiptBody("f3b", 3, false, "err-three-b"), http.StatusCreated)
	wantConfigStatus(t, h, "gw", 3, 0, "err-three-b")
	wantPendingVersion(t, h, "gw", 3)

	// v1 catching up only invalidates the v1 failure; v2/v3 failures survive
	// and the latest of those (f3b) stays visible — one success is not a reset.
	postReceipt(t, h, "gw", receiptBody("ok1", 1, true, ""), http.StatusCreated)
	wantConfigStatus(t, h, "gw", 3, 1, "err-three-b")

	// v2 catching up invalidates f2 but f3b still concerns pending v3.
	postReceipt(t, h, "gw", receiptBody("ok2", 2, true, ""), http.StatusCreated)
	wantConfigStatus(t, h, "gw", 3, 2, "err-three-b")
	wantPendingVersion(t, h, "gw", 3)

	// Finally v3 applies: every failure is obsolete and the reason is empty.
	postReceipt(t, h, "gw", receiptBody("ok3", 3, true, ""), http.StatusCreated)
	wantConfigStatus(t, h, "gw", 3, 3, "")
	wantPendingVersion(t, h, "gw", 0)

	// All six receipts remain queryable in first-receive order.
	receipts := listConfigReceipts(t, h, "gw")
	wantIDs := []string{"f2", "f1", "f3a", "f3b", "ok1", "ok2", "ok3"}
	if len(receipts) != len(wantIDs) {
		t.Fatalf("receipts = %d, want %d: %+v", len(receipts), len(wantIDs), receipts)
	}
	for i, id := range wantIDs {
		if receipts[i].ReceiptID != id {
			t.Fatalf("receipts[%d] = %s, want %s", i, receipts[i].ReceiptID, id)
		}
	}
}
