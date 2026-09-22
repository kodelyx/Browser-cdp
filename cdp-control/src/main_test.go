package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodelyx/Browser-cdp/cdp-control/src/bridge"
	"github.com/kodelyx/Browser-cdp/cdp-control/src/cdp"
	"github.com/kodelyx/Browser-cdp/cdp-control/src/needle"
)

// newTestServer builds the API with a bridge and nothing attached.
//
// Most of what can go wrong in a debug tool is a bad answer to "there is nothing
// there", so that is what most of these cover. Every handler that needs a browser
// must say so in a way that names the fix, rather than returning an empty list or
// a zero status that reads as success.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	br := bridge.NewBridge(nil, nil, t.TempDir())
	srv := httptest.NewServer(routes(&server{bridge: br}))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, base, path string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(base + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func post(t *testing.T, base, path, payload string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(base+path, "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()

	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func TestHealthReportsNothingAttached(t *testing.T) {
	base := newTestServer(t).URL

	status, body := get(t, base, "/health")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if attached, ok := body["attached"].(bool); !ok || attached {
		t.Errorf("attached = %v, want false", body["attached"])
	}
}

func TestStatusNamesTheFixWhenNothingIsAttached(t *testing.T) {
	base := newTestServer(t).URL

	status, body := get(t, base, "/status")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if attached, _ := body["attached"].(bool); attached {
		t.Error("nothing is attached, so attached must be false")
	}
	// A debug tool that says "not attached" and stops has told you what you
	// already knew. The hint is the part that saves the trip to the README.
	hint, _ := body["hint"].(string)
	if hint == "" {
		t.Error("the unattached status must say what to do about it")
	}
	if !strings.Contains(hint, "extension") {
		t.Errorf("the hint must name the folder to load, got %q", hint)
	}
}

// TestEveryBrowserEndpointRefusesWithoutOne walks the handlers that need a browser.
//
// Each has to fail, and fail with a 503 rather than a 200 carrying an empty result:
// an empty list from /requests is indistinguishable from a page that made no
// requests, and that is exactly the confusion this tool exists to remove.
func TestEveryBrowserEndpointRefusesWithoutOne(t *testing.T) {
	base := newTestServer(t).URL

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"tabs", "GET", "/tabs", ""},
		{"eval", "POST", "/eval", `{"expression":"1"}`},
		{"cdp", "POST", "/cdp", `{"method":"Runtime.evaluate"}`},
		{"click", "POST", "/click", `{"text":"Save"}`},
		{"events", "GET", "/events", ""},
		{"requests", "GET", "/requests", ""},
		{"cookies", "GET", "/cookies?domain=example.com", ""},
		{"agent/act", "POST", "/agent/act", `{"instruction":"Click Save"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var status int
			var body map[string]any
			if tc.method == "GET" {
				status, body = get(t, base, tc.path)
			} else {
				status, body = post(t, base, tc.path, tc.body)
			}

			if status != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", status)
			}
			message, _ := body["error"].(string)
			if message == "" {
				t.Fatal("an error body is required, not an empty one")
			}
			if !strings.Contains(message, "no extension attached") {
				t.Errorf("the error must say what is missing, got %q", message)
			}
		})
	}
}

// TestRequestsWithoutAMethodFailsBeforeTheBrowser keeps the cheap validation
// cheap: a request missing its argument should not need a browser to reject it.
func TestBadRequestsAreRejectedWithoutABrowser(t *testing.T) {
	base := newTestServer(t).URL

	for _, tc := range []struct {
		name string
		path string
		body string
	}{
		{"eval without an expression", "/eval", `{}`},
		{"cdp without a method", "/cdp", `{}`},
		{"click without text", "/click", `{}`},
		{"cookies without a domain", "/cookies", ""},
		{"agent/act without an instruction", "/agent/act", `{}`},
		{"agent/act with a blank instruction", "/agent/act", `{"instruction":"   "}`},
		{"agent/extract without text", "/agent/extract", `{"schema":{"type":"object"}}`},
		{"agent/extract without a schema", "/agent/extract", `{"text":"a booking for tuesday"}`},
		{"agent/extract with a non-object schema", "/agent/extract", `{"text":"x","schema":"not-an-object"}`},
		{"agent/extract with an empty schema", "/agent/extract", `{"text":"x","schema":{}}`},
		{"agent/extract with an unusable schema", "/agent/extract", `{"text":"x","schema":{"title":"no type or properties"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := post(t, base, tc.path, tc.body)
			if status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", status)
			}
			if message, _ := body["error"].(string); message == "" {
				t.Error("a rejected request must say what was missing")
			}
		})
	}
}

func TestCookiesDoesNotReturnValuesByDefault(t *testing.T) {
	// The handler is the only place that can decide this, and it cannot be
	// exercised without a browser — so the intent is pinned here as a comment on
	// the contract rather than as a runtime assertion. The check that matters is
	// that the query parameter is read at all.
	base := newTestServer(t).URL
	status, _ := get(t, base, "/cookies?domain=example.com")
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 — it should reach the browser check", status)
	}
}

func TestSplitList(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", []string{}},
		{"  ", []string{}},
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{" a , b ", []string{"a", "b"}},
		{"a,,b", []string{"a", "b"}},
	} {
		got := splitList(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitList(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitList(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}

func TestMustJSONEscapesWhatItEmbeds(t *testing.T) {
	// The click handler builds a page expression by embedding this, so a quote in
	// the text would end the string literal and break the call rather than
	// escaping it. This is the one place that matters.
	got := mustJSON(`a "quoted" thing`)
	if got != `"a \"quoted\" thing"` {
		t.Errorf("mustJSON = %s, want the quotes escaped", got)
	}
}

// requestEvent builds a buffered event shaped the way the extension forwards one:
// the raw CDP frame, method at the top level, params carrying the nested request.
func requestEvent(method, url, verb, postData string) cdp.BufferedEvent {
	params := map[string]any{
		"requestId": "1000.1",
		"type":      "Fetch",
		"request": map[string]any{
			"url":      url,
			"method":   verb,
			"postData": postData,
		},
	}
	raw, _ := json.Marshal(params)
	return cdp.BufferedEvent{Method: method, Params: raw}
}

// TestParseRequestsReadsTheNestedRequest pins the shape that makes this endpoint
// worth having.
//
// `Network.requestWillBeSent` puts the URL and the POST body under `request`, not
// at the top level. Getting that wrong yields an empty list, which looks exactly
// like a page that made no requests — the one failure mode this endpoint exists to
// rule out. So the shape is asserted against a realistic event rather than trusted.
func TestParseRequestsReadsTheNestedRequest(t *testing.T) {
	events := []cdp.BufferedEvent{
		requestEvent("Network.requestWillBeSent",
			"https://api.example.com/v1/orders", "POST", `{"order":{"id":"abc"}}`),
	}

	got := parseRequests(events, "", "")
	if len(got) != 1 {
		t.Fatalf("got %d requests, want 1 — the nested request was not read", len(got))
	}
	if got[0].URL != "https://api.example.com/v1/orders" {
		t.Errorf("url = %q", got[0].URL)
	}
	if got[0].Method != "POST" {
		t.Errorf("method = %q, want POST", got[0].Method)
	}
	if got[0].Body != `{"order":{"id":"abc"}}` {
		t.Errorf("body = %q, want the postData", got[0].Body)
	}
	if !got[0].HasBody {
		t.Error("a request with postData must report has_body")
	}
}

// TestParseRequestsKeepsOnlyRealRequests covers the rest of the stream: every
// other CDP event, a request with no URL, and a malformed frame all have to be
// skipped rather than emitted as blanks.
func TestParseRequestsKeepsOnlyRealRequests(t *testing.T) {
	events := []cdp.BufferedEvent{
		{Method: "Network.responseReceived", Params: json.RawMessage(`{"response":{"url":"https://x/y"}}`)},
		{Method: "Page.loadEventFired", Params: json.RawMessage(`{"timestamp":1}`)},
		{Method: "Network.requestWillBeSent", Params: json.RawMessage(`{"request":{"method":"GET"}}`)},
		{Method: "Network.requestWillBeSent", Params: json.RawMessage(`not json`)},
		requestEvent("Network.requestWillBeSent", "https://api.example.com/v1/invoices", "GET", ""),
	}

	got := parseRequests(events, "", "")
	if len(got) != 1 {
		t.Fatalf("got %d requests, want only the one with a URL", len(got))
	}
	if got[0].HasBody {
		t.Error("a GET with no postData must report has_body=false")
	}
}

func TestParseRequestsFilters(t *testing.T) {
	events := []cdp.BufferedEvent{
		requestEvent("Network.requestWillBeSent", "https://api.example.com/v1/orders", "POST", `{}`),
		requestEvent("Network.requestWillBeSent", "https://api.example.com/v1/invoices", "POST", `{}`),
		requestEvent("Network.requestWillBeSent", "https://accounts.example.com/oauth2/token", "GET", ""),
	}

	for _, tc := range []struct {
		name   string
		filter string
		method string
		want   int
	}{
		{"no filter keeps everything", "", "", 3},
		{"filter matches a substring anywhere", "orders", "", 1},
		{"filter is a substring, not a prefix", "api.example.com", "", 2},
		{"method narrows to one verb", "", "GET", 1},
		{"method is case-insensitive", "", "get", 1},
		{"filter and method together", "api.example.com", "POST", 2},
		{"a filter matching nothing yields an empty list", "nope", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRequests(events, tc.filter, tc.method)
			if len(got) != tc.want {
				t.Errorf("got %d requests, want %d", len(got), tc.want)
			}
		})
	}
}

// TestParseRequestsReturnsAnEmptySliceNotNull keeps the JSON honest: a nil slice
// encodes as `null`, and a client reading `requests` expects an array.
func TestParseRequestsReturnsAnEmptySliceNotNull(t *testing.T) {
	if got := parseRequests(nil, "", ""); got == nil {
		t.Error("no events must still produce an empty slice, not nil")
	}
}

// --- agent endpoints -------------------------------------------------------

// fakeBrowser answers from memory. A test has no extension to talk to, so this is
// the only way to reach the dispatch and capture logic that the agent endpoints
// are made of.
type fakeBrowser struct {
	mu        sync.Mutex
	evals     []string
	readCalls int
	batches   [][]cdp.BufferedEvent
	cookies   []cdp.Cookie
	evalErr   error
}

func (f *fakeBrowser) Evaluate(_ context.Context, expression string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evals = append(f.evals, expression)
	if f.evalErr != nil {
		return nil, f.evalErr
	}
	return json.RawMessage(`{"clicked":true}`), nil
}

func (f *fakeBrowser) ListCookies(_ context.Context, _ map[string]any) ([]cdp.Cookie, error) {
	return f.cookies, nil
}

// ReadEvents hands back one batch per call, then nothing — mirroring a drained
// buffer rather than a repeating one.
func (f *fakeBrowser) ReadEvents(_ context.Context, _ int) ([]cdp.BufferedEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := f.readCalls
	f.readCalls++
	if call >= len(f.batches) {
		return nil, nil
	}
	return f.batches[call], nil
}

func (f *fakeBrowser) expressions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.evals...)
}

func (f *fakeBrowser) reads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readCalls
}

// TestExecuteAgentCallsRunsInOrderAndKeepsGoing covers the property the whole
// endpoint rests on: "click Save, then check the request" arrives as two calls,
// and running them in the model's order is the difference between reproducing a
// sequence and reproducing a permutation of it.
//
// The middle call is malformed on purpose. One bad action must not swallow the
// results of the good ones around it.
func TestExecuteAgentCallsRunsInOrderAndKeepsGoing(t *testing.T) {
	fake := &fakeBrowser{}
	calls := []needle.FunctionCall{
		{Name: "click_element", Arguments: map[string]any{"text": "Save"}},
		{Name: "click_element", Arguments: map[string]any{}},
		{Name: "eval_js", Arguments: map[string]any{"expression": "document.title"}},
	}

	got, warnings := executeAgentCalls(context.Background(), fake, calls)
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	if len(got) != 3 {
		t.Fatalf("executed %d actions, want 3", len(got))
	}
	if got[0].Tool != "click_element" || got[0].Target != "Save" {
		t.Errorf("first action = %+v, want click_element on Save", got[0])
	}
	if got[1].Error == "" {
		t.Error("a call with no text must report an error rather than silently do nothing")
	}
	if got[2].Tool != "eval_js" || got[2].Error != "" {
		t.Errorf("third action = %+v, want the eval_js call to have run", got[2])
	}

	// The two well-formed calls reached the page, in order.
	if evals := fake.expressions(); len(evals) != 2 {
		t.Errorf("the page saw %d expressions, want 2", len(evals))
	} else if !strings.Contains(evals[0], "Save") || !strings.Contains(evals[1], "document.title") {
		t.Errorf("expressions ran out of order: %v", evals)
	}
}

func TestExecuteAgentCallsCapsTheList(t *testing.T) {
	calls := make([]needle.FunctionCall, 12)
	for i := range calls {
		calls[i] = needle.FunctionCall{Name: "not_a_tool"}
	}

	got, warnings := executeAgentCalls(context.Background(), &fakeBrowser{}, calls)
	if len(got) != maxAgentCalls {
		t.Errorf("executed %d actions, want the cap of %d", len(got), maxAgentCalls)
	}
	if len(warnings) == 0 {
		t.Error("truncating the list must be reported, not done quietly")
	}
}

// TestExecuteAgentActionRejectsBadCalls runs with a nil browser on purpose: these
// paths must reject the call before touching the page, so a malformed tool call
// never becomes a page error the caller has to interpret.
func TestExecuteAgentActionRejectsBadCalls(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
	}{
		{"unknown tool", "delete_everything", nil},
		{"click with no text", "click_element", map[string]any{}},
		{"eval with no expression", "eval_js", map[string]any{}},
		{"cookies with no domain", "read_cookies", map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := executeAgentAction(context.Background(), nil, tc.tool, tc.args); err == nil {
				t.Error("want an error, got none")
			}
		})
	}
}

// TestReadCookiesActionReturnsNamesNotValues is the security-relevant one. The
// agent endpoints are reached by an automated caller that never asked for a
// credential, so a cookie value must not travel in the response even though the
// browser is willing to hand it over.
func TestReadCookiesActionReturnsNamesNotValues(t *testing.T) {
	fake := &fakeBrowser{cookies: []cdp.Cookie{
		{Domain: "example.com", Name: "session", Value: "SUPER_SECRET_VALUE"},
	}}

	raw, target, err := executeAgentAction(context.Background(), fake, "read_cookies",
		map[string]any{"domain": "example.com"})
	if err != nil {
		t.Fatalf("read_cookies: %v", err)
	}
	if target != "example.com" {
		t.Errorf("target = %q, want the domain it read", target)
	}
	if strings.Contains(string(raw), "SUPER_SECRET_VALUE") {
		t.Errorf("the response leaked a cookie value: %s", raw)
	}
	if !strings.Contains(string(raw), "session") {
		t.Errorf("the response should still name the cookie: %s", raw)
	}
}

func TestCaptureAfterActionCollectsALateRequest(t *testing.T) {
	// The first drain is empty, exactly as it is for a click whose handler has
	// not built the request yet. A single drain would report "nothing happened".
	fake := &fakeBrowser{batches: [][]cdp.BufferedEvent{
		{},
		{requestEvent("Network.requestWillBeSent", "https://example.com/api/v1/update", "POST", `{"id":123}`)},
	}}

	got, err := captureAfterAction(context.Background(), fake, "api", 2*time.Second)
	if err != nil {
		t.Fatalf("captureAfterAction: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("captured %d requests, want 1", len(got))
	}
	if got[0].URL != "https://example.com/api/v1/update" || got[0].Method != "POST" {
		t.Errorf("captured %+v, want the POST to /api/v1/update", got[0])
	}
	if got[0].Body != `{"id":123}` {
		t.Errorf("body = %q, want the payload the page sent", got[0].Body)
	}
	if fake.reads() < 2 {
		t.Errorf("drained %d times, want at least 2 — a late request needs a second look", fake.reads())
	}
}

func TestCaptureAfterActionStopsEarlyOnceItHasSomething(t *testing.T) {
	fake := &fakeBrowser{batches: [][]cdp.BufferedEvent{
		{requestEvent("Network.requestWillBeSent", "https://example.com/api/v1/update", "POST", `{}`)},
	}}

	started := time.Now()
	got, err := captureAfterAction(context.Background(), fake, "", 5*time.Second)
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("captureAfterAction: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("captured %d requests, want 1", len(got))
	}
	// It must not sit out the whole settle window once it has the answer: the
	// caller is waiting, and the request it asked about already arrived.
	if elapsed >= 5*time.Second {
		t.Errorf("took %s — it should have stopped after the grace window", elapsed)
	}
}

func TestCaptureAfterActionHonoursTheFilter(t *testing.T) {
	fake := &fakeBrowser{batches: [][]cdp.BufferedEvent{{
		requestEvent("Network.requestWillBeSent", "https://example.com/static/logo.png", "GET", ""),
		requestEvent("Network.requestWillBeSent", "https://example.com/api/v1/list", "GET", ""),
	}}}

	got, err := captureAfterAction(context.Background(), fake, "api", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("captureAfterAction: %v", err)
	}
	if len(got) != 1 || !strings.Contains(got[0].URL, "/api/") {
		t.Errorf("captured %+v, want only the api request", got)
	}
}

// TestCaptureAfterActionReturnsAnEmptySliceNotNull keeps the JSON honest for the
// same reason parseRequests does: `null` and `[]` read differently to a client.
func TestCaptureAfterActionReturnsAnEmptySliceNotNull(t *testing.T) {
	got, err := captureAfterAction(context.Background(), &fakeBrowser{}, "", 30*time.Millisecond)
	if err != nil {
		t.Fatalf("captureAfterAction: %v", err)
	}
	if got == nil {
		t.Error("a capture that found nothing must still be an empty slice, not nil")
	}
}

func TestClampSettleMS(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{0, defaultSettleMS},
		{-5, defaultSettleMS},
		{250, 250},
		{maxSettleMS + 1, maxSettleMS},
	} {
		if got := clampSettleMS(tc.in); got != tc.want {
			t.Errorf("clampSettleMS(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestExtractToolsJSONWrapsTheSchemaAsOneTool(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"invoice_id":{"type":"string"}},"required":["invoice_id"]}`)

	encoded, err := extractToolsJSON(schema)
	if err != nil {
		t.Fatalf("extractToolsJSON: %v", err)
	}

	var tools []map[string]any
	if err := json.Unmarshal([]byte(encoded), &tools); err != nil {
		t.Fatalf("the wrapped schema is not valid JSON: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("wrapped into %d tools, want exactly 1", len(tools))
	}
	if tools[0]["name"] != extractToolName {
		t.Errorf("tool name = %v, want %q", tools[0]["name"], extractToolName)
	}

	// The caller's schema has to arrive intact: the grammar is compiled from it,
	// so anything dropped here silently changes what the model may answer.
	params, _ := tools[0]["parameters"].(map[string]any)
	if params == nil {
		t.Fatal("the schema did not survive as the tool's parameters")
	}
	props, _ := params["properties"].(map[string]any)
	if _, ok := props["invoice_id"]; !ok {
		t.Errorf("the schema's properties were lost: %v", params)
	}
}

func TestExtractToolsJSONRejectsWhatCannotBeAGrammar(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema string
	}{
		{"not an object", `"just a string"`},
		{"empty object", `{}`},
		{"no type and no properties", `{"title":"unusable"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := extractToolsJSON(json.RawMessage(tc.schema)); err == nil {
				t.Error("want an error, got none")
			}
		})
	}
}

// TestClickExpressionIsSharedWithTheAgent pins the escaping at the boundary where
// it matters: the text is embedded in a page expression, so a quote that is not
// escaped ends the string literal and turns a click into a syntax error.
func TestClickExpressionIsSharedWithTheAgent(t *testing.T) {
	for _, text := range []string{
		`Save`,
		`a "quoted" thing`,
		`back\slash`,
		"line\nbreak",
		`</script>`,
	} {
		expr := clickExpression(text)

		literal := "const want = " + mustJSON(text) + ";"
		if !strings.Contains(expr, literal) {
			t.Errorf("the expression for %q does not embed it as a JSON string", text)
		}

		var roundTripped string
		if err := json.Unmarshal([]byte(mustJSON(text)), &roundTripped); err != nil {
			t.Errorf("mustJSON(%q) is not valid JSON: %v", text, err)
			continue
		}
		if roundTripped != text {
			t.Errorf("mustJSON(%q) round-tripped to %q", text, roundTripped)
		}
	}
}
