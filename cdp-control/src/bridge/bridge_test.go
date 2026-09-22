package bridge

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodelyx/Browser-cdp/cdp-control/src/cookiejar"
)

// newTestBridge returns a bridge with no extension attached and no persisted
// cookie file, so a refresh can only succeed by reusing what the test seeded.
func newTestBridge(t *testing.T) *Bridge {
	t.Helper()
	b := NewBridge(nil, nil, t.TempDir())
	b.cookieFile = filepath.Join(t.TempDir(), "absent.json")
	return b
}

func seedJar(b *Bridge, raw string) *cookiejar.Jar {
	jar := cookiejar.FromRaw(raw, ".google.com", "test")
	b.mu.Lock()
	b.lastJar = jar
	b.mu.Unlock()
	return jar
}

// TestRefreshSessionReusesARecentRefresh covers the coalescing rule.
//
// Every worker draws on the same account, so they share its cookies and go stale
// together: a fleet hitting a 401 at the same moment would otherwise each attach
// to the same tab and wait out the full pause. A refresh that finished inside the
// window is served from the jar instead.
//
// No extension is attached here, so anything that tried a real refresh would fail
// with "no extension connected" — returning the jar is the proof it was reused.
func TestRefreshSessionReusesARecentRefresh(t *testing.T) {
	b := newTestBridge(t)
	jar := seedJar(b, "__Secure-1PSID=abc; SID=def")
	b.lastRefresh = time.Now()

	got, err := b.RefreshSession(context.Background())
	if err != nil {
		t.Fatalf("a refresh inside the window should be reused, got: %v", err)
	}
	if got == nil || got.Hash() != jar.Hash() {
		t.Fatalf("reused the wrong jar: %v", got)
	}
}

// TestRefreshSessionOutsideTheWindowDoesNotReuse is the other half of the rule.
//
// Once the window has passed the jar must not be handed back, or a genuinely
// stale session would be served forever and the engine would never recover.
func TestRefreshSessionOutsideTheWindowDoesNotReuse(t *testing.T) {
	b := newTestBridge(t)
	seedJar(b, "__Secure-1PSID=abc")
	b.lastRefresh = time.Now().Add(-RefreshCoalesceWindow - time.Second)

	if _, err := b.RefreshSession(context.Background()); err == nil {
		t.Fatal("a refresh older than the window must not be reused")
	}
}

// TestRefreshSessionWithoutAJarFallsThrough pins that the timestamp alone is not
// enough. A window that is recent but has no jar to serve must still attempt a
// real refresh, otherwise a first-ever call would report success with nothing.
func TestRefreshSessionWithoutAJarFallsThrough(t *testing.T) {
	b := newTestBridge(t)
	b.lastRefresh = time.Now()

	if _, err := b.RefreshSession(context.Background()); err == nil {
		t.Fatal("a recent timestamp with no jar must fall through to a real refresh")
	}
}

// TestRefreshSessionDoesNotCoalesceFailures pins that only a *completed* refresh
// opens the window. If a failure reset it, the next caller would be told a good
// refresh had just happened while the session was still dead.
func TestRefreshSessionDoesNotCoalesceFailures(t *testing.T) {
	b := newTestBridge(t)
	seedJar(b, "__Secure-1PSID=abc")

	// No extension, so this fails and must leave the window closed.
	if _, err := b.RefreshSession(context.Background()); err == nil {
		t.Fatal("expected the refresh to fail with no extension attached")
	}

	if !b.lastRefresh.IsZero() {
		t.Fatalf("a failed refresh must not open the coalescing window, got %s", b.lastRefresh)
	}

	// And a second caller must therefore also try for real rather than reuse.
	if _, err := b.RefreshSession(context.Background()); err == nil {
		t.Fatal("a failed refresh must not be reused by the next caller")
	}
}

// TestRefreshCoalesceWindowIsShort keeps the window honest. It exists to absorb a
// burst of 401s, which arrives in well under a second; a long window would serve
// an ageing jar to callers that genuinely needed a new one.
func TestRefreshCoalesceWindowIsShort(t *testing.T) {
	if RefreshCoalesceWindow <= 0 {
		t.Fatal("the window must be positive")
	}
	if RefreshCoalesceWindow > 10*time.Second {
		t.Errorf("a %s window is long enough to serve a stale jar", RefreshCoalesceWindow)
	}
}

// TestUniversalBridgeDeclaresAnEmptyScope is the regression test for universal
// mode.
//
// A nil slice marshals as `null`, not `[]`. That only reaches the extension's
// "empty means full access" behaviour by way of a `|| []` coercion on the far
// side, which is a thin thread to hang the tool's default posture on. The patch
// has to say what it means.
func TestUniversalBridgeDeclaresAnEmptyScope(t *testing.T) {
	// nil targets and nil domains: the zero value, which is what an unscoped
	// bridge is built from.
	br := NewBridge(nil, nil, t.TempDir())

	encoded, err := json.Marshal(br.configPatch())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var patch map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &patch); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, key := range []string{"targetUrlPrefixes", "cookieDomains"} {
		raw, present := patch[key]
		if !present {
			t.Errorf("%s was omitted, so an extension would keep whatever scope it already had", key)
			continue
		}
		if string(raw) != "[]" {
			t.Errorf("%s = %s, want an explicit empty array so the extension reads it as full access", key, raw)
		}
	}
}

// TestUnscopedBridgeRefusesToMirrorCookies pins the other half of universal mode.
//
// The cookie mirror exists so a backend can keep working after the browser
// closes, which is only meaningful when the caller named a site. Running it
// unscoped means asking the browser for every cookie it holds, and reporting the
// empty result as "is the account signed in?" — a misleading answer to a question
// nobody asked.
func TestUnscopedBridgeRefusesToMirrorCookies(t *testing.T) {
	br := NewBridge(nil, nil, t.TempDir())

	_, err := br.SyncCookies(context.Background(), nil)
	if err == nil {
		t.Fatal("want an error, got none")
	}
	if !strings.Contains(err.Error(), "no cookie domains configured") {
		t.Errorf("err = %v, want it to name the real reason", err)
	}
}
