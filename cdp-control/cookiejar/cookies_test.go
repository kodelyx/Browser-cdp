package cookiejar

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFromRawAndHash(t *testing.T) {
	jar := FromRaw("__Secure-1PSID=abc; SID=def", ".google.com", "test")

	if jar.Count() != 2 {
		t.Fatalf("expected 2 cookies, got %d", jar.Count())
	}
	if !jar.HasAuthCookies() {
		t.Error("a jar with __Secure-1PSID and SID should report credentials")
	}

	// The hash must be stable regardless of input ordering, because cached
	// tokens are keyed on it.
	reordered := FromRaw("SID=def; __Secure-1PSID=abc", ".google.com", "test")
	if jar.Hash() != reordered.Hash() {
		t.Errorf("hash should not depend on cookie order: %s != %s", jar.Hash(), reordered.Hash())
	}

	different := FromRaw("__Secure-1PSID=xyz; SID=def", ".google.com", "test")
	if jar.Hash() == different.Hash() {
		t.Error("a different cookie value must produce a different hash")
	}
}

func TestHasAuthCookies(t *testing.T) {
	analyticsOnly := FromCookies([]Cookie{
		{Domain: ".google.com", Path: "/", Name: "_ga", Value: "GA1.2.3"},
		{Domain: ".google.com", Path: "/", Name: "NID", Value: "511"},
	}, "test")

	if analyticsOnly.HasAuthCookies() {
		t.Error("a jar with only analytics cookies should not report credentials")
	}
	if !analyticsOnly.Expired(time.Now()) {
		t.Error("a jar with no credential cookie should count as expired")
	}
}

func TestExpiry(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).Unix()
	past := time.Now().Add(-2 * time.Hour).Unix()

	live := FromCookies([]Cookie{
		{Domain: ".google.com", Path: "/", Name: "__Secure-1PSID", Value: "a", ExpirationDate: float64(future)},
	}, "test")
	if live.Expired(time.Now()) {
		t.Error("a jar with an unexpired credential cookie should not be expired")
	}

	expiry, ok := live.EarliestExpiry()
	if !ok {
		t.Fatal("expected a known expiry")
	}
	if time.Until(expiry) < time.Hour {
		t.Errorf("earliest expiry %s is sooner than expected", expiry)
	}

	dead := FromCookies([]Cookie{
		{Domain: ".google.com", Path: "/", Name: "__Secure-1PSID", Value: "a", ExpirationDate: float64(past)},
	}, "test")
	if !dead.Expired(time.Now()) {
		t.Error("a jar whose only credential cookie has expired should be expired")
	}
}

func TestLoadFileJSONArray(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.json")

	content := `[
	  {"domain":".google.com","name":"__Secure-1PSID","path":"/","value":"abc","secure":true},
	  {"domain":".google.com","name":"SID","path":"/","value":"def"}
	]`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	jar, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if jar.Count() != 2 {
		t.Errorf("expected 2 cookies, got %d", jar.Count())
	}
	if !jar.HasAuthCookies() {
		t.Error("expected credentials to be detected")
	}
}

func TestLoadFileLegacyObject(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.json")

	content := `{"cookies":"__Secure-1PSID=abc; SID=def","updated_at":1700000000}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	jar, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if jar.Count() != 2 {
		t.Errorf("expected 2 cookies from the legacy format, got %d", jar.Count())
	}
}

func TestLoadFileUnsupported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.json")
	if err := os.WriteFile(path, []byte(`{"unexpected":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadFile(path); err == nil {
		t.Error("an unrecognised format should produce an error rather than an empty jar")
	}
}

func TestSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "cookies.json")

	original := FromCookies([]Cookie{
		{Domain: ".google.com", Path: "/", Name: "__Secure-1PSID", Value: "abc", Secure: true},
		{Domain: ".labs.google", Path: "/", Name: "session", Value: "xyz"},
	}, "test")

	if err := original.Save(path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile after Save failed: %v", err)
	}
	if loaded.Hash() != original.Hash() {
		t.Errorf("round trip changed the jar hash: %s != %s", loaded.Hash(), original.Hash())
	}
}

// TestForDomain checks the standard domain-match, path-match and Secure rules.
//
// Note the domains: labs.google and google.com are different registrable
// domains, so a .google.com cookie is not sent to labs.google. That is correct
// browser behaviour, and it is why the engine queries Labs and Google cookies
// separately rather than assuming one jar covers both.
func TestForDomain(t *testing.T) {
	jar := FromCookies([]Cookie{
		{Domain: ".labs.google", Path: "/", Name: "labs", Value: "2"},
		{Domain: ".google.com", Path: "/", Name: "google", Value: "1"},
		{Domain: ".example.com", Path: "/", Name: "other", Value: "3"},
		{Domain: ".labs.google", Path: "/", Name: "secureonly", Value: "4", Secure: true},
		{Domain: ".labs.google", Path: "/fx", Name: "scoped", Value: "5"},
	}, "test")

	https := jar.HeaderForDomain("https://labs.google/fx/tools/flow")
	for _, want := range []string{"labs=2", "secureonly=4", "scoped=5"} {
		if !contains(https, want) {
			t.Errorf("https request should carry %s, got: %s", want, https)
		}
	}
	for _, unwanted := range []string{"google=1", "other=3"} {
		if contains(https, unwanted) {
			t.Errorf("request should not carry %s, got: %s", unwanted, https)
		}
	}

	// A .google.com cookie is sent to a google.com host.
	googleHeader := jar.HeaderForDomain("https://accounts.google.com/")
	if !contains(googleHeader, "google=1") {
		t.Errorf("accounts.google.com should carry google=1, got: %s", googleHeader)
	}

	// A Secure cookie must not be sent over plain http.
	httpHeader := jar.HeaderForDomain("http://labs.google/")
	if contains(httpHeader, "secureonly=4") {
		t.Errorf("a Secure cookie must not be sent over http: %s", httpHeader)
	}

	// A path-scoped cookie must not leak to a sibling path.
	otherPath := jar.HeaderForDomain("https://labs.google/other")
	if contains(otherPath, "scoped=5") {
		t.Errorf("a /fx-scoped cookie must not be sent to /other: %s", otherPath)
	}
}

func TestExpiredCookiesAreExcluded(t *testing.T) {
	past := time.Now().Add(-time.Hour).Unix()
	jar := FromCookies([]Cookie{
		{Domain: ".labs.google", Path: "/", Name: "dead", Value: "1", ExpirationDate: float64(past)},
		{Domain: ".labs.google", Path: "/", Name: "live", Value: "2"},
	}, "test")

	header := jar.HeaderForDomain("https://labs.google/")
	if contains(header, "dead=1") {
		t.Errorf("an expired cookie must not be sent: %s", header)
	}
	if !contains(header, "live=2") {
		t.Errorf("a live cookie should be sent: %s", header)
	}
}

func TestDuplicatesCollapse(t *testing.T) {
	jar := FromCookies([]Cookie{
		{Domain: ".google.com", Path: "/", Name: "dup", Value: "first"},
		{Domain: ".google.com", Path: "/", Name: "dup", Value: "second"},
	}, "test")

	if jar.Count() != 1 {
		t.Errorf("identical domain/path/name should collapse to one cookie, got %d", jar.Count())
	}
}

func TestNamesDoesNotLeakValues(t *testing.T) {
	jar := FromRaw("__Secure-1PSID=SUPER_SECRET_VALUE", ".google.com", "test")

	for _, name := range jar.Names() {
		if name == "SUPER_SECRET_VALUE" {
			t.Fatal("Names() must never expose a cookie value")
		}
	}
}

func TestNilJarIsSafe(t *testing.T) {
	var jar *Jar
	if jar.Count() != 0 || jar.HasAuthCookies() || jar.Raw() != "" || !jar.Expired(time.Now()) {
		t.Error("a nil jar should behave as an empty, expired jar")
	}
	if jar.HeaderForDomain("https://labs.google/") != "" {
		t.Error("a nil jar should produce an empty header")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
