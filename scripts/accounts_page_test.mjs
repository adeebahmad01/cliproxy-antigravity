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
