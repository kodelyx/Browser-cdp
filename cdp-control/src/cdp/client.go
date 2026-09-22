// Package cdp speaks the browser-Cdp Chrome extension protocol.
//
// The extension dials out to the backend, so the normal mode is server-side:
// Listen accepts an extension connection and hands back a *Client that can issue
// operations and receive events. Dial is provided for the opposite arrangement
// (a backend that connects to something else) and for tests.
//
// It is deliberately independent of any product: any project can import this
// package and drive a signed-in Chrome tab without launching Chrome with a
// debugging port.
package cdp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	stdhttp "net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// ErrNotConnected is returned when the extension socket is not established.
var ErrNotConnected = errors.New("cdp: extension not connected")

// Event is an unsolicited frame pushed by the extension.
type Event struct {
	Name     string          `json:"event"`
	Params   json.RawMessage `json:"params,omitempty"`
	Received time.Time       `json:"-"`
}

// Cookie mirrors the shape chrome.cookies returns, which is also what the
// extension forwards verbatim.
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

// Tab is the subset of chrome.tabs.Tab the bridge exposes.
type Tab struct {
	ID       int    `json:"id"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	Active   bool   `json:"active"`
	WindowID int    `json:"windowId"`
}

// Config mirrors ../browser-Cdp/config.js so a backend can push its own allowlists
// at startup instead of asking the user to retype them.
//
// TargetURLPrefixes and CookieDomains deliberately carry no `omitempty`. The
// extension treats an empty list as full access, but it *merges* a pushed patch
// over what it has stored, so an omitted key leaves a previously persisted
// narrowing in place. A universal backend that says nothing would silently keep
// whatever an earlier scoped backend wrote. Sending `[]` explicitly is what makes
// "no scope" mean no scope.
type Config struct {
	BridgeURL         string   `json:"bridgeUrl,omitempty"`
	TargetURLPrefixes []string `json:"targetUrlPrefixes"`
	CookieDomains     []string `json:"cookieDomains"`
	EnableDomains     []string `json:"enableDomains,omitempty"`
	BlockedMethods    []string `json:"blockedMethods,omitempty"`
	// BridgeToken is the shared secret the extension must present on the
	// WebSocket upgrade. Delivered over an already-established connection, so
	// the extension can store it and use it from then on.
	BridgeToken       string   `json:"bridgeToken,omitempty"`
	AutoOpenOnStart   *bool    `json:"autoOpenOnStart,omitempty"`
	AutoOpenOnCommand *bool    `json:"autoOpenOnCommand,omitempty"`
	FocusOnAutoOpen   *bool    `json:"focusOnAutoOpen,omitempty"`
	EventBufferSize   *int     `json:"eventBufferSize,omitempty"`
	ReconnectDelayMS  *int     `json:"reconnectDelayMs,omitempty"`
	KeepAliveMinutes  *float64 `json:"keepAliveMinutes,omitempty"`
}

type wireRequest struct {
	ID     string `json:"id"`
	Op     string `json:"op"`
	Params any    `json:"params,omitempty"`
}

type wireResponse struct {
	ID     string          `json:"id,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *wireError      `json:"error,omitempty"`
	Event  string          `json:"event,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

type wireError struct {
	Message string `json:"message"`
}

type pendingCall struct {
	ch chan wireResponse
}

// Client is one extension connection.
type Client struct {
	conn *websocket.Conn

	mu      sync.Mutex
	pending map[string]*pendingCall
	closed  bool

	subMu   sync.RWMutex
	subs    map[int]chan Event
	nextSub int64

	writeMu sync.Mutex

	lastErr atomic.Value // string

	// Version is the extension version reported in bridge.ready / ping.
	Version atomic.Value // string

	// RemoteAddr is the peer address, for logging.
	RemoteAddr string

	// opsProbed records whether this extension has been asked what it
	// implements. The answer is cached in advertised, so reporting it costs one
	// round trip per connection and nothing after that.
	opsProbed atomic.Bool

	// opsMu guards advertised, the operation list the extension returned from
	// ping. It is reported rather than inferred: which operations an extension
	// offers is the first thing worth knowing when one behaves differently from
	// the one before it.
	opsMu      sync.Mutex
	advertised []string
}

/* ------------------------------------------------------------------ *
 * Construction
 * ------------------------------------------------------------------ */

// New wraps an established WebSocket connection and starts its read loop.
func New(conn *websocket.Conn) *Client {
	c := &Client{
		conn:    conn,
		pending: make(map[string]*pendingCall),
		subs:    make(map[int]chan Event),
	}
	c.lastErr.Store("")
	c.Version.Store("")
	if conn != nil {
		c.RemoteAddr = conn.RemoteAddr().String()
		go c.readLoop(conn)
	}
	return c
}

// Dial connects to a WebSocket endpoint and wraps it. Used for tests and for the
// reverse arrangement where the backend is the client.
func Dial(ctx context.Context, rawURL string) (*Client, error) {
	if rawURL == "" {
		rawURL = "ws://127.0.0.1:9222"
	}
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second

	conn, resp, err := dialer.DialContext(ctx, rawURL, nil)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("cdp: dial %s: %w (http %d)", rawURL, err, resp.StatusCode)
		}
		return nil, fmt.Errorf("cdp: dial %s: %w", rawURL, err)
	}
	return New(conn), nil
}

// ListenOptions configures the WebSocket host.
type ListenOptions struct {
	// Addr is the listen address, e.g. "127.0.0.1:9222".
	Addr string
	// Token, when non-empty, is required on every upgrade. It is accepted from
	// a `?token=` query parameter or an `Authorization: Bearer` header. The
	// query parameter is the practical path: a browser cannot set arbitrary
	// headers on a WebSocket handshake.
	Token string
	// AllowTokenless reports whether a connection presenting no token should be
	// accepted. It is a function rather than a bool so the window can close while
	// the server is running: pairing flips it as soon as an extension proves it
	// holds the token.
	AllowTokenless func() bool
	// OnConnect is called for every accepted connection.
	OnConnect func(*Client)
	// OnAuthenticated is called after a connection proves it holds the token,
	// as opposed to being let in by AllowTokenless.
	OnAuthenticated func(*Client)
	// RepairHint is the concrete instruction logged when an extension is turned
	// away for presenting no token at all. This package cannot know where the
	// host keeps its token or its pairing marker, so the caller supplies the
	// wording. Without it the log says only that the upgrade was rejected, which
	// leaves the reader with a repeated line and no next step.
	RepairHint string
}

// Listen hosts a WebSocket server and calls OnConnect for every extension that
// dials in. It blocks until ctx is cancelled.
//
// Two checks guard the upgrade. Origin must be a Chrome extension or loopback,
// so a web page cannot reach this socket. And, when a token is configured, the
// caller must present it — without that, any local process could dial
// 127.0.0.1 and drive the user's browser through the extension, because a
// non-browser client simply omits the Origin header and would otherwise pass the
// origin check.
func Listen(ctx context.Context, opts ListenOptions) error {
	upgrader := websocket.Upgrader{
		HandshakeTimeout: 10 * time.Second,
		CheckOrigin:      isExtensionOrigin,
	}

	addr := opts.Addr
	if addr == "" {
		addr = "127.0.0.1:9222"
	}

	mux := stdhttp.NewServeMux()

	// A rejected extension retries on a timer — every few seconds, forever. Each
	// attempt is a fresh connection from a fresh port, so the log fills with the
	// same line and buries the one thing worth reading. Explain each distinct
	// refusal once, and reset after a connection succeeds so a recurrence is
	// reported again rather than swallowed.
	var (
		rejectMu    sync.Mutex
		explainedAt = map[RejectionReason]bool{}
	)

	mux.HandleFunc("/", func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if !isExtensionOrigin(r) {
			stdhttp.Error(w, "origin not allowed", stdhttp.StatusForbidden)
			return
		}

		authorized, ok := opts.authorize(r)
		if !ok {
			reason := opts.rejectReason(r)

			rejectMu.Lock()
			explain := !explainedAt[reason]
			explainedAt[reason] = true
			rejectMu.Unlock()

			if explain {
				log.Printf("cdp: rejected an upgrade from %s — %s", r.RemoteAddr, reason.explain(opts.RepairHint))
			}
			stdhttp.Error(w, "unauthorized: missing or invalid bridge token", stdhttp.StatusUnauthorized)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("cdp: upgrade failed from %s: %v", r.RemoteAddr, err)
			return
		}
		client := New(conn)
		log.Printf("cdp: extension connected from %s (authenticated=%v)", r.RemoteAddr, authorized)

		rejectMu.Lock()
		explainedAt = map[RejectionReason]bool{}
		rejectMu.Unlock()

		if authorized && opts.OnAuthenticated != nil {
			opts.OnAuthenticated(client)
		}
		if opts.OnConnect != nil {
			opts.OnConnect(client)
		}
	})

	server := &stdhttp.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, stdhttp.ErrServerClosed) {
		return fmt.Errorf("cdp: listen on %s: %w", addr, err)
	}
	return nil
}

// authorize reports whether the upgrade may proceed, and whether the caller
// proved it holds the token rather than being let through by the pairing window.
//
// The first return value is "authorized", not "a token was supplied". Getting
// that distinction wrong is a silent bypass: a caller presenting a wrong token
// would be admitted.
func (o ListenOptions) authorize(r *stdhttp.Request) (authenticated bool, authorized bool) {
	if o.Token == "" {
		return false, true
	}

	supplied := presentedToken(r)
	if supplied != "" {
		if subtle.ConstantTimeCompare([]byte(supplied), []byte(o.Token)) == 1 {
			return true, true
		}
		return false, false
	}

	if o.AllowTokenless != nil && o.AllowTokenless() {
		return false, true
	}
	return false, false
}

// presentedToken returns the token the caller supplied, from the query parameter
// or the Authorization header. The query parameter is the practical path: a
// browser cannot set arbitrary headers on a WebSocket handshake.
func presentedToken(r *stdhttp.Request) string {
	if supplied := r.URL.Query().Get("token"); supplied != "" {
		return supplied
	}
	if header := r.Header.Get("Authorization"); strings.HasPrefix(header, "Bearer ") {
		return strings.TrimPrefix(header, "Bearer ")
	}
	return ""
}

// RejectionReason says why an upgrade was refused.
//
// The two cases call for different actions and used to be indistinguishable in
// the log, which is the only place the user ever sees them. Both are logged as
// "rejected unauthenticated upgrade", so a stale token and a missing one looked
// the same, and neither said what to do.
type RejectionReason int

const (
	// RejectNoToken: nothing was presented and the pairing window is closed. The
	// extension holds no token at all, which is what reinstalling it looks like —
	// that clears chrome.storage.local, so a paired extension can come back with
	// nothing.
	RejectNoToken RejectionReason = iota
	// RejectWrongToken: a token was presented and did not match. The extension is
	// holding a stale one, which happens when the host regenerates its token.
	RejectWrongToken
)

// rejectReason classifies a refusal. It is only meaningful after authorize has
// returned false.
func (o ListenOptions) rejectReason(r *stdhttp.Request) RejectionReason {
	if presentedToken(r) != "" {
		return RejectWrongToken
	}
	return RejectNoToken
}

// String names the reason, so a failing assertion says which one it was.
func (reason RejectionReason) String() string {
	if reason == RejectWrongToken {
		return "RejectWrongToken"
	}
	return "RejectNoToken"
}

// explain turns a refusal into the next step.
func (reason RejectionReason) explain(repairHint string) string {
	switch reason {
	case RejectWrongToken:
		return "a token was presented and did not match, so the extension is holding a stale one. " +
			"Give it the current token, or clear the pairing marker and restart to re-pair."
	default:
		if repairHint != "" {
			return "no token was presented and pairing is closed, so the extension has none stored. " + repairHint
		}
		return "no token was presented and pairing is closed, so the extension has none stored. " +
			"Clear the pairing marker and restart to re-pair."
	}
}

// isExtensionOrigin accepts chrome-extension:// origins and non-browser clients
// that send no Origin header at all, which is what an MV3 service worker looks
// like in some Chrome versions. It is a weak check on its own — the token is what
// actually keeps other local processes out.
func isExtensionOrigin(r *stdhttp.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if strings.HasPrefix(origin, "chrome-extension://") {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "127.0.0.1" || host == "localhost"
}

/* ------------------------------------------------------------------ *
 * Lifecycle
 * ------------------------------------------------------------------ */

// Close tears down the socket and fails every in-flight call.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	conn := c.conn
	c.conn = nil
	inFlight := c.pending
	c.pending = make(map[string]*pendingCall)
	c.mu.Unlock()

	for _, p := range inFlight {
		close(p.ch)
	}
	if conn != nil {
		return conn.Close()
	}
	return nil
}

// Connected reports whether a live socket exists.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil && !c.closed
}

// LastError returns the most recent transport-level error, if any.
func (c *Client) LastError() string {
	if v, ok := c.lastErr.Load().(string); ok {
		return v
	}
	return ""
}

// Subscribe returns a channel of unsolicited events. The returned cancel
// function removes the subscription and closes the channel.
func (c *Client) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 64
	}
	ch := make(chan Event, buffer)
	c.subMu.Lock()
	id := int(atomic.AddInt64(&c.nextSub, 1))
	c.subs[id] = ch
	c.subMu.Unlock()

	cancel := func() {
		c.subMu.Lock()
		if existing, ok := c.subs[id]; ok {
			delete(c.subs, id)
			close(existing)
		}
		c.subMu.Unlock()
	}
	return ch, cancel
}

func (c *Client) readLoop(conn *websocket.Conn) {
	for {
		var frame wireResponse
		if err := conn.ReadJSON(&frame); err != nil {
			c.lastErr.Store(err.Error())
			c.failAll(err)
			return
		}

		if frame.Event != "" {
			c.dispatch(Event{Name: frame.Event, Params: frame.Params, Received: time.Now()})
			continue
		}
		if frame.ID == "" {
			continue
		}

		c.mu.Lock()
		p, ok := c.pending[frame.ID]
		if ok {
			delete(c.pending, frame.ID)
		}
		c.mu.Unlock()
		if ok {
			p.ch <- frame
			close(p.ch)
		}
	}
}

func (c *Client) dispatch(ev Event) {
	if ev.Name == "bridge.ready" {
		var payload struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(ev.Params, &payload); err == nil && payload.Version != "" {
			c.Version.Store(payload.Version)
		}
	}

	c.subMu.RLock()
	defer c.subMu.RUnlock()
	for _, ch := range c.subs {
		select {
		case ch <- ev:
		default:
			// Slow subscriber: drop rather than block the read loop.
		}
	}
}

func (c *Client) failAll(cause error) {
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	inFlight := c.pending
	c.pending = make(map[string]*pendingCall)
	c.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
	for _, p := range inFlight {
		close(p.ch)
	}
	if cause != nil && !errors.Is(cause, websocket.ErrCloseSent) {
		log.Printf("cdp: socket closed: %v", cause)
	}
}

/* ------------------------------------------------------------------ *
 * Operations
 * ------------------------------------------------------------------ */

// Call sends one operation and decodes the result into out (if non-nil).
func (c *Client) Call(ctx context.Context, op string, params any, out any) error {
	c.mu.Lock()
	conn := c.conn
	if conn == nil || c.closed {
		c.mu.Unlock()
		return ErrNotConnected
	}
	id := uuid.NewString()
	p := &pendingCall{ch: make(chan wireResponse, 1)}
	c.pending[id] = p
	c.mu.Unlock()

	req := wireRequest{ID: id, Op: op, Params: params}

	c.writeMu.Lock()
	err := conn.WriteJSON(req)
	c.writeMu.Unlock()

	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("cdp: write %s: %w", op, err)
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case frame, ok := <-p.ch:
		if !ok {
			return ErrNotConnected
		}
		if frame.Error != nil {
			return fmt.Errorf("cdp: %s: %s", op, frame.Error.Message)
		}
		if out != nil && len(frame.Result) > 0 {
			if err := json.Unmarshal(frame.Result, out); err != nil {
				return fmt.Errorf("cdp: decode %s result: %w", op, err)
			}
		}
		return nil
	}
}

/* ------------------------------------------------------------------ *
 * Typed convenience wrappers
 * ------------------------------------------------------------------ */

// Ping checks liveness and returns the extension version.
func (c *Client) Ping(ctx context.Context) (string, error) {
	var res struct {
		Version       string `json:"version"`
		AttachedTabID *int   `json:"attachedTabId"`
	}
	if err := c.Call(ctx, "ping", nil, &res); err != nil {
		return "", err
	}
	if res.Version != "" {
		c.Version.Store(res.Version)
	}
	return res.Version, nil
}

// GetConfig reads the extension's current configuration.
func (c *Client) GetConfig(ctx context.Context) (Config, error) {
	var cfg Config
	err := c.Call(ctx, "config.get", nil, &cfg)
	return cfg, err
}

// SetConfig pushes a partial configuration. This is how a backend declares its
// own allowlists so the user does not have to type them anywhere.
func (c *Client) SetConfig(ctx context.Context, patch Config) (Config, error) {
	var cfg Config
	err := c.Call(ctx, "config.set", map[string]any{"patch": patch}, &cfg)
	return cfg, err
}

// ListTabs returns tabs inside the configured URL allowlist.
func (c *Client) ListTabs(ctx context.Context) ([]Tab, error) {
	var tabs []Tab
	err := c.Call(ctx, "tabs.list", nil, &tabs)
	return tabs, err
}

// OpenTab opens a URL, which must be inside the configured allowlist.
func (c *Client) OpenTab(ctx context.Context, rawURL string, active bool) (Tab, error) {
	var tab Tab
	err := c.Call(ctx, "tabs.open", map[string]any{"url": rawURL, "active": active}, &tab)
	return tab, err
}

// Attach attaches to a specific tab, or to the best allowed tab when tabID is 0.
func (c *Client) Attach(ctx context.Context, tabID int) (Tab, error) {
	var tab Tab
	params := map[string]any{}
	if tabID != 0 {
		params["tabId"] = tabID
	}
	err := c.Call(ctx, "tab.attach", params, &tab)
	return tab, err
}

// Detach detaches from the current tab.
func (c *Client) Detach(ctx context.Context) error {
	return c.Call(ctx, "tab.detach", nil, nil)
}

// CurrentTab reports the currently attached tab, or nil when nothing is attached.
func (c *Client) CurrentTab(ctx context.Context) (*Tab, error) {
	var tab *Tab
	if err := c.Call(ctx, "tab.current", nil, &tab); err != nil {
		return nil, err
	}
	return tab, nil
}

// BufferedEvent is one entry from the extension's CDP event buffer.
//
// Note the shape: the buffer stores the raw CDP event, so the method name is a
// top-level field. Pushed `cdp.event` frames are different — there the method
// sits inside params under an `event` wrapper. Decoding a buffered event as if it
// were a pushed frame silently yields an empty method for every entry.
type BufferedEvent struct {
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params,omitempty"`
	Timestamp int64           `json:"timestamp,omitempty"`
}

// ReadEvents drains up to limit buffered CDP events from the extension.
func (c *Client) ReadEvents(ctx context.Context, limit int) ([]BufferedEvent, error) {
	var events []BufferedEvent
	err := c.Call(ctx, "events.read", map[string]any{"limit": limit}, &events)
	return events, err
}

// ListCookies returns cookies inside the configured cookie scope.
func (c *Client) ListCookies(ctx context.Context, details map[string]any) ([]Cookie, error) {
	var cookies []Cookie
	err := c.Call(ctx, "cookies.list", map[string]any{"details": details}, &cookies)
	return cookies, err
}

// CookieHeader renders the scoped cookies for one URL as a request Cookie header.
// This is the single call a backend needs to bootstrap from the browser.
func (c *Client) CookieHeader(ctx context.Context, rawURL string) (string, []Cookie, error) {
	cookies, err := c.ListCookies(ctx, map[string]any{"url": rawURL})
	if err != nil {
		return "", nil, err
	}
	return CookiesToHeader(cookies), cookies, nil
}

// CookiesToHeader joins cookies into a Cookie header value, skipping expired ones.
func CookiesToHeader(cookies []Cookie) string {
	now := float64(time.Now().Unix())
	out := ""
	for _, ck := range cookies {
		if ck.ExpirationDate > 0 && ck.ExpirationDate < now {
			continue
		}
		if ck.Name == "" {
			continue
		}
		if out != "" {
			out += "; "
		}
		out += ck.Name + "=" + ck.Value
	}
	return out
}

/* ------------------------------------------------------------------ *
 * Capability
 * ------------------------------------------------------------------ */

// Ops reports the operations the attached extension advertises, asking once and
// caching the answer.
//
// This is the whole of what this backend knows about the far side. It names no
// operation of its own and requires none: an extension answers `ping` with
// whatever it implements, and every call site uses the generic CDP surface in
// cdp.go. Reporting the list is what makes a deployment that behaves differently
// from the one before it diagnosable in one request, instead of by watching how
// some call happened to fail.
//
// A failed ping is cached as "nothing advertised" rather than retried on every
// caller, because the callers are status endpoints and the connection is
// re-established rather than repaired.
func (c *Client) Ops() []string {
	if !c.opsProbed.Load() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var out struct {
			Ops []string `json:"ops"`
		}
		// The error is deliberately dropped: an extension that does not answer
		// ping, or does not carry a list, advertises nothing, and that is the
		// same answer either way.
		_ = c.Call(ctx, "ping", nil, &out)

		c.opsMu.Lock()
		c.advertised = append([]string(nil), out.Ops...)
		c.opsMu.Unlock()
		c.opsProbed.Store(true)
	}

	c.opsMu.Lock()
	defer c.opsMu.Unlock()
	return append([]string(nil), c.advertised...)
}
