/**
 * browser-Cdp - AI connection prompt builder.
 *
 * Produces a single self-contained markdown prompt describing how to talk to
 * this extension. The point is that a user can copy it and paste it into ANY AI
 * assistant (or hand it to a developer) and that assistant has everything it
 * needs to connect: the endpoint, the wire protocol, the live allowlists, worked
 * examples in several languages, and the operating rules.
 *
 * It is generated from live state rather than hardcoded, so the copied prompt
 * always reflects the configuration actually in effect.
 */

function fence(lang, body) {
  return '```' + lang + '\n' + body.trim() + '\n```';
}

function bulletList(items, fallback = '- (none configured)') {
  if (!items || items.length === 0) return fallback;
  return items.map((item) => `- \`${item}\``).join('\n');
}

/**
 * Build the connection prompt.
 *
 * @param {object} state  live status from the background worker
 * @param {object} config live config
 * @returns {string} markdown prompt
 */
export function buildConnectionPrompt(state = {}, config = {}) {
  const bridgeUrl = config.bridgeUrl || 'ws://127.0.0.1:9222';
  const targets = config.targetUrlPrefixes || [];
  const domains = config.cookieDomains || [];
  const enableDomains = config.enableDomains || [];
  const blocked = config.blockedMethods || [];

  const attached = state.attachedTabId != null
    ? `tab \`${state.attachedTabId}\` — ${state.tabUrl || 'unknown URL'}`
    : 'nothing (not attached yet)';

  const lines = [];

  lines.push('# Connect to my browser-Cdp bridge');
  lines.push('');
  lines.push('I have the **browser-Cdp** Chrome extension loaded and running. It is a generic');
  lines.push('Chrome DevTools Protocol bridge: it lets a local program drive a real, already');
  lines.push('signed-in Chrome tab over a WebSocket — no `--remote-debugging-port`, no separate');
  lines.push('profile, no re-login.');
  lines.push('');
  lines.push('Read this whole brief before writing code. Everything you need is below; you do not');
  lines.push('need to ask me for the protocol.');
  lines.push('');

  lines.push('## 1. Endpoint');
  lines.push('');
  lines.push('```');
  lines.push(bridgeUrl);
  lines.push('```');
  lines.push('');
  lines.push('Host a WebSocket **server** on that address — the extension dials out to');
  lines.push('you (it is the client, you are the server). Once it connects it sends');
  lines.push('`bridge.ready`, then you send `{id, op, params}` frames and it replies with');
  lines.push('`{id, result}` / `{id, error}`. If no extension connects, Chrome is closed,');
  lines.push('the service worker is asleep, or your server is not listening — open the');
  lines.push('popup and press **Reconnect**.');
  lines.push('');

  lines.push('## 2. Live configuration');
  lines.push('');
  lines.push('These are the allowlists currently in force. They are enforced by the extension,');
  lines.push('not by convention, so anything outside them will be rejected.');
  lines.push('');
  lines.push('**Tabs the bridge may attach to** (`targetUrlPrefixes`):');
  lines.push(bulletList(targets, '- (empty — full access, every tab is attachable)'));
  lines.push('');
  lines.push('**Cookie scope** (`cookieDomains`):');
  lines.push(bulletList(domains, '- (empty — full access, all cookies readable/writable)'));
  lines.push('');
  lines.push('**CDP domains enabled on attach** (`enableDomains`):');
  lines.push(bulletList(enableDomains));
  lines.push('');
  lines.push('**Blocked CDP methods** (`blockedMethods`):');
  lines.push(bulletList(blocked, '- (empty — no methods blocked, raw CDP access)'));
  lines.push('');
  lines.push('**Auto-open**: ' + (
    config.autoOpenOnCommand === false
      ? 'disabled — `tab.attach` will fail rather than open a tab'
      : `enabled — a command that needs a tab will open \`${targets[0] || '(none configured yet)'}\``
  ));
  lines.push('');
  lines.push('**Current attach state**: ' + attached);
  lines.push('');

  lines.push('## 3. Wire protocol');
  lines.push('');
  lines.push('One JSON object per WebSocket text frame. The extension opens the');
  lines.push('socket to your server; after that the roles are:');
  lines.push('');
  lines.push(fence('text', `
you -> extension : {"id": "<uuid>", "op": "<operation>", "params": { ... }}
extension -> you : {"id": "<uuid>", "result": { ... }}
extension -> you : {"id": "<uuid>", "error": {"message": "..."}}
extension -> you : {"event": "<name>", "params": { ... }}     // unsolicited
`));
  lines.push('');
  lines.push('Correlate replies by `id`. Frames carrying `event` instead of `id` are pushed at');
  lines.push('you at any time and must be handled asynchronously.');
  lines.push('');

  lines.push('### Operations');
  lines.push('');
  lines.push('| `op` | `params` | `result` |');
  lines.push('| --- | --- | --- |');
  lines.push('| `ping` | — | `{version, attachedTabId, config}` |');
  lines.push('| `config.get` | — | full config |');
  lines.push('| `config.set` | `{patch}` | updated config |');
  lines.push('| `config.reset` | — | defaults |');
  lines.push('| `tabs.list` | — | allowed tabs only |');
  lines.push('| `tabs.open` | `{url, active}` | `{tabId, url, title}` |');
  lines.push('| `tabs.navigate` | `{url}` | `{tabId, url, title}` |');
  lines.push('| `tab.attach` | `{tabId?}` | `{tabId, url, title}` |');
  lines.push('| `tab.detach` | — | `{detached}` |');
  lines.push('| `tab.current` | — | `{tabId, url, title}` or `null` |');
  lines.push('| `cdp.call` | `{method, params, tabId?}` | raw CDP result |');
  lines.push('| `cdp.evaluate` | `{expression, returnByValue?, awaitPromise?, userGesture?, tabId?}` | evaluated value |');
  lines.push('| `events.read` | `{limit}` | drained event buffer |');
  lines.push('| `events.clear` | — | `{cleared}` |');
  lines.push('| `cookies.list` | `{details}` | scoped cookies |');
  lines.push('| `cookies.get` | `{url, name, storeId?}` | one cookie |');
  lines.push('| `cookies.set` | `{details}` | written cookie |');
  lines.push('| `cookies.remove` | `{url, name, storeId?}` | removed cookie |');
  lines.push('');

  lines.push('### Events');
  lines.push('');
  lines.push('| `event` | `params` |');
  lines.push('| --- | --- |');
  lines.push('| `bridge.ready` | `{version, config}` |');
  lines.push('| `cdp.attached` | `{tabId, url, title}` |');
  lines.push('| `cdp.detached` | `{reason}` |');
  lines.push('| `cdp.event` | `{method, params, timestamp}` |');
  lines.push('| `cdp.domainError` | `{domain, message}` |');
  lines.push('| `tab.navigated` | `{tabId, url}` |');
  lines.push('| `tab.autoOpened` | `{tabId, url}` |');
  lines.push('| `tab.autoOpenFailed` | `{message}` |');
  lines.push('');

  lines.push('## 4. Minimum working sequence');
  lines.push('');
  lines.push('Always do these in order. Most failures are a step being skipped.');
  lines.push('');
  lines.push('1. **Serve** the WebSocket — listen on the endpoint above and wait for');
  lines.push('   the extension to dial in (it sends `bridge.ready` first).');
  lines.push('2. **`ping`** — confirms the extension is alive and returns the live config.');
  lines.push('3. **`config.set`** — optional. Only needed to narrow scope (set');
  lines.push('   `targetUrlPrefixes` / `cookieDomains` / `blockedMethods`). Empty means full');
  lines.push('   access. The **first** `targetUrlPrefixes` entry is also the URL the');
  lines.push('   extension will open if it has to.');
  lines.push('4. **`tab.attach`** — with no `tabId` it picks the active tab (or the active');
  lines.push('   allowed tab when scoped). If no tab is open and `autoOpenOnCommand` is on');
  lines.push('   (the default), the extension opens `targetUrlPrefixes[0]` itself and waits');
  lines.push('   for it, so a backend can start before the browser does.');
  lines.push('5. **`cdp.evaluate`** or **`cdp.call`** — do the work.');
  lines.push('6. **`events.read`** periodically if you need network or DOM events.');
  lines.push('');
  lines.push('`cookies.*` does **not** require an attach. It is the cheapest way to bootstrap.');
  lines.push('');

  lines.push('## 5. Worked examples');
  lines.push('');
  lines.push('### Python');
  lines.push('');
  lines.push(fence('python', `
import asyncio, json, uuid, websockets

# You are the SERVER: the extension dials out to you.
async def handle(ws):
    async def call(op, params=None, timeout=30):
        rid = str(uuid.uuid4())
        await ws.send(json.dumps({"id": rid, "op": op, "params": params or {}}))
        while True:
            frame = json.loads(await asyncio.wait_for(ws.recv(), timeout))
            if frame.get("event"):
                print("event:", frame["event"], frame.get("params"))
                continue
            if frame.get("id") == rid:
                if "error" in frame:
                    raise RuntimeError(frame["error"]["message"])
                return frame.get("result")

    print(await call("ping"))
    await call("tab.attach")
    print(await call("cdp.evaluate", {"expression": "document.title"}))
    print(await call("cookies.list", {"details": {}}))

async def main():
    async with websockets.serve(handle, "127.0.0.1", 9222):
        await asyncio.Future()  # run forever

asyncio.run(main())
`));
  lines.push('');
  lines.push('### Node.js');
  lines.push('');
  lines.push(fence('javascript', `
import { WebSocketServer } from 'ws';
import { randomUUID } from 'node:crypto';

// You are the SERVER: the extension dials out to you.
const wss = new WebSocketServer({ host: '127.0.0.1', port: 9222 });

wss.on('connection', (ws) => {
  const pending = new Map();
  ws.on('message', (raw) => {
    const frame = JSON.parse(raw);
    if (frame.event) return console.log('event:', frame.event, frame.params);
    const entry = pending.get(frame.id);
    if (!entry) return;
    pending.delete(frame.id);
    frame.error ? entry.reject(new Error(frame.error.message)) : entry.resolve(frame.result);
  });

  function call(op, params = {}, timeoutMs = 30000) {
    return new Promise((resolve, reject) => {
      const id = randomUUID();
      pending.set(id, {resolve, reject});
      ws.send(JSON.stringify({id, op, params}));
      setTimeout(() => {
        if (pending.delete(id)) reject(new Error(\`\${op} timed out\`));
      }, timeoutMs);
    });
  }

  (async () => {
    console.log(await call('ping'));
    await call('tab.attach');
    console.log(await call('cdp.evaluate', {expression: 'document.title'}));
  })().catch(console.error);
});
`));
  lines.push('');
  lines.push('### Go');
  lines.push('');
  lines.push(fence('go', `
// You are the SERVER: listen for the extension, then speak the wire protocol
// over the accepted socket (gorilla/websocket). One JSON object per text
// frame: send {"id","op","params"}, read back {"id","result"} / {"id","error"},
// and handle {"event",...} pushes asynchronously.
//
//   upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
//   http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
//       conn, _ := upgrader.Upgrade(w, r, nil)
//       // ... WriteJSON({id, op: "ping"}), ReadJSON loop, correlate by id ...
//   })
//   http.ListenAndServe("127.0.0.1:9222", nil)
//
// Ops: ping, config.get/set, tabs.list/open, tab.attach/detach/current,
// cdp.call, cdp.evaluate, events.read/clear, cookies.list/get/set/remove.
`));
  lines.push('');

  lines.push('## 6. Rules and gotchas');
  lines.push('');
  lines.push('- **Attach before CDP.** `cdp.call` and `cdp.evaluate` auto-attach, but only');
  lines.push('  to a tab inside `targetUrlPrefixes` when scoped. With full access (empty');
  lines.push('  allowlist) any tab qualifies and the active tab wins.');
  lines.push('- **Cookies are full-access by default.** Set `cookieDomains` via `config.set`');
  lines.push('  only if you want scoping; `blockedMethods` can re-block raw cookie CDP');
  lines.push('  methods so backends must use `cookies.*`. By default nothing is blocked.');
  lines.push('- **`events.read` drains.** Events are buffered in the extension and removed when');
  lines.push('  read. `cdp.event` pushes are best-effort; if your consumer is slow they are dropped');
  lines.push('  rather than blocking the socket. Drain regularly.');
  lines.push('- **Only one tab is attached at a time.** This is a `chrome.debugger` constraint.');
  lines.push('  Attaching elsewhere detaches the previous tab.');
  lines.push('- **Opening DevTools on the attached tab detaches the bridge.** Chrome does not allow');
  lines.push('  two debugger clients. You will get `cdp.detached`.');
  lines.push('- **The service worker sleeps.** MV3 evicts idle workers; a keepalive alarm');
  lines.push('  reconnects every 30s. A brief disconnect after long idle periods is normal —');
  lines.push('  reconnect rather than assuming the extension died.');
  lines.push('- **`cdp.evaluate` runs in the page.** Treat page context as untrusted: it is the');
  lines.push('  page\'s own JS realm, with the page\'s own session.');
  lines.push('- **`config.set` can change the endpoint.** Setting `bridgeUrl` closes the socket so');
  lines.push('  it reconnects to the new address. Do not do this mid-conversation unless you mean');
  lines.push('  to move.');
  lines.push('');

  lines.push('## 7. What to do now');
  lines.push('');
  lines.push('1. Confirm which site and which operations you want automated.');
  lines.push('2. Optionally narrow scope: push `targetUrlPrefixes` / `cookieDomains` /');
  lines.push('   `blockedMethods` via `config.set`. Empty means full browser access.');
  lines.push('3. Host a server against the protocol above (the extension dials out to you).');
  lines.push('4. If a call fails, report the exact `error.message` — the extension surfaces the');
  lines.push('   real reason (allowlist miss, blocked method, detached tab) rather than a generic');
  lines.push('   failure.');
  lines.push('');
  lines.push('Ask me for anything ambiguous. Otherwise, start with step 1.');
  lines.push('');

  return lines.join('\n');
}
