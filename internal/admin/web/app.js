// shunt web UI: the same management API the CLI and TUI use.
// Every server or user string reaches the DOM through textContent, value or
// setAttribute only. The admin token lives in a variable of this tab.
'use strict';

const API = '/_shunt/api/v1/';
const WIRE_PREFIX = 'shunt/';
const ALIASES = ['sonnet', 'opus', 'haiku', 'fable'];
const BASE_URL_EXAMPLE = 'https://api.example.com/anthropic';
const MESSAGES_PATH = '/v1/messages';
const NOTICE_MS = 6000;
const COPIED_MS = 1500;
const POLL_MS = 3000;
const RECENT_ROWS = 25;

let token = null;
let state = null;
let activity = null;
let shown = ''; // what the tables currently show, serialized
let syncNotice = null;
const tests = new Map(); // route name -> { busy } or { ok, text }

const $ = (id) => document.getElementById(id);
const sorted = (obj) => Object.keys(obj || {}).sort();
const isAlias = (name) => ALIASES.includes(name);
const aliasWarning = (name) =>
  `While this route exists, a main thread using the ${name} alias gets 400 errors (routes serve subagents only).`;
const plural = (n, word) => `${n} ${word}${n === 1 ? '' : 's'}`;
const clock = (iso) => new Date(iso).toLocaleTimeString([], { hour12: false });
const duration = (ms) => (ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(1)} s`);

// el builds an element. props: text, class, on<event> handlers, or plain
// attributes (true = present, false/null = absent). Children are nodes or
// strings (appended as text).
function el(tag, props, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (k === 'text') e.textContent = v;
    else if (k === 'class') e.className = v;
    else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
    else if (v === true) e.setAttribute(k, '');
    else if (v !== false && v != null) e.setAttribute(k, String(v));
  }
  for (const kid of kids) if (kid != null) e.append(kid);
  return e;
}

const badge = (kind, text, title) => el('span', { class: 'badge ' + kind, text, title: title || null });

class ApiError extends Error {
  constructor(status, code, message) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

async function api(method, call, body) {
  const headers = {};
  if (token) headers.Authorization = 'Bearer ' + token;
  const init = { method, headers, cache: 'no-store', credentials: 'omit', redirect: 'error' };
  if (method !== 'GET') {
    headers['Content-Type'] = 'application/json';
    if (body !== undefined) init.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(API + call, init);
  } catch {
    throw new ApiError(0, 'unreachable', 'The shunt daemon is not reachable. Is it still running?');
  }
  let data = null;
  try {
    data = await res.json();
  } catch {
    data = null;
  }
  if (res.ok) return data || {};
  const err = (data && data.error) || {};
  if (res.status === 401 && token) signOut('Your session is no longer valid.');
  throw new ApiError(res.status, err.code || 'http', err.message || `Request failed (HTTP ${res.status}).`);
}

const enc = encodeURIComponent;

// ---- session -------------------------------------------------------------

function showLogin(reason) {
  $('app').hidden = true;
  $('reload').hidden = true;
  $('health').hidden = true;
  $('listen').textContent = '';
  $('login-reason').textContent = reason;
  $('login-reason').hidden = !reason;
  $('login').hidden = false;
  for (const d of document.querySelectorAll('dialog[open]')) d.close();
}

function signOut(reason) {
  token = null;
  state = null;
  activity = null;
  shown = '';
  tests.clear();
  showLogin(reason);
}

async function signIn() {
  const hash = location.hash;
  // The one-time code must not stay in the address bar or history.
  if (hash) history.replaceState(null, '', location.pathname + location.search);
  const code = hash.startsWith('#code=') ? hash.slice('#code='.length) : '';
  if (!code) return showLogin('');
  try {
    token = (await api('POST', 'web-token', { code })).token || null;
  } catch (e) {
    const used = e.status === 401 ? 'This link was already used or has expired.' : e.message;
    return showLogin(used);
  }
  if (!token) return showLogin('The daemon did not return a session.');
  $('login').hidden = true;
  $('app').hidden = false;
  $('reload').hidden = false;
  await run(refresh);
  setInterval(poll, POLL_MS);
  document.addEventListener('visibilitychange', poll);
}

// ---- notices -------------------------------------------------------------

// run reports a failed action; a 401 has already switched to the login view.
async function run(fn) {
  try {
    await fn();
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) return;
    notify(e.message, 'error');
  }
}

// notify shows a message. Only 'ok' messages fade; the rest stay until
// dismissed. code, if given, is shown as a command to copy.
function notify(text, kind = 'ok', code = '') {
  const msg = el('span', { text });
  if (code) msg.append(' ', el('code', { text: code }));
  const n = el('div', { class: 'notice ' + kind, role: kind === 'error' ? 'alert' : 'status' }, msg);
  if (kind === 'ok') {
    setTimeout(() => n.remove(), NOTICE_MS);
  } else {
    n.append(el('button', { type: 'button', class: 'link', text: 'Dismiss', onclick: () => n.remove() }));
  }
  $('notices').append(n);
  return n;
}

// syncNeeded keeps one notice telling the user to run sync_command: the
// web UI cannot touch settings.json, which only the local CLI writes.
function syncNeeded(out) {
  if (!out.needs_sync) return;
  if (syncNotice) syncNotice.remove();
  syncNotice = notify(
    'Alias routes changed. If routing is on, update settings.json from a terminal, then restart Claude Code sessions:',
    'warn', out.sync_command || 'shunt on');
}

// ---- data ----------------------------------------------------------------

// refresh loads the config and the activity and redraws what changed.
async function refresh() {
  const [st, act] = await Promise.all([api('GET', 'state'), api('GET', 'activity')]);
  state = st;
  activity = act;
  $('offline').hidden = true;
  render();
}

// poll picks up changes made elsewhere (CLI, TUI, hand edits after a
// reload) and new traffic. It stays quiet: one banner while the daemon is
// away, nothing else.
async function poll() {
  if (!token || document.hidden) return;
  try {
    await refresh();
  } catch (e) {
    if (!token) return; // signed out by a 401
    shown = ''; // the next good answer redraws, even if nothing else changed
    $('offline').textContent = e.message + ' Showing the last known state.';
    $('offline').hidden = false;
    $('health').className = 'lamp bad';
    $('health').textContent = 'unreachable';
  }
}

// locks says why config or keys changes are refused, or '' when allowed.
function locks() {
  if (state.load_error) {
    const why = 'the config failed to load';
    return { config: why, keys: why };
  }
  const [cfg, keys] = state.files || [];
  return {
    config: cfg && !cfg.writable ? cfg.path + ' is read-only' : '',
    keys: keys && !keys.writable ? keys.path + ' is read-only' : '',
  };
}

const keyState = (provider) => state.keys[provider] || 'missing';
const hasProvider = (name) => Object.hasOwn(state.config.providers, name);

// ---- rendering -----------------------------------------------------------

// render redraws the page from state, activity and tests, unless nothing
// changed; the focused control keeps the focus across a redraw.
function render() {
  const next = JSON.stringify([state, activity, [...tests]]);
  if (next === shown) return;
  shown = next;
  const focus = document.activeElement && document.activeElement.dataset.key;

  const lock = locks();
  $('listen').textContent = state.listen;
  $('health').hidden = false;
  $('health').className = 'lamp ' + (state.load_error ? 'bad' : 'ok');
  $('health').textContent = state.load_error ? 'config error' : 'running';
  const loadErr = $('load-error');
  loadErr.hidden = !state.load_error;
  loadErr.textContent = state.load_error
    ? `Config error: ${state.load_error}. Fix the files, then press Reload config.`
    : '';
  const reasons = [...new Set([lock.config, lock.keys].filter(Boolean))];
  $('lock').hidden = reasons.length === 0 || Boolean(state.load_error);
  $('lock').textContent = reasons.length ? `Changes are disabled: ${reasons.join('; ')}.` : '';
  renderSetup();
  renderRoutes(lock);
  renderProviders(lock);
  renderRecent();
  renderFiles();

  if (focus) {
    const again = document.querySelector(`[data-key="${CSS.escape(focus)}"]`);
    if (again && !document.querySelector('dialog[open]')) again.focus();
  }
}

function renderSetup() {
  const providers = sorted(state.config.providers);
  const keyed = providers.some((p) => ['literal', 'file'].includes(keyState(p)));
  const steps = [
    [providers.length > 0, 'Add a provider: an Anthropic-compatible API you have a key for.'],
    [keyed, 'Set its key.'],
    [sorted(state.config.routes).length > 0, 'Add a route that sends a model name of your choice to that provider.'],
  ];
  $('setup').hidden = steps.every(([done]) => done);
  $('steps').replaceChildren(...steps.map(([done, text]) =>
    el('li', { class: done ? 'done' : null }, el('span', { text }), done ? badge('ok', 'done') : null)));
}

function renderFiles() {
  const labels = ['Config file', 'Keys file'];
  const rows = (state.files || []).map((f, i) => {
    const facts = [f.format, f.exists ? '' : 'not created yet', f.writable ? '' : 'read-only'];
    return [el('dt', { text: labels[i] || 'File' }),
      el('dd', null, el('code', { text: f.path }), ' ', el('span', { text: facts.filter(Boolean).join(' · ') }))];
  });
  $('files').replaceChildren(...rows.flat());
}

function cell(label, ...kids) {
  return el('td', { 'data-label': label }, el('div', { class: 'cell' }, ...kids));
}

// button builds a row action; key identifies it across redraws.
function button(key, text, onclick, why, extra) {
  return el('button', {
    type: 'button', class: extra || 'secondary', 'data-key': key,
    disabled: Boolean(why), title: why || null, text, onclick,
  });
}

const KEY_KIND = { literal: 'ok', file: 'ok', missing: 'warn', invalid: 'bad' };
const KEY_TEXT = { literal: 'key set', file: 'key file', missing: 'no key', invalid: 'key invalid' };

// routeProblem says why a route cannot serve requests, or ''.
function routeProblem(r) {
  if (!hasProvider(r.provider)) return 'provider missing';
  const k = keyState(r.provider);
  return k === 'literal' || k === 'file' ? '' : KEY_TEXT[k] || 'key invalid';
}

function traffic(name) {
  const s = (activity.routes || {})[name];
  const lines = [];
  if (s) {
    const last = `${plural(s.requests, 'request')} · last ${s.status} at ${clock(s.time)}`;
    lines.push(el('div', null, el('span', { text: last }),
      s.errors ? ' ' : null, s.errors ? badge('warn', `${s.errors} failed`) : null));
  } else {
    lines.push(el('div', { class: 'muted', text: 'no requests yet' }));
  }
  const t = tests.get(name);
  if (t && t.busy) lines.push(el('div', { class: 'test busy', text: 'testing…' }));
  else if (t) lines.push(el('div', { class: 'test wrap ' + (t.ok ? 'ok-text' : 'bad-text'), role: 'status', text: 'Test: ' + t.text }));
  return lines;
}

function renderRoutes(lock) {
  const routes = state.config.routes;
  const names = sorted(routes);
  const rows = names.map((name) => {
    const r = routes[name];
    const id = WIRE_PREFIX + name;
    const copyBtn = el('button', { type: 'button', class: 'link', 'data-key': `route:${name}:copy`, 'aria-label': 'Copy model ID ' + id, text: 'Copy' });
    copyBtn.addEventListener('click', () => copy(id, copyBtn));
    const problem = routeProblem(r);
    const busy = tests.get(name) && tests.get(name).busy;
    return el('tr', null,
      cell('Route',
        el('div', { class: 'name' }, el('strong', { text: name }), isAlias(name) ? ' ' : null,
          isAlias(name) ? badge('warn', 'alias', aliasWarning(name)) : null),
        el('div', { class: 'sub' }, el('code', { class: 'id', text: id }), ' ', copyBtn)),
      cell('Sends to',
        el('div', { class: 'target' }, el('strong', { text: r.provider }), ' ',
          problem ? badge('bad', problem) : badge('ok', 'ready', 'provider and key are in place')),
        el('div', { class: 'sub mono wrap', text: r.model })),
      cell('Traffic', ...traffic(name)),
      cell('Actions', el('div', { class: 'row-actions' },
        button(`route:${name}:test`, 'Test', () => testRoute(name),
          busy ? 'test running' : '', 'secondary'),
        button(`route:${name}:edit`, 'Edit', () => openRoute(name), lock.config),
        button(`route:${name}:delete`, 'Delete', () => deleteRoute(name), lock.config, 'secondary danger-text'))));
  });
  $('routes').replaceChildren(...rows);
  $('routes-count').textContent = names.length || '';
  $('routes').closest('table').hidden = names.length === 0;
  $('routes-empty').hidden = names.length > 0;
  const add = $('add-route');
  const why = lock.config || (sorted(state.config.providers).length ? '' : 'add a provider first');
  add.disabled = Boolean(why);
  add.title = why;
}

function renderProviders(lock) {
  const providers = state.config.providers;
  const names = sorted(providers);
  const usedBy = {};
  for (const [route, r] of Object.entries(state.config.routes)) (usedBy[r.provider] ||= []).push(route);
  const rows = names.map((name) => {
    const k = keyState(name);
    const users = (usedBy[name] || []).sort();
    const actions = el('div', { class: 'row-actions' },
      button(`provider:${name}:key`, k === 'missing' ? 'Set key' : 'Replace key', () => openKey(name), lock.keys),
      k === 'missing' ? null : button(`provider:${name}:clear`, 'Clear key', () => clearKey(name), lock.keys),
      button(`provider:${name}:edit`, 'Edit', () => openProvider(name), lock.config),
      button(`provider:${name}:delete`, 'Delete', () => deleteProvider(name),
        lock.config || (users.length ? `used by ${plural(users.length, 'route')}; remove or move them first` : ''),
        'secondary danger-text'));
    return el('tr', null,
      cell('Name', el('strong', { text: name })),
      cell('Base URL', el('span', { class: 'wrap mono', text: providers[name].base_url })),
      cell('Key', badge(KEY_KIND[k] || 'bad', KEY_TEXT[k] || k)),
      cell('Used by', users.length
        ? el('span', { class: 'wrap', text: users.join(', ') })
        : el('span', { class: 'muted', text: 'no route' })),
      cell('Actions', actions));
  });
  $('providers').replaceChildren(...rows);
  $('providers-count').textContent = names.length || '';
  $('providers').closest('table').hidden = names.length === 0;
  $('providers-empty').hidden = names.length > 0;
  const add = $('add-provider');
  add.disabled = Boolean(lock.config);
  add.title = lock.config;
}

function renderRecent() {
  const all = activity.recent || [];
  const rows = all.slice(0, RECENT_ROWS).map((e) => {
    const target = e.target === 'rejected' ? badge('warn', 'rejected', 'answered by shunt, not sent anywhere')
      : el('span', { class: e.route ? null : 'muted', text: e.target });
    return el('tr', null,
      cell('Time', el('span', { class: 'mono', text: clock(e.time) })),
      cell('Model', el('span', { class: 'wrap mono', text: e.model || '—' }),
        e.path === MESSAGES_PATH ? null : ' ',
        e.path === MESSAGES_PATH ? null : el('span', { class: 'muted wrap small', text: e.path })),
      cell('Sent to', target),
      cell('Status', badge('code ' + (e.status >= 400 ? 'bad' : 'ok'), String(e.status))),
      el('td', { 'data-label': 'Duration', class: 'num' }, el('div', { class: 'cell mono', text: duration(e.ms) })));
  });
  $('recent').replaceChildren(...rows);
  $('recent').closest('table').hidden = all.length === 0;
  $('recent-empty').hidden = all.length > 0;
  $('recent-note').textContent = all.length > RECENT_ROWS ? `latest ${RECENT_ROWS} of ${all.length}` : '';
}

async function copy(text, btn) {
  try {
    await navigator.clipboard.writeText(text);
    btn.textContent = 'Copied';
    setTimeout(() => { btn.textContent = 'Copy'; }, COPIED_MS);
  } catch {
    notify('Copying failed; select the model ID and copy it by hand.', 'warn');
  }
}

// testRoute asks the daemon to send one small request through the route and
// shows the outcome in its row.
async function testRoute(name) {
  tests.set(name, { busy: true });
  render();
  try {
    const r = await api('POST', `routes/${enc(name)}/test`);
    const text = r.ok ? `OK (HTTP ${r.status}, ${duration(r.ms)})`
      : r.status ? `failed (HTTP ${r.status}): ${r.message}` : `failed: ${r.message}`;
    tests.set(name, { ok: Boolean(r.ok), text });
  } catch (e) {
    tests.delete(name);
    if (!(e instanceof ApiError && e.status === 401)) notify(e.message, 'error');
  }
  if (state) render();
}

// ---- dialogs -------------------------------------------------------------

function openDialog(id) {
  const d = $(id);
  const err = d.querySelector('.form-error');
  err.hidden = true;
  err.textContent = '';
  d.showModal();
  return d;
}

// submitting wires a dialog form: fn runs on submit (Enter or the submit
// button); an error stays in the dialog, success closes it and refreshes.
function submitting(formId, fn) {
  const form = $(formId);
  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const d = form.closest('dialog');
    const err = form.querySelector('.form-error');
    const submit = form.querySelector('button[type=submit]');
    submit.disabled = true;
    try {
      await fn(form);
      d.close();
      await run(refresh);
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) return;
      err.textContent = e.message;
      err.hidden = false;
      if (token) run(refresh); // a first step may have been saved
    } finally {
      submit.disabled = false;
    }
  });
}

let editing = null; // name of the provider or route a dialog edits

function openProvider(name) {
  editing = name || null;
  $('provider-title').textContent = name ? 'Edit provider ' + name : 'Add provider';
  const nameIn = $('provider-name');
  nameIn.value = name || '';
  nameIn.readOnly = Boolean(name);
  $('provider-url').value = name ? state.config.providers[name].base_url : '';
  $('provider-url').placeholder = BASE_URL_EXAMPLE;
  $('provider-url-hint').textContent = name
    ? 'Changing the host removes a literal key; you then set it again.'
    : 'The provider’s Anthropic-compatible endpoint, without /v1/messages.';
  $('provider-key').value = '';
  $('provider-key-row').hidden = Boolean(name);
  openDialog('provider-dlg');
  (name ? $('provider-url') : nameIn).focus();
}

async function saveProvider(form) {
  const name = editing || form.elements.name.value.trim();
  const baseURL = form.elements.base_url.value.trim();
  const keyIn = form.elements.key;
  const key = $('provider-key-row').hidden ? '' : keyIn.value.trim();
  keyIn.value = '';
  const retry = Boolean(editing) && Boolean(key); // the provider itself was saved by the first attempt
  const out = await api('PUT', 'providers/' + enc(name), { base_url: baseURL });
  if (!retry) notify(`provider ${name}: ${baseURL}`);
  if (key) {
    // The provider is saved: if the key is refused, the dialog stays open
    // on this provider and only the key needs typing again.
    editing = name;
    form.elements.name.readOnly = true;
    const set = await api('PUT', `providers/${enc(name)}/key`, { value: key });
    notify(`key for ${name} set: ${set.state}`);
  } else if (out.key_cleared) {
    notify(`The key of ${name} was removed because the host changed. Set the key again.`, 'warn');
  }
}

function openKey(name) {
  editing = name;
  $('key-title').textContent = 'Set key for ' + name;
  $('key-value').value = '';
  openDialog('key-dlg');
  $('key-value').focus();
}

async function saveKey(form) {
  const input = form.elements.value;
  const value = input.value.trim();
  input.value = '';
  const out = await api('PUT', `providers/${enc(editing)}/key`, { value });
  notify(`key for ${editing} set: ${out.state}`);
}

function openRoute(name) {
  editing = name || null;
  const r = name ? state.config.routes[name] : { provider: '', model: '' };
  $('route-title').textContent = name ? 'Edit route ' + name : 'Add route';
  const nameIn = $('route-name');
  nameIn.value = name || '';
  nameIn.readOnly = Boolean(name);
  const select = $('route-provider');
  const opts = sorted(state.config.providers).map((p) => el('option', { value: p, text: p }));
  if (r.provider && !hasProvider(r.provider)) {
    opts.unshift(el('option', { value: r.provider, text: r.provider + ' (missing)' }));
  }
  select.replaceChildren(...opts);
  if (r.provider) select.value = r.provider;
  $('route-model').value = r.model;
  showRouteHints();
  openDialog('route-dlg');
  (name ? select : nameIn).focus();
}

// showRouteHints follows the form: the model id of the name typed so far,
// the alias warning, and the models other routes already use at the
// selected provider as suggestions.
function showRouteHints() {
  const name = $('route-name').value.trim();
  $('route-id').textContent = name ? `Subagents ask for it as ${WIRE_PREFIX}${name}.` : 'Lower-case letters, digits and dashes.';
  const hint = $('route-alias');
  hint.hidden = !isAlias(name);
  hint.textContent = isAlias(name) ? aliasWarning(name) : '';
  const provider = $('route-provider').value;
  const models = new Set(Object.values(state.config.routes).filter((r) => r.provider === provider).map((r) => r.model));
  $('route-models').replaceChildren(...[...models].sort().map((m) => el('option', { value: m })));
}

async function saveRoute(form) {
  const name = editing || form.elements.name.value.trim();
  const provider = form.elements.provider.value;
  const model = form.elements.model.value.trim();
  const out = await api('PUT', 'routes/' + enc(name), { provider, model });
  tests.delete(name); // an old test result says nothing about the new target
  notify(`route ${name}: model ${WIRE_PREFIX}${name} -> ${model} at ${provider}`);
  if (isAlias(name)) notify('Warning: ' + aliasWarning(name), 'warn');
  syncNeeded(out);
}

// confirmAction asks before a destructive change; Esc or Cancel says no.
function confirmAction(text, label) {
  const d = $('confirm-dlg');
  $('confirm-text').textContent = text;
  $('confirm-ok').textContent = label;
  d.returnValue = '';
  d.showModal();
  return new Promise((resolve) => {
    d.addEventListener('close', () => resolve(d.returnValue === 'ok'), { once: true });
  });
}

async function deleteProvider(name) {
  if (!(await confirmAction(`Delete provider ${name} and its key?`, 'Delete'))) return;
  await run(async () => {
    await api('DELETE', 'providers/' + enc(name));
    notify(`provider ${name} removed`);
    await refresh();
  });
}

async function clearKey(name) {
  if (!(await confirmAction(`Remove the key of ${name}? Routes to it fail until a key is set again.`, 'Remove key'))) return;
  await run(async () => {
    await api('DELETE', `providers/${enc(name)}/key`);
    notify(`key for ${name} removed`);
    await refresh();
  });
}

async function deleteRoute(name) {
  if (!(await confirmAction(`Delete route ${name} (model ${WIRE_PREFIX}${name})?`, 'Delete'))) return;
  await run(async () => {
    const out = await api('DELETE', 'routes/' + enc(name));
    tests.delete(name);
    notify(`route ${name} removed`);
    syncNeeded(out);
    await refresh();
  });
}

async function reload() {
  await run(async () => {
    try {
      await api('POST', 'reload');
      notify('config reloaded');
    } finally {
      if (token) await refresh();
    }
  });
}

// ---- wiring --------------------------------------------------------------

function wire() {
  $('reload').addEventListener('click', reload);
  $('add-provider').addEventListener('click', () => openProvider(null));
  $('add-route').addEventListener('click', () => openRoute(null));
  $('route-name').addEventListener('input', showRouteHints);
  $('route-provider').addEventListener('change', showRouteHints);
  for (const b of document.querySelectorAll('[data-close]')) {
    b.addEventListener('click', () => b.closest('dialog').close());
  }
  // A key typed but not submitted is dropped with the dialog.
  $('key-dlg').addEventListener('close', () => { $('key-value').value = ''; });
  $('provider-dlg').addEventListener('close', () => { $('provider-key').value = ''; });
  submitting('provider-form', saveProvider);
  submitting('key-form', saveKey);
  submitting('route-form', saveRoute);
}

wire();
signIn();
