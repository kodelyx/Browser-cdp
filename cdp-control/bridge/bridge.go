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
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kodelyx/cdp-control/cdp"
	"github.com/kodelyx/cdp-control/cookiejar"
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
	// entry is what it auto-opens when it has no tab.
	Targets []string
	// CookieDomains is the cookie scope handed to the extension.
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
	// A Flow bridge beats a generic one, whatever the connection order.
	//
	// Both can be attached at once, and `current` is what every ordinary call
	// uses. Last-one-wins meant the purpose-built extension lost to a generic CDP
	// bridge whenever the generic one happened to reconnect — not a decision
	// anybody made, just an artefact of timing. The Flow extension exposes the
	// narrow operations the backend wants; the generic one is the fallback, so it
	// only takes `current` when nothing better is attached.
	//
	// The probe is outside the lock because it is a round trip. `hasFlowOps` pings
	// once and caches the answer, so this costs one call per connection and
	// nothing after that.
	flow := client.ProbeSurface().FlowOperations

	b.mu.Lock()
	defer b.mu.Unlock()

	b.clients[client.RemoteAddr] = client
	if flow || b.current == nil || !b.current.ProbeSurface().FlowOperations {
		b.current = client
	}
}

func (b *Bridge) remove(client *cdp.Client) {
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.clients, client.RemoteAddr)
	if b.current != client {
		return
	}

	// Same preference on the way out: a Flow bridge if one is still attached,
	// otherwise whatever is left.
	b.current = nil
	for _, other := range b.clients {
		if other.ProbeSurface().FlowOperations {
			b.current = other
			return
		}
		if b.current == nil {
			b.current = other
		}
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

	// Say which surface this extension offers, because it decides the path every
	// Flow call takes. The two extensions are not interchangeable: one exposes
	// the backend's own narrow operations, the other only the generic evaluate
	// fallback. Both work, so nothing looks wrong when the wrong one is loaded —
	// the difference is what the browser is being asked to do, and how much
	// surface is being shipped to it.
	surface := client.ProbeSurface()
	if surface.FlowOperations {
		log.Printf("bridge: extension offers %d operations including the Flow surface — using the narrow calls",
			len(surface.Advertised))
	} else {
		log.Printf("bridge: extension offers no Flow operations — falling back to the generic evaluate surface")
	}

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

	if _, err := b.SyncCookies(ctx, client); err != nil {
		log.Printf("bridge: initial cookie sync failed: %v", err)
	}
}

// PushConfig declares this project's allowlists to the extension.
//
// This is a permanent narrowing: the extension persists config in
// chrome.storage.local, so it survives after flow-go disconnects. The applied
// scope is therefore logged on every push, not only when it changes, so the
// backend log always shows what the extension was told to do.
func (b *Bridge) PushConfig(ctx context.Context, client *cdp.Client) error {
	client = b.resolve(client)
	if client == nil {
		return fmt.Errorf("bridge: no extension connected")
	}

	// Let the extension open the target itself when nothing matches, so the
	// engine can start before the browser and still work.
	autoOpenCommand := true
	autoOpenStart := true
	focus := true

	patch := cdp.Config{
		TargetURLPrefixes: b.Targets,
		CookieDomains:     b.CookieDomains,
		BlockedMethods:    b.BlockedMethods,
		BridgeToken:       b.token,
		AutoOpenOnCommand: &autoOpenCommand,
		AutoOpenOnStart:   &autoOpenStart,
		FocusOnAutoOpen:   &focus,
	}

	applied, err := client.SetConfig(ctx, patch)
	if err != nil {
		return err
	}

	log.Printf("bridge: scope applied — targets=%v cookie_domains=%v blocked_methods=%d token=%s",
		applied.TargetURLPrefixes, applied.CookieDomains,
		len(applied.BlockedMethods), tokenLabel(applied.BridgeToken))
	return nil
}

// tokenLabel describes a token without printing it.
func tokenLabel(token string) string {
	if token == "" {
		return "not set"
	}
	return fmt.Sprintf("%d chars", len(token))
}

// SyncCookies pulls the scoped cookies from the browser, persists them, and
// returns them as a jar. This is the only data the browser supplies.
func (b *Bridge) SyncCookies(ctx context.Context, client *cdp.Client) (*cookiejar.Jar, error) {
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
		// Only the cookies Flow depends on leave the browser. A signed-in profile
		// carries hundreds for Mail, Drive, Play, NotebookLM and analytics, none
		// of which have any bearing here — together they made a 19 kB Cookie
		// header, and they were being written to disk for no reason.
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
			"bridge: none of the %d cookies the browser offered are ones Flow needs — is the account signed in?",
			len(seen))
	}

	// Name the jar after the bridge that actually supplied it.
	//
	// The label was the literal "browser-cdp" wherever a jar was built, so a run
	// through the Flow extension still reported browser-cdp. It reads as
	// information and is not, which is why it went unnoticed: every other field on
	// that line was right, and the one that was wrong looked like a reading.
	source := "browser-cdp"
	if client.ProbeSurface().FlowOperations {
		source = "flow-go-extension"
	}
	jar := cookiejar.FromCookies(converted, source)

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
// More than one can be connected at once — a second browser, or the generic
// bridge alongside the Flow one — but only the most recent becomes `current`,
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

// projectLinkExpression scrapes the project list from the Flow app landing page.
// The list is rendered as ordinary anchors, so this needs no framework access.
const projectLinkExpression = `(() => [...document.querySelectorAll('a[href*="/project/"]')]` +
	`.map(a => a.href).slice(0, 40))()`

// projectPathMarker identifies a Flow project editor URL.
const projectPathMarker = "/project/"

// urlMatchesAccount reports whether a Flow URL belongs to a signed-in account.
//
// Google addresses its accounts with a `/u/<n>/` path segment; a URL without one
// is the first account. This is the same index the `authuser` query parameter
// carries, which is why it is the only thing that distinguishes one account's
// project from another's.
func urlMatchesAccount(rawURL string, accountIndex int) bool {
	const marker = "/u/"
	i := strings.Index(rawURL, marker)
	if i < 0 {
		return accountIndex == 0
	}
	rest := rawURL[i+len(marker):]
	j := strings.IndexByte(rest, '/')
	if j < 0 {
		return accountIndex == 0
	}
	n, err := strconv.Atoi(rest[:j])
	if err != nil {
		return accountIndex == 0
	}
	return n == accountIndex
}

// DiscoverProjects returns the project IDs visible on the currently attached tab.
//
// The Flow app landing page lists the account's projects, so this is how the
// engine learns which project to generate into without the user pasting an ID.
//
// Links are filtered by account. The list is read out of the DOM, and until a
// navigation completes the previous page's links are still there — so without
// this filter a switch reads the account it just left and reports its project.
func (b *Bridge) DiscoverProjects(ctx context.Context, accountIndex int) ([]string, error) {
	client := b.Current()
	if client == nil || !client.Connected() {
		return nil, fmt.Errorf("bridge: no extension connected")
	}

	raw, err := client.FlowProjects(ctx, projectLinkExpression)
	if err != nil {
		return nil, fmt.Errorf("bridge: could not read the project list: %w", err)
	}

	var links []string
	if err := json.Unmarshal(raw, &links); err != nil {
		return nil, fmt.Errorf("bridge: the project list had an unexpected shape: %w", err)
	}

	seen := make(map[string]bool)
	var ids []string
	for _, link := range links {
		if !urlMatchesAccount(link, accountIndex) {
			continue
		}
		id := projectIDFromFlowURL(link)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

// EnsureProjectTab makes sure a Flow project editor tab is open and attached.
//
// This is not a convenience. The reCAPTCHA widget the generation call needs only
// loads inside the editor; on the project list it is absent entirely, so a broker
// evaluating there returns an empty token. Attaching to a project page is what
// makes the broker path work.
// EnsureProjectTab returns a tab sitting on a project editor for the given
// signed-in account, opening one if necessary.
//
// accountIndex selects which of the browser's signed-in accounts the project has
// to belong to. Projects are per-account, so this cannot be inferred from the
// cookies — the accounts share one jar.
func (b *Bridge) EnsureProjectTab(ctx context.Context, accountIndex int) (*cdp.Tab, error) {
	client := b.Current()
	if client == nil || !client.Connected() {
		return nil, fmt.Errorf("bridge: no extension connected")
	}

	tabs, err := client.ListTabs(ctx)
	if err != nil {
		return nil, fmt.Errorf("bridge: could not list tabs: %w", err)
	}

	// Already on a project editor for this account: use it.
	//
	// The account matters, because a project belongs to one. A URL remembered
	// from a different account does not open — it 404s — and the engine would
	// then hold a project id its session cannot use.
	for _, tab := range tabs {
		if strings.Contains(tab.URL, projectPathMarker) && urlMatchesAccount(tab.URL, accountIndex) {
			attached, attachErr := client.Attach(ctx, tab.ID)
			if attachErr != nil {
				return nil, attachErr
			}
			return &attached, nil
		}
	}

	// Go to this account's own Flow home before reading the project list, so the
	// ids discovered belong to the account the engine is acting as.
	//
	// This runs for account 0 too, not just the others: the shortcut above only
	// accepts a tab already on *this* account's project, so reaching here means
	// the page is showing something else, and its links are not an answer.
	//
	// Poll rather than pause: the list is rendered by the SPA, and a fixed wait
	// that is long enough on an idle machine is too short on a busy one.
	{
		home := fmt.Sprintf("https://flow.google.com/u/%d/", accountIndex)

		// Navigate the attached tab rather than opening another one. This runs on
		// every account switch and every project change, so opening would leave a
		// Flow tab behind each time until the browser hangs.
		//
		// Driven through location.href instead of a tabs API: that acts on
		// whatever tab is already attached, so it needs no new extension surface
		// and cannot accumulate anything.
		if _, err := client.Attach(ctx, 0); err != nil {
			return nil, fmt.Errorf("bridge: could not attach to a tab: %w", err)
		}
		if err := client.FlowNavigate(ctx, home, navigateExpression(home)); err != nil {
			return nil, fmt.Errorf("bridge: could not open %s: %w", home, err)
		}

		// Wait for the navigation to land before reading anything. Until it does,
		// the previous page is still in the DOM and its project links look like an
		// answer — which is exactly how a switch reported the account it left.
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			cur, err := client.CurrentTab(ctx)
			if err == nil && cur != nil && urlMatchesAccount(cur.URL, accountIndex) {
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}

		// Then wait for the list itself to render.
		for time.Now().Before(deadline) {
			if ids, err := b.DiscoverProjects(ctx, accountIndex); err == nil && len(ids) > 0 {
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}

	// Otherwise find a project ID from the list, then open its editor.
	ids, err := b.DiscoverProjects(ctx, accountIndex)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf(
			"bridge: no Flow project found. Open a project in the browser once, or set " +
				"FLOW_PROJECT_ID explicitly")
	}

	// The account is in the path, because the same project id under a different
	// account is a 404 — which is exactly what an unqualified URL produced.
	target := fmt.Sprintf("https://flow.google.com/u/%d%s%s", accountIndex, projectPathMarker, ids[0])
	log.Printf("bridge: opening the Flow editor for project %s as account %d", ids[0], accountIndex)

	// Same reasoning as the account home above: reuse the tab, do not add one.
	if err := client.FlowNavigate(ctx, target, navigateExpression(target)); err != nil {
		return nil, fmt.Errorf("bridge: could not open %s: %w", target, err)
	}

	// The editor is a heavy SPA; give it a moment to boot before anything tries
	// to run script in it.
	select {
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Prefer the live tab once it has actually landed, so the caller gets the
	// settled URL. Otherwise report the target: re-querying can still show the
	// previous page while the SPA boots, and the caller reads the project id off
	// this URL, so the known target is the trustworthy answer.
	if current, err := client.CurrentTab(ctx); err == nil && current != nil &&
		strings.Contains(current.URL, projectPathMarker) && urlMatchesAccount(current.URL, accountIndex) {
		return current, nil
	}
	if current, err := client.CurrentTab(ctx); err == nil && current != nil {
		return &cdp.Tab{ID: current.ID, URL: target, Title: current.Title}, nil
	}
	return &cdp.Tab{URL: target}, nil
}

// navigateExpression points the current page at a URL.
//
// A page-level navigation rather than a tabs API, deliberately: it acts on
// whatever tab is already attached, so a caller that navigates repeatedly cannot
// accumulate tabs — and there is no new extension surface to keep in step.
func navigateExpression(rawURL string) string {
	return fmt.Sprintf("(() => { location.href = %s; return 'nav'; })()", strconv.Quote(rawURL))
}

// Fingerprint is the browser's own request identity.
//
// It matters because the reCAPTCHA assessment is tied to the client that
// produced it. If the generation request then goes out under a different
// user-agent or sec-ch-ua, the assessment does not line up with the request and
// the upstream answers 403 PUBLIC_ERROR_UNUSUAL_ACTIVITY — a rejection that looks
// like a captcha failure but is really a fingerprint mismatch.
type Fingerprint struct {
	UserAgent    string `json:"userAgent"`
	Language     string `json:"language"`
	Brands       string `json:"brands"`
	Platform     string `json:"platform"`
	Mobile       string `json:"mobile"`
	PlatformFull string `json:"platformFull"`
}

// fingerprintExpression reads the identity the browser would send.
const fingerprintExpression = `(() => {
  const d = navigator.userAgentData || {};
  const brands = (d.brands || []).map(b => '"' + b.brand + '";v="' + b.version + '"').join(', ');
  return {
    userAgent: navigator.userAgent,
    language: navigator.language || 'en-US',
    brands,
    platform: d.platform || '',
    mobile: d.mobile ? '?1' : '?0',
    platformFull: '"' + (d.platform || 'macOS') + '"',
  };
})()`

// Fingerprint reads the browser's request identity from the attached tab.
func (b *Bridge) Fingerprint(ctx context.Context) (*Fingerprint, error) {
	client := b.Current()
	if client == nil || !client.Connected() {
		return nil, fmt.Errorf("bridge: no extension connected")
	}

	raw, err := client.FlowFingerprint(ctx, fingerprintExpression)
	if err != nil {
		return nil, fmt.Errorf("bridge: could not read the browser fingerprint: %w", err)
	}

	var fp Fingerprint
	if err := json.Unmarshal(raw, &fp); err != nil {
		return nil, fmt.Errorf("bridge: the fingerprint had an unexpected shape: %w", err)
	}
	if fp.UserAgent == "" {
		return nil, fmt.Errorf("bridge: the browser reported an empty user agent")
	}
	return &fp, nil
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

	// FlowOperations reports whether the attached extension offers the backend's
	// own narrow operations rather than only the generic evaluate fallback, and
	// ExtensionOps lists what it advertised. Both are empty until the extension
	// has been asked, which happens on connect.
	FlowOperations bool     `json:"flow_operations"`
	ExtensionOps   []string `json:"extension_ops,omitempty"`
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
		surface := client.Surface()
		s.FlowOperations = surface.FlowOperations
		s.ExtensionOps = surface.Advertised
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

// projectIDFromFlowURL pulls a project id out of a Flow URL.
//
// It moved here from the application's auth package: the bridge needs it to read
// the project off a page it is attached to, and reaching back into the application
// for one string-splitting helper is what kept this package from standing alone.
func projectIDFromFlowURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, part := range parts {
		if part == "project" && i+1 < len(parts) {
			return sanitizeID(parts[i+1])
		}
	}
	if len(parts) > 0 && strings.Contains(u.Path, "/flow/") {
		return sanitizeID(parts[len(parts)-1])
	}
	return ""
}

func sanitizeID(value string) string {
	var out strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out.WriteRune(r)
		}
	}
	return out.String()
}
