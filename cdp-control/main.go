// Command cdp-control drives a browser through the generic CDP extension, on its
// own ports.
//
// It exists so debugging does not have to go through the Flow engine. Finding out
// what the app actually sends means driving its UI and reading its network
// traffic, and doing that from inside the product means every experiment runs
// against the product's own bridge, its own scope and its own state. This is the
// same extension on a separate port with nothing else attached.
//
//	ws   127.0.0.1:9223   the extension dials in here
//	http 127.0.0.1:8201   this is what you talk to
//
// The Flow engine uses 9222 and 8200, so the two can run at once.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kodelyx/cdp-control/bridge"
	"github.com/kodelyx/cdp-control/cdp"
)

func main() {
	// 9222 is the extension's own default, so this works with nothing configured
	// in the browser. Pass a different -ws when the Flow engine is also running,
	// since it holds that port.
	wsAddr := flag.String("ws", "127.0.0.1:9222", "address the extension dials")
	httpAddr := flag.String("http", "127.0.0.1:8201", "address this API listens on")
	dataDir := flag.String("data", "", "where to keep the pairing token (default: a temp dir)")
	targets := flag.String("targets", defaultTargets, "comma-separated URLs the extension may attach to")
	domains := flag.String("domains", defaultDomains, "comma-separated cookie scopes")
	flag.Parse()

	dir := *dataDir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "cdp-control")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatalf("cdp-control: could not create %s: %v", dir, err)
	}

	br := bridge.NewBridge(splitList(*targets), splitList(*domains), dir)
	br.ListenAddr = *wsAddr

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := br.Listen(ctx); err != nil && ctx.Err() == nil {
			log.Fatalf("cdp-control: bridge stopped: %v", err)
		}
	}()

	srv := &server{bridge: br}
	mux := routes(srv)

	httpSrv := &http.Server{Addr: *httpAddr, Handler: mux}
	go func() {
		log.Printf("cdp-control: API on http://%s", *httpAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("cdp-control: http stopped: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("cdp-control: shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

// routes wires the API. It is separate from main so a test can exercise the
// handlers without starting a bridge or binding a port.
func routes(srv *server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.health)
	mux.HandleFunc("/status", srv.status)
	mux.HandleFunc("/tabs", srv.tabs)
	mux.HandleFunc("/eval", srv.eval)
	mux.HandleFunc("/cdp", srv.cdp)
	mux.HandleFunc("/click", srv.click)
	mux.HandleFunc("/events", srv.events)
	mux.HandleFunc("/requests", srv.requests)
	mux.HandleFunc("/cookies", srv.cookies)
	return mux
}

// The defaults are the hosts a Flow debugging session needs, so the tool is
// useful with no flags at all.
const (
	defaultTargets = "https://labs.google/fx/tools/flow,https://flow.google.com"
	defaultDomains = "labs.google,google.com,accounts.google.com"
)

func splitList(raw string) []string {
	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

type server struct {
	bridge *bridge.Bridge
}

// client resolves the attached extension, or reports why there is none.
func (s *server) client() (*cdp.Client, error) {
	client := s.bridge.Current()
	if client == nil || !client.Connected() {
		return nil, fmt.Errorf("no extension attached — load ../extension in Chrome " +
			"and point it at this bridge's address")
	}
	return client, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	client := s.bridge.Current()
	attached := client != nil && client.Connected()
	body := map[string]any{"attached": attached}
	if attached {
		body["addr"] = client.RemoteAddr
		surface := client.ProbeSurface()
		body["flow_operations"] = surface.FlowOperations
		body["ops"] = surface.Advertised
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *server) tabs(w http.ResponseWriter, r *http.Request) {
	client, err := s.client()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	tabs, err := client.ListTabs(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tabs": tabs})
}

// eval runs an expression in the attached tab and returns whatever it yields.
//
// This is the endpoint the tool exists for: it is how the app's own behaviour gets
// observed — click a control, read a value, walk the DOM — without adding anything
// to the product to support the question.
func (s *server) eval(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Expression string `json:"expression"`
		TabID      int    `json:"tab_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Expression == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("expression is required"))
		return
	}
	client, err := s.client()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if _, err := client.Attach(ctx, req.TabID); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	raw, err := client.Evaluate(ctx, req.Expression)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": json.RawMessage(raw)})
}

// cdp issues any CDP method, for the cases eval cannot reach.
func (s *server) cdp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
		TabID  int            `json:"tab_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Method == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("method is required"))
		return
	}
	client, err := s.client()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if _, err := client.Attach(ctx, req.TabID); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	var raw json.RawMessage
	if err := client.CallCDP(ctx, req.Method, req.Params, &raw); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": raw})
}

// click finds an element by its visible text and clicks it.
//
// Driving a UI by hand means locating a control, and the text on it is the one
// thing that survives a redesign. It walks up from the text to the nearest
// clickable ancestor, which is what a person does without thinking about it.
func (s *server) click(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text  string `json:"text"`
		TabID int    `json:"tab_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Text == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("text is required"))
		return
	}
	client, err := s.client()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if _, err := client.Attach(ctx, req.TabID); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	expr := fmt.Sprintf(`(() => {
  const want = %s;
  const all = [...document.querySelectorAll('button,a,[role=button],[role=menuitem],span,div')];
  const hit = all.find(e => (e.innerText || '').trim() === want);
  if (!hit) return {clicked: false, reason: 'no element with that exact text'};
  const target = hit.closest('button,a,[role=button],[role=menuitem]') || hit;
  target.click();
  return {clicked: true, tag: target.tagName, label: (target.innerText || '').trim().slice(0, 60)};
})()`, mustJSON(req.Text))

	raw, err := client.Evaluate(ctx, expr)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": json.RawMessage(raw)})
}

// events reads the extension's buffered CDP events, optionally filtered.
//
// Network events are the point: `Network.requestWillBeSent` carries the URL and
// the POST body, which is how a request shape gets read off the app rather than
// guessed at.
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	client, err := s.client()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	events, err := client.ReadEvents(ctx, 5000)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	filter := r.URL.Query().Get("filter")
	type_ := r.URL.Query().Get("type")
	out := []map[string]any{}
	for _, ev := range events {
		if type_ != "" && ev.Method != type_ {
			continue
		}
		if filter != "" && !strings.Contains(string(ev.Params), filter) {
			continue
		}
		out = append(out, map[string]any{"type": ev.Method, "params": json.RawMessage(ev.Params)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "events": out})
}

// requests returns the network requests the page has made, parsed.
//
// This is the endpoint the tool is really for. Reading a request shape off the app
// is the only way to know what it sends, and the raw event stream buries the two
// fields that matter — the URL and the POST body — inside
// `Network.requestWillBeSent`. Every question of the form "what does the app send
// when you click this" is answered here, and none of them are answered by guessing.
//
// `filter` matches anywhere in the URL. `method` narrows to one verb.
func (s *server) requests(w http.ResponseWriter, r *http.Request) {
	client, err := s.client()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	events, err := client.ReadEvents(ctx, 5000)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	filter := r.URL.Query().Get("filter")
	method := strings.ToUpper(r.URL.Query().Get("method"))

	type request struct {
		Method  string `json:"method"`
		URL     string `json:"url"`
		Body    string `json:"body,omitempty"`
		HasBody bool   `json:"has_body"`
	}

	out := []request{}
	for _, ev := range events {
		if ev.Method != "Network.requestWillBeSent" {
			continue
		}
		var payload struct {
			Request struct {
				URL      string `json:"url"`
				Method   string `json:"method"`
				PostData string `json:"postData"`
			} `json:"request"`
		}
		if err := json.Unmarshal(ev.Params, &payload); err != nil {
			continue
		}
		if payload.Request.URL == "" {
			continue
		}
		if filter != "" && !strings.Contains(payload.Request.URL, filter) {
			continue
		}
		if method != "" && payload.Request.Method != method {
			continue
		}
		out = append(out, request{
			Method:  payload.Request.Method,
			URL:     payload.Request.URL,
			Body:    payload.Request.PostData,
			HasBody: payload.Request.PostData != "",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "requests": out})
}

// cookies returns the cookies the extension can see for a scope.
//
// Names and hosts only by default. A cookie value is a credential, and a debug tool
// that prints them by default turns every shared log into a leak. `values=1` asks
// for them explicitly, which is a decision somebody makes rather than one they
// inherit.
func (s *server) cookies(w http.ResponseWriter, r *http.Request) {
	// Validate before reaching for the browser: a missing argument is the
	// caller's mistake, and answering it with "no extension attached" sends them
	// to fix the wrong thing.
	domain := r.URL.Query().Get("domain")
	if domain == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("domain is required"))
		return
	}
	wantValues := r.URL.Query().Get("values") == "1"

	client, err := s.client()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	found, err := client.ListCookies(ctx, map[string]any{"domain": domain})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	type cookie struct {
		Domain string `json:"domain"`
		Name   string `json:"name"`
		Value  string `json:"value,omitempty"`
	}
	out := make([]cookie, 0, len(found))
	for _, c := range found {
		entry := cookie{Domain: c.Domain, Name: c.Name}
		if wantValues {
			entry.Value = c.Value
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "cookies": out})
}

// status is the one call to make when something is not working.
//
// It answers, in one response, every question that otherwise takes three requests
// to settle: is anything attached, which extension is it, what can it do, what is
// it allowed to reach, and what is it currently sitting on.
func (s *server) status(w http.ResponseWriter, r *http.Request) {
	client := s.bridge.Current()
	if client == nil || !client.Connected() {
		writeJSON(w, http.StatusOK, map[string]any{
			"attached": false,
			"hint": "load ../extension in Chrome, then set its Backend bridge field to " +
				"this tool's address",
		})
		return
	}

	surface := client.ProbeSurface()
	body := map[string]any{
		"attached":        true,
		"addr":            client.RemoteAddr,
		"flow_operations": surface.FlowOperations,
		"ops":             surface.Advertised,
		"targets":         s.bridge.Targets,
		"cookie_domains":  s.bridge.CookieDomains,
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if tab, err := client.CurrentTab(ctx); err == nil && tab != nil {
		body["tab"] = tab
	}
	writeJSON(w, http.StatusOK, body)
}

func mustJSON(v any) string {
	encoded, err := json.Marshal(v)
	if err != nil {
		return `""`
	}
	return string(encoded)
}
