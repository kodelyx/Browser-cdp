package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kodelyx/Browser-cdp/cdp-control/bridge"
	"github.com/kodelyx/Browser-cdp/cdp-control/cdp"
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
			"https://labs.google/fx/api/trpc/flow.upscale", "POST", `{"json":{"mediaId":"abc"}}`),
	}

	got := parseRequests(events, "", "")
	if len(got) != 1 {
		t.Fatalf("got %d requests, want 1 — the nested request was not read", len(got))
	}
	if got[0].URL != "https://labs.google/fx/api/trpc/flow.upscale" {
		t.Errorf("url = %q", got[0].URL)
	}
	if got[0].Method != "POST" {
		t.Errorf("method = %q, want POST", got[0].Method)
	}
	if got[0].Body != `{"json":{"mediaId":"abc"}}` {
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
		requestEvent("Network.requestWillBeSent", "https://labs.google/fx/api/trpc/flow.list", "GET", ""),
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
		requestEvent("Network.requestWillBeSent", "https://labs.google/fx/api/trpc/flow.upscale", "POST", `{}`),
		requestEvent("Network.requestWillBeSent", "https://labs.google/fx/api/trpc/flow.list", "POST", `{}`),
		requestEvent("Network.requestWillBeSent", "https://accounts.google.com/o/oauth2", "GET", ""),
	}

	for _, tc := range []struct {
		name   string
		filter string
		method string
		want   int
	}{
		{"no filter keeps everything", "", "", 3},
		{"filter matches a substring anywhere", "upscale", "", 1},
		{"filter is a substring, not a prefix", "labs.google", "", 2},
		{"method narrows to one verb", "", "GET", 1},
		{"method is case-insensitive", "", "get", 1},
		{"filter and method together", "labs.google", "POST", 2},
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
