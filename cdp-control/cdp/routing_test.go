package cdp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

/*
 * Which surface the client talks to.
 *
 * Two extensions speak this protocol and they are not interchangeable. The
 * generic one offers `cdp.call` / `cdp.evaluate` — arbitrary DevTools access —
 * while the Flow one offers the six operations the backend actually needs and
 * nothing else. The client has to work with either, so it asks `ping` for an
 * `ops` list and routes accordingly.
 *
 * That routing is the part worth pinning down, because both directions fail
 * quietly. Choose the fallback when the named operation exists and the backend
 * ships a generic escape hatch to a user's browser anyway; choose the named
 * operation when it does not exist and every call fails with an unhelpful
 * "unknown operation". Neither throws at build time, and neither shows up
 * without a browser attached — so the decision is asserted here instead, over a
 * real socket, against a fake extension that records what it was asked for.
 */

// fakeExtension is the extension side of the protocol.
type fakeExtension struct {
	// flowOps advertises the flow.* operations, the way flow-go-extension does.
	// Left false, it answers ping like the generic bridge: no ops list.
	flowOps bool

	mu     sync.Mutex
	ops    []string
	params map[string]json.RawMessage
}

func (f *fakeExtension) record(op string, params json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, op)
	if f.params == nil {
		f.params = map[string]json.RawMessage{}
	}
	f.params[op] = params
}

func (f *fakeExtension) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, seen := range f.ops {
		if seen == op {
			n++
		}
	}
	return n
}

func (f *fakeExtension) called(op string) bool { return f.count(op) > 0 }

func (f *fakeExtension) paramsFor(op string) json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.params[op]
}

func (f *fakeExtension) answer(op string) any {
	switch op {
	case "ping":
		pong := map[string]any{"ok": true, "protocol": 1, "version": "test"}
		if f.flowOps {
			pong["ops"] = []string{
				"ping", "config.get", "config.set", "tabs.list", "tabs.open",
				"tab.attach", "tab.detach", "tab.current", "events.read",
				"cookies.list", "flow.fingerprint", "flow.navigate",
				"flow.projects", "flow.captcha", "flow.upscale",
			}
		}
		return pong

	case "flow.fingerprint":
		return map[string]any{
			"userAgent": "Mozilla/5.0 (from the named operation)", "language": "en-US",
			"brands": `"Chromium";v="140"`, "platform": "macOS",
			"mobile": "?0", "platformFull": `"macOS"`,
		}
	case "flow.projects":
		return []string{"https://flow.google.com/project/one"}
	case "flow.navigate":
		return map[string]any{"tabId": 1, "url": "https://flow.google.com/"}
	case "flow.captcha":
		return map[string]any{"available": true, "token": strings.Repeat("t", 40)}
	case "flow.upscale":
		return map[string]any{"data": strings.Repeat("A", 600)}

	case "cdp.evaluate":
		// The expression the fallback path would have run.
		return json.RawMessage(`{"userAgent":"from-the-fallback-expression"}`)
	}
	return map[string]any{}
}

// serve runs the fake extension and returns a client connected to it.
func (f *fakeExtension) serve(t *testing.T) *Client {
	t.Helper()

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		for {
			var req wireRequest
			if err := conn.ReadJSON(&req); err != nil {
				return
			}
			var params json.RawMessage
			if req.Params != nil {
				params, _ = json.Marshal(req.Params)
			}
			f.record(req.Op, params)

			frame := map[string]any{"id": req.ID, "result": f.answer(req.Op)}
			if err := conn.WriteJSON(frame); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err != nil {
		t.Fatalf("dial the fake extension: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestFlowOperationsAreUsedWhenAdvertised(t *testing.T) {
	ext := &fakeExtension{flowOps: true}
	client := ext.serve(t)
	ctx := context.Background()

	raw, err := client.FlowFingerprint(ctx, "FALLBACK-EXPRESSION")
	if err != nil {
		t.Fatalf("FlowFingerprint: %v", err)
	}
	if !ext.called("flow.fingerprint") {
		t.Error("the named operation should have been used")
	}
	if ext.called("cdp.evaluate") {
		t.Error("the fallback expression must not run when the extension implements the operation")
	}

	// The named operation's answer is what comes back, not the expression's.
	if !strings.Contains(string(raw), "from the named operation") {
		t.Errorf("unexpected fingerprint: %s", raw)
	}
}

func TestFlowOperationsFallBackOnTheGenericExtension(t *testing.T) {
	ext := &fakeExtension{flowOps: false}
	client := ext.serve(t)
	ctx := context.Background()

	raw, err := client.FlowFingerprint(ctx, "FALLBACK-EXPRESSION")
	if err != nil {
		t.Fatalf("FlowFingerprint: %v", err)
	}
	if !ext.called("cdp.evaluate") {
		t.Error("the generic extension has no flow.fingerprint, so the expression should run")
	}
	if ext.called("flow.fingerprint") {
		t.Error("the named operation must not be attempted when ping does not advertise it")
	}
	if !strings.Contains(string(raw), "from-the-fallback-expression") {
		t.Errorf("unexpected fingerprint: %s", raw)
	}
}

// TestFlowOperationWithoutAFallbackIsUnavailable covers the operation the
// generic extension cannot serve at all. It has no expression to fall back to,
// so the call has to fail rather than silently do nothing.
func TestFlowOperationWithoutAFallbackIsUnavailable(t *testing.T) {
	ext := &fakeExtension{flowOps: false}
	client := ext.serve(t)

	_, err := client.FlowProjects(context.Background(), "")
	if err == nil {
		t.Fatal("an operation with no fallback should be reported as unavailable")
	}
	if !strings.Contains(err.Error(), "does not implement") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestFlowCapabilityProbeIsCachedOnce: the probe is a round trip, so it must
// happen once per connection rather than once per call.
func TestFlowCapabilityProbeIsCachedOnce(t *testing.T) {
	ext := &fakeExtension{flowOps: true}
	client := ext.serve(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := client.FlowFingerprint(ctx, "FALLBACK"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	if got := ext.count("ping"); got != 1 {
		t.Errorf("ping was called %d times, want 1 — the answer should be cached", got)
	}
}

// TestFlowUpscaleCarriesTheMediaID: the source path the app expects names the
// media's editor route, so the media id has to reach the extension. It did not
// while the request struct carried only the content id, which meant the named
// path built a different request from the fallback expression.
func TestFlowUpscaleCarriesTheMediaID(t *testing.T) {
	ext := &fakeExtension{flowOps: true}
	client := ext.serve(t)

	out, err := client.FlowUpscale(context.Background(), FlowUpscaleRequest{
		ProjectID:  "project-1",
		MediaID:    "media-1",
		ContentID:  "content-1",
		Resolution: 2,
		BuildLabel: "build-label",
	}, "")
	if err != nil {
		t.Fatalf("FlowUpscale: %v", err)
	}
	if len(out.Data) != 600 {
		t.Errorf("upscale returned %d bytes, want 600", len(out.Data))
	}

	var sent struct {
		ProjectID  string `json:"projectId"`
		MediaID    string `json:"mediaId"`
		ContentID  string `json:"contentId"`
		Resolution int    `json:"resolution"`
		BuildLabel string `json:"buildLabel"`
	}
	if err := json.Unmarshal(ext.paramsFor("flow.upscale"), &sent); err != nil {
		t.Fatalf("decode the upscale params: %v", err)
	}
	if sent.MediaID != "media-1" {
		t.Errorf("mediaId = %q, want %q", sent.MediaID, "media-1")
	}
	if sent.ProjectID != "project-1" || sent.ContentID != "content-1" {
		t.Errorf("params = %+v", sent)
	}
	if sent.Resolution != 2 || sent.BuildLabel != "build-label" {
		t.Errorf("params = %+v", sent)
	}
}

// TestFlowNavigateCarriesTheURL covers the one operation whose parameter is the
// whole point of the call.
func TestFlowNavigateCarriesTheURL(t *testing.T) {
	ext := &fakeExtension{flowOps: true}
	client := ext.serve(t)

	const target = "https://flow.google.com/project/abc123"
	if err := client.FlowNavigate(context.Background(), target, ""); err != nil {
		t.Fatalf("FlowNavigate: %v", err)
	}

	var sent struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(ext.paramsFor("flow.navigate"), &sent); err != nil {
		t.Fatalf("decode the navigate params: %v", err)
	}
	if sent.URL != target {
		t.Errorf("url = %q, want %q", sent.URL, target)
	}
}

// TestSurfaceReportsTheFlowExtension covers the report that says which surface
// is in use. Both extensions work, so nothing looks broken when the wrong one is
// loaded — which is exactly why it has to be visible rather than inferred.
func TestSurfaceReportsTheFlowExtension(t *testing.T) {
	ext := &fakeExtension{flowOps: true}
	client := ext.serve(t)

	// Nothing has been asked yet, and "not asked" must not read as "no".
	if got := client.Surface(); got.Known {
		t.Error("the surface cannot be known before the extension is asked")
	}

	got := client.ProbeSurface()
	if !got.Known {
		t.Fatal("ProbeSurface should leave the answer known")
	}
	if !got.FlowOperations {
		t.Error("this extension advertises the Flow operations")
	}
	if len(got.Advertised) == 0 {
		t.Error("the advertised operation list should be kept")
	}

	// The cached answer has to agree with the probed one.
	if cached := client.Surface(); !cached.Known || !cached.FlowOperations {
		t.Errorf("Surface = %+v, want the cached Flow answer", cached)
	}
}

// TestSurfaceReportsTheGenericExtensionHonestly: the generic bridge answers ping
// without an operation list, and that must be reported as "no Flow operations"
// rather than as unknown.
func TestSurfaceReportsTheGenericExtensionHonestly(t *testing.T) {
	ext := &fakeExtension{flowOps: false}
	client := ext.serve(t)

	got := client.ProbeSurface()
	if !got.Known {
		t.Fatal("the answer should be known after probing")
	}
	if got.FlowOperations {
		t.Error("the generic extension does not implement the Flow operations")
	}
	if len(got.Advertised) != 0 {
		t.Errorf("advertised = %v, want none", got.Advertised)
	}
}
