package main

// The two endpoints an AI agent drives.
//
// They are separate from handlers_browser.go because they are a different kind of
// thing: those endpoints answer a question about the tab, and these two put a
// model in the loop — one acts on the tab and reports what that caused, the other
// reads typed fields out of text with no browser involved at all.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kodelyx/Browser-cdp/cdp-control/src/needle"
)

// agentAct is the endpoint an AI agent drives the browser with.
//
// It is one round trip on purpose. An agent forced to call "decide", then "act",
// then "read the network" learns the flow wrong — it forgets the third call, or
// makes it after the buffer has moved on. Here the action and the evidence for it
// come back together, which is the shape of the question the agent actually has:
// "I clicked Save; what did the app send?"
//
//	POST /agent/act {"instruction": "Click Save and show me the API call",
//	                 "url_filter": "api"}
//
// The model picks the action; the Go side executes it on the attached tab and
// returns the requests that action caused.
func (s *server) agentAct(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Instruction string `json:"instruction"`
		URLFilter   string `json:"url_filter"`
		TabID       int    `json:"tab_id"`
		SettleMS    int    `json:"settle_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Instruction) == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("instruction is required"))
		return
	}
	settle := time.Duration(clampSettleMS(req.SettleMS)) * time.Millisecond

	client, err := s.client()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	// Attach before the model is asked anything: "there is no tab to drive" is a
	// property of the browser, and paying for inference to discover it wastes the
	// caller's time and tells them less.
	tab, err := client.Attach(ctx, req.TabID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	total := time.Now()

	loadStart := time.Now()
	engine, err := needle.GetNeedleEngine()
	loadMS := time.Since(loadStart).Milliseconds()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("needle 3 engine unavailable: %w", err))
		return
	}

	modelStart := time.Now()
	resp, err := engine.CompleteTools(req.Instruction, agentTools)
	modelMS := time.Since(modelStart).Milliseconds()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	warnings := []string{}

	// Baseline the buffer, then act.
	//
	// Drained here rather than at the top of the handler: the model call takes
	// real time, and anything the page sent during it is not an answer to this
	// instruction. Reading the buffer once immediately before the action is what
	// makes `captured_requests` attributable to the action rather than to the
	// page's own background chatter.
	if _, err := client.ReadEvents(ctx, 5000); err != nil {
		warnings = append(warnings, fmt.Sprintf("could not baseline the event buffer: %v", err))
	}

	actionStart := time.Now()
	actions, callWarnings := executeAgentCalls(ctx, client, resp.FunctionCalls)
	actionMS := time.Since(actionStart).Milliseconds()
	warnings = append(warnings, callWarnings...)

	captureStart := time.Now()
	captured, err := captureAfterAction(ctx, client, req.URLFilter, settle)
	captureMS := time.Since(captureStart).Milliseconds()
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("network capture incomplete: %v", err))
	}

	body := map[string]any{
		"action_executed":   "none",
		"captured_requests": captured,
		"actions":           actions,
		"tab":               tab,
		"url_filter":        req.URLFilter,
		"reasoning":         resp.Reasoning,
		"confidence":        resp.Confidence,
		"engine":            statsFrom(resp),
		"timing_ms": map[string]int64{
			"engine_load": loadMS,
			"model":       modelMS,
			"action":      actionMS,
			"capture":     captureMS,
			"total":       time.Since(total).Milliseconds(),
		},
	}

	// The flat pair mirrors the shape a caller is most likely written against:
	// one action, one target. `actions` carries the rest, for the instructions
	// that legitimately turn into two calls.
	if len(actions) > 0 {
		body["action_executed"] = actions[0].Tool
		body["target"] = actions[0].Target
	} else {
		warnings = append(warnings, "the model chose no action for this instruction")
	}
	if len(warnings) > 0 {
		body["warnings"] = warnings
	}

	writeJSON(w, http.StatusOK, body)
}

// agentExtract turns raw text — a DOM dump, a captured payload — into typed JSON.
//
// No browser is involved, and that is deliberate. The common case is an agent
// that already holds the payload (from /agent/act, or from a log) and needs it as
// fields; requiring a live tab to parse text already in memory would make the
// endpoint useless exactly when it is most wanted.
//
//	POST /agent/extract {"text": "...", "schema": {"type": "object", ...}}
//
// The schema becomes the decode grammar, so the answer conforms to it or the
// model declines. `matched: false` is that decline, and it is reported as a 200:
// the model refusing to invent fields the text does not contain is the grammar
// working, not the caller making a mistake.
func (s *server) agentExtract(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text   string          `json:"text"`
		Schema json.RawMessage `json:"schema"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("text is required"))
		return
	}
	if len(req.Schema) == 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("schema is required"))
		return
	}
	toolsJSON, err := extractToolsJSON(req.Schema)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	total := time.Now()

	loadStart := time.Now()
	engine, err := needle.GetNeedleEngine()
	loadMS := time.Since(loadStart).Milliseconds()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("needle 3 engine unavailable: %w", err))
		return
	}

	modelStart := time.Now()
	resp, err := engine.CompleteTools(req.Text, toolsJSON)
	modelMS := time.Since(modelStart).Milliseconds()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	extracted := map[string]any{}
	matched := false
	if len(resp.FunctionCalls) > 0 && resp.FunctionCalls[0].Name == extractToolName {
		if args := resp.FunctionCalls[0].Arguments; args != nil {
			extracted = args
		}
		matched = true
	}

	body := map[string]any{
		"extracted":  extracted,
		"matched":    matched,
		"reasoning":  resp.Reasoning,
		"confidence": resp.Confidence,
		"engine":     statsFrom(resp),
		"timing_ms": map[string]int64{
			"engine_load": loadMS,
			"model":       modelMS,
			"total":       time.Since(total).Milliseconds(),
		},
	}
	if !matched {
		body["warning"] = "the model produced no call for this schema and text"
	}
	writeJSON(w, http.StatusOK, body)
}
