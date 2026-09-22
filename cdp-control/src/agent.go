package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kodelyx/Browser-cdp/cdp-control/src/cdp"
	"github.com/kodelyx/Browser-cdp/cdp-control/src/needle"
)

// The agent endpoints exist so a calling AI can use this tool as its eyes and
// hands: drive the live tab, then read back exactly what the page sent. The
// interesting part is not the HTTP; it is that the answer is measured off the
// browser rather than inferred, so the caller can write code against it on the
// first attempt.

// agentTools is the action surface the model chooses from.
//
// Three, not thirty. Needle 3 is a 121M model: it routes well over a short list
// of plainly named tools and gets worse as the list grows. Nothing is lost by
// keeping it small — navigation, form filling and DOM reads are all one
// `eval_js` expression away, so `eval_js` is the escape hatch that makes a narrow
// surface free.
//
// The descriptions carry more weight than they would for a large model, and they
// are written defensively because of it. `read_cookies` in particular is the one
// a bare domain name in the instruction pulls the model towards, so it says out
// loud when *not* to use it — without that line, "navigate to example.com and
// read the title" answers with a cookie lookup.
const agentTools = `[
	{
		"name": "click_element",
		"description": "Click a button, link or menu item on the page, found by the visible text on it. Use this when the instruction says to click, press or tap something",
		"parameters": {
			"type": "object",
			"properties": {
				"text": {"type": "string", "description": "The visible text on the element to click"}
			},
			"required": ["text"]
		}
	},
	{
		"name": "eval_js",
		"description": "Run JavaScript in the page and return its value. Use this to navigate, to read the page title or the DOM, to inspect what the page requested, or to fill a field. Use this whenever no other tool fits the instruction",
		"parameters": {
			"type": "object",
			"properties": {
				"expression": {"type": "string", "description": "JavaScript expression to evaluate"}
			},
			"required": ["expression"]
		}
	},
	{
		"name": "read_cookies",
		"description": "Read the cookie names the browser holds for one domain. Use this only when the instruction explicitly asks about cookies or a session, not when it merely mentions a website",
		"parameters": {
			"type": "object",
			"properties": {
				"domain": {"type": "string", "description": "Cookie domain, for example example.com"}
			},
			"required": ["domain"]
		}
	}
]`

// Capture tuning.
//
// A click and the request it causes are not simultaneous — the handler runs, the
// app builds the call, the network stack sends it. Draining the buffer once
// immediately after the action returns an empty list for a page that is about to
// send exactly the request the caller asked about, which reads as "nothing
// happened" and is the worst possible answer. So the capture polls, and stops as
// soon as something matched plus a short grace window to collect sibling calls.
const (
	defaultSettleMS = 400
	maxSettleMS     = 5000
	captureGrace    = 120 * time.Millisecond
	capturePoll     = 25 * time.Millisecond
	maxAgentCalls   = 8
)

// browser is the slice of *cdp.Client the agent endpoints use.
//
// An interface rather than the concrete client so dispatch and capture can be
// tested without a browser attached — which is the only way to test the parts
// that matter, because a test has no extension to talk to.
type browser interface {
	Evaluate(ctx context.Context, expression string) (json.RawMessage, error)
	ListCookies(ctx context.Context, details map[string]any) ([]cdp.Cookie, error)
	ReadEvents(ctx context.Context, limit int) ([]cdp.BufferedEvent, error)
}

// engineStats is the runtime's own account of the call it just served.
//
// Reported rather than logged because the caller is an agent: if a response took
// a second, the agent should be able to see whether that was the model or the
// network, and decide whether to ask a narrower question next time.
type engineStats struct {
	PrefillTPS *float64 `json:"prefill_tps,omitempty"`
	DecodeTPS  *float64 `json:"decode_tps,omitempty"`
	PeakRAMMB  *float64 `json:"peak_ram_mb,omitempty"`
}

func statsFrom(resp *needle.NeedleResponse) *engineStats {
	if resp == nil {
		return nil
	}
	return &engineStats{
		PrefillTPS: resp.PrefillTPS,
		DecodeTPS:  resp.DecodeTPS,
		PeakRAMMB:  resp.PeakRAMMB,
	}
}

// clampSettleMS keeps a caller-supplied wait inside a range the server can honour.
func clampSettleMS(ms int) int {
	if ms <= 0 {
		return defaultSettleMS
	}
	if ms > maxSettleMS {
		return maxSettleMS
	}
	return ms
}

// executedAction is one action the model chose, and what came of it.
type executedAction struct {
	Tool   string          `json:"tool"`
	Args   map[string]any  `json:"arguments"`
	Target string          `json:"target,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// executeAgentCalls runs the model's chosen actions in order.
//
// In order matters: the model answers "click Save, then check the request" with
// two calls, and running them in the order it gave them is the difference between
// reproducing the sequence and reproducing a permutation of it. One action
// failing does not abandon the rest — the caller gets every result, each with its
// own error, which is more useful than the first failure alone.
func executeAgentCalls(ctx context.Context, client browser, calls []needle.FunctionCall) ([]executedAction, []string) {
	out := make([]executedAction, 0, len(calls))
	warnings := []string{}

	if len(calls) > maxAgentCalls {
		warnings = append(warnings, fmt.Sprintf(
			"the model returned %d actions; only the first %d were executed", len(calls), maxAgentCalls))
		calls = calls[:maxAgentCalls]
	}

	for _, call := range calls {
		entry := executedAction{Tool: call.Name, Args: call.Arguments}
		result, target, err := executeAgentAction(ctx, client, call.Name, call.Arguments)
		entry.Target = target
		entry.Result = result
		if err != nil {
			entry.Error = err.Error()
		}
		out = append(out, entry)
	}
	return out, warnings
}

// executeAgentAction runs one action against the attached tab.
//
// The second return value is the thing the action was aimed at — a button's
// label, an expression, a domain. It is echoed back because a caller reading
// `click_element` with the target missing cannot tell a mis-routed tool call from
// a correct one that hit nothing.
func executeAgentAction(ctx context.Context, client browser, tool string, args map[string]any) (json.RawMessage, string, error) {
	switch tool {
	case "click_element":
		text, _ := args["text"].(string)
		if text == "" {
			return nil, "", fmt.Errorf("click_element needs a text argument")
		}
		raw, err := client.Evaluate(ctx, clickExpression(text))
		return raw, text, err

	case "eval_js":
		expression, _ := args["expression"].(string)
		if expression == "" {
			return nil, "", fmt.Errorf("eval_js needs an expression argument")
		}
		raw, err := client.Evaluate(ctx, expression)
		return raw, expression, err

	case "read_cookies":
		domain, _ := args["domain"].(string)
		if domain == "" {
			return nil, "", fmt.Errorf("read_cookies needs a domain argument")
		}
		found, err := client.ListCookies(ctx, map[string]any{"domain": domain})
		if err != nil {
			return nil, domain, err
		}
		// Names and hosts only, matching /cookies. A cookie value is a
		// credential, and this endpoint is reached by an automated caller that
		// never asked for one. `values=1` is a decision a person makes; there is
		// nobody here to make it.
		names := make([]string, 0, len(found))
		for _, ck := range found {
			names = append(names, ck.Name)
		}
		encoded, err := json.Marshal(map[string]any{
			"domain": domain,
			"count":  len(names),
			"names":  names,
		})
		return encoded, domain, err
	}

	return nil, "", fmt.Errorf("unknown action %q", tool)
}

// captureAfterAction polls the extension's event buffer until it sees a matching
// request, then a little longer to collect the siblings, or until the settle
// window closes.
//
// `events.read` drains, so every event arrives exactly once and no de-duplication
// is needed. It also means the caller must drain once *before* acting — see the
// handler — or the capture would contain whatever the page was doing anyway.
func captureAfterAction(ctx context.Context, client browser, filter string, settle time.Duration) ([]networkRequest, error) {
	out := []networkRequest{}
	deadline := time.Now().Add(settle)
	var firstMatchAt time.Time

	for {
		events, err := client.ReadEvents(ctx, 5000)
		if err != nil {
			return out, err
		}
		matched := parseRequests(events, filter, "")
		if len(matched) > 0 && firstMatchAt.IsZero() {
			firstMatchAt = time.Now()
		}
		out = append(out, matched...)

		now := time.Now()
		if !firstMatchAt.IsZero() && now.Sub(firstMatchAt) >= captureGrace {
			break
		}
		if !now.Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(capturePoll):
		}
	}
	return out, nil
}

// extractToolName is the synthetic tool whose parameter block carries the
// caller's schema.
const extractToolName = "extract"

// extractToolsJSON wraps a caller-supplied JSON Schema as the parameters of one
// synthetic tool.
//
// The native runtime compiles its output grammar from the tools declared at
// needle_init; there is no separate extraction entry point in the C API. Declaring
// the schema as a tool's parameters is therefore what makes the decode
// grammar-constrained — the model can answer in the requested shape or not answer
// at all, but it cannot answer in a shape of its own.
//
// What that guarantees is worth stating precisely: the output *parses* and
// conforms to the schema. It does not make the values true — they are still the
// model's reading of the text you handed it.
func extractToolsJSON(schema json.RawMessage) (string, error) {
	var obj map[string]any
	if err := json.Unmarshal(schema, &obj); err != nil {
		return "", fmt.Errorf("schema must be a JSON object: %w", err)
	}
	if len(obj) == 0 {
		return "", fmt.Errorf("schema must not be empty")
	}
	if _, hasType := obj["type"]; !hasType {
		if _, hasProperties := obj["properties"]; !hasProperties {
			return "", fmt.Errorf(`schema needs a "type" or "properties" key`)
		}
	}

	encoded, err := json.Marshal([]map[string]any{{
		"name":        extractToolName,
		"description": "Record the fields the schema asks for, read out of the supplied text",
		"parameters":  obj,
	}})
	if err != nil {
		return "", fmt.Errorf("could not encode the schema as a tool: %w", err)
	}
	return string(encoded), nil
}

/* ------------------------------------------------------------------ *
 * Shared UI driving
 * ------------------------------------------------------------------ */

// clickExpression builds the script that finds an element by its visible text and
// clicks the nearest clickable ancestor.
//
// Shared with the /click endpoint on purpose. If the model's `click_element` and
// the human's `/click` resolved elements differently, an agent would reproduce a
// flow the person cannot, and the difference would only show up as a mystery.
func clickExpression(text string) string {
	return fmt.Sprintf(`(() => {
  const want = %s;
  const all = [...document.querySelectorAll('button,a,[role=button],[role=menuitem],span,div')];
  const hit = all.find(e => (e.innerText || '').trim() === want);
  if (!hit) return {clicked: false, reason: 'no element with that exact text'};
  const target = hit.closest('button,a,[role=button],[role=menuitem]') || hit;
  target.click();
  return {clicked: true, tag: target.tagName, label: (target.innerText || '').trim().slice(0, 60)};
})()`, mustJSON(text))
}

// mustJSON renders a Go value as a JavaScript literal, so a caller-supplied
// string can be embedded in an expression without escaping it by hand.
func mustJSON(v any) string {
	encoded, err := json.Marshal(v)
	if err != nil {
		return `""`
	}
	return string(encoded)
}
