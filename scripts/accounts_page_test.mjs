import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { runInNewContext } from 'node:vm';

const html = readFileSync(new URL('../accounts_page.html', import.meta.url), 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
const origin = 'http://localhost:8317';
const host = new URL(origin).host;
const userAgent = 'test-browser';
const managementKey = 'fixture-management-key';

function rememberedLogin(version, apiBase) {
  const json = JSON.stringify({ state: { apiBase, managementKey, rememberPassword: true }, version: 0 });
  const suffix = version === 1 ? `|${userAgent}` : '';
  const salt = Buffer.from(`cli-proxy-api-webui::secure-storage|${host}${suffix}`);
  const bytes = Buffer.from(json);
  for (let i = 0; i < bytes.length; i++) bytes[i] ^= salt[i % salt.length];
  return `enc::v${version}::${bytes.toString('base64')}`;
}

async function accountsPageRequests(login) {
  const requests = [];
  const element = {
    classList: { add() {}, toggle() {} },
    addEventListener() {}, removeAttribute() {}, setAttribute() {}, replaceChildren() {}, append() {},
  };
  const window = {};
  window.parent = window;
  runInNewContext(script, {
    window, TextEncoder, TextDecoder, atob, URL,
    document: { getElementById: () => element, createElement: () => element, createTextNode: (text) => text },
    location: { host, origin }, navigator: { userAgent },
    localStorage: { getItem: (key) => key === 'cli-proxy-auth' ? login : null },
    fetch: async (url, { headers }) => {
      requests.push({ url, authorization: headers.Authorization });
      return { ok: true, json: async () => ({ accounts: [] }) };
    },
  });
  await new Promise(setImmediate);
  return requests;
}

for (const version of [1, 2]) {
  test(`reuses a same-origin CPAMC v${version} remembered login`, async () => {
    assert.deepEqual(await accountsPageRequests(rememberedLogin(version, origin)), [
      { url: '/v0/management/agy/accounts', authorization: `Bearer ${managementKey}` },
    ]);
  });
}

test('does not send a remembered key for a different origin', async () => {
  assert.deepEqual(await accountsPageRequests(rememberedLogin(2, 'http://127.0.0.1:8317')), []);
});

test('shows and copies the raw sign-in URL and clears it on cancel', async () => {
  const signInUrl = 'https://accounts.google.com/o/oauth2/v2/auth?client_id=test&state=abc123';
  const elements = new Map();
  const makeEl = (id = '') => {
    const classes = new Set();
    const listeners = new Map();
    return {
      id,
      value: '',
      href: '',
      textContent: '',
      disabled: false,
      checked: false,
      open: false,
      className: '',
      classList: {
        add: (...names) => names.forEach((n) => classes.add(n)),
        remove: (...names) => names.forEach((n) => classes.delete(n)),
        toggle: (name, force) => (force === undefined ? (classes.has(name) ? classes.delete(name) : classes.add(name)) : (force ? classes.add(name) : classes.delete(name))),
        contains: (name) => classes.has(name),
      },
      addEventListener: (type, fn) => {
        if (!listeners.has(type)) listeners.set(type, []);
        listeners.get(type).push(fn);
      },
      dispatch: async (type, ev = { preventDefault() {} }) => {
        for (const fn of listeners.get(type) || []) await fn(ev);
      },
      removeAttribute(attr) { if (attr === 'href') this.href = ''; },
      setAttribute() {},
      replaceChildren() {},
      append() {},
      focus() {},
      select() {},
      close() {},
    };
  };
  const getElementById = (id) => {
    if (!elements.has(id)) elements.set(id, makeEl(id));
    return elements.get(id);
  };
  const copied = [];
  const window = {};
  window.parent = window;
  runInNewContext(script, {
    window, TextEncoder, TextDecoder, atob, URL, setTimeout, clearTimeout,
    document: { getElementById, createElement: () => makeEl(), createTextNode: (text) => text },
    location: { host, origin },
    navigator: {
      userAgent,
      clipboard: { writeText: async (text) => { copied.push(text); } },
    },
    localStorage: { getItem: (key) => (key === 'cli-proxy-auth' ? rememberedLogin(2, origin) : null) },
    fetch: async (url) => {
      if (url === '/v0/management/agy/accounts') return { ok: true, json: async () => ({ accounts: [] }) };
      if (url === '/v8/management/oauth/auth-url?provider=agy') {
        return { ok: true, json: async () => ({ url: signInUrl, state: 'st-1' }) };
      }
      if (url.startsWith('/v8/management/oauth/session?state=')) return { ok: true, json: async () => ({ ok: true }) };
      throw new Error(`unexpected fetch: ${url}`);
    },
  });
  await new Promise(setImmediate);

  await getElementById('addBtn').dispatch('click');
  assert.equal(getElementById('loginUrlInput').value, signInUrl);
  assert.equal(getElementById('loginLink').href, signInUrl);
  assert.equal(getElementById('loginUrlRow').classList.contains('hidden'), false);

  await getElementById('copyLoginUrlBtn').dispatch('click');
  assert.deepEqual(copied, [signInUrl]);
  assert.equal(getElementById('copyLoginUrlBtn').textContent, 'Copied');

  await getElementById('cancelBtn').dispatch('click');
  assert.equal(getElementById('loginUrlInput').value, '');
  assert.equal(getElementById('loginLink').href, '');
  assert.equal(getElementById('loginUrlRow').classList.contains('hidden'), true);
  assert.equal(getElementById('copyLoginUrlBtn').textContent, 'Copy URL');
});

test('ignores stale copy completion after cancel and restart', async () => {
  const urls = [
    'https://accounts.google.com/o/oauth2/v2/auth?client_id=test&state=first',
    'https://accounts.google.com/o/oauth2/v2/auth?client_id=test&state=second',
  ];
  let loginCall = 0;
  let resolveCopy;
  let execCommandCalls = 0;
  const elements = new Map();
  const makeEl = (id = '') => {
    const classes = new Set();
    const listeners = new Map();
    return {
      id,
      value: '',
      href: '',
      textContent: '',
      disabled: false,
      checked: false,
      open: false,
      className: '',
      classList: {
        add: (...names) => names.forEach((n) => classes.add(n)),
        remove: (...names) => names.forEach((n) => classes.delete(n)),
        toggle: (name, force) => (force === undefined ? (classes.has(name) ? classes.delete(name) : classes.add(name)) : (force ? classes.add(name) : classes.delete(name))),
        contains: (name) => classes.has(name),
      },
      addEventListener: (type, fn) => {
        if (!listeners.has(type)) listeners.set(type, []);
        listeners.get(type).push(fn);
      },
      dispatch: async (type, ev = { preventDefault() {} }) => {
        for (const fn of listeners.get(type) || []) await fn(ev);
      },
      removeAttribute(attr) { if (attr === 'href') this.href = ''; },
      setAttribute() {},
      replaceChildren() {},
      append() {},
      focus() {},
      select() {},
      close() {},
    };
  };
  const getElementById = (id) => {
    if (!elements.has(id)) elements.set(id, makeEl(id));
    return elements.get(id);
  };
  const window = {};
  window.parent = window;
  runInNewContext(script, {
    window, TextEncoder, TextDecoder, atob, URL, setTimeout, clearTimeout,
    document: {
      getElementById,
      createElement: () => makeEl(),
      createTextNode: (text) => text,
      execCommand: () => { execCommandCalls += 1; return true; },
    },
    location: { host, origin },
    navigator: {
      userAgent,
      clipboard: { writeText: () => new Promise((resolve) => { resolveCopy = resolve; }) },
    },
    localStorage: { getItem: (key) => (key === 'cli-proxy-auth' ? rememberedLogin(2, origin) : null) },
    fetch: async (url) => {
      if (url === '/v0/management/agy/accounts') return { ok: true, json: async () => ({ accounts: [] }) };
      if (url === '/v8/management/oauth/auth-url?provider=agy') {
        const nextUrl = urls[loginCall];
        loginCall += 1;
        return { ok: true, json: async () => ({ url: nextUrl, state: `st-${loginCall}` }) };
      }
      if (url.startsWith('/v8/management/oauth/session?state=')) return { ok: true, json: async () => ({ ok: true }) };
      throw new Error(`unexpected fetch: ${url}`);
    },
  });
  await new Promise(setImmediate);

  await getElementById('addBtn').dispatch('click');
  const pendingCopy = getElementById('copyLoginUrlBtn').dispatch('click');
  await getElementById('cancelBtn').dispatch('click');
  await getElementById('addBtn').dispatch('click');
  assert.equal(getElementById('loginUrlInput').value, urls[1]);
  assert.equal(getElementById('copyLoginUrlBtn').textContent, 'Copy URL');

  resolveCopy();
  await pendingCopy;
  assert.equal(getElementById('copyLoginUrlBtn').textContent, 'Copy URL');
  assert.equal(execCommandCalls, 0);
});
