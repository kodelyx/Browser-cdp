// Package cookiejar models the one piece of state that legitimately comes from
// the browser: cookies.
//
// Everything else — access tokens, project IDs, generation calls, polling,
// downloads — is derived by the Go engine from these cookies. The browser is
// never asked to execute a generation request.
package cookiejar

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// Cookie is a browser cookie. The JSON tags match the shape both
// chrome.cookies and the browser-Cdp bridge emit, so a dump can be fed in
// unmodified.
type Cookie struct {
	Domain         string  `json:"domain"`
	ExpirationDate float64 `json:"expirationDate,omitempty"`
	HostOnly       bool    `json:"hostOnly,omitempty"`
	HTTPOnly       bool    `json:"httpOnly,omitempty"`
	Name           string  `json:"name"`
	Path           string  `json:"path"`
	SameSite       string  `json:"sameSite,omitempty"`
	Secure         bool    `json:"secure,omitempty"`
	Session        bool    `json:"session,omitempty"`
	StoreID        string  `json:"storeId,omitempty"`
	Value          string  `json:"value"`
}

// ExpiresAt returns the cookie's expiry, or the zero time for session cookies.
func (c Cookie) ExpiresAt() time.Time {
	if c.Session || c.ExpirationDate <= 0 {
		return time.Time{}
	}
	sec := int64(c.ExpirationDate)
	nsec := int64((c.ExpirationDate - float64(sec)) * 1e9)
	return time.Unix(sec, nsec)
}

// Expired reports whether the cookie is past its expiry at the given instant.
func (c Cookie) Expired(now time.Time) bool {
	exp := c.ExpiresAt()
	return !exp.IsZero() && exp.Before(now)
}

// Jar is an immutable set of cookies plus the derived request headers.
type Jar struct {
	cookies []Cookie
	raw     string
	hash    string
	source  string
}

// AuthCookieNames are the cookies the Labs session exchange actually depends on.
// Their presence is what "has usable credentials" means; a jar without them is
// inert no matter how many other cookies it carries.
var AuthCookieNames = []string{
	"__Secure-next-auth.session-token",
	"__Secure-1PSID",
	"__Secure-3PSID",
	"__Secure-1PSIDTS",
	"__Secure-3PSIDTS",
	"SID",
	"HSID",
	"SSID",
	"APISID",
	"SAPISID",
}

// HasAuthCookies reports whether the jar carries at least one credential cookie.
func (j *Jar) HasAuthCookies() bool {
	if j == nil {
		return false
	}
	for _, ck := range j.cookies {
		for _, want := range AuthCookieNames {
			if strings.EqualFold(ck.Name, want) {
				return true
			}
		}
	}
	return false
}

// EssentialCookieNames is the set worth carrying out of the browser.
//
// A signed-in Chrome profile holds hundreds of cookies and almost none of them
// have any bearing on Flow: analytics beacons (_ga and its per-property
// variants, _gcl_*, __utmz), a per-product session for Mail, Drive, Play,
// NotebookLM, Colab and the rest, and OSID copies for each of those services.
// Syncing them all produced a 19 kB Cookie header — large enough that one
// endpoint answered 431 Request Header Fields Too Large — and wrote a great deal
// of unrelated session state to a file on disk for no benefit.
//
// What is left is the credential set the Labs session exchange needs, the CSRF
// and callback cookies the NextAuth rebuild needs, and the few the Flow app sets
// for itself.
var EssentialCookieNames = []string{
	// The Labs session, and the Google identity cookies it is rebuilt from.
	"__Secure-next-auth.session-token",
	"__Secure-next-auth.callback-url",
	"__Host-next-auth.csrf-token",
	"__Secure-1PSID",
	"__Secure-3PSID",
	"__Secure-1PSIDTS",
	"__Secure-3PSIDTS",
	"SID",
	"HSID",
	"SSID",
	"APISID",
	"SAPISID",
	// Set by the Flow app itself.
	"EMAIL",
	"OSID",
	"__Secure-OSID",
	// The account session, and the record of which accounts are signed in.
	//
	// `authuser=N` selects among the accounts a browser is signed into, but it
	// only means anything alongside the cookies that carry those sessions.
	// Dropping these left the parameter with nothing to select, so every index
	// answered for the default account and the account list read as one long.
	"LSID",
	"LSOLH",
	"__Host-1PLSID",
	"__Host-3PLSID",
	"ACCOUNT_CHOOSER",
}

// IsEssential reports whether a cookie is worth keeping.
func IsEssential(name string) bool {
	for _, want := range EssentialCookieNames {
		if strings.EqualFold(name, want) {
			return true
		}
	}
	return false
}

// Cookies returns a copy of the underlying slice.
func (j *Jar) Cookies() []Cookie {
	if j == nil {
		return nil
	}
	out := make([]Cookie, len(j.cookies))
	copy(out, j.cookies)
	return out
}

// Raw returns the cookies rendered as a request Cookie header value.
func (j *Jar) Raw() string {
	if j == nil {
		return ""
	}
	return j.raw
}

// Hash is a stable fingerprint of the jar's contents. Cached tokens are keyed on
// this so a token can never outlive the cookies it was minted from.
func (j *Jar) Hash() string {
	if j == nil {
		return ""
	}
	return j.hash
}

// Source describes where the jar came from, for logging and diagnostics.
func (j *Jar) Source() string {
	if j == nil {
		return ""
	}
	return j.source
}

// Count returns the number of cookies held.
func (j *Jar) Count() int {
	if j == nil {
		return 0
	}
	return len(j.cookies)
}

// EarliestExpiry returns the soonest expiry among the credential cookies, and
// false when every credential cookie is a session cookie.
func (j *Jar) EarliestExpiry() (time.Time, bool) {
	if j == nil {
		return time.Time{}, false
	}
	var best time.Time
	found := false
	for _, ck := range j.cookies {
		if !isAuthCookie(ck.Name) {
			continue
		}
		exp := ck.ExpiresAt()
		if exp.IsZero() {
			continue
		}
		if !found || exp.Before(best) {
			best = exp
			found = true
		}
	}
	return best, found
}

// Expired reports whether the jar is no longer usable: either it carries no
// credential cookie at all, or every credential cookie it carries has expired.
// Non-credential cookies are ignored, since a jar full of analytics cookies is
// not a usable session.
func (j *Jar) Expired(now time.Time) bool {
	if j == nil {
		return true
	}
	for _, ck := range j.cookies {
		if !isAuthCookie(ck.Name) {
			continue
		}
		if !ck.Expired(now) {
			return false
		}
	}
	// Reaching here means either no credential cookie exists, or all of them
	// have expired. Both are "not usable".
	return true
}

func isAuthCookie(name string) bool {
	for _, want := range AuthCookieNames {
		if strings.EqualFold(name, want) {
			return true
		}
	}
	return false
}

/* ------------------------------------------------------------------ *
 * Construction
 * ------------------------------------------------------------------ */

func newJar(cookies []Cookie, source string) *Jar {
	cleaned := make([]Cookie, 0, len(cookies))
	seen := make(map[string]struct{}, len(cookies))
	for _, ck := range cookies {
		if ck.Name == "" {
			continue
		}
		key := strings.ToLower(ck.Domain) + "|" + ck.Path + "|" + ck.Name
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		cleaned = append(cleaned, ck)
	}

	// Sort for a stable hash regardless of the order the browser returned them.
	sort.SliceStable(cleaned, func(i, k int) bool {
		if cleaned[i].Domain != cleaned[k].Domain {
			return cleaned[i].Domain < cleaned[k].Domain
		}
		if cleaned[i].Path != cleaned[k].Path {
			return cleaned[i].Path < cleaned[k].Path
		}
		return cleaned[i].Name < cleaned[k].Name
	})

	parts := make([]string, 0, len(cleaned))
	for _, ck := range cleaned {
		parts = append(parts, ck.Name+"="+ck.Value)
	}
	raw := strings.Join(parts, "; ")
	sum := sha256.Sum256([]byte(raw))

	return &Jar{
		cookies: cleaned,
		raw:     raw,
		hash:    hex.EncodeToString(sum[:]),
		source:  source,
	}
}

// FromCookies builds a jar from parsed cookie objects.
func FromCookies(cookies []Cookie, source string) *Jar {
	return newJar(cookies, source)
}

// FromRaw builds a jar from a raw "a=1; b=2" header string. Domain is applied to
// every cookie, since a raw header carries no domain information.
func FromRaw(raw, domain, source string) *Jar {
	if domain == "" {
		domain = ".google.com"
	}
	var cookies []Cookie
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		cookies = append(cookies, Cookie{
			Domain: domain,
			Path:   "/",
			Name:   strings.TrimSpace(name),
			Value:  strings.TrimSpace(value),
		})
	}
	return newJar(cookies, source)
}

// LoadFile reads a cookie file. Two formats are accepted, matching what the
// Python engine and the browser-Cdp bridge produce:
//
//  1. a JSON array of cookie objects
//  2. a legacy object {"cookies": "a=1; b=2", "updated_at": 123}
func LoadFile(path string) (*Jar, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var list []Cookie
	if err := json.Unmarshal(data, &list); err == nil && len(list) > 0 {
		return newJar(list, path), nil
	}

	var legacy struct {
		Cookies   string  `json:"cookies"`
		UpdatedAt float64 `json:"updated_at"`
	}
	if err := json.Unmarshal(data, &legacy); err == nil && legacy.Cookies != "" {
		return FromRaw(legacy.Cookies, ".google.com", path), nil
	}

	// Last resort: a bare header string.
	trimmed := strings.TrimSpace(string(data))
	if trimmed != "" && strings.Contains(trimmed, "=") && !strings.HasPrefix(trimmed, "{") {
		return FromRaw(trimmed, ".google.com", path), nil
	}

	return nil, fmt.Errorf("cookiejar: unsupported cookie format in %s", path)
}

// Save writes the jar to path as a JSON array, creating parent directories.
func (j *Jar) Save(path string) error {
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(j.cookies, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func dirOf(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i > 0 {
		return path[:i]
	}
	return "."
}

/* ------------------------------------------------------------------ *
 * Domain helpers
 * ------------------------------------------------------------------ */

// ForDomain returns the subset of cookies that would be sent to a URL, applying
// the standard domain-match and path-match rules plus Secure.
func (j *Jar) ForDomain(rawURL string) []Cookie {
	if j == nil {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	secure := strings.EqualFold(u.Scheme, "https")
	now := time.Now()

	var out []Cookie
	for _, ck := range j.cookies {
		if ck.Expired(now) {
			continue
		}
		if ck.Secure && !secure {
			continue
		}
		domain := strings.ToLower(ck.Domain)
		bare := strings.TrimPrefix(domain, ".")
		if !(host == bare || strings.HasSuffix(host, "."+bare)) {
			continue
		}
		if ck.Path != "" && !strings.HasPrefix(path, ck.Path) {
			continue
		}
		out = append(out, ck)
	}
	return out
}

// HeaderForDomain renders the cookies that apply to rawURL as a Cookie header.
func (j *Jar) HeaderForDomain(rawURL string) string {
	subset := j.ForDomain(rawURL)
	if len(subset) == 0 {
		return ""
	}
	parts := make([]string, 0, len(subset))
	for _, ck := range subset {
		parts = append(parts, ck.Name+"="+ck.Value)
	}
	return strings.Join(parts, "; ")
}

// Names lists the cookie names held, for diagnostics. Values are never exposed.
func (j *Jar) Names() []string {
	if j == nil {
		return nil
	}
	out := make([]string, 0, len(j.cookies))
	for _, ck := range j.cookies {
		out = append(out, ck.Name)
	}
	return out
}
