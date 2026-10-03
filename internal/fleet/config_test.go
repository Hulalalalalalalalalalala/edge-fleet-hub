package fleet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// --- response types and helpers ---------------------------------------------

type configPublishResponse struct {
	RequestID   string          `json:"requestId"`
	Version     int64           `json:"version"`
	Config      json.RawMessage `json:"config"`
	PublishedAt time.Time       `json:"publishedAt"`
}

type configViewResponse struct {
	Version     int64           `json:"version"`
	Config      json.RawMessage `json:"config"`
	PublishedAt time.Time       `json:"publishedAt"`
}

type configReceiptResponse struct {
	ReceiptID  string    `json:"receiptId"`
	Version    int64     `json:"version"`
	Success    bool      `json:"success"`
	Reason     string    `json:"reason"`
	ReceivedAt time.Time `json:"receivedAt"`
}

type configStatusResponse struct {
	TargetVersion  int64  `json:"targetVersion"`
	AppliedVersion int64  `json:"appliedVersion"`
	FailureReason  string `json:"failureReason"`
}

func publishBody(requestID string, base int64, config string) string {
	return fmt.Sprintf(`{"requestId":%q,"baseVersion":%d,"config":%s}`, requestID, base, config)
}

func receiptBody(receiptID string, version int64, success bool, reason string) string {
	if reason != "" {
		return fmt.Sprintf(`{"receiptId":%q,"version":%d,"success":%v,"reason":%q}`, receiptID, version, success, reason)
	}
	return fmt.Sprintf(`{"receiptId":%q,"version":%d,"success":%v}`, receiptID, version, success)
}

func publishConfig(t *testing.T, h http.Handler, device, body string, wantStatus int) configPublishResponse {
	t.Helper()
	r := doRequest(t, h, http.MethodPost, fmt.Sprintf("/v1/devices/%s/configs", device), body)
	if r.Code != wantStatus {
		t.Fatalf("publish status = %d, want %d: %s", r.Code, wantStatus, r.Body.String())
	}
	if wantStatus == http.StatusCreated || wantStatus == http.StatusOK {
		return decodeBody[configPublishResponse](t, r)
	}
	return configPublishResponse{}
}

func postReceipt(t *testing.T, h http.Handler, device, body string, wantStatus int) configReceiptResponse {
	t.Helper()
	r := doRequest(t, h, http.MethodPost, fmt.Sprintf("/v1/devices/%s/configs/receipts", device), body)
	if r.Code != wantStatus {
		t.Fatalf("receipt status = %d, want %d: %s", r.Code, wantStatus, r.Body.String())
	}
	if wantStatus == http.StatusCreated || wantStatus == http.StatusOK {
		return decodeBody[configReceiptResponse](t, r)
	}
	return configReceiptResponse{}
}

func getPending(t *testing.T, h http.Handler, device string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, h, http.MethodGet, fmt.Sprintf("/v1/devices/%s/configs/pending", device), "")
}

func getConfigStatus(t *testing.T, h http.Handler, device string) configStatusResponse {
	t.Helper()
	r := doRequest(t, h, http.MethodGet, fmt.Sprintf("/v1/devices/%s/configs/status", device), "")
	if r.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", r.Code, r.Body.String())
	}
	return decodeBody[configStatusResponse](t, r)
}

func listConfigs(t *testing.T, h http.Handler, device string) []configViewResponse {
	t.Helper()
	r := doRequest(t, h, http.MethodGet, fmt.Sprintf("/v1/devices/%s/configs", device), "")
	if r.Code != http.StatusOK {
		t.Fatalf("list configs = %d: %s", r.Code, r.Body.String())
	}
	return decodeBody[struct {
		Configs []configViewResponse `json:"configs"`
	}](t, r).Configs
}

func listConfigReceipts(t *testing.T, h http.Handler, device string) []configReceiptResponse {
	t.Helper()
	r := doRequest(t, h, http.MethodGet, fmt.Sprintf("/v1/devices/%s/configs/receipts", device), "")
	if r.Code != http.StatusOK {
		t.Fatalf("list receipts = %d: %s", r.Code, r.Body.String())
	}
	return decodeBody[struct {
		Receipts []configReceiptResponse `json:"receipts"`
	}](t, r).Receipts
}

// --- publishing --------------------------------------------------------------

func TestConfigFreshDeviceReportsZeroVersions(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")

	if r := getPending(t, h, "gw"); r.Code != http.StatusNoContent || r.Body.Len() != 0 {
		t.Fatalf("pending = %d body=%q, want 204 empty", r.Code, r.Body.String())
	}
	status := getConfigStatus(t, h, "gw")
	if status.TargetVersion != 0 || status.AppliedVersion != 0 || status.FailureReason != "" {
		t.Fatalf("fresh status = %+v", status)
	}
	if configs := listConfigs(t, h, "gw"); len(configs) != 0 {
		t.Fatalf("fresh configs = %+v", configs)
	}
	if receipts := listConfigReceipts(t, h, "gw"); len(receipts) != 0 {
		t.Fatalf("fresh receipts = %+v", receipts)
	}
}

func TestConfigPublishCreatesNextVersionAndReturns201(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	first := publishConfig(t, h, "gw", publishBody("req-1", 0, `{"nested":{"a":[1,2,true,null]},"x":"y"}`), http.StatusCreated)
	if first.Version != 1 || first.RequestID != "req-1" {
		t.Fatalf("first = %+v", first)
	}
	if !sameJSON(first.Config, json.RawMessage(`{"nested":{"a":[1,2,true,null]},"x":"y"}`)) {
		t.Fatalf("content echoed wrong: %s", first.Config)
	}
	if !first.PublishedAt.Equal(clock.UTC()) {
		t.Fatalf("publishedAt = %s, want first publish time %s", first.PublishedAt, clock.UTC())
	}

	second := publishConfig(t, h, "gw", publishBody("req-2", 1, `{"v":2}`), http.StatusCreated)
	if second.Version != 2 {
		t.Fatalf("second version = %d, want 2", second.Version)
	}

	// Stale base conflicts and does not consume a version.
	stale := doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs", publishBody("req-3", 1, `{"v":9}`))
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale base = %d, want 409", stale.Code)
	}
	third := publishConfig(t, h, "gw", publishBody("req-4", 2, `{"v":3}`), http.StatusCreated)
	if third.Version != 3 {
		t.Fatalf("version after stale = %d, want 3 (loser must not consume)", third.Version)
	}
}

func TestConfigConcurrentPublishesOnSameBaseOnlyOneWins(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")

	const callers = 16
	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup
	codes := make(chan int, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait()
			body := publishBody(fmt.Sprintf("req-%d", i), 0, fmt.Sprintf(`{"i":%d}`, i))
			r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs", body)
			codes <- r.Code
		}(i)
	}
	start.Done()
	wg.Wait()
	close(codes)

	created, conflicts := 0, 0
	for code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if created != 1 || conflicts != callers-1 {
		t.Fatalf("created=%d conflicts=%d, want 1 and %d", created, conflicts, callers-1)
	}
	status := getConfigStatus(t, h, "gw")
	if status.TargetVersion != 1 {
		t.Fatalf("target = %d, want 1", status.TargetVersion)
	}
}

func TestConfigRequestDedupRetryReturns200FirstResult(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", publishBody("req-1", 0, `{"b":1,"a":2}`), http.StatusCreated)
	firstTime := clock.UTC()

	*clock = clock.Add(time.Hour)
	// Same requestId, base and content; reordered keys and different whitespace
	// are still a retry. Returns 200 with the ORIGINAL publish time.
	retry := publishConfig(t, h, "gw", publishBody("req-1", 0, `{ "a" : 2 , "b" : 1 }`), http.StatusOK)
	if retry.Version != 1 {
		t.Fatalf("retry version = %d, want 1 (no new version)", retry.Version)
	}
	if !retry.PublishedAt.Equal(firstTime) {
		t.Fatalf("retry publishedAt = %s, want first time %s", retry.PublishedAt, firstTime)
	}
	status := getConfigStatus(t, h, "gw")
	if status.TargetVersion != 1 {
		t.Fatalf("target after retry = %d, want 1", status.TargetVersion)
	}

	// Same requestId with a different base or content conflicts.
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs",
		publishBody("req-1", 0, `{"a":2,"b":3}`)); r.Code != http.StatusConflict {
		t.Fatalf("changed content status = %d, want 409", r.Code)
	}
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs",
		publishBody("req-1", 1, `{"a":2,"b":1}`)); r.Code != http.StatusConflict {
		t.Fatalf("changed base status = %d, want 409", r.Code)
	}
	// Numeric equality is by value, and array order is significant.
	publishConfig(t, h, "gw", publishBody("req-2", 1, `{"n":[1,2]}`), http.StatusCreated)
	publishConfig(t, h, "gw", publishBody("req-2", 1, `{"n":[1.0,2.0]}`), http.StatusOK)
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs",
		publishBody("req-2", 1, `{"n":[2,1]}`)); r.Code != http.StatusConflict {
		t.Fatalf("reordered array status = %d, want 409", r.Code)
	}
}

func TestConfigRequestDedupComparesNumbersExactly(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	publishConfig(t, h, "gw", publishBody("req-1", 0,
		`{"big":9007199254740992,"frac":0.1,"zero":0,"nested":{"a":[1,2]}}`), http.StatusCreated)
	firstTime := clock.UTC()
	*clock = clock.Add(time.Hour)

	// Numbers that only share a float64 (precision loss or underflow to zero)
	// are different content, at any nesting depth.
	changed := []string{
		`{"big":9007199254740993,"frac":0.1,"zero":0,"nested":{"a":[1,2]}}`,
		`{"big":9007199254740992,"frac":0.10000000000000001,"zero":0,"nested":{"a":[1,2]}}`,
		`{"big":9007199254740992,"frac":0.1,"zero":1e-400,"nested":{"a":[1,2]}}`,
		`{"big":9007199254740992,"frac":0.1,"zero":0,"nested":{"a":[1,9007199254740993]}}`,
		`{"big":9007199254740992,"frac":0.1,"zero":0,"nested":{"a":[1,"2"]}}`,
	}
	for _, config := range changed {
		if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs",
			publishBody("req-1", 0, config)); r.Code != http.StatusConflict {
			t.Fatalf("changed content %s => %d, want 409", config, r.Code)
		}
	}
	// The rejected retries created nothing and changed nothing.
	status := getConfigStatus(t, h, "gw")
	if status.TargetVersion != 1 || status.AppliedVersion != 0 {
		t.Fatalf("rejected retries changed state: %+v", status)
	}
	configs := listConfigs(t, h, "gw")
	if len(configs) != 1 || string(configs[0].Config) != `{"big":9007199254740992,"frac":0.1,"zero":0,"nested":{"a":[1,2]}}` {
		t.Fatalf("rejected retries rewrote history: %+v", configs)
	}

	// Same value in a different notation is still the same content: the retry
	// returns the first result with the first publish time and content.
	retry := publishConfig(t, h, "gw", publishBody("req-1", 0,
		`{ "zero" : -0.000 , "frac" : 0.10000 , "big" : 9007199254740992.0 , "nested" : { "a" : [ 1e0 , 2.000 ] } }`), http.StatusOK)
	if retry.Version != 1 || !retry.PublishedAt.Equal(firstTime) {
		t.Fatalf("retry = %+v, want version 1 at first time %s", retry, firstTime)
	}
	if string(retry.Config) != `{"big":9007199254740992,"frac":0.1,"zero":0,"nested":{"a":[1,2]}}` {
		t.Fatalf("retry rewrote stored content: %s", retry.Config)
	}
	if status := getConfigStatus(t, h, "gw"); status.TargetVersion != 1 {
		t.Fatalf("target after retry = %d, want 1", status.TargetVersion)
	}
}

func TestConfigRequestDedupRetryStaysFirstResultAfterNewerVersions(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	first := publishConfig(t, h, "gw", publishBody("req-1", 0, `{"v":9007199254740992}`), http.StatusCreated)
	*clock = clock.Add(time.Hour)
	publishConfig(t, h, "gw", publishBody("req-2", 1, `{"v":2}`), http.StatusCreated)
	postReceipt(t, h, "gw", receiptBody("rc-2", 2, true, ""), http.StatusCreated)
	*clock = clock.Add(time.Hour)

	// A legal retry of the old requestId still returns its own first result;
	// it neither creates a version nor moves the device back to the old target.
	retry := publishConfig(t, h, "gw", publishBody("req-1", 0, `{"v":9007199254740992.0}`), http.StatusOK)
	if retry.Version != 1 || !retry.PublishedAt.Equal(first.PublishedAt) {
		t.Fatalf("old retry = %+v, want first result %+v", retry, first)
	}
	status := getConfigStatus(t, h, "gw")
	if status.TargetVersion != 2 || status.AppliedVersion != 2 {
		t.Fatalf("old retry moved device state: %+v", status)
	}
	// And its content change is still a conflict, not a silent new publish.
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs",
		publishBody("req-1", 0, `{"v":9007199254740993}`)); r.Code != http.StatusConflict {
		t.Fatalf("changed old request = %d, want 409", r.Code)
	}
	if configs := listConfigs(t, h, "gw"); len(configs) != 2 {
		t.Fatalf("old retry created a version: %+v", configs)
	}
}

// --- numbers beyond the float64 range stay legal and exact -------------------

func TestConfigPublishKeepsOutOfRangeNumbersExact(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")

	// A legal JSON number larger than MaxFloat64 (or smaller than the smallest
	// positive subnormal) must publish at any nesting depth and come back
	// byte-for-byte, never as Infinity, zero, a string or a rounded value.
	large := `{"range":1e309,"nested":{"steps":[-1e309,1e-400]}}`
	first := publishConfig(t, h, "gw", publishBody("req-1", 0, large), http.StatusCreated)
	if first.Version != 1 {
		t.Fatalf("version = %d, want 1", first.Version)
	}
	if string(first.Config) != large {
		t.Fatalf("publish result changed number: %s", first.Config)
	}
	firstTime := clock.UTC()

	// History keeps the exact literals.
	if configs := listConfigs(t, h, "gw"); len(configs) != 1 || string(configs[0].Config) != large {
		t.Fatalf("history changed number: %+v", configs)
	}
	// The simulated device pull serves the exact literals as the pending target.
	r := getPending(t, h, "gw")
	if r.Code != http.StatusOK {
		t.Fatalf("pending = %d, want 200", r.Code)
	}
	if pending := decodeBody[configViewResponse](t, r); string(pending.Config) != large {
		t.Fatalf("pending changed number: %s", pending.Config)
	}

	// Same value in a different notation (10e308 == 1e309 exactly) is an
	// idempotent retry: 200 with the first version, time and stored content.
	*clock = clock.Add(time.Hour)
	retry := publishConfig(t, h, "gw", publishBody("req-1", 0,
		`{ "nested" : { "steps" : [ -10e308 , 10e-401 ] } , "range" : 10e308 }`), http.StatusOK)
	if retry.Version != 1 || !retry.PublishedAt.Equal(firstTime) {
		t.Fatalf("equal-value retry = %+v, want v1 at first time %s", retry, firstTime)
	}
	if string(retry.Config) != large {
		t.Fatalf("retry rewrote stored content: %s", retry.Config)
	}
	if status := getConfigStatus(t, h, "gw"); status.TargetVersion != 1 {
		t.Fatalf("retry created a version: %+v", status)
	}

	// A genuinely different out-of-range value conflicts and adds nothing.
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs",
		publishBody("req-1", 0, `{"range":2e309,"nested":{"steps":[-1e309,1e-400]}}`)); r.Code != http.StatusConflict {
		t.Fatalf("different value = %d, want 409", r.Code)
	}
	if configs := listConfigs(t, h, "gw"); len(configs) != 1 {
		t.Fatalf("409 added a version: %+v", configs)
	}
}

func TestConfigPublishRejectsNonFiniteNumberSpellings(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	for _, config := range []string{
		`{"x":NaN}`,
		`{"x":Infinity}`,
		`{"x":-Infinity}`,
		`{"nested":{"steps":[1e309,NaN]}}`,
	} {
		if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs",
			publishBody("r", 0, config)); r.Code != http.StatusBadRequest {
			t.Fatalf("config %s => %d, want 400: %s", config, r.Code, r.Body.String())
		}
	}
	// The rejected bodies changed no state, and the requestId they used (which
	// never committed) is free for a corrected publish.
	status := getConfigStatus(t, h, "gw")
	if status.TargetVersion != 0 || status.AppliedVersion != 0 {
		t.Fatalf("rejected publish changed state: %+v", status)
	}
	if configs := listConfigs(t, h, "gw"); len(configs) != 0 {
		t.Fatalf("rejected publish wrote history: %+v", configs)
	}
	publishConfig(t, h, "gw", publishBody("r", 0, `{"range":1e309}`), http.StatusCreated)
}

func TestConfigHistoryImmutableRollForwardByRePublishing(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", publishBody("r1", 0, `{"v":"old"}`), http.StatusCreated)
	publishConfig(t, h, "gw", publishBody("r2", 1, `{"v":"new"}`), http.StatusCreated)
	// Restore the old content: it becomes version 3; version 1 is untouched.
	rollback := publishConfig(t, h, "gw", publishBody("r3", 2, `{"v":"old"}`), http.StatusCreated)
	if rollback.Version != 3 {
		t.Fatalf("rollback version = %d, want 3", rollback.Version)
	}
	configs := listConfigs(t, h, "gw")
	if len(configs) != 3 {
		t.Fatalf("configs = %d, want 3", len(configs))
	}
	for i, view := range configs {
		if view.Version != int64(i+1) {
			t.Fatalf("config %d version = %d, want ascending %d", i, view.Version, i+1)
		}
	}
	if string(configs[0].Config) != `{"v":"old"}` || string(configs[2].Config) != `{"v":"old"}` {
		t.Fatalf("history mutated: %s / %s", configs[0].Config, configs[2].Config)
	}
}

func TestConfigPublishValidationFailures(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	cases := map[string]string{
		"blank requestId":     `{"requestId":"   ","baseVersion":0,"config":{"a":1}}`,
		"missing requestId":   `{"baseVersion":0,"config":{"a":1}}`,
		"missing baseVersion": `{"requestId":"r","config":{"a":1}}`,
		"negative base":       `{"requestId":"r","baseVersion":-1,"config":{"a":1}}`,
		"float base":          `{"requestId":"r","baseVersion":0.5,"config":{"a":1}}`,
		"text base":           `{"requestId":"r","baseVersion":"0","config":{"a":1}}`,
		"null base":           `{"requestId":"r","baseVersion":null,"config":{"a":1}}`,
		"missing config":      `{"requestId":"r","baseVersion":0}`,
		"empty object":        `{"requestId":"r","baseVersion":0,"config":{}}`,
		"array config":        `{"requestId":"r","baseVersion":0,"config":[1,2]}`,
		"scalar config":       `{"requestId":"r","baseVersion":0,"config":5}`,
		"null config":         `{"requestId":"r","baseVersion":0,"config":null}`,
		"string config":       `{"requestId":"r","baseVersion":0,"config":"x"}`,
		"not JSON":            `not json`,
		"two JSON objects":    `{"requestId":"r","baseVersion":0,"config":{"a":1}}{"requestId":"s","baseVersion":0,"config":{"b":2}}`,
		"unknown field":       `{"requestId":"r","baseVersion":0,"config":{"a":1},"nope":true}`,
		"config two objects":  `{"requestId":"r","baseVersion":0,"config":{"a":1}{"b":2}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs", body); r.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", r.Code, r.Body.String())
			}
		})
	}
	// Nested objects, arrays and ordinary JSON values are allowed.
	publishConfig(t, h, "gw", `{"requestId":"ok","baseVersion":0,"config":{"o":{"x":[1,"s",true,false,null,3.5]}}}`, http.StatusCreated)
}

func TestConfigUnknownDeviceIs404(t *testing.T) {
	h := NewHandler(NewStore())
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/ghost/configs",
		publishBody("r", 0, `{"a":1}`)); r.Code != http.StatusNotFound {
		t.Fatalf("publish unknown = %d, want 404", r.Code)
	}
	if r := getPending(t, h, "ghost"); r.Code != http.StatusNotFound {
		t.Fatalf("pending unknown = %d, want 404", r.Code)
	}
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/ghost/configs/receipts",
		receiptBody("x", 1, true, "")); r.Code != http.StatusNotFound {
		t.Fatalf("receipt unknown = %d, want 404 (even before version is known)", r.Code)
	}
	if r := doRequest(t, h, http.MethodGet, "/v1/devices/ghost/configs", ""); r.Code != http.StatusNotFound {
		t.Fatalf("list unknown = %d, want 404", r.Code)
	}
}

// --- pending reads -----------------------------------------------------------

func TestConfigPendingReturnsOnlyLatestTarget(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", publishBody("r1", 0, `{"v":1}`), http.StatusCreated)
	publishConfig(t, h, "gw", publishBody("r2", 1, `{"v":2}`), http.StatusCreated)
	publishConfig(t, h, "gw", publishBody("r3", 2, `{"v":3}`), http.StatusCreated)

	r := getPending(t, h, "gw")
	if r.Code != http.StatusOK {
		t.Fatalf("pending = %d, want 200", r.Code)
	}
	view := decodeBody[configViewResponse](t, r)
	if view.Version != 3 || string(view.Config) != `{"v":3}` {
		t.Fatalf("pending view = %+v, want latest v3 only", view)
	}

	// Repeated reads do not change state.
	r2 := getPending(t, h, "gw")
	if r2.Code != http.StatusOK {
		t.Fatalf("second read = %d", r2.Code)
	}
	status := getConfigStatus(t, h, "gw")
	if status.AppliedVersion != 0 || status.TargetVersion != 3 {
		t.Fatalf("reads changed state: %+v", status)
	}

	// Once applied, pending goes 204.
	postReceipt(t, h, "gw", receiptBody("ok-3", 3, true, ""), http.StatusCreated)
	if r := getPending(t, h, "gw"); r.Code != http.StatusNoContent {
		t.Fatalf("pending after apply = %d, want 204", r.Code)
	}
}

// --- receipts ----------------------------------------------------------------

func TestConfigReceiptSuccessAdvancesApplied(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", publishBody("r1", 0, `{"v":1}`), http.StatusCreated)

	receipt := postReceipt(t, h, "gw", receiptBody("rc-1", 1, true, ""), http.StatusCreated)
	if receipt.Version != 1 || !receipt.Success || receipt.Reason != "" || receipt.ReceivedAt.IsZero() {
		t.Fatalf("receipt = %+v", receipt)
	}
	status := getConfigStatus(t, h, "gw")
	if status.TargetVersion != 1 || status.AppliedVersion != 1 {
		t.Fatalf("status = %+v", status)
	}
	if r := getPending(t, h, "gw"); r.Code != http.StatusNoContent {
		t.Fatalf("pending = %d, want 204", r.Code)
	}
}

func TestConfigReceiptFailureKeepsTargetPendingWithReason(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", publishBody("r1", 0, `{"v":1}`), http.StatusCreated)

	fail := postReceipt(t, h, "gw", receiptBody("rc-f", 1, false, "disk full"), http.StatusCreated)
	if fail.Reason != "disk full" {
		t.Fatalf("failure receipt = %+v", fail)
	}
	status := getConfigStatus(t, h, "gw")
	if status.AppliedVersion != 0 || status.FailureReason != "disk full" {
		t.Fatalf("status after failure = %+v", status)
	}
	// Failure does not advance; latest config stays readable for retry.
	if r := getPending(t, h, "gw"); r.Code != http.StatusOK {
		t.Fatalf("pending after failure = %d, want 200", r.Code)
	}
	// A later success clears the failure reason.
	postReceipt(t, h, "gw", receiptBody("rc-ok", 1, true, ""), http.StatusCreated)
	status = getConfigStatus(t, h, "gw")
	if status.AppliedVersion != 1 || status.FailureReason != "" {
		t.Fatalf("status after recovery = %+v", status)
	}
}

func TestConfigFailureReasonRequiresNonBlankReason(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", publishBody("r1", 0, `{"v":1}`), http.StatusCreated)
	for _, body := range []string{
		`{"receiptId":"rc","version":1,"success":false}`,
		`{"receiptId":"rc","version":1,"success":false,"reason":"   "}`,
		`{"receiptId":"","version":1,"success":true}`,
		`{"receiptId":"rc","version":0,"success":true}`,
		`{"receiptId":"rc","version":1.5,"success":true}`,
		`{"receiptId":"rc","version":"1","success":true}`,
		`{"receiptId":"rc","version":1}`,
		`not json`,
		`{"receiptId":"rc","version":1,"success":true}{"receiptId":"x","version":1,"success":true}`,
	} {
		if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs/receipts", body); r.Code != http.StatusBadRequest {
			t.Fatalf("body %s => %d, want 400: %s", body, r.Code, r.Body.String())
		}
	}
}

func TestConfigReceiptUnknownVersionIs404(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", publishBody("r1", 0, `{"v":1}`), http.StatusCreated)
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs/receipts",
		receiptBody("rc", 2, true, "")); r.Code != http.StatusNotFound {
		t.Fatalf("future version = %d, want 404", r.Code)
	}
	// The rejected receipt must not be stored.
	if receipts := listConfigReceipts(t, h, "gw"); len(receipts) != 0 {
		t.Fatalf("404 receipt was stored: %+v", receipts)
	}
}

func TestConfigLaggingSuccessRecordedButTargetStaysPending(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", publishBody("r1", 0, `{"v":1}`), http.StatusCreated)
	publishConfig(t, h, "gw", publishBody("r2", 1, `{"v":2}`), http.StatusCreated)
	publishConfig(t, h, "gw", publishBody("r3", 2, `{"v":3}`), http.StatusCreated)

	// Device finally applies the version it had in hand (v2) while target is 3.
	postReceipt(t, h, "gw", receiptBody("ok-2", 2, true, ""), http.StatusCreated)
	status := getConfigStatus(t, h, "gw")
	if status.AppliedVersion != 2 || status.TargetVersion != 3 {
		t.Fatalf("status = %+v, want applied=2 target=3", status)
	}
	r := getPending(t, h, "gw")
	if r.Code != http.StatusOK {
		t.Fatalf("pending = %d, want 200 (new target still pending)", r.Code)
	}
	view := decodeBody[configViewResponse](t, r)
	if view.Version != 3 {
		t.Fatalf("pending offers v%d, want v3", view.Version)
	}

	// A new receipt below applied (v1) is 409; a failure for an already
	// successful version (v2) is 409 too.
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs/receipts",
		receiptBody("late-1", 1, true, "")); r.Code != http.StatusConflict {
		t.Fatalf("receipt below applied = %d, want 409", r.Code)
	}
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs/receipts",
		receiptBody("fail-2", 2, false, "nope")); r.Code != http.StatusConflict {
		t.Fatalf("failure for applied version = %d, want 409", r.Code)
	}
}

func TestConfigReceiptDedup(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", publishBody("r1", 0, `{"v":1}`), http.StatusCreated)
	first := postReceipt(t, h, "gw", receiptBody("rc-f", 1, false, "boom"), http.StatusCreated)
	firstReceived := first.ReceivedAt

	*clock = clock.Add(time.Hour)
	// Same receiptId + same content retries with 200 and the first time; it
	// must not be appended again.
	retry := postReceipt(t, h, "gw", receiptBody("rc-f", 1, false, "boom"), http.StatusOK)
	if !retry.ReceivedAt.Equal(firstReceived) {
		t.Fatalf("retry receivedAt = %s, want first %s", retry.ReceivedAt, firstReceived)
	}
	receipts := listConfigReceipts(t, h, "gw")
	if len(receipts) != 1 {
		t.Fatalf("receipts = %d, want 1", len(receipts))
	}

	// Same id, different content => 409 in every direction.
	for _, body := range []string{
		receiptBody("rc-f", 1, true, ""),
		receiptBody("rc-f", 1, false, "other"),
	} {
		if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/configs/receipts", body); r.Code != http.StatusConflict {
			t.Fatalf("changed receipt %s => %d, want 409", body, r.Code)
		}
	}
	if receipts := listConfigReceipts(t, h, "gw"); len(receipts) != 1 {
		t.Fatalf("409 retries changed receipts: %+v", receipts)
	}

	// A different receiptId can report the later success.
	postReceipt(t, h, "gw", receiptBody("rc-ok", 1, true, ""), http.StatusCreated)
	// Re-reporting the old failure id still retries as 200 (stored first result)
	// rather than becoming a new conflict against applied state.
	postReceipt(t, h, "gw", receiptBody("rc-f", 1, false, "boom"), http.StatusOK)
}

// --- isolation and ordering --------------------------------------------------

func TestConfigVersionsAndDedupArePerDevice(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "a")
	mustRegister(t, h, "b")
	body := publishBody("shared", 0, `{"v":1}`)
	publishConfig(t, h, "a", body, http.StatusCreated)
	publishConfig(t, h, "b", body, http.StatusCreated)
	if sa, sb := getConfigStatus(t, h, "a"), getConfigStatus(t, h, "b"); sa.TargetVersion != 1 || sb.TargetVersion != 1 {
		t.Fatalf("statuses = %+v %+v", sa, sb)
	}
	// Each device independently applies and keeps its own receipts.
	postReceipt(t, h, "a", receiptBody("shared-rc", 1, true, ""), http.StatusCreated)
	postReceipt(t, h, "b", receiptBody("shared-rc", 1, false, "x"), http.StatusCreated)
	if sa, sb := getConfigStatus(t, h, "a"), getConfigStatus(t, h, "b"); sa.AppliedVersion != 1 || sb.AppliedVersion != 0 {
		t.Fatalf("cross-device leak: %+v %+v", sa, sb)
	}
}

func TestConfigReceiptsListedInReceiveOrder(t *testing.T) {
	h := NewHandler(NewStore())
	mustRegister(t, h, "gw")
	publishConfig(t, h, "gw", publishBody("r1", 0, `{"v":1}`), http.StatusCreated)
	postReceipt(t, h, "gw", receiptBody("rc-1", 1, false, "a"), http.StatusCreated)
	postReceipt(t, h, "gw", receiptBody("rc-2", 1, false, "b"), http.StatusCreated)
	postReceipt(t, h, "gw", receiptBody("rc-3", 1, true, ""), http.StatusCreated)
	receipts := listConfigReceipts(t, h, "gw")
	want := []string{"rc-1", "rc-2", "rc-3"}
	if len(receipts) != 3 {
		t.Fatalf("receipts = %d, want 3", len(receipts))
	}
	for i, id := range want {
		if receipts[i].ReceiptID != id {
			t.Fatalf("order[%d] = %s, want %s", i, receipts[i].ReceiptID, id)
		}
	}
}

// --- config operations leave the other subsystems alone ----------------------

func TestConfigOperationsDoNotTouchDeviceActivityOrTelemetry(t *testing.T) {
	store, clock := newClockStore()
	h := NewHandler(store)
	mustRegister(t, h, "gw")
	*clock = clock.Add(2 * time.Hour)
	telemetryAt := clock.UTC()
	if r := doRequest(t, h, http.MethodPost, "/v1/devices/gw/telemetry", `{"temperature":10}`); r.Code != http.StatusAccepted {
		t.Fatalf("telemetry = %d", r.Code)
	}
	beforeEvents := allEvents(t, h, "gw")
	if len(beforeEvents) != 1 {
		t.Fatalf("seed events = %d", len(beforeEvents))
	}

	*clock = clock.Add(48 * time.Hour)
	publishConfig(t, h, "gw", publishBody("r1", 0, `{"v":1}`), http.StatusCreated)
	getPending(t, h, "gw")
	postReceipt(t, h, "gw", receiptBody("rc-f", 1, false, "boom"), http.StatusCreated)
	publishConfig(t, h, "gw", publishBody("r2", 1, `{"v":2}`), http.StatusCreated)
	postReceipt(t, h, "gw", receiptBody("rc-ok", 2, true, ""), http.StatusCreated)

	snapshot := decodeBody[struct {
		Devices []Device `json:"devices"`
	}](t, doRequest(t, h, http.MethodGet, "/v1/fleet", ""))
	if got := snapshot.Devices[0]; !got.LastSeenAt.Equal(telemetryAt) {
		t.Fatalf("lastSeenAt = %s, want telemetry time %s (config ops must not refresh it)", got.LastSeenAt, telemetryAt)
	}
	events := allEvents(t, h, "gw")
	if len(events) != 1 {
		t.Fatalf("telemetry history changed: %+v", events)
	}
}
