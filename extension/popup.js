/**
 * browser-Cdp - popup.
 *
 * Status line plus Reconnect and Copy-prompt buttons. Configuration is pushed
 * by the backend over the bridge (config.set), so there is nothing to edit here.
 */

import { buildConnectionPrompt } from './ai-prompt.js';

const dot = document.getElementById('dot');
const title = document.getElementById('title');
const detail = document.getElementById('detail');
const version = document.getElementById('version');
const note = document.getElementById('note');

let noteTimer = null;

function hostOf(url) {
  try {
    return new URL(url).host;
  } catch {
    return url || '';
  }
}

function render(state, config) {
  const endpoint = config?.bridgeUrl || 'ws://127.0.0.1:9222';

  if (state.version) version.textContent = `v${state.version}`;

  if (!state.daemonConnected) {
    dot.className = 'dot';
    title.textContent = 'Waiting for backend';
    detail.textContent = endpoint;
    return;
  }

  if (state.attachedTabId != null) {
    dot.className = 'dot live';
    title.textContent = 'Attached';
    detail.textContent = hostOf(state.tabUrl) || `tab ${state.attachedTabId}`;
    return;
  }

  dot.className = 'dot live';
  title.textContent = 'Connected';
  detail.textContent = 'no tab attached yet';
}

async function refresh() {
  const [state, config] = await Promise.all([
    chrome.runtime.sendMessage({op: 'ui.status'}).catch(() => null),
    chrome.runtime.sendMessage({op: 'ui.config.get'}).catch(() => null),
  ]);
  render(state || {}, config || {});
  return {state: state || {}, config: config || {}};
}

function flash(message, kind) {
  clearTimeout(noteTimer);
  note.textContent = message;
  note.className = `note show ${kind}`;
  noteTimer = setTimeout(() => {
    note.className = 'note';
  }, 2600);
}

document.getElementById('reconnect').addEventListener('click', async () => {
  try {
    await chrome.runtime.sendMessage({op: 'ui.reconnect'});
    flash('Reconnecting…', 'ok');
  } catch {
    flash('Reconnect failed.', 'bad');
  }
  refresh();
});

document.getElementById('copyPrompt').addEventListener('click', async () => {
  const {state, config} = await refresh();
  const prompt = buildConnectionPrompt(state, config);
  try {
    await navigator.clipboard.writeText(prompt);
    flash('Copied — paste it into your AI.', 'ok');
  } catch {
    flash('Clipboard blocked by the browser.', 'bad');
  }
});

refresh();
setInterval(refresh, 2000);
