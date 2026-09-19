package bridge

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodelyx/cdp-control/cookiejar"
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

// TestURLMatchesAccount pins the account test that keeps a project from being
// reused across accounts.
//
// Projects belong to one account, so a URL remembered from another does not open
// — it 404s — and the engine would then hold a project id its session cannot use.
// Google addresses accounts with `/u/<n>/`, and a URL without one is the first.
func TestURLMatchesAccount(t *testing.T) {
	cases := []struct {
		url   string
		index int
		want  bool
	}{
		{"https://flow.google.com/project/abc", 0, true},
		{"https://flow.google.com/project/abc", 1, false},
		{"https://flow.google.com/project/abc", 2, false},

		{"https://flow.google.com/u/0/project/abc", 0, true},
		{"https://flow.google.com/u/1/project/abc", 1, true},
		{"https://flow.google.com/u/2/project/abc", 2, true},
		{"https://flow.google.com/u/1/project/abc", 0, false},
		{"https://flow.google.com/u/1/project/abc", 2, false},

		{"https://flow.google.com/u/2/?pli=1", 2, true},
		{"https://flow.google.com/u/2/?pli=1", 1, false},

		// A malformed or trailing index must not be mistaken for account 0 when
		// the caller asked for something else.
		{"https://flow.google.com/u/x/project/abc", 1, false},
		{"https://flow.google.com/u/", 1, false},
		{"https://flow.google.com/", 0, true},
		{"https://flow.google.com/", 2, false},
	}

	for _, tc := range cases {
		if got := urlMatchesAccount(tc.url, tc.index); got != tc.want {
			t.Errorf("urlMatchesAccount(%q, %d) = %v, want %v", tc.url, tc.index, got, tc.want)
		}
	}
}
