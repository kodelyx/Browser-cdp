/**
 * browser-Cdp - configuration.
 *
 * Everything a host project would otherwise hardcode lives here, so the same
 * extension binary can serve Flow, Weavy, Gemini, or anything else that speaks
 * CDP. Values are persisted in chrome.storage.local and can be overwritten at
 * runtime by the backend via the `config.set` bridge operation.
 *
 * Nothing in this file is project specific. The defaults are intentionally
 * full-access: an empty allowlist means every tab / cookie is reachable, just
 * like a raw --remote-debugging-port. A backend that wants scoping can still
 * push a narrower allowlist at runtime via the `config.set` bridge operation.
 */

export const DEFAULTS = {
  // WebSocket endpoint of the local backend that drives this extension.
  bridgeUrl: 'ws://127.0.0.1:9222',

  // Shared secret required on the WebSocket upgrade. The backend generates one
  // on first run and hands it over via config.set, so this is normally filled in
  // automatically. It exists because the origin check alone is not a boundary: a
  // non-browser local process omits the Origin header and would otherwise pass
  // it, letting any program on the machine drive your tabs.
  bridgeToken: '',

  // Only tabs whose URL starts with one of these prefixes can be attached or
  // listed. Empty array = full access (every tab is attachable, like a raw
  // --remote-debugging-port). Put a narrower list here (or push one via
  // `config.set`) only if you want scoping.
  //
  // The first entry doubles as the URL opened by the auto-open behaviour below,
  // so put the tab you want the backend to work with first.
  targetUrlPrefixes: [],

  // Open targetUrlPrefixes[0] when Chrome starts and no matching tab exists.
  // Off by default: silently opening a tab on every launch is intrusive, and a
  // generic bridge has no business assuming the user wants one.
  autoOpenOnStart: false,

  // When a command arrives that needs a tab and none is open, open
  // targetUrlPrefixes[0] and wait for it, instead of failing. This is what makes
  // an unattended cold start work: the backend can start before the browser.
  autoOpenOnCommand: true,

  // Bring the opened tab's window to the front. Only applies when the bridge
  // opens a tab itself.
  focusOnAutoOpen: true,

  // Cookie operations are scoped to these domains. Empty array = full access
  // (all cookies readable/writable, like a raw --remote-debugging-port).
  // A non-empty list restricts reads and rejects writes outside it.
  cookieDomains: [],

  // CDP domains enabled on attach.
  enableDomains: ['Runtime', 'Page', 'DOM', 'Network'],

  // CDP methods always refused. Empty by default for full raw-CDP access.
  // Push a deny-list via `config.set` if you want to re-enable scoping
  // (e.g. block Network.getCookies so backends must use cookies.*).
  blockedMethods: [],

  // Maximum CDP events buffered before the oldest are dropped.
  eventBufferSize: 2000,

  // Reconnect backoff for the backend socket.
  reconnectDelayMs: 1500,

  // Keep-alive alarm period, in minutes. Chrome MV3 service workers are
  // evicted when idle; this wakes the worker so the socket stays up.
  keepAliveMinutes: 0.5,
};

const STORAGE_KEY = 'browserCdp.config';

/** Read config from chrome.storage.local, falling back to defaults per key. */
export async function loadConfig() {
  let stored = {};
  try {
    const raw = await chrome.storage.local.get(STORAGE_KEY);
    stored = raw?.[STORAGE_KEY] || {};
  } catch {
    stored = {};
  }
  return {...DEFAULTS, ...stored};
}

/** Merge a partial config over what is stored and persist it. */
export async function saveConfig(patch = {}) {
  const current = await loadConfig();
  const next = {...current, ...patch};
  await chrome.storage.local.set({[STORAGE_KEY]: next});
  return next;
}

/** Reset to factory defaults. */
export async function resetConfig() {
  await chrome.storage.local.remove(STORAGE_KEY);
  return {...DEFAULTS};
}

/** True when `url` is allowed by the current targetUrlPrefixes list. Empty list = allow all (full access). */
export function urlAllowed(url, config) {
  const value = String(url || '');
  if (!value) return false;
  const prefixes = config.targetUrlPrefixes || [];
  if (prefixes.length === 0) return true;
  return prefixes.some((prefix) => value.startsWith(prefix));
}

/** True when a cookie domain is inside the configured cookie scope. Empty list = allow all (full access). */
export function domainAllowed(domain, config) {
  const value = String(domain || '');
  if (!value) return false;
  const allowed = config.cookieDomains || [];
  if (allowed.length === 0) return true;
  return allowed.some((entry) => {
    const normalized = entry.startsWith('.') ? entry : `.${entry}`;
    return value === entry || value === normalized || value.endsWith(normalized);
  });
}
