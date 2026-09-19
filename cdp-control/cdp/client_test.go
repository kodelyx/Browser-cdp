package cdp

import (
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAuthorizeRejectsTokenless is the regression test for the hole the origin
// check left open: a non-browser local process omits the Origin header, so it
// passed the origin check and could drive the user's tabs. The token is what
// actually closes that.
func TestAuthorizeRejectsTokenless(t *testing.T) {
	opts := ListenOptions{Token: "s3cret-bridge-token"}

	req := httptest.NewRequest(stdhttp.MethodGet, "/", nil)

	authenticated, ok := opts.authorize(req)
	if ok {
		t.Fatal("a tokenless upgrade must be rejected when a token is configured")
	}
	if authenticated {
		t.Error("a rejected upgrade cannot be authenticated")
	}
}

func TestAuthorizeRejectsWrongToken(t *testing.T) {
	opts := ListenOptions{Token: "s3cret-bridge-token"}

	for _, supplied := range []string{"wrong", "s3cret-bridge-toke", "s3cret-bridge-tokenx", ""} {
		req := httptest.NewRequest(stdhttp.MethodGet, "/?token="+supplied, nil)
		if _, ok := opts.authorize(req); ok {
			t.Errorf("token %q must be rejected", supplied)
		}
	}
}

func TestAuthorizeAcceptsQueryToken(t *testing.T) {
	opts := ListenOptions{Token: "s3cret-bridge-token"}
	req := httptest.NewRequest(stdhttp.MethodGet, "/?token=s3cret-bridge-token", nil)

	authenticated, ok := opts.authorize(req)
	if !ok {
		t.Fatal("the correct query token must be accepted")
	}
	if !authenticated {
		t.Error("a proven token must be reported as authenticated")
	}
}

func TestAuthorizeAcceptsBearerHeader(t *testing.T) {
	opts := ListenOptions{Token: "s3cret-bridge-token"}
	req := httptest.NewRequest(stdhttp.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer s3cret-bridge-token")

	authenticated, ok := opts.authorize(req)
	if !ok || !authenticated {
		t.Error("a correct Authorization header must be accepted and authenticated")
	}
}

// TestAuthorizeTokenlessWindowCloses covers first pairing: the window is open
// only until the extension proves it holds the token, and the decision is read
// live so it can close while the server is running.
func TestAuthorizeTokenlessWindowCloses(t *testing.T) {
	open := true
	opts := ListenOptions{
		Token:          "s3cret-bridge-token",
		AllowTokenless: func() bool { return open },
	}

	req := httptest.NewRequest(stdhttp.MethodGet, "/", nil)

	authenticated, ok := opts.authorize(req)
	if !ok {
		t.Fatal("the pairing window should accept a tokenless connection")
	}
	if authenticated {
		t.Error("a waived connection must not be reported as authenticated")
	}

	// Pairing completes: the window must close without a restart.
	open = false
	if _, ok := opts.authorize(req); ok {
		t.Error("once paired, a tokenless connection must be rejected")
	}
}

func TestAuthorizeWithoutConfiguredToken(t *testing.T) {
	// No token configured means no authentication is required. This is the
	// library default so that cdp remains usable standalone.
	opts := ListenOptions{}
	req := httptest.NewRequest(stdhttp.MethodGet, "/", nil)

	authenticated, ok := opts.authorize(req)
	if !ok {
		t.Error("with no token configured the upgrade should be allowed")
	}
	if authenticated {
		t.Error("nothing was proven, so it must not be marked authenticated")
	}
}

func TestIsExtensionOrigin(t *testing.T) {
	cases := []struct {
		origin string
		want   bool
	}{
		{"", true}, // non-browser client; the token check is what guards this
		{"chrome-extension://abcdefghijklmnop", true},
		{"http://127.0.0.1:8200", true},
		{"http://localhost:3000", true},
		{"https://evil.example.com", false},
		{"https://labs.google", false},
		{"not a url", false},
	}

	for _, tc := range cases {
		req := httptest.NewRequest(stdhttp.MethodGet, "/", nil)
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		if got := isExtensionOrigin(req); got != tc.want {
			t.Errorf("isExtensionOrigin(origin=%q) = %v, want %v", tc.origin, got, tc.want)
		}
	}
}

// TestRejectReasonTellsTheTwoRefusalsApart covers the distinction the log was
// missing. Both cases used to print "rejected unauthenticated upgrade", so an
// extension holding a stale token and one holding no token at all looked
// identical — and they need different fixes.
func TestRejectReasonTellsTheTwoRefusalsApart(t *testing.T) {
	opts := ListenOptions{Token: "s3cret-bridge-token"}

	missing := httptest.NewRequest(stdhttp.MethodGet, "/", nil)
	if got := opts.rejectReason(missing); got != RejectNoToken {
		t.Errorf("nothing presented: reason = %v, want RejectNoToken", got)
	}

	stale := httptest.NewRequest(stdhttp.MethodGet, "/?token=outdated", nil)
	if got := opts.rejectReason(stale); got != RejectWrongToken {
		t.Errorf("a wrong token presented: reason = %v, want RejectWrongToken", got)
	}
}

// TestRejectReasonExplainsTheNextStep: the log is the only place a user ever sees
// a refusal, so it has to carry the action and not just the fact. A rejected
// extension retries on a timer, which means this is the message that repeats.
func TestRejectReasonExplainsTheNextStep(t *testing.T) {
	hint := "Delete /tmp/bridge-token.claimed and restart."

	if got := RejectNoToken.explain(hint); !strings.Contains(got, hint) {
		t.Errorf("the repair hint should be carried through, got %q", got)
	}

	// Without a hint it still has to name the action.
	if got := RejectNoToken.explain(""); !strings.Contains(got, "pairing marker") {
		t.Errorf("a refusal with no hint should still name the action, got %q", got)
	}

	if got := RejectWrongToken.explain(hint); !strings.Contains(got, "stale") {
		t.Errorf("a wrong token should be described as stale, got %q", got)
	}
}

// TestPresentedTokenReadsBothCarriers pins the extraction that authorize and
// rejectReason share, so the two cannot disagree about what was supplied.
func TestPresentedTokenReadsBothCarriers(t *testing.T) {
	query := httptest.NewRequest(stdhttp.MethodGet, "/?token=from-query", nil)
	if got := presentedToken(query); got != "from-query" {
		t.Errorf("query token = %q, want %q", got, "from-query")
	}

	header := httptest.NewRequest(stdhttp.MethodGet, "/", nil)
	header.Header.Set("Authorization", "Bearer from-header")
	if got := presentedToken(header); got != "from-header" {
		t.Errorf("header token = %q, want %q", got, "from-header")
	}

	none := httptest.NewRequest(stdhttp.MethodGet, "/", nil)
	if got := presentedToken(none); got != "" {
		t.Errorf("no token = %q, want empty", got)
	}
}

func TestCookiesToHeader(t *testing.T) {
	cookies := []Cookie{
		{Name: "a", Value: "1"},
		{Name: "b", Value: "2"},
		{Name: "", Value: "ignored"},
	}
	if got := CookiesToHeader(cookies); got != "a=1; b=2" {
		t.Errorf("CookiesToHeader = %q, want %q", got, "a=1; b=2")
	}

	// An expired cookie must be dropped.
	expired := []Cookie{
		{Name: "old", Value: "x", ExpirationDate: 1}, // 1970
		{Name: "new", Value: "y"},
	}
	if got := CookiesToHeader(expired); got != "new=y" {
		t.Errorf("CookiesToHeader = %q, want %q", got, "new=y")
	}
}
