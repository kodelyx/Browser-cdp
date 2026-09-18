/**
 * browser-Cdp - generic Chrome DevTools Protocol bridge.
 *
 * Purpose
 * -------
 * A drop-in alternative to launching Chrome with --remote-debugging-port.
 * The extension dials out to a local backend over a WebSocket, and the
 * backend gets:
 *
 *   - attach/detach to any tab (or only tabs inside targetUrlPrefixes when set)
 *   - arbitrary chrome.debugger.sendCommand() calls (minus blockedMethods when set)
 *   - a stream of CDP events from the attached tab
 *   - cookie read/write (scoped to cookieDomains when set, full access otherwise)
 *
 * There is deliberately NO project logic in this file: no flow IDs, no node
 * catalogs, no site-specific selectors. Anything domain-specific belongs in the
 * backend. The only site-aware values are the allowlists in config.js.
 *
 * Wire protocol (JSON, one message per frame)
 * ------------------------------------------
 * backend -> extension : {id, op, params}
 * extension -> backend : {id, result} | {id, error: {message}}
 * extension -> backend : {event, params}   (unsolicited)
 *
 * Operations
 * ----------
 *   ping                          -> {version, attachedTabId}
 *   config.get                    -> full config
 *   config.set   {patch}          -> updated config (reconnects if bridgeUrl changed)
 *   config.reset                  -> defaults
 *   tabs.list                     -> [{id,url,title,active,windowId}]
 *   tabs.open    {url,active}     -> {tabId,url,title}
 *   tab.attach   {tabId?}         -> {tabId,url,title}
 *   tab.detach                    -> {detached:true}
 *   tab.current                   -> {tabId,url,title} | null
 *   cdp.call     {method,params}  -> raw CDP result
 *   cdp.evaluate {expression,...} -> Runtime.evaluate result
 *   events.read  {limit}          -> drained event buffer
 *   events.clear                  -> {cleared:n}
 *   cookies.list {details}        -> scoped cookies
 *   cookies.get  {url,name}       -> single cookie
 *   cookies.set  {details}        -> written cookie
 *   cookies.remove {url,name}     -> removed cookie
 */

import {
  loadConfig,
  saveConfig,
  resetConfig,
  urlAllowed,
  domainAllowed,
} from './config.js';

let socket = null;
let reconnectTimer = null;
let attachedTabId = null;
let config = null;
let eventQueue = [];

const state = {
  daemonConnected: false,
  backendConnected: false,
  attachedTabId: null,
  tabTitle: null,
  tabUrl: null,
  lastOp: null,
  lastError: null,
  lastActivity: null,
};

function updateState(values) {
  Object.assign(state, values, {lastActivity: Date.now()});
  chrome.action.setBadgeText({text: socket ? 'ON' : ''}).catch(() => {});
  chrome.action.setBadgeBackgroundColor({color: '#16803c'}).catch(() => {});
}

function send(payload) {
  if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify(payload));
}

async function ensureConfig() {
  if (!config) config = await loadConfig();
  return config;
}

/* ------------------------------------------------------------------ *
 * Tab helpers
 * ------------------------------------------------------------------ */

/**
 * Wait for a tab to reach a state.
 *
 * On timeout the error names the last URL seen, because the usual cause is the
 * page redirecting somewhere outside the allowlist (a sign-in page, most often)
 * and the operator needs to see where it actually went.
 */
async function waitForTab(tabId, predicate, timeoutMs = 45000, what = 'The tab') {
  const deadline = Date.now() + timeoutMs;
  let last = null;
  while (Date.now() < deadline) {
    last = await chrome.tabs.get(tabId).catch(() => null);
    if (last && predicate(last)) return last;
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  const where = last?.url ? ` — last URL was ${last.url}` : '';
  throw new Error(`${what} was not ready within ${Math.round(timeoutMs / 1000)}s${where}`);
}

async function listAllowedTabs() {
  await ensureConfig();
  const all = await chrome.tabs.query({});
  return all
    .filter((tab) => urlAllowed(tab.url, config))
    .map(({id, url, title, active, windowId}) => ({id, url, title, active, windowId}));
}

/**
 * Open the first configured target URL and wait for it to load.
 *
 * Generic by construction: the URL comes from targetUrlPrefixes, so the
 * extension still knows nothing about any particular site. Projects that want a
 * tab ready on cold start put that URL first in their allowlist.
 */
async function openTargetTab() {
  await ensureConfig();
  const target = (config.targetUrlPrefixes || [])[0];
  if (!target) {
    throw new Error(
      'No targetUrlPrefixes configured, so there is nothing to open. ' +
        'The backend should call config.set with at least one entry.'
    );
  }

  const focus = config.focusOnAutoOpen !== false;
  const created = await chrome.tabs.create({url: target, active: focus});
  if (!created?.id) throw new Error(`Could not open ${target}`);

  if (focus && created.windowId != null) {
    await chrome.windows.update(created.windowId, {focused: true}).catch(() => {});
  }

  // Wait for the URL to land inside the allowlist rather than for status
  // "complete": a heavy SPA can take far longer than the page needs to start
  // running its own session renewal, and requiring "complete" turns a working
  // page into a spurious timeout.
  const tab = await waitForTab(
    created.id,
    (item) => urlAllowed(item.url, config),
    45000,
    `${target}`
  );

  send({event: 'tab.autoOpened', params: {tabId: tab.id, url: tab.url}});
  return tab;
}

/**
 * Open a target tab at Chrome start when autoOpenOnStart is on and nothing
 * matching is already open. Off by default — see config.js.
 */
async function maybeAutoOpenOnStart() {
  await ensureConfig();
  if (!config.autoOpenOnStart) return;

  const existing = await listAllowedTabs().catch(() => []);
  if (existing.length > 0) return;

  try {
    await openTargetTab();
  } catch (error) {
    send({event: 'tab.autoOpenFailed', params: {message: error?.message || String(error)}});
  }
}

async function resolveTab(tabId = null) {
  await ensureConfig();

  if (tabId) {
    const tab = await chrome.tabs.get(tabId).catch(() => null);
    if (!tab) throw new Error(`Tab ${tabId} not found`);
    if (!urlAllowed(tab.url, config)) {
      throw new Error(`Tab ${tabId} is outside the configured URL allowlist`);
    }
    return tab;
  }

  if (attachedTabId !== null) {
    const tab = await chrome.tabs.get(attachedTabId).catch(() => null);
    if (tab && urlAllowed(tab.url, config)) return tab;
    attachedTabId = null;
  }

  const candidates = await listAllowedTabs();
  const active = candidates.find((tab) => tab.active) || candidates[0];
  if (!active) {
    // Nothing to attach to. On an unattended cold start the backend may be up
    // before the browser, so open the target ourselves rather than failing.
    if (config.autoOpenOnCommand) return openTargetTab();
    throw new Error(
      'No tab matches the configured URL allowlist. Open a matching tab, or set config.targetUrlPrefixes.'
    );
  }
  return chrome.tabs.get(active.id);
}

async function attach(tabId) {
  await ensureConfig();
  if (attachedTabId === tabId) return chrome.tabs.get(tabId);

  if (attachedTabId !== null) {
    await chrome.debugger.detach({tabId: attachedTabId}).catch(() => {});
    attachedTabId = null;
  }

  await chrome.debugger.attach({tabId}, '1.3');
  attachedTabId = tabId;

  for (const domain of config.enableDomains || []) {
    await chrome.debugger
      .sendCommand({tabId}, `${domain}.enable`, {})
      .catch((error) => {
        // Not every domain can be enabled on every page; report but continue.
        send({event: 'cdp.domainError', params: {domain, message: error?.message || String(error)}});
      });
  }

  const tab = await chrome.tabs.get(tabId);
  updateState({attachedTabId: tabId, tabTitle: tab.title || null, tabUrl: tab.url || null, lastError: null});
  send({event: 'cdp.attached', params: {tabId, url: tab.url, title: tab.title || null}});
  return tab;
}

async function detach() {
  if (attachedTabId === null) return {detached: false};
  const tabId = attachedTabId;
  await chrome.debugger.detach({tabId}).catch(() => {});
  attachedTabId = null;
  updateState({attachedTabId: null, tabTitle: null, tabUrl: null});
  return {detached: true, tabId};
}

/* ------------------------------------------------------------------ *
 * Cookie helpers (domain-scoped, always)
 * ------------------------------------------------------------------ */

function cookieKey(cookie) {
  return `${cookie.storeId}:${cookie.domain}:${cookie.path}:${cookie.name}`;
}

async function cookieList(details = {}) {
  await ensureConfig();
  const {url, domain, name, path, secure, session} = details;
  const query = {};
  if (domain) query.domain = domain;
  if (name) query.name = name;
  if (path) query.path = path;
  if (secure !== undefined) query.secure = secure;
  if (session !== undefined) query.session = session;

  // chrome.cookies.getAll requires either a url or a domain. When the caller
  // gives neither, fan out across every allowed domain so the result stays
  // inside the configured scope. In full-access mode (empty cookieDomains)
  // a bare query returns every cookie, like a raw --remote-debugging-port.
  const queries = [];
  if (url) {
    if (!domainAllowed(new URL(url).hostname, config)) {
      throw new Error(`Cookie URL is outside the configured cookie scope: ${url}`);
    }
    queries.push({...query, url});
  } else if (domain) {
    if (!domainAllowed(domain, config)) {
      throw new Error(`Cookie domain is outside the configured cookie scope: ${domain}`);
    }
    queries.push(query);
  } else if ((config.cookieDomains || []).length === 0) {
    queries.push({...query});
  } else {
    for (const allowed of config.cookieDomains) {
      queries.push({...query, domain: allowed});
    }
  }

  const found = new Map();
  for (const q of queries) {
    const results = await chrome.cookies.getAll(q).catch(() => []);
    for (const cookie of results) {
      if (domainAllowed(cookie.domain, config)) found.set(cookieKey(cookie), cookie);
    }
  }
  return [...found.values()];
}

function assertCookieDomain(details) {
  if (details.domain && !domainAllowed(details.domain, config)) {
    throw new Error(`Cookie domain is outside the configured cookie scope: ${details.domain}`);
  }
  if (details.url) {
    const host = new URL(details.url).hostname;
    if (!domainAllowed(host, config)) {
      throw new Error(`Cookie URL is outside the configured cookie scope: ${details.url}`);
    }
  }
}

async function cookieGet({url, name, storeId}) {
  await ensureConfig();
  if (!url) throw new Error('cookies.get requires a url');
  if (!domainAllowed(new URL(url).hostname, config)) {
    throw new Error(`Cookie URL is outside the configured cookie scope: ${url}`);
  }
  return chrome.cookies.get({url, name, storeId});
}

async function cookieSet(details = {}) {
  await ensureConfig();
  assertCookieDomain(details);
  return chrome.cookies.set(details);
}

async function cookieRemove({url, name, storeId}) {
  await ensureConfig();
  if (!url) throw new Error('cookies.remove requires a url');
  if (!domainAllowed(new URL(url).hostname, config)) {
    throw new Error(`Cookie URL is outside the configured cookie scope: ${url}`);
  }
  return chrome.cookies.remove({url, name, storeId});
}

/* ------------------------------------------------------------------ *
 * Operation dispatch
 * ------------------------------------------------------------------ */

async function handle(message) {
  const {id, op, params = {}} = message;
  try {
    updateState({backendConnected: true, lastOp: op, lastError: null});
    let result;

    switch (op) {
      case 'ping':
        result = {
          version: chrome.runtime.getManifest().version,
          attachedTabId,
          config: await ensureConfig(),
        };
        break;

      case 'config.get':
        result = await ensureConfig();
        break;

      case 'config.set': {
        const previous = await ensureConfig();
        config = await saveConfig(params.patch || {});

        // Reconnect when the endpoint or the credential changes. On first
        // pairing this is what moves the socket from tokenless to authenticated.
        const endpointChanged =
          (params.patch?.bridgeUrl && params.patch.bridgeUrl !== previous.bridgeUrl) ||
          (params.patch?.bridgeToken && params.patch.bridgeToken !== previous.bridgeToken);
        if (endpointChanged) socket?.close();

        result = config;
        break;
      }

      case 'config.reset':
        config = await resetConfig();
        result = config;
        break;

      case 'tabs.list':
        result = await listAllowedTabs();
        break;

      case 'tabs.open': {
        await ensureConfig();
        const url = String(params.url || '');
        if (!urlAllowed(url, config)) {
          throw new Error(`Refusing to open a URL outside the allowlist: ${url}`);
        }
        const created = await chrome.tabs.create({url, active: params.active !== false});
        if (!created?.id) throw new Error('Could not open tab');
        const tab = await waitForTab(created.id, (item) => urlAllowed(item.url, config), 45000, url);
        result = {tabId: tab.id, url: tab.url, title: tab.title || null};
        break;
      }

      case 'tab.attach': {
        const tab = await resolveTab(params.tabId || null);
        const attached = await attach(tab.id);
        result = {tabId: attached.id, url: attached.url, title: attached.title || null};
        break;
      }

      case 'tab.detach':
        result = await detach();
        break;

      case 'tab.current': {
        if (attachedTabId === null) {
          result = null;
          break;
        }
        const tab = await chrome.tabs.get(attachedTabId).catch(() => null);
        result = tab ? {tabId: tab.id, url: tab.url, title: tab.title || null} : null;
        break;
      }

      case 'cdp.call': {
        await ensureConfig();
        const method = String(params.method || '');
        if (!method) throw new Error('cdp.call requires a method');
        if ((config.blockedMethods || []).includes(method)) {
          throw new Error(
            `CDP method ${method} is blocked. Use the scoped cookies.* operations instead.`
          );
        }
        const tab = await resolveTab(params.tabId || null);
        if (attachedTabId !== tab.id) await attach(tab.id);
        result = await chrome.debugger.sendCommand(
          {tabId: attachedTabId},
          method,
          params.params || {}
        );
        break;
      }

      case 'cdp.evaluate': {
        await ensureConfig();
        const tab = await resolveTab(params.tabId || null);
        if (attachedTabId !== tab.id) await attach(tab.id);
        const evaluated = await chrome.debugger.sendCommand(
          {tabId: attachedTabId},
          'Runtime.evaluate',
          {
            expression: String(params.expression || ''),
            returnByValue: params.returnByValue !== false,
            awaitPromise: params.awaitPromise === true,
            userGesture: params.userGesture === true,
          }
        );
        if (evaluated?.exceptionDetails) {
          throw new Error(
            evaluated.exceptionDetails.exception?.description ||
              evaluated.exceptionDetails.text ||
              'Evaluation failed'
          );
        }
        result = evaluated?.result?.value;
        break;
      }

      case 'events.read': {
        const limit = Math.max(1, Math.min(Number(params.limit || 100), 1000));
        result = eventQueue.splice(0, limit);
        break;
      }

      case 'events.clear': {
        const cleared = eventQueue.length;
        eventQueue = [];
        result = {cleared};
        break;
      }

      case 'cookies.list':
        result = await cookieList(params.details || {});
        break;

      case 'cookies.get':
        result = await cookieGet(params);
        break;

      case 'cookies.set':
        result = await cookieSet(params);
        break;

      case 'cookies.remove':
        result = await cookieRemove(params);
        break;

      default:
        throw new Error(`Unknown bridge operation: ${op}`);
    }

    send({id, result});
  } catch (error) {
    updateState({lastError: error?.message || String(error)});
    send({id, error: {message: error?.message || String(error)}});
  }
}

/* ------------------------------------------------------------------ *
 * Socket lifecycle
 * ------------------------------------------------------------------ */

/**
 * Build the socket URL, appending the bridge token when one is configured.
 *
 * The token goes in the query string because a browser cannot set arbitrary
 * headers on a WebSocket handshake. It is empty until the backend hands one over
 * during first pairing.
 */
function socketUrl(cfg) {
  const base = cfg.bridgeUrl || 'ws://127.0.0.1:9222';
  if (!cfg.bridgeToken) return base;
  const separator = base.includes('?') ? '&' : '?';
  return `${base}${separator}token=${encodeURIComponent(cfg.bridgeToken)}`;
}

async function connect() {
  if (socket && [WebSocket.OPEN, WebSocket.CONNECTING].includes(socket.readyState)) return;
  const cfg = await ensureConfig();

  try {
    socket = new WebSocket(socketUrl(cfg));
  } catch (error) {
    updateState({daemonConnected: false, lastError: error?.message || String(error)});
    scheduleReconnect();
    return;
  }

  socket.onopen = () => {
    updateState({daemonConnected: true, backendConnected: false, lastError: null});
    send({
      event: 'bridge.ready',
      params: {
        version: chrome.runtime.getManifest().version,
        config: cfg,
      },
    });
  };

  socket.onmessage = (event) => {
    let message;
    try {
      message = JSON.parse(event.data);
    } catch (error) {
      send({error: {message: `Malformed frame from backend: ${error.message}`}});
      return;
    }
    handle(message);
  };

  socket.onclose = () => {
    socket = null;
    updateState({daemonConnected: false, backendConnected: false});
    scheduleReconnect();
  };

  socket.onerror = () => socket?.close();
}

function scheduleReconnect() {
  clearTimeout(reconnectTimer);
  const delay = config?.reconnectDelayMs ?? 1500;
  reconnectTimer = setTimeout(connect, delay);
}

/* ------------------------------------------------------------------ *
 * Events
 * ------------------------------------------------------------------ */

chrome.debugger.onEvent.addListener((source, method, params) => {
  if (source.tabId !== attachedTabId) return;
  const item = {method, params, timestamp: Date.now()};
  eventQueue.push(item);
  const cap = config?.eventBufferSize ?? 2000;
  if (eventQueue.length > cap) eventQueue.shift();
  send({event: 'cdp.event', params: item});
});

chrome.debugger.onDetach.addListener((source, reason) => {
  if (source.tabId !== attachedTabId) return;
  attachedTabId = null;
  updateState({
    attachedTabId: null,
    tabTitle: null,
    tabUrl: null,
    lastError: `Debugger detached: ${reason}`,
  });
  send({event: 'cdp.detached', params: {reason}});
});

chrome.tabs.onRemoved.addListener((tabId) => {
  if (tabId !== attachedTabId) return;
  attachedTabId = null;
  updateState({attachedTabId: null, tabTitle: null, tabUrl: null});
  send({event: 'cdp.detached', params: {reason: 'tab-closed'}});
});

chrome.tabs.onUpdated.addListener((tabId, changeInfo, tab) => {
  if (tabId !== attachedTabId || !changeInfo.url) return;
  updateState({tabUrl: tab.url || null, tabTitle: tab.title || null});
  send({event: 'tab.navigated', params: {tabId, url: tab.url}});
});

/* ------------------------------------------------------------------ *
 * Popup messaging
 *
 * The popup is intentionally thin: it reads status and config, builds the
 * AI connection prompt, and offers a manual reconnect. Configuration is set
 * by the backend over the bridge (`config.set`), so there is no editor here.
 * ------------------------------------------------------------------ */

chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
  (async () => {
    if (message?.op === 'ui.status') {
      const tabs = await listAllowedTabs().catch(() => []);
      return {...state, allowedTabCount: tabs.length, version: chrome.runtime.getManifest().version};
    }
    if (message?.op === 'ui.config.get') return ensureConfig();
    if (message?.op === 'ui.reconnect') {
      socket?.close();
      await connect();
      return {reconnecting: true};
    }
    throw new Error('Unknown popup operation');
  })()
    .then(sendResponse)
    .catch((error) => sendResponse({error: error?.message || String(error)}));
  return true;
});

/* ------------------------------------------------------------------ *
 * Boot
 * ------------------------------------------------------------------ */

chrome.runtime.onInstalled.addListener(() => {
  connect();
  maybeAutoOpenOnStart();
});
chrome.runtime.onStartup.addListener(() => {
  connect();
  maybeAutoOpenOnStart();
});
chrome.alarms.create('browser-cdp-keepalive', {periodInMinutes: 0.5});
chrome.alarms.onAlarm.addListener((alarm) => {
  if (alarm.name === 'browser-cdp-keepalive') connect();
});

connect();
