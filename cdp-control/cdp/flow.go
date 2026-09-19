package cdp

// The Flow bridge's surface.
//
// The Flow extension exposes these as named operations, so the backend never has
// to ship a generic evaluate() to a user's browser. Each one falls back to the
// expression the backend used before that extension existed, which is what lets
// one backend talk to either bridge.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

/* ------------------------------------------------------------------ *
 * Flow operations
 *
 * The Flow bridge exposes these directly, so the backend does not have to ship a
 * generic evaluate() to a user's browser. The generic bridge does not have them,
 * so each one falls back to the equivalent expression — which is what the
 * backend used before the Flow bridge existed. Trying the narrow call first
 * means either extension works, and a deployment can move across at its own pace.
 * ------------------------------------------------------------------ */

// hasFlowOps reports whether the attached extension implements the flow.*
// operations, probing once and caching the answer.
func (c *Client) hasFlowOps() bool {
	switch c.flowOps.Load() {
	case 1:
		return true
	case 0:
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out struct {
		Ops []string `json:"ops"`
	}
	if err := c.Call(ctx, "ping", nil, &out); err != nil {
		c.flowOps.Store(0)
		return false
	}

	c.opsMu.Lock()
	c.advertised = append([]string(nil), out.Ops...)
	c.opsMu.Unlock()

	for _, op := range out.Ops {
		if op == "flow.captcha" {
			c.flowOps.Store(1)
			return true
		}
	}
	c.flowOps.Store(0)
	return false
}

// Surface describes which operations the attached extension offers.
//
// The two extensions are not interchangeable, and which one is attached decides
// whether the backend's own narrow operations are used or the generic evaluate
// fallback. That is the first question to answer when a deployment behaves
// differently from the one before it, so it is reported rather than inferred
// from how some call happened to behave.
type Surface struct {
	// FlowOperations is true when the extension implements the flow.* calls.
	FlowOperations bool `json:"flow_operations"`
	// Advertised is the operation list the extension returned from ping. It is
	// empty for the generic bridge, which answers ping without one.
	Advertised []string `json:"advertised,omitempty"`
	// Known is false until the extension has been asked, so that a caller can
	// tell "no Flow operations" apart from "not asked yet".
	Known bool `json:"known"`
}

// Surface reports the attached extension's operations without probing, so a
// caller like a health check does not pay a round trip. Use ProbeSurface to ask
// for the answer now.
func (c *Client) Surface() Surface {
	switch c.flowOps.Load() {
	case 0, 1:
	default:
		return Surface{}
	}

	c.opsMu.Lock()
	ops := append([]string(nil), c.advertised...)
	c.opsMu.Unlock()

	return Surface{
		FlowOperations: c.flowOps.Load() == 1,
		Advertised:     ops,
		Known:          true,
	}
}

// ProbeSurface asks the extension what it offers and caches the answer.
func (c *Client) ProbeSurface() Surface {
	c.hasFlowOps()
	return c.Surface()
}

// flowCall runs a Flow operation, or evaluates the fallback expression when the
// extension does not implement it. An empty fallback makes the operation
// unavailable rather than silently doing nothing.
func (c *Client) flowCall(ctx context.Context, op string, params any, fallback string) (json.RawMessage, error) {
	if c.hasFlowOps() {
		var raw json.RawMessage
		if err := c.Call(ctx, op, params, &raw); err != nil {
			return nil, err
		}
		return raw, nil
	}
	if fallback == "" {
		return nil, fmt.Errorf("cdp: the attached extension does not implement %s", op)
	}
	return c.Evaluate(ctx, fallback)
}

// FlowFingerprint returns the page's own request identity.
func (c *Client) FlowFingerprint(ctx context.Context, fallback string) (json.RawMessage, error) {
	return c.flowCall(ctx, "flow.fingerprint", nil, fallback)
}

// FlowNavigate moves the attached tab to a URL.
func (c *Client) FlowNavigate(ctx context.Context, rawURL, fallback string) error {
	_, err := c.flowCall(ctx, "flow.navigate", map[string]any{"url": rawURL}, fallback)
	return err
}

// FlowProjects returns the project links rendered on the current page.
func (c *Client) FlowProjects(ctx context.Context, fallback string) (json.RawMessage, error) {
	return c.flowCall(ctx, "flow.projects", nil, fallback)
}

// FlowAt returns the page's anti-CSRF token, the `at` parameter of a
// batchexecute call. Only the page has it; the generic bridge cannot supply it.
func (c *Client) FlowAt(ctx context.Context, fallback string) (json.RawMessage, error) {
	return c.flowCall(ctx, "flow.at", nil, fallback)
}

// FlowCaptchaResult is either a probe answer or a minted token.
type FlowCaptchaResult struct {
	// Available reports whether the page has the client loaded at all. It is the
	// only field set when the call was a probe.
	Available bool `json:"available"`
	// Token is set when an action was asked for and the mint succeeded.
	Token string `json:"token,omitempty"`
	// Error is set when the page reported a problem; the call itself succeeded.
	Error string `json:"error,omitempty"`
}

// FlowCaptcha probes for the reCAPTCHA client, or mints a token for an action
// when one is given.
func (c *Client) FlowCaptcha(ctx context.Context, action, fallback string) (*FlowCaptchaResult, error) {
	params := map[string]any{}
	if action != "" {
		params["action"] = action
	}

	raw, err := c.flowCall(ctx, "flow.captcha", params, fallback)
	if err != nil {
		return nil, err
	}
	return decodeFlowCaptcha(raw)
}

// decodeFlowCaptcha accepts either answer shape.
//
// The two paths differ: the Flow extension returns an object, while the fallback
// expressions the backend used before it existed return a bare boolean for a
// probe and a bare token string for a mint. Decoding only the object made every
// probe look like a failure — which reads as "the page has no reCAPTCHA client"
// and silently drops the broker to the low-score HTTP provider, so the only
// symptom is a worse captcha.
func decodeFlowCaptcha(raw json.RawMessage) (*FlowCaptchaResult, error) {
	// An object is only an answer if it carries something. `{}` and `null` decode
	// happily into the zero value, which reads as "the page has no client" — the
	// same silent wrong answer this function exists to prevent, so they are
	// rejected rather than believed.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err == nil {
		if len(fields) == 0 {
			return nil, fmt.Errorf("cdp: flow.captcha returned an empty answer: %s", truncateRaw(raw))
		}
		var out FlowCaptchaResult
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("cdp: decode flow.captcha: %w", err)
		}
		return &out, nil
	}

	var available bool
	if err := json.Unmarshal(raw, &available); err == nil {
		return &FlowCaptchaResult{Available: available}, nil
	}

	var token string
	if err := json.Unmarshal(raw, &token); err == nil && token != "" {
		return &FlowCaptchaResult{Available: true, Token: token}, nil
	}

	return nil, fmt.Errorf("cdp: flow.captcha answered in an unrecognised shape: %s", truncateRaw(raw))
}

// truncateRaw keeps an unexpected payload short enough to log.
func truncateRaw(raw json.RawMessage) string {
	const max = 120
	if len(raw) <= max {
		return string(raw)
	}
	return string(raw[:max]) + "..."
}

// FlowUpscaleRequest describes an in-page image upscale.
type FlowUpscaleRequest struct {
	ProjectID string
	// MediaID names the asset in the editor route the RPC is scoped to. The
	// source path carries it, so an upscale cannot be built from the content id
	// alone.
	MediaID    string
	ContentID  string
	Resolution int
	BuildLabel string
	SiteKey    string
}

// FlowUpscaleResult carries the upscaled image, base64 as the page returned it.
type FlowUpscaleResult struct {
	Data  string `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

// FlowUpscale runs the app's own SPrCad call inside the page.
func (c *Client) FlowUpscale(ctx context.Context, req FlowUpscaleRequest, fallback string) (*FlowUpscaleResult, error) {
	params := map[string]any{
		"projectId":  req.ProjectID,
		"mediaId":    req.MediaID,
		"contentId":  req.ContentID,
		"resolution": req.Resolution,
		"buildLabel": req.BuildLabel,
	}
	if req.SiteKey != "" {
		params["siteKey"] = req.SiteKey
	}

	raw, err := c.flowCall(ctx, "flow.upscale", params, fallback)
	if err != nil {
		return nil, err
	}
	var out FlowUpscaleResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("cdp: decode flow.upscale: %w", err)
	}
	return &out, nil
}
