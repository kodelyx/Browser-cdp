# browser-Cdp

A generic, project-agnostic Chrome DevTools Protocol bridge.

**browser-Cdp** is a lightweight, general-purpose Chrome DevTools Protocol (CDP) bridge extension. It provides core capabilities to **attach to a tab, send CDP commands, stream CDP events, and read cookies — with optional configurable allowlists.**

It is a drop-in alternative to launching Chrome with
`--remote-debugging-port=9222`, with two advantages: the browser stays your normal
signed-in browser (no separate profile, no re-login), and scoping is available
when you want it — set `targetUrlPrefixes` / `cookieDomains` / `blockedMethods`
via `config.set`. By default (empty allowlists) the backend gets full raw-CDP
access to every tab, just like the debugging port.

## What it is not

- Not a scraping framework, not a site automation library.
- It has no knowledge of any particular website.
- It does not read browser passwords, profile data, or cookies outside the
  configured domain allowlist.

## Wire protocol

One JSON object per WebSocket frame. The extension dials out to your backend —
you host the server, it is the client. After it connects and sends
`bridge.ready`, you send operations and it replies:

```
you -> extension : {"id": "...", "op": "cdp.call", "params": {...}}
extension -> you : {"id": "...", "result": {...}}
extension -> you : {"id": "...", "error": {"message": "..."}}
extension -> you : {"event": "cdp.event", "params": {...}}
```

### Operations

| op | params | result |
| --- | --- | --- |
| `ping` | — | `{version, attachedTabId, config}` |
| `config.get` | — | full config |
| `config.set` | `{patch}` | updated config |
| `config.reset` | — | defaults |
| `tabs.list` | — | allowed tabs only |
| `tabs.open` | `{url, active}` | `{tabId, url, title}` |
| `tab.attach` | `{tabId?}` | `{tabId, url, title}` |
| `tab.detach` | — | `{detached}` |
| `tab.current` | — | `{tabId, url, title}` or `null` |
| `cdp.call` | `{method, params, tabId?}` | raw CDP result |
| `cdp.evaluate` | `{expression, returnByValue?, awaitPromise?, userGesture?, tabId?}` | evaluated value |
| `events.read` | `{limit}` | drained event buffer |
| `events.clear` | — | `{cleared}` |
| `cookies.list` | `{details}` | scoped cookies |
| `cookies.get` | `{url, name, storeId?}` | one cookie |
| `cookies.set` | `{details}` | written cookie |
| `cookies.remove` | `{url, name, storeId?}` | removed cookie |

### Events

| event | params |
| --- | --- |
| `bridge.ready` | `{version, config}` |
| `cdp.attached` | `{tabId, url, title}` |
| `cdp.detached` | `{reason}` |
| `cdp.event` | `{method, params, timestamp}` |
| `cdp.domainError` | `{domain, message}` |
| `tab.navigated` | `{tabId, url}` |

## Copy AI prompt

The popup is intentionally three things: a status line, a **Reconnect** button
that forces the socket to redial, and a **Copy AI prompt** button. Nothing else.

That button builds a single self-contained markdown brief from the extension's
*live* state and configuration, and copies it to the clipboard. Paste it into any
AI assistant and that assistant has everything it needs to connect and drive the
bridge:

- the WebSocket endpoint currently in use
- the full operation and event tables
- the allowlists actually in force (`targetUrlPrefixes`, `cookieDomains`,
  `enableDomains`, `blockedMethods`)
- the current attach state
- worked client examples in Python, Node.js, and Go
- the operating rules and the failure modes worth knowing about

Because it is generated from live state rather than hardcoded, it stays accurate
when the config changes.

The generator lives in `ai-prompt.js` and is a plain function, so the same brief
can be produced from anywhere:

```js
import { buildConnectionPrompt } from './ai-prompt.js';
const prompt = buildConnectionPrompt(state, config);
```

## Configuration

Config lives in `chrome.storage.local`. It is normally written by the backend at
startup via the `config.set` bridge operation, so the extension ships inert and
the project that drives it declares its own scope. There is no settings UI in the
popup by design — one fewer place for the two to drift apart.

| key | meaning | default |
| --- | --- | --- |
| `bridgeUrl` | backend WebSocket server the extension dials out to | `ws://127.0.0.1:9222` |
| `bridgeToken` | shared secret required on the upgrade; normally delivered by the backend during pairing | `''` (no token) |
| `targetUrlPrefixes` | tabs the bridge may attach to / list. Empty = full access. The **first entry** is the URL auto-open uses. | `[]` (full access) |
| `cookieDomains` | cookie scope. Empty = full access. | `[]` (full access) |
| `enableDomains` | CDP domains enabled on attach | `Runtime, Page, DOM, Network` |
| `blockedMethods` | CDP methods always refused | `[]` (nothing blocked) |
| `autoOpenOnStart` | open `targetUrlPrefixes[0]` at Chrome start when no matching tab is open | `false` |
| `autoOpenOnCommand` | open `targetUrlPrefixes[0]` when a command needs a tab and none is open | `true` |
| `focusOnAutoOpen` | bring the auto-opened tab's window to the front | `true` |
| `eventBufferSize` | buffered CDP events | `2000` |
| `reconnectDelayMs` | socket reconnect backoff | `1500` |
| `keepAliveMinutes` | MV3 service-worker keepalive | `0.5` |

Both allowlists default to empty, which means full browser access — every tab
is attachable and every cookie is readable/writable, like a raw
`--remote-debugging-port`. Push narrower values via `config.set` when you want
scoping instead of asking the user to type them anywhere.

### Pairing (`bridgeToken`)

The origin check is not a security boundary on its own: a non-browser local
process omits the `Origin` header, so it passes. `bridgeToken` is what actually
keeps other programs on the machine out of the bridge.

The backend generates a token on first run and requires it on the upgrade. Since
the extension cannot know the token before it can connect, pairing is
trust-on-first-use:

1. The backend generates the token and accepts one tokenless connection.
2. It delivers the token via `config.set`; the extension stores it and
   reconnects with `?token=…`.
3. That authenticated connection closes the window permanently. Every later
   upgrade with a missing or wrong token gets `401`.

The backend logs a warning while the window is still open, so an unpaired bridge
is never silent. To re-pair after clearing extension storage, delete the
`bridge-token.claimed` marker beside the token file and restart the backend.

The token is accepted from a `?token=` query parameter or an
`Authorization: Bearer` header. The query parameter is the practical path, since
a browser cannot set arbitrary headers on a WebSocket handshake.

### Auto-opening a tab

`autoOpenOnCommand` is on by default. It means a backend can start before the
browser does: the first command that needs a tab opens `targetUrlPrefixes[0]`,
waits for it to load, and attaches. That is what makes an unattended cold start
work.

`autoOpenOnStart` is **off** by default, because silently opening a tab every
time Chrome launches is intrusive for a generic tool. Turn it on if you want the
target always ready. When the bridge opens a tab it emits `tab.autoOpened`; if it
fails it emits `tab.autoOpenFailed` with the reason.

## Install

1. Open `chrome://extensions` and enable **Developer mode**.
2. **Load unpacked** → select this `browser-Cdp/` directory.
3. Start your backend WebSocket server on `ws://127.0.0.1:9222` (or push a
   different `bridgeUrl` via `config.set`). The extension dials out to it.
4. Sign in to the site you are automating in a normal tab.
5. If the extension shows "Waiting for backend", open the popup and press
   **Reconnect**.

## Safety properties

CDP access is unfiltered once a caller is through the door, and that is
deliberate — this is a generic bridge, not a policy engine. Two things stand
between your browser and another program on the machine:

- **The bridge token.** Without the correct `?token=`, the upgrade is refused
  with `401`. See [Pairing](#pairing-bridgetoken) above. This is the real
  boundary; leave `bridgeToken` unset and you have none.
- **The loopback bind.** Host the server on `127.0.0.1`, never `0.0.0.0`, so the
  exposure is limited to local processes.

Scope, when you configure it, is enforced in code:

- **Cookie scope is enforced when `cookieDomains` is non-empty.** A cookie outside
  it is filtered out of reads and rejected on writes, even though the manifest
  requests broad host permissions.
- **Raw CDP cookie methods can be denied** via `blockedMethods`. This matters:
  without a deny-list, a caller can attach to an allowed tab and then issue
  `Network.getAllCookies`, which returns every cookie in the profile. The tab
  allowlist limits which tab you attach to, not which method you then run. Backends
  can push a deny-list for exactly this reason.
- **Tab allowlist is enforced on every attach** when `targetUrlPrefixes` is
  non-empty, including attaches triggered indirectly by `cdp.call` /
  `cdp.evaluate`.
- **`tabs.open` refuses URLs outside the allowlist** when one is configured, so
  the backend cannot use the extension to navigate the user somewhere
  unexpected.
- **No remote code.** The extension is plain MV3 JavaScript, no WASM, no eval of
  backend-supplied code — `cdp.evaluate` is the one deliberate exception, and it
  runs inside the attached tab.

## Limitations

- `chrome.debugger` shows a banner on the attached tab and detaches if the user
  opens DevTools on the same tab. This is a Chrome constraint, not a bug here.
- Only one tab is attached at a time, matching `chrome.debugger`'s per-tab model.
- MV3 service workers are evicted when idle; the keepalive alarm reconnects, so a
  short socket drop after long idle periods is expected.
