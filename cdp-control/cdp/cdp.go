package cdp

// The generic bridge's surface: arbitrary DevTools access to the attached tab.
//
// These three are what a generic bridge offers and a Flow bridge deliberately
// does not. They live in their own file so the escape hatch is one place to
// read, audit or remove, rather than interleaved with the Flow operations that
// replaced it.

import (
	"context"
	"encoding/json"
)

// CallCDP issues an arbitrary CDP command against the attached tab.
func (c *Client) CallCDP(ctx context.Context, method string, params any, out any) error {
	body := map[string]any{"method": method}
	if params != nil {
		body["params"] = params
	}
	return c.Call(ctx, "cdp.call", body, out)
}

// Evaluate runs an expression in the attached tab and returns its value.
//
// awaitPromise is on. Without it, an expression that returns a Promise resolves
// to an empty object rather than the value, because CDP serialises the Promise
// itself instead of waiting for it — which silently breaks anything written as
// an async IIFE, including the reCAPTCHA broker.
func (c *Client) Evaluate(ctx context.Context, expression string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := c.Call(ctx, "cdp.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	}, &raw)
	return raw, err
}

// EvaluateString runs an expression and decodes a string result.
func (c *Client) EvaluateString(ctx context.Context, expression string) (string, error) {
	raw, err := c.Evaluate(ctx, expression)
	if err != nil {
		return "", err
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out, nil
}
