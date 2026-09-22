// Package bridge owns the WebSocket the browser-Cdp extension dials into.
//
// It is the entire browser surface of this project. The extension pushes cookies
// and base information; the engine derives everything else itself. There is no
// path by which a generation request travels through the browser.
package bridge

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kodelyx/Browser-cdp/cdp-control/src/cdp"
	"github.com/kodelyx/Browser-cdp/cdp-control/src/cookiejar"
)

// DefaultBlockedMethods is the CDP deny-list pushed to every extension.
//
// These are the methods that would let a caller read or write cookies outside
// the configured scope, or reach past the attached tab. The extension ships its
// own copy of this list, but the backend pushes it explicitly rather than
// relying on that default: the security posture should be declared by the party
// that depends on it, and it should be visible in this repository.
var DefaultBlockedMethods = []string{
	"Network.getCookies",
	"Network.getAllCookies",
	"Network.setCookie",
	"Network.setCookies",
	"Network.deleteCookies",
	"Network.clearBrowserCookies",
	"Storage.getCookies",
	"Storage.setCookies",
	"Storage.clearCookies",
	"Browser.close",
	"Browser.crash",
	"Target.createTarget",
	"Target.closeTarget",
	"Security.setIgnoreCertificateFlags",
	"Security.setIgnoreCertificateErrors",
}

// Bridge owns the WebSocket the browser-Cdp extension dials into.
type Bridge struct {
	// Targets are the URLs the extension is allowed to attach to. The first
	// entry is what it auto-opens when it has no tab. Empty means every tab is
	// attachable, which is the default for this tool.
	Targets []string
	// CookieDomains is the cookie scope handed to the extension. Empty means
	// every domain is in scope; it also switches cookie mirroring off, since
	// there is no declared site to mirror.
	CookieDomains []string
	// DataDir is where the bridge keeps its cookie file and its pairing token.
	// It used to come from the surrounding application's config, which is what
	// tied this package to one deployment.
	DataDir string
	// ListenAddr is the address the extension dials. Empty means
	// DefaultListenAddr.
	ListenAddr string
	// BlockedMethods is the CDP deny-list handed to the extension.
	BlockedMethods []string

	mu      sync.RWMutex
	clients map[string]*cdp.Client
	current *cdp.Client

	// lastJar is the most recent cookie set pulled from the browser.
	lastJar *cookiejar.Jar

	// cookieFile persists the last synced cookies so the engine can keep running
	// after the browser closes, which is the whole point of syncing them.
	cookieFile string

	// token guards the WebSocket upgrade so another local process cannot drive
	// the browser through the extension. See EnsureToken.
	token          string
	allowTokenless bool
	claimPath      string

	// refreshMu serialises session refreshes.
	//
	// Every worker draws on the same account, so they share its cookies and go
	// stale together — a fleet hitting a 401 at the same moment would otherwise
	// each attach to the same tab and wait out the whole pause. Serialising
	// collapses that to one round trip.
	refreshMu sync.Mutex
	// lastRefresh is when the last refresh completed, so a caller that arrived
	// while one was in flight can reuse it rather than repeating it.
	lastRefresh time.Time
}

// RefreshCoalesceWindow is how long a completed session refresh is treated as
// fresh enough to serve another caller.
//
// The window only has to cover the burst of 401s that follows a shared session
// going stale, which is sub-second in practice; three seconds is generous enough
// for slow schedulers without ever serving a jar old enough to matter.
const RefreshCoalesceWindow = 3 * time.Second

// DefaultListenAddr is where the extension looks for the bridge when the caller
// does not name an address.
const DefaultListenAddr = "127.0.0.1:9222"

// NewBridge builds a bridge with the given allowlists.
func NewBridge(targets, cookieDomains []string, dataDir string) *Bridge {
	return &Bridge{
		Targets:        targets,
		CookieDomains:  cookieDomains,
		DataDir:        dataDir,
		BlockedMethods: DefaultBlockedMethods,
		clients:        make(map[string]*cdp.Client),
		cookieFile:     filepath.Join(dataDir, "cookies.json"),
	}
}

// EnsureToken loads the bridge token, generating one on first run.
//
// The token exists because the origin check alone is not a boundary: a
// non-browser local process simply omits the Origin header, and would otherwise
// be free to attach to the user's tabs. Pairing works as trust-on-first-use,
// because the extension has no way to learn the token before it can connect:
//
//   - First run ever (no token file): a token is generated, and a tokenless
//     connection is accepted once so the extension can be handed it.
//   - After that connection, a claim marker is written and the token is
//     required on every subsequent upgrade.
//   - To re-pair, delete the claim marker next to the token file.
//
// The token file is written 0600 in the data directory.
func (b *Bridge) EnsureToken() error {
	dir := b.DataDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tokenPath := filepath.Join(dir, "bridge-token")
	claimPath := tokenPath + ".claimed"

	data, err := os.ReadFile(tokenPath)
	switch {
	case err == nil && len(strings.TrimSpace(string(data))) >= 32:
		b.token = strings.TrimSpace(string(data))
	case errors.Is(err, os.ErrNotExist):
		generated, genErr := generateToken()
		if genErr != nil {
			return genErr
		}
		if writeErr := os.WriteFile(tokenPath, []byte(generated+"\n"), 0o600); writeErr != nil {
			return writeErr
		}
		b.token = generated
		log.Printf("bridge: generated a bridge token at %s", tokenPath)
	default:
		return fmt.Errorf("bridge: could not read %s: %w", tokenPath, err)
	}

	// Only waive the token while the extension has never successfully paired.
	if _, err := os.Stat(claimPath); errors.Is(err, os.ErrNotExist) {
		b.allowTokenless = true
		log.Printf("bridge: no paired extension yet — accepting one tokenless connection to pair")
	}

	b.claimPath = claimPath
	return nil
}

func generateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("bridge: could not generate a token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// claim marks the extension as paired, so the tokenless window closes.
func (b *Bridge) claim() {
	if b.claimPath == "" {
		return
	}
	b.mu.Lock()
	path := b.claimPath
	b.allowTokenless = false
	b.mu.Unlock()

	if err := os.WriteFile(path, []byte("paired\n"), 0o600); err != nil {
		log.Printf("bridge: could not write the pairing marker: %v", err)
		return
	}
	log.Printf("bridge: extension paired — the bridge token is now required on every connection")
}

// Listen starts the WebSocket server. Blocks until ctx is cancelled.
func (b *Bridge) Listen(ctx context.Context) error {
	if b.token == "" {
		if err := b.EnsureToken(); err != nil {
			return err
		}
	}

	addr := b.ListenAddr
	if addr == "" {
		addr = DefaultListenAddr
	}
	log.Printf("bridge: listening for a browser extension on ws://%s (token required)", addr)

	return cdp.Listen(ctx, cdp.ListenOptions{
		Addr:  addr,
		Token: b.token,
		AllowTokenless: func() bool {
			b.mu.RLock()
			defer b.mu.RUnlock()
			return b.allowTokenless
		},
		// The one refusal a user actually has to act on. Chrome clears
		// chrome.storage.local when an unpacked extension is removed and added
		// again, so a paired extension can come back with no token, and nothing
		// in the log used to say what to do about it.
		RepairHint: fmt.Sprintf(
			"Delete %s and restart, or paste the token from %s into the extension popup.",
			b.claimPath, filepath.Join(b.DataDir, "bridge-token")),
		OnAuthenticated: func(client *cdp.Client) {
			b.claim()
		},
		OnConnect: func(client *cdp.Client) {
			b.add(client)
			go b.onConnect(ctx, client)
		},
	})
}

func (b *Bridge) add(client *cdp.Client) {
	// The most recent connection becomes `current`, which is what every call
	// uses.
	//
	// Nothing distinguishes one extension from another any more: every call goes
	// over the generic CDP surface in the cdp package, so no connection is
	// better than another and there is no preference left to encode. Last-one-wins
	// is the rule that needs no explanation, and the one a reader already
	// assumes.
	b.mu.Lock()
	defer b.mu.Unlock()

	b.clients[client.RemoteAddr] = client
	b.current = client
}

func (b *Bridge) remove(client *cdp.Client) {
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.clients, client.RemoteAddr)
	if b.current != client {
		return
	}

	// Promote whatever is left. There is no longer a better or worse extension to
	// choose between, so any remaining connection will do.
	b.current = nil
	for _, other := range b.clients {
		b.current = other
		return
	}
}

// onConnect declares our allowlists and pulls an initial cookie set, so a fresh
// extension becomes useful without the user touching the popup.
func (b *Bridge) onConnect(ctx context.Context, client *cdp.Client) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("bridge: recovered from panic in connect handler: %v", r)
		}
	}()

	// Watch for disconnects so `current` does not point at a dead socket.
	events, cancel := client.Subscribe(32)
	defer cancel()
	go func() {
		for ev := range events {
			if ev.Name == "cdp.detached" {
				log.Printf("bridge: extension detached: %s", string(ev.Params))
			}
		}
	}()

	pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
	version, err := client.Ping(pingCtx)
	pingCancel()
	if err != nil {
		log.Printf("bridge: extension handshake failed: %v", err)
		b.remove(client)
		return
	}
	log.Printf("bridge: extension v%s ready", version)

	// If the tokenless window is still open, this extension has not adopted the
	// token yet. Say so plainly: the bridge works, but it is not yet locked to
	// this extension, and any local process could also connect.
	b.mu.RLock()
	unpaired := b.allowTokenless
	b.mu.RUnlock()
	if unpaired {
		log.Printf("bridge: WARNING pairing is open — this connection arrived without a token, " +
			"and the token is being handed to it now. Until the extension reconnects with it, " +
			"any local process could also connect to this socket.")
	}

	if err := b.PushConfig(ctx, client); err != nil {
		log.Printf("bridge: could not push config: %v", err)
	}

	// Cookie mirroring is off unless the caller named domains.
	//
	// The mirror exists so a backend can keep talking to a site after the browser
	// closes. That is only meaningful when the caller declared which site, and
	// syncing "every cookie in the browser" to disk is not something a debugging
	// tool should do by default — a caller that wants a cookie reads it from the
	// live browser through /cookies. Attempting it unscoped just produces a
	// misleading "is the account signed in?" every time an extension connects.
	if len(b.CookieDomains) == 0 {
		log.Printf("bridge: no cookie domains configured — running unscoped, cookie mirroring off")
		return
	}

	if _, err := b.SyncCookies(ctx, client); err != nil {
		log.Printf("bridge: initial cookie sync failed: %v", err)
	}
}

// PushConfig declares this project's scope to the extension.
//
// This is a permanent narrowing when a scope is set: the extension persists
// config in chrome.storage.local, so it outlives this process. The applied scope
// is therefore logged on every push, not only when it changes, so the log always
// shows what the extension was told to do.
func (b *Bridge) PushConfig(ctx context.Context, client *cdp.Client) error {
	client = b.resolve(client)
	if client == nil {
		return fmt.Errorf("bridge: no extension connected")
	}

	applied, err := client.SetConfig(ctx, b.configPatch())
	if err != nil {
		return err
	}

	log.Printf("bridge: scope applied — targets=%v cookie_domains=%v blocked_methods=%d token=%s",
		applied.TargetURLPrefixes, applied.CookieDomains,
		len(applied.BlockedMethods), tokenLabel(applied.BridgeToken))
	return nil
}

// configPatch is the scope this bridge declares to an extension.
//
// Separated from PushConfig so the declaration can be asserted without a browser
// attached. What it contains is the difference between universal access and an
// extension that quietly stayed narrowed, and that is not something to find out
// from a live connection.
func (b *Bridge) configPatch() cdp.Config {
	// Let the extension open the target itself when nothing matches, so a backend
	// can start before the browser and still work. With no scope declared there is
	// no target to open, and the extension says so rather than opening something
	// arbitrary.
	autoOpenCommand := true
	autoOpenStart := true
	focus := true

	return cdp.Config{
		TargetURLPrefixes: orEmpty(b.Targets),
		CookieDomains:     orEmpty(b.CookieDomains),
		BlockedMethods:    b.BlockedMethods,
		BridgeToken:       b.token,
		AutoOpenOnCommand: &autoOpenCommand,
		AutoOpenOnStart:   &autoOpenStart,
		FocusOnAutoOpen:   &focus,
	}
}

// tokenLabel describes a token without printing it.
func tokenLabel(token string) string {
	if token == "" {
		return "not set"
	}
	return fmt.Sprintf("%d chars", len(token))
}

// orEmpty turns a nil slice into an empty one.
//
// `null` and `[]` are not the same thing on the wire. The extension reads an empty
// allowlist as full access, and Go's zero value for a slice marshals as `null`,
// which only reaches that behaviour by way of a `|| []` coercion on the far side.
// Saying "no scope" explicitly is worth four lines: the caller's intent should be
// visible in the bytes, not inferred from a language default.
func orEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// SyncCookies pulls the scoped cookies from the browser, persists them, and
// returns them as a jar. This is the only data the browser supplies.
func (b *Bridge) SyncCookies(ctx context.Context, client *cdp.Client) (*cookiejar.Jar, error) {
	// The scope is checked before the connection on purpose. "No cookie domains
	// configured" is true whatever is attached, and it is the answer that says the
	// feature is off rather than the answer that sends the caller off to attach an
	// extension and come back to the same refusal.
	if len(b.CookieDomains) == 0 {
		return nil, fmt.Errorf("bridge: no cookie domains configured — cookie mirroring is off when unscoped")
	}

	client = b.resolve(client)
	if client == nil {
		return nil, fmt.Errorf("bridge: no extension connected")
	}

	// Query per domain so the extension's scope check passes, and so a failure
	// on one domain does not lose the others.
	seen := make(map[string]cdp.Cookie)
	for _, domain := range b.CookieDomains {
		cookies, err := client.ListCookies(ctx, map[string]any{"domain": domain})
		if err != nil {
			log.Printf("bridge: cookie query for %s failed: %v", domain, err)
			continue
		}
		for _, ck := range cookies {
			key := ck.Domain + "|" + ck.Path + "|" + ck.Name
			seen[key] = ck
		}
	}

	if len(seen) == 0 {
		return nil, fmt.Errorf(
			"bridge: the extension returned no cookies for %v — is the account signed in?", b.CookieDomains)
	}

	converted := make([]cookiejar.Cookie, 0, len(seen))
	skipped := 0
	for _, ck := range seen {
		// Only the cookies a session depends on leave the browser. A signed-in
		// profile carries hundreds for Mail, Drive, Play, NotebookLM and
		// analytics, none of which have any bearing here — together they made a
		// 19 kB Cookie header, and they were being written to disk for no reason.
		if !cookiejar.IsEssential(ck.Name) {
			skipped++
			continue
		}
		converted = append(converted, cookiejar.Cookie{
			Domain:         ck.Domain,
			ExpirationDate: ck.ExpirationDate,
			HostOnly:       ck.HostOnly,
			HTTPOnly:       ck.HTTPOnly,
			Name:           ck.Name,
			Path:           ck.Path,
			SameSite:       ck.SameSite,
			Secure:         ck.Secure,
			Session:        ck.Session,
			StoreID:        ck.StoreID,
			Value:          ck.Value,
		})
	}

	if len(converted) == 0 {
		return nil, fmt.Errorf(
			"bridge: none of the %d cookies the browser offered are ones a session needs — is the account signed in?",
			len(seen))
	}

	// Name the jar after the bridge that supplied it.
	jar := cookiejar.FromCookies(converted, "browser-cdp")

	b.mu.Lock()
	b.lastJar = jar
	b.mu.Unlock()

	if err := jar.Save(b.cookieFile); err != nil {
		log.Printf("bridge: could not persist cookies: %v", err)
	} else {
		log.Printf("bridge: synced %d cookies (%d credential cookies, %d unrelated dropped) to %s",
			jar.Count(), countAuth(jar), skipped, b.cookieFile)
	}

	return jar, nil
}

func countAuth(jar *cookiejar.Jar) int {
	n := 0
	for _, ck := range jar.Cookies() {
		for _, want := range cookiejar.AuthCookieNames {
			if ck.Name == want {
				n++
				break
			}
		}
	}
	return n
}

// Jar returns the most recently synced cookies, falling back to the persisted
// copy so the engine works with the browser closed.
func (b *Bridge) Jar() *cookiejar.Jar {
	b.mu.RLock()
	if b.lastJar != nil {
		jar := b.lastJar
		b.mu.RUnlock()
		return jar
	}
	b.mu.RUnlock()

	if jar, err := cookiejar.LoadFile(b.cookieFile); err == nil {
		b.mu.Lock()
		b.lastJar = jar
		b.mu.Unlock()
		return jar
	}
	return nil
}

// Current returns the live extension connection, if any.
func (b *Bridge) Current() *cdp.Client {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.current
}

// Clients returns every attached extension, keyed by peer address.
//
// More than one can be connected at once — a second browser, or the same
// extension pointed at two bridges — but only the most recent becomes `current`,
// which is what every ordinary call uses. Reaching the others is what makes a
// comparison possible without disconnecting one to look at the other, and a
// comparison is the only way to tell "the browser does not have this" apart from
// "this extension cannot see it".
func (b *Bridge) Clients() map[string]*cdp.Client {
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := make(map[string]*cdp.Client, len(b.clients))
	for addr, client := range b.clients {
		if client != nil && client.Connected() {
			out[addr] = client
		}
	}
	return out
}

// Connected reports whether an extension is attached.
func (b *Bridge) Connected() bool {
	c := b.Current()
	return c != nil && c.Connected()
}

// WaitForExtension blocks until an extension connects and completes its
// handshake, or the timeout expires.
func (b *Bridge) WaitForExtension(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if b.Connected() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("bridge: no browser-Cdp extension connected within %s", timeout)
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// RefreshSession asks the browser to bring the target page up and settle, then
// re-syncs cookies.
//
// This is the one place the browser does something beyond handing over cookies,
// and it is unavoidable: Labs invalidates its own session server-side and marks
// the minted token ACCESS_TOKEN_REFRESH_NEEDED when the cookies have aged out.
// Only the site itself can renew its session, so the engine opens the page (the
// extension auto-opens it if no tab is present), lets it refresh, and re-reads
// the cookies. No generation traffic goes through the browser either way.
//
// Callers are coalesced: a refresh that finished within RefreshCoalesceWindow is
// reused rather than repeated, and concurrent callers serialise behind one
// round trip. That matters because the engine wires this into a per-worker
// 401 handler and every worker draws on the same account — they go stale
// together, so without coalescing a fleet would attach to the same tab N times.
func (b *Bridge) RefreshSession(ctx context.Context) (*cookiejar.Jar, error) {
	// One refresh at a time, and a caller that arrives while one is in flight
	// reuses its result. Workers share an account's cookies, so they go stale
	// together: without this, a fleet hitting 401 at once would each attach to
	// the same tab and wait out the full pause, serialised but pointless.
	//
	// This is checked before the connection test on purpose — a jar refreshed
	// moments ago is fresh whether or not the extension is still attached.
	b.refreshMu.Lock()
	defer b.refreshMu.Unlock()

	if age := time.Since(b.lastRefresh); !b.lastRefresh.IsZero() && age < RefreshCoalesceWindow {
		if jar := b.Jar(); jar != nil {
			log.Printf("bridge: reusing the session refresh from %s ago",
				age.Round(time.Millisecond))
			return jar, nil
		}
	}

	client := b.Current()
	if client == nil || !client.Connected() {
		return nil, fmt.Errorf("bridge: no extension connected")
	}

	attachCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	tab, err := client.Attach(attachCtx, 0)
	if err != nil {
		return nil, fmt.Errorf("bridge: could not attach to the target tab: %w", err)
	}
	log.Printf("bridge: attached to %s for a session refresh", tab.URL)

	// Give the page time to run its own session renewal and write fresh cookies.
	// There is no event to wait on for this, so it is a bounded pause.
	select {
	case <-time.After(6 * time.Second):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	jar, err := b.SyncCookies(ctx, client)
	if err != nil {
		return nil, err
	}
	// Only a completed refresh resets the window; a failure must not make the
	// next caller think a good refresh just happened.
	b.lastRefresh = time.Now()
	return jar, nil
}

// Status is a JSON-friendly snapshot for the /health endpoint.
type Status struct {
	Connected      bool     `json:"extension_connected"`
	Version        string   `json:"extension_version,omitempty"`
	RemoteAddr     string   `json:"remote_addr,omitempty"`
	CookieCount    int      `json:"cookie_count"`
	HasCredentials bool     `json:"has_credentials"`
	Targets        []string `json:"targets"`
	CookieDomains  []string `json:"cookie_domains"`

	// ExtensionOps lists the operations the attached extension advertises. It is
	// empty until the extension has been asked, which happens on the first status
	// call and is cached after that.
	ExtensionOps []string `json:"extension_ops,omitempty"`
}

// Status reports the current bridge state.
func (b *Bridge) Status() Status {
	s := Status{Targets: b.Targets, CookieDomains: b.CookieDomains}

	if client := b.Current(); client != nil && client.Connected() {
		s.Connected = true
		s.RemoteAddr = client.RemoteAddr
		if v, ok := client.Version.Load().(string); ok {
			s.Version = v
		}
		s.ExtensionOps = client.Ops()
	}

	if jar := b.Jar(); jar != nil {
		s.CookieCount = jar.Count()
		s.HasCredentials = jar.HasAuthCookies()
	}
	return s
}

func (b *Bridge) resolve(client *cdp.Client) *cdp.Client {
	if client != nil {
		return client
	}
	return b.Current()
}

// MarshalStatus is a debugging helper that renders the status as indented JSON.
func (b *Bridge) MarshalStatus() string {
	data, err := json.MarshalIndent(b.Status(), "", "  ")
	if err != nil {
		return err.Error()
	}
	return string(data)
}
