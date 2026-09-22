package main

// The endpoints that talk to the attached tab: what is attached, what is on
// screen, and what the page sent over the wire.
//
// Every one of them is a thin translation between an HTTP request and one call
// on the cdp client. The work — and the comments worth reading — lives in the cdp
// package; this file is the surface a caller speaks to.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kodelyx/Browser-cdp/cdp-control/src/cdp"
)

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	client := s.bridge.Current()
	attached := client != nil && client.Connected()
	body := map[string]any{"attached": attached}
	if attached {
		body["addr"] = client.RemoteAddr
		body["ops"] = client.Ops()
	}
	writeJSON(w, http.StatusOK, body)
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
			"hint": "load ../extension in Chrome as an unpacked extension, and set " +
				"bridgeUrl in its config.js to this tool's ws address",
		})
		return
	}

	body := map[string]any{
		"attached":       true,
		"addr":           client.RemoteAddr,
		"ops":            client.Ops(),
		"targets":        s.bridge.Targets,
		"cookie_domains": s.bridge.CookieDomains,
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if tab, err := client.CurrentTab(ctx); err == nil && tab != nil {
		body["tab"] = tab
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
//
// The expression itself lives in agent.go, because the model's `click_element`
// uses it too and the two must resolve elements identically.
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

	raw, err := client.Evaluate(ctx, clickExpression(req.Text))
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

// networkRequest is one request the page made, with the two fields that matter.
type networkRequest struct {
	Method  string `json:"method"`
	URL     string `json:"url"`
	Body    string `json:"body,omitempty"`
	HasBody bool   `json:"has_body"`
}

// parseRequests pulls the requests out of a CDP event stream.
//
// Separate from the handler so it can be tested against a captured event, which is
// the only way to pin the shape: `Network.requestWillBeSent` nests the URL and the
// POST body under `request`, and a mistake there produces an empty list rather than
// an error — indistinguishable from a page that made no requests, which is exactly
// the confusion this endpoint exists to remove.
func parseRequests(events []cdp.BufferedEvent, filter, method string) []networkRequest {
	method = strings.ToUpper(method)
	out := []networkRequest{}

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
		if method != "" && strings.ToUpper(payload.Request.Method) != method {
			continue
		}
		out = append(out, networkRequest{
			Method:  payload.Request.Method,
			URL:     payload.Request.URL,
			Body:    payload.Request.PostData,
			HasBody: payload.Request.PostData != "",
		})
	}
	return out
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

	out := parseRequests(events, r.URL.Query().Get("filter"), r.URL.Query().Get("method"))
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
