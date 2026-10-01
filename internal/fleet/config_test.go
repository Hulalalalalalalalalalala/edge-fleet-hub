package fleet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// --- response types ---------------------------------------------------------

type configVersionResponse struct {
	Version     int64           `json:"version"`
	Config      json.RawMessage `json:"config"`
	PublishedAt time.Time       `json:"publishedAt"`
}

type configReceiptResponse struct {
	ReceiptID  string    `json:"receiptId"`
	Version    int64     `json:"version"`
	Result     string    `json:"result"`
	Reason     string    `json:"reason"`
	ReceivedAt time.Time `json:"receivedAt"`
}

type configStatusResponse struct {
	TargetVersion  int64  `json:"targetVersion"`
	AppliedVersion int64  `json:"appliedVersion"`
	FailureReason  string `json:"failureReason"`
}

// --- helpers ----------------------------------------------------------------

func publishBody(requestID string, baseVersion int64, config string) string {
	return fmt.Sprintf(`{"requestId":%q,"baseVersion":%d,"config":%s}`, requestID, baseVersion, config)
}

func receiptBody(receiptID string, version int64, result, reason string) string {
	if reason == "" {
		return fmt.Sprintf(`{"receiptId":%q,"version":%d,"result":%q}`, receiptID, version, result)
	}
	return fmt.Sprintf(`{"receiptId":%q,"version":%d,"result":%q,"reason":%q}`, receiptID, version, result, reason)
}

func publishConfig(t *testing.T, h http.Handler, device, requestID string, baseVersion int64, config string) configVersionResponse {
	t.Helper()
	r := doRequest(t, h, http.MethodPost, fmt.Sprintf("/v1/devices/%s/config", device),
		publishBody(requestID, baseVersion, config))
	if r.Code != http.StatusCreated {
		t.Fatalf("publish %s = %d, want 201: %s", requestID, r.Code, r.Body.String())
	}
	return decodeBody[configVersionResponse](t, r)
}

func pendingConfig(t *testing.T, h http.Handler, device string) (int, *configVersionResponse) {
	t.Helper()
	r := doRequest(t, h, http.MethodGet, fmt.Sprintf("/v1/devices/%s/config", device), "")
	if r.Code == http.StatusNoContent {
		return r.Code, nil
	}
	if r.Code != http.StatusOK {
		t.Fatalf("pending config = %d: %s", r.Code, r.Body.String())
	}
	resp := decodeBody[configVersionResponse](t, r)
	return r.Code, &resp
}

func configStatus(t *testing.T, h http.Handler, device string) configStatusResponse {
	t.Helper()
	r := doRequest(t, h, http.MethodGet, fmt.Sprintf("/v1/devices/%s/config/status", device), "")
	if r.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", r.Code, r.Body.String())
	}
	return decodeBody[configStatusResponse](t, r)
}

func listConfigVersions(t *testing.T, h http.Handler, device string) []configVersionResponse {
	t.Helper()
	r := doRequest(t, h, http.MethodGet, fmt.Sprintf("/v1/devices/%s/config/versions", device), "")
	if r.Code != http.StatusOK {
		t.Fatalf("versions = %d: %s", r.Code, r.Body.String())
	}
	return decodeBody[struct {
		Versions []configVersionResponse `json:"versions"`
	}](t, r).Versions
}

func listConfigReceipts(t *testing.T, h http.Handler, device string) []configReceiptResponse {
	t.Helper()
	r := doRequest(t, h, http.MethodGet, fmt.Sprintf("/v1/devices/%s/config/receipts", device), "")
	if r.Code != http.StatusOK {
		t.Fatalf("receipts = %d: %s", r.Code, r.Body.String())
	}
	return decodeBody[struct {
		Receipts []configReceiptResponse `json:"receipts"`
	}](t, r).Receipts
}

func reportReceipt(t *testing.T, h http.Handler, device, receiptID string, version int64, result, reason string) configReceiptResponse {
	t.Helper()
	r := doRequest(t, h, http.MethodPost, fmt.Sprintf("/v1/devices/%s/config/receipt", device),
		receiptBody(receiptID, version, result, reason))
	if r.Code != http.StatusCreated {
		t.Fatalf("receipt %s = %d, want 201: %s", receiptID, r.Code, r.Body.String())
	}
	return decodeBody[configReceiptResponse](t, r)
}

// --- publish and poll -------------------------------------------------------

func TestConfigPublishCreatesVersionAndPollReturnsIt(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")

	published := publishConfig(t, h, "gw", "req-1", 0, `{"mode":"safe","threshold":25}`)
	if published.Version != 1 {
		t.Fatalf("version = %d, want 1", published.Version)
	}
	if published.PublishedAt.IsZero() {
		t.Fatalf("publishedAt is zero")
	}
	if !jsonEqual(published.Config, `{"mode":"safe","threshold":25}`) {
		t.Fatalf("config = %s", published.Config)
	}

	code, pending := pendingConfig(t, h, "gw")
	if code != http.StatusOK || pending == nil || pending.Version != 1 {
		t.Fatalf("pending = %d/%+v", code, pending)
	}
	if !jsonEqual(pending.Config, `{"mode":"safe","threshold":25}`) {
		t.Fatalf("pending config = %s", pending.Config)
	}

	status := configStatus(t, h, "gw")
	if status.TargetVersion != 1 || status.AppliedVersion != 0 || status.FailureReason != "" {
		t.Fatalf("status = %+v", status)
	}

	versions := listConfigVersions(t, h, "gw")
	if len(versions) != 1 || versions[0].Version != 1 {
		t.Fatalf("versions = %+v", versions)
	}
	if receipts := listConfigReceipts(t, h, "gw"); len(receipts) != 0 {
		t.Fatalf("receipts = %+v", receipts)
	}
}

func TestConfigPollReturns204WhenApplied(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", "req-1", 0, `{"mode":"safe"}`)

	// Before any receipt: pending.
	if code, _ := pendingConfig(t, h, "gw"); code != http.StatusOK {
		t.Fatalf("pending before receipt = %d", code)
	}

	reportReceipt(t, h, "gw", "rcpt-1", 1, "success", "")
	if code, pending := pendingConfig(t, h, "gw"); code != http.StatusNoContent || pending != nil {
		t.Fatalf("pending after success = %d/%+v, want 204", code, pending)
	}
	status := configStatus(t, h, "gw")
	if status.TargetVersion != 1 || status.AppliedVersion != 1 {
		t.Fatalf("status = %+v", status)
	}

	// Repeated reads do not change state.
	if code, _ := pendingConfig(t, h, "gw"); code != http.StatusNoContent {
		t.Fatalf("repeat poll = %d", code)
	}
}

func TestConfigFailureDoesNotAdvanceVersion(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", "req-1", 0, `{"mode":"safe"}`)

	reportReceipt(t, h, "gw", "rcpt-1", 1, "failed", "sensor timeout")
	status := configStatus(t, h, "gw")
	if status.TargetVersion != 1 || status.AppliedVersion != 0 || status.FailureReason != "sensor timeout" {
		t.Fatalf("status = %+v", status)
	}
	// Latest config still readable for retry.
	if code, pending := pendingConfig(t, h, "gw"); code != http.StatusOK || pending == nil {
		t.Fatalf("pending after failure = %d", code)
	}

	reportReceipt(t, h, "gw", "rcpt-2", 1, "success", "")
	status = configStatus(t, h, "gw")
	if status.AppliedVersion != 1 {
		t.Fatalf("applied = %d, want 1", status.AppliedVersion)
	}
	if code, _ := pendingConfig(t, h, "gw"); code != http.StatusNoContent {
		t.Fatalf("pending after success = %d", code)
	}
}

// --- publish dedup and conflicts -------------------------------------------

func TestConfigPublishDedupByIdenticalRequest(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")

	first := publishConfig(t, h, "gw", "req-1", 0, `{"a":1,"b":[1,2]}`)

	// Same requestId, same base, semantically equal content (reordered keys,
	// whitespace): 200 with the first result, no new version.
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/config",
		publishBody("req-1", 0, `  {"b":[1,2],"a":1.0}  `))
	if r.Code != http.StatusOK {
		t.Fatalf("retry = %d, want 200: %s", r.Code, r.Body.String())
	}
	retry := decodeBody[configVersionResponse](t, r)
	if retry.Version != first.Version || !retry.PublishedAt.Equal(first.PublishedAt) {
		t.Fatalf("retry changed result: %+v vs %+v", retry, first)
	}
	if versions := listConfigVersions(t, h, "gw"); len(versions) != 1 {
		t.Fatalf("versions = %+v, want 1", versions)
	}

	// Same requestId, different content: 409.
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/config",
		publishBody("req-1", 0, `{"a":2}`))
	if r.Code != http.StatusConflict {
		t.Fatalf("changed content = %d, want 409", r.Code)
	}
	// Same requestId, different base: 409.
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/config",
		publishBody("req-1", 1, `{"a":1,"b":[1,2]}`))
	if r.Code != http.StatusConflict {
		t.Fatalf("changed base = %d, want 409", r.Code)
	}
	if versions := listConfigVersions(t, h, "gw"); len(versions) != 1 {
		t.Fatalf("versions after conflicts = %+v, want 1", versions)
	}
}

func TestConfigBaseVersionConflict(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", "req-1", 0, `{"v":1}`)

	// Target is now 1; base 0 conflicts.
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/config",
		publishBody("req-2", 0, `{"v":2}`))
	if r.Code != http.StatusConflict {
		t.Fatalf("stale base = %d, want 409", r.Code)
	}
	// base 1 creates version 2.
	publishConfig(t, h, "gw", "req-2", 1, `{"v":2}`)
	// base 5 conflicts.
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/config",
		publishBody("req-3", 5, `{"v":3}`))
	if r.Code != http.StatusConflict {
		t.Fatalf("future base = %d, want 409", r.Code)
	}
	if versions := listConfigVersions(t, h, "gw"); len(versions) != 2 {
		t.Fatalf("versions = %+v, want 2", versions)
	}
}

func TestConfigConcurrentPublishSameBaseOneWins(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")

	const callers = 12
	var start, wg sync.WaitGroup
	start.Add(1)
	codes := make([]int, callers)
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			start.Wait()
			r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/config",
				publishBody(fmt.Sprintf("req-%d", i), 0, `{"v":1}`))
			codes[i] = r.Code
		}(i)
	}
	start.Done()
	wg.Wait()

	created, conflict := 0, 0
	for _, code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
		default:
			t.Errorf("unexpected status %d", code)
		}
	}
	if created != 1 || conflict != callers-1 {
		t.Fatalf("codes = %d created / %d conflict, want 1/%d", created, conflict, callers-1)
	}
	if versions := listConfigVersions(t, h, "gw"); len(versions) != 1 {
		t.Fatalf("versions = %+v, want 1", versions)
	}
}

// --- config equality semantics ---------------------------------------------

func TestConfigEqualitySemantics(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", "req-1", 0, `{"a":1,"b":[1,2,{"c":3}]}`)

	cases := map[string]bool{
		`{"b":[1,2,{"c":3}],"a":1.0}`:     true,  // reordered keys, numeric equivalence
		`{"a":1,"b":[1,2,{"c":3}]}`:       true,  // identical
		`{"a":1,"b":[1,2,{"c":4}]}`:       false, // nested value differs
		`{"a":1,"b":[2,1,{"c":3}]}`:       false, // array order differs
		`{"a":"1","b":[1,2,{"c":3}]}`:     false, // type differs
		`{"a":1,"b":[1,2,{"c":3}],"x":1}`: false, // extra key
	}
	for body, wantEqual := range cases {
		r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/config",
			publishBody("req-1", 0, body))
		want := http.StatusOK
		if !wantEqual {
			want = http.StatusConflict
		}
		if r.Code != want {
			t.Errorf("body %s: status = %d, want %d", body, r.Code, want)
		}
	}
}

// --- receipt dedup and conflicts -------------------------------------------

func TestConfigReceiptDedup(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", "req-1", 0, `{"v":1}`)

	first := reportReceipt(t, h, "gw", "rcpt-1", 1, "success", "")

	// Same receiptId, same content: 200 with the first result.
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/config/receipt",
		receiptBody("rcpt-1", 1, "success", ""))
	if r.Code != http.StatusOK {
		t.Fatalf("retry = %d, want 200", r.Code)
	}
	retry := decodeBody[configReceiptResponse](t, r)
	if retry.ReceiptID != first.ReceiptID || !retry.ReceivedAt.Equal(first.ReceivedAt) {
		t.Fatalf("retry changed result: %+v vs %+v", retry, first)
	}

	// Same receiptId, different result: 409.
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/config/receipt",
		receiptBody("rcpt-1", 1, "failed", "timeout"))
	if r.Code != http.StatusConflict {
		t.Fatalf("changed result = %d, want 409", r.Code)
	}
	// Same receiptId, different version: 409.
	publishConfig(t, h, "gw", "req-2", 1, `{"v":2}`)
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/config/receipt",
		receiptBody("rcpt-1", 2, "success", ""))
	if r.Code != http.StatusConflict {
		t.Fatalf("changed version = %d, want 409", r.Code)
	}
	if receipts := listConfigReceipts(t, h, "gw"); len(receipts) != 1 {
		t.Fatalf("receipts = %+v, want 1", receipts)
	}
}

func TestConfigReceiptUnknownVersionIs404(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")

	// No config published: version 1 is unknown.
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/config/receipt",
		receiptBody("rcpt-1", 1, "success", ""))
	if r.Code != http.StatusNotFound {
		t.Fatalf("unknown version = %d, want 404", r.Code)
	}

	publishConfig(t, h, "gw", "req-1", 0, `{"v":1}`)
	// Version 2 does not exist yet.
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/config/receipt",
		receiptBody("rcpt-2", 2, "success", ""))
	if r.Code != http.StatusNotFound {
		t.Fatalf("future version = %d, want 404", r.Code)
	}
}

func TestConfigReceiptBelowAppliedIs409(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", "req-1", 0, `{"v":1}`)
	publishConfig(t, h, "gw", "req-2", 1, `{"v":2}`)
	reportReceipt(t, h, "gw", "rcpt-1", 2, "success", "")

	// A new receipt for version 1 is below the applied version: 409.
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/config/receipt",
		receiptBody("rcpt-2", 1, "success", ""))
	if r.Code != http.StatusConflict {
		t.Fatalf("below applied = %d, want 409", r.Code)
	}
	// Reporting failure on the already-succeeded version 2: 409.
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/config/receipt",
		receiptBody("rcpt-3", 2, "failed", "too late"))
	if r.Code != http.StatusConflict {
		t.Fatalf("failed on applied = %d, want 409", r.Code)
	}
}

func TestConfigSuccessBehindTargetIsRecorded(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", "req-1", 0, `{"v":1}`)
	publishConfig(t, h, "gw", "req-2", 1, `{"v":2}`)
	publishConfig(t, h, "gw", "req-3", 2, `{"v":3}`)

	// Device applies version 1 (skipping 2); target 3 still pending.
	reportReceipt(t, h, "gw", "rcpt-1", 1, "success", "")
	status := configStatus(t, h, "gw")
	if status.TargetVersion != 3 || status.AppliedVersion != 1 {
		t.Fatalf("status = %+v", status)
	}
	if code, pending := pendingConfig(t, h, "gw"); code != http.StatusOK || pending == nil || pending.Version != 3 {
		t.Fatalf("pending = %d/%+v, want version 3", code, pending)
	}
}

// --- versions and receipts listing -----------------------------------------

func TestConfigVersionsAscendingAndReceiptsInReceiveOrder(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	for i := 1; i <= 3; i++ {
		publishConfig(t, h, "gw", fmt.Sprintf("req-%d", i), int64(i-1), fmt.Sprintf(`{"v":%d}`, i))
	}
	reportReceipt(t, h, "gw", "rcpt-a", 1, "failed", "timeout")
	reportReceipt(t, h, "gw", "rcpt-b", 2, "success", "")
	reportReceipt(t, h, "gw", "rcpt-c", 3, "success", "")

	versions := listConfigVersions(t, h, "gw")
	if len(versions) != 3 {
		t.Fatalf("versions = %+v", versions)
	}
	for i, v := range versions {
		if v.Version != int64(i+1) {
			t.Fatalf("version %d = %d", i, v.Version)
		}
	}

	receipts := listConfigReceipts(t, h, "gw")
	if len(receipts) != 3 {
		t.Fatalf("receipts = %+v", receipts)
	}
	wantIDs := []string{"rcpt-a", "rcpt-b", "rcpt-c"}
	for i, r := range receipts {
		if r.ReceiptID != wantIDs[i] {
			t.Fatalf("receipt %d = %s, want %s", i, r.ReceiptID, wantIDs[i])
		}
	}
	status := configStatus(t, h, "gw")
	if status.FailureReason != "timeout" {
		t.Fatalf("failureReason = %q", status.FailureReason)
	}
}

// --- validation -------------------------------------------------------------

func TestConfigValidationFailures(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")

	cases := map[string]string{
		"not JSON":            `not json`,
		"two JSON values":     `{"requestId":"r","baseVersion":0,"config":{"a":1}}{"requestId":"s","baseVersion":0,"config":{"a":1}}`,
		"blank requestId":     `{"requestId":"  ","baseVersion":0,"config":{"a":1}}`,
		"missing requestId":   `{"baseVersion":0,"config":{"a":1}}`,
		"non-string requestId": `{"requestId":5,"baseVersion":0,"config":{"a":1}}`,
		"negative base":       `{"requestId":"r","baseVersion":-1,"config":{"a":1}}`,
		"float base":          `{"requestId":"r","baseVersion":1.5,"config":{"a":1}}`,
		"string base":         `{"requestId":"r","baseVersion":"0","config":{"a":1}}`,
		"missing base":        `{"requestId":"r","config":{"a":1}}`,
		"empty config object": `{"requestId":"r","baseVersion":0,"config":{}}`,
		"array config":        `{"requestId":"r","baseVersion":0,"config":[1,2]}`,
		"string config":       `{"requestId":"r","baseVersion":0,"config":"nope"}`,
		"null config":         `{"requestId":"r","baseVersion":0,"config":null}`,
		"missing config":      `{"requestId":"r","baseVersion":0}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/config", body)
			if r.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", r.Code, r.Body.String())
			}
		})
	}

	// Failed validation must not create versions.
	if versions := listConfigVersions(t, h, "gw"); len(versions) != 0 {
		t.Fatalf("versions = %+v", versions)
	}
}

func TestConfigReceiptValidationFailures(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", "req-1", 0, `{"v":1}`)

	cases := map[string]string{
		"blank receiptId":   `{"receiptId":"  ","version":1,"result":"success"}`,
		"missing receiptId": `{"version":1,"result":"success"}`,
		"zero version":      `{"receiptId":"r","version":0,"result":"success"}`,
		"negative version":  `{"receiptId":"r","version":-1,"result":"success"}`,
		"float version":     `{"receiptId":"r","version":1.5,"result":"success"}`,
		"string version":    `{"receiptId":"r","version":"1","result":"success"}`,
		"missing version":   `{"receiptId":"r","result":"success"}`,
		"bad result":       `{"receiptId":"r","version":1,"result":"maybe"}`,
		"missing result":    `{"receiptId":"r","version":1}`,
		"failed no reason":  `{"receiptId":"r","version":1,"result":"failed"}`,
		"failed blank reason": `{"receiptId":"r","version":1,"result":"failed","reason":"  "}`,
		"reason non-string": `{"receiptId":"r","version":1,"result":"failed","reason":5}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/config/receipt", body)
			if r.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", r.Code, r.Body.String())
			}
		})
	}
}

func TestConfigUnknownDeviceIs404(t *testing.T) {
	h := NewHandler(NewStore())

	r := doRequest(t, h, http.MethodPost, "/v1/devices/ghost/config",
		publishBody("req-1", 0, `{"v":1}`))
	if r.Code != http.StatusNotFound {
		t.Fatalf("publish = %d, want 404", r.Code)
	}
	r = doRequest(t, h, http.MethodGet, "/v1/devices/ghost/config", "")
	if r.Code != http.StatusNotFound {
		t.Fatalf("poll = %d, want 404", r.Code)
	}
	r = doRequest(t, h, http.MethodPost, "/v1/devices/ghost/config/receipt",
		receiptBody("rcpt-1", 1, "success", ""))
	if r.Code != http.StatusNotFound {
		t.Fatalf("receipt = %d, want 404", r.Code)
	}
	r = doRequest(t, h, http.MethodGet, "/v1/devices/ghost/config/status", "")
	if r.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", r.Code)
	}
	r = doRequest(t, h, http.MethodGet, "/v1/devices/ghost/config/versions", "")
	if r.Code != http.StatusNotFound {
		t.Fatalf("versions = %d, want 404", r.Code)
	}
	r = doRequest(t, h, http.MethodGet, "/v1/devices/ghost/config/receipts", "")
	if r.Code != http.StatusNotFound {
		t.Fatalf("receipts = %d, want 404", r.Code)
	}
}

// --- config ops do not touch telemetry/rules --------------------------------

func TestConfigOpsDoNotTouchTelemetryOrRules(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	createRule(t, h, "gw", "r1", "temperature", 30, 25)

	// Telemetry before config.
	*clock = clock.Add(time.Second)
	doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":20}`)
	before := decodeBody[struct {
		Devices []Device `json:"devices"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/fleet", "")).Devices[0]

	// Config publish and receipt.
	publishConfig(t, h, "gw", "req-1", 0, `{"mode":"safe"}`)
	reportReceipt(t, h, "gw", "rcpt-1", 1, "success", "")

	after := decodeBody[struct {
		Devices []Device `json:"devices"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/fleet", "")).Devices[0]
	if !after.LastSeenAt.Equal(before.LastSeenAt) {
		t.Fatalf("lastSeenAt changed: %s -> %s", before.LastSeenAt, after.LastSeenAt)
	}
	if after.LastTelemetry["temperature"] != before.LastTelemetry["temperature"] {
		t.Fatalf("lastTelemetry changed: %+v -> %+v", before.LastTelemetry, after.LastTelemetry)
	}
	if events := allEvents(t, h, "gw"); len(events) != 1 {
		t.Fatalf("history changed: %+v", events)
	}
	if alerts := listAlerts(t, h, "gw", ""); len(alerts) != 0 {
		t.Fatalf("alerts changed: %+v", alerts)
	}
}

// --- persistence -------------------------------------------------------------

func TestConfigPersistenceRestartPreservesState(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", "req-1", 0, `{"mode":"safe","threshold":25}`)
	publishConfig(t, h, "gw", "req-2", 1, `{"mode":"full","threshold":40}`)
	reportReceipt(t, h, "gw", "rcpt-1", 1, "success", "")
	reportReceipt(t, h, "gw", "rcpt-2", 2, "failed", "sensor timeout")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)

	status := configStatus(t, h2, "gw")
	if status.TargetVersion != 2 || status.AppliedVersion != 1 || status.FailureReason != "sensor timeout" {
		t.Fatalf("status after restart = %+v", status)
	}
	versions := listConfigVersions(t, h2, "gw")
	if len(versions) != 2 || versions[0].Version != 1 || versions[1].Version != 2 {
		t.Fatalf("versions after restart = %+v", versions)
	}
	receipts := listConfigReceipts(t, h2, "gw")
	if len(receipts) != 2 || receipts[0].ReceiptID != "rcpt-1" || receipts[1].ReceiptID != "rcpt-2" {
		t.Fatalf("receipts after restart = %+v", receipts)
	}
	// Version 2 still pending.
	if code, pending := pendingConfig(t, h2, "gw"); code != http.StatusOK || pending == nil || pending.Version != 2 {
		t.Fatalf("pending after restart = %d/%+v", code, pending)
	}

	// Dedup survives restart: retry publish and receipt return 200.
	r := doRequest(t, h2, http.MethodPost, "/v1/devices/gw/config",
		publishBody("req-1", 0, `{"threshold":25,"mode":"safe"}`))
	if r.Code != http.StatusOK {
		t.Fatalf("publish retry after restart = %d, want 200", r.Code)
	}
	r = doRequest(t, h2, http.MethodPost, "/v1/devices/gw/config/receipt",
		receiptBody("rcpt-1", 1, "success", ""))
	if r.Code != http.StatusOK {
		t.Fatalf("receipt retry after restart = %d, want 200", r.Code)
	}

	// New publish continues at version 3.
	publishConfig(t, h2, "gw", "req-3", 2, `{"mode":"off"}`)
	versions = listConfigVersions(t, h2, "gw")
	if len(versions) != 3 || versions[2].Version != 3 {
		t.Fatalf("versions after new publish = %+v", versions)
	}
}

func TestConfigPersistenceWriteFailure503AndRetry(t *testing.T) {
	dir := t.TempDir()
	store := openPersistent(t, dir)
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", "req-1", 0, `{"v":1}`)

	real := store.wal.(*walFile)
	failing := &failingWAL{inner: real}
	store.wal = failing

	// Fail the next publish: 503, no version consumed, retryable.
	r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/config",
		publishBody("req-2", 1, `{"v":2}`))
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing publish = %d, want 503", r.Code)
	}
	if versions := listConfigVersions(t, h, "gw"); len(versions) != 1 {
		t.Fatalf("versions after failed publish = %+v", versions)
	}
	// The failing WAL fails once then succeeds; the same request retries.
	publishConfig(t, h, "gw", "req-2", 1, `{"v":2}`)
	if versions := listConfigVersions(t, h, "gw"); len(versions) != 2 {
		t.Fatalf("versions after retry = %+v", versions)
	}

	// Fail the next receipt: 503, no receipt kept, retryable.
	failing.didFail = false
	r = doRequest(t, h, http.MethodPost, "/v1/devices/gw/config/receipt",
		receiptBody("rcpt-1", 2, "success", ""))
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing receipt = %d, want 503", r.Code)
	}
	if receipts := listConfigReceipts(t, h, "gw"); len(receipts) != 0 {
		t.Fatalf("receipts after failed receipt = %+v", receipts)
	}
	reportReceipt(t, h, "gw", "rcpt-1", 2, "success", "")
	if receipts := listConfigReceipts(t, h, "gw"); len(receipts) != 1 {
		t.Fatalf("receipts after retry = %+v", receipts)
	}

	// Reopen: the failed writes never became visible.
	store.wal = real
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2 := reopenPersistent(t, dir)
	h2 := NewHandler(store2)
	if versions := listConfigVersions(t, h2, "gw"); len(versions) != 2 {
		t.Fatalf("versions after reopen = %+v", versions)
	}
	if receipts := listConfigReceipts(t, h2, "gw"); len(receipts) != 1 {
		t.Fatalf("receipts after reopen = %+v", receipts)
	}
}

// jsonEqual reports whether raw JSON content is semantically equal to the
// expected JSON string.
func jsonEqual(raw json.RawMessage, expected string) bool {
	var a, b any
	if err := json.Unmarshal(raw, &a); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(expected), &b); err != nil {
		return false
	}
	return jsonEqualValue(a, b)
}

func jsonEqualValue(a, b any) bool {
	return deepEqualJSON(a, b)
}

func deepEqualJSON(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			if !deepEqualJSON(v, bv[k]) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !deepEqualJSON(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}
